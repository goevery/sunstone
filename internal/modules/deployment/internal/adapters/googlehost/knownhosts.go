package googlehost

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func sshHostAlias(instanceID uint64) string {
	return fmt.Sprintf("compute.%d:22", instanceID)
}

func tofuHostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	if path == "" {
		return nil, errors.New("known hosts path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create known hosts directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open known hosts: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close known hosts: %w", err)
	}
	if _, err := knownhosts.New(path); err != nil {
		return nil, fmt.Errorf("parse known hosts: %w", err)
	}

	var mu sync.Mutex
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		if key.Type() != ssh.KeyAlgoED25519 {
			return fmt.Errorf("expected an ED25519 SSH host key, got %s", key.Type())
		}
		mu.Lock()
		defer mu.Unlock()

		check, err := knownhosts.New(path)
		if err != nil {
			return fmt.Errorf("read known hosts: %w", err)
		}
		err = check(hostname, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) || len(keyErr.Want) != 0 {
			return fmt.Errorf("SSH host key verification failed for %s: %w", hostname, err)
		}

		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return fmt.Errorf("open known hosts for writing: %w", err)
		}
		line := knownhosts.Line([]string{hostname}, key) + "\n"
		if _, err := file.WriteString(line); err != nil {
			file.Close()
			return fmt.Errorf("record SSH host key: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return fmt.Errorf("sync known hosts: %w", err)
		}

		return file.Close()
	}, nil
}
