package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cedws/iapc/iap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const (
	sshAuthenticationAttempts       = 6
	sshAuthenticationInitialBackoff = 250 * time.Millisecond
	sshAuthenticationMaximumBackoff = 2 * time.Second
)

// Target identifies one Compute Engine VM.
type Target struct {
	Project  string
	Zone     string
	Instance string
}

// Identity selects the service account a session impersonates.
type Identity struct {
	ServiceAccount string
}

// GCPConnector establishes sessions to Compute Engine VMs using service
// account impersonation, OS Login, and IAP tunnels, reusing impersonated
// credentials and renewed OS Login key authorizations across targets that
// share them.
type GCPConnector struct {
	knownHostsPath string
	signer         ssh.Signer

	mu         sync.Mutex
	identities map[loginKey]*loginIdentity
}

// NewGCPConnector constructs a connector that verifies SSH host keys
// against knownHostsPath and authenticates with signer. Both collaborators
// are mandatory; the composition root supplies them.
func NewGCPConnector(knownHostsPath string, signer ssh.Signer) (*GCPConnector, error) {
	if knownHostsPath == "" {
		return nil, errors.New("known hosts path is empty")
	}
	if signer == nil {
		return nil, errors.New("SSH signer is required")
	}

	return &GCPConnector{
		knownHostsPath: knownHostsPath,
		signer:         signer,
		identities:     make(map[loginKey]*loginIdentity),
	}, nil
}

// Connect impersonates the identity, renews the OS Login authorization of
// its persistent SSH key, and opens an IAP tunnel carrying an
// authenticated SSH connection to the target.
func (c *GCPConnector) Connect(ctx context.Context, target Target, identity Identity) (*Session, error) {
	credentials, err := c.credentials(ctx, target.Project, identity.ServiceAccount, prepareCredentials)
	if err != nil {
		return nil, err
	}
	instance, err := prepareTarget(
		ctx,
		credentials,
		c.signer,
		target.Project,
		identity.ServiceAccount,
		func() (*compute.Instance, error) {
			return credentials.compute.Instances.Get(target.Project, target.Zone, target.Instance).Context(ctx).Do()
		},
		authorizeLoginKey,
	)
	if err != nil {
		return nil, err
	}
	if instance.Id == 0 {
		return nil, errors.New("Compute Engine returned an empty VM instance ID")
	}

	hostAlias := sshHostAlias(instance.Id)
	hostKeyCallback, err := tofuHostKeyCallback(c.knownHostsPath)
	if err != nil {
		return nil, err
	}

	return retrySSHAuthentication(ctx, func() (*Session, error) {
		tokenSource := credentials.tokenSource
		// The tunnel is dialed under establishSession's owned lifecycle so
		// the session survives this attempt's setup context; only credentials,
		// key authorization, and the Compute lookup above honor the caller
		// context directly.
		return establishSession(ctx, func(lifeCtx context.Context) (net.Conn, error) {
			tunnel, err := iap.Dial(lifeCtx,
				iap.WithProject(target.Project),
				iap.WithInstance(target.Instance, target.Zone, "nic0"),
				iap.WithPort("22"),
				iap.WithTokenSource(&tokenSource),
			)
			if err != nil {
				return nil, fmt.Errorf("open IAP tunnel: %w", err)
			}

			return tunnel, nil
		}, Handshake{
			User:            credentials.username,
			Signer:          c.signer,
			HostAlias:       hostAlias,
			HostKeyCallback: hostKeyCallback,
		})
	})
}

func sshHostAlias(instanceID uint64) string {
	return fmt.Sprintf("compute.%d:22", instanceID)
}

func (c *GCPConnector) credentials(ctx context.Context, project, serviceAccount string, prepare func(context.Context, string) (*loginIdentity, error)) (*loginIdentity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := loginKey{project: project, serviceAccount: serviceAccount}
	if identity, ok := c.identities[key]; ok {
		return identity, nil
	}
	identity, err := prepare(ctx, serviceAccount)
	if err != nil {
		return nil, err
	}
	c.identities[key] = identity

	return identity, nil
}

type loginKey struct {
	project        string
	serviceAccount string
}

type loginIdentity struct {
	tokenSource oauth2.TokenSource
	compute     *compute.Service

	loginOnce sync.Once
	username  string
	loginErr  error
}

func (identity *loginIdentity) login(ctx context.Context, signer ssh.Signer, project, serviceAccount string, authorize func(context.Context, oauth2.TokenSource, ssh.Signer, string, string) (string, error)) error {
	identity.loginOnce.Do(func() {
		identity.username, identity.loginErr = authorize(ctx, identity.tokenSource, signer, project, serviceAccount)
	})

	return identity.loginErr
}

func prepareTarget(ctx context.Context, identity *loginIdentity, signer ssh.Signer, project, serviceAccount string, lookup func() (*compute.Instance, error), authorize func(context.Context, oauth2.TokenSource, ssh.Signer, string, string) (string, error)) (*compute.Instance, error) {
	loginDone := make(chan error, 1)
	go func() {
		loginDone <- identity.login(ctx, signer, project, serviceAccount, authorize)
	}()
	instance, instanceErr := lookup()
	loginErr := <-loginDone
	if loginErr != nil {
		return nil, loginErr
	}
	if instanceErr != nil {
		return nil, fmt.Errorf("get VM: %w", instanceErr)
	}

	return instance, nil
}

func prepareCredentials(ctx context.Context, serviceAccount string) (*loginIdentity, error) {
	tokenSource, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: serviceAccount,
		Scopes:          []string{compute.CloudPlatformScope},
	})
	if err != nil {
		return nil, fmt.Errorf("impersonate service account %s: %w", serviceAccount, err)
	}
	computeClient, err := compute.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return nil, fmt.Errorf("create Compute Engine client: %w", err)
	}

	return &loginIdentity{tokenSource: tokenSource, compute: computeClient}, nil
}

func retrySSHAuthentication(ctx context.Context, connect func() (*Session, error)) (*Session, error) {
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
