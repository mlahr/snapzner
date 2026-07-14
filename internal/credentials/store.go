package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Store struct {
	Version int               `yaml:"version"`
	Tokens  map[string]string `yaml:"tokens"`
}

func PathForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "credentials.yaml")
}

func Load(path string) (Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Store{Version: 1, Tokens: map[string]string{}}, nil
		}
		return Store{}, err
	}
	var s Store
	if err := yaml.Unmarshal(b, &s); err != nil {
		return Store{}, fmt.Errorf("decode credentials: %w", err)
	}
	if s.Version != 1 {
		return Store{}, fmt.Errorf("credentials version must be 1")
	}
	if s.Tokens == nil {
		s.Tokens = map[string]string{}
	}
	return s, nil
}

func (s Store) Token(project string) (string, error) {
	t := s.Tokens[project]
	if t == "" {
		return "", fmt.Errorf("no credential configured for project %q", project)
	}
	return t, nil
}

func Save(path string, s Store) error {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Tokens == nil {
		s.Tokens = map[string]string{}
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
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
