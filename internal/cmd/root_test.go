package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/mlahr/snapzner/internal/snapzner"
)

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
