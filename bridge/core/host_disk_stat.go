// host_disk_stat.go — host filesystem free/total probe for
// /admin/docker/disk. The Engine API's /system/df only reports the
// daemon's view of its own objects (containers/images/volumes); it
// does NOT report how much room is left on the disk those objects
// live on. The dashboard wants "you need 42 GB, you have 78 GB free
// ✓", which requires a direct statfs.
//
// Strategy: SD Core runs containerised in the normal deployment, so we
// can't statfs the daemon's /var/lib/docker directly — that lives on
// the host (or inside the Docker Desktop WSL2 VHDX). Instead we probe
// /data first, which is the bind-mounted host directory containing
// docker-compose.yml. statfs on a bind-mounted path reports the
// underlying host filesystem stats, which is exactly the disk where
// the user's pull/build/volume bytes will land on Docker Desktop.
//
// Fall back to /var/lib/docker for non-containerised deployments. If
// neither path is reachable (rare), we return zeros plus a `note`
// field; the frontend falls back to its static estimate.
package main

import "fmt"

// HostDiskUsage carries the result of a statfs probe. Path identifies
// which candidate the result describes (or "" when none succeeded).
type HostDiskUsage struct {
	Path       string
	FreeBytes  uint64
	TotalBytes uint64
	Note       string
}

// hostDiskStatFn is the underlying statfs call, indirected through a
// var so tests can substitute a deterministic value without touching
// the real filesystem.
var hostDiskStatFn = hostDiskStatDefault

// resolveHostDiskUsage probes candidates in order and returns the
// first one that yields a non-zero total. If every candidate errors,
// returns a zero-valued usage carrying a diagnostic note (the
// frontend renders this as "host disk free unavailable").
func resolveHostDiskUsage(candidates []string) HostDiskUsage {
	var firstErr error
	for _, p := range candidates {
		if p == "" {
			continue
		}
		free, total, err := hostDiskStatFn(p)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p, err)
			}
			continue
		}
		if total == 0 {
			continue
		}
		return HostDiskUsage{Path: p, FreeBytes: free, TotalBytes: total}
	}
	note := "host disk free unavailable in this deployment"
	if firstErr != nil {
		note = fmt.Sprintf("host disk free unavailable in this deployment (%v)", firstErr)
	}
	return HostDiskUsage{Note: note}
}
