package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndGlobalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  operation_timeout: 30m\n  keep_last: 7\nprojects:\n  - name: prod\n    include: [id:42]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.LabelSelector != DefaultSelector {
		t.Fatalf("selector = %q", cfg.Defaults.LabelSelector)
	}
	if cfg.Defaults.OperationTimeout != 30*time.Minute {
		t.Fatalf("timeout = %s", cfg.Defaults.OperationTimeout)
	}
	policy := cfg.Policy()
	if policy.KeepMin != 1 || policy.KeepLast != 7 || policy.MinAge != 0 || policy.MaxAge != 0 {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestLoadParsesRetentionAges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  keep_min: 2\n  keep_last: 5\n  min_age: 1d12h\n  max_age: 6w\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.MinAge != 36*time.Hour || cfg.Defaults.MaxAge != 6*7*24*time.Hour {
		t.Fatalf("ages = %s, %s", cfg.Defaults.MinAge, cfg.Defaults.MaxAge)
	}
	if cfg.Defaults.MinAgeRaw != "1d12h" || cfg.Defaults.MaxAgeRaw != "6w" {
		t.Fatalf("raw ages = %q, %q", cfg.Defaults.MinAgeRaw, cfg.Defaults.MaxAgeRaw)
	}
}

func TestLoadDisablesExplicitZeroRetentionAges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ndefaults:\n  min_age: 0\n  max_age: 0h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.MinAge != 0 || cfg.Defaults.MaxAge != 0 {
		t.Fatalf("ages = %s, %s", cfg.Defaults.MinAge, cfg.Defaults.MaxAge)
	}
}

func TestParseRetentionDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"": 0, "0": 0, "24h": 24 * time.Hour, "30d": 30 * 24 * time.Hour,
		"1w2d12h": 228 * time.Hour, "1.5d": 36 * time.Hour,
	} {
		t.Run(input, func(t *testing.T) {
			got, err := ParseRetentionDuration(input)
			if err != nil || got != want {
				t.Fatalf("ParseRetentionDuration(%q) = %s, %v; want %s", input, got, err, want)
			}
		})
	}
	for _, input := range []string{"-1h", "1 month", "1d-nope"} {
		if _, err := ParseRetentionDuration(input); err == nil {
			t.Fatalf("ParseRetentionDuration(%q) succeeded", input)
		}
	}
}

func TestLoadRejectsPerProjectPolicyOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nprojects:\n  - name: prod\n    keep_last: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestLoadPreservesDisabledSelector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  label_selector: \"\"\nprojects:\n  - name: prod\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.LabelSelector != "" {
		t.Fatalf("selector = %q", cfg.Defaults.LabelSelector)
	}
}

func TestValidationRejectsUnsafeReferencesAndRetention(t *testing.T) {
	for name, cfg := range map[string]Config{
		"reference":    {Version: 1, Defaults: Default().Defaults, Projects: []Project{{Name: "prod", Include: []string{"server"}}}},
		"retention":    {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepLast = -1; return d }()},
		"keep minimum": {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepMin = 4; return d }()},
		"age order":    {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.MinAgeRaw = "30d"; d.MaxAgeRaw = "1w"; return d }()},
		"invalid age":  {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.MinAgeRaw = "tomorrow"; return d }()},
	} {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSaveUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	cfg := Default()
	cfg.UpsertProject("prod")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", dirInfo.Mode().Perm())
	}
}
