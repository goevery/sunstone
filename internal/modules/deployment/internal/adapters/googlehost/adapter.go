package googlehost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	oslogin "cloud.google.com/go/oslogin/apiv1"
	"cloud.google.com/go/oslogin/apiv1/osloginpb"
	"cloud.google.com/go/oslogin/common/commonpb"
	"github.com/cedws/iapc/iap"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"github.com/goevery/sunstone/internal/modules/routing"
	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const sshKeyLifetime = 15 * time.Minute

// Adapter connects to private Compute Engine VMs using OS Login, IAP, and SSH.
type Adapter struct {
	knownHostsPath string
}

// New constructs a Google host adapter using the operator's home directory.
func New() (*Adapter, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("find home directory for SSH known hosts: %v", err)
	}

	return &Adapter{knownHostsPath: filepath.Join(home, ".ssh", "google_compute_known_hosts")}, nil
}

// Connect opens a Docker API client through an authenticated SSH connection.
func (a *Adapter) Connect(ctx context.Context, target deploy.Target, serviceAccount string) (deploy.Host, error) {
	tokenSource, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: serviceAccount,
		Scopes:          []string{compute.CloudPlatformScope},
	})
	if err != nil {
		return nil, fmt.Errorf("impersonate service account %s: %w", serviceAccount, err)
	}
	signer, err := generateKey()
	if err != nil {
		return nil, err
	}
	computeClient, err := compute.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return nil, fmt.Errorf("create Compute Engine client: %w", err)
	}
	instance, err := computeClient.Instances.Get(target.Project, target.Zone, target.Instance).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get VM: %w", err)
	}
	if instance.Id == 0 {
		return nil, errors.New("Compute Engine returned an empty VM instance ID")
	}

	username, err := importLoginKey(ctx, tokenSource, signer, target.Project, serviceAccount)
	if err != nil {
		return nil, err
	}
	hostAlias := sshHostAlias(instance.Id)
	hostKeyCallback, err := tofuHostKeyCallback(a.knownHostsPath)
	if err != nil {
		return nil, err
	}
	tunnel, err := iap.Dial(ctx,
		iap.WithProject(target.Project),
		iap.WithInstance(target.Instance, target.Zone, "nic0"),
		iap.WithPort("22"),
		iap.WithTokenSource(&tokenSource),
	)
	if err != nil {
		return nil, fmt.Errorf("open IAP tunnel: %w", err)
	}

	host, err := openHost(tunnel, signer, username, hostAlias, hostKeyCallback)
	if err != nil {
		tunnel.Close()
		return nil, err
	}

	return host, nil
}

func importLoginKey(ctx context.Context, tokenSource oauth2.TokenSource, signer ssh.Signer, project, user string) (string, error) {
	loginClient, err := oslogin.NewClient(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return "", fmt.Errorf("create OS Login client: %w", err)
	}
	defer loginClient.Close()

	expires := time.Now().Add(sshKeyLifetime)
	response, err := loginClient.ImportSshPublicKey(ctx, &osloginpb.ImportSshPublicKeyRequest{
		Parent:    "users/" + user,
		ProjectId: project,
		SshPublicKey: &commonpb.SshPublicKey{
			Key:                strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
			ExpirationTimeUsec: expires.UnixMicro(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("import OS Login SSH key: %w", err)
	}
	for _, account := range response.GetLoginProfile().GetPosixAccounts() {
		if account.GetAccountId() == project && account.GetUsername() != "" {
			return account.GetUsername(), nil
		}
	}

	return "", fmt.Errorf("OS Login profile has no POSIX account for %s", project)
}

func openHost(tunnel net.Conn, signer ssh.Signer, username, hostAlias string, hostKeyCallback ssh.HostKeyCallback) (*host, error) {
	if err := tunnel.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set SSH handshake deadline: %w", err)
	}
	connection, channels, requests, err := ssh.NewClientConn(tunnel, hostAlias, &ssh.ClientConfig{
		User:              username,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
	})
	if err != nil {
		return nil, fmt.Errorf("SSH handshake: %w", err)
	}
	sshClient := ssh.NewClient(connection, channels, requests)
	if err := tunnel.SetDeadline(time.Time{}); err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("clear SSH handshake deadline: %w", err)
	}
	dockerClient, err := client.New(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return sshClient.DialContext(ctx, "unix", "/var/run/docker.sock")
		}),
		client.WithTimeout(30*time.Second),
	)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("create Docker client: %w", err)
	}

	controlTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return sshClient.DialContext(ctx, "tcp", routing.DefaultControlAddress)
		},
	}
	controlClient := &http.Client{Transport: controlTransport}

	return &host{
		docker:           dockerClient,
		ssh:              sshClient,
		controlTransport: controlTransport,
		routes:           sunbeampbconnect.NewSunbeamClient(controlClient, "http://sunbeam"),
	}, nil
}

func generateKey() (ssh.Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral SSH key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("create SSH signer: %w", err)
	}

	return signer, nil
}
