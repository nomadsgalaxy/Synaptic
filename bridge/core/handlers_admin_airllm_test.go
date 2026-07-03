// handlers_admin_airllm_test.go — wave 7c3 unit tests for the AirLLM
// diagnostics + install_script endpoints. Live-Ollama / live-daemon
// probes are deliberately out of scope here — the offline-safe paths
// (template generation, OS detection, parameter parsing) are what we
// can test without spinning up a sidecar.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────
// Disk + first-run estimators
// ──────────────────────────────────────────────────────────────────────────

func TestEstimateDiskGB_ByModelAndCompression(t *testing.T) {
	cases := []struct {
		model       string
		compression string
		want        int
	}{
		{"meta-llama/Llama-3.1-70B-Instruct", "4bit", 42},
		{"meta-llama/Llama-3.1-70B-Instruct", "8bit", 77},
		{"meta-llama/Llama-3.1-8B-Instruct", "4bit", 4},
		{"meta-llama/Llama-3.1-405B-Instruct", "4bit", 243},
		{"unknown/random-3B", "none", 6},
	}
	for _, c := range cases {
		if got := estimateDiskGB(c.model, c.compression); got != c.want {
			t.Errorf("estimateDiskGB(%q, %q) = %d, want %d", c.model, c.compression, got, c.want)
		}
	}
}

func TestEstimateFirstRunMinutes_BoundsLowerLimit(t *testing.T) {
	// Smallest model never goes below 3 minutes.
	if got := estimateFirstRunMinutes("3B"); got < 3 {
		t.Errorf("small model should still bound at >=3 min, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Install-script generation
// ──────────────────────────────────────────────────────────────────────────

func TestBuildBashInstallScript_BakesInParams(t *testing.T) {
	script := buildBashInstallScript(airllmScriptParams{
		Platform:     "linux",
		Model:        "meta-llama/Llama-3.1-70B-Instruct",
		Compression:  "4bit",
		EndpointPort: 9912,
	})
	for _, must := range []string{
		"#!/usr/bin/env bash",
		"meta-llama/Llama-3.1-70B-Instruct",
		"COMPRESSION=\"4bit\"",
		"PORT=9912",
		"airllm",
		"bridge/airllm/server.py",
	} {
		if !strings.Contains(script, must) {
			t.Errorf("bash script missing %q", must)
		}
	}
}

func TestBuildWindowsInstallScript_ContainsLiteralBacktickContinuations(t *testing.T) {
	// PowerShell uses backtick for line continuation. The template
	// generator goes through a BT placeholder; verify the substitution
	// landed literal backticks in the output.
	script := buildWindowsInstallScript(airllmScriptParams{
		Platform:     "windows",
		Model:        "meta-llama/Llama-3.1-70B-Instruct",
		Compression:  "4bit",
		EndpointPort: 9912,
	})
	if !strings.Contains(script, "$MODEL = ") {
		t.Errorf("windows script missing $MODEL = assignment")
	}
	if !strings.Contains(script, "Start-Process -FilePath") {
		t.Errorf("windows script missing Start-Process invocation")
	}
	if !strings.Contains(script, "`") {
		t.Errorf("windows script should contain literal backtick for PS line continuation")
	}
	// And the placeholder should NOT still be present.
	if strings.Contains(script, "<<BT>>") {
		t.Errorf("windows script still has BT placeholder; substitution failed")
	}
}

func TestBuildBashProbeScript_EmitsJSON(t *testing.T) {
	script := buildBashProbeScript()
	for _, must := range []string{
		"python_path",
		"gpu_vendor",
		"disk_free_gb",
		"nvidia-smi",
	} {
		if !strings.Contains(script, must) {
			t.Errorf("bash probe script missing %q", must)
		}
	}
}

func TestBuildWindowsProbeScript_EmitsJSON(t *testing.T) {
	script := buildWindowsProbeScript()
	for _, must := range []string{
		"Get-CimInstance Win32_VideoController",
		"ConvertTo-Json",
		"python_path",
	} {
		if !strings.Contains(script, must) {
			t.Errorf("windows probe script missing %q", must)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// install_script HTTP handler
// ──────────────────────────────────────────────────────────────────────────

func TestInstallScript_RequiresPlatformWhenUnresolvable(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState() // gate off; no compose probe; no SD_HOST_OS
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/install_script", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMInstallScript(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 when platform unresolvable, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestInstallScript_GeneratesBashWithLinuxPlatform(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/install_script?platform=linux&model=meta-llama/Llama-3.1-8B-Instruct&compression=4bit", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMInstallScript(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp AirLLMInstallScript
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Filename != "install_airllm.sh" {
		t.Errorf("filename: got %q", resp.Filename)
	}
	if resp.Platform != "linux" {
		t.Errorf("platform: got %q", resp.Platform)
	}
	if !strings.Contains(resp.Script, "#!/usr/bin/env bash") {
		t.Errorf("script doesn't look like bash: %s", resp.Script)
	}
	if resp.Model != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("model echo: got %q", resp.Model)
	}
}

func TestInstallScript_ProbeOnlyEmitsDifferentScript(t *testing.T) {
	srv := newTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/install_script?platform=linux&probe_only=1", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMInstallScript(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp AirLLMInstallScript
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Filename != "probe_host.sh" {
		t.Errorf("probe filename: got %q", resp.Filename)
	}
	if !strings.Contains(resp.Script, "python_path") || !strings.Contains(resp.Script, "gpu_vendor") {
		t.Errorf("probe script should emit JSON probe; got %s", resp.Script)
	}
	// Probe script must NOT include the install-side pip step.
	if strings.Contains(resp.Script, "pip install airllm") {
		t.Errorf("probe script should NOT contain pip install airllm")
	}
}

func TestInstallScript_AcceptsPostBody(t *testing.T) {
	srv := newTestServer(t, nil)
	body := strings.NewReader(`{"platform":"windows","model":"x/y-8B","compression":"8bit","endpoint_port":9913}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/install_script", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMInstallScript(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp AirLLMInstallScript
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Platform != "windows" || resp.Filename != "install_airllm.ps1" {
		t.Errorf("windows POST: got platform=%q filename=%q", resp.Platform, resp.Filename)
	}
	if resp.EndpointPort != 9913 {
		t.Errorf("endpoint port: got %d", resp.EndpointPort)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Diagnostics handler — gate-off path
// ──────────────────────────────────────────────────────────────────────────

func TestDiagnostics_Works_WhenDockerGateOff(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	srv.docker = NewDockerControlState() // gate off
	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/diagnostics", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMDiagnostics(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (degrades cleanly when docker off), got %d body=%s", w.Code, w.Body.String())
	}
	var resp AirLLMDiagnostics
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.DockerSocketActive {
		t.Errorf("docker_socket_active should be false when gate is off")
	}
	if resp.SDCoreRuntime.GoVersion == "" {
		t.Errorf("runtime should always carry Go version")
	}
	// host_probe should carry the hint pointing at install_script.
	if resp.HostProbe.Hint == "" {
		t.Errorf("host_probe should carry a hint")
	}
}

func TestDiagnostics_MethodGuard(t *testing.T) {
	srv := newTestServer(t, nil)
	srv.docker = NewDockerControlState()
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/diagnostics", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMDiagnostics(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST want 405, got %d", w.Code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// portFromEndpoint
// ──────────────────────────────────────────────────────────────────────────

func TestPortFromEndpoint(t *testing.T) {
	cases := []struct {
		url, want string
	}{
		{"http://synaptic-airllm-tier3:9912", "9912"},
		{"http://localhost", ""},
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := portFromEndpoint(c.url); got != c.want {
			t.Errorf("portFromEndpoint(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
