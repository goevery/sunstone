//go:build linux

package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const tmpfsMagic = 0x01021994

type backup struct {
	Config []byte      `json:"config"`
	Mode   os.FileMode `json:"mode"`
}

func requireTmpfs(path string) error {
	for {
		var stat syscall.Statfs_t
		err := syscall.Statfs(path, &stat)
		if err == nil {
			if stat.Type != tmpfsMagic {
				return errors.New("secret preparation and runtime state require tmpfs")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect ephemeral filesystem: %w", err)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return errors.New("cannot locate ephemeral filesystem")
		}
		path = parent
	}
}

func privateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("runtime state directory must be private and owned by runtime user")
	}
	return nil
}

func lock(directory string) (func(), error) {
	if err := privateDirectory(directory); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
}

func readRegular(path string) ([]byte, os.FileMode, error) {
	return readLimited(path, 2*1024*1024)
}

func readLimited(path string, limit int64) ([]byte, os.FileMode, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open OCI state: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, 0, errors.New("OCI state must be a regular file within the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, 0, errors.New("OCI state exceeds limit")
	}
	return data, info.Mode().Perm(), err
}

func replace(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".sunray-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func readSnapshot(path string) (snapshot, error) {
	var state snapshot
	data, _, err := readRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, errors.New("invalid runtime snapshot")
	}
	for name, reference := range state.References {
		if !environmentName.MatchString(name) || !versionName.MatchString(reference) {
			return state, errors.New("invalid runtime snapshot reference")
		}
	}
	return state, nil
}

func recoverBundle(directory, bundle string) error {
	journalPath := filepath.Join(directory, "original.json")
	data, _, err := readLimited(journalPath, 4*1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := requireTmpfs(bundle); err != nil {
		return err
	}
	var journal backup
	if err := json.Unmarshal(data, &journal); err != nil || len(journal.Config) == 0 {
		return errors.New("invalid bundle recovery journal")
	}
	if err := replace(filepath.Join(bundle, "config.json"), journal.Config, journal.Mode); err != nil {
		// A deleted bundle is already gone; never recreate its directory.
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restore original OCI bundle: %w", err)
		}
	}
	orphans, err := filepath.Glob(filepath.Join(bundle, ".sunray-*"))
	if err != nil {
		return err
	}
	for _, orphan := range orphans {
		if err := os.Remove(orphan); err != nil {
			return err
		}
	}
	return removeIfExists(journalPath)
}

func clearState(directory string) error {
	for _, name := range []string{"snapshot.json", "original.json"} {
		if err := removeIfExists(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return cleanupProcesses(directory)
}

func cleanupProcesses(directory string) error {
	entries, err := filepath.Glob(filepath.Join(directory, "process-*.json"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.Remove(entry); err != nil {
			return err
		}
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
