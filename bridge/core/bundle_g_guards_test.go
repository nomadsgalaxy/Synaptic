// bundle_g_guards_test.go — guards for v2.6 Bundle G (user-managed
// Docker containers attached via ProviderConfig.Managed):
//
//   - Validate accepts a complete Managed block, rejects empty
//     image / invalid container_name / out-of-range ports
//   - Validate is a no-op when the Managed block is unset (the default
//     for every existing user; this is the test that locks in
//     "feature is opt-in, doesn't break default-stack users")
//   - userManagedServiceNames harvests names only when
//     LifecycleEnabled=true; nil bank is safe
//   - Per-tier settings round-trip preserves the Managed block
//     through bank.SetProviderConfig + GetProviderConfig
//   - Cross-tier dedup so two tiers attaching the same container_name
//     produce only one candidate-service entry (the lifecycle reaper
//     would otherwise emit duplicate stop calls)
package main

import (
	"testing"
)

// ── Validate ─────────────────────────────────────────────────────────────

func TestProviderConfigValidate_ManagedBlock_Accepts(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://192.168.1.50:1234/v1",
		Model:   "qwen-3.5-4b",
		Managed: &ManagedContainer{
			Image:            "ghcr.io/ggerganov/llama.cpp:server",
			ContainerName:    "my-llama-container",
			HostPort:         1234,
			ContainerPort:    8080,
			Env:              []string{"OMP_NUM_THREADS=4"},
			Volumes:          []string{"/data/models:/models"},
			LifecycleEnabled: true,
		},
	}
	if err := cfg.Validate(Tier2); err != nil {
		t.Errorf("expected valid config, got %v", err)
	}
}

func TestProviderConfigValidate_ManagedBlock_RejectsEmptyImage(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://localhost:1234/v1",
		Model:   "m",
		Managed: &ManagedContainer{
			ContainerName: "my-llama",
			// Image omitted
		},
	}
	if err := cfg.Validate(Tier2); err == nil {
		t.Errorf("expected reject when image missing")
	}
}

func TestProviderConfigValidate_ManagedBlock_RejectsEmptyContainerName(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://localhost:1234/v1",
		Model:   "m",
		Managed: &ManagedContainer{Image: "scratch"},
	}
	if err := cfg.Validate(Tier2); err == nil {
		t.Errorf("expected reject when container_name missing")
	}
}

func TestProviderConfigValidate_ManagedBlock_RejectsBadCharsInName(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://localhost:1234/v1",
		Model:   "m",
		Managed: &ManagedContainer{
			Image:         "scratch",
			ContainerName: "my llama!", // space + bang are illegal in docker container names
		},
	}
	if err := cfg.Validate(Tier2); err == nil {
		t.Errorf("expected reject for illegal container_name characters")
	}
}

func TestProviderConfigValidate_ManagedBlock_RejectsOutOfRangePorts(t *testing.T) {
	cfg := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://localhost:1234/v1",
		Model:   "m",
		Managed: &ManagedContainer{
			Image:         "scratch",
			ContainerName: "my-llama",
			HostPort:      70000, // > 65535
		},
	}
	if err := cfg.Validate(Tier2); err == nil {
		t.Errorf("expected reject for out-of-range host_port")
	}
}

// The most important "default behaviour preserved" guard: the v2.5 user
// who never touches the Managed block must not see a validator regression.
func TestProviderConfigValidate_NilManagedIsNoop(t *testing.T) {
	cfg := ProviderConfig{
		Kind:  ProviderOllama,
		Model: "llama3.2:3b",
	}
	if err := cfg.Validate(Tier1); err != nil {
		t.Errorf("default config (no Managed block) should validate clean, got %v", err)
	}
}

// ── userManagedServiceNames ──────────────────────────────────────────────

func TestUserManagedServiceNames_NilBankSafe(t *testing.T) {
	if names := userManagedServiceNames(nil); len(names) != 0 {
		t.Errorf("nil bank should produce empty list, got %v", names)
	}
}

func TestUserManagedServiceNames_EmptyBankIsEmpty(t *testing.T) {
	bank := newTestBank(t)
	if names := userManagedServiceNames(bank); len(names) != 0 {
		t.Errorf("empty bank should produce empty list, got %v", names)
	}
}

func TestUserManagedServiceNames_OnlyIncludesLifecycleEnabled(t *testing.T) {
	bank := newTestBank(t)
	// Tier 1: Managed block, but LifecycleEnabled=false → SKIPPED.
	cfg1 := ProviderConfig{
		Kind:    ProviderOllama,
		Model:   "llama3.2:3b",
		Managed: &ManagedContainer{Image: "ollama/ollama", ContainerName: "tier1-container"},
	}
	if err := bank.SaveProviderConfig(Tier1, cfg1); err != nil {
		t.Fatalf("SaveProviderConfig Tier1: %v", err)
	}
	// Tier 2: Managed block + LifecycleEnabled=true → INCLUDED.
	cfg2 := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://192.168.1.50:1234/v1",
		Model:   "qwen-4b",
		Managed: &ManagedContainer{
			Image:            "ghcr.io/ggerganov/llama.cpp:server",
			ContainerName:    "tier2-container",
			LifecycleEnabled: true,
		},
	}
	if err := bank.SaveProviderConfig(Tier2, cfg2); err != nil {
		t.Fatalf("SaveProviderConfig Tier2: %v", err)
	}
	names := userManagedServiceNames(bank)
	if len(names) != 1 || names[0] != "tier2-container" {
		t.Errorf("expected [tier2-container], got %v", names)
	}
}

func TestUserManagedServiceNames_DedupesAcrossTiers(t *testing.T) {
	bank := newTestBank(t)
	mc := &ManagedContainer{
		Image:            "scratch",
		ContainerName:    "shared-container",
		LifecycleEnabled: true,
	}
	// Two tiers attach the SAME container name. Reaper should see one
	// candidate, not two.
	for _, tier := range []TierKey{Tier1, Tier2} {
		cfg := ProviderConfig{
			Kind:    ProviderCustom,
			BaseURL: "http://localhost:1234/v1",
			Model:   "m",
			Managed: mc,
		}
		if err := bank.SaveProviderConfig(tier, cfg); err != nil {
			t.Fatalf("SaveProviderConfig %s: %v", tier, err)
		}
	}
	names := userManagedServiceNames(bank)
	count := 0
	for _, n := range names {
		if n == "shared-container" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected shared-container once, saw %d times in %v", count, names)
	}
}

// ── Round-trip ──────────────────────────────────────────────────────────

func TestProviderConfig_ManagedBlockRoundTrip(t *testing.T) {
	bank := newTestBank(t)
	in := ProviderConfig{
		Kind:    ProviderCustom,
		BaseURL: "http://192.168.1.50:1234/v1",
		Model:   "qwen-4b",
		Managed: &ManagedContainer{
			Image:            "ghcr.io/ggerganov/llama.cpp:server",
			ContainerName:    "my-llama",
			HostPort:         1234,
			ContainerPort:    8080,
			Env:              []string{"OMP_NUM_THREADS=4", "GGML_NTHREADS=4"},
			Volumes:          []string{"/data/models:/models"},
			LifecycleEnabled: true,
		},
	}
	if err := bank.SaveProviderConfig(Tier2, in); err != nil {
		t.Fatalf("SaveProviderConfig: %v", err)
	}
	out, err := bank.GetProviderConfig(Tier2)
	if err != nil {
		t.Fatalf("GetProviderConfig: %v", err)
	}
	if out.Managed == nil {
		t.Fatalf("Managed block did not round-trip — got nil")
	}
	if out.Managed.Image != in.Managed.Image {
		t.Errorf("Image: got %q, want %q", out.Managed.Image, in.Managed.Image)
	}
	if out.Managed.ContainerName != in.Managed.ContainerName {
		t.Errorf("ContainerName: got %q, want %q", out.Managed.ContainerName, in.Managed.ContainerName)
	}
	if out.Managed.HostPort != in.Managed.HostPort {
		t.Errorf("HostPort: got %d, want %d", out.Managed.HostPort, in.Managed.HostPort)
	}
	if !out.Managed.LifecycleEnabled {
		t.Errorf("LifecycleEnabled should round-trip true, got false")
	}
	if len(out.Managed.Env) != 2 || len(out.Managed.Volumes) != 1 {
		t.Errorf("Env/Volumes slices truncated: Env=%v Volumes=%v", out.Managed.Env, out.Managed.Volumes)
	}
}

// ── IsZero ──────────────────────────────────────────────────────────────

func TestManagedContainer_IsZero(t *testing.T) {
	cases := []struct {
		name string
		m    *ManagedContainer
		want bool
	}{
		{"nil", nil, true},
		{"empty struct", &ManagedContainer{}, true},
		{"only image", &ManagedContainer{Image: "x"}, false},
		{"only lifecycle flag", &ManagedContainer{LifecycleEnabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.IsZero(); got != tc.want {
				t.Errorf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}
