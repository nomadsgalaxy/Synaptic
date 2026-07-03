package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mkProbe is a small helper for fabricating the tested ifaceProbe shape.
func mkProbe(name string, up, loop bool, addrs ...string) ifaceProbe {
	p := ifaceProbe{name: name, up: up, loopback: loop}
	for _, a := range addrs {
		ip, ipnet, err := net.ParseCIDR(a)
		if err != nil {
			continue
		}
		ipnet.IP = ip
		p.addrs = append(p.addrs, ipnet)
	}
	return p
}

// ──────────────────────────────────────────────────────────────────────────

func TestAdminNetwork_PrefersPrivateIPv4(t *testing.T) {
	probes := []ifaceProbe{
		mkProbe("Ethernet", true, false, "10.0.0.5/24", "8.8.8.8/32"),
	}
	ip, iface := selectPrimaryIPv4(probes)
	if ip != "10.0.0.5" {
		t.Errorf("expected private 10.0.0.5, got %q", ip)
	}
	if iface != "Ethernet" {
		t.Errorf("expected Ethernet, got %q", iface)
	}
}

func TestAdminNetwork_FallbackToGlobalIPv4(t *testing.T) {
	probes := []ifaceProbe{
		mkProbe("Ethernet", true, false, "8.8.8.8/32"),
	}
	ip, _ := selectPrimaryIPv4(probes)
	if ip != "8.8.8.8" {
		t.Errorf("expected 8.8.8.8 fallback, got %q", ip)
	}
}

func TestAdminNetwork_SkipsLinkLocal(t *testing.T) {
	probes := []ifaceProbe{
		mkProbe("Wi-Fi", true, false, "169.254.1.1/16", "192.168.1.5/24"),
	}
	ip, _ := selectPrimaryIPv4(probes)
	if ip != "192.168.1.5" {
		t.Errorf("link-local should be skipped; got %q", ip)
	}
}

func TestAdminNetwork_SkipsLoopbackAndDown(t *testing.T) {
	probes := []ifaceProbe{
		mkProbe("Loopback", true, true, "127.0.0.1/8"),
		mkProbe("DownEthernet", false, false, "192.168.1.5/24"),
	}
	ip, iface := selectPrimaryIPv4(probes)
	if ip != "" {
		t.Errorf("expected no primary (loopback + down only), got %q on %q", ip, iface)
	}
}

func TestAdminNetwork_MultipleInterfaces_FirstPrivateWins(t *testing.T) {
	probes := []ifaceProbe{
		mkProbe("vEthernet (Docker)", true, false, "172.17.0.1/16"),
		mkProbe("Ethernet", true, false, "192.168.1.5/24"),
	}
	ip, iface := selectPrimaryIPv4(probes)
	// Both are private — first one in iteration wins (matches "primary"
	// semantics: whichever interface is enumerated first).
	if ip != "172.17.0.1" {
		t.Errorf("expected first-private to win, got %q on %q", ip, iface)
	}
}

func TestAdminNetwork_SelectIPv6Global(t *testing.T) {
	probes := []ifaceProbe{
		mkProbe("Ethernet", true, false, "fe80::1234/64", "2001:db8::42/64"),
	}
	ip := selectPrimaryIPv6(probes)
	if ip != "2001:db8::42" {
		t.Errorf("expected global IPv6, got %q", ip)
	}
}

func TestAdminNetwork_RequiresAuth(t *testing.T) {
	t.Setenv("SD_API_TOKEN", "net-test-token")
	bank := newTestBank(t)
	server := &Server{bank: bank}
	handler := newAuthMiddleware(server.routes())

	req := httptest.NewRequest(http.MethodGet, "/admin/network", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", rec.Code)
	}
}

func TestAdminNetwork_HappyPathHasFields(t *testing.T) {
	bank := newTestBank(t)
	server := &Server{bank: bank}

	req := httptest.NewRequest(http.MethodGet, "/admin/network", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var info netInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Hostname should always be set.
	if info.Hostname == "" {
		t.Errorf("expected hostname populated; got empty")
	}
	// At least one interface (loopback always exists).
	if len(info.Interfaces) == 0 {
		t.Errorf("expected at least one interface in response")
	}
	if info.ListenPort != 9911 {
		t.Errorf("expected listen_port=9911 (default), got %d", info.ListenPort)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Cloudflared hint
// ──────────────────────────────────────────────────────────────────────────

func TestAdminNetwork_CloudflaredHintFound(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	must(t, os.WriteFile(cfg, []byte(`tunnel: foo
ingress:
  - hostname: cognito.example.com
    path: ^/(api)$
    service: http://localhost:9911
  - service: http_status:404
`), 0o644))
	got := scanCloudflaredConfig(cfg, 9911)
	if got != "cognito.example.com" {
		t.Errorf("expected cognito.example.com, got %q", got)
	}
}

func TestAdminNetwork_CloudflaredHintNoMatch(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	must(t, os.WriteFile(cfg, []byte(`ingress:
  - hostname: other.example.com
    service: http://localhost:8000
`), 0o644))
	got := scanCloudflaredConfig(cfg, 9911)
	if got != "" {
		t.Errorf("expected empty (no match for port 9911), got %q", got)
	}
}

func TestAdminNetwork_CloudflaredHintMissingFile(t *testing.T) {
	got := scanCloudflaredConfig("/does-not-exist-zzz/.cloudflared/config.yml", 9911)
	if got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}
