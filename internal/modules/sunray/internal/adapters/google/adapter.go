package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
)

var numericResource = regexp.MustCompile(`^projects/[A-Za-z0-9-]+/secrets/[A-Za-z0-9_-]+/versions/[1-9][0-9]*$`)
var projectNumber = regexp.MustCompile(`^[0-9]+$`)

// Adapter accesses Secret Manager using metadata credentials only.
type Adapter struct {
	client             *http.Client
	metadata, endpoint string
}

// New requires explicit HTTP and endpoint collaborators.
func New(client *http.Client, metadata, endpoint string) (*Adapter, error) {
	if client == nil {
		return nil, errors.New("HTTP client is required")
	}
	for _, address := range []string{metadata, endpoint} {
		parsed, err := url.Parse(address)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("invalid host integration endpoint")
		}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Adapter{client: &copyClient, metadata: metadata, endpoint: strings.TrimRight(endpoint, "/")}, nil
}

// Fetch returns values and numeric version references without caching payloads.
func (a *Adapter) Fetch(ctx context.Context, references map[string]string) (map[string]string, map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.metadata, nil)
	if err != nil {
		return nil, nil, errors.New("invalid metadata request")
	}
	request.Header.Set("Metadata-Flavor", "Google")
	response, err := a.request(request)
	if err != nil {
		return nil, nil, errors.New("fetch VM identity token: metadata request failed")
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&token)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Metadata-Flavor") != "Google" || decodeErr != nil || token.AccessToken == "" {
		return nil, nil, fmt.Errorf("fetch VM identity token: invalid metadata response (HTTP %d)", response.StatusCode)
	}
	values, resolved := make(map[string]string), make(map[string]string)
	names := make(map[string][]string)
	for name, reference := range references {
		names[reference] = append(names[reference], name)
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	var mutex sync.Mutex
	for reference, keys := range names {
		group.Go(func() error {
			value, numeric, err := a.access(ctx, token.AccessToken, reference)
			if err != nil {
				return err
			}
			mutex.Lock()
			defer mutex.Unlock()
			for _, name := range keys {
				values[name], resolved[name] = value, numeric
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	return values, resolved, nil
}

// Retry only transport failures and transient statuses, with the caller's
// deadline bounding the entire preparation. Never retain error response bodies.
func (a *Adapter) request(request *http.Request) (*http.Response, error) {
	for attempt := 0; attempt < 3; attempt++ {
		response, err := a.client.Do(request)
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

func (a *Adapter) access(ctx context.Context, token, reference string) (string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint+"/v1/"+reference+":access", nil)
	if err != nil {
		return "", "", errors.New("invalid secret access request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.request(request)
	if err != nil {
		return "", "", fmt.Errorf("access secret %s: request failed", reference)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("access secret %s: HTTP %d %s", reference, response.StatusCode, http.StatusText(response.StatusCode))
	}
	var access struct {
		Name    string `json:"name"`
		Payload struct {
			Data []byte       `json:"data"`
			CRC  *json.Number `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 128*1024)).Decode(&access); err != nil || access.Payload.CRC == nil {
		return "", "", fmt.Errorf("access secret %s: invalid payload or missing checksum", reference)
	}
	checksum, err := strconv.ParseUint(access.Payload.CRC.String(), 10, 32)
	if err != nil || uint32(checksum) != crc32.Checksum(access.Payload.Data, crc32.MakeTable(crc32.Castagnoli)) {
		return "", "", fmt.Errorf("access secret %s: checksum mismatch", reference)
	}
	if !utf8.Valid(access.Payload.Data) || strings.ContainsRune(string(access.Payload.Data), 0) {
		return "", "", fmt.Errorf("access secret %s: payload must be UTF-8 without NUL bytes", reference)
	}
	requested, returned := strings.Split(reference, "/"), strings.Split(access.Name, "/")
	if !numericResource.MatchString(access.Name) || len(requested) != 6 || len(returned) != 6 {
		return "", "", fmt.Errorf("access secret %s: response does not identify a numeric version of this secret", reference)
	}
	// Secret Manager canonicalizes a project ID to its project number. Trust
	// that canonicalization from the authenticated API, never another secret.
	sameProject := requested[1] == returned[1] || (!projectNumber.MatchString(requested[1]) && projectNumber.MatchString(returned[1]))
	_, numericRequestErr := strconv.ParseUint(requested[5], 10, 64)
	if !sameProject || requested[3] != returned[3] || (numericRequestErr == nil && requested[5] != returned[5]) {
		return "", "", fmt.Errorf("access secret %s: response does not identify a numeric version of this secret", reference)
	}
	return string(access.Payload.Data), access.Name, nil
}
