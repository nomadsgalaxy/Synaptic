// host_disk_stat_test.go — covers the platform-independent probe
// orchestration. The Linux statfs path is exercised live via the
// rebuild-and-verify step; here we inject hostDiskStatFn.
package main

import (
	"errors"
	"strings"
	"testing"
)

func withMockStat(t *testing.T, fn func(path string) (uint64, uint64, error)) {
	t.Helper()
	prev := hostDiskStatFn
	hostDiskStatFn = fn
	t.Cleanup(func() { hostDiskStatFn = prev })
}

func TestResolveHostDiskUsage_FirstCandidateWins(t *testing.T) {
	withMockStat(t, func(path string) (uint64, uint64, error) {
		if path == "/data" {
			return 84_293_476_352, 1_024_000_000_000, nil
		}
		return 0, 0, errors.New("nope")
	})
	got := resolveHostDiskUsage([]string{"/data", "/var/lib/docker"})
	if got.Path != "/data" {
		t.Errorf("path: want /data, got %q", got.Path)
	}
	if got.FreeBytes != 84_293_476_352 {
		t.Errorf("free: want 84293476352, got %d", got.FreeBytes)
	}
	if got.TotalBytes != 1_024_000_000_000 {
		t.Errorf("total: want 1024000000000, got %d", got.TotalBytes)
	}
	if got.Note != "" {
		t.Errorf("note should be empty on success, got %q", got.Note)
	}
}

func TestResolveHostDiskUsage_FallsThroughToSecondCandidate(t *testing.T) {
	withMockStat(t, func(path string) (uint64, uint64, error) {
		if path == "/data" {
			return 0, 0, errors.New("not mounted")
		}
		if path == "/var/lib/docker" {
			return 100, 200, nil
		}
		return 0, 0, errors.New("nope")
	})
	got := resolveHostDiskUsage([]string{"/data", "/var/lib/docker"})
	if got.Path != "/var/lib/docker" {
		t.Errorf("expected fallback to /var/lib/docker; got %q", got.Path)
	}
	if got.FreeBytes != 100 || got.TotalBytes != 200 {
		t.Errorf("free/total: want 100/200, got %d/%d", got.FreeBytes, got.TotalBytes)
	}
}

func TestResolveHostDiskUsage_AllCandidatesFailReturnsNote(t *testing.T) {
	withMockStat(t, func(path string) (uint64, uint64, error) {
		return 0, 0, errors.New("statfs: permission denied")
	})
	got := resolveHostDiskUsage([]string{"/data", "/var/lib/docker"})
	if got.Path != "" {
		t.Errorf("path should be empty when all fail, got %q", got.Path)
	}
	if got.FreeBytes != 0 || got.TotalBytes != 0 {
		t.Errorf("free/total should both be 0 on failure; got %d/%d", got.FreeBytes, got.TotalBytes)
	}
	if !strings.Contains(got.Note, "host disk free unavailable") {
		t.Errorf("note should describe unavailability; got %q", got.Note)
	}
	if !strings.Contains(got.Note, "permission denied") {
		t.Errorf("note should surface the underlying error; got %q", got.Note)
	}
}

func TestResolveHostDiskUsage_SkipsCandidateWithZeroTotal(t *testing.T) {
	// A zero-total result (e.g. statfs of an unmounted overlay) shouldn't
	// be reported as the answer; treat it as a soft miss and try the
	// next candidate.
	withMockStat(t, func(path string) (uint64, uint64, error) {
		if path == "/data" {
			return 0, 0, nil // success but useless
		}
		return 500, 1000, nil
	})
	got := resolveHostDiskUsage([]string{"/data", "/var/lib/docker"})
	if got.Path != "/var/lib/docker" {
		t.Errorf("zero-total result should be skipped; got path %q", got.Path)
	}
	if got.TotalBytes != 1000 {
		t.Errorf("want total 1000, got %d", got.TotalBytes)
	}
}

func TestResolveHostDiskUsage_EmptyCandidates(t *testing.T) {
	got := resolveHostDiskUsage(nil)
	if got.Path != "" || got.FreeBytes != 0 || got.TotalBytes != 0 {
		t.Errorf("nil candidates should yield zero usage; got %+v", got)
	}
	if got.Note == "" {
		t.Errorf("nil candidates should still yield a diagnostic note")
	}
}

func TestResolveHostDiskUsage_EmptyStringCandidatesIgnored(t *testing.T) {
	called := 0
	withMockStat(t, func(path string) (uint64, uint64, error) {
		called++
		return 1, 2, nil
	})
	got := resolveHostDiskUsage([]string{"", "/x"})
	if called != 1 {
		t.Errorf("empty candidate should not invoke statfs; got %d calls", called)
	}
	if got.Path != "/x" {
		t.Errorf("want path /x, got %q", got.Path)
	}
}
