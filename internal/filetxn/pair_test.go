package filetxn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWritePairReplacesBothFilesPrivately(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "credentials.yaml")
	second := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(first, []byte("old credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("old config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WritePair(File{first, []byte("new credentials"), 0o600}, File{second, []byte("new config"), 0o600}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{first: "new credentials", second: "new config"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q", path, got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
}

func TestWritePairRollsBackWhenSecondCommitFails(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "credentials.yaml")
	second := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(first, []byte("old credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("old config"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalRename := renameFile
	calls := 0
	renameFile = func(old, new string) error {
		calls++
		if calls == 4 {
			return errors.New("simulated second commit failure")
		}
		return os.Rename(old, new)
	}
	defer func() { renameFile = originalRename }()
	if err := WritePair(File{first, []byte("new credentials"), 0o600}, File{second, []byte("new config"), 0o600}); err == nil {
		t.Fatal("expected error")
	}
	for path, want := range map[string]string{first: "old credentials", second: "old config"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestWritePairRejectsDifferentDirectoriesWithoutChanges(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "one", "credentials")
	second := filepath.Join(dir, "two", "config")
	if err := WritePair(File{first, []byte("new"), 0o600}, File{second, []byte("new"), 0o600}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first path unexpectedly changed: %v", err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("second path unexpectedly changed: %v", err)
	}
}
