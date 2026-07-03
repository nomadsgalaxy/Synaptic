package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staticMimeServer creates a Server pointed at a temp data dir seeded with
// a few vendor / asset files so we can exercise the static handler's MIME
// override without booting the full stack.
func staticMimeServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	// Vendor JS file (the original failing case).
	must(t, os.MkdirAll(filepath.Join(dir, "vendor"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "vendor", "gif.js"),
		[]byte("// gif.js worker stub\n"), 0o644))
	// Plus a CSS and a WASM under assets/ so we hit a different prefix.
	must(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "assets", "style.css"),
		[]byte("body{}"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "assets", "engine.wasm"),
		[]byte{0, 0x61, 0x73, 0x6d}, 0o644))
	// And the index.html exact-match.
	must(t, os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<html></html>"), 0o644))

	server := &Server{dataDir: dir}
	return server, dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fireStatic(t *testing.T, server *Server, path string) (int, string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/", server.staticDashboard())
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
}

// ──────────────────────────────────────────────────────────────────────────

func TestStatic_JSContentType(t *testing.T) {
	server, _ := staticMimeServer(t)
	code, ct, _ := fireStatic(t, server, "/vendor/gif.js")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("expected Content-Type to start with application/javascript, got %q", ct)
	}
}

func TestStatic_WasmContentType(t *testing.T) {
	server, _ := staticMimeServer(t)
	code, ct, _ := fireStatic(t, server, "/assets/engine.wasm")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.HasPrefix(ct, "application/wasm") {
		t.Errorf("expected Content-Type=application/wasm, got %q", ct)
	}
}

func TestStatic_CSSContentType(t *testing.T) {
	server, _ := staticMimeServer(t)
	code, ct, _ := fireStatic(t, server, "/assets/style.css")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !strings.HasPrefix(ct, "text/css") {
		t.Errorf("expected Content-Type=text/css, got %q", ct)
	}
}

func TestStatic_VendorPathReachable(t *testing.T) {
	server, _ := staticMimeServer(t)
	code, _, body := fireStatic(t, server, "/vendor/gif.js")
	if code != http.StatusOK {
		t.Fatalf("/vendor/* must be in allowedPrefixes; got %d", code)
	}
	if !strings.Contains(body, "gif.js worker stub") {
		t.Errorf("body should contain seeded content, got %q", body)
	}
}
