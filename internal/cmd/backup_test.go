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
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestParseDiscoveredServerIDs(t *testing.T) {
	ids, discover, err := parseDiscoveredServerIDs([]string{"42", "id:7", "42"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !discover || !reflect.DeepEqual(ids, []int64{42, 7}) {
		t.Fatalf("ids = %v, discover = %t", ids, discover)
	}

	for _, test := range []struct {
		values   []string
		projects []string
	}{
		{values: []string{"database"}},
		{values: []string{"42"}, projects: []string{"prod"}},
		{values: []string{"prod/42"}},
	} {
		if ids, discover, err := parseDiscoveredServerIDs(test.values, test.projects); err != nil || discover || ids != nil {
			t.Fatalf("parseDiscoveredServerIDs(%v, %v) = %v, %t, %v; want no discovery", test.values, test.projects, ids, discover, err)
		}
	}

	if _, _, err := parseDiscoveredServerIDs([]string{"0"}, nil); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("zero ID error = %v", err)
	}
	if _, _, err := parseDiscoveredServerIDs([]string{"id:not-a-number"}, nil); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("invalid explicit ID error = %v", err)
	}
}

func TestFilteredBackupValidatesEveryProjectBeforeMutation(t *testing.T) {
	var mutations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/create_image") {
			mutations.Add(1)
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/firewalls" {
			writeBackupAPIJSON(t, w, map[string]any{
				"firewalls": []schema.Firewall{},
				"meta":      map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
			})
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
		map[string][]string{"prod": {"database"}, "stage": {"blocked"}}, false, nil,
	)
	if err == nil || !strings.Contains(errors.String(), "not selected by project configuration") {
		t.Fatalf("error = %v, stderr = %q", err, errors.String())
	}
	if got := mutations.Load(); got != 0 {
		t.Fatalf("created %d snapshots before preflight completed", got)
	}

	errors.Reset()
	err = a.runFilteredBackup(
		context.Background(), []string{"prod", "stage"},
		map[string][]string{"prod": {"database"}, "stage": {"blocked"}}, true, nil,
	)
	if err == nil || strings.Contains(errors.String(), "not selected by project configuration") {
		t.Fatalf("forced error = %v, stderr = %q", err, errors.String())
	}
	if got := mutations.Load(); got != 2 {
		t.Fatalf("forced backup attempted %d snapshots, want 2", got)
	}
}

func TestDiscoveredIDBackupSpansProjectsAndSkipsUnmanaged(t *testing.T) {
	var mu sync.Mutex
	var created []int64
	createdLabels := map[string]map[string]string{}
	serverSchema := func(id int64, name, ip string) schema.Server {
		return schema.Server{
			ID: id, Name: name, Status: "running", Labels: map[string]string{"AUTOBACKUP": "true"},
			ServerType: schema.ServerType{ID: 2, Name: "cx22", Architecture: "x86"},
			Location:   schema.Location{ID: 3, Name: "fsn1"},
			PublicNet:  schema.ServerPublicNet{IPv4: schema.ServerPublicNetIPv4{ID: 4, IP: ip}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.URL.Path == "/servers":
			var servers []schema.Server
			if r.URL.Query().Get("label_selector") != "" {
				switch token {
				case "prod-token":
					servers = []schema.Server{serverSchema(1, "database", "192.0.2.1")}
				case "stage-token":
					servers = []schema.Server{serverSchema(2, "web", "192.0.2.2")}
				}
			}
			writeBackupAPIJSON(t, w, map[string]any{"servers": servers, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
		case r.URL.Path == "/firewalls":
			writeBackupAPIJSON(t, w, map[string]any{"firewalls": []schema.Firewall{}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
		case strings.HasSuffix(r.URL.Path, "/actions/create_image"):
			id := int64(1)
			if strings.Contains(r.URL.Path, "/2/") {
				id = 2
			}
			var request schema.ServerActionCreateImageRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			labels := dereferenceBackupLabels(request.Labels)
			mu.Lock()
			created = append(created, id)
			createdLabels[token] = labels
			mu.Unlock()
			now := time.Now().UTC()
			writeBackupAPIJSON(t, w, schema.ServerActionCreateImageResponse{
				Action: schema.Action{ID: id + 10, Status: "success"},
				Image:  schema.Image{ID: id + 20, Status: "available", Type: "snapshot", Created: &now, Labels: labels},
			})
		case r.URL.Path == "/images":
			mu.Lock()
			labels := createdLabels[token]
			mu.Unlock()
			images := []schema.Image{}
			if labels != nil {
				now := time.Now().UTC()
				images = append(images, schema.Image{ID: 21, Status: "available", Type: "snapshot", Created: &now, Labels: labels})
			}
			writeBackupAPIJSON(t, w, map[string]any{"images": images, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("SNAPZNER_API_ENDPOINT", server.URL)

	path := discoveredBackupConfig(t)
	var output, errors bytes.Buffer
	a := &app{configPath: path, version: "test", out: &output, errOut: &errors}
	if err := a.runDiscoveredIDBackup(context.Background(), []int64{1, 2, 999}, nil); err != nil {
		t.Fatalf("backup failed: %v; stderr = %q", err, errors.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 || !containsInt64(created, 1) || !containsInt64(created, 2) {
		t.Fatalf("created snapshots for %v", created)
	}
	if !strings.Contains(output.String(), "server is not selected by any configured project; skipped (id=999)") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestDiscoveredIDBackupFailsPreflightBeforeMutation(t *testing.T) {
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
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "stage-token" {
			http.Error(w, "project unavailable", http.StatusInternalServerError)
			return
		}
		writeBackupAPIJSON(t, w, map[string]any{
			"servers": []schema.Server{{ID: 1, Name: "database"}},
			"meta":    map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	}))
	defer server.Close()
	t.Setenv("SNAPZNER_API_ENDPOINT", server.URL)

	path := discoveredBackupConfig(t)
	var output, errors bytes.Buffer
	a := &app{configPath: path, version: "test", out: &output, errOut: &errors}
	err := a.runDiscoveredIDBackup(context.Background(), []int64{1}, nil)
	if err == nil || !strings.Contains(errors.String(), "managed server discovery failed") {
		t.Fatalf("error = %v; stderr = %q", err, errors.String())
	}
	if mutations != 0 {
		t.Fatalf("created %d snapshots before preflight completed", mutations)
	}
}

func TestDiscoveredIDBackupRejectsAmbiguousManagedID(t *testing.T) {
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
		writeBackupAPIJSON(t, w, map[string]any{
			"servers": []schema.Server{{ID: 1, Name: "duplicate"}},
			"meta":    map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	}))
	defer server.Close()
	t.Setenv("SNAPZNER_API_ENDPOINT", server.URL)

	path := discoveredBackupConfig(t)
	var output, errors bytes.Buffer
	a := &app{configPath: path, version: "test", out: &output, errOut: &errors}
	err := a.runDiscoveredIDBackup(context.Background(), []int64{1}, nil)
	if err == nil || !strings.Contains(errors.String(), "matched multiple configured projects") {
		t.Fatalf("error = %v; stderr = %q", err, errors.String())
	}
	if mutations != 0 {
		t.Fatalf("created %d snapshots after ambiguous discovery", mutations)
	}
}

func TestDiscoveredIDBackupSucceedsWhenEveryIDIsUnmanaged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/servers" {
			t.Errorf("unexpected mutating request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		writeBackupAPIJSON(t, w, map[string]any{
			"servers": []schema.Server{},
			"meta":    map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	}))
	defer server.Close()
	t.Setenv("SNAPZNER_API_ENDPOINT", server.URL)

	path := discoveredBackupConfig(t)
	var output, errors bytes.Buffer
	a := &app{configPath: path, version: "test", out: &output, errOut: &errors}
	if err := a.runDiscoveredIDBackup(context.Background(), []int64{999}, nil); err != nil {
		t.Fatalf("backup failed: %v; stderr = %q", err, errors.String())
	}
	if !strings.Contains(output.String(), "skipped (id=999)") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestDiscoveredIDBackupFailsOnMissingProjectCredential(t *testing.T) {
	path := discoveredBackupConfig(t)
	if err := credentials.Save(credentials.PathForConfig(path), credentials.Store{
		Version: 1, Tokens: map[string]string{"prod": "prod-token"},
	}); err != nil {
		t.Fatal(err)
	}
	var output, errors bytes.Buffer
	a := &app{configPath: path, version: "test", out: &output, errOut: &errors}
	err := a.runDiscoveredIDBackup(context.Background(), []int64{1}, nil)
	if err == nil || !strings.Contains(errors.String(), "credential unavailable") {
		t.Fatalf("error = %v; stderr = %q", err, errors.String())
	}
}

func discoveredBackupConfig(t *testing.T) string {
	t.Helper()
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
	return path
}

func dereferenceBackupLabels(labels *map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	return *labels
}

func containsInt64(values []int64, value int64) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func writeBackupAPIJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
