package session

// OS Login authorization for the connector's persistent SSH key. Each
// command renews the key's authorization for a short window instead of
// rotating the key, so one stable local key per identity authorizes many
// commands and an expired authorization only blocks new connections.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	oslogin "cloud.google.com/go/oslogin/apiv1"
	"cloud.google.com/go/oslogin/apiv1/osloginpb"
	"cloud.google.com/go/oslogin/common/commonpb"
	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const sshKeyLifetime = 15 * time.Minute

// authorizeLoginKey authorizes signer's public key with OS Login for user on
// project and returns the project's POSIX username. It creates a scoped OS
// Login client from the impersonated token source.
func authorizeLoginKey(ctx context.Context, tokenSource oauth2.TokenSource, signer ssh.Signer, project, user string) (string, error) {
	loginClient, err := oslogin.NewClient(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return "", fmt.Errorf("create OS Login client: %w", err)
	}
	defer loginClient.Close()

	return authorizeOSLoginKey(ctx, loginClient, signer, project, user)
}

// authorizeOSLoginKey renews the OS Login authorization of signer's public
// key to span sshKeyLifetime and returns the project's POSIX username. When
// the profile lacks the key or the project's POSIX account, it imports the
// key to provision them. Importing never extends an unexpired authorization
// and never rotates the key; any other error fails the command.
func authorizeOSLoginKey(ctx context.Context, loginClient *oslogin.Client, signer ssh.Signer, project, user string) (string, error) {
	profile, err := readOSLoginProfile(ctx, loginClient, user, project)
	if err != nil {
		return "", err
	}

	expires := time.Now().Add(sshKeyLifetime).UnixMicro()
	fingerprint, err := findAuthorizedKey(profile.GetSshPublicKeys(), signer.PublicKey())
	if err != nil {
		return "", err
	}
	username := posixUsername(profile, project)
	if fingerprint != "" && username != "" {
		resource := keyResource(user, fingerprint)
		err := renewKeyAuthorization(ctx, loginClient, resource, signer, expires)
		if status.Code(err) == codes.NotFound {
			// The key vanished between the profile read and the renewal;
			// importing re-provisions it.
			return authorizeImportedKey(ctx, loginClient, signer, project, user, expires)
		}
		if err != nil {
			return "", err
		}

		return username, nil
	}

	return authorizeImportedKey(ctx, loginClient, signer, project, user, expires)
}

// readOSLoginProfile reads the user's project-scoped login profile. A
// missing profile — the user has not logged in yet — reads as an empty
// profile so importing provisions it; every other error fails the command.
func readOSLoginProfile(ctx context.Context, loginClient *oslogin.Client, user, project string) (*osloginpb.LoginProfile, error) {
	profile, err := loginClient.GetLoginProfile(ctx, &osloginpb.GetLoginProfileRequest{
		Name:      "users/" + user,
		ProjectId: project,
	})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read OS Login profile: %w", err)
	}

	return profile, nil
}

// authorizeImportedKey imports the connector's public key, which provisions
// a missing profile, key, or POSIX account, and returns the project's POSIX
// username. Importing keeps an existing unexpired authorization untouched,
// so when the server holds any other expiry the authorization is renewed
// explicitly to the exact command window.
func authorizeImportedKey(ctx context.Context, loginClient *oslogin.Client, signer ssh.Signer, project, user string, expires int64) (string, error) {
	response, err := loginClient.ImportSshPublicKey(ctx, &osloginpb.ImportSshPublicKeyRequest{
		Parent:    "users/" + user,
		ProjectId: project,
		SshPublicKey: &commonpb.SshPublicKey{
			Key:                authorizedKeyText(signer.PublicKey()),
			ExpirationTimeUsec: expires,
		},
	})
	if err != nil {
		return "", fmt.Errorf("import OS Login SSH key: %w", err)
	}

	profile := response.GetLoginProfile()
	fingerprint, err := findAuthorizedKey(profile.GetSshPublicKeys(), signer.PublicKey())
	if err != nil {
		return "", err
	}
	if fingerprint == "" {
		return "", errors.New("OS Login import response is missing the imported SSH key")
	}
	username := posixUsername(profile, project)
	if username == "" {
		return "", fmt.Errorf("OS Login profile has no POSIX account for %s", project)
	}

	if profile.GetSshPublicKeys()[fingerprint].GetExpirationTimeUsec() != expires {
		// A concurrent creator, or a key already on record with a different
		// expiry — older, far-future, or none — made the import keep that
		// expiry; the command needs an exact 15-minute authorization, so
		// renew it explicitly.
		resource := keyResource(user, fingerprint)
		if err := renewKeyAuthorization(ctx, loginClient, resource, signer, expires); err != nil {
			return "", err
		}
	}

	return username, nil
}

// renewKeyAuthorization extends the authorization of the key named resource
// until expires and verifies the server stored exactly that expiry for the
// connector's key material.
func renewKeyAuthorization(ctx context.Context, loginClient *oslogin.Client, resource string, signer ssh.Signer, expires int64) error {
	updated, err := loginClient.UpdateSshPublicKey(ctx, &osloginpb.UpdateSshPublicKeyRequest{
		Name: resource,
		SshPublicKey: &commonpb.SshPublicKey{
			ExpirationTimeUsec: expires,
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expiration_time_usec"}},
	})
	if err != nil {
		return fmt.Errorf("renew OS Login SSH key authorization: %w", err)
	}
	if updated.GetExpirationTimeUsec() != expires {
		return fmt.Errorf("OS Login stored SSH key expiry %d; want %d", updated.GetExpirationTimeUsec(), expires)
	}

	return verifyAuthorizedKey(updated, signer.PublicKey())
}

// findAuthorizedKey returns the server-assigned fingerprint of the profile
// entry whose key material matches publicKey, ignoring SSH key comments.
// An absent key is not an error: it means the key must be imported. Multiple
// matching entries are ambiguous and fail safely.
func findAuthorizedKey(keys map[string]*commonpb.SshPublicKey, publicKey ssh.PublicKey) (string, error) {
	want := publicKey.Marshal()
	matches := 0
	fingerprint := ""
	for candidate, key := range keys {
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key.GetKey()))
		if err != nil || !bytes.Equal(parsed.Marshal(), want) {
			continue
		}
		matches++
		fingerprint = candidate
	}
	if matches > 1 {
		return "", fmt.Errorf("OS Login profile has %d entries for this SSH key; want one", matches)
	}

	return fingerprint, nil
}

// posixUsername returns the username OS Login assigns to the user on project.
func posixUsername(profile *osloginpb.LoginProfile, project string) string {
	for _, account := range profile.GetPosixAccounts() {
		if account.GetAccountId() == project && account.GetUsername() != "" {
			return account.GetUsername()
		}
	}

	return ""
}

// keyResource is the OS Login resource name of the key identified by
// fingerprint. Fingerprints are assigned by OS Login; the connector only
// ever reads them from the server's profile.
func keyResource(user, fingerprint string) string {
	return fmt.Sprintf("users/%s/sshPublicKeys/%s", user, fingerprint)
}

// authorizedKeyText renders publicKey as the one-line authorized_keys entry
// OS Login stores.
func authorizedKeyText(publicKey ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))
}

// verifyAuthorizedKey fails safely when the server's key material does not
// match the connector's public key.
func verifyAuthorizedKey(key *commonpb.SshPublicKey, publicKey ssh.PublicKey) error {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key.GetKey()))
	if err != nil {
		return fmt.Errorf("OS Login returned an unparseable SSH key: %w", err)
	}
	if !bytes.Equal(parsed.Marshal(), publicKey.Marshal()) {
		return errors.New("OS Login key material does not match the connector's public key")
	}

	return nil
}
