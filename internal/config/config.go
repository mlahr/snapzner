package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultSelector       = "AUTOBACKUP=true"
	DefaultRetentionLabel = "AUTOBACKUP.KEEP-LAST"
	DefaultSnapshotName   = "%name%-%timestamp%"
)

type Config struct {
	Version  int       `yaml:"version"`
	Defaults Defaults  `yaml:"defaults"`
	Projects []Project `yaml:"projects"`
}

type Defaults struct {
	LabelSelector       string        `yaml:"label_selector"`
	RetentionLabel      string        `yaml:"retention_label"`
	KeepLast            int           `yaml:"keep_last"`
	SnapshotName        string        `yaml:"snapshot_name"`
	OperationTimeout    time.Duration `yaml:"-"`
	OperationTimeoutRaw string        `yaml:"operation_timeout"`
	ProjectConcurrency  int           `yaml:"project_concurrency"`
	ServerConcurrency   int           `yaml:"server_concurrency"`
}

type Project struct {
	Name    string   `yaml:"name"`
	Include []string `yaml:"include,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
}

type Policy struct {
	LabelSelector  string
	RetentionLabel string
	KeepLast       int
	SnapshotName   string
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

func Default() Config {
	return Config{
		Version: 1,
		Defaults: Defaults{
			LabelSelector:       DefaultSelector,
			RetentionLabel:      DefaultRetentionLabel,
			KeepLast:            3,
			SnapshotName:        DefaultSnapshotName,
			OperationTimeout:    time.Hour,
			OperationTimeoutRaw: "1h",
			ProjectConcurrency:  4,
			ServerConcurrency:   4,
		},
	}
}

func DefaultPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "snapzner", "config.yaml"), nil
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c := Default()
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("decode config: multiple YAML documents are not supported")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func LoadOrDefault(path string) (Config, error) {
	c, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	return c, err
}

func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("config version must be 1")
	}
	if c.Defaults.RetentionLabel == "" {
		return fmt.Errorf("defaults.retention_label cannot be empty")
	}
	if c.Defaults.KeepLast == 0 {
		c.Defaults.KeepLast = 3
	}
	if c.Defaults.KeepLast < 1 {
		return fmt.Errorf("defaults.keep_last must be at least 1")
	}
	if c.Defaults.SnapshotName == "" {
		return fmt.Errorf("defaults.snapshot_name cannot be empty")
	}
	if c.Defaults.OperationTimeoutRaw == "" {
		c.Defaults.OperationTimeoutRaw = "1h"
	}
	d, err := time.ParseDuration(c.Defaults.OperationTimeoutRaw)
	if err != nil || d <= 0 {
		return fmt.Errorf("defaults.operation_timeout must be a positive Go duration")
	}
	c.Defaults.OperationTimeout = d
	if c.Defaults.ProjectConcurrency == 0 {
		c.Defaults.ProjectConcurrency = 4
	}
	if c.Defaults.ServerConcurrency == 0 {
		c.Defaults.ServerConcurrency = 4
	}
	if c.Defaults.ProjectConcurrency < 1 || c.Defaults.ServerConcurrency < 1 {
		return fmt.Errorf("concurrency values must be at least 1")
	}
	seen := map[string]bool{}
	for i, p := range c.Projects {
		if !aliasPattern.MatchString(p.Name) {
			return fmt.Errorf("projects[%d].name is invalid", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate project name %q", p.Name)
		}
		seen[p.Name] = true
		for _, ref := range append(append([]string{}, p.Include...), p.Exclude...) {
			if err := ValidateServerRef(ref); err != nil {
				return fmt.Errorf("project %q: %w", p.Name, err)
			}
		}
	}
	return nil
}

func ValidateServerRef(ref string) error {
	if strings.HasPrefix(ref, "id:") && strings.TrimPrefix(ref, "id:") != "" {
		return nil
	}
	if strings.HasPrefix(ref, "name:") && strings.TrimPrefix(ref, "name:") != "" {
		return nil
	}
	return fmt.Errorf("server reference %q must start with id: or name:", ref)
}

func (c Config) Policy() Policy {
	return Policy{c.Defaults.LabelSelector, c.Defaults.RetentionLabel, c.Defaults.KeepLast, c.Defaults.SnapshotName}
}

func ValidateProjectName(name string) error {
	if !aliasPattern.MatchString(name) {
		return fmt.Errorf("project name must be 1-63 characters using letters, digits, dots, underscores, or dashes")
	}
	return nil
}

func (c Config) FindProject(name string) (Project, bool) {
	for _, p := range c.Projects {
		if p.Name == name {
			return p, true
		}
	}
	return Project{}, false
}

func (c *Config) UpsertProject(name string) {
	for _, p := range c.Projects {
		if p.Name == name {
			return
		}
	}
	c.Projects = append(c.Projects, Project{Name: name})
}

func (c *Config) RemoveProject(name string) bool {
	for i, p := range c.Projects {
		if p.Name == name {
			c.Projects = append(c.Projects[:i], c.Projects[i+1:]...)
			return true
		}
	}
	return false
}

func Save(path string, c Config) error {
	b, err := Marshal(c)
	if err != nil {
		return err
	}
	return atomicWrite(path, b, 0o600)
}

func Marshal(c Config) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(c)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".snapzner-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
