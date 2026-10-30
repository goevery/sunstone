package session

import (
	"context"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	oslogin "cloud.google.com/go/oslogin/apiv1"
	"cloud.google.com/go/oslogin/apiv1/osloginpb"
	"cloud.google.com/go/oslogin/common/commonpb"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const (
	osLoginTestUser    = "deployer@example.com"
	osLoginTestProject = "acme-prod"
)

func TestAuthorizeOSLoginKeyRenewsExistingAuthorizationWithoutImport(t *testing.T) {
	signer, otherSigner := testSigners(t)
	service := newFakeOSLogin()
	service.accounts = []*commonpb.PosixAccount{
		{AccountId: "acme-dev", Username: "svc-dev"},
		{AccountId: osLoginTestProject, Username: "svc-prod"},
	}
	// The server holds the key under its own fingerprint with a different
	// comment; matching must ignore comments, and the renewal must target
	// the server-assigned fingerprint rather than any derived value.
	service.keys["server-fingerprint"] = &commonpb.SshPublicKey{
		Key:                authorizedKeyText(signer.PublicKey()) + " deployer@laptop",
		ExpirationTimeUsec: time.Now().Add(time.Hour).UnixMicro(),
	}
	service.keys["other-fingerprint"] = &commonpb.SshPublicKey{
		Key:                authorizedKeyText(otherSigner.PublicKey()),
		ExpirationTimeUsec: time.Now().Add(time.Hour).UnixMicro(),
	}

	client := newOSLoginClient(t, service)
	started := time.Now()
	username, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err != nil {
		t.Fatal(err)
	}
	if username != "svc-prod" {
		t.Fatalf("username = %q; want svc-prod", username)
	}
	if len(service.profileReads) != 1 {
		t.Fatalf("profile reads = %d; want 1", len(service.profileReads))
	}
	request := service.profileReads[0]
	if request.GetName() != "users/"+osLoginTestUser || request.GetProjectId() != osLoginTestProject {
		t.Fatalf("profile request = %v; want the user's project-scoped profile", request)
	}
	if len(service.imports) != 0 {
		t.Fatalf("imports = %d; want 0", len(service.imports))
	}
	if len(service.updates) != 1 {
		t.Fatalf("updates = %d; want 1", len(service.updates))
	}

	update := service.updates[0]
	wantName := "users/" + osLoginTestUser + "/sshPublicKeys/server-fingerprint"
	if update.GetName() != wantName {
		t.Fatalf("update name = %q; want %q", update.GetName(), wantName)
	}
	if paths := update.GetUpdateMask().GetPaths(); len(paths) != 1 || paths[0] != "expiration_time_usec" {
		t.Fatalf("update mask = %v; want [expiration_time_usec]", paths)
	}
	if update.GetSshPublicKey().GetKey() != "" {
		t.Fatalf("renewal sent key material %q; want none", update.GetSshPublicKey().GetKey())
	}
	assertDesiredExpiry(t, started, update.GetSshPublicKey().GetExpirationTimeUsec())
}

func TestAuthorizeOSLoginKeyRenewsExpiredAuthorization(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	seedAuthorizedKey(t, service, signer)
	fingerprint := fakeFingerprint(authorizedKeyText(signer.PublicKey()))
	service.keys[fingerprint].ExpirationTimeUsec = time.Now().Add(-time.Hour).UnixMicro()

	client := newOSLoginClient(t, service)
	started := time.Now()
	username, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err != nil {
		t.Fatal(err)
	}
	if username != "svc-prod" || len(service.updates) != 1 || len(service.imports) != 0 {
		t.Fatalf("username = %q, updates = %d, imports = %d; want svc-prod, 1, 0", username, len(service.updates), len(service.imports))
	}

	assertDesiredExpiry(t, started, service.keys[fingerprint].GetExpirationTimeUsec())
}

func TestAuthorizeOSLoginKeyImportsToProvisionMissingProfileOrKey(t *testing.T) {
	cases := []struct {
		name         string
		prepare      func(*fakeOSLogin)
		wantUsername string
	}{
		{
			name: "profile without the key",
			prepare: func(service *fakeOSLogin) {
				service.accounts = []*commonpb.PosixAccount{{AccountId: osLoginTestProject, Username: "svc-prod"}}
			},
			wantUsername: "svc-prod",
		},
		{
			name: "missing profile",
			prepare: func(service *fakeOSLogin) {
				service.getProfileErr = status.Error(codes.NotFound, "user has no profile")
				service.provisionAccount = true
			},
			wantUsername: "svc-import",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			signer, _ := testSigners(t)
			service := newFakeOSLogin()
			testCase.prepare(service)

			client := newOSLoginClient(t, service)
			started := time.Now()
			username, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
			if err != nil {
				t.Fatal(err)
			}
			if username != testCase.wantUsername {
				t.Fatalf("username = %q; want %q", username, testCase.wantUsername)
			}
			if len(service.updates) != 0 {
				t.Fatalf("updates = %d; want 0", len(service.updates))
			}
			if len(service.imports) != 1 {
				t.Fatalf("imports = %d; want 1", len(service.imports))
			}

			imported := service.imports[0]
			if imported.GetParent() != "users/"+osLoginTestUser {
				t.Fatalf("import parent = %q", imported.GetParent())
			}
			if imported.GetProjectId() != osLoginTestProject {
				t.Fatalf("import project = %q; want %q", imported.GetProjectId(), osLoginTestProject)
			}
			if imported.GetSshPublicKey().GetKey() != authorizedKeyText(signer.PublicKey()) {
				t.Fatalf("imported key = %q; want %q", imported.GetSshPublicKey().GetKey(), authorizedKeyText(signer.PublicKey()))
			}
			assertDesiredExpiry(t, started, imported.GetSshPublicKey().GetExpirationTimeUsec())
		})
	}
}

func TestAuthorizeOSLoginKeyRenewsWhenImportKeepsOlderExpiry(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	// The key is authorized with a shorter remaining window than the
	// command needs, and the project's POSIX account is missing. The import
	// provisions the account but keeps the older expiry, so the
	// authorization must be renewed explicitly.
	fingerprint := fakeFingerprint(authorizedKeyText(signer.PublicKey()))
	service.keys[fingerprint] = &commonpb.SshPublicKey{
		Key:                authorizedKeyText(signer.PublicKey()),
		ExpirationTimeUsec: time.Now().Add(5 * time.Minute).UnixMicro(),
	}
	service.accounts = []*commonpb.PosixAccount{{AccountId: "acme-dev", Username: "svc-dev"}}
	service.provisionAccount = true

	client := newOSLoginClient(t, service)
	started := time.Now()
	username, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err != nil {
		t.Fatal(err)
	}
	if username != "svc-import" {
		t.Fatalf("username = %q; want svc-import", username)
	}
	if len(service.imports) != 1 || len(service.updates) != 1 {
		t.Fatalf("imports = %d, updates = %d; want 1 and 1", len(service.imports), len(service.updates))
	}

	update := service.updates[0]
	wantName := "users/" + osLoginTestUser + "/sshPublicKeys/" + fingerprint
	if update.GetName() != wantName {
		t.Fatalf("update name = %q; want %q", update.GetName(), wantName)
	}
	if paths := update.GetUpdateMask().GetPaths(); len(paths) != 1 || paths[0] != "expiration_time_usec" {
		t.Fatalf("update mask = %v; want [expiration_time_usec]", paths)
	}
	assertDesiredExpiry(t, started, update.GetSshPublicKey().GetExpirationTimeUsec())
}

func TestAuthorizeOSLoginKeyRenewsWhenImportKeepsFutureExpiry(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	// A pre-existing authorization with a far-future expiry outlives the
	// command window. The contract is an exact 15-minute authorization, not
	// at least 15 minutes, so the expiry the import kept must be normalized
	// by an explicit renewal.
	fingerprint := fakeFingerprint(authorizedKeyText(signer.PublicKey()))
	service.keys[fingerprint] = &commonpb.SshPublicKey{
		Key:                authorizedKeyText(signer.PublicKey()),
		ExpirationTimeUsec: time.Now().Add(24 * time.Hour).UnixMicro(),
	}
	service.accounts = []*commonpb.PosixAccount{{AccountId: "acme-dev", Username: "svc-dev"}}
	service.provisionAccount = true

	client := newOSLoginClient(t, service)
	started := time.Now()
	username, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err != nil {
		t.Fatal(err)
	}
	if username != "svc-import" {
		t.Fatalf("username = %q; want svc-import", username)
	}
	if len(service.imports) != 1 || len(service.updates) != 1 {
		t.Fatalf("imports = %d, updates = %d; want 1 and 1", len(service.imports), len(service.updates))
	}

	update := service.updates[0]
	wantName := "users/" + osLoginTestUser + "/sshPublicKeys/" + fingerprint
	if update.GetName() != wantName {
		t.Fatalf("update name = %q; want %q", update.GetName(), wantName)
	}
	if paths := update.GetUpdateMask().GetPaths(); len(paths) != 1 || paths[0] != "expiration_time_usec" {
		t.Fatalf("update mask = %v; want [expiration_time_usec]", paths)
	}
	assertDesiredExpiry(t, started, update.GetSshPublicKey().GetExpirationTimeUsec())
}

func TestAuthorizeOSLoginKeyImportsAgainWhenKeyVanishedBeforeRenewal(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	service.accounts = []*commonpb.PosixAccount{{AccountId: osLoginTestProject, Username: "svc-prod"}}
	fingerprint := fakeFingerprint(authorizedKeyText(signer.PublicKey()))
	service.keys[fingerprint] = &commonpb.SshPublicKey{
		Key:                authorizedKeyText(signer.PublicKey()),
		ExpirationTimeUsec: time.Now().Add(5 * time.Minute).UnixMicro(),
	}
	// The first renewal reports the key was removed after the profile read,
	// while a concurrent creator re-imported it unexpired with an older
	// expiry: the import must keep that expiry, and the renewal must run
	// again.
	service.notFoundUpdates = 1

	client := newOSLoginClient(t, service)
	started := time.Now()
	username, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err != nil {
		t.Fatal(err)
	}
	if username != "svc-prod" {
		t.Fatalf("username = %q; want svc-prod", username)
	}
	if len(service.imports) != 1 || len(service.updates) != 2 {
		t.Fatalf("imports = %d, updates = %d; want 1 and 2", len(service.imports), len(service.updates))
	}

	renewal := service.updates[1]
	wantName := "users/" + osLoginTestUser + "/sshPublicKeys/" + fingerprint
	if renewal.GetName() != wantName {
		t.Fatalf("renewal name = %q; want %q", renewal.GetName(), wantName)
	}
	assertDesiredExpiry(t, started, renewal.GetSshPublicKey().GetExpirationTimeUsec())
}

func TestAuthorizeOSLoginKeyFailsOnProfileReadErrorsWithoutImporting(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			signer, _ := testSigners(t)
			service := newFakeOSLogin()
			service.getProfileErr = status.Error(code, "profile unavailable")

			client := newOSLoginClient(t, service)
			_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
			if status.Code(err) != code {
				t.Fatalf("error = %v; want code %s", err, code)
			}
			if len(service.imports) != 0 || len(service.updates) != 0 {
				t.Fatalf("imports = %d, updates = %d; want none", len(service.imports), len(service.updates))
			}
		})
	}
}

func TestAuthorizeOSLoginKeyFailsOnRenewalErrorsWithoutImporting(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			signer, _ := testSigners(t)
			service := newFakeOSLogin()
			seedAuthorizedKey(t, service, signer)
			service.updateErr = status.Error(code, "renewal refused")

			client := newOSLoginClient(t, service)
			_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
			if status.Code(err) != code {
				t.Fatalf("error = %v; want code %s", err, code)
			}
			if len(service.imports) != 0 {
				t.Fatalf("imports = %d; want none", len(service.imports))
			}
			if len(service.updates) != 1 {
				t.Fatalf("updates = %d; want 1", len(service.updates))
			}
		})
	}
}

func TestAuthorizeOSLoginKeyFailsOnImportErrors(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	service.getProfileErr = status.Error(codes.NotFound, "user has no profile")
	service.importErr = status.Error(codes.PermissionDenied, "import refused")

	client := newOSLoginClient(t, service)
	_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("error = %v; want code PermissionDenied", err)
	}
	if len(service.updates) != 0 {
		t.Fatalf("updates = %d; want none", len(service.updates))
	}
}

func TestAuthorizeOSLoginKeyFailsWithoutProjectPosixAccount(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	// The import provisions the key but no POSIX account for the project.
	service.getProfileErr = status.Error(codes.NotFound, "user has no profile")

	client := newOSLoginClient(t, service)
	_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err == nil || !strings.Contains(err.Error(), "no POSIX account") {
		t.Fatalf("error = %v; want missing POSIX account failure", err)
	}
	if len(service.updates) != 0 {
		t.Fatalf("updates = %d; want none", len(service.updates))
	}
}

func TestAuthorizeOSLoginKeyFailsWhenRenewalReturnsDifferentKey(t *testing.T) {
	signer, otherSigner := testSigners(t)
	service := newFakeOSLogin()
	seedAuthorizedKey(t, service, signer)
	service.mismatchedKey = authorizedKeyText(otherSigner.PublicKey())

	client := newOSLoginClient(t, service)
	_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v; want key material mismatch", err)
	}
	if len(service.imports) != 0 {
		t.Fatalf("imports = %d; want none", len(service.imports))
	}
}

func TestAuthorizeOSLoginKeyFailsWhenRenewalKeepsWrongExpiry(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	seedAuthorizedKey(t, service, signer)
	service.wrongUpdateExpiry = true

	client := newOSLoginClient(t, service)
	_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err == nil || !strings.Contains(err.Error(), "stored SSH key expiry") {
		t.Fatalf("error = %v; want expiry confirmation failure", err)
	}
	if len(service.imports) != 0 {
		t.Fatalf("imports = %d; want none", len(service.imports))
	}
}

func TestAuthorizeOSLoginKeyFailsWhenImportResponseLacksKey(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	service.getProfileErr = status.Error(codes.NotFound, "user has no profile")
	service.provisionAccount = true
	service.hideKeys = true

	client := newOSLoginClient(t, service)
	_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err == nil || !strings.Contains(err.Error(), "missing the imported SSH key") {
		t.Fatalf("error = %v; want missing imported key failure", err)
	}
	if len(service.updates) != 0 {
		t.Fatalf("updates = %d; want none", len(service.updates))
	}
}

func TestAuthorizeOSLoginKeyFailsOnDuplicateKeyEntries(t *testing.T) {
	signer, _ := testSigners(t)
	service := newFakeOSLogin()
	service.accounts = []*commonpb.PosixAccount{{AccountId: osLoginTestProject, Username: "svc-prod"}}
	key := &commonpb.SshPublicKey{
		Key:                authorizedKeyText(signer.PublicKey()),
		ExpirationTimeUsec: time.Now().Add(time.Hour).UnixMicro(),
	}
	service.keys["fingerprint-one"] = proto.Clone(key).(*commonpb.SshPublicKey)
	service.keys["fingerprint-two"] = proto.Clone(key).(*commonpb.SshPublicKey)

	client := newOSLoginClient(t, service)
	_, err := authorizeOSLoginKey(t.Context(), client, signer, osLoginTestProject, osLoginTestUser)
	if err == nil || !strings.Contains(err.Error(), "entries for this SSH key") {
		t.Fatalf("error = %v; want ambiguous key entries failure", err)
	}
	if len(service.imports) != 0 || len(service.updates) != 0 {
		t.Fatalf("imports = %d, updates = %d; want none", len(service.imports), len(service.updates))
	}
}

func assertDesiredExpiry(t *testing.T, started time.Time, expiresMicros int64) {
	t.Helper()
	expires := time.UnixMicro(expiresMicros)
	oldest := started.Add(sshKeyLifetime - time.Second)
	newest := time.Now().Add(sshKeyLifetime + time.Second)
	if expires.Before(oldest) || expires.After(newest) {
		t.Fatalf("expiry %s outside [%s, %s]", expires, oldest, newest)
	}
}

func testSigners(t *testing.T) (signer, other ssh.Signer) {
	t.Helper()
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	other, err = GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}

	return signer, other
}

func seedAuthorizedKey(t *testing.T, service *fakeOSLogin, signer ssh.Signer) {
	t.Helper()
	service.accounts = []*commonpb.PosixAccount{{AccountId: osLoginTestProject, Username: "svc-prod"}}
	service.keys[fakeFingerprint(authorizedKeyText(signer.PublicKey()))] = &commonpb.SshPublicKey{
		Key:                authorizedKeyText(signer.PublicKey()),
		ExpirationTimeUsec: time.Now().Add(time.Hour).UnixMicro(),
	}
}

// newOSLoginClient returns a real SDK client backed by an in-process OS
// Login service so tests exercise the wire requests production sends.
func newOSLoginClient(t *testing.T, service *fakeOSLogin) *oslogin.Client {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	osloginpb.RegisterOsLoginServiceServer(server, service)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		<-served
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial OS Login test service: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client, err := oslogin.NewClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("create OS Login client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// The SDK retries transient errors with long backoffs; tests inject
	// errors directly, so drop the default retry policy to keep them fast.
	client.CallOptions.GetLoginProfile = nil
	client.CallOptions.ImportSshPublicKey = nil
	client.CallOptions.UpdateSshPublicKey = nil

	return client
}

// fakeOSLogin is an in-process OS Login service that mirrors the live
// behavior validated in the persistent-key PoC: importing never extends an
// unexpired authorization, updates change only masked fields, and keys are
// identified by server-assigned fingerprints.
type fakeOSLogin struct {
	osloginpb.UnimplementedOsLoginServiceServer

	mu       sync.Mutex
	accounts []*commonpb.PosixAccount
	keys     map[string]*commonpb.SshPublicKey

	getProfileErr     error
	importErr         error
	updateErr         error
	notFoundUpdates   int
	provisionAccount  bool
	hideKeys          bool
	mismatchedKey     string
	wrongUpdateExpiry bool

	profileReads []*osloginpb.GetLoginProfileRequest
	imports      []*osloginpb.ImportSshPublicKeyRequest
	updates      []*osloginpb.UpdateSshPublicKeyRequest
}

func newFakeOSLogin() *fakeOSLogin {
	return &fakeOSLogin{keys: make(map[string]*commonpb.SshPublicKey)}
}

func (f *fakeOSLogin) GetLoginProfile(_ context.Context, req *osloginpb.GetLoginProfileRequest) (*osloginpb.LoginProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileReads = append(f.profileReads, req)
	if f.getProfileErr != nil {
		return nil, f.getProfileErr
	}

	return f.profileLocked(req.GetName()), nil
}

func (f *fakeOSLogin) ImportSshPublicKey(_ context.Context, req *osloginpb.ImportSshPublicKeyRequest) (*osloginpb.ImportSshPublicKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, req)
	if f.importErr != nil {
		return nil, f.importErr
	}

	imported := proto.Clone(req.GetSshPublicKey()).(*commonpb.SshPublicKey)
	fingerprint := fakeFingerprint(imported.GetKey())
	if existing, ok := f.keys[fingerprint]; !ok || existing.GetExpirationTimeUsec() <= time.Now().UnixMicro() {
		f.keys[fingerprint] = imported
	}
	if f.provisionAccount {
		f.ensureAccountLocked(req.GetProjectId(), "svc-import")
	}

	return &osloginpb.ImportSshPublicKeyResponse{LoginProfile: f.profileLocked(req.GetParent())}, nil
}

func (f *fakeOSLogin) UpdateSshPublicKey(_ context.Context, req *osloginpb.UpdateSshPublicKeyRequest) (*commonpb.SshPublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, req)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.notFoundUpdates > 0 {
		f.notFoundUpdates--

		return nil, status.Error(codes.NotFound, "SSH key removed after profile read")
	}

	parts := strings.Split(req.GetName(), "/")
	fingerprint := parts[len(parts)-1]
	stored, ok := f.keys[fingerprint]
	if !ok {
		return nil, status.Error(codes.NotFound, "unknown SSH key")
	}
	updated := proto.Clone(stored).(*commonpb.SshPublicKey)
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path == "expiration_time_usec" {
			updated.ExpirationTimeUsec = req.GetSshPublicKey().GetExpirationTimeUsec()
		}
	}
	f.keys[fingerprint] = updated

	if f.mismatchedKey != "" {
		return &commonpb.SshPublicKey{
			Key:                f.mismatchedKey,
			ExpirationTimeUsec: updated.GetExpirationTimeUsec(),
		}, nil
	}
	if f.wrongUpdateExpiry {
		return &commonpb.SshPublicKey{
			Key:                updated.GetKey(),
			ExpirationTimeUsec: updated.GetExpirationTimeUsec() - 1,
		}, nil
	}

	return updated, nil
}

func (f *fakeOSLogin) ensureAccountLocked(project, username string) {
	for _, account := range f.accounts {
		if account.GetAccountId() == project {
			return
		}
	}
	f.accounts = append(f.accounts, &commonpb.PosixAccount{AccountId: project, Username: username})
}

func (f *fakeOSLogin) profileLocked(name string) *osloginpb.LoginProfile {
	accounts := make([]*commonpb.PosixAccount, len(f.accounts))
	for index, account := range f.accounts {
		accounts[index] = proto.Clone(account).(*commonpb.PosixAccount)
	}

	keys := make(map[string]*commonpb.SshPublicKey)
	if !f.hideKeys {
		for fingerprint, key := range f.keys {
			keys[fingerprint] = proto.Clone(key).(*commonpb.SshPublicKey)
		}
	}

	return &osloginpb.LoginProfile{Name: name, PosixAccounts: accounts, SshPublicKeys: keys}
}

// fakeFingerprint derives the fake's server-assigned fingerprint from key
// material. OS Login's real fingerprint scheme is server-internal, and the
// connector only reads fingerprints from the server's profile, so any stable
// value works here.
func fakeFingerprint(authorizedKey string) string {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return "unparseable"
	}

	return base64.RawURLEncoding.EncodeToString(parsed.Marshal())
}
