package snapzner

import (
	"context"
	"encoding/json"
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

func TestPruneCandidatesKeepsNewestPrefix(t *testing.T) {
	images := []*hcloud.Image{{ID: 3}, {ID: 2}, {ID: 1}}
	got := PruneCandidates(images, 2)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("unexpected candidates: %#v", got)
	}
	if got := PruneCandidates(images, 3); len(got) != 0 {
		t.Fatalf("expected no candidates: %#v", got)
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
	svc := Service{Project: "prod", Cloud: &Cloud{Client: client}, Policy: config.Policy{LabelSelector: "AUTOBACKUP=true", RetentionLabel: "AUTOBACKUP.KEEP-LAST", KeepLast: 3, SnapshotName: "%name%-%timestamp%"}, Timeout: time.Second, ServerConcurrency: 1, OnProgress: func(update Progress) {
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
