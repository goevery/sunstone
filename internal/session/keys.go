package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// identityPattern accepts plausible service-account emails such as
// deployer@project.iam.gserviceaccount.com. Every accepted value is a
// single safe path component: separators, "." and ".." cannot match.
var identityPattern = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)

// LoadSigner returns the Ed25519 signer for identity, generating and
// persisting a new key on first use. The key is shared across processes
// under dataHome/sunstone/ssh/<identity>/id_ed25519: an existing key is
// always loaded as-is, and a malformed or corrupt one is reported rather
// than replaced. dataHome is the caller-resolved absolute data directory;
// environment defaults are handled by the caller. Both inputs are validated
// before anything is created, and a missing data home is created with 0700
// without touching an existing one.
func LoadSigner(dataHome, identity string) (ssh.Signer, error) {
	if err := validateIdentity(identity); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(dataHome) {
		return nil, fmt.Errorf("data home %q is not an absolute path", dataHome)
	}
	if err := os.MkdirAll(dataHome, 0o700); err != nil {
		return nil, fmt.Errorf("create data home %s: %w", dataHome, err)
	}

	sshDir := filepath.Join(dataHome, "sunstone", "ssh")
	appDirs := []string{
		filepath.Join(dataHome, "sunstone"),
		sshDir,
		filepath.Join(sshDir, identity),
	}
	for _, dir := range appDirs {
		if err := ensureAppDir(dir); err != nil {
			return nil, err
		}
	}

	keyDir := filepath.Join(sshDir, identity)
	keyPath := filepath.Join(keyDir, "id_ed25519")

	release, err := acquireKeyLock(keyPath + ".lock")
	if err != nil {
		return nil, err
	}
	defer release()

	// Under the lock, either parse the existing key or generate one, so
	// concurrent processes converge on a single complete key file.
	// O_NOFOLLOW refuses symlinks and O_NONBLOCK keeps a planted FIFO from
	// hanging the open; neither flag affects regular files.
	file, err := os.OpenFile(keyPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		signer, err := parseExistingKey(keyPath, file)
		file.Close()
		if err != nil {
			return nil, err
		}

		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("key file %s is a symlink", keyPath)
		}

		return nil, fmt.Errorf("open key file: %w", err)
	}

	return writeNewKey(keyDir, keyPath)
}

// validateIdentity rejects values that are not safe single path components
// or not plausible email addresses.
func validateIdentity(identity string) error {
	switch {
	case identity == "":
		return errors.New("identity is empty")
	case strings.ContainsAny(identity, `/\`):
		return fmt.Errorf("identity %q contains a path separator", identity)
	case identity == "." || identity == "..":
		return fmt.Errorf("identity %q is not a safe path component", identity)
	case !identityPattern.MatchString(identity):
		return fmt.Errorf("identity %q is not a plausible email address", identity)
	}

	return nil
}

// ensureAppDir validates an app-owned directory, creating it with 0700 on
// first use. Existing directories are never modified: symlinks and paths
// with group/other permissions or a foreign owner are rejected.
func ensureAppDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create directory %s: %w", path, err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a %s, not a directory", path, info.Mode().Type())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("directory %s has unsafe permissions %o", path, perm)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("directory %s is owned by uid %d, not %d", path, st.Uid, os.Geteuid())
	}

	return nil
}

// checkPrivateFile rejects anything that is not a regular file owned by
// the effective user with owner-only permissions.
func checkPrivateFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is a %s, not a regular file", path, info.Mode().Type())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has unsafe permissions %o", path, perm)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, os.Geteuid())
	}

	return nil
}

// acquireKeyLock takes an exclusive advisory flock on a stable lock file
// next to the key and returns a release function. O_NOFOLLOW refuses
// symlinks and O_NONBLOCK keeps a planted FIFO from hanging the open; the
// file is checked to be regular and private first, and flock blocking is
// unaffected by O_NONBLOCK.
func acquireKeyLock(lockPath string) (func(), error) {
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("lock file %s is a symlink", lockPath)
		}

		return nil, fmt.Errorf("open lock file: %w", err)
	}

	info, err := lock.Stat()
	if err != nil {
		lock.Close()

		return nil, fmt.Errorf("stat lock file: %w", err)
	}
	if err := checkPrivateFile(lockPath, info); err != nil {
		lock.Close()

		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		lock.Close()

		return nil, fmt.Errorf("lock key: %w", err)
	}
	release := func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}

	return release, nil
}

// parseExistingKey parses the already-opened private key file, refusing
// anything that is not a regular private file owned by the effective user.
func parseExistingKey(keyPath string, file *os.File) (ssh.Signer, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat key file: %w", err)
	}
	if err := checkPrivateFile(keyPath, info); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse key file %s: %w", keyPath, err)
	}

	return signer, nil
}

// writeNewKey generates an Ed25519 key and publishes it atomically: the
// OpenSSH private key is written to a private temporary file in the same
// directory, synced, renamed into place, and the directory synced, so no
// reader ever observes a partial or world-readable key, and a failed
// generation or write never leaves a partial final key behind.
func writeNewKey(keyDir, keyPath string) (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("create signer from generated key: %w", err)
	}

	tmp, err := os.CreateTemp(keyDir, "."+filepath.Base(keyPath)+"-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary key file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(pem.EncodeToMemory(block)); err != nil {
		tmp.Close()

		return nil, fmt.Errorf("write private key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()

		return nil, fmt.Errorf("sync private key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temporary key file: %w", err)
	}
	if err := os.Rename(tmpPath, keyPath); err != nil {
		return nil, fmt.Errorf("publish private key: %w", err)
	}

	dir, err := os.Open(keyDir)
	if err != nil {
		return nil, fmt.Errorf("open key directory for sync: %w", err)
	}
	defer dir.Close()
	if err := unix.Fsync(int(dir.Fd())); err != nil {
		return nil, fmt.Errorf("sync key directory: %w", err)
	}

	return signer, nil
}
