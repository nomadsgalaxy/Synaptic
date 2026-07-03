// compose_runner.go — Wave 7b1 compose-CLI shell-out via a one-shot
// helper container.
//
// The `scratch` base image has no compose CLI (or any shell). Rather
// than fattening the image with a ~50MB compose binary that also
// requires `docker` to invoke, we run a one-shot `docker:cli` helper
// container with the daemon socket + compose project directory mounted.
// The helper runs `docker compose -f <path> -p <project> <args>` and
// exits; we capture its output via the existing log frame parser and
// return it to the caller.
//
// Threat model preserved: the SD_ALLOW_DOCKER_SOCKET gate + service
// allowlist still apply at the handler layer. The helper image is
// pinned (defaults to `docker:cli`) and pulled if missing. The helper
// gets the same docker socket SD Core itself holds, so it has no
// escalated capability — anything it can do, SD Core can already do.
//
// Image size impact: ~0 bytes on SD Core itself. The helper image is
// pulled lazily on first ComposeExec call and cached by the daemon.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultComposeHelperImage is the image the helper runs as. `docker:cli`
// includes the docker daemon CLI + compose v2 plugin. Override with
// SD_DOCKER_COMPOSE_HELPER_IMAGE if a pinned version is needed.
const DefaultComposeHelperImage = "docker:cli"

// ComposeRunner wraps DockerClient with compose-helper logic. Holds the
// resolved host-side compose path so helper containers mount the right
// directory.
type ComposeRunner struct {
	client          *DockerClient
	helperImage     string
	composeHostPath string // host filesystem path to docker-compose.yml's directory
	composeProject  string
	composeFile     string // path inside the helper (always /workspace/<basename>)
}

// NewComposeRunner builds a runner. helperImage may be empty (uses
// DefaultComposeHelperImage). composeHostPath is the HOST directory
// containing docker-compose.yml — this is the value passed to docker
// daemon as a mount source. composeFileInHost is the basename + suffix
// of the compose file (defaults to "docker-compose.yml" when empty).
func NewComposeRunner(client *DockerClient, helperImage, composeHostPath, composeProject, composeFileInHost string) *ComposeRunner {
	if helperImage == "" {
		helperImage = DefaultComposeHelperImage
	}
	if composeFileInHost == "" {
		composeFileInHost = "docker-compose.yml"
	}
	return &ComposeRunner{
		client:          client,
		helperImage:     helperImage,
		composeHostPath: composeHostPath,
		composeProject:  composeProject,
		composeFile:     "/workspace/" + composeFileInHost,
	}
}

// ComposeExecResult is the captured output of one helper-container run.
// ExitCode 0 means success; non-zero is treated as an error by the
// handlers (with the captured Stdout+Stderr surfaced in the response).
type ComposeExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// ComposeExec runs `docker compose -f <file> -p <project> <args>` inside
// a one-shot helper container. Blocks until the helper exits or ctx is
// cancelled. The helper container is auto-removed by the daemon
// (AutoRemove=true).
//
// Output capture: we Create the helper without AutoRemove, Start it,
// Wait for exit, fetch logs, then Remove it explicitly. AutoRemove would
// race with our logs fetch. The explicit teardown is reliable.
//
// Returns ComposeExecResult even on non-zero exit codes — callers
// surface the stderr to the user so failures (missing compose file,
// invalid service name) are diagnosable.
func (cr *ComposeRunner) ComposeExec(ctx context.Context, args []string) (ComposeExecResult, error) {
	if cr == nil || cr.client == nil {
		return ComposeExecResult{}, errors.New("compose runner not initialised")
	}
	if cr.composeHostPath == "" {
		return ComposeExecResult{}, errors.New("compose host path unresolved; set SD_COMPOSE_HOST_PATH")
	}
	start := time.Now()

	// Ensure the helper image is available locally. PullImage is a no-op
	// when the image is already cached; first call may take seconds.
	if exists, err := cr.client.ImageExists(ctx, cr.helperImage); err == nil && !exists {
		if pullErr := cr.client.PullImage(ctx, cr.helperImage); pullErr != nil {
			return ComposeExecResult{}, fmt.Errorf("pull helper image %s: %w", cr.helperImage, pullErr)
		}
	}

	// Build the helper's command. The compose v2 plugin is invoked via
	// `docker compose ...` — NOT the legacy `docker-compose` binary.
	//
	// --project-directory is the load-bearing flag here: without it,
	// compose resolves `./` bind sources against the helper's CWD
	// (/workspace) and any container recreate through the helper gets a
	// broken `/workspace:/data` bind. On Linux hosts cr.composeHostPath
	// is the absolute host path. On Docker Desktop / Windows it's the
	// daemon-translated `/run/desktop/mnt/host/c/…` form supplied by
	// ResolveComposeHostPath (reads it from our own /data mount Source,
	// not from the compose label) so a Linux compose binary's
	// filepath.IsAbs accepts it.
	//
	// --env-file is the OTHER load-bearing flag. Compose v2 looks for
	// `.env` at `--project-directory` by default, not the helper's cwd
	// (`/workspace`). The host-translated `--project-directory` path
	// doesn't exist *inside the helper* — only `/workspace` (the bind
	// mount of the install root) does — so without `--env-file`,
	// compose silently substitutes the `${VAR:-default}` defaults for
	// every variable in docker-compose.yml. Effect: every UI-triggered
	// profile/recreate clobbers the core container's env (SD_API_TOKEN,
	// SD_ALLOW_DOCKER_SOCKET, SD_TIER*_API_KEY, …) on the next compose
	// op, leaving the Services panel locked out and Bearer auth broken.
	// Fix: point `--env-file` at the helper-visible bind path. Only do
	// this when /data/.env actually exists on the host — otherwise
	// compose errors with "env file ... not found".
	envFile := ""
	if hasDotEnvAtDataDir() {
		envFile = "/workspace/.env"
	}
	cmd := buildComposeArgs(cr.composeFile, cr.composeProject, cr.composeHostPath, envFile, args)

	helperName := fmt.Sprintf("sd-compose-helper-%d", time.Now().UnixNano())
	// Bind layout — three mounts, two of them deliberate redundancy:
	//
	//   1. /var/run/docker.sock — talk to the daemon.
	//   2. <host-path>:/workspace:rw — the install dir at a stable
	//      helper-relative path. WorkingDir=/workspace anchors compose's
	//      cwd here, and -f /workspace/docker-compose.yml resolves through
	//      it.
	//   3. <host-path>:<host-path>:ro — the SAME install dir mounted at
	//      its host-translated path. Without this, compose's
	//      `--project-directory <host-path>` invocation succeeds at
	//      resolving `volumes: ./` (those binds get evaluated daemon-side,
	//      where the host path is real), but FAILS on `build: ./bridge/X`
	//      because compose runs a local existence check on the build
	//      context BEFORE shipping it to the daemon, and that check uses
	//      the helper's filesystem view. With the duplicate bind,
	//      `/run/desktop/mnt/host/c/.../bridge/airllm` resolves through
	//      the helper's mount table; without it, "unable to prepare
	//      context: path ... not found" blocks every first-time image
	//      build (`airllm-tier2`, future custom services, etc.).
	binds := []string{
		"/var/run/docker.sock:/var/run/docker.sock:rw",
		cr.composeHostPath + ":/workspace:rw",
	}
	// Skip the duplicate mount if the host path is already `/workspace`
	// (test-only edge case) — Docker rejects two binds with the same
	// source colliding on the same target.
	if cr.composeHostPath != "/workspace" {
		binds = append(binds, cr.composeHostPath+":"+cr.composeHostPath+":ro")
	}
	spec := CreateContainerSpec{
		Image:      cr.helperImage,
		Cmd:        cmd,
		WorkingDir: "/workspace",
		Labels: map[string]string{
			"sd-core.helper": "compose",
			"sd-core.run":    helperName,
		},
		HostConfig: CreateHostConfig{
			Binds:      binds,
			AutoRemove: false, // we remove explicitly so logs fetch can race-free read after wait
		},
	}

	id, err := cr.client.CreateContainer(ctx, helperName, spec)
	if err != nil {
		return ComposeExecResult{}, fmt.Errorf("compose helper create: %w", err)
	}
	// Defer removal so we always clean up even on partial failures.
	defer func() {
		// Use a fresh context for cleanup so a cancelled outer ctx
		// doesn't abandon the helper.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cr.client.RemoveContainer(cleanupCtx, id, true)
	}()

	if err := cr.client.StartContainer(ctx, id); err != nil {
		return ComposeExecResult{}, fmt.Errorf("compose helper start: %w", err)
	}

	exitCode, err := cr.client.WaitContainer(ctx, id)
	if err != nil {
		return ComposeExecResult{}, fmt.Errorf("compose helper wait: %w", err)
	}

	// Capture all logs the helper produced. Tail=0 means "no tail
	// limit"; the daemon returns everything from container birth to
	// now. We use the same multi-frame parser as the regular logs
	// endpoint so stdout/stderr split is preserved.
	//
	// Timestamps=false so parsers like /admin/docker/profiles
	// (`compose config --profiles` → one profile name per line) don't
	// see a leading RFC3339 timestamp prefixed onto every line.
	noTimestamps := false
	var stdoutBuf, stderrBuf bytes.Buffer
	logErr := cr.client.StreamLogs(ctx, id, LogStreamOpts{
		Follow:     false,
		Tail:       0,
		Timestamps: &noTimestamps,
	}, func(l LogLine) {
		switch l.Stream {
		case "stderr":
			stderrBuf.WriteString(l.Line)
			stderrBuf.WriteString("\n")
		default:
			stdoutBuf.WriteString(l.Line)
			stdoutBuf.WriteString("\n")
		}
	})
	if logErr != nil {
		// Log capture failures are non-fatal — we still have the exit code.
		stderrBuf.WriteString(fmt.Sprintf("(log capture: %v)\n", logErr))
	}

	return ComposeExecResult{
		ExitCode: exitCode,
		Stdout:   strings.TrimRight(stdoutBuf.String(), "\n"),
		Stderr:   strings.TrimRight(stderrBuf.String(), "\n"),
		Duration: time.Since(start),
	}, nil
}

// buildComposeArgs renders the argv for the helper container's
// `docker compose ...` invocation. Pulled into its own helper so the
// argument shape (load-bearing for the helper-bind footgun documented
// below) is unit-testable without spinning up a daemon.
//
// Footgun history: any compose recreate routed through this helper that
// touches `core` (or another service whose YAML has a `./` bind) used
// to break the resulting container's /data mount — compose resolved
// `./` against the helper's CWD (/workspace) instead of the host, so
// the new container's bind source was literally `/workspace`. /workspace
// doesn't exist on the host, so /data was empty and SD Core's static
// handler 404'd `GET /`.
//
// Fix: pass `--project-directory <linux-absolute host path>`. The path
// MUST be one compose's Linux binary will accept as absolute via
// filepath.IsAbs — i.e. starts with `/`. ResolveComposeHostPath now
// pulls this from the daemon-translated /data mount Source (which on
// Docker Desktop / Windows comes back as `/run/desktop/mnt/host/c/…`),
// not from the `com.docker.compose.project.working_dir` label (which
// preserves the literal Windows `C:\…` form compose rejects).
//
// The boot self-check in main.go (checkStaticBindMount) is the safety
// net for any future regression of this flag set.
func buildComposeArgs(composeFile, composeProject, hostProjectDir, envFile string, extra []string) []string {
	cmd := []string{"docker", "compose",
		"-f", composeFile,
		"-p", composeProject,
		"--project-directory", hostProjectDir,
	}
	if envFile != "" {
		cmd = append(cmd, "--env-file", envFile)
	}
	return append(cmd, extra...)
}

// hasDotEnvAtDataDir reports whether `.env` exists at the install root,
// viewed from inside SD Core's filesystem (the install dir is bind-mounted
// at /data). Used to decide whether to pass `--env-file` to the helper;
// passing it when the file doesn't exist makes compose error out.
func hasDotEnvAtDataDir() bool {
	dataDir := envOr("SD_DATA_DIR", "/data")
	_, err := os.Stat(filepath.Join(dataDir, ".env"))
	return err == nil
}

// ResolveComposeHostPath figures out the project-directory path that the
// helper container's compose binary should use to resolve `./` bind
// sources. Resolution order:
//
//  1. SD_COMPOSE_HOST_PATH env var (explicit override).
//  2. Our own container's /data mount Source.
//  3. com.docker.compose.project.working_dir label.
//
// On Docker Desktop / Windows the daemon returns Windows-style paths
// (`C:\Users\…`) for both the mount Source and the compose label. A
// Linux compose binary inside the helper container's filepath.IsAbs
// rejects those, so compose joins them with /workspace and the recreate
// fails with "too many colons". To dodge that, we translate Windows
// paths to the Docker Desktop convention `/run/desktop/mnt/host/<drive>/<path>`
// which IS Linux-absolute and which the Docker Desktop VM has bind-mounted
// to the host drive. See translateWindowsHostPath.
//
// Returns "" if nothing is resolvable; callers should refuse to run
// compose ops in that case.
func ResolveComposeHostPath(ctx context.Context, client *DockerClient, selfContainerName string) string {
	if v := envOr("SD_COMPOSE_HOST_PATH", ""); v != "" {
		return v
	}
	if client == nil || selfContainerName == "" {
		return ""
	}
	insp, err := client.InspectContainer(ctx, selfContainerName)
	if err != nil {
		return ""
	}
	var raw string
	for _, m := range insp.Mounts {
		if m.Destination == "/data" && m.Source != "" {
			raw = m.Source
			break
		}
	}
	if raw == "" {
		if v, ok := insp.Labels["com.docker.compose.project.working_dir"]; ok && v != "" {
			raw = v
		}
	}
	if raw == "" {
		return ""
	}
	return translateWindowsHostPath(raw)
}

// translateWindowsHostPath converts `C:\Users\foo` →
// `/run/desktop/mnt/host/c/Users/foo` for Docker Desktop. Leaves any path
// that's already Linux-style (starts with `/`) untouched, so Linux hosts
// are unaffected. The translation is the convention Docker Desktop uses
// to expose host drives inside its Linux VM — using it as the source
// makes a Linux compose binary's filepath.IsAbs return true.
func translateWindowsHostPath(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	// Recognise `<letter>:` or `<letter>:\` at the start.
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(string(p[0]))
		rest := strings.TrimPrefix(p[2:], "\\")
		rest = strings.ReplaceAll(rest, "\\", "/")
		return "/run/desktop/mnt/host/" + drive + "/" + rest
	}
	return p
}
