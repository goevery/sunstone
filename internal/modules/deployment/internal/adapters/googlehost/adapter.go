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
	"sync"
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

const (
	sshKeyLifetime                  = 15 * time.Minute
	sshAuthenticationAttempts       = 6
	sshAuthenticationInitialBackoff = 250 * time.Millisecond
	sshAuthenticationMaximumBackoff = 2 * time.Second
)

var errSSHAuthentication = errors.New("SSH authentication rejected")

type loginKey struct {
	project        string
	serviceAccount string
}

type loginIdentity struct {
	tokenSource oauth2.TokenSource
	compute     *compute.Service
	username    string
}

// Adapter connects to private Compute Engine VMs using OS Login, IAP, and SSH.
type Adapter struct {
	knownHostsPath string
	signer         ssh.Signer

	mu         sync.Mutex
	identities map[loginKey]loginIdentity
}

// New constructs a Google host adapter using the operator's home directory.
func New() (*Adapter, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("find home directory for SSH known hosts: %v", err)
	}

	signer, err := generateKey()
	if err != nil {
		return nil, err
	}

	return &Adapter{
		knownHostsPath: filepath.Join(home, ".ssh", "google_compute_known_hosts"),
		signer:         signer,
		identities:     make(map[loginKey]loginIdentity),
	}, nil
}

// Connect opens a Docker API client through an authenticated SSH connection.
func (a *Adapter) Connect(ctx context.Context, target deploy.Target, serviceAccount string) (deploy.Host, error) {
	identity, err := a.login(ctx, target.Project, serviceAccount, prepareLogin)
	if err != nil {
		return nil, err
	}
	instance, err := identity.compute.Instances.Get(target.Project, target.Zone, target.Instance).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get VM: %w", err)
	}
	if instance.Id == 0 {
		return nil, errors.New("Compute Engine returned an empty VM instance ID")
	}

	hostAlias := sshHostAlias(instance.Id)
	hostKeyCallback, err := tofuHostKeyCallback(a.knownHostsPath)
	if err != nil {
		return nil, err
	}
	return retrySSHAuthentication(ctx, func() (*host, error) {
		tokenSource := identity.tokenSource
		tunnel, err := iap.Dial(ctx,
			iap.WithProject(target.Project),
			iap.WithInstance(target.Instance, target.Zone, "nic0"),
			iap.WithPort("22"),
			iap.WithTokenSource(&tokenSource),
		)
		if err != nil {
			return nil, fmt.Errorf("open IAP tunnel: %w", err)
		}
		connected, err := openHost(tunnel, a.signer, identity.username, hostAlias, hostKeyCallback)
		if err != nil {
			tunnel.Close()
			return nil, err
		}
		return connected, nil
	})
}

func (a *Adapter) login(ctx context.Context, project, serviceAccount string, prepare func(context.Context, ssh.Signer, string, string) (loginIdentity, error)) (loginIdentity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := loginKey{project: project, serviceAccount: serviceAccount}
	if identity, ok := a.identities[key]; ok {
		return identity, nil
	}
	identity, err := prepare(ctx, a.signer, project, serviceAccount)
	if err != nil {
		return loginIdentity{}, err
	}
	a.identities[key] = identity
	return identity, nil
}

func prepareLogin(ctx context.Context, signer ssh.Signer, project, serviceAccount string) (loginIdentity, error) {
	tokenSource, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: serviceAccount,
		Scopes:          []string{compute.CloudPlatformScope},
	})
	if err != nil {
		return loginIdentity{}, fmt.Errorf("impersonate service account %s: %w", serviceAccount, err)
	}
	computeClient, err := compute.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return loginIdentity{}, fmt.Errorf("create Compute Engine client: %w", err)
	}
	username, err := importLoginKey(ctx, tokenSource, signer, project, serviceAccount)
	if err != nil {
		return loginIdentity{}, err
	}
	return loginIdentity{tokenSource: tokenSource, compute: computeClient, username: username}, nil
}

func retrySSHAuthentication(ctx context.Context, connect func() (*host, error)) (*host, error) {
	backoff := sshAuthenticationInitialBackoff
	for attempt := 1; ; attempt++ {
		connected, err := connect()
		if err == nil || !errors.Is(err, errSSHAuthentication) || attempt == sshAuthenticationAttempts {
			return connected, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, sshAuthenticationMaximumBackoff)
	}
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
		if strings.Contains(err.Error(), "ssh: unable to authenticate") {
			return nil, fmt.Errorf("%w: %v", errSSHAuthentication, err)
		}
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
