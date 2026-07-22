package snapzner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	"github.com/mlahr/snapzner/internal/config"
)

func TestRenderNameUsesUTCFields(t *testing.T) {
	server := &hcloud.Server{ID: 42, Name: "db"}
	now := time.Date(2026, 7, 14, 2, 3, 4, 0, time.UTC)
	got := RenderName("%project%-%id%-%name%-%timestamp%-%date%-%time%", "prod", server, now)
	want := "prod-42-db-1783994584-2026-07-14-02:03:04"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSnapshotSummaryMatchesListAndPruneFormat(t *testing.T) {
	created := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	image := &hcloud.Image{
		Description: "root-1784030400", Created: created,
		Labels: map[string]string{metadataPrefix + "managed": "v1", metadataPrefix + "source-name": "root"},
	}
	want := "root-1784030400 | managed=true | pinned=false | source=root | created=2026-07-14T12:00:00Z"
	if got := SnapshotSummary(image); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestSelectBackupServersStrictlyIntersectsConfiguredSelection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		var servers []schema.Server
		switch {
		case r.URL.Query().Get("label_selector") != "":
			servers = []schema.Server{{ID: 1, Name: "database"}, {ID: 2, Name: "web"}}
		case r.URL.Query().Get("name") == "database":
			servers = []schema.Server{{ID: 1, Name: "database"}}
		case r.URL.Query().Get("name") == "web":
			servers = []schema.Server{{ID: 2, Name: "web"}}
		}
		writeJSON(t, w, map[string]any{
			"servers": servers,
			"meta":    map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	})
	mux.HandleFunc("/servers/1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, schema.ServerGetResponse{Server: schema.Server{ID: 1, Name: "database"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{
		Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second,
		Policy: config.Policy{LabelSelector: "AUTOBACKUP=true"},
	}
	project := config.Project{Name: "prod", Exclude: []string{"name:web"}}
	selected, err := svc.SelectBackupServers(context.Background(), project, []string{"database", "1", "name:database", "id:1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != 1 {
		t.Fatalf("selected = %#v", selected)
	}
	if _, err := svc.SelectBackupServers(context.Background(), project, []string{"web"}); err == nil || !strings.Contains(err.Error(), "not selected by project configuration") {
		t.Fatalf("ineligible server error = %v", err)
	}
	forced, err := svc.ResolveBackupServers(context.Background(), []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(forced) != 1 || forced[0].ID != 2 {
		t.Fatalf("forced selection = %#v", forced)
	}
}

func TestPruneCandidatesKeepsLatestPrefix(t *testing.T) {
	images := []*hcloud.Image{{ID: 3}, {ID: 2}, {ID: 1}}
	policy := config.Policy{KeepMax: 2, KeepLatest: 2}
	got := PruneCandidates(images, policy, time.Now())
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("unexpected candidates: %#v", got)
	}
	policy.KeepMax = 3
	policy.KeepLatest = 3
	if got := PruneCandidates(images, policy, time.Now()); len(got) != 0 {
		t.Fatalf("expected no candidates: %#v", got)
	}
}

func TestPruneCandidatesKeepsLatestAndAgeTargetsWithinMaximum(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	image := func(id int64, age time.Duration) *hcloud.Image {
		return &hcloud.Image{ID: id, Created: now.Add(-age), Labels: map[string]string{}}
	}
	images := []*hcloud.Image{
		image(1, time.Hour),
		image(2, 2*time.Hour),
		image(3, 12*time.Hour),
		image(4, 25*time.Hour),
		image(5, 2*24*time.Hour),
		image(6, 8*24*time.Hour),
		image(7, 15*24*time.Hour),
		image(8, 16*24*time.Hour),
	}
	policy := config.Policy{KeepMax: 5, KeepLatest: 2, KeepTargets: []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour}}
	got := PruneCandidates(images, policy, now)
	want := []int64{3, 5, 8}
	if len(got) != len(want) {
		t.Fatalf("candidates = %#v", got)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("candidates = %#v", got)
		}
	}
}

func TestPruneCandidatesRetainsRequestedFivePointExample(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	var images []*hcloud.Image
	for i := 0; i < 30*5; i++ {
		images = append(images, &hcloud.Image{
			ID:      int64(i + 1),
			Created: now.Add(-time.Duration(i) * 24 * time.Hour / 5),
			Labels:  map[string]string{},
		})
	}
	policy := config.Policy{
		KeepMax: 5, KeepLatest: 2,
		KeepTargets: []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour},
	}
	candidates := PruneCandidates(images, policy, now)
	deleted := make(map[int64]bool, len(candidates))
	for _, image := range candidates {
		deleted[image.ID] = true
	}
	var retainedAges []time.Duration
	for _, image := range images {
		if !deleted[image.ID] {
			retainedAges = append(retainedAges, now.Sub(image.Created))
		}
	}
	want := []time.Duration{0, 24 * time.Hour / 5, 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour}
	if len(retainedAges) != len(want) {
		t.Fatalf("retained ages = %v", retainedAges)
	}
	for i := range want {
		if retainedAges[i] != want[i] {
			t.Fatalf("retained ages = %v, want %v", retainedAges, want)
		}
	}
}

func TestPruneCandidatesServerMaximumOverrideCapsTargets(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	images := []*hcloud.Image{
		{ID: 4, Created: now, Labels: map[string]string{metadataPrefix + "keep-max": "2"}},
		{ID: 3, Created: now.Add(-time.Hour), Labels: map[string]string{}},
		{ID: 2, Created: now.Add(-24 * time.Hour), Labels: map[string]string{}},
		{ID: 1, Created: now.Add(-7 * 24 * time.Hour), Labels: map[string]string{}},
	}
	policy := config.Policy{KeepMax: 4, KeepLatest: 1, KeepTargets: []time.Duration{24 * time.Hour, 7 * 24 * time.Hour}}
	got := PruneCandidates(images, policy, now)
	if len(got) != 2 || got[0].ID != 3 || got[1].ID != 1 {
		t.Fatalf("candidates = %#v", got)
	}
}

func TestPruneCandidatesLeavesUnfillableTargetEmpty(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	images := []*hcloud.Image{
		{ID: 2, Created: now, Labels: map[string]string{}},
		{ID: 1, Created: now.Add(-time.Hour), Labels: map[string]string{}},
	}
	policy := config.Policy{KeepMax: 2, KeepLatest: 1, KeepTargets: []time.Duration{24 * time.Hour}}
	got := PruneCandidates(images, policy, now)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("candidates = %#v", got)
	}
}

func TestPruneDryRunAndApplyUseSameAgeCandidates(t *testing.T) {
	now := time.Now().UTC()
	created := []time.Time{now.Add(-40 * 24 * time.Hour), now.Add(-41 * 24 * time.Hour), now.Add(-42 * 24 * time.Hour)}
	var deleted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/images", func(w http.ResponseWriter, _ *http.Request) {
		images := make([]schema.Image, 3)
		for i := range images {
			images[i] = schema.Image{
				ID: int64(3 - i), Status: "available", Type: "snapshot", Description: "database-backup", Created: &created[i],
				Labels: map[string]string{metadataPrefix + "managed": "v1", metadataPrefix + "source-id": "42", metadataPrefix + "source-name": "database"},
			}
		}
		writeJSON(t, w, map[string]any{"images": images, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
	})
	mux.HandleFunc("/images/2", func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/images/1", func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{
		Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second,
		Policy: config.Policy{KeepMax: 1, KeepLatest: 1},
	}
	dryRun := svc.Prune(context.Background(), false, false)
	if len(dryRun) != 2 || len(deleted) != 0 {
		t.Fatalf("dry run events = %#v, deleted = %#v", dryRun, deleted)
	}
	if !strings.Contains(dryRun[0].Message, "database-backup | managed=true | pinned=false | source=database | created=") {
		t.Fatalf("dry run message = %q", dryRun[0].Message)
	}
	applied := svc.Prune(context.Background(), true, false)
	if len(applied) != 2 || len(deleted) != 2 {
		t.Fatalf("applied events = %#v, deleted = %#v", applied, deleted)
	}
	for i := range dryRun {
		if dryRun[i].ResourceID != applied[i].ResourceID {
			t.Fatalf("candidate mismatch: dry run = %#v, applied = %#v", dryRun, applied)
		}
	}
}

func TestSelectorKey(t *testing.T) {
	if got := selectorKey("AUTOBACKUP=true"); got != "AUTOBACKUP" {
		t.Fatalf("got %q", got)
	}
}

func TestBackupCreatesManagedSnapshotBeforePruning(t *testing.T) {
	var created schema.ServerActionCreateImageRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("label_selector"); got != "AUTOBACKUP=true" {
			t.Errorf("selector = %q", got)
		}
		writeJSON(t, w, map[string]any{
			"servers": []schema.Server{{
				ID: 1, Name: "db", Status: "running", Labels: map[string]string{"AUTOBACKUP": "true"},
				ServerType: schema.ServerType{ID: 2, Name: "cx22", Architecture: "x86"},
				Location:   schema.Location{ID: 3, Name: "fsn1"},
				PublicNet:  schema.ServerPublicNet{IPv4: schema.ServerPublicNetIPv4{ID: 4, IP: "192.0.2.1"}},
			}},
			"meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	})
	mux.HandleFunc("/firewalls", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"firewalls": []schema.Firewall{}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
	})
	mux.HandleFunc("/servers/1/actions/create_image", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
			t.Error(err)
		}
		now := time.Now().UTC()
		writeJSON(t, w, schema.ServerActionCreateImageResponse{
			Action: schema.Action{ID: 10, Status: "success"},
			Image:  schema.Image{ID: 20, Status: "available", Type: "snapshot", Created: &now, Labels: derefLabels(created.Labels)},
		})
	})
	mux.HandleFunc("/images", func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now().UTC()
		writeJSON(t, w, map[string]any{
			"images": []schema.Image{{ID: 20, Status: "available", Type: "snapshot", Created: &now, Labels: derefLabels(created.Labels)}},
			"meta":   map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL), hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}))
	var progress []Progress
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Policy: config.Policy{LabelSelector: "AUTOBACKUP=true", RetentionLabel: "AUTOBACKUP.KEEP-MAX", KeepMax: 5, KeepLatest: 2, KeepTargets: []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour}, SnapshotName: "%name%-%timestamp%"}, Timeout: time.Second, ServerConcurrency: 1, OnProgress: func(update Progress) {
		progress = append(progress, update)
	}}
	events := svc.Backup(context.Background(), config.Project{Name: "prod"})
	for _, event := range events {
		if event.Error != "" {
			t.Fatalf("event failed: %+v", event)
		}
	}
	labels := derefLabels(created.Labels)
	if labels[metadataPrefix+"managed"] != "v1" {
		t.Fatalf("management label missing: %#v", labels)
	}
	if labels[metadataPrefix+"source-id"] != "1" {
		t.Fatalf("source metadata missing: %#v", labels)
	}
	if labels[metadataPrefix+"keep-max"] != "5" {
		t.Fatalf("retention metadata missing: %#v", labels)
	}
	if created.Type == nil || *created.Type != "snapshot" {
		t.Fatalf("create type = %#v", created.Type)
	}
	wantProgress := []string{"selecting servers", "selected 1 server", "creating snapshot", "snapshot available", "enforcing retention for 1 server", "backup finished: 1/1 snapshots created"}
	if len(progress) != len(wantProgress) {
		t.Fatalf("progress updates = %#v", progress)
	}
	for i, want := range wantProgress {
		if progress[i].Message != want {
			t.Fatalf("progress[%d].Message = %q, want %q", i, progress[i].Message, want)
		}
	}
	if progress[2].ServerID != 1 || progress[3].Completed != 1 || progress[3].Total != 1 {
		t.Fatalf("server progress = %#v, %#v", progress[2], progress[3])
	}
}

func TestFilteredBackupSnapshotsOnlyRequestedServer(t *testing.T) {
	var createdServerIDs []int64
	var created schema.ServerActionCreateImageRequest
	serverSchema := func(id int64, name string) schema.Server {
		return schema.Server{
			ID: id, Name: name, Status: "running", Labels: map[string]string{"AUTOBACKUP": "true"},
			ServerType: schema.ServerType{ID: 2, Name: "cx22", Architecture: "x86"},
			Location:   schema.Location{ID: 3, Name: "fsn1"},
			PublicNet:  schema.ServerPublicNet{IPv4: schema.ServerPublicNetIPv4{ID: 4, IP: "192.0.2.1"}},
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		servers := []schema.Server{}
		if r.URL.Query().Get("label_selector") != "" {
			servers = []schema.Server{serverSchema(1, "database"), serverSchema(2, "web")}
		} else if r.URL.Query().Get("name") == "database" {
			servers = []schema.Server{serverSchema(1, "database")}
		}
		writeJSON(t, w, map[string]any{
			"servers": servers,
			"meta":    map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	})
	mux.HandleFunc("/firewalls", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"firewalls": []schema.Firewall{}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
	})
	mux.HandleFunc("/servers/1/actions/create_image", func(w http.ResponseWriter, r *http.Request) {
		createdServerIDs = append(createdServerIDs, 1)
		if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
			t.Error(err)
		}
		now := time.Now().UTC()
		writeJSON(t, w, schema.ServerActionCreateImageResponse{
			Action: schema.Action{ID: 10, Status: "success"},
			Image:  schema.Image{ID: 20, Status: "available", Type: "snapshot", Created: &now, Labels: derefLabels(created.Labels)},
		})
	})
	mux.HandleFunc("/servers/2/actions/create_image", func(w http.ResponseWriter, _ *http.Request) {
		createdServerIDs = append(createdServerIDs, 2)
		http.Error(w, "unexpected server", http.StatusInternalServerError)
	})
	mux.HandleFunc("/images", func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now().UTC()
		writeJSON(t, w, map[string]any{
			"images": []schema.Image{{ID: 20, Status: "available", Type: "snapshot", Created: &now, Labels: derefLabels(created.Labels)}},
			"meta":   map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL), hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}))
	svc := Service{
		Project: "prod", Cloud: &Cloud{Client: client},
		Policy: config.Policy{
			LabelSelector: "AUTOBACKUP=true", RetentionLabel: "AUTOBACKUP.KEEP-MAX",
			KeepMax: 5, KeepLatest: 2, KeepTargets: []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour}, SnapshotName: "%name%-%timestamp%",
		},
		Timeout: time.Second, ServerConcurrency: 2,
	}
	servers, err := svc.SelectBackupServers(context.Background(), config.Project{Name: "prod"}, []string{"database"})
	if err != nil {
		t.Fatal(err)
	}
	events := svc.BackupServers(context.Background(), servers)
	for _, event := range events {
		if event.Error != "" {
			t.Fatalf("event failed: %+v", event)
		}
	}
	if len(createdServerIDs) != 1 || createdServerIDs[0] != 1 {
		t.Fatalf("created snapshots for servers %v", createdServerIDs)
	}
}

func TestDeleteSnapshotsForceOverridesOwnershipAndProtection(t *testing.T) {
	var protectionDisabled, deleted bool
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/images/99", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, schema.ImageGetResponse{Image: schema.Image{
				ID: 99, Status: "available", Type: "snapshot", Created: &now,
				Protection: schema.ImageProtection{Delete: true}, Labels: map[string]string{},
			}})
		case http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	mux.HandleFunc("/images/99/actions/change_protection", func(w http.ResponseWriter, r *http.Request) {
		var request schema.ImageActionChangeProtectionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		protectionDisabled = request.Delete != nil && !*request.Delete
		writeJSON(t, w, schema.ImageActionChangeProtectionResponse{Action: schema.Action{ID: 7, Status: "success"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second}
	events := svc.DeleteSnapshots(context.Background(), []int64{99}, true)
	if len(events) != 1 || events[0].Error != "" {
		t.Fatalf("events = %#v", events)
	}
	if !protectionDisabled || !deleted {
		t.Fatalf("protectionDisabled=%t deleted=%t", protectionDisabled, deleted)
	}
}

func TestDeleteSnapshotsWithoutForceRejectsUnmanagedSnapshot(t *testing.T) {
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/images/99", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation without force: %s", r.Method)
		}
		writeJSON(t, w, schema.ImageGetResponse{Image: schema.Image{
			ID: 99, Status: "available", Type: "snapshot", Created: &now,
			Protection: schema.ImageProtection{Delete: true}, Labels: map[string]string{},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second}
	events := svc.DeleteSnapshots(context.Background(), []int64{99}, false)
	if len(events) != 1 || !strings.Contains(events[0].Error, "--force") {
		t.Fatalf("events = %#v", events)
	}
}

func TestSetSnapshotPinsPreservesLabelsAndIsIdempotent(t *testing.T) {
	now := time.Now().UTC()
	labels := map[string]string{
		metadataPrefix + "managed": "v1",
		"owner":                    "database-team",
	}
	updates := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/images/99", func(w http.ResponseWriter, r *http.Request) {
		image := schema.Image{ID: 99, Status: "available", Type: "snapshot", Created: &now, Labels: labels}
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, schema.ImageGetResponse{Image: image})
		case http.MethodPut:
			var request schema.ImageUpdateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Labels == nil {
				t.Fatal("label update missing")
			}
			labels = *request.Labels
			updates++
			image.Labels = labels
			writeJSON(t, w, schema.ImageUpdateResponse{Image: image})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second}

	for _, pinned := range []bool{true, true, false, false} {
		events := svc.SetSnapshotPins(context.Background(), []int64{99}, pinned)
		if len(events) != 1 || events[0].Error != "" {
			t.Fatalf("pinned=%t events = %#v", pinned, events)
		}
		if labels["owner"] != "database-team" {
			t.Fatalf("unrelated labels changed: %#v", labels)
		}
		if (labels[pinnedMetadata] == "v1") != pinned {
			t.Fatalf("pinned=%t labels = %#v", pinned, labels)
		}
	}
	if updates != 2 {
		t.Fatalf("updates = %d, want 2", updates)
	}
}

func TestSetSnapshotPinsReportsPartialFailures(t *testing.T) {
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/images/77", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(schema.ErrorResponse{Error: schema.Error{Code: "not_found"}}); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("/images/88", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, schema.ImageGetResponse{Image: schema.Image{ID: 88, Type: "system", Created: &now}})
	})
	mux.HandleFunc("/images/99", func(w http.ResponseWriter, r *http.Request) {
		image := schema.Image{ID: 99, Type: "snapshot", Created: &now, Labels: map[string]string{pinnedMetadata: "v1"}}
		if r.Method == http.MethodGet {
			writeJSON(t, w, schema.ImageGetResponse{Image: image})
			return
		}
		t.Errorf("unexpected idempotent mutation: %s", r.Method)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second}
	events := svc.SetSnapshotPins(context.Background(), []int64{77, 88, 99}, true)
	if len(events) != 3 || events[0].Message != "snapshot not found" || events[0].Error == "" || events[1].Error == "" || events[2].Error != "" || events[2].Message != "snapshot already pinned" {
		t.Fatalf("events = %#v", events)
	}
}

func TestPruneAlwaysRetainsPinnedSnapshotsOutsideRetention(t *testing.T) {
	now := time.Now().UTC()
	created := []time.Time{now, now.Add(-time.Hour), now.Add(-2 * time.Hour)}
	var deleted []int64
	mux := http.NewServeMux()
	mux.HandleFunc("/images", func(w http.ResponseWriter, _ *http.Request) {
		images := []schema.Image{
			{ID: 3, Status: "available", Type: "snapshot", Created: &created[0], Labels: map[string]string{metadataPrefix + "managed": "v1", metadataPrefix + "source-id": "42", pinnedMetadata: "v1"}},
			{ID: 2, Status: "available", Type: "snapshot", Created: &created[1], Labels: map[string]string{metadataPrefix + "managed": "v1", metadataPrefix + "source-id": "42"}},
			{ID: 1, Status: "available", Type: "snapshot", Created: &created[2], Labels: map[string]string{metadataPrefix + "managed": "v1", metadataPrefix + "source-id": "42"}},
		}
		writeJSON(t, w, map[string]any{"images": images, "meta": map[string]any{"pagination": map[string]any{"page": 1, "last_page": 1}}})
	})
	for _, id := range []int64{1, 2, 3} {
		id := id
		mux.HandleFunc(fmt.Sprintf("/images/%d", id), func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("unexpected method for image %d: %s", id, r.Method)
			}
			deleted = append(deleted, id)
			w.WriteHeader(http.StatusNoContent)
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{
		Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second,
		Policy: config.Policy{KeepMax: 1, KeepLatest: 1},
	}
	events := svc.Prune(context.Background(), true, true)
	if len(events) != 2 || events[0].ResourceID != 3 || !strings.Contains(events[0].Message, "retained pinned") || events[1].ResourceID != 1 {
		t.Fatalf("events = %#v", events)
	}
	if len(deleted) != 1 || deleted[0] != 1 {
		t.Fatalf("deleted = %v, want [1]", deleted)
	}
}

func TestDeleteSnapshotsForceRejectsPinnedSnapshot(t *testing.T) {
	now := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/images/99", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation of pinned snapshot: %s", r.Method)
		}
		writeJSON(t, w, schema.ImageGetResponse{Image: schema.Image{
			ID: 99, Status: "available", Type: "snapshot", Created: &now,
			Protection: schema.ImageProtection{Delete: true}, Labels: map[string]string{pinnedMetadata: "v1"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := hcloud.NewClient(hcloud.WithToken("test"), hcloud.WithEndpoint(server.URL))
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Timeout: time.Second}
	events := svc.DeleteSnapshots(context.Background(), []int64{99}, true)
	if len(events) != 1 || !strings.Contains(events[0].Error, "unpin it before deletion") {
		t.Fatalf("events = %#v", events)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func derefLabels(labels *map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return *labels
}
