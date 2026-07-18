// Package mount embeds rclone's VFS and go-fuse mount in-process, so wisp
// self-mounts its virtual library with no external rclone binary or process.
// The mount reads from wisp's own HTTP server over the loopback interface via
// rclone's on-the-fly http backend.
package mount

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
}

// Mount is a live in-process FUSE mount.
type Mount struct {
	mp  *mountlib.MountPoint
	log *slog.Logger
}

// Start mounts the wisp library at opt.Mountpoint and returns immediately; the
// FUSE server runs in the background until Close is called.
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

	mountOpt := mountlib.Opt // copy defaults
	mountOpt.AllowOther = opt.AllowOther
	mountOpt.AllowNonEmpty = true
	mountOpt.Daemon = false

	vfsOpt := vfscommon.Opt // copy defaults
	vfsOpt.CacheMode = vfscommon.CacheModeOff
	vfsOpt.DirCacheTime = fs.Duration(10 * time.Second)
	if opt.ReadChunkSize > 0 {
		vfsOpt.ChunkSize = fs.SizeSuffix(opt.ReadChunkSize)
	}
	if opt.ReadChunkSizeLimit > 0 {
		vfsOpt.ChunkSizeLimit = fs.SizeSuffix(opt.ReadChunkSizeLimit)
	}

	mp := mountlib.NewMountPoint(mountFn, opt.Mountpoint, f, &mountOpt, &vfsOpt)
	if _, err := mp.Mount(); err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	log.Info("mounted", "mountpoint", opt.Mountpoint, "backend", "http+go-fuse")

	m := &Mount{mp: mp, log: log}
	go m.watch()
	return m, nil
}

// watch logs an unexpected mount failure.
func (m *Mount) watch() {
	if m.mp.ErrChan == nil {
		return
	}
	if err := <-m.mp.ErrChan; err != nil {
		m.log.Error("mount exited", "error", err)
	}
}

// Close unmounts the library.
func (m *Mount) Close() error {
	if m == nil || m.mp == nil || m.mp.UnmountFn == nil {
		return nil
	}
	return m.mp.UnmountFn()
}
