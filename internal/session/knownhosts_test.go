package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh/knownhosts"
)

func TestTOFURejectsChangedHostKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ssh", "google_compute_known_hosts")
	alias := sshHostAlias(8845001922363177616)
	remote := &net.TCPAddr{IP: net.IPv4zero, Port: 22}
	first, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	callback, err := tofuHostKeyCallback(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := callback(alias, remote, first.PublicKey()); err != nil {
		t.Fatalf("trust first use: %v", err)
	}
	if err := callback(alias, remote, changed.PublicKey()); err == nil {
		t.Fatal("accepted changed host key")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := knownhosts.Line([]string{alias}, first.PublicKey()) + "\n"
	if string(contents) != want {
		t.Fatalf("known hosts = %q, want %q", contents, want)
	}
}
