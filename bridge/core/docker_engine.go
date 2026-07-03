// docker_engine.go — Wave 7a Docker control plane client.
//
// Minimal HTTP client over the Docker Engine API (unix socket). We talk
// directly to the daemon rather than vendoring `github.com/docker/docker/client`:
//
//   - The whole interaction surface is ~5 endpoints; the SDK pulls in
//     hundreds of types we don't use.
//   - Single-platform target: SD Core runs in a Linux container, so we
//     always dial /var/run/docker.sock. Docker Desktop on Windows/macOS
//     surfaces the same Unix socket inside our container, so no Windows
//     named-pipe support is required.
//   - The Engine API is stable + versionless on common paths (the v1.41
//     prefix is optional in practice; the daemon falls back to its own
//     min/max-supported version).
//
// Scope shipped this wave:
//   - List + inspect containers
//   - Start / Stop / Restart actions
//   - Log streaming (line-delimited, suitable for SSE re-emission)
//
// Compose-specific operations (recreate / build / profile up-down) shell
// out to `docker compose` from a separate handler file in a later wave.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultDockerSocketPath is where the daemon listens on Linux + Docker
// Desktop's Linux-VM. Override with SD_DOCKER_SOCKET if the user wired a
// non-default mount path.
const DefaultDockerSocketPath = "/var/run/docker.sock"

// DockerClient is the tiny Engine-API client.
type DockerClient struct {
	socketPath string
	http       *http.Client
}

// NewDockerClient builds the client. Doesn't dial — first request does.
// Callers should treat NewDockerClient as cheap.
func NewDockerClient(socketPath string) *DockerClient {
	if socketPath == "" {
		socketPath = DefaultDockerSocketPath
	}
	return &DockerClient{
		socketPath: socketPath,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Available returns nil if the daemon is reachable + responsive. Used by
// the gate to decide between "available", "configured but unreachable",
// and the gate-disabled state.
func (c *DockerClient) Available(ctx context.Context) error {
	if c == nil {
		return errors.New("docker client not initialised")
	}
	if _, err := os.Stat(c.socketPath); err != nil {
		return fmt.Errorf("docker socket not present at %s: %w", c.socketPath, err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker daemon ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker daemon ping: %s", resp.Status)
	}
	return nil
}

// ContainerSummary is the trimmed projection of the Engine `containers/json`
// payload we care about for the Services panel.
type ContainerSummary struct {
	ID         string            `json:"id"`
	Names      []string          `json:"names"`        // includes leading slash, e.g. "/synaptic-core"
	Image      string            `json:"image"`
	State      string            `json:"state"`        // running | exited | restarting | paused | created
	Status     string            `json:"status"`       // human-readable "Up 23 minutes (healthy)"
	Labels     map[string]string `json:"labels"`
	Ports      []ContainerPort   `json:"ports"`
	CreatedAt  int64             `json:"created"`      // unix seconds
}

// ContainerPort is one published-or-exposed port mapping. PublicPort is
// the host-side port; PrivatePort the in-container port.
type ContainerPort struct {
	PrivatePort int    `json:"private_port"`
	PublicPort  int    `json:"public_port,omitempty"`
	Type        string `json:"type"` // tcp | udp
}

// ContainerInspect is a slim projection of `GET /containers/{id}/json`
// — only the fields we surface in /admin/docker/services responses.
type ContainerInspect struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created string `json:"created"`
	State   struct {
		Status     string `json:"status"`
		Running    bool   `json:"running"`
		StartedAt  string `json:"started_at"`
		FinishedAt string `json:"finished_at"`
		Restarts   int    `json:"restarts"`
		Health     struct {
			Status string `json:"status"` // starting | healthy | unhealthy
		} `json:"health"`
	} `json:"state"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels"`
	Config struct {
		Image string `json:"image"`
	} `json:"config"`
	// Mounts mirrors the daemon's resolved bind mounts. The Source field
	// here is daemon-translated — on Docker Desktop / Windows this is the
	// Linux-friendly form (`/run/desktop/mnt/host/c/…`), not the original
	// `C:\…` literal that lives in com.docker.compose.project.working_dir.
	// Used by ResolveComposeHostPath to pick a value compose's Linux
	// binary can call filepath.IsAbs on.
	Mounts []InspectMount `json:"mounts,omitempty"`
}

// InspectMount is one row of ContainerInspect.Mounts. Only Source +
// Destination are exposed — we don't care about Mode / RW for the
// resolver path.
type InspectMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// ListContainers calls `GET /containers/json?all=1` with optional label
// filter (key=value). When labelFilter is non-empty, only containers with
// a matching label are returned.
func (c *DockerClient) ListContainers(ctx context.Context, labelFilter map[string]string) ([]ContainerSummary, error) {
	if c == nil {
		return nil, errors.New("docker client nil")
	}
	q := url.Values{}
	q.Set("all", "1")
	if len(labelFilter) > 0 {
		filtersObj := map[string][]string{"label": {}}
		for k, v := range labelFilter {
			filtersObj["label"] = append(filtersObj["label"], k+"="+v)
		}
		fb, _ := json.Marshal(filtersObj)
		q.Set("filters", string(fb))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/json?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("docker list: %s: %s", resp.Status, body)
	}
	// Engine returns capitalised JSON keys; we decode to a shape that
	// matches both casings via field tags via an intermediate struct.
	var raw []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		State   string            `json:"State"`
		Status  string            `json:"Status"`
		Labels  map[string]string `json:"Labels"`
		Created int64             `json:"Created"`
		Ports   []struct {
			PrivatePort int    `json:"PrivatePort"`
			PublicPort  int    `json:"PublicPort"`
			Type        string `json:"Type"`
		} `json:"Ports"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]ContainerSummary, len(raw))
	for i, r := range raw {
		ports := make([]ContainerPort, 0, len(r.Ports))
		for _, p := range r.Ports {
			ports = append(ports, ContainerPort{
				PrivatePort: p.PrivatePort,
				PublicPort:  p.PublicPort,
				Type:        p.Type,
			})
		}
		out[i] = ContainerSummary{
			ID:        r.ID,
			Names:     r.Names,
			Image:     r.Image,
			State:     r.State,
			Status:    r.Status,
			Labels:    r.Labels,
			Ports:     ports,
			CreatedAt: r.Created,
		}
	}
	return out, nil
}

// InspectContainer calls `GET /containers/{id}/json`. id may be a name or
// an id; the Engine accepts both.
func (c *DockerClient) InspectContainer(ctx context.Context, id string) (ContainerInspect, error) {
	var out ContainerInspect
	if c == nil {
		return out, errors.New("docker client nil")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+url.PathEscape(id)+"/json", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return out, errContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("docker inspect: %s: %s", resp.Status, body)
	}
	// Pull the few fields we need via a permissive raw map; the full
	// inspect payload has 40+ top-level fields most of which we don't
	// surface. Decoding only into ContainerInspect with json tags would
	// match `Id`/`State`/etc. case-insensitively in Go's stdlib decoder,
	// but to be explicit we decode through a shaped struct.
	var raw struct {
		ID      string `json:"Id"`
		Name    string `json:"Name"`
		Created string `json:"Created"`
		State   struct {
			Status     string `json:"Status"`
			Running    bool   `json:"Running"`
			StartedAt  string `json:"StartedAt"`
			FinishedAt string `json:"FinishedAt"`
			Restarts   int    `json:"RestartCount"`
			Health     struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Config.Labels"` // not used; labels also under Config
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		Mounts []struct {
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return out, err
	}
	out.ID = raw.ID
	out.Name = raw.Name
	out.Created = raw.Created
	out.State.Status = raw.State.Status
	out.State.Running = raw.State.Running
	out.State.StartedAt = raw.State.StartedAt
	out.State.FinishedAt = raw.State.FinishedAt
	out.State.Restarts = raw.State.Restarts
	out.State.Health.Status = raw.State.Health.Status
	out.Image = raw.Image
	out.Labels = raw.Config.Labels
	out.Config.Image = raw.Config.Image
	for _, m := range raw.Mounts {
		out.Mounts = append(out.Mounts, InspectMount{Source: m.Source, Destination: m.Destination})
	}
	return out, nil
}

var errContainerNotFound = errors.New("container not found")

// IsContainerNotFound is the exported predicate so handlers can map to 404.
func IsContainerNotFound(err error) bool {
	return errors.Is(err, errContainerNotFound)
}

// StartContainer calls `POST /containers/{id}/start`.
func (c *DockerClient) StartContainer(ctx context.Context, id string) error {
	return c.simpleAction(ctx, "start", id, nil)
}

// StopContainer calls `POST /containers/{id}/stop?t=<seconds>`. timeout is
// the SIGTERM grace before SIGKILL. Default 10s when timeout <= 0.
func (c *DockerClient) StopContainer(ctx context.Context, id string, timeout int) error {
	q := url.Values{}
	if timeout > 0 {
		q.Set("t", strconv.Itoa(timeout))
	}
	return c.simpleAction(ctx, "stop", id, q)
}

// RestartContainer calls `POST /containers/{id}/restart?t=<seconds>`.
func (c *DockerClient) RestartContainer(ctx context.Context, id string, timeout int) error {
	q := url.Values{}
	if timeout > 0 {
		q.Set("t", strconv.Itoa(timeout))
	}
	return c.simpleAction(ctx, "restart", id, q)
}

func (c *DockerClient) simpleAction(ctx context.Context, action, id string, q url.Values) error {
	if c == nil {
		return errors.New("docker client nil")
	}
	u := "http://docker/containers/" + url.PathEscape(id) + "/" + action
	if q != nil && len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 204 No Content = success; 304 Not Modified = already in state
	// (e.g. start on running). The Engine treats 304 as "ok, no-op."
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return errContainerNotFound
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("docker %s: %s: %s", action, resp.Status, body)
}

// LogLine is one parsed log entry. Stream is "stdout" or "stderr".
type LogLine struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// StreamLogs calls `GET /containers/{id}/logs?stdout=1&stderr=1&...` and
// invokes onLine for each parsed line. When follow=true, blocks until ctx
// is cancelled or the daemon closes the stream. When follow=false, returns
// after the historical chunk completes.
//
// Output framing: when the container has no TTY (the default for
// service containers), Docker prefixes each chunk with an 8-byte header
// `{stream:1, _:3, size:4}`. We parse that to separate stdout/stderr.
// When the container DOES have a TTY (rare in our stack), the stream is
// raw; we treat the whole thing as stdout.
func (c *DockerClient) StreamLogs(ctx context.Context, id string, opts LogStreamOpts, onLine func(LogLine)) error {
	if c == nil {
		return errors.New("docker client nil")
	}
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	if opts.Follow {
		q.Set("follow", "1")
	}
	if opts.Tail > 0 {
		q.Set("tail", strconv.Itoa(opts.Tail))
	}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	// Timestamps default true (preserves prior behaviour for /admin/docker/services/{name}/logs)
	// unless the caller explicitly opted out.
	if opts.Timestamps == nil || *opts.Timestamps {
		q.Set("timestamps", "1")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker logs: %s: %s", resp.Status, body)
	}
	return parseLogStream(resp.Body, onLine)
}

// LogStreamOpts captures the optional query knobs.
type LogStreamOpts struct {
	Follow bool
	Tail   int
	Since  string // RFC3339 or "1m" / "30s"
	// Timestamps controls whether the daemon prefixes each log line with
	// an RFC3339 timestamp. Defaults to true for human-readable log
	// streams; the compose runner sets it false because we parse the
	// raw command output (e.g. `compose config --profiles` produces one
	// profile name per line and we don't want a timestamp prefix
	// corrupting the parse).
	Timestamps *bool
}

// ──────────────────────────────────────────────────────────────────────────
// Wave 7b1: container create / wait / remove + system_df + image pull
// (needed by the helper-container approach for compose CLI shell-out)
// ──────────────────────────────────────────────────────────────────────────

// CreateContainerSpec is the trimmed projection of the Engine's
// /containers/create request body we need for one-shot helper containers
// (docker:cli running compose subcommands). Fields are intentionally
// minimal — the helper-container surface is narrow and audited.
type CreateContainerSpec struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`        // "KEY=value" entries
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig CreateHostConfig  `json:"HostConfig"`
}

// CreateHostConfig is the inner HostConfig projection. Binds are
// "host_path:container_path[:ro]" strings (the same format the docker
// CLI uses for -v). AutoRemove tells the daemon to remove the container
// once it exits — convenient for one-shot helper runs.
type CreateHostConfig struct {
	Binds      []string `json:"Binds,omitempty"`
	AutoRemove bool     `json:"AutoRemove,omitempty"`
	NetworkMode string  `json:"NetworkMode,omitempty"` // "host" | "" (bridge default)
}

// CreateContainer calls POST /containers/create?name=<name>. Returns the
// new container's id. The container is NOT started — call StartContainer
// afterward.
func (c *DockerClient) CreateContainer(ctx context.Context, name string, spec CreateContainerSpec) (string, error) {
	if c == nil {
		return "", errors.New("docker client nil")
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	u := "http://docker/containers/create"
	if name != "" {
		u += "?name=" + url.QueryEscape(name)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("docker create: %s: %s", resp.Status, raw)
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// WaitContainer blocks on POST /containers/{id}/wait?condition=not-running
// and returns the exit code when the container exits. Used by the
// helper-container compose runner to learn whether the compose command
// succeeded. Timeout is governed by ctx — pass a context with a deadline
// suitable for the operation (builds can be minutes; restarts are fast).
func (c *DockerClient) WaitContainer(ctx context.Context, id string) (int, error) {
	if c == nil {
		return 0, errors.New("docker client nil")
	}
	// Use a separate http client with no timeout — wait can be long.
	noTimeoutClient := &http.Client{
		Transport: c.http.Transport,
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/containers/"+url.PathEscape(id)+"/wait?condition=not-running", nil)
	resp, err := noTimeoutClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("docker wait: %s: %s", resp.Status, raw)
	}
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, fmt.Errorf("container wait reported error: %s", out.Error.Message)
	}
	return out.StatusCode, nil
}

// RemoveContainer calls DELETE /containers/{id}?force=1. Used to clean up
// helper containers after their one-shot run.
func (c *DockerClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	if c == nil {
		return errors.New("docker client nil")
	}
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://docker/containers/"+url.PathEscape(id)+"?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("docker rm: %s: %s", resp.Status, raw)
}

// SystemDF is the trimmed projection of GET /system/df we surface in
// /admin/docker/disk. Volumes and Images carry sizes for the dashboard's
// disk affordance.
type SystemDF struct {
	LayersSize int64               `json:"layers_size"`
	Images     []SystemDFImage     `json:"images"`
	Containers []SystemDFContainer `json:"containers"`
	Volumes    []SystemDFVolume    `json:"volumes"`
}

type SystemDFImage struct {
	ID         string `json:"id"`
	Repository string `json:"repository,omitempty"`
	Size       int64  `json:"size"`
	SharedSize int64  `json:"shared_size,omitempty"`
}

type SystemDFContainer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	SizeRW     int64  `json:"size_rw"`
	SizeRootFs int64  `json:"size_root_fs"`
}

type SystemDFVolume struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Size     int64  `json:"size"`
	RefCount int    `json:"ref_count"`
}

// GetSystemDF calls GET /system/df and returns the trimmed projection.
// Used by /admin/docker/disk.
func (c *DockerClient) GetSystemDF(ctx context.Context) (SystemDF, error) {
	var out SystemDF
	if c == nil {
		return out, errors.New("docker client nil")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/system/df", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("docker system df: %s: %s", resp.Status, raw)
	}
	// Engine returns mixed-case keys with nested structures we don't need;
	// decode through an intermediate.
	var raw struct {
		LayersSize int64 `json:"LayersSize"`
		Images     []struct {
			ID         string   `json:"Id"`
			RepoTags   []string `json:"RepoTags"`
			Size       int64    `json:"Size"`
			SharedSize int64    `json:"SharedSize"`
		} `json:"Images"`
		Containers []struct {
			ID         string   `json:"Id"`
			Names      []string `json:"Names"`
			State      string   `json:"State"`
			SizeRw     int64    `json:"SizeRw"`
			SizeRootFs int64    `json:"SizeRootFs"`
		} `json:"Containers"`
		Volumes []struct {
			Name       string `json:"Name"`
			Driver     string `json:"Driver"`
			UsageData  struct {
				Size     int64 `json:"Size"`
				RefCount int   `json:"RefCount"`
			} `json:"UsageData"`
		} `json:"Volumes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return out, err
	}
	out.LayersSize = raw.LayersSize
	for _, im := range raw.Images {
		repo := ""
		if len(im.RepoTags) > 0 && im.RepoTags[0] != "<none>:<none>" {
			repo = im.RepoTags[0]
		}
		out.Images = append(out.Images, SystemDFImage{
			ID: shortID(im.ID), Repository: repo, Size: im.Size, SharedSize: im.SharedSize,
		})
	}
	for _, ct := range raw.Containers {
		out.Containers = append(out.Containers, SystemDFContainer{
			ID:         shortID(ct.ID),
			Name:       primaryName(ct.Names),
			State:      ct.State,
			SizeRW:     ct.SizeRw,
			SizeRootFs: ct.SizeRootFs,
		})
	}
	for _, v := range raw.Volumes {
		out.Volumes = append(out.Volumes, SystemDFVolume{
			Name: v.Name, Driver: v.Driver,
			Size: v.UsageData.Size, RefCount: v.UsageData.RefCount,
		})
	}
	return out, nil
}

// SystemInfo is the trimmed projection of GET /info we use for host-level
// probes (today: GPU detection via the Runtimes map). nvidia-container-
// toolkit registers a "nvidia" runtime on install — its presence is a
// reliable proxy for "NVIDIA driver + CUDA available on the host."
type SystemInfo struct {
	Runtimes        map[string]struct{} `json:"runtimes"`
	DefaultRuntime  string              `json:"default_runtime"`
	OperatingSystem string              `json:"operating_system"`
}

// GetSystemInfo calls GET /info and returns the trimmed projection used
// by gpu_probe.go.
func (c *DockerClient) GetSystemInfo(ctx context.Context) (SystemInfo, error) {
	var out SystemInfo
	if c == nil {
		return out, errors.New("docker client nil")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/info", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("docker info: %s: %s", resp.Status, raw)
	}
	var raw struct {
		Runtimes        map[string]json.RawMessage `json:"Runtimes"`
		DefaultRuntime  string                     `json:"DefaultRuntime"`
		OperatingSystem string                     `json:"OperatingSystem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return out, err
	}
	out.DefaultRuntime = raw.DefaultRuntime
	out.OperatingSystem = raw.OperatingSystem
	out.Runtimes = make(map[string]struct{}, len(raw.Runtimes))
	for k := range raw.Runtimes {
		out.Runtimes[k] = struct{}{}
	}
	return out, nil
}

// PullImage calls POST /images/create?fromImage=<image>&tag=<tag>. The
// daemon streams progress as a sequence of JSON objects. We drain the
// stream until it ends, looking for an error field. Used to ensure the
// docker:cli helper image is locally available before ComposeExec runs.
func (c *DockerClient) PullImage(ctx context.Context, image string) error {
	if c == nil {
		return errors.New("docker client nil")
	}
	// Split image:tag (default to "latest").
	imageName, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > 0 {
		imageName = image[:i]
		tag = image[i+1:]
	}
	q := url.Values{}
	q.Set("fromImage", imageName)
	q.Set("tag", tag)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/images/create?"+q.Encode(), nil)
	noTimeoutClient := &http.Client{Transport: c.http.Transport}
	resp, err := noTimeoutClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker pull: %s: %s", resp.Status, raw)
	}
	// Stream of newline-delimited JSON objects. Look for an "error" field.
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var msg struct {
			Status   string `json:"status"`
			Error    string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("docker pull stream error: %s", msg.Error)
		}
	}
	return nil
}

// ImageExists returns true if the daemon reports the image present
// locally. Used by ComposeExec to skip the pull when the helper image is
// already cached.
func (c *DockerClient) ImageExists(ctx context.Context, image string) (bool, error) {
	if c == nil {
		return false, errors.New("docker client nil")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/images/"+url.PathEscape(image)+"/json", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("docker image inspect: %s: %s", resp.Status, raw)
}

// parseLogStream walks the docker multiplexed log frame format. Each
// frame is 8 bytes: [stream(1) reserved(3) size(4 BE)] followed by `size`
// bytes of payload. The payload usually contains a trailing newline; we
// split on \n and emit one LogLine per text line.
func parseLogStream(r io.Reader, onLine func(LogLine)) error {
	header := make([]byte, 8)
	buf := bytes.Buffer{}
	streamLabel := func(b byte) string {
		switch b {
		case 1:
			return "stdout"
		case 2:
			return "stderr"
		default:
			return "stdout"
		}
	}
	for {
		_, err := io.ReadFull(r, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Flush any trailing buffered partial line.
			if buf.Len() > 0 {
				onLine(LogLine{Stream: "stdout", Line: buf.String()})
			}
			return nil
		}
		if err != nil {
			return err
		}
		size := int(binary.BigEndian.Uint32(header[4:8]))
		if size <= 0 || size > 1<<20 { // 1 MiB sanity cap
			return fmt.Errorf("docker logs: invalid frame size %d", size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		stream := streamLabel(header[0])
		// Split payload on newline; emit one LogLine per line. Keep
		// any incomplete tail in buf for the next frame.
		buf.Write(payload)
		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				// No more complete lines this frame; preserve the
				// partial for next iteration.
				buf.Reset()
				buf.WriteString(line)
				break
			}
			onLine(LogLine{Stream: stream, Line: strings.TrimRight(line, "\n")})
		}
	}
}
