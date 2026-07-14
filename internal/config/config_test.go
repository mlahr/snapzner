package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAndGlobalPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  operation_timeout: 30m\n  keep_max: 7\nprojects:\n  - name: prod\n    include: [id:42]\n")
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
	if policy.KeepMax != 7 || policy.KeepLatest != 2 || len(policy.KeepTargets) != 3 {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestLoadParsesRetentionTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  keep_max: 4\n  keep_latest: 2\n  keep_targets: [1d12h, 6w]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Defaults.KeepTargets) != 2 || cfg.Defaults.KeepTargets[0] != 36*time.Hour || cfg.Defaults.KeepTargets[1] != 6*7*24*time.Hour {
		t.Fatalf("targets = %v", cfg.Defaults.KeepTargets)
	}
	if len(cfg.Defaults.KeepTargetsRaw) != 2 || cfg.Defaults.KeepTargetsRaw[0] != "1d12h" || cfg.Defaults.KeepTargetsRaw[1] != "6w" {
		t.Fatalf("raw targets = %q", cfg.Defaults.KeepTargetsRaw)
	}
}

func TestLoadRejectsZeroRetentionTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ndefaults:\n  keep_targets: [0]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadMigratesLegacyRetentionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  retention_label: AUTOBACKUP.KEEP-LAST\n  keep_min: 1\n  keep_last: 3\n  min_age: 24h\n  max_age: 30d\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LegacyRetentionFields {
		t.Fatal("legacy retention fields were not reported")
	}
	if cfg.Defaults.RetentionLabel != DefaultRetentionLabel || cfg.Defaults.KeepMax != 5 || cfg.Defaults.KeepLatest != 2 || len(cfg.Defaults.KeepTargets) != 3 {
		t.Fatalf("migrated defaults = %+v", cfg.Defaults)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"keep_min:", "keep_last:", "min_age:", "max_age:", "AUTOBACKUP.KEEP-LAST"} {
		if strings.Contains(string(saved), legacy) {
			t.Fatalf("saved configuration still contains %q:\n%s", legacy, saved)
		}
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
	if err := os.WriteFile(path, []byte("version: 1\nprojects:\n  - name: prod\n    keep_max: 7\n"), 0o600); err != nil {
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
		"reference":       {Version: 1, Defaults: Default().Defaults, Projects: []Project{{Name: "prod", Include: []string{"server"}}}},
		"negative max":    {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepMax = -1; return d }()},
		"latest over max": {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepLatest = d.KeepMax + 1; return d }()},
		"too many slots":  {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepMax = 4; return d }()},
		"target order":    {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepTargetsRaw = []string{"2w", "1w"}; return d }()},
		"invalid target":  {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepTargetsRaw = []string{"tomorrow"}; return d }()},
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
