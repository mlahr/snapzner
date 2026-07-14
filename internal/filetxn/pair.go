package filetxn

import (
	"fmt"
	"os"
	"path/filepath"
)

type File struct {
	Path string
	Data []byte
	Mode os.FileMode
}

var renameFile = os.Rename

// WritePair replaces two files in order and restores both original files if an
// in-process error occurs. Callers should put credentials first and config
// second so a process crash cannot expose new config without its credentials.
func WritePair(first, second File) error {
	if filepath.Dir(first.Path) != filepath.Dir(second.Path) {
		return fmt.Errorf("transaction files must share a directory")
	}
	dir := filepath.Dir(first.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}

	firstTemp, err := prepare(dir, first)
	if err != nil {
		return err
	}
	defer os.Remove(firstTemp)
	secondTemp, err := prepare(dir, second)
	if err != nil {
		return err
	}
	defer os.Remove(secondTemp)

	firstBackup, firstExisted, err := backup(first.Path)
	if err != nil {
		return err
	}
	secondBackup, secondExisted, err := backup(second.Path)
	if err != nil {
		restore(first.Path, firstBackup, firstExisted)
		return err
	}
	committedFirst := false
	committedSecond := false
	rollback := func() {
		if committedSecond {
			_ = os.Remove(second.Path)
		}
		if committedFirst {
			_ = os.Remove(first.Path)
		}
		restore(second.Path, secondBackup, secondExisted)
		restore(first.Path, firstBackup, firstExisted)
	}
	if err := renameFile(firstTemp, first.Path); err != nil {
		rollback()
		return err
	}
	committedFirst = true
	if err := renameFile(secondTemp, second.Path); err != nil {
		rollback()
		return err
	}
	committedSecond = true
	if err := syncDir(dir); err != nil {
		rollback()
		return err
	}
	if firstExisted {
		_ = os.Remove(firstBackup)
	}
	if secondExisted {
		_ = os.Remove(secondBackup)
	}
	_ = syncDir(dir)
	return nil
}

func prepare(dir string, file File) (string, error) {
	f, err := os.CreateTemp(dir, ".snapzner-new-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	fail := func(err error) (string, error) { _ = f.Close(); _ = os.Remove(name); return "", err }
	if err := f.Chmod(file.Mode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(file.Data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func backup(path string) (string, bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".snapzner-backup-*")
	if err != nil {
		return "", false, err
	}
	backup := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(backup)
		return "", false, err
	}
	if err := os.Remove(backup); err != nil {
		return "", false, err
	}
	if err := renameFile(path, backup); err != nil {
		return "", false, err
	}
	return backup, true, nil
}

func restore(path, backup string, existed bool) {
	if existed {
		_ = renameFile(backup, path)
	} else {
		_ = os.Remove(path)
	}
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
