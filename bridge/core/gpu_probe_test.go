// gpu_probe_test.go — covers the NVIDIA-detection logic in
// probeGPU + the airllm-compression-requires-CUDA helper. The live
// /proc and /info paths are exercised in the rebuild-and-verify
// step; tests here inject deterministic substitutes.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withMockDockerInfo swaps the package-level dockerInfoFn for the
// duration of the test, then restores it.
func withMockDockerInfo(t *testing.T, fn func(ctx context.Context, docker *DockerControlState) (SystemInfo, error)) {
	t.Helper()
	prev := dockerInfoFn
	dockerInfoFn = fn
	t.Cleanup(func() { dockerInfoFn = prev })
}

// withMockNvidiaProc points nvidiaProcVersionPath at a temp file (or
// a non-existent path) so the /proc check has a deterministic outcome.
func withMockNvidiaProc(t *testing.T, present bool) {
	t.Helper()
	prev := nvidiaProcVersionPath
	dir := t.TempDir()
	p := filepath.Join(dir, "nvidia_version")
	if present {
		if err := os.WriteFile(p, []byte("NVRM version: 555.42.06\n"), 0o644); err != nil {
			t.Fatalf("write mock proc: %v", err)
		}
	}
	nvidiaProcVersionPath = p
	t.Cleanup(func() { nvidiaProcVersionPath = prev })
}

// ──────────────────────────────────────────────────────────────────────────
// probeGPU
// ──────────────────────────────────────────────────────────────────────────

func TestProbeGPU_NoSignal_ReturnsNoneWithWarning(t *testing.T) {
	withMockNvidiaProc(t, false)
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		return SystemInfo{Runtimes: map[string]struct{}{"runc": {}}}, nil
	})

	gpu := probeGPU(context.Background(), nil)
	if gpu.Vendor != "none" {
		t.Errorf("no signal: want vendor=none, got %q", gpu.Vendor)
	}
	if gpu.CUDAAvailable {
		t.Errorf("CUDAAvailable should be false")
	}
	if !gpu.CUDARequiredFor4Bit {
		t.Errorf("CUDARequiredFor4Bit must always be true (it's a contract for the FE)")
	}
	if gpu.Warning == "" {
		t.Errorf("non-nvidia must carry a warning string")
	}
	if !strings.Contains(gpu.Warning, "AirLLM") || !strings.Contains(gpu.Warning, "CUDA") {
		t.Errorf("warning text should mention AirLLM + CUDA; got %q", gpu.Warning)
	}
}

func TestProbeGPU_DockerNvidiaRuntime_DetectsNvidia(t *testing.T) {
	withMockNvidiaProc(t, false)
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		return SystemInfo{Runtimes: map[string]struct{}{
			"runc":   {},
			"nvidia": {},
		}}, nil
	})

	gpu := probeGPU(context.Background(), nil)
	if gpu.Vendor != "nvidia" {
		t.Errorf("nvidia runtime present: want vendor=nvidia, got %q", gpu.Vendor)
	}
	if !gpu.CUDAAvailable {
		t.Errorf("CUDAAvailable should be true when nvidia runtime is registered")
	}
	if gpu.Warning != "" {
		t.Errorf("nvidia detected should NOT carry a warning; got %q", gpu.Warning)
	}
	if gpu.DetectionSource != "docker_info_runtime_nvidia" {
		t.Errorf("detection_source: got %q", gpu.DetectionSource)
	}
}

func TestProbeGPU_ProcDriverNvidiaWinsOverDockerInfo(t *testing.T) {
	// /proc passthrough is the strongest positive — should short-circuit
	// before we even hit /info.
	withMockNvidiaProc(t, true)
	called := 0
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		called++
		return SystemInfo{Runtimes: map[string]struct{}{}}, nil
	})

	gpu := probeGPU(context.Background(), nil)
	if gpu.Vendor != "nvidia" {
		t.Errorf("proc driver present: want vendor=nvidia, got %q", gpu.Vendor)
	}
	if gpu.DetectionSource != "proc_driver_nvidia" {
		t.Errorf("detection_source should be proc_driver_nvidia; got %q", gpu.DetectionSource)
	}
	if called != 0 {
		t.Errorf("proc-driver hit should short-circuit before dockerInfoFn; got %d calls", called)
	}
}

func TestProbeGPU_DockerInfoError_FallsBackToNone(t *testing.T) {
	withMockNvidiaProc(t, false)
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		return SystemInfo{}, context.DeadlineExceeded
	})

	gpu := probeGPU(context.Background(), nil)
	if gpu.Vendor != "none" {
		t.Errorf("docker info error: want vendor=none, got %q", gpu.Vendor)
	}
	if gpu.Warning == "" {
		t.Errorf("warning should be present on fall-through")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// airllmCompressionRequiresCUDA
// ──────────────────────────────────────────────────────────────────────────

func TestAirllmCompressionRequiresCUDA(t *testing.T) {
	cases := map[string]bool{
		"4bit": true,
		"8bit": true,
		"":     true, // AirLLM's headline default is 4bit — gate by default
		"none": false,
		"fp16": false,
	}
	for in, want := range cases {
		if got := airllmCompressionRequiresCUDA(in); got != want {
			t.Errorf("airllmCompressionRequiresCUDA(%q) = %v, want %v", in, got, want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// configure_tier — cuda_required gate
// ──────────────────────────────────────────────────────────────────────────

func TestConfigureTier_NoNvidia_4bit_ReturnsCUDARequired(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	srv.docker = NewDockerControlState()

	withMockNvidiaProc(t, false)
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		return SystemInfo{Runtimes: map[string]struct{}{"runc": {}}}, nil
	})

	// Endpoint deliberately unreachable so Ping fails and the GPU
	// preflight gate runs.
	body := strings.NewReader(`{"tier":"tier2","endpoint":"http://127.0.0.1:1","model":"meta-llama/Llama-3.1-70B-Instruct","compression":"4bit"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/configure_tier", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMConfigureTier(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 cuda_required, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "cuda_required" {
		t.Errorf("error field: want cuda_required, got %v", resp["error"])
	}
	hint, _ := resp["hint"].(string)
	if !strings.Contains(hint, "NVIDIA") || !strings.Contains(hint, "Ollama") {
		t.Errorf("hint should mention NVIDIA + Ollama fallback; got %q", hint)
	}
	// Provider config MUST NOT be persisted on cuda_required.
	cfg, _ := bank.GetProviderConfig(Tier2)
	if cfg.Kind == ProviderAirLLM {
		t.Errorf("config persisted despite cuda_required; should be rejected pre-save")
	}
}

func TestConfigureTier_NoNvidia_NoneCompression_StillGetsUnreachable(t *testing.T) {
	// compression=none opts out of the CUDA path. The gate should NOT
	// fire, so the user falls through to the regular unreachable error
	// (since the test endpoint is bogus).
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	srv.docker = NewDockerControlState()

	withMockNvidiaProc(t, false)
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		return SystemInfo{Runtimes: map[string]struct{}{}}, nil
	})

	body := strings.NewReader(`{"tier":"tier2","endpoint":"http://127.0.0.1:1","model":"m","compression":"none"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/airllm/configure_tier", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMConfigureTier(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 unreachable, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "unreachable" {
		t.Errorf("compression=none should bypass cuda gate; want unreachable error, got %v", resp["error"])
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HostProbe shape — diagnostics now exposes GPU
// ──────────────────────────────────────────────────────────────────────────

func TestDiagnostics_HostProbeIncludesGPU(t *testing.T) {
	bank := newTestBank(t)
	srv := newTestServer(t, bank)
	srv.docker = NewDockerControlState()

	withMockNvidiaProc(t, false)
	withMockDockerInfo(t, func(context.Context, *DockerControlState) (SystemInfo, error) {
		return SystemInfo{Runtimes: map[string]struct{}{"runc": {}}}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/airllm/diagnostics", nil)
	w := httptest.NewRecorder()
	srv.handleAdminAirLLMDiagnostics(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp AirLLMDiagnostics
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.HostProbe.GPU.Vendor != "none" {
		t.Errorf("expect vendor=none on test host without nvidia; got %q", resp.HostProbe.GPU.Vendor)
	}
	if !resp.HostProbe.GPU.CUDARequiredFor4Bit {
		t.Errorf("cuda_required_for_4bit must be true (FE contract)")
	}
	if resp.HostProbe.GPU.Warning == "" {
		t.Errorf("non-nvidia must carry a warning")
	}
}
