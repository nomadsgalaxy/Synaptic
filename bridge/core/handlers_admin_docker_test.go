// handlers_admin_docker_test.go — wave 7a Docker control plane.
//
// Focused tests:
//   - gate-off path returns 412 with the right shape
//   - allowlist regex enforcement
//   - log frame parser (offline; no daemon required)
//   - rate-limit window
//
// Live-daemon tests would require a Docker socket mount inside the test
// container; that's an integration concern handled by the rebuild-and-
// verify step in the wave write-up.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────
// Gate behaviour — SD_ALLOW_DOCKER_SOCKET=0
// ──────────────────────────────────────────────────────────────────────────

func TestDockerServices_GateDisabledReturns412(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		composeProject: "synaptic",
		socketAllowed:  false, // gate off
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/docker/services", nil)
	w := httptest.NewRecorder()
	srv.handleAdminDockerServices(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "docker_socket_disabled" {
		t.Errorf("error tag: want docker_socket_disabled, got %v", resp["error"])
	}
	if resp["socket_allowed"] != false {
		t.Errorf("socket_allowed: want false, got %v", resp["socket_allowed"])
	}
	if _, ok := resp["hint"]; !ok {
		t.Errorf("response missing hint field")
	}
}

func TestDockerHealth_GateDisabledReturns412(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/docker/health", nil)
	w := httptest.NewRecorder()
	srv.handleAdminDockerHealth(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412 when gate disabled, got %d", w.Code)
	}
}

func TestDockerAction_GateDisabledReturns412(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/core/restart", nil)
	w := httptest.NewRecorder()
	srv.handleAdminDockerServiceAction(w, req, "core", "restart")
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412, got %d", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Gate on + socket unreachable → 503
// ──────────────────────────────────────────────────────────────────────────

func TestDockerServices_GateOnSocketMissingReturns503(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/definitely-not-a-real-docker.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		composeProject: "synaptic",
		socketAllowed:  true, // gate on but socket absent
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/docker/services", nil)
	w := httptest.NewRecorder()
	srv.handleAdminDockerServices(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when socket missing, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "docker_socket_unreachable" {
		t.Errorf("error tag: want docker_socket_unreachable, got %v", resp["error"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Allowlist + rate limit (unit-level — no socket)
// ──────────────────────────────────────────────────────────────────────────

func TestDockerControlState_AllowlistAcceptsAndRejects(t *testing.T) {
	ds := &DockerControlState{
		allowlistRegex: regexp.MustCompile("^synaptic-"),
	}
	if !ds.nameAllowed("synaptic-core") {
		t.Errorf("synaptic-core should be allowed")
	}
	if !ds.nameAllowed("synaptic-ollama") {
		t.Errorf("synaptic-ollama should be allowed")
	}
	if ds.nameAllowed("postgres-15") {
		t.Errorf("postgres-15 should NOT be allowed")
	}
	if ds.nameAllowed("/synaptic-core-impostor") {
		t.Errorf("regex anchors on ^; impostor with leading slash should NOT match")
	}
}

func TestDockerControlState_RateLimitWindow(t *testing.T) {
	ds := &DockerControlState{
		rateLimit:    50 * time.Millisecond,
		lastActionAt: map[string]time.Time{},
	}
	if !ds.rateOK("core", "restart") {
		t.Errorf("first call should pass")
	}
	if ds.rateOK("core", "restart") {
		t.Errorf("second call within window should be blocked")
	}
	// Different (service, action) tuple is independent.
	if !ds.rateOK("core", "stop") {
		t.Errorf("different action should not be rate-limited")
	}
	if !ds.rateOK("ollama", "restart") {
		t.Errorf("different service should not be rate-limited")
	}
}

func TestDockerControlState_RateLimitZeroDisables(t *testing.T) {
	ds := &DockerControlState{rateLimit: 0}
	for i := 0; i < 5; i++ {
		if !ds.rateOK("core", "restart") {
			t.Errorf("rate-limit=0 must always allow; iteration %d blocked", i)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Docker log frame parser
// ──────────────────────────────────────────────────────────────────────────

func TestParseLogStream_MultiFrameLines(t *testing.T) {
	// Build two frames manually: one stdout, one stderr.
	var buf bytes.Buffer
	writeFrame := func(stream byte, payload string) {
		header := make([]byte, 8)
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
		buf.Write(header)
		buf.WriteString(payload)
	}
	writeFrame(1, "first stdout line\n")
	writeFrame(2, "an stderr line\n")
	writeFrame(1, "second stdout line\n")

	var got []LogLine
	if err := parseLogStream(&buf, func(l LogLine) { got = append(got, l) }); err != nil {
		t.Fatalf("parseLogStream: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 lines, got %d: %+v", len(got), got)
	}
	if got[0].Stream != "stdout" || got[0].Line != "first stdout line" {
		t.Errorf("line 1: %+v", got[0])
	}
	if got[1].Stream != "stderr" || got[1].Line != "an stderr line" {
		t.Errorf("line 2: %+v", got[1])
	}
	if got[2].Stream != "stdout" || got[2].Line != "second stdout line" {
		t.Errorf("line 3: %+v", got[2])
	}
}

func TestParseLogStream_SplitLineAcrossFrames(t *testing.T) {
	// Docker can split a single line across two frames if the kernel
	// wrote half of it. The parser must buffer the partial.
	var buf bytes.Buffer
	writeFrame := func(stream byte, payload string) {
		header := make([]byte, 8)
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
		buf.Write(header)
		buf.WriteString(payload)
	}
	writeFrame(1, "hello, ") // no newline yet
	writeFrame(1, "world\n")

	var got []LogLine
	if err := parseLogStream(&buf, func(l LogLine) { got = append(got, l) }); err != nil {
		t.Fatalf("parseLogStream: %v", err)
	}
	if len(got) != 1 || got[0].Line != "hello, world" {
		t.Errorf("want one joined line, got %+v", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Route dispatcher
// ──────────────────────────────────────────────────────────────────────────

func TestAdminDockerRouter_UnknownActionReturns404(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState() // gate off by default in tests
	req := httptest.NewRequest(http.MethodPost, "/admin/docker/services/core/yeet", nil)
	w := httptest.NewRecorder()
	srv.adminDockerRouter(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown action, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown action") {
		t.Errorf("body should hint at the legal set; got %s", w.Body.String())
	}
}

func TestAdminDockerRouter_RoutesServicesAndHealth(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = &DockerControlState{
		client:         NewDockerClient("/tmp/nope.sock"),
		allowlistRegex: regexp.MustCompile("^synaptic-"),
		socketAllowed:  false,
	}
	for _, path := range []string{"/admin/docker/services", "/admin/docker/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.adminDockerRouter(w, req)
		if w.Code != http.StatusPreconditionFailed {
			t.Errorf("%s: want 412, got %d", path, w.Code)
		}
	}
}
