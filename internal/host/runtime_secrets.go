package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
)

const (
	vmTokenURL             = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token"
	secretAPIURL           = "https://secretmanager.googleapis.com/v1/"
	secretPreparationLimit = 30 * time.Second
)

var (
	secretVersionResource = regexp.MustCompile(`^projects/[A-Za-z0-9-]+/secrets/[A-Za-z0-9_-]+/versions/([1-9][0-9]*|[A-Za-z][A-Za-z0-9_-]*)$`)
	numericSecretResource = regexp.MustCompile(`^projects/[A-Za-z0-9-]+/secrets/[A-Za-z0-9_-]+/versions/[1-9][0-9]*$`)
	secretProjectNumber   = regexp.MustCompile(`^[0-9]+$`)
)

type redactedDiagnostic struct {
	cause   error
	message string
}

func (e *redactedDiagnostic) Error() string { return e.message }
func (e *redactedDiagnostic) Unwrap() error { return e.cause }

// environment resolves each unique reference once for this host connection.
// Compare and create therefore use one snapshot, even if an alias changes.
func (h *DockerRuntime) environment(ctx context.Context, spec ContainerSpec) (map[string]string, error) {
	environment := make(map[string]string, len(spec.Environment)+len(spec.SecretEnvironment))
	maps.Copy(environment, spec.Environment)
	if len(spec.SecretEnvironment) == 0 {
		return environment, nil
	}

	h.secretsMu.Lock()
	defer h.secretsMu.Unlock()
	missing := make(map[string]struct{})
	for _, reference := range spec.SecretEnvironment {
		if !secretVersionResource.MatchString(reference) {
			return nil, errors.New("invalid secret version reference")
		}
		if _, found := h.secretValues[reference]; !found {
			missing[reference] = struct{}{}
		}
	}
	if len(missing) != 0 {
		ctx, cancel := context.WithTimeout(ctx, secretPreparationLimit)
		defer cancel()
		values, err := fetchSecrets(ctx, h.secretClient, missing)
		if err != nil {
			return nil, err
		}
		if h.secretValues == nil {
			h.secretValues = make(map[string]string)
		}

		maps.Copy(h.secretValues, values)
	}

	for name, reference := range spec.SecretEnvironment {
		environment[name] = h.secretValues[reference]
	}

	return environment, nil
}

// Fetch only through the SSH-backed client. Never use ADC, operator tokens,
// subprocesses, proxies, or a redirect that could move credentials elsewhere.
func fetchSecrets(ctx context.Context, client *http.Client, references map[string]struct{}) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, vmTokenURL, nil)
	if err != nil {
		return nil, errors.New("construct metadata request")
	}
	request.Header.Set("Metadata-Flavor", "Google")
	response, err := secretRequest(client, request)
	if err != nil {
		return nil, secretRequestError(ctx, "fetch VM identity token")
	}

	var credentials struct {
		AccessToken string `json:"access_token"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&credentials)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Metadata-Flavor") != "Google" || decodeErr != nil || credentials.AccessToken == "" {
		return nil, fmt.Errorf("fetch VM identity token: invalid metadata response (HTTP %d)", response.StatusCode)
	}

	values := make(map[string]string, len(references))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	var mutex sync.Mutex
	for reference := range references {
		group.Go(func() error {
			value, err := accessSecret(ctx, client, credentials.AccessToken, reference)
			if err != nil {
				return err
			}
			mutex.Lock()
			values[reference] = value
			mutex.Unlock()

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return values, nil
}

func accessSecret(ctx context.Context, client *http.Client, token, reference string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, secretAPIURL+reference+":access", nil)
	if err != nil {
		return "", errors.New("construct secret access request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := secretRequest(client, request)
	if err != nil {
		return "", secretRequestError(ctx, "access secret "+reference)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("access secret %s: HTTP %d %s", reference, response.StatusCode, http.StatusText(response.StatusCode))
	}
	var access struct {
		Name    string `json:"name"`
		Payload struct {
			Data []byte       `json:"data"`
			CRC  *json.Number `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 128*1024)).Decode(&access); err != nil || access.Payload.CRC == nil {
		return "", fmt.Errorf("access secret %s: invalid payload or missing checksum", reference)
	}
	checksum, err := strconv.ParseUint(access.Payload.CRC.String(), 10, 32)
	if err != nil || uint32(checksum) != crc32.Checksum(access.Payload.Data, crc32.MakeTable(crc32.Castagnoli)) {
		return "", fmt.Errorf("access secret %s: checksum mismatch", reference)
	}
	if !utf8.Valid(access.Payload.Data) || strings.ContainsRune(string(access.Payload.Data), 0) {
		return "", fmt.Errorf("access secret %s: payload must be UTF-8 without NUL bytes", reference)
	}
	requested, returned := strings.Split(reference, "/"), strings.Split(access.Name, "/")
	if !numericSecretResource.MatchString(access.Name) || len(returned) != 6 {
		return "", fmt.Errorf("access secret %s: invalid returned version resource", reference)
	}
	// The authenticated API can canonicalize a project ID to its project number.
	sameProject := requested[1] == returned[1] || (!secretProjectNumber.MatchString(requested[1]) && secretProjectNumber.MatchString(returned[1]))
	_, numericErr := strconv.ParseUint(requested[5], 10, 64)
	if !sameProject || requested[3] != returned[3] || (numericErr == nil && requested[5] != returned[5]) {
		return "", fmt.Errorf("access secret %s: returned version does not match requested secret", reference)
	}

	return string(access.Payload.Data), nil
}

func secretRequest(client *http.Client, request *http.Request) (*http.Response, error) {
	for attempt := 0; attempt < 3; attempt++ {
		response, err := client.Do(request)
		if err == nil && (response.StatusCode < 500 && response.StatusCode != 429 || attempt == 2) {
			return response, nil
		}
		if response != nil {
			response.Body.Close()
		}

		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		if attempt == 2 {
			return nil, errors.New("HTTP request failed")
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-request.Context().Done():
			timer.Stop()

			return nil, request.Context().Err()
		case <-timer.C:
		}
	}

	return nil, errors.New("HTTP request failed")
}

func secretRequestError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	// HTTP errors can contain response snippets and must not cross this boundary.
	return fmt.Errorf("%s: request failed", operation)
}

// Redact raw values and their JSON-escaped representation in Docker diagnostics.
// Empty values cannot be meaningfully redacted.
func (h *DockerRuntime) redact(text string) string {
	h.secretsMu.Lock()
	defer h.secretsMu.Unlock()
	var values []string
	add := func(value string) {
		if value == "" {
			return
		}
		values = append(values, value)
		encoded, _ := json.Marshal(value)
		if escaped := string(encoded[1 : len(encoded)-1]); escaped != value {
			values = append(values, escaped)
		}
	}
	for _, value := range h.secretValues {
		add(value)
	}

	for _, value := range h.enrolledSecrets {
		add(value)
	}

	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}

	return text
}

// enrollDeployedSecrets records the deployed values of secret-backed
// environment entries for diagnostic redaction, without fetching them. The
// fetch cache is untouched so ordinary deployments still resolve fresh
// snapshots.
func (h *DockerRuntime) enrollDeployedSecrets(environment []string, secretEnvironment map[string]string) {
	if len(secretEnvironment) == 0 {
		return
	}
	deployed := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		deployed[name] = value
	}

	h.secretsMu.Lock()
	defer h.secretsMu.Unlock()
	for name := range secretEnvironment {
		if value := deployed[name]; value != "" {
			h.enrolledSecrets = append(h.enrolledSecrets, value)
		}
	}
}

// mutationError redacts a mutating request failure and classifies it
// conservatively: only a structured known rejection proves the remote did
// not apply the mutation. Every other failure — lost transport, a generic
// connection failure, a server internal error, or a malformed or unknown
// response — leaves the remote effects unconfirmed.
func (h *DockerRuntime) mutationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	err = h.safeError(err)
	if knownRejection(err) {
		return err
	}

	return unconfirmed(operation, err)
}

func (h *DockerRuntime) safeError(err error) error {
	if err == nil {
		return nil
	}
	message := h.redact(err.Error())
	if message == err.Error() {
		return err
	}

	return &redactedDiagnostic{cause: err, message: message}
}
