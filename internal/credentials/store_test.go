package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapzner", "credentials.yaml")
	want := Store{Version: 1, Tokens: map[string]string{"prod": "secret-token"}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens["prod"] != "secret-token" {
		t.Fatal("token did not round-trip")
	}
}
