// host_disk_stat_linux.go — Linux implementation of hostDiskStatDefault.
// SD Core's binary is built linux/amd64 (CGO_ENABLED=0) and shipped in
// a scratch image, so this is the production path.

//go:build linux

package main

import (
	"fmt"
	"syscall"
)

func hostDiskStatDefault(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	if st.Bsize <= 0 {
		return 0, 0, fmt.Errorf("statfs %s: invalid block size %d", path, st.Bsize)
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, nil
}
