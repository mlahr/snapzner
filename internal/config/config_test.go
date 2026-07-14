package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndProjectOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("version: 1\ndefaults:\n  operation_timeout: 30m\nprojects:\n  - name: prod\n    keep_last: 7\n    include: [id:42]\n")
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
	policy := cfg.PolicyFor(cfg.Projects[0])
	if policy.KeepLast != 7 {
		t.Fatalf("keep_last = %d", policy.KeepLast)
	}
}

func TestValidationRejectsUnsafeReferencesAndRetention(t *testing.T) {
	for name, cfg := range map[string]Config{
		"reference": {Version: 1, Defaults: Default().Defaults, Projects: []Project{{Name: "prod", Include: []string{"server"}}}},
		"retention": {Version: 1, Defaults: func() Defaults { d := Default().Defaults; d.KeepLast = -1; return d }()},
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
