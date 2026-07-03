// handlers_admin_docker.go — Wave 7a Docker control plane HTTP surface.
//
// Endpoints (all under /admin/docker/*):
//
//   GET  /admin/docker/services                       — list services in the synaptic compose project
//   POST /admin/docker/services/{name}/start          — start the named service's container
//   POST /admin/docker/services/{name}/stop           — stop
//   POST /admin/docker/services/{name}/restart        — restart
//   GET  /admin/docker/services/{name}/logs           — tail or follow logs (SSE when follow=1)
//   GET  /admin/docker/health                         — chip-friendly aggregated health
//
// Compose-specific operations (recreate, build, profile up/down) shell
// out to `docker compose` and ship in a follow-up wave.
//
// Security model (handoff §Security):
//
//   1. Off by default. Enabled only when SD_ALLOW_DOCKER_SOCKET=1 AND the
//      socket is mounted into the container at SD_DOCKER_SOCKET (default
//      /var/run/docker.sock).
//   2. Allowlist: service names must match SD_DOCKER_SERVICE_ALLOWLIST
//      (default ^synaptic-). Container names must also carry the compose
//      project label SD_COMPOSE_PROJECT (default synaptic).
//   3. Every operation lands in audit_log with operation: docker_*.
//   4. No exec, no commit, no run, no image pull. Read + lifecycle only.
//
// Frontend pre-wire: the Services panel renders the unavailable state
// when GET /admin/docker/services returns 412 Precondition Failed with
// `{error, hint}`; it lights up when the same endpoint returns 200.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DockerControlState holds per-process Docker config + a cached client.
// Initialised by main.go on boot; nil-safe handlers degrade to 412 when
// the gate is disabled.
type DockerControlState struct {
	mu              sync.Mutex
	client          *DockerClient
	allowlistRegex  *regexp.Regexp
	composeProject  string
	socketAllowed   bool
	socketPath      string
	rateLimit       time.Duration
	lastActionAt    map[string]time.Time

	// Wave 7b1 compose-CLI shell-out. Built lazily on first compose op
	// because resolving composeHostPath requires inspecting our own
	// container (which we can't do until the gate is on + the daemon
	// is reachable).
	composeRunner   *ComposeRunner
	composeHelperImage string
	selfContainerName  string
}

// NewDockerControlState reads the relevant env vars and returns a state
// object. Doesn't dial the daemon — Available() / requests do.
func NewDockerControlState() *DockerControlState {
	allowed := envOr("SD_ALLOW_DOCKER_SOCKET", "0") == "1"
	socket := envOr("SD_DOCKER_SOCKET", DefaultDockerSocketPath)
	allowlistStr := envOr("SD_DOCKER_SERVICE_ALLOWLIST", "^synaptic-")
	composeProject := envOr("SD_COMPOSE_PROJECT", "synaptic")
	// Per-action rate limit (per service+action). Defaults to 2s. Prevents
	// accidental restart loops.
	rateLimitMs := envInt("SD_DOCKER_RATE_LIMIT_MS", 2000)
	allowRe, err := regexp.Compile(allowlistStr)
	if err != nil {
		// Bad regex falls back to the safe default rather than panicking.
		allowRe = regexp.MustCompile("^synaptic-")
	}
	helperImage := envOr("SD_DOCKER_COMPOSE_HELPER_IMAGE", DefaultComposeHelperImage)
	// HOSTNAME inside a container is the container's short id; the
	// daemon also accepts that as a container reference for inspect,
	// which is how we find our own labels. Fall back to a configured
	// name for native (non-container) runs.
	self := envOr("SD_SELF_CONTAINER_NAME", envOr("HOSTNAME", "synaptic-core"))
	return &DockerControlState{
		client:             NewDockerClient(socket),
		allowlistRegex:     allowRe,
		composeProject:     composeProject,
		socketAllowed:      allowed,
		socketPath:         socket,
		rateLimit:          time.Duration(rateLimitMs) * time.Millisecond,
		lastActionAt:       map[string]time.Time{},
		composeHelperImage: helperImage,
		selfContainerName:  self,
	}
}

// ensureComposeRunner builds the runner lazily — first compose op
// inspects our own container to learn the host-side compose directory.
// Subsequent calls reuse the cached runner.
func (ds *DockerControlState) ensureComposeRunner(ctx context.Context) (*ComposeRunner, error) {
	if ds == nil {
		return nil, errors.New("docker control state nil")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.composeRunner != nil {
		return ds.composeRunner, nil
	}
	hostPath := ResolveComposeHostPath(ctx, ds.client, ds.selfContainerName)
	if hostPath == "" {
		return nil, errors.New("could not resolve compose host path; set SD_COMPOSE_HOST_PATH explicitly")
	}
	ds.composeRunner = NewComposeRunner(
		ds.client,
		ds.composeHelperImage,
		hostPath,
		ds.composeProject,
		envOr("SD_COMPOSE_FILE_BASENAME", "docker-compose.yml"),
	)
	return ds.composeRunner, nil
}

// gateOK returns nil when the Docker control plane is enabled AND the
// socket is reachable. Otherwise it writes the appropriate error response
// and returns a non-nil error so the caller knows to bail.
//
// 412 Precondition Failed when SD_ALLOW_DOCKER_SOCKET != 1 — the frontend
// renders this as the "Docker management unavailable" card.
// 503 Service Unavailable when the gate is on but the socket can't be
// reached (volume mount missing, daemon down).
func (ds *DockerControlState) gateOK(ctx context.Context, w http.ResponseWriter) error {
	if ds == nil || !ds.socketAllowed {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error":              "docker_socket_disabled",
			"socket_allowed":     false,
			"hint":               "Set SD_ALLOW_DOCKER_SOCKET=1 in your environment and mount /var/run/docker.sock into the SD Core container, then docker compose up -d --build core.",
			"compose_project":    ds.safeComposeProject(),
		})
		return errors.New("docker socket gate disabled")
	}
	if err := ds.client.Available(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":          "docker_socket_unreachable",
			"socket_allowed": true,
			"detail":         err.Error(),
			"hint":           "Gate is enabled (SD_ALLOW_DOCKER_SOCKET=1) but the daemon socket is not reachable at " + ds.socketPath + ". Confirm the volume mount.",
		})
		return err
	}
	return nil
}

func (ds *DockerControlState) safeComposeProject() string {
	if ds == nil {
		return ""
	}
	return ds.composeProject
}

// nameAllowed checks that a service or container name is in the
// allowlist. Used for action endpoints.
func (ds *DockerControlState) nameAllowed(name string) bool {
	if ds == nil || ds.allowlistRegex == nil {
		return false
	}
	return ds.allowlistRegex.MatchString(name)
}

// rateOK records the action time + returns false if we're under the rate
// limit. Per (service, action) key.
func (ds *DockerControlState) rateOK(service, action string) bool {
	if ds == nil || ds.rateLimit <= 0 {
		return true
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	key := service + ":" + action
	now := time.Now()
	if last, ok := ds.lastActionAt[key]; ok {
		if now.Sub(last) < ds.rateLimit {
			return false
		}
	}
	ds.lastActionAt[key] = now
	return true
}

// ServiceInfo is the per-service payload of GET /admin/docker/services.
// Trimmed for the dashboard's Services panel.
type ServiceInfo struct {
	Name          string            `json:"name"`           // compose service name (com.docker.compose.service)
	ContainerID   string            `json:"container_id"`
	ContainerName string            `json:"container_name"` // canonical, no leading slash
	Image         string            `json:"image"`
	State         string            `json:"state"`          // running | exited | restarting | paused | created
	Health        string            `json:"health"`         // starting | healthy | unhealthy | unknown | ""
	Status        string            `json:"status"`         // "Up 23 minutes (healthy)"
	Restarts      int               `json:"restarts"`
	Ports         []ContainerPort   `json:"ports"`
	Labels        map[string]string `json:"labels,omitempty"`
	Profiles      []string          `json:"profiles"`
	IsEssential   bool              `json:"is_essential"`
	// Source distinguishes shipped services (compose project members)
	// from user-registered ones attached via ProviderConfig.Managed.
	// v2.6 Bundle G — lets the dashboard render user entries with a
	// "user-attached" badge and a different action set.
	//   "shipped" — present in docker-compose.yml under the synaptic
	//               project; always-on or profile-gated; lifecycle
	//               policy from defaultPolicyFor or persisted row.
	//   "user"    — registered via a per-tier ProviderConfig.Managed
	//               block; lifecycle only honoured when LifecycleEnabled.
	Source string `json:"source,omitempty"`
	// TierKey shows which tier registered this container (only set
	// when Source="user"). UI uses this to surface "Tier 2 → my-llama
	// container" relationships.
	TierKey string `json:"tier_key,omitempty"`
}

// ServicesResponse is the GET /admin/docker/services body.
type ServicesResponse struct {
	ComposeProject      string        `json:"compose_project"`
	DockerSocketAllowed bool          `json:"docker_socket_allowed"`
	Services            []ServiceInfo `json:"services"`
	AvailableProfiles   []string      `json:"available_profiles"`
	ActiveProfiles      []string      `json:"active_profiles"`
	Warnings            []string      `json:"warnings"`
}

// handleAdminDockerServices serves GET /admin/docker/services.
func (s *Server) handleAdminDockerServices(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return // gateOK wrote the response
	}
	out, err := s.collectServices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) collectServices(ctx context.Context) (ServicesResponse, error) {
	resp := ServicesResponse{
		ComposeProject:      s.docker.composeProject,
		DockerSocketAllowed: true,
		Services:            []ServiceInfo{},
		AvailableProfiles:   []string{},
		ActiveProfiles:      []string{},
		Warnings:            []string{},
	}
	containers, err := s.docker.client.ListContainers(ctx, map[string]string{
		"com.docker.compose.project": s.docker.composeProject,
	})
	if err != nil {
		return resp, err
	}
	profileSet := map[string]bool{}
	activeProfileSet := map[string]bool{}
	for _, c := range containers {
		name := primaryName(c.Names)
		// Allowlist enforcement: container name must match the configured
		// regex. Belt + suspenders alongside the compose-project filter.
		if !s.docker.nameAllowed(name) {
			continue
		}
		svc := c.Labels["com.docker.compose.service"]
		if svc == "" {
			svc = name
		}
		profiles := splitNonEmpty(c.Labels["com.docker.compose.depends_on_profiles"], ",")
		// Compose's actual profile label is `com.docker.compose.config.profiles`
		// in modern compose; fall back to the depends_on variant.
		if v := c.Labels["com.docker.compose.config.profiles"]; v != "" {
			profiles = splitNonEmpty(v, ",")
		}
		for _, p := range profiles {
			profileSet[p] = true
			if c.State == "running" {
				activeProfileSet[p] = true
			}
		}
		insp, _ := s.docker.client.InspectContainer(ctx, name)
		health := insp.State.Health.Status
		if health == "" {
			health = "unknown"
		}
		// Essential = default-up services that the dashboard treats as
		// load-bearing (hides destructive actions, includes them in the
		// HUD chip's "essential_services_ok" rollup). Post wave-8 retirement,
		// `ollama-tier1` is the canonical Ollama; legacy `ollama` is
		// profile-gated `legacy-ollama` and intentionally not essential
		// — when it's running, it's only because the user opted into it
		// for migration, not because the stack depends on it.
		isEssential := svc == "core" || svc == "ollama-tier1"
		resp.Services = append(resp.Services, ServiceInfo{
			Name:          svc,
			ContainerID:   shortID(c.ID),
			ContainerName: name,
			Image:         c.Image,
			State:         c.State,
			Health:        health,
			Status:        c.Status,
			Restarts:      insp.State.Restarts,
			Ports:         c.Ports,
			Labels:        slimLabels(c.Labels),
			Profiles:      profiles,
			IsEssential:   isEssential,
			Source:        "shipped",
		})
	}
	// v2.6 Bundle G — append user-registered containers from per-tier
	// ProviderConfig.Managed blocks. These are surfaced regardless of
	// whether they're currently running (Source="user" rows with no
	// matching compose-project container appear with State="absent"),
	// so the UI can show "you've attached my-llama but it isn't running".
	// De-dup against the shipped list by container_name.
	if s.bank != nil {
		shippedByName := map[string]bool{}
		for _, svc := range resp.Services {
			shippedByName[svc.ContainerName] = true
		}
		for _, tier := range []TierKey{Tier1, Tier2, Tier3, TierEmbedding} {
			cfg, err := s.bank.GetProviderConfig(tier)
			if err != nil || cfg.Managed == nil || cfg.Managed.ContainerName == "" {
				continue
			}
			cname := cfg.Managed.ContainerName
			if shippedByName[cname] {
				// Already in the shipped list — update the existing entry
				// in-place with source="user" so the UI knows it's
				// user-attached, but keep the live container state.
				for i := range resp.Services {
					if resp.Services[i].ContainerName == cname {
						resp.Services[i].Source = "user"
						resp.Services[i].TierKey = string(tier)
						break
					}
				}
				continue
			}
			ports := []ContainerPort{}
			if cfg.Managed.HostPort > 0 || cfg.Managed.ContainerPort > 0 {
				ports = append(ports, ContainerPort{
					PrivatePort: cfg.Managed.ContainerPort,
					PublicPort:  cfg.Managed.HostPort,
					Type:        "tcp",
				})
			}
			state := "absent"
			status := "Registered (not running)"
			if !cfg.Managed.LifecycleEnabled {
				status += " · lifecycle disabled"
			}
			resp.Services = append(resp.Services, ServiceInfo{
				Name:          cname,
				ContainerName: cname,
				Image:         cfg.Managed.Image,
				State:         state,
				Health:        "unknown",
				Status:        status,
				Ports:         ports,
				Source:        "user",
				TierKey:       string(tier),
			})
		}
	}
	for p := range profileSet {
		resp.AvailableProfiles = append(resp.AvailableProfiles, p)
	}
	for p := range activeProfileSet {
		resp.ActiveProfiles = append(resp.ActiveProfiles, p)
	}
	return resp, nil
}

// handleAdminDockerServiceAction serves the POST action endpoints. action
// is one of start|stop|restart.
func (s *Server) handleAdminDockerServiceAction(w http.ResponseWriter, r *http.Request, service, action string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	// Allowlist + rate limit on the requested NAME, not the action.
	containerName := s.resolveContainerName(r.Context(), service)
	if containerName == "" {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !s.docker.nameAllowed(containerName) {
		http.Error(w, "service not allowed by SD_DOCKER_SERVICE_ALLOWLIST", http.StatusForbidden)
		return
	}
	if !s.docker.rateOK(service, action) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "rate_limited",
			"hint":  fmt.Sprintf("Wait %s before retrying.", s.docker.rateLimit),
		})
		return
	}

	// Capture before-state for the audit row.
	beforeInsp, _ := s.docker.client.InspectContainer(r.Context(), containerName)

	startedAt := time.Now().UTC()
	var actErr error
	var composeOut *ComposeExecResult
	switch action {
	case "start":
		actErr = s.docker.client.StartContainer(r.Context(), containerName)
	case "stop":
		actErr = s.docker.client.StopContainer(r.Context(), containerName, 10)
	case "restart":
		actErr = s.docker.client.RestartContainer(r.Context(), containerName, 10)
	case "recreate", "build":
		// Wave 7b1 compose-CLI shell-out via the helper container. Both
		// actions invoke `docker compose up -d <flag> <service>` — recreate
		// uses --force-recreate (no image rebuild), build uses --build
		// (rebuild image first, then up). Long-running; we give the helper
		// up to 10 minutes for build (covers a from-scratch Go build).
		runner, rErr := s.docker.ensureComposeRunner(r.Context())
		if rErr != nil {
			http.Error(w, rErr.Error(), http.StatusInternalServerError)
			return
		}
		composeArgs := []string{"up", "-d"}
		if action == "recreate" {
			composeArgs = append(composeArgs, "--force-recreate")
		} else {
			composeArgs = append(composeArgs, "--build")
		}
		composeArgs = append(composeArgs, service)
		execCtx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		res, runErr := runner.ComposeExec(execCtx, composeArgs)
		composeOut = &res
		if runErr != nil {
			actErr = runErr
		} else if res.ExitCode != 0 {
			actErr = fmt.Errorf("compose %s exit %d: %s", action, res.ExitCode, firstNonEmpty(res.Stderr, res.Stdout))
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if actErr != nil {
		if IsContainerNotFound(actErr) {
			http.Error(w, "container not found", http.StatusNotFound)
			return
		}
		http.Error(w, actErr.Error(), http.StatusInternalServerError)
		return
	}

	afterInsp, _ := s.docker.client.InspectContainer(r.Context(), containerName)

	s.auditWrite(AuditEntry{
		Operation:  "docker_" + action,
		EntityType: "container",
		EntityID:   containerName,
		BeforeJSON: mustJSON(map[string]any{
			"state":    beforeInsp.State.Status,
			"running":  beforeInsp.State.Running,
			"restarts": beforeInsp.State.Restarts,
		}),
		AfterJSON: mustJSON(map[string]any{
			"state":    afterInsp.State.Status,
			"running":  afterInsp.State.Running,
			"restarts": afterInsp.State.Restarts,
		}),
		AdapterID: adapterIDFromRequest(r),
		Reason:    r.URL.Query().Get("reason"),
	})

	// Fire-and-forget WS event so the frontend can refresh the chip + card.
	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "service_state_changed",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-docker",
			Payload: map[string]any{
				"service":        service,
				"container_name": containerName,
				"action":         action,
				"state":          afterInsp.State.Status,
				"health":         afterInsp.State.Health.Status,
			},
		})
	}

	resp := map[string]any{
		"started_at":   startedAt.Format(time.RFC3339Nano),
		"action":       action,
		"service":      service,
		"container_id": shortID(afterInsp.ID),
		"before_state": beforeInsp.State.Status,
		"after_state":  afterInsp.State.Status,
	}
	// For compose-driven actions, surface the captured output so the
	// dashboard can show build logs / recreate-stage failures inline.
	if composeOut != nil {
		resp["compose_exit_code"] = composeOut.ExitCode
		resp["compose_stdout"] = composeOut.Stdout
		resp["compose_stderr"] = composeOut.Stderr
		resp["compose_duration_ms"] = composeOut.Duration.Milliseconds()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminDockerLogs serves GET /admin/docker/services/{name}/logs.
// Modes:
//   - follow=1 → Server-Sent Events stream; one `event: log\ndata: {...}` per line
//   - follow=0 (or missing) → one-shot JSON `{lines: [{stream,line}...]}`
//
// Query params:
//   - tail=N  (default 200)
//   - since=<rfc3339 | duration like 1m>
//   - follow=1
func (s *Server) handleAdminDockerLogs(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	containerName := s.resolveContainerName(r.Context(), service)
	if containerName == "" {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !s.docker.nameAllowed(containerName) {
		http.Error(w, "service not allowed by SD_DOCKER_SERVICE_ALLOWLIST", http.StatusForbidden)
		return
	}
	opts := LogStreamOpts{
		Since: r.URL.Query().Get("since"),
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && v > 0 {
		opts.Tail = v
	} else {
		opts.Tail = 200
	}
	follow := r.URL.Query().Get("follow") == "1"

	if !follow {
		// One-shot JSON response.
		lines := []LogLine{}
		err := s.docker.client.StreamLogs(r.Context(), containerName, opts, func(l LogLine) {
			lines = append(lines, l)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service": service,
			"lines":   lines,
		})
		return
	}

	// SSE streaming mode. Set headers + flush after each event.
	opts.Follow = true
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Initial primer so the SSE pipe opens cleanly through proxies.
	fmt.Fprintf(w, "event: open\ndata: {\"service\":%q}\n\n", service)
	flusher.Flush()
	err := s.docker.client.StreamLogs(r.Context(), containerName, opts, func(l LogLine) {
		payload, _ := json.Marshal(map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"stream":    l.Stream,
			"line":      l.Line,
		})
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload)
		flusher.Flush()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		// Best-effort error event; the client may have already disconnected.
		fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
	}
}

// handleAdminDockerHealth serves GET /admin/docker/health. Chip-friendly
// aggregated payload for the HUD-level health chip.
func (s *Server) handleAdminDockerHealth(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	svc, err := s.collectServices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	essentialOK := true
	optional := []map[string]any{}
	for _, sv := range svc.Services {
		// Essential services must be running AND (if they declare a health
		// check) healthy. Unknown health on a running container counts as
		// OK — many services don't declare healthchecks.
		if sv.IsEssential {
			if sv.State != "running" {
				essentialOK = false
			} else if sv.Health == "unhealthy" {
				essentialOK = false
			}
		} else {
			optional = append(optional, map[string]any{
				"name":   sv.Name,
				"state":  sv.State,
				"health": sv.Health,
			})
		}
	}
	status := "ok"
	if !essentialOK {
		status = "down"
	} else {
		for _, sv := range svc.Services {
			if sv.Health == "unhealthy" {
				status = "degraded"
				break
			}
		}
	}
	// 24-hour audit summary for the HUD's "recent actions" affordance.
	auditSummary := s.dockerAuditSummary(24 * time.Hour)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                  status,
		"essential_services_ok":   essentialOK,
		"optional_services":       optional,
		"audit_summary_24h":       auditSummary,
		"compose_project":         s.docker.composeProject,
		"socket_allowed":          true,
	})
}

// dockerAuditSummary returns a (docker_actions, errors) summary for the
// last `window`. Errors are detected by audit rows whose Reason starts
// with "error:" — the action handlers don't currently emit these, but
// the schema supports it for future expansion.
func (s *Server) dockerAuditSummary(window time.Duration) map[string]int {
	out := map[string]int{"docker_actions": 0, "errors": 0}
	if s.bank == nil {
		return out
	}
	since := time.Now().UTC().Add(-window).Format(time.RFC3339Nano)
	entries, err := s.bank.ListAuditLog(AuditFilter{Since: since, Limit: 500})
	if err != nil {
		return out
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Operation, "docker_") {
			out["docker_actions"]++
			if strings.HasPrefix(e.Reason, "error:") {
				out["errors"]++
			}
		}
	}
	return out
}

// resolveContainerName turns a compose service name ("core", "ollama")
// into the actual container name ("synaptic-core", "synaptic-ollama") by
// inspecting the compose project labels. Returns "" if no match.
func (s *Server) resolveContainerName(ctx context.Context, service string) string {
	if s == nil || s.docker == nil {
		return ""
	}
	containers, err := s.docker.client.ListContainers(ctx, map[string]string{
		"com.docker.compose.project": s.docker.composeProject,
		"com.docker.compose.service": service,
	})
	if err != nil || len(containers) == 0 {
		// Fallback: maybe `service` is already the full container name.
		// Verify via inspect.
		if _, ierr := s.docker.client.InspectContainer(ctx, service); ierr == nil {
			return service
		}
		return ""
	}
	return primaryName(containers[0].Names)
}

// ──────────────────────────────────────────────────────────────────────────
// Route dispatcher — /admin/docker[...] paths
// ──────────────────────────────────────────────────────────────────────────

// adminDockerRouter dispatches everything under /admin/docker/. Registered
// in main.go alongside the other admin routes. Path forms:
//
//   /admin/docker/services
//   /admin/docker/services/{name}/start|stop|restart|logs
//   /admin/docker/health
func (s *Server) adminDockerRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/docker")
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case rest == "services":
		s.handleAdminDockerServices(w, r)
	case rest == "health":
		s.handleAdminDockerHealth(w, r)
	case rest == "disk":
		s.handleAdminDockerDisk(w, r)
	case rest == "profiles":
		s.handleAdminDockerProfilesList(w, r)
	case strings.HasPrefix(rest, "services/"):
		sub := strings.TrimPrefix(rest, "services/")
		parts := strings.SplitN(sub, "/", 2)
		if len(parts) < 2 {
			http.Error(w, "missing action", http.StatusBadRequest)
			return
		}
		service, action := parts[0], parts[1]
		if service == "" {
			http.Error(w, "missing service name", http.StatusBadRequest)
			return
		}
		switch action {
		case "logs":
			s.handleAdminDockerLogs(w, r, service)
		case "models":
			// Wave 7c1 — GET only; lists Ollama-pulled models for the
			// named Ollama container (404 for non-Ollama services).
			s.handleAdminDockerModels(w, r, service)
		case "pull_model":
			// Wave 7c1 — POST only; triggers `ollama pull` via the
			// container's /api/pull endpoint; SSE-streams progress.
			s.handleAdminDockerPullModel(w, r, service)
		case "delete_model":
			// Wave 8c — POST only; parallels pull_model. Refuses if it
			// would empty the container (no other models left).
			s.handleAdminDockerDeleteModel(w, r, service)
		case "start", "stop", "restart", "recreate", "build":
			s.handleAdminDockerServiceAction(w, r, service, action)
		default:
			http.Error(w, "unknown action; expected start|stop|restart|recreate|build|logs|models|pull_model|delete_model", http.StatusNotFound)
		}
	case strings.HasPrefix(rest, "profiles/"):
		sub := strings.TrimPrefix(rest, "profiles/")
		parts := strings.SplitN(sub, "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			http.Error(w, "missing profile action; expected enable|disable", http.StatusBadRequest)
			return
		}
		s.handleAdminDockerProfileAction(w, r, parts[0], parts[1])
	default:
		http.NotFound(w, r)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Wave 7b1 — /admin/docker/disk + /admin/docker/profiles/*
// ──────────────────────────────────────────────────────────────────────────

// handleAdminDockerDisk serves GET /admin/docker/disk. Returns daemon-wide
// disk usage (containers / volumes / images) via the Engine's /system/df
// — no compose CLI needed.
func (s *Server) handleAdminDockerDisk(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	df, err := s.docker.client.GetSystemDF(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Filter the volume list to volumes our compose project owns so the
	// dashboard sees the relevant ones first. Keep the full image list
	// (small) and a small projection of containers.
	composedVolumes := []SystemDFVolume{}
	for _, v := range df.Volumes {
		// Compose-managed volumes are named "<project>_<volname>" OR are
		// declared external (then the name is user-chosen). We surface
		// both — anything starting with our project name OR matching a
		// known-stack volume name from the synaptic-disorder-* family.
		if strings.HasPrefix(v.Name, s.docker.composeProject+"_") ||
			strings.HasPrefix(v.Name, "synaptic-disorder-") {
			composedVolumes = append(composedVolumes, v)
		}
	}
	var totalVolumesBytes int64
	for _, v := range composedVolumes {
		totalVolumesBytes += v.Size
	}
	// Host disk free/total. /data is the bind-mounted compose project
	// directory in our containerised deploy → statfs reports the host
	// filesystem. /var/lib/docker is the fallback for non-containerised
	// SD Core.
	hostUsage := resolveHostDiskUsage([]string{"/data", "/var/lib/docker"})
	resp := map[string]any{
		"volumes":               composedVolumes,
		"all_volumes":           df.Volumes,
		"total_volume_bytes":    totalVolumesBytes,
		"layers_size_bytes":     df.LayersSize,
		"images":                df.Images,
		"containers":            df.Containers,
		"compose_project":       s.docker.composeProject,
		"host_disk_path":        hostUsage.Path,
		"host_disk_free_bytes":  hostUsage.FreeBytes,
		"host_disk_total_bytes": hostUsage.TotalBytes,
	}
	if hostUsage.Note != "" {
		resp["host_disk_note"] = hostUsage.Note
	}
	writeJSON(w, http.StatusOK, resp)
}

// ComposeProfilesResponse is the GET /admin/docker/profiles body.
type ComposeProfilesResponse struct {
	Available []string `json:"available"`
	Active    []string `json:"active"`
}

// handleAdminDockerProfilesList serves GET /admin/docker/profiles. The
// active set is derived from currently-running containers' profile
// labels; the available set runs `docker compose config --profiles` via
// the helper container to enumerate every declared profile.
func (s *Server) handleAdminDockerProfilesList(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	// Available: from compose config --profiles.
	runner, rErr := s.docker.ensureComposeRunner(r.Context())
	if rErr != nil {
		http.Error(w, rErr.Error(), http.StatusInternalServerError)
		return
	}
	execCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := runner.ComposeExec(execCtx, []string{"config", "--profiles"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if res.ExitCode != 0 {
		http.Error(w,
			fmt.Sprintf("compose config --profiles exit %d: %s", res.ExitCode, firstNonEmpty(res.Stderr, res.Stdout)),
			http.StatusInternalServerError)
		return
	}
	available := splitLines(res.Stdout)

	// Active: scan running containers' profile labels.
	containers, _ := s.docker.client.ListContainers(r.Context(), map[string]string{
		"com.docker.compose.project": s.docker.composeProject,
	})
	activeSet := map[string]bool{}
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		v := c.Labels["com.docker.compose.config.profiles"]
		for _, p := range splitNonEmpty(v, ",") {
			activeSet[p] = true
		}
	}
	active := make([]string, 0, len(activeSet))
	for p := range activeSet {
		active = append(active, p)
	}

	writeJSON(w, http.StatusOK, ComposeProfilesResponse{
		Available: available,
		Active:    active,
	})
}

// handleAdminDockerProfileAction serves POST /admin/docker/profiles/{name}/{enable|disable}.
// enable → compose --profile <name> up -d
// disable → compose --profile <name> stop
func (s *Server) handleAdminDockerProfileAction(w http.ResponseWriter, r *http.Request, profile, action string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	if profile == "" {
		http.Error(w, "missing profile name", http.StatusBadRequest)
		return
	}
	if action != "enable" && action != "disable" {
		http.Error(w, "unknown profile action; expected enable|disable", http.StatusBadRequest)
		return
	}
	if !s.docker.rateOK("profile:"+profile, action) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "rate_limited",
			"hint":  fmt.Sprintf("Wait %s before retrying.", s.docker.rateLimit),
		})
		return
	}
	runner, rErr := s.docker.ensureComposeRunner(r.Context())
	if rErr != nil {
		http.Error(w, rErr.Error(), http.StatusInternalServerError)
		return
	}
	var args []string
	var timeout time.Duration
	var parentCtx context.Context
	switch action {
	case "enable":
		// `up -d` may build a missing image on first invocation. AirLLM's
		// image pulls PyTorch + bitsandbytes + AirLLM (~5-10 GB) and
		// realistically takes 10-15 min on a residential connection.
		// Mirror the pull_model timeout (30 min) so a first-time enable
		// can complete instead of getting SIGKILL'd halfway through.
		//
		// CRITICAL: decouple from r.Context() — when the HTTP client
		// (curl, browser fetch) hits ITS default 5-min timeout and
		// disconnects, the request context cancels, which would
		// cascade-kill the helper container mid-build. Use a fresh
		// background context so the build survives the client
		// disconnect. The user's UI can re-poll /admin/docker/services
		// or /admin/airllm/diagnostics to see the new container land.
		args = []string{"--profile", profile, "up", "-d"}
		timeout = 30 * time.Minute
		parentCtx = context.Background()
	case "disable":
		// `stop` is fast (graceful container stop on cached images). 60s
		// is plenty. Keep this on the request context — short enough that
		// no client should time out, and cancelability is fine.
		args = []string{"--profile", profile, "stop"}
		timeout = 60 * time.Second
		parentCtx = r.Context()
	}
	execCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	res, err := runner.ComposeExec(execCtx, args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.auditWrite(AuditEntry{
		Operation:  "docker_profile_" + action,
		EntityType: "profile",
		EntityID:   profile,
		AfterJSON: mustJSON(map[string]any{
			"exit_code":   res.ExitCode,
			"duration_ms": res.Duration.Milliseconds(),
		}),
		AdapterID: adapterIDFromRequest(r),
	})

	if s.hub != nil {
		s.hub.Fanout(Event{
			SchemaVersion: SchemaVersion,
			Type:          "service_state_changed",
			Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
			AdapterID:     "sd-core-docker",
			Payload: map[string]any{
				"profile":   profile,
				"action":    "profile_" + action,
				"exit_code": res.ExitCode,
			},
		})
	}

	resp := map[string]any{
		"profile":             profile,
		"action":              action,
		"compose_exit_code":   res.ExitCode,
		"compose_stdout":      res.Stdout,
		"compose_stderr":      res.Stderr,
		"compose_duration_ms": res.Duration.Milliseconds(),
	}
	if res.ExitCode != 0 {
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ──────────────────────────────────────────────────────────────────────────
// Wave 7c1 — Per-tier Ollama model management proxies
// ──────────────────────────────────────────────────────────────────────────

// handleAdminDockerModels serves GET /admin/docker/services/{name}/models.
// Proxies the named container's Ollama `GET /api/tags` so the dashboard
// can show which models are pulled into each tier. Only valid for
// Ollama-shaped services; non-Ollama services get a clean 404.
func (s *Server) handleAdminDockerModels(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	containerName := s.resolveContainerName(r.Context(), service)
	if containerName == "" {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !s.docker.nameAllowed(containerName) {
		http.Error(w, "service not allowed by SD_DOCKER_SERVICE_ALLOWLIST", http.StatusForbidden)
		return
	}
	// Heuristic gate: image name must contain "ollama" — Engine inspect
	// gives us the image; this filters out non-Ollama services without
	// requiring a config key per service.
	insp, _ := s.docker.client.InspectContainer(r.Context(), containerName)
	if !strings.Contains(strings.ToLower(insp.Config.Image), "ollama") {
		http.Error(w, "service does not appear to be Ollama (image does not contain 'ollama'); /models is only valid for Ollama containers", http.StatusNotFound)
		return
	}
	if !insp.State.Running {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "container_not_running",
			"service": service,
			"state":   insp.State.Status,
			"hint":    "Start the container first via POST /admin/docker/services/" + service + "/start",
		})
		return
	}
	cli := NewOllamaProxyClient(ollamaBaseURLForContainer(containerName))
	models, version, err := cli.ListModels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        service,
		"container_name": containerName,
		"endpoint":       ollamaBaseURLForContainer(containerName),
		"ollama_version": version,
		"models":         models,
	})
}

// handleAdminDockerPullModel serves POST /admin/docker/services/{name}/pull_model.
// Body: {"model":"llama3.1:8b"}. Optional query `?stream=1` selects SSE
// progress streaming; otherwise the handler blocks until the pull
// completes and returns a summary JSON.
func (s *Server) handleAdminDockerPullModel(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	containerName := s.resolveContainerName(r.Context(), service)
	if containerName == "" {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !s.docker.nameAllowed(containerName) {
		http.Error(w, "service not allowed by SD_DOCKER_SERVICE_ALLOWLIST", http.StatusForbidden)
		return
	}
	insp, _ := s.docker.client.InspectContainer(r.Context(), containerName)
	if !strings.Contains(strings.ToLower(insp.Config.Image), "ollama") {
		http.Error(w, "service does not appear to be Ollama", http.StatusNotFound)
		return
	}
	if !insp.State.Running {
		http.Error(w, "container not running; start it first", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	// Rate limit one pull at a time per (service, model) to keep daemon
	// load sane. The lifecycle rate-limiter key works fine here.
	if !s.docker.rateOK(service+":pull_model", body.Model) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "rate_limited",
			"hint":  "wait before retrying the pull",
		})
		return
	}
	cli := NewOllamaProxyClient(ollamaBaseURLForContainer(containerName))

	stream := r.URL.Query().Get("stream") == "1"
	if !stream {
		// Synchronous pull — block until done, then return a summary.
		// Bounded by ctx (which gets a 30-minute deadline; the user can
		// cancel by closing the connection).
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		lastStatus := ""
		err := cli.StreamPull(ctx, body.Model, func(s OllamaPullStatus) {
			lastStatus = s.Status
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditWrite(AuditEntry{
			Operation:  "docker_pull_model",
			EntityType: "container",
			EntityID:   containerName,
			AfterJSON:  mustJSON(map[string]any{"model": body.Model, "final_status": lastStatus}),
			AdapterID:  adapterIDFromRequest(r),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"service":      service,
			"model":        body.Model,
			"final_status": lastStatus,
		})
		return
	}
	// SSE streaming mode.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "event: open\ndata: {\"service\":%q,\"model\":%q}\n\n", service, body.Model)
	flusher.Flush()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	err := cli.StreamPull(ctx, body.Model, func(s OllamaPullStatus) {
		payload, _ := json.Marshal(s)
		fmt.Fprintf(w, "event: progress\ndata: %s\n\n", payload)
		flusher.Flush()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
	} else {
		fmt.Fprintf(w, "event: done\ndata: {\"model\":%q}\n\n", body.Model)
		flusher.Flush()
		s.auditWrite(AuditEntry{
			Operation:  "docker_pull_model",
			EntityType: "container",
			EntityID:   containerName,
			AfterJSON:  mustJSON(map[string]any{"model": body.Model, "streamed": true}),
			AdapterID:  adapterIDFromRequest(r),
		})
	}
}

// handleAdminDockerDeleteModel serves POST /admin/docker/services/{name}/delete_model.
// Body: {"model":"llama3.1:8b"}. Refuses (409) when the target is the
// only model in the container — accidental deletion of an Ollama
// container's last model would empty Tier 1's serving capacity.
// Parallel to handleAdminDockerPullModel; same gate + allowlist + rate
// limit. Audit op: `docker_delete_model`. Returns `freed_bytes` from
// the diff of ListModels before/after.
func (s *Server) handleAdminDockerDeleteModel(w http.ResponseWriter, r *http.Request, service string) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST", "OPTIONS")
		return
	}
	if err := s.docker.gateOK(r.Context(), w); err != nil {
		return
	}
	containerName := s.resolveContainerName(r.Context(), service)
	if containerName == "" {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !s.docker.nameAllowed(containerName) {
		http.Error(w, "service not allowed by SD_DOCKER_SERVICE_ALLOWLIST", http.StatusForbidden)
		return
	}
	insp, _ := s.docker.client.InspectContainer(r.Context(), containerName)
	if !strings.Contains(strings.ToLower(insp.Config.Image), "ollama") {
		http.Error(w, "service does not appear to be Ollama", http.StatusNotFound)
		return
	}
	if !insp.State.Running {
		http.Error(w, "container not running; start it first", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	if !s.docker.rateOK(service+":delete_model", body.Model) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "rate_limited",
			"hint":  "wait before retrying the delete",
		})
		return
	}

	cli := NewOllamaProxyClient(ollamaBaseURLForContainer(containerName))

	// Snapshot the model set BEFORE the delete so we can:
	//   1. Refuse if removing this model would leave the container empty
	//   2. Report freed_bytes from the size of the matched entry
	beforeModels, _, err := cli.ListModels(r.Context())
	if err != nil {
		http.Error(w, "ollama list (pre-delete): "+err.Error(), http.StatusBadGateway)
		return
	}
	var targetSize int64
	matched := false
	for _, m := range beforeModels {
		if m.Name == body.Model {
			matched = true
			targetSize = m.Size
			break
		}
	}
	if !matched {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":   "model_not_found",
			"service": service,
			"model":   body.Model,
			"hint":    "GET /admin/docker/services/" + service + "/models to see what's pulled",
		})
		return
	}
	// Only-model guard. Without this, deleting Tier 1's last model would
	// empty its container and silently break realtime ingest.
	if len(beforeModels) == 1 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "only_model",
			"service": service,
			"model":   body.Model,
			"hint":    "Refusing to delete the container's only model. Pull a replacement first, or stop the container if you want it empty.",
		})
		return
	}

	delCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := cli.DeleteModel(delCtx, body.Model); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.auditWrite(AuditEntry{
		Operation:  "docker_delete_model",
		EntityType: "container",
		EntityID:   containerName,
		AfterJSON: mustJSON(map[string]any{
			"model":       body.Model,
			"freed_bytes": targetSize,
		}),
		AdapterID: adapterIDFromRequest(r),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"service":        service,
		"container_name": containerName,
		"model":          body.Model,
		"final_status":   "success",
		"freed_bytes":    targetSize,
	})
}

// firstNonEmpty returns the first non-empty string from the args. Used
// when surfacing compose errors — stderr first, fall back to stdout.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// splitLines splits a multi-line string into trimmed, non-empty lines.
func splitLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────
// Small helpers
// ──────────────────────────────────────────────────────────────────────────

// primaryName returns the canonical container name (no leading slash).
// Docker returns names like "/synaptic-core"; we strip the slash for the
// API surface.
func primaryName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	n := names[0]
	return strings.TrimPrefix(n, "/")
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// slimLabels strips the noisy compose labels that aren't useful in the
// API surface (signatures, image digests, etc.). Keeps anything not in
// the noise set so user-added labels survive.
func slimLabels(in map[string]string) map[string]string {
	noise := map[string]bool{
		"com.docker.compose.config-hash":   true,
		"com.docker.compose.container-number": true,
		"com.docker.compose.depends_on":    true,
		"com.docker.compose.image":         true,
		"com.docker.compose.oneoff":        true,
		"com.docker.compose.version":       true,
	}
	out := map[string]string{}
	for k, v := range in {
		if noise[k] {
			continue
		}
		out[k] = v
	}
	return out
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Suppress unused-import warnings when none of the helpers reference these
// in a given build mode (e.g. test stripping). Cheap insurance.
var _ = os.Getenv
var _ = json.Marshal
