package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mlahr/snapzner/internal/snapzner"
)

func TestLoadConfigWarnsOnceForLegacyRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ndefaults:\n  keep_last: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	a := &app{configPath: path, out: io.Discard, errOut: &warnings}
	if _, err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	got := warnings.String()
	if strings.Count(got, "legacy retention fields") != 1 || !strings.Contains(got, "run 'snapzner configure' to migrate") {
		t.Fatalf("warning = %q", got)
	}
}

func TestLoadConfigSuppressesLegacyWarningWhenQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ndefaults:\n  keep_last: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	a := &app{configPath: path, quiet: true, out: io.Discard, errOut: &warnings}
	if _, err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if warnings.Len() != 0 {
		t.Fatalf("quiet warning = %q", warnings.String())
	}
}

func TestBackupProgressReporterWritesToErrorOutput(t *testing.T) {
	var output bytes.Buffer
	reporter := newBackupProgressRenderer(&output, false)
	t.Cleanup(reporter.Close)
	reporter.Report(snapzner.Progress{Project: "prod", Message: "snapshot available", ServerID: 42, ServerName: "database", Completed: 1, Total: 3})
	want := "[prod] database (server 42) [1/3] snapshot available\n"
	if output.String() != want {
		t.Fatalf("progress output = %q, want %q", output.String(), want)
	}
}

func TestSnapshotAndProtectionFlags(t *testing.T) {
	a := &app{out: io.Discard, errOut: io.Discard}
	root := a.rootCommand()
	tests := []struct {
		path []string
		flag string
	}{
		{[]string{"snapshots", "list"}, "all"},
		{[]string{"snapshots", "delete"}, "force"},
		{[]string{"backup"}, "server"},
		{[]string{"prune"}, "force"},
		{[]string{"replay", "rebuild"}, "force"},
	}
	for _, test := range tests {
		command, _, err := root.Find(test.path)
		if err != nil {
			t.Fatalf("find %v: %v", test.path, err)
		}
		if command.Flags().Lookup(test.flag) == nil {
			t.Errorf("%s has no --%s flag", strings.Join(test.path, " "), test.flag)
		}
	}
	deleteCommand, _, err := root.Find([]string{"snapshots", "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if deleteCommand.Flags().Lookup("force-unmanaged") != nil {
		t.Fatal("snapshots delete still exposes removed --force-unmanaged flag")
	}
}

func TestBackupProgressReporterHonorsQuiet(t *testing.T) {
	var output bytes.Buffer
	reporter := newBackupProgressRenderer(&output, true)
	t.Cleanup(reporter.Close)
	reporter.Report(snapzner.Progress{Project: "prod", Message: "selecting servers"})
	if strings.TrimSpace(output.String()) != "" {
		t.Fatalf("quiet progress output = %q", output.String())
	}
}
