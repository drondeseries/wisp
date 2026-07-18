FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /wisp ./cmd/wisp

FROM alpine:3.20
RUN apk add --no-cache ca-certificates fuse3
COPY --from=build /wisp /usr/local/bin/wisp
EXPOSE 8080
ENTRYPOINT ["wisp"]
