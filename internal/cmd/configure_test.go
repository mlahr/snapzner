package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/mlahr/snapzner/internal/config"
	"github.com/mlahr/snapzner/internal/credentials"
)

type scriptedPrompt struct {
	lines    []string
	raw      []string
	secrets  []string
	confirms []bool
}

func (p *scriptedPrompt) Line(_ string, current string) (string, error) {
	if len(p.lines) == 0 {
		return "", io.EOF
	}
	value := p.lines[0]
	p.lines = p.lines[1:]
	if value == "" {
		return current, nil
	}
	return value, nil
}
func (p *scriptedPrompt) Raw(_ string) (string, error) {
	if len(p.raw) == 0 {
		return "", io.EOF
	}
	value := p.raw[0]
	p.raw = p.raw[1:]
	return value, nil
}
func (p *scriptedPrompt) Secret(_ string) (string, error) {
	if len(p.secrets) == 0 {
		return "", io.EOF
	}
	value := p.secrets[0]
	p.secrets = p.secrets[1:]
	return value, nil
}
func (p *scriptedPrompt) Confirm(_ string, _ bool) (bool, error) {
	if len(p.confirms) == 0 {
		return false, io.EOF
	}
	value := p.confirms[0]
	p.confirms = p.confirms[1:]
	return value, nil
}

type fakeProjectAPI struct {
	validateErr error
	servers     []*hcloud.Server
	matched     []*hcloud.Server
}

func (f *fakeProjectAPI) Validate(context.Context) error                       { return f.validateErr }
func (f *fakeProjectAPI) AllServers(context.Context) ([]*hcloud.Server, error) { return f.servers, nil }
func (f *fakeProjectAPI) SelectorServers(context.Context, string) ([]*hcloud.Server, error) {
	return f.matched, nil
}

func TestConfigureFirstRunAndRerun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	one := &hcloud.Server{ID: 1, Name: "db"}
	two := &hcloud.Server{ID: 2, Name: "test"}
	api := &fakeProjectAPI{servers: []*hcloud.Server{one, two}, matched: []*hcloud.Server{one}}
	var output bytes.Buffer
	first := &configureWizard{
		ctx: context.Background(), prompt: &scriptedPrompt{
			lines: []string{"role=backup", "BACKUP.KEEP", "5", "%project%-%name%", "30m", "2", "3"},
			raw:   []string{"prod"}, secrets: []string{"super-secret-token"}, confirms: []bool{false, true},
		}, out: &output, factory: func(string) projectAPI { return api }, configPath: path, version: "test",
		picker: pickerWithChanges(map[int64]bool{2: true}),
	}
	if err := first.run(&output); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.LabelSelector != "role=backup" || cfg.Defaults.KeepLast != 5 {
		t.Fatalf("unexpected defaults: %+v", cfg.Defaults)
	}
	if got := cfg.Projects[0].Include; len(got) != 1 || got[0] != "id:2" {
		t.Fatalf("includes = %#v", got)
	}
	store, err := credentials.Load(credentials.PathForConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	if store.Tokens["prod"] != "super-secret-token" {
		t.Fatal("credential was not saved")
	}
	if strings.Contains(output.String(), "super-secret-token") {
		t.Fatal("token leaked to output")
	}

	output.Reset()
	second := &configureWizard{
		ctx: context.Background(), prompt: &scriptedPrompt{
			lines:    []string{"", "", "", "", "", "", ""},
			confirms: []bool{true, false, false, true},
		}, out: &output, factory: func(token string) projectAPI {
			if token != "super-secret-token" {
				t.Fatalf("unexpected token passed to API: %q", token)
			}
			return api
		}, configPath: path, version: "test", picker: pickerWithChanges(map[int64]bool{1: false}),
	}
	if err := second.run(&output); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Projects[0].Include; len(got) != 1 || got[0] != "id:2" {
		t.Fatalf("includes after rerun = %#v", got)
	}
	if got := cfg.Projects[0].Exclude; len(got) != 1 || got[0] != "id:1" {
		t.Fatalf("excludes after rerun = %#v", got)
	}
	if strings.Contains(output.String(), "super-secret-token") {
		t.Fatal("stored token leaked to output")
	}
}

func TestConfigureRetriesInvalidNewToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	goodAPI := &fakeProjectAPI{}
	var output bytes.Buffer
	w := &configureWizard{
		ctx: context.Background(), prompt: &scriptedPrompt{
			lines: []string{"", "", "", "", "", "", ""}, raw: []string{"prod"},
			secrets: []string{"bad-token", "good-token"}, confirms: []bool{false, true},
		}, out: &output, configPath: path,
		factory: func(token string) projectAPI {
			if token == "bad-token" {
				return &fakeProjectAPI{validateErr: errors.New("unauthorized")}
			}
			return goodAPI
		},
	}
	if err := w.run(&output); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Load(credentials.PathForConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	if store.Tokens["prod"] != "good-token" {
		t.Fatalf("stored token = %q", store.Tokens["prod"])
	}
	if strings.Contains(output.String(), "bad-token") || strings.Contains(output.String(), "good-token") {
		t.Fatal("token leaked to output")
	}
}

func TestConfigureRemovesProjectAndCredentialBeforeAddingReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Projects = []config.Project{{Name: "old"}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Save(credentials.PathForConfig(path), credentials.Store{Version: 1, Tokens: map[string]string{"old": "old-token"}}); err != nil {
		t.Fatal(err)
	}
	api := &fakeProjectAPI{}
	w := &configureWizard{
		ctx: context.Background(), prompt: &scriptedPrompt{
			lines: []string{"", "", "", "", "", "", ""}, raw: []string{"new"}, secrets: []string{"new-token"}, confirms: []bool{false, false, true},
		}, out: io.Discard, factory: func(string) projectAPI { return api }, configPath: path,
	}
	if err := w.run(io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Name != "new" {
		t.Fatalf("projects = %#v", cfg.Projects)
	}
	store, err := credentials.Load(credentials.PathForConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Tokens["old"]; ok {
		t.Fatal("removed project credential was retained")
	}
	if store.Tokens["new"] != "new-token" {
		t.Fatal("new project credential missing")
	}
}

func TestConfigureCancellationDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	api := &fakeProjectAPI{}
	w := &configureWizard{
		ctx: context.Background(), prompt: &scriptedPrompt{
			lines: []string{"", "", "", "", "", "", ""}, raw: []string{"prod"}, secrets: []string{"token"}, confirms: []bool{false, false},
		}, out: io.Discard, factory: func(string) projectAPI { return api }, configPath: path,
	}
	if err := w.run(io.Discard); err == nil {
		t.Fatal("expected cancellation error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("configuration should not exist: %v", err)
	}
	if _, err := credentials.Load(credentials.PathForConfig(path)); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedOverrides(t *testing.T) {
	servers := []*hcloud.Server{{ID: 30, Name: "c"}, {ID: 10, Name: "a"}, {ID: 20, Name: "b"}}
	include, exclude := deriveOverrides(servers, map[int64]bool{10: true, 30: true}, map[int64]bool{10: false, 20: true, 30: true})
	if strings.Join(include, ",") != "id:20" {
		t.Fatalf("include = %#v", include)
	}
	if strings.Join(exclude, ",") != "id:10" {
		t.Fatalf("exclude = %#v", exclude)
	}
}

func pickerWithChanges(changes map[int64]bool) func(context.Context, io.Writer, string, pickerState) (map[int64]bool, error) {
	return func(_ context.Context, _ io.Writer, _ string, state pickerState) (map[int64]bool, error) {
		selected := cloneSelection(state.selected)
		for id, value := range changes {
			selected[id] = value
		}
		return selected, nil
	}
}
