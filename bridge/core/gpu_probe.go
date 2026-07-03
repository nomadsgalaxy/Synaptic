// gpu_probe.go — GPU vendor / CUDA-availability detection for the
// AirLLM diagnostics + configure_tier preflight.
//
// Why this matters: AirLLM's 4bit/8bit quantization path requires
// NVIDIA CUDA, but the failure surfaces only AFTER weights download
// (100+ GB at full precision). Users on AMD / Intel / Mac / CPU-only
// hardware burn the download, hit
//   RuntimeError: Found no NVIDIA driver on your system.
// during quantization, and have to start over with compression=none
// (very slow, needs N×16GB RAM) or switch to Ollama. This probe lets
// the dashboard warn before the user wires the sidecar.
//
// Detection strategy from inside SD Core's (non-GPU) container:
//
//  1. /proc/driver/nvidia/version — only present when the container
//     was started with the nvidia runtime + driver passthrough. SD
//     Core itself isn't started that way, so this almost never fires
//     for us, but it's a definitive positive when it does.
//
//  2. Docker daemon `/info` Runtimes map — the nvidia-container-toolkit
//     registers a "nvidia" runtime on install. Its presence is the
//     reliable host-level signal that an NVIDIA driver + CUDA are
//     available on the box, even when SD Core's own container has no
//     GPU access.
//
// AMD / Intel / Apple detection from inside a Linux container is
// substantially harder (no equivalent runtime signal) — we mark them
// as "none" today and leave a TODO. The frontend's warning still
// applies, since AirLLM 4bit/8bit needs CUDA regardless.
package main

import (
	"context"
	"os"
)

// GPUProbe is the host-GPU section of HostProbe. Fields chosen to
// match the brief's frontend contract; the warning string is
// populated only when the user is in danger (vendor != nvidia).
type GPUProbe struct {
	Vendor              string `json:"vendor"`                 // nvidia | amd | intel | apple | none
	Name                string `json:"name"`                   // empty when unknown
	CUDAAvailable       bool   `json:"cuda_available"`
	ROCmAvailable       bool   `json:"rocm_available"`         // reserved for AMD; always false today
	CUDARequiredFor4Bit bool   `json:"cuda_required_for_4bit"` // semantic hint for the FE
	Warning             string `json:"warning,omitempty"`      // surfaced inline by the FE when set
	DetectionSource     string `json:"detection_source,omitempty"`
}

// nvidiaProcVersionPath is the in-container path the nvidia runtime
// mounts when the host driver is exposed. Indirected so tests can
// inject a tempfile.
var nvidiaProcVersionPath = "/proc/driver/nvidia/version"

// dockerInfoFn is the system-info fetcher; tests swap it.
var dockerInfoFn = defaultDockerInfo

func defaultDockerInfo(ctx context.Context, docker *DockerControlState) (SystemInfo, error) {
	if docker == nil || docker.client == nil {
		return SystemInfo{}, nil
	}
	return docker.client.GetSystemInfo(ctx)
}

// probeGPU returns the host's GPU posture as best SD Core can tell
// from inside its container. Never errors — a probe miss becomes
// vendor:"none" with a diagnostic detection_source.
func probeGPU(ctx context.Context, docker *DockerControlState) GPUProbe {
	out := GPUProbe{
		Vendor:              "none",
		CUDARequiredFor4Bit: true,
		DetectionSource:     "no_signal",
	}

	// (1) Direct driver passthrough — definitive when present.
	if _, err := os.Stat(nvidiaProcVersionPath); err == nil {
		out.Vendor = "nvidia"
		out.CUDAAvailable = true
		out.DetectionSource = "proc_driver_nvidia"
		return out
	}

	// (2) Docker daemon Runtimes — strongest host-level signal we have.
	info, err := dockerInfoFn(ctx, docker)
	if err == nil {
		if _, hasNvidia := info.Runtimes["nvidia"]; hasNvidia {
			out.Vendor = "nvidia"
			out.CUDAAvailable = true
			out.DetectionSource = "docker_info_runtime_nvidia"
			return out
		}
	}

	// No NVIDIA signal found. Set the warning so the frontend's profile
	// card renders the red banner.
	out.Warning = "AirLLM 4bit quantization requires NVIDIA CUDA. Detected: none. Model load will fail."
	return out
}

// airllmCompressionRequiresCUDA returns true when the given compression
// mode hits the CUDA-only quantization path. Empty compression is the
// AirLLM headline default (4bit), so we treat unset as 4bit for the
// purposes of the gate. Only the explicit "none" lets the user opt
// into the CPU-only path.
func airllmCompressionRequiresCUDA(compression string) bool {
	switch compression {
	case "4bit", "8bit", "":
		return true
	default:
		return false
	}
}
