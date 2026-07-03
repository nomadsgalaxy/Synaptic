// host_disk_stat_other.go — non-Linux stub. SD Core never runs in
// production on these platforms (the build is linux/amd64 scratch),
// but `go test ./...` on a Windows or macOS dev host needs the symbol
// to exist so the package compiles. Returning an error keeps the
// probe path inert; tests inject hostDiskStatFn directly.

//go:build !linux

package main

import (
	"errors"
	"runtime"
)

func hostDiskStatDefault(path string) (free, total uint64, err error) {
	return 0, 0, errors.New("host disk statfs not supported on " + runtime.GOOS)
}
