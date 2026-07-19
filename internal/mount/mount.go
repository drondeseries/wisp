// Package mount embeds rclone's VFS and go-fuse mount in-process, so wisp
// self-mounts its virtual library with no external rclone binary or process.
// The mount reads from wisp's own HTTP server over the loopback interface via
// rclone's on-the-fly http backend, and self-heals: if the FUSE mount ever
// exits or goes unresponsive, wisp remounts it automatically.
package mount

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/rclone/rclone/backend/http" // register the http backend
	_ "github.com/rclone/rclone/cmd/mount2"   // register the pure-Go (go-fuse) mount
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// Options configure the embedded mount.
type Options struct {
	// ServerURL is wisp's own HTTP server, e.g. http://127.0.0.1:8080.
	ServerURL string
	// Mountpoint is where the virtual library is exposed, e.g. /mnt/wisp.
	Mountpoint string
	// AllowOther exposes the mount to other users (needed when a media server
	// container reads the mount as a different UID).
	AllowOther bool
	// ReadChunkSize is the initial VFS read chunk; it doubles up to
	// ReadChunkSizeLimit. Small first chunks keep seeks snappy; the ramp keeps
	// sequential playback efficient.
	ReadChunkSize      int64
	ReadChunkSizeLimit int64
	Delete             func(context.Context, string) error
}

const (
	healthInterval = 30 * time.Second
	healthTimeout  = 5 * time.Second
	maxBackoff     = 30 * time.Second
)

// Mount is a live, self-healing in-process FUSE mount.
type Mount struct {
	opt      Options
	log      *slog.Logger
	fs       fs.Fs
	mountFn  mountlib.MountFn
	mountOpt mountlib.Options
	vfsOpt   vfscommon.Options

	mu      sync.Mutex
	mp      *mountlib.MountPoint
	healthy atomic.Bool
	stop    chan struct{}
	stopped atomic.Bool
}

// Start mounts the wisp library at opt.Mountpoint and returns immediately. A
// supervisor goroutine keeps it mounted, remounting on failure, until Close.
func Start(ctx context.Context, opt Options, log *slog.Logger) (*Mount, error) {
	if err := os.MkdirAll(opt.Mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("create mountpoint: %w", err)
	}
	_, mountFn := mountlib.ResolveMountMethod("mount2")
	if mountFn == nil {
		return nil, fmt.Errorf("mount2 (go-fuse) is not available on this build")
	}

	// On-the-fly http backend — no rclone config file required.
	remote := fmt.Sprintf(":http,url='%s':", strings.TrimRight(opt.ServerURL, "/"))
	f, err := fs.NewFs(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("create http backend: %w", err)
	}
	if opt.Delete != nil {
		f = &deleteFS{Fs: f, delete: opt.Delete}
	}

	mountOpt := mountlib.Opt // copy defaults
	mountOpt.AllowOther = opt.AllowOther
	mountOpt.AllowNonEmpty = true
	mountOpt.Daemon = false

	vfsOpt := vfscommon.Opt // copy defaults
	vfsOpt.CacheMode = vfscommon.CacheModeOff
	vfsOpt.DirCacheTime = fs.Duration(0) // disable directory caching for instant metadata/size updates
	if opt.ReadChunkSize > 0 {
		vfsOpt.ChunkSize = fs.SizeSuffix(opt.ReadChunkSize)
	}
	if opt.ReadChunkSizeLimit > 0 {
		vfsOpt.ChunkSizeLimit = fs.SizeSuffix(opt.ReadChunkSizeLimit)
	}

	m := &Mount{
		opt: opt, log: log, fs: f, mountFn: mountFn,
		mountOpt: mountOpt, vfsOpt: vfsOpt, stop: make(chan struct{}),
	}
	if err := m.mountOnce(); err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	log.Info("mounted", "mountpoint", opt.Mountpoint, "backend", "http+go-fuse")
	go m.supervise()
	return m, nil
}

// deleteFS adds unlink support to rclone's otherwise read-only HTTP backend.
type deleteFS struct {
	fs.Fs
	delete func(context.Context, string) error
}

func (f *deleteFS) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	for i, entry := range entries {
		if obj, ok := entry.(fs.Object); ok {
			entries[i] = &deleteObject{Object: obj, fs: f}
		}
	}
	return entries, nil
}

func (f *deleteFS) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	obj, err := f.Fs.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return &deleteObject{Object: obj, fs: f}, nil
}

// Directories are synthesized from pin paths and vanish with their final pin.
func (f *deleteFS) Rmdir(context.Context, string) error { return nil }

type deleteObject struct {
	fs.Object
	fs *deleteFS
}

func (o *deleteObject) Fs() fs.Info { return o.fs }
func (o *deleteObject) Remove(ctx context.Context) error {
	return o.fs.delete(ctx, strings.TrimLeft(o.Remote(), "/"))
}

// mountOnce performs a single mount and records it as the current mount point.
func (m *Mount) mountOnce() error {
	mp := mountlib.NewMountPoint(m.mountFn, m.opt.Mountpoint, m.fs, &m.mountOpt, &m.vfsOpt)
	if _, err := mp.Mount(); err != nil {
		return err
	}
	m.mu.Lock()
	m.mp = mp
	m.mu.Unlock()
	m.healthy.Store(true)
	return nil
}

// supervise remounts on an unexpected mount exit or an unresponsive mountpoint.
func (m *Mount) supervise() {
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		errChan := m.mp.ErrChan
		m.mu.Unlock()

		select {
		case <-m.stop:
			return
		case err := <-errChan:
			if m.stopped.Load() {
				return // expected exit from Close
			}
			m.log.Warn("mount exited; remounting", "error", err)
			m.healthy.Store(false)
			m.remount()
		case <-ticker.C:
			if !m.alive() {
				m.log.Warn("mount unresponsive; remounting", "mountpoint", m.opt.Mountpoint)
				m.healthy.Store(false)
				m.remount()
			}
		}
	}
}

// alive reports whether the mountpoint still answers a stat within the timeout.
// A dead FUSE connection returns ENOTCONN or hangs; both are treated as dead.
func (m *Mount) alive() bool {
	done := make(chan bool, 1)
	go func() {
		_, err := os.Stat(m.opt.Mountpoint)
		done <- err == nil
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(healthTimeout):
		return false
	}
}

// remount tears down the stale mount and remounts with capped backoff.
func (m *Mount) remount() {
	m.mu.Lock()
	if m.mp != nil && m.mp.UnmountFn != nil {
		_ = m.mp.UnmountFn()
	}
	m.mu.Unlock()

	backoff := time.Second
	for {
		select {
		case <-m.stop:
			return
		default:
		}
		if err := m.mountOnce(); err == nil {
			m.log.Info("mount recovered", "mountpoint", m.opt.Mountpoint)
			return
		} else {
			m.log.Error("remount failed; retrying", "error", err, "backoff", backoff.String())
		}
		select {
		case <-m.stop:
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Healthy reports whether the mount is currently live.
func (m *Mount) Healthy() bool {
	return m != nil && m.healthy.Load()
}

// Close stops the supervisor and unmounts the library.
func (m *Mount) Close() error {
	if m == nil {
		return nil
	}
	m.stopped.Store(true)
	close(m.stop)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mp != nil && m.mp.UnmountFn != nil {
		return m.mp.UnmountFn()
	}
	return nil
}
