package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	"github.com/mlahr/snapzner/internal/config"
	"github.com/mlahr/snapzner/internal/credentials"
)

func TestParseBackupTargets(t *testing.T) {
	tests := []struct {
		name         string
		values       []string
		projects     []string
		wantTargets  map[string][]string
		wantProjects []string
	}{
		{name: "unfiltered"},
		{
			name: "single project shorthand", values: []string{"database", "42", "database"}, projects: []string{"prod"},
			wantTargets: map[string][]string{"prod": {"database", "42"}}, wantProjects: []string{"prod"},
		},
		{
			name: "qualified projects", values: []string{"prod/database", "stage/name:web", "stage/id:42"},
			wantTargets: map[string][]string{"prod": {"database"}, "stage": {"name:web", "id:42"}}, wantProjects: []string{"prod", "stage"},
		},
		{
			name: "qualified with matching flags", values: []string{"stage/web", "prod/db"}, projects: []string{"prod", "stage", "prod"},
			wantTargets: map[string][]string{"prod": {"db"}, "stage": {"web"}}, wantProjects: []string{"prod", "stage"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotTargets, gotProjects, err := parseBackupTargets(test.values, test.projects)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotTargets, test.wantTargets) || !reflect.DeepEqual(gotProjects, test.wantProjects) {
				t.Fatalf("targets = %#v, projects = %#v; want %#v, %#v", gotTargets, gotProjects, test.wantTargets, test.wantProjects)
			}
		})
	}
}

func TestParseBackupTargetsRejectsAmbiguousOrMismatchedValues(t *testing.T) {
	tests := []struct {
		values   []string
		projects []string
		contains string
	}{
		{values: []string{"db"}, contains: "exactly one --project"},
		{values: []string{"db"}, projects: []string{"prod", "stage"}, contains: "exactly one --project"},
		{values: []string{"/db"}, contains: "PROJECT/SERVER"},
		{values: []string{"prod/"}, contains: "PROJECT/SERVER"},
		{values: []string{"prod/db/extra"}, contains: "PROJECT/SERVER"},
		{values: []string{"stage/db"}, projects: []string{"prod"}, contains: "not selected"},
		{values: []string{"prod/db"}, projects: []string{"prod", "stage"}, contains: "has no --server target"},
		{values: []string{""}, projects: []string{"prod"}, contains: "cannot be empty"},
	}
	for _, test := range tests {
		_, _, err := parseBackupTargets(test.values, test.projects)
		if err == nil || !strings.Contains(err.Error(), test.contains) {
			t.Errorf("parseBackupTargets(%q, %q) error = %v; want containing %q", test.values, test.projects, err, test.contains)
		}
	}
}

func TestFilteredBackupValidatesEveryProjectBeforeMutation(t *testing.T) {
	var mutations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/create_image") {
			mutations++
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/servers" {
			http.NotFound(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var servers []schema.Server
		switch {
		case r.URL.Query().Get("label_selector") != "" && token == "prod-token":
			servers = []schema.Server{{ID: 1, Name: "database"}}
		case r.URL.Query().Get("name") == "database" && token == "prod-token":
			servers = []schema.Server{{ID: 1, Name: "database"}}
		case r.URL.Query().Get("label_selector") != "" && token == "stage-token":
			servers = []schema.Server{{ID: 2, Name: "web"}}
		case r.URL.Query().Get("name") == "blocked" && token == "stage-token":
			servers = []schema.Server{{ID: 3, Name: "blocked"}}
		}
		writeBackupAPIJSON(t, w, map[string]any{
			"servers": servers,
			"meta":    map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	}))
	defer server.Close()
	t.Setenv("SNAPZNER_API_ENDPOINT", server.URL)

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.Default()
	cfg.Projects = []config.Project{{Name: "prod"}, {Name: "stage"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Save(credentials.PathForConfig(path), credentials.Store{
		Version: 1, Tokens: map[string]string{"prod": "prod-token", "stage": "stage-token"},
	}); err != nil {
		t.Fatal(err)
	}
	var output, errors bytes.Buffer
	a := &app{configPath: path, version: "test", out: &output, errOut: &errors}
	err := a.runFilteredBackup(
		context.Background(), []string{"prod", "stage"},
		map[string][]string{"prod": {"database"}, "stage": {"blocked"}}, nil,
	)
	if err == nil || !strings.Contains(errors.String(), "not selected by project configuration") {
		t.Fatalf("error = %v, stderr = %q", err, errors.String())
	}
	if mutations != 0 {
		t.Fatalf("created %d snapshots before preflight completed", mutations)
	}
}

func writeBackupAPIJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
