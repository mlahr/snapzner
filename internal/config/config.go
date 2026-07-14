package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultSelector       = "AUTOBACKUP=true"
	DefaultRetentionLabel = "AUTOBACKUP.KEEP-MAX"
	DefaultSnapshotName   = "%name%-%timestamp%"
	legacyRetentionLabel  = "AUTOBACKUP.KEEP-LAST"
)

type Config struct {
	Version               int       `yaml:"version"`
	Defaults              Defaults  `yaml:"defaults"`
	Projects              []Project `yaml:"projects"`
	LegacyRetentionFields bool      `yaml:"-"`
}

type Defaults struct {
	LabelSelector       string          `yaml:"label_selector"`
	RetentionLabel      string          `yaml:"retention_label"`
	KeepMax             int             `yaml:"keep_max"`
	KeepLatest          int             `yaml:"keep_latest"`
	KeepTargets         []time.Duration `yaml:"-"`
	KeepTargetsRaw      []string        `yaml:"keep_targets"`
	LegacyKeepMin       *int            `yaml:"keep_min,omitempty"`
	LegacyKeepLast      *int            `yaml:"keep_last,omitempty"`
	LegacyMinAge        *string         `yaml:"min_age,omitempty"`
	LegacyMaxAge        *string         `yaml:"max_age,omitempty"`
	SnapshotName        string          `yaml:"snapshot_name"`
	OperationTimeout    time.Duration   `yaml:"-"`
	OperationTimeoutRaw string          `yaml:"operation_timeout"`
	ProjectConcurrency  int             `yaml:"project_concurrency"`
	ServerConcurrency   int             `yaml:"server_concurrency"`
}

type Project struct {
	Name    string   `yaml:"name"`
	Include []string `yaml:"include,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
}

type Policy struct {
	LabelSelector  string
	RetentionLabel string
	KeepMax        int
	KeepLatest     int
	KeepTargets    []time.Duration
	SnapshotName   string
}

var (
	aliasPattern                 = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	retentionDurationPartPattern = regexp.MustCompile(`^(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h|d|w)`)
)

func Default() Config {
	return Config{
		Version: 1,
		Defaults: Defaults{
			LabelSelector:       DefaultSelector,
			RetentionLabel:      DefaultRetentionLabel,
			KeepMax:             5,
			KeepLatest:          2,
			KeepTargets:         []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour},
			KeepTargetsRaw:      []string{"1d", "1w", "2w"},
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
	if c.Defaults.LegacyKeepMin != nil || c.Defaults.LegacyKeepLast != nil || c.Defaults.LegacyMinAge != nil || c.Defaults.LegacyMaxAge != nil {
		// Legacy count/age fields cannot be translated exactly into age-target
		// slots. Loading them selects the new defaults, and clearing these
		// pointers ensures the next Save writes only the new schema.
		c.Defaults.LegacyKeepMin = nil
		c.Defaults.LegacyKeepLast = nil
		c.Defaults.LegacyMinAge = nil
		c.Defaults.LegacyMaxAge = nil
		c.LegacyRetentionFields = true
		if c.Defaults.RetentionLabel == legacyRetentionLabel {
			c.Defaults.RetentionLabel = DefaultRetentionLabel
		}
	}
	if c.Defaults.KeepMax == 0 {
		c.Defaults.KeepMax = 5
	}
	if c.Defaults.KeepMax < 1 {
		return fmt.Errorf("defaults.keep_max must be at least 1")
	}
	if c.Defaults.KeepLatest == 0 {
		c.Defaults.KeepLatest = 2
	}
	if c.Defaults.KeepLatest < 1 {
		return fmt.Errorf("defaults.keep_latest must be at least 1")
	}
	if c.Defaults.KeepLatest > c.Defaults.KeepMax {
		return fmt.Errorf("defaults.keep_latest cannot exceed defaults.keep_max")
	}
	if c.Defaults.KeepLatest+len(c.Defaults.KeepTargetsRaw) > c.Defaults.KeepMax {
		return fmt.Errorf("defaults.keep_latest plus the number of defaults.keep_targets cannot exceed defaults.keep_max")
	}
	c.Defaults.KeepTargets = make([]time.Duration, len(c.Defaults.KeepTargetsRaw))
	for i, raw := range c.Defaults.KeepTargetsRaw {
		target, err := ParseRetentionDuration(raw)
		if err != nil || target <= 0 {
			return fmt.Errorf("defaults.keep_targets[%d] must be a positive retention duration", i)
		}
		if i > 0 && target <= c.Defaults.KeepTargets[i-1] {
			return fmt.Errorf("defaults.keep_targets must be ordered from youngest to oldest without duplicates")
		}
		c.Defaults.KeepTargets[i] = target
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
	return Policy{
		LabelSelector: c.Defaults.LabelSelector, RetentionLabel: c.Defaults.RetentionLabel,
		KeepMax: c.Defaults.KeepMax, KeepLatest: c.Defaults.KeepLatest,
		KeepTargets: append([]time.Duration(nil), c.Defaults.KeepTargets...), SnapshotName: c.Defaults.SnapshotName,
	}
}

// ParseRetentionDuration parses a fixed elapsed duration. It follows Go's
// duration syntax and additionally treats d as 24 hours and w as 168 hours.
// An empty string or any zero duration parses as zero.
func ParseRetentionDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	remaining := value
	var normalized strings.Builder
	for remaining != "" {
		part := retentionDurationPartPattern.FindString(remaining)
		if part == "" {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		remaining = remaining[len(part):]
		unit := ""
		number := part
		for _, candidate := range []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h", "d", "w"} {
			if strings.HasSuffix(part, candidate) {
				unit = candidate
				number = strings.TrimSuffix(part, candidate)
				break
			}
		}
		if unit == "d" || unit == "w" {
			amount, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q", value)
			}
			hours := 24.0
			if unit == "w" {
				hours = 168.0
			}
			normalized.WriteString(strconv.FormatFloat(amount*hours, 'f', -1, 64))
			normalized.WriteByte('h')
		} else {
			normalized.WriteString(part)
		}
	}
	duration, err := time.ParseDuration(normalized.String())
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", value, err)
	}
	return duration, nil
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
