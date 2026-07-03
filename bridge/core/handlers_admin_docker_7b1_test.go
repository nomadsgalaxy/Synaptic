// handlers_admin_docker_7b1_test.go — wave 7b1 extensions (recreate /
// build / profiles / disk). Live-daemon paths are exercised in the
// rebuild-and-verify step; here we cover offline-safe paths only:
//   - new router cases dispatch correctly through the gate
//   - gate-off behaviour preserved
//   - helper utilities (firstNonEmpty / splitLines)
package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestAdminDockerRouter_AcceptsRecreateAndBuild(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		composeProject: "synaptic",
		socketAllowed:  false, // gate off → 412 short-circuits before compose
	}
	for _, action := range []string{"recreate", "build"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/core/"+action, nil)
		w := httptest.NewRecorder()
		srv.adminDockerRouter(w, req)
		if w.Code != http.StatusPreconditionFailed {
			t.Errorf("action %s: want 412 (gate off), got %d body=%s", action, w.Code, w.Body.String())
		}
	}
}

func TestAdminDockerRouter_DiskEndpointDispatch(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/docker/disk", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412 when gate off, got %d", w.Code)
	}
}

func TestAdminDockerRouter_ProfilesListDispatch(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/docker/profiles", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412, got %d", w.Code)
	}
}

func TestAdminDockerRouter_ProfileActionRequiresBoth(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	// Missing action → 400 BEFORE gate check (router-level path validation).
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/profiles/cloudflared", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	// Path is "/admin/docker/profiles/cloudflared" → trimmed sub="cloudflared",
	// SplitN(":", 2) yields ["cloudflared"], len < 2 → 400.
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing profile action: want 400, got %d", w.Code)
	}

	// With action → falls through to gate check.
	req = httptest.NewRequest(http.MethodPost, "/admin/docker/profiles/cloudflared/enable", nil)
	w = httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("with action + gate off: want 412, got %d", w.Code)
	}
}

func TestAdminDockerRouter_UnknownActionListsNewLegalSet(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState()
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/core/yeet", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
	body := w.Body.String()
	for _, action := range []string{"start", "stop", "restart", "recreate", "build", "logs"} {
		if !contains7b1(body, action) {
			t.Errorf("404 body should mention legal action %q; got %s", action, body)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Helper functions
// ──────────────────────────────────────────────────────────────────────────

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", " ", "first", "second"); got != "first" {
		t.Errorf("want first, got %q", got)
	}
	if got := firstNonEmpty("", "  ", ""); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestSplitLines_TrimsAndDropsEmpties(t *testing.T) {
	got := splitLines("\nprofile1\n  profile2 \n\n\nprofile3\n")
	if len(got) != 3 {
		t.Fatalf("want 3 lines, got %d: %v", len(got), got)
	}
	if got[0] != "profile1" || got[1] != "profile2" || got[2] != "profile3" {
		t.Errorf("contents: %v", got)
	}
}

// substring check (duplicated naming to avoid colliding with sensitive_test's contains).
func contains7b1(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
