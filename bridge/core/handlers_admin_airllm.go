// handlers_admin_airllm.go — Wave 7c3 AirLLM-specific admin endpoints.
//
// Two endpoints:
//
//   GET /admin/airllm/diagnostics  — deploy-readiness probe
//   GET /admin/airllm/install_script — generate platform-tailored
//                                      install script for native install
//
// Both honour the existing Bearer-token auth + CORS conventions. The
// diagnostics endpoint does NOT require the Docker gate to be on — it
// degrades gracefully when SD_ALLOW_DOCKER_SOCKET=0 (just omits the
// docker-probed sections). install_script needs no gate; it's pure
// template generation.
//
// What SD Core can see from inside a Linux container:
//   - Its own runtime info (Go version, CPU count, memory limits)
//   - Configured tier endpoints (probed via the existing AirLLMProvider.Ping)
//   - Sibling docker containers whose image name contains "airllm" (gated)
//   - host.docker.internal reachability + scan for native sidecars on
//     ports 9912-9920 on that host
//   - The compose working_dir label (gives us a strong host-OS hint via
//     path separator)
//
// What we CANNOT see (without host-side help):
//   - Host's Python/pip/torch/CUDA versions
//   - Host's free disk / RAM / GPU details
//
// For the host-side bits the diagnostics response carries a `host_probe`
// hint pointing at the install_script's `--probe-only` mode (the script
// can be run with that flag to emit a JSON dump the user pastes back).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// AirLLMDiagnostics is the response shape for GET /admin/airllm/diagnostics.
type AirLLMDiagnostics struct {
	HostProbe          HostProbe                   `json:"host_probe"`
	SDCoreRuntime      SDCoreRuntime               `json:"sd_core_runtime"`
	AirLLMContainers   []AirLLMContainerInfo       `json:"airllm_containers"`
	ConfiguredTiers    []AirLLMConfiguredTier      `json:"configured_tiers"`
	PortsScanned       []AirLLMPortProbe           `json:"ports_scanned"`
	DockerSocketActive bool                        `json:"docker_socket_active"`
	GeneratedAt        string                      `json:"generated_at"`
}

type HostProbe struct {
	DetectedOS         string `json:"detected_os"`           // windows | macos | linux | unknown
	DetectionSource   string `json:"detection_source"`      // "compose_working_dir" | "envvar" | "unknown"
	ComposeWorkingDir string `json:"compose_working_dir,omitempty"`
	Hint              string `json:"hint"`
	GPU               GPUProbe `json:"gpu"`
}

type SDCoreRuntime struct {
	GoVersion            string `json:"go_version"`
	NumCPU               int    `json:"num_cpu"`
	NumGoroutine         int    `json:"num_goroutine"`
	HostDockerInternalUp bool   `json:"host_docker_internal_reachable"`
}

type AirLLMContainerInfo struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Image    string `json:"image"`
	Endpoint string `json:"endpoint"`
}

type AirLLMConfiguredTier struct {
	Tier      string `json:"tier"`
	Kind      string `json:"kind"`
	Endpoint  string `json:"endpoint,omitempty"`
	Reachable bool   `json:"reachable"`
	Loaded    bool   `json:"loaded,omitempty"`
	Model     string `json:"model,omitempty"`
	Error     string `json:"error,omitempty"`
}

type AirLLMPortProbe struct {
	Target    string `json:"target"`              // host:port
	Reachable bool   `json:"reachable"`
	Loaded    bool   `json:"loaded,omitempty"`
	Model     string `json:"model,omitempty"`
}

// handleAdminAirLLMDiagnostics serves GET /admin/airllm/diagnostics.
func (s *Server) handleAdminAirLLMDiagnostics(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	out := AirLLMDiagnostics{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SDCoreRuntime: SDCoreRuntime{
			GoVersion:    runtime.Version(),
			NumCPU:       runtime.NumCPU(),
			NumGoroutine: runtime.NumGoroutine(),
		},
	}
	// Host-OS probe via compose working_dir path separator. Best-effort —
	// if we can't reach the daemon we still report "unknown" cleanly.
	out.HostProbe = probeHostOS(r.Context(), s.docker)
	// GPU probe — vendor + CUDA availability. Drives the FE's red-banner
	// warning for AirLLM 4bit/8bit configs on non-NVIDIA hardware.
	out.HostProbe.GPU = probeGPU(r.Context(), s.docker)

	// host.docker.internal reachability — useful for users running the
	// AirLLM Python sidecar natively on the host while SD Core is in Docker.
	out.SDCoreRuntime.HostDockerInternalUp = canDialTCP("host.docker.internal:80", 2*time.Second)

	// Docker-gated sections.
	if s.docker != nil && s.docker.socketAllowed {
		out.DockerSocketActive = true
		out.AirLLMContainers = listAirLLMContainers(r.Context(), s.docker)
	}

	// Configured tier endpoints — probe each tier whose kind is airllm.
	out.ConfiguredTiers = probeConfiguredAirLLMTiers(r.Context(), s.bank, s.router)

	// Port scan against host.docker.internal and the docker network for
	// any sidecar on 9912-9920. Cheap parallel probes with a tight cap.
	out.PortsScanned = scanAirLLMPorts(r.Context(), out.AirLLMContainers, out.SDCoreRuntime.HostDockerInternalUp)

	writeJSON(w, http.StatusOK, out)
}

// probeHostOS reads our own container's com.docker.compose.project.working_dir
// label and guesses the host OS from path separator.
func probeHostOS(ctx context.Context, docker *DockerControlState) HostProbe {
	out := HostProbe{
		DetectedOS:      "unknown",
		DetectionSource: "unknown",
		Hint:            "For host Python / GPU / disk diagnostics, download the install_script with ?probe_only=1 and run it on your host.",
	}
	// Honour an explicit override if set.
	if v := envOr("SD_HOST_OS", ""); v != "" {
		out.DetectedOS = v
		out.DetectionSource = "envvar"
		return out
	}
	if docker == nil || !docker.socketAllowed || docker.client == nil {
		return out
	}
	selfName := envOr("SD_SELF_CONTAINER_NAME", envOr("HOSTNAME", "synaptic-core"))
	insp, err := docker.client.InspectContainer(ctx, selfName)
	if err != nil {
		return out
	}
	workDir := insp.Labels["com.docker.compose.project.working_dir"]
	if workDir == "" {
		return out
	}
	out.ComposeWorkingDir = workDir
	out.DetectionSource = "compose_working_dir"
	switch {
	case strings.Contains(workDir, `\`) || (len(workDir) >= 2 && workDir[1] == ':'):
		out.DetectedOS = "windows"
	case strings.HasPrefix(workDir, "/Users/"):
		out.DetectedOS = "macos"
	case strings.HasPrefix(workDir, "/home/") || strings.HasPrefix(workDir, "/root/") || strings.HasPrefix(workDir, "/opt/") || strings.HasPrefix(workDir, "/srv/"):
		out.DetectedOS = "linux"
	default:
		// Unix-ish path but no strong hint — leave as "unknown" so the UI
		// asks the user.
		out.DetectedOS = "unknown"
	}
	return out
}

// canDialTCP attempts a quick TCP connection to "host:port". Returns
// false on any error.
func canDialTCP(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// resolveAirLLMEndpointForSDCore rewrites loopback endpoints to the
// compose-DNS endpoint of a sibling AirLLM container when SD Core is
// running inside a container with the docker socket available. Native
// SD Core installs (no docker control plane, or no sibling container)
// fall through with the user's input unchanged.
//
// Detection is best-effort: if we can't see an airllm container, we
// trust the user (they may be pointing at a native AirLLM sidecar
// running on the host).
func resolveAirLLMEndpointForSDCore(ctx context.Context, s *Server, endpoint string) string {
	if endpoint == "" {
		return endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	host := u.Hostname()
	// Only rewrite when the user's endpoint clearly points at this
	// container's own loopback. Don't touch host.docker.internal,
	// real DNS names, or LAN IPs.
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return endpoint
	}
	if s == nil || s.docker == nil {
		return endpoint
	}
	containers := listAirLLMContainers(ctx, s.docker)
	for _, c := range containers {
		if c.Endpoint != "" {
			// Found a sibling — use its compose-DNS endpoint.
			return c.Endpoint
		}
	}
	return endpoint
}

// listAirLLMContainers walks the docker container list and returns those
// whose image looks like AirLLM. Best-effort; daemon errors yield an
// empty list rather than a 500.
func listAirLLMContainers(ctx context.Context, docker *DockerControlState) []AirLLMContainerInfo {
	out := []AirLLMContainerInfo{}
	if docker == nil || docker.client == nil {
		return out
	}
	// Match the compose project so we don't surface unrelated AirLLM
	// containers from other stacks on the same host.
	containers, err := docker.client.ListContainers(ctx, map[string]string{
		"com.docker.compose.project": docker.composeProject,
	})
	if err != nil {
		return out
	}
	for _, c := range containers {
		name := primaryName(c.Names)
		if !strings.Contains(strings.ToLower(c.Image), "airllm") &&
			!strings.Contains(strings.ToLower(name), "airllm") {
			continue
		}
		port := 9912
		if len(c.Ports) > 0 {
			port = c.Ports[0].PrivatePort
		}
		out = append(out, AirLLMContainerInfo{
			Name:     name,
			State:    c.State,
			Image:    c.Image,
			Endpoint: fmt.Sprintf("http://%s:%d", name, port),
		})
	}
	return out
}

// probeConfiguredAirLLMTiers reads the provider configs for tiers
// 1/2/3/embedding and pings each tier whose kind is airllm.
func probeConfiguredAirLLMTiers(ctx context.Context, bank *Bank, router *ModelRouter) []AirLLMConfiguredTier {
	out := []AirLLMConfiguredTier{}
	if bank == nil {
		return out
	}
	for _, tier := range AllTierKeys {
		cfg, err := bank.GetProviderConfig(tier)
		if err != nil {
			continue
		}
		if cfg.Kind != ProviderAirLLM {
			continue
		}
		entry := AirLLMConfiguredTier{
			Tier:     string(tier),
			Kind:     string(cfg.Kind),
			Endpoint: cfg.BaseURL,
		}
		// Use the existing AirLLMProvider to ping. We don't have the
		// fully-wired provider here, so build a probe one.
		probe := &AirLLMProvider{
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := probe.Ping(probeCtx); err == nil {
			entry.Reachable = true
			entry.Loaded = true
			entry.Model = cfg.Model
		} else {
			entry.Error = err.Error()
		}
		cancel()
		out = append(out, entry)
	}
	return out
}

// scanAirLLMPorts probes a small set of targets for AirLLM-shaped
// /healthz responses. Targets:
//   - Each detected AirLLM container's compose-DNS name
//   - host.docker.internal:9912..9920 (when reachable)
//
// Done in parallel with a tight per-probe timeout.
func scanAirLLMPorts(ctx context.Context, containers []AirLLMContainerInfo, hostInternalUp bool) []AirLLMPortProbe {
	type target struct {
		display string
		url     string
	}
	var targets []target
	for _, c := range containers {
		targets = append(targets, target{
			display: c.Name + ":" + portFromEndpoint(c.Endpoint),
			url:     c.Endpoint + "/healthz",
		})
	}
	if hostInternalUp {
		// Common ports. The user's install_script defaults to 9912; we
		// Wave 8c retired the dedicated Tier 3 container; the canonical
		// AirLLM port is 9912 (the single airllm-tier2 service serves
		// both Tier 2 and Tier 3 traffic when the user picks AirLLM at
		// either tier). 9913 still scanned for back-compat with users
		// who ran a separate native-install Tier 3 sidecar on the
		// pre-8c convention.
		for _, port := range []int{9912, 9913} {
			targets = append(targets, target{
				display: fmt.Sprintf("host.docker.internal:%d", port),
				url:     fmt.Sprintf("http://host.docker.internal:%d/healthz", port),
			})
		}
	}
	out := make([]AirLLMPortProbe, len(targets))
	cl := &http.Client{Timeout: 2 * time.Second}
	for i, t := range targets {
		out[i].Target = t.display
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
		resp, err := cl.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			out[i].Reachable = true
			var body struct {
				Loaded bool   `json:"loaded"`
				Model  string `json:"model"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			out[i].Loaded = body.Loaded
			out[i].Model = body.Model
		}
		resp.Body.Close()
	}
	return out
}

// portFromEndpoint extracts the port number from a URL like
// "http://host:9912". Returns "" when the URL has no explicit port.
func portFromEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	if p := u.Port(); p != "" {
		return p
	}
	return ""
}

// ──────────────────────────────────────────────────────────────────────────
// /admin/airllm/install_script — generate platform-tailored install script
// ──────────────────────────────────────────────────────────────────────────

// AirLLMInstallScript is the response body.
type AirLLMInstallScript struct {
	Filename                 string `json:"filename"`
	Platform                 string `json:"platform"`
	Script                   string `json:"script"`
	EstimatedDiskGB          int    `json:"estimated_disk_gb"`
	EstimatedFirstRunMinutes int    `json:"estimated_first_run_minutes"`
	Model                    string `json:"model"`
	Compression              string `json:"compression"`
	EndpointPort             int    `json:"endpoint_port"`
}

// handleAdminAirLLMInstallScript serves GET/POST /admin/airllm/install_script.
// Query / body params:
//
//   platform=linux|macos|windows   (required; auto-detect from host_probe if absent)
//   model=<hf-id>                  (default: meta-llama/Llama-3.1-70B-Instruct)
//   compression=4bit|8bit|none     (default: 4bit)
//   port=<int>                     (default: 9912)
//   venv=<path>                    (default platform-specific)
//   probe_only=1                   (return a one-shot script that emits JSON
//                                    diagnostics instead of installing — used
//                                    to fill in host-side fields the
//                                    diagnostics endpoint can't see)
func (s *Server) handleAdminAirLLMInstallScript(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, "GET", "POST", "OPTIONS")
		return
	}
	// Read params from query first; POST body overrides.
	params := readAirLLMScriptParams(r)
	if params.Platform == "" {
		// Auto-detect from host_probe.
		hp := probeHostOS(r.Context(), s.docker)
		params.Platform = hp.DetectedOS
	}
	if params.Platform == "" || params.Platform == "unknown" {
		http.Error(w, "platform is required (linux|macos|windows); set ?platform= explicitly or set SD_HOST_OS", http.StatusBadRequest)
		return
	}
	if params.Model == "" {
		params.Model = "meta-llama/Llama-3.1-70B-Instruct"
	}
	if params.Compression == "" {
		params.Compression = "4bit"
	}
	if params.EndpointPort == 0 {
		params.EndpointPort = 9912
	}
	probeOnly := r.URL.Query().Get("probe_only") == "1"
	script, filename := buildAirLLMInstallScript(params, probeOnly)
	writeJSON(w, http.StatusOK, AirLLMInstallScript{
		Filename:                 filename,
		Platform:                 params.Platform,
		Script:                   script,
		EstimatedDiskGB:          estimateDiskGB(params.Model, params.Compression),
		EstimatedFirstRunMinutes: estimateFirstRunMinutes(params.Model),
		Model:                    params.Model,
		Compression:              params.Compression,
		EndpointPort:             params.EndpointPort,
	})
}

type airllmScriptParams struct {
	Platform     string `json:"platform"`
	Model        string `json:"model"`
	Compression  string `json:"compression"`
	EndpointPort int    `json:"endpoint_port"`
	VenvPath     string `json:"venv"`
}

func readAirLLMScriptParams(r *http.Request) airllmScriptParams {
	q := r.URL.Query()
	p := airllmScriptParams{
		Platform:    strings.ToLower(q.Get("platform")),
		Model:       q.Get("model"),
		Compression: strings.ToLower(q.Get("compression")),
		VenvPath:    q.Get("venv"),
	}
	if v := q.Get("port"); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		p.EndpointPort = n
	}
	if r.Method == http.MethodPost {
		var body airllmScriptParams
		if err := readJSON(r, &body); err == nil {
			if body.Platform != "" {
				p.Platform = strings.ToLower(body.Platform)
			}
			if body.Model != "" {
				p.Model = body.Model
			}
			if body.Compression != "" {
				p.Compression = strings.ToLower(body.Compression)
			}
			if body.EndpointPort != 0 {
				p.EndpointPort = body.EndpointPort
			}
			if body.VenvPath != "" {
				p.VenvPath = body.VenvPath
			}
		}
	}
	return p
}

func buildAirLLMInstallScript(p airllmScriptParams, probeOnly bool) (script, filename string) {
	switch p.Platform {
	case "windows":
		filename = "install_airllm.ps1"
		if probeOnly {
			filename = "probe_host.ps1"
			return buildWindowsProbeScript(), filename
		}
		return buildWindowsInstallScript(p), filename
	default: // linux + macos share the bash template
		filename = "install_airllm.sh"
		if probeOnly {
			filename = "probe_host.sh"
			return buildBashProbeScript(), filename
		}
		return buildBashInstallScript(p), filename
	}
}

// estimateDiskGB returns a rough disk estimate for the chosen model +
// compression. 70B at 4-bit is ~40GB; 70B at 8-bit ~75GB; 8B at 4-bit ~5GB.
func estimateDiskGB(model, compression string) int {
	size := 8
	switch {
	case strings.Contains(model, "70B"):
		size = 70
	case strings.Contains(model, "405B"):
		size = 405
	case strings.Contains(model, "8B"):
		size = 8
	case strings.Contains(model, "3B"):
		size = 3
	}
	mult := 1.5 // float16 baseline ~= 2× params in GB; .75 for 4bit, 1 for 8bit
	switch compression {
	case "4bit":
		mult = 0.6
	case "8bit":
		mult = 1.1
	case "none":
		mult = 2.0
	}
	return int(float64(size) * mult)
}

// estimateFirstRunMinutes is a crude "expect this many minutes for the
// first run" estimate dominated by model download time at typical home
// connection speeds (~50 Mbps).
func estimateFirstRunMinutes(model string) int {
	gb := estimateDiskGB(model, "4bit")
	// At 50 Mbps ≈ 6.25 MB/s → ~16s/GB → 0.27 min/GB → round up
	minutes := (gb*16 + 59) / 60
	if minutes < 3 {
		return 3
	}
	return minutes + 2 // +2 min for venv setup
}

// ──────────────────────────────────────────────────────────────────────────
// Script templates
// ──────────────────────────────────────────────────────────────────────────

func buildBashInstallScript(p airllmScriptParams) string {
	venv := p.VenvPath
	if venv == "" {
		venv = `"${HOME}/.synaptic/airllm-venv"`
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
# Synaptic — AirLLM native install (generated)
#
# This script installs AirLLM into a dedicated Python venv on your host
# and starts the sidecar listening on 127.0.0.1:%d. The dashboard's
# /admin/airllm/diagnostics endpoint will detect it once running.
#
# Run once. After it finishes, the model downloads + loads on first
# inference call (which may take several minutes for a 70B model).
set -euo pipefail

MODEL=%q
COMPRESSION=%q
PORT=%d
VENV=%s

echo "Synaptic AirLLM installer"
echo "  Model:       $MODEL"
echo "  Compression: $COMPRESSION"
echo "  Port:        $PORT"
echo "  Venv:        $VENV"
echo ""

# Detect Python 3.10+.
if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 not found. Install Python 3.10+ from https://python.org and rerun."
  exit 1
fi
PY_VER=$(python3 -c 'import sys; print("{}.{}".format(*sys.version_info[:2]))')
echo "Found Python $PY_VER"

mkdir -p "$(dirname "$VENV")"
python3 -m venv "$VENV"
# shellcheck source=/dev/null
source "$VENV/bin/activate"
pip install --upgrade pip
pip install airllm huggingface_hub torch

# Sidecar script lives in your Synaptic repo. Adjust SD_REPO to your
# actual checkout path if different.
SD_REPO="${SD_REPO:-${HOME}/Synaptic-Disorder}"
if [ ! -f "$SD_REPO/bridge/airllm/server.py" ]; then
  echo "WARNING: bridge/airllm/server.py not found at $SD_REPO."
  echo "Set SD_REPO=/path/to/Synaptic-Disorder and rerun, or copy server.py manually."
  exit 1
fi

nohup python "$SD_REPO/bridge/airllm/server.py" \
  --listen "127.0.0.1:$PORT" \
  --model "$MODEL" \
  --compression "$COMPRESSION" \
  > "$VENV/airllm.log" 2>&1 &
PID=$!

echo ""
echo "AirLLM started (pid=$PID). Logs: $VENV/airllm.log"
echo "Configure SD Core's Tier 2 or Tier 3 to:"
echo "  Kind:        AirLLM"
echo "  Endpoint:    http://host.docker.internal:$PORT  (if SD Core runs in Docker)"
echo "               http://127.0.0.1:$PORT             (if SD Core runs natively)"
echo ""
echo "The model downloads on first inference call (~%d min for $MODEL)."
`, p.EndpointPort, p.Model, p.Compression, p.EndpointPort, venv, estimateFirstRunMinutes(p.Model))
}

func buildWindowsInstallScript(p airllmScriptParams) string {
	venv := p.VenvPath
	if venv == "" {
		venv = `"$env:USERPROFILE\.synaptic\airllm-venv"`
	}
	// PowerShell uses backticks for line continuation, which would
	// terminate a Go raw-string. We use the BT placeholder and
	// substitute literal backticks back in at the end.
	const BT = "<<BT>>"
	tmpl := `# Synaptic — AirLLM native install (Windows / PowerShell)
#
# This script installs AirLLM into a dedicated Python venv on your host
# and starts the sidecar listening on 127.0.0.1:%d. The dashboard's
# /admin/airllm/diagnostics endpoint will detect it once running.
$ErrorActionPreference = "Stop"

$MODEL = %q
$COMPRESSION = %q
$PORT = %d
$VENV = %s

Write-Host "Synaptic AirLLM installer"
Write-Host "  Model:       $MODEL"
Write-Host "  Compression: $COMPRESSION"
Write-Host "  Port:        $PORT"
Write-Host "  Venv:        $VENV"
Write-Host ""

# Detect Python 3.10+.
$pythonCmd = $null
foreach ($candidate in @("python3", "python", "py")) {
    if (Get-Command $candidate -ErrorAction SilentlyContinue) {
        $pythonCmd = $candidate
        break
    }
}
if (-not $pythonCmd) {
    Write-Error "Python 3.10+ not found. Install from https://python.org and rerun."
    exit 1
}
Write-Host "Found Python: $pythonCmd"

New-Item -ItemType Directory -Force -Path (Split-Path $VENV -Parent) | Out-Null
& $pythonCmd -m venv $VENV
& "$VENV\Scripts\python.exe" -m pip install --upgrade pip
& "$VENV\Scripts\pip.exe" install airllm huggingface_hub torch

$SD_REPO = $env:SD_REPO
if (-not $SD_REPO) { $SD_REPO = "$env:USERPROFILE\Synaptic-Disorder" }
$SERVER = "$SD_REPO\bridge\airllm\server.py"
if (-not (Test-Path $SERVER)) {
    Write-Error "bridge/airllm/server.py not found at $SERVER. Set <<BT>>$env:SD_REPO and rerun."
    exit 1
}

$LogFile = "$VENV\airllm.log"
Start-Process -FilePath "$VENV\Scripts\python.exe" <<BT>>
    -ArgumentList @($SERVER, "--listen", "127.0.0.1:$PORT", "--model", $MODEL, "--compression", $COMPRESSION) <<BT>>
    -RedirectStandardOutput $LogFile -RedirectStandardError $LogFile -NoNewWindow

Write-Host ""
Write-Host "AirLLM started. Logs: $LogFile"
Write-Host "Configure SD Core's Tier 2 or Tier 3 to:"
Write-Host "  Kind:     AirLLM"
Write-Host "  Endpoint: http://host.docker.internal:$PORT  (if SD Core runs in Docker)"
Write-Host "            http://127.0.0.1:$PORT             (if SD Core runs natively)"
Write-Host ""
Write-Host "The model downloads on first inference call (~%d min for $MODEL)."
`
	out := fmt.Sprintf(tmpl, p.EndpointPort, p.Model, p.Compression, p.EndpointPort, venv, estimateFirstRunMinutes(p.Model))
	return strings.ReplaceAll(out, BT, "`")
}

func buildBashProbeScript() string {
	return `#!/usr/bin/env bash
# Synaptic — Host probe (generated, probe-only)
#
# Emits a JSON diagnostics blob to stdout. Run on your host (not inside
# Docker) and paste the output into the dashboard's diagnostics card.
set -u

json_escape() { python3 -c 'import json,sys; print(json.dumps(sys.stdin.read().strip()))'; }

PYTHON_PATH=$(command -v python3 || echo "")
PIP_PATH=$(command -v pip3 || command -v pip || echo "")
PYTHON_VERSION=""
if [ -n "$PYTHON_PATH" ]; then
  PYTHON_VERSION=$("$PYTHON_PATH" --version 2>&1)
fi

DISK_FREE_GB=$(df -k "${HOME:-/}" 2>/dev/null | awk 'NR==2 {printf "%.1f", $4/1024/1024}')
RAM_GB=""
if command -v free >/dev/null 2>&1; then
  RAM_GB=$(free -g | awk '/^Mem:/ {print $2}')
elif command -v sysctl >/dev/null 2>&1; then
  RAM_GB=$(sysctl -n hw.memsize 2>/dev/null | awk '{printf "%.0f", $1/1024/1024/1024}')
fi

GPU_VENDOR=""
GPU_NAME=""
GPU_VRAM_GB=""
if command -v nvidia-smi >/dev/null 2>&1; then
  GPU_VENDOR="nvidia"
  GPU_NAME=$(nvidia-smi --query-gpu=name --format=csv,noheader,nounits | head -n 1 | sed 's/^ *//; s/ *$//')
  GPU_VRAM_GB=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits | head -n 1 | awk '{printf "%.0f", $1/1024}')
fi

cat <<EOF
{
  "python_path": "$PYTHON_PATH",
  "python_version": "$PYTHON_VERSION",
  "pip_path": "$PIP_PATH",
  "disk_free_gb": $DISK_FREE_GB,
  "ram_gb": "$RAM_GB",
  "gpu_vendor": "$GPU_VENDOR",
  "gpu_name": "$GPU_NAME",
  "gpu_vram_gb": "$GPU_VRAM_GB"
}
EOF
`
}

func buildWindowsProbeScript() string {
	return `# Synaptic — Host probe (Windows, PowerShell, generated)
$python = (Get-Command python -ErrorAction SilentlyContinue).Source
if (-not $python) { $python = (Get-Command python3 -ErrorAction SilentlyContinue).Source }
$pythonVersion = ""
if ($python) { $pythonVersion = (& $python --version 2>&1) }
$pip = (Get-Command pip -ErrorAction SilentlyContinue).Source

$disk = Get-PSDrive -Name (Split-Path $env:USERPROFILE -Qualifier).Trim(":") -ErrorAction SilentlyContinue
$diskFreeGB = if ($disk) { [math]::Round($disk.Free / 1GB, 1) } else { 0 }

$ramGB = [math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB, 0)

$gpu = Get-CimInstance Win32_VideoController | Where-Object { $_.AdapterRAM -gt 0 } | Select-Object -First 1
$gpuVendor = if ($gpu.Name -match "NVIDIA") { "nvidia" } elseif ($gpu.Name -match "AMD") { "amd" } else { "" }
$gpuName = if ($gpu) { $gpu.Name } else { "" }
$gpuVramGB = if ($gpu) { [math]::Round($gpu.AdapterRAM / 1GB, 0) } else { 0 }

$blob = @{
    python_path = if ($python) { $python } else { "" }
    python_version = $pythonVersion
    pip_path = if ($pip) { $pip } else { "" }
    disk_free_gb = $diskFreeGB
    ram_gb = $ramGB
    gpu_vendor = $gpuVendor
    gpu_name = $gpuName
    gpu_vram_gb = $gpuVramGB
}
$blob | ConvertTo-Json
`
}

// ──────────────────────────────────────────────────────────────────────────
// Wave 8a — configure_tier + health/{port}
// ──────────────────────────────────────────────────────────────────────────

// handleAdminAirLLMConfigureTier serves POST /admin/airllm/configure_tier.
//
// Atomic alternative to a multi-step Settings → Connection edit: ping the
// AirLLM endpoint, refuse to persist if unreachable, otherwise save the
// provider config. Body:
//
//   { "tier": "tier2_provider"|"tier3_provider"|"tier2"|"tier3",
//     "endpoint": "http://host.docker.internal:9912",
//     "model": "meta-llama/Llama-3.1-70B-Instruct",
//     "compression": "4bit"|"8bit"|"none" }
//
// Successful response includes the Ping result so the frontend can show
// a "verified" chip immediately. Failed Ping returns 400 with the error
// so the user can fix the endpoint + retry without persisting a broken
// config.
//
// `compression` is stored alongside in the generic settings table under
// `airllm.<tier>.compression` — ProviderConfig doesn't carry a
// compression field today.
func (s *Server) handleAdminAirLLMConfigureTier(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if s.bank == nil {
		http.Error(w, "bank disabled", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Tier        string `json:"tier"`
		Endpoint    string `json:"endpoint"`
		Model       string `json:"model"`
		Compression string `json:"compression"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Endpoint == "" {
		http.Error(w, "endpoint is required", http.StatusBadRequest)
		return
	}
	if body.Model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	tier, ok := parseTierKey(body.Tier)
	if !ok {
		http.Error(w, "unknown tier; expected tier2|tier3|tier2_provider|tier3_provider", http.StatusBadRequest)
		return
	}
	// Validate(tier) is called by SaveProviderConfig; we replicate the
	// "AirLLM allowed for Tier 2 + Tier 3 only" check here so the user
	// gets a clear 400 before the network ping happens.
	if tier != Tier2 && tier != Tier3 {
		http.Error(w, "AirLLM is only supported at Tier 2 or Tier 3", http.StatusBadRequest)
		return
	}
	if body.Compression != "" {
		switch strings.ToLower(body.Compression) {
		case "4bit", "8bit", "none":
		default:
			http.Error(w, "compression must be 4bit | 8bit | none", http.StatusBadRequest)
			return
		}
	}

	// Resolve endpoint. When SD Core is running inside a container and
	// the user supplied a loopback endpoint (`localhost:9912` /
	// `127.0.0.1:9912`), the dial will always fail because the container's
	// loopback isn't the host's. If we can SEE a sibling AirLLM container
	// in the same compose project, transparently swap in its compose-DNS
	// endpoint — it's what the user almost certainly meant, and what every
	// other tier's auto-register flow uses. This eliminates a major fresh-
	// install footgun where the UI's "Base URL" field default of
	// `http://127.0.0.1:9912` looks right but doesn't work from inside
	// Docker.
	originalEndpoint := body.Endpoint
	body.Endpoint = resolveAirLLMEndpointForSDCore(r.Context(), s, body.Endpoint)
	if body.Endpoint != originalEndpoint {
		log.Printf("airllm configure_tier: rewrote endpoint %q -> %q (sd-core in-container; sibling airllm detected)",
			originalEndpoint, body.Endpoint)
	}

	// Ping the endpoint via the existing AirLLMProvider.
	probe := &AirLLMProvider{
		BaseURL: body.Endpoint,
		Model:   body.Model,
	}
	pingCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	pingErr := probe.Ping(pingCtx)
	// GPU preflight is meaningful only when the sidecar isn't already
	// running. If it IS running + healthy, CUDA detection is moot —
	// it's already up. When unreachable AND the requested compression
	// needs CUDA, surface cuda_required ahead of the generic
	// unreachable error so the user understands WHY they should fix
	// their hardware story before re-trying.
	if pingErr != nil {
		compression := strings.ToLower(body.Compression)
		if airllmCompressionRequiresCUDA(compression) {
			gpu := probeGPU(r.Context(), s.docker)
			if gpu.Vendor != "nvidia" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error":       "cuda_required",
					"endpoint":    body.Endpoint,
					"compression": compression,
					"gpu":         gpu,
					"hint": "Your host has no NVIDIA driver detected. AirLLM 4bit/8bit needs CUDA. " +
						"Use compression=none (very slow + requires N×16GB RAM for N billion params) " +
						"OR switch to Ollama for Tier 2.",
				})
				return
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "unreachable",
			"endpoint": body.Endpoint,
			"detail":   pingErr.Error(),
			"hint":     "Confirm the AirLLM service is running at this endpoint (POST /healthz must return 200). The provider config was NOT saved.",
		})
		return
	}

	// Persist provider config.
	cfg := ProviderConfig{
		Kind:    ProviderAirLLM,
		BaseURL: strings.TrimRight(body.Endpoint, "/"),
		Model:   body.Model,
	}
	if err := s.bank.SaveProviderConfig(tier, cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Persist compression alongside if provided. Settings key shape
	// mirrors the per-service settings convention used by wave 7b2.
	if body.Compression != "" {
		_ = s.bank.SetSetting(
			fmt.Sprintf("airllm.%s.compression", string(tier)),
			strings.ToLower(body.Compression),
		)
	}
	// Hot-swap the live router — same code path PUT /settings/providers/
	// {tier} uses. Without this, the AirLLM-specific configure endpoint
	// would still require a restart while the generic PUT path does not,
	// surprising users who used the AirLLM form. Phase C bulk-flag also
	// fires for Tier 2 changes when the re-encode toggle is on.
	hotSwapped := false
	var hotSwapName string
	var reencodeFlagged int64
	if s.router != nil {
		newProvider := buildOneTierFromSettingsOrEnv(s.bank, tier)
		old, ok := s.router.ReplaceTier(tier, newProvider)
		if ok {
			hotSwapped = true
			oldName := "<none>"
			if old != nil {
				oldName = old.Name()
			}
			newName := "<none>"
			if newProvider != nil {
				newName = newProvider.Name()
				hotSwapName = newName
			}
			log.Printf("router: %s hot-swapped %s → %s (via airllm configure_tier)", tier, oldName, newName)
			if tier == Tier2 && newProvider != nil && oldName != newName {
				reencodeOn := false
				if v, ok, _ := s.bank.GetSetting("deep_enrich_reencode_on_model_change"); ok {
					reencodeOn = v == "true" || v == "1" || v == "on"
				}
				if reencodeOn {
					if n, err := s.bank.MarkAllDeepEncodedDirty("model_upgrade", newName); err != nil {
						log.Printf("router: re-encode flag failed: %v", err)
					} else {
						reencodeFlagged = n
						log.Printf("router: %d memories flagged for re-encode under new Tier 2 model %s", n, newName)
					}
				}
			}
		}
	}
	auditReason := "atomic AirLLM tier configure via /admin/airllm/configure_tier"
	if hotSwapped {
		auditReason += " (hot-swapped on live router)"
	}
	s.auditWrite(AuditEntry{
		Operation:  "configure",
		EntityType: "setting",
		EntityID:   "airllm." + string(tier),
		AfterJSON: mustJSON(map[string]any{
			"tier":        string(tier),
			"endpoint":    cfg.BaseURL,
			"model":       cfg.Model,
			"compression": strings.ToLower(body.Compression),
		}),
		Reason:    auditReason,
		AdapterID: adapterIDFromRequest(r),
	})
	// Wave 8e — auto-benchmark this (kind, model, compression) tuple so
	// the Dream Journal's estimate is ready by the time the user
	// navigates there. Fire-and-forget goroutine; skips when a
	// recent (<24h) measurement is already cached.
	s.maybeAutoBenchmark(tier, string(ProviderAirLLM), cfg.Model, strings.ToLower(body.Compression))
	note := "Restart sd-core for the new provider to take effect."
	if hotSwapped {
		note = "Provider hot-swapped on the live router — no restart needed."
	}
	resp := map[string]any{
		"ok":          true,
		"tier":        string(tier),
		"endpoint":    cfg.BaseURL,
		"model":       cfg.Model,
		"compression": strings.ToLower(body.Compression),
		"ping": map[string]any{
			"reachable": true,
			"loaded":    true,
		},
		"note":        note,
		"hot_swapped": hotSwapped,
	}
	if hotSwapName != "" {
		resp["active_provider"] = hotSwapName
	}
	if reencodeFlagged > 0 {
		resp["reencode_flagged"] = reencodeFlagged
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminAirLLMHealthPort serves GET /admin/airllm/health/{port}.
//
// Lightweight reachability proxy for a specific port. Used by the
// "Verify install" button while the user waits for their native sidecar
// to come up. Safety: port range limited to 9900–9999 (AirLLM's typical
// allocation) so this can't be used as an arbitrary port-scanner proxy.
// Resolves against `host.docker.internal` by default; pass
// `?host=synaptic-airllm-tier3` to probe a sibling container instead
// (allowlisted via the existing SD_DOCKER_SERVICE_ALLOWLIST regex).
func (s *Server) handleAdminAirLLMHealthPort(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	portStr := strings.TrimPrefix(r.URL.Path, "/admin/airllm/health/")
	portStr = strings.Trim(portStr, "/")
	if portStr == "" {
		http.Error(w, "missing port", http.StatusBadRequest)
		return
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port < 9900 || port > 9999 {
		http.Error(w, "port must be an integer in [9900, 9999]", http.StatusBadRequest)
		return
	}
	host := r.URL.Query().Get("host")
	if host == "" {
		host = "host.docker.internal"
	} else if host != "host.docker.internal" && host != "localhost" {
		// Container-host names must match the service allowlist when
		// the Docker control plane is configured. Refuse arbitrary hosts.
		if s.docker == nil || !s.docker.nameAllowed(host) {
			http.Error(w, "host not allowed; use host.docker.internal or a synaptic-* container name", http.StatusForbidden)
			return
		}
	}
	target := fmt.Sprintf("http://%s:%d", host, port)
	probe := &AirLLMProvider{BaseURL: target}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := probe.Ping(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"target":    target,
			"reachable": false,
			"detail":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":    target,
		"reachable": true,
		"loaded":    true,
	})
}

// ──────────────────────────────────────────────────────────────────────────
// Wave 8d — HF token validation (POST /admin/airllm/validate_hf_token)
// ──────────────────────────────────────────────────────────────────────────

// hfWhoamiBaseURL is the HuggingFace API root. Overridable via env so
// tests can point at a httptest server. The path /api/whoami-v2 is
// appended at request time.
func hfWhoamiBaseURL() string {
	return envOr("SD_HF_API_BASE_URL", "https://huggingface.co")
}

// handleAdminAirLLMValidateHFToken serves POST /admin/airllm/validate_hf_token.
//
// Calls HF's whoami-v2 endpoint with the supplied token to confirm
// (a) the token is well-formed, (b) HF accepts it (not expired/revoked),
// and (c) returns the associated account so the audit row can record
// "validated against user X" without leaking the secret. The token is
// NEVER persisted by this endpoint — that's the frontend's job to do
// AFTER validation via the existing /admin/env_write surface.
//
// Response statuses:
//   200 — valid token; body carries {valid:true, user:{name,type,orgs}}
//   400 — body missing/malformed (caller didn't send `token`)
//   401 — HF rejected the token (invalid / expired); body carries
//         a `hint` pointing the user at hf.co/settings/tokens
//   403 — HF returned 403 (e.g. account suspended / org-scoped restrictions)
//   502 — HF unreachable (network failure / DNS / timeout)
//
// 401/403/502 responses all include {valid:false, error, hint} so the
// frontend can render an inline error without branching on status code.
func (s *Server) handleAdminAirLLMValidateHFToken(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	// 8s ceiling — HF whoami is normally <1s; if the user's network is
	// slow enough to need longer we'd rather fast-fail with the
	// "unreachable" hint than freeze the wizard.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		hfWhoamiBaseURL()+"/api/whoami-v2", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "synaptic-disorder-core")

	cl := &http.Client{Timeout: 8 * time.Second}
	resp, doErr := cl.Do(req)
	if doErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"valid": false,
			"error": "hf_unreachable",
			"detail": doErr.Error(),
			"hint":  "Couldn't reach huggingface.co — check network.",
		})
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to parse + success response
	case http.StatusUnauthorized:
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"valid": false,
			"error": "invalid_token",
			"hint":  "Token is invalid or expired. Generate a new one at https://huggingface.co/settings/tokens.",
		})
		return
	case http.StatusForbidden:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"valid": false,
			"error": "forbidden",
			"hint":  "HuggingFace returned 403 — the token may be scoped to an org you don't belong to, or the account is restricted.",
		})
		return
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"valid":  false,
			"error":  "hf_unreachable",
			"detail": fmt.Sprintf("HuggingFace returned status %d", resp.StatusCode),
			"hint":   "Couldn't reach huggingface.co — check network.",
		})
		return
	}

	// Parse the OK payload. HF's whoami-v2 returns many fields; we surface
	// only the bits useful for the audit row + the dashboard's "validated
	// against <user>" affordance.
	var raw struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Orgs []struct {
			Name string `json:"name"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"valid":  false,
			"error":  "hf_response_unparseable",
			"detail": err.Error(),
			"hint":   "HuggingFace returned 200 but the body wasn't recognisable JSON. Try again; if persistent, file a bug.",
		})
		return
	}
	orgNames := make([]string, 0, len(raw.Orgs))
	for _, o := range raw.Orgs {
		if o.Name != "" {
			orgNames = append(orgNames, o.Name)
		}
	}

	// Audit op with the validated username — NOT the token. Lets the user
	// retroactively see "I validated tokens against accounts X, Y, Z over
	// time" without anything sensitive landing on disk.
	s.auditWrite(AuditEntry{
		Operation:  "airllm_token_validated",
		EntityType: "setting",
		EntityID:   "HUGGING_FACE_HUB_TOKEN",
		AfterJSON: mustJSON(map[string]any{
			"hf_user": raw.Name,
			"hf_type": raw.Type,
			"hf_orgs": orgNames,
		}),
		Reason:    "HF whoami-v2 returned 200 — token accepted",
		AdapterID: adapterIDFromRequest(r),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true,
		"user": map[string]any{
			"name": raw.Name,
			"type": raw.Type,
			"orgs": orgNames,
		},
	})
}

// silence unused-import warnings under different build modes
var _ = os.Getenv
