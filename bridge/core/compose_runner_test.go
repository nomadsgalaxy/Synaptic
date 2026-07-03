// compose_runner_test.go — unit coverage for the helper-container argv
// shape. Live daemon round-trips are out of scope.
//
// What we guard:
//   - --project-directory is always passed so compose resolves `./` against
//     the host project dir, not the helper's CWD (/workspace).
//   - It must precede user-supplied subcommand args.
//   - -f points at the in-helper mount of docker-compose.yml.
//
// Failure mode this guards against (compose-helper bind-mount footgun):
// without --project-directory, recreating `core` through the helper
// produced a /workspace:/data bind, leaving /data/index.html missing
// and the dashboard's static handler 404ing.
package main

import (
	"strings"
	"testing"
)

func TestBuildComposeArgs_PassesProjectDirectory(t *testing.T) {
	args := buildComposeArgs(
		"/workspace/docker-compose.yml",
		"synaptic",
		"/run/desktop/mnt/host/c/Users/Anthony/AppData/Local/Synaptic",
		"/workspace/.env",
		[]string{"up", "-d", "core"},
	)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"docker compose",
		"-f /workspace/docker-compose.yml",
		"-p synaptic",
		"--project-directory /run/desktop/mnt/host/c/Users/Anthony/AppData/Local/Synaptic",
		"--env-file /workspace/.env",
		"up -d core",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q; got %q", want, joined)
		}
	}
}

func TestBuildComposeArgs_ProjectDirectoryPrecedesSubcommand(t *testing.T) {
	// --project-directory MUST be parsed as a `compose` flag, not a
	// subcommand arg. If the order ever flips, compose's CLI parser
	// would route it to (say) `up`, which doesn't know that flag, and
	// we'd silently regress the original footgun.
	args := buildComposeArgs(
		"/workspace/docker-compose.yml",
		"p",
		"/host/path",
		"/workspace/.env",
		[]string{"--profile", "tier2", "up", "-d", "core"},
	)
	pdIdx := indexOf(args, "--project-directory")
	upIdx := indexOf(args, "up")
	profileIdx := indexOf(args, "--profile")
	if pdIdx == -1 || upIdx == -1 || profileIdx == -1 {
		t.Fatalf("expected --project-directory / --profile / up; argv=%v", args)
	}
	if pdIdx > profileIdx || pdIdx > upIdx {
		t.Errorf("--project-directory (idx %d) must precede --profile (%d) and up (%d); argv=%v",
			pdIdx, profileIdx, upIdx, args)
	}
}

func TestBuildComposeArgs_FilePointsAtHelperMount(t *testing.T) {
	args := buildComposeArgs("/workspace/docker-compose.yml", "p", "/host/path", "/workspace/.env", []string{"ps"})
	fIdx := indexOf(args, "-f")
	if fIdx == -1 || args[fIdx+1] != "/workspace/docker-compose.yml" {
		t.Errorf("-f should point at /workspace/docker-compose.yml; got %v", args)
	}
	pdIdx := indexOf(args, "--project-directory")
	if pdIdx == -1 || args[pdIdx+1] != "/host/path" {
		t.Errorf("--project-directory should be the host path; got %v", args)
	}
}

func TestBuildComposeArgs_EmptyExtraStillValid(t *testing.T) {
	args := buildComposeArgs("/workspace/docker-compose.yml", "p", "/host", "/workspace/.env", nil)
	if len(args) < 8 {
		t.Errorf("nil extra should still produce the base argv (now incl. --env-file); got %v", args)
	}
}

// When no .env exists on the host install root, the caller passes an
// empty envFile and the flag must NOT appear in argv (compose errors
// with "env file ... not found" if pointed at a missing file).
func TestBuildComposeArgs_OmitsEnvFileWhenEmpty(t *testing.T) {
	args := buildComposeArgs("/workspace/docker-compose.yml", "p", "/host", "", []string{"ps"})
	for _, v := range args {
		if v == "--env-file" {
			t.Errorf("--env-file must not appear when envFile is empty; got %v", args)
		}
	}
}

// translateWindowsHostPath translates Docker Desktop's Windows bind
// sources into the Linux-absolute form a Linux compose binary accepts.
// Guarding both directions: Windows paths get converted, Linux paths
// pass through unchanged.
func TestTranslateWindowsHostPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/already/linux", "/already/linux"},
		{"/run/desktop/mnt/host/c/foo", "/run/desktop/mnt/host/c/foo"},
		{"C:\\Users\\Anthony\\AppData\\Local\\SynapticDisorder",
			"/run/desktop/mnt/host/c/Users/Anthony/AppData/Local/SynapticDisorder"},
		{"D:\\Projects\\synaptic",
			"/run/desktop/mnt/host/d/Projects/synaptic"},
		{"c:\\lower",
			"/run/desktop/mnt/host/c/lower"},
		// Edge case: weird relative-looking path that's not a Windows
		// drive letter. Leave it alone — caller passed garbage, we
		// don't pretend to fix it.
		{"some-relative-path", "some-relative-path"},
	}
	for _, c := range cases {
		if got := translateWindowsHostPath(c.in); got != c.want {
			t.Errorf("translateWindowsHostPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}
