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
		{[]string{"snapshots", "pin"}, "id"},
		{[]string{"snapshots", "unpin"}, "id"},
		{[]string{"snapshots", "delete"}, "force"},
		{[]string{"backup"}, "server"},
		{[]string{"backup"}, "force"},
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

func TestBackupForceRequiresProjectScopedServer(t *testing.T) {
	for _, test := range []struct {
		args     []string
		contains string
	}{
		{args: []string{"backup", "--force"}, contains: "requires at least one --server"},
		{args: []string{"backup", "--force", "--server", "108959890"}, contains: "requires --project or a PROJECT/SERVER target"},
	} {
		a := &app{out: io.Discard, errOut: io.Discard}
		root := a.rootCommand()
		root.SetArgs(test.args)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), test.contains) {
			t.Fatalf("snapzner %s error = %v, want containing %q", strings.Join(test.args, " "), err, test.contains)
		}
	}
}

func TestSnapshotPinCommandsValidateProjectAndIDs(t *testing.T) {
	for _, test := range []struct {
		args     []string
		contains string
	}{
		{args: []string{"snapshots", "pin", "--id", "99"}, contains: "requires exactly one --project"},
		{args: []string{"snapshots", "unpin", "--project", "prod"}, contains: "at least one --id is required"},
	} {
		a := &app{out: io.Discard, errOut: io.Discard}
		root := a.rootCommand()
		root.SetArgs(test.args)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), test.contains) {
			t.Fatalf("snapzner %s error = %v, want containing %q", strings.Join(test.args, " "), err, test.contains)
		}
	}
}

func TestPrintEventsAlignsSnapshotListColumns(t *testing.T) {
	var output bytes.Buffer
	a := &app{out: &output, errOut: io.Discard}
	a.printEvents([]snapzner.Event{
		{
			Project: "pdfdancer", Operation: "list", ResourceID: 408358487,
			Message: "ignored", DisplayColumns: []string{"pdfdancer-api-production-1784019793", "managed=true", "pinned=false", "source=pdfdancer-api-production", "created=2026-07-14T09:03:13Z"},
		},
		{
			Project: "root", Operation: "list", ResourceID: 408358488,
			Message: "ignored", DisplayColumns: []string{"root-v2-1784019793", "managed=true", "pinned=false", "source=root-v2", "created=2026-07-14T09:03:13Z"},
		},
	})
	want := "[pdfdancer] pdfdancer-api-production-1784019793 | managed=true | pinned=false | source=pdfdancer-api-production | created=2026-07-14T09:03:13Z (id=408358487)\n" +
		"[root]      root-v2-1784019793                  | managed=true | pinned=false | source=root-v2                  | created=2026-07-14T09:03:13Z (id=408358488)\n"
	if output.String() != want {
		t.Fatalf("snapshot list output = %q, want %q", output.String(), want)
	}
}

func TestPrintEventsAlignsGeneralResultRows(t *testing.T) {
	var output bytes.Buffer
	a := &app{out: &output, errOut: io.Discard}
	a.printEvents([]snapzner.Event{
		{Project: "production", Operation: "delete", ResourceID: 9, Message: "snapshot deleted"},
		{Project: "dev", Operation: "delete", ResourceID: 123, Message: "snapshot deletion failed later"},
	})
	want := "[production] snapshot deleted               (id=9)\n" +
		"[dev]        snapshot deletion failed later (id=123)\n"
	if output.String() != want {
		t.Fatalf("result output = %q, want %q", output.String(), want)
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
