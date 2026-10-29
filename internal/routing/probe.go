package routing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// errStartupDeadline reports that the startup probe exhausted its own
// readiness budget, as opposed to inheriting the caller's cancellation:
// the candidate was observed not starting for the whole budget, before
// any route change.
var errStartupDeadline = errors.New("startup probe deadline exceeded")

// waitUntilStarted blocks until the candidate backend answers its startup
// probe successfully, bounded by the startup deadline.
func (r *Router) waitUntilStarted(ctx context.Context, candidate Backend) error {
	ctx, cancel := context.WithTimeoutCause(ctx, r.config.StartupDeadline, errStartupDeadline)
	defer cancel()
	client := &http.Client{
		Timeout: r.config.ProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	probeURL := "http://" + candidate.Address + candidate.StartupProbePath
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 400 {
				return nil
			}
		}

		timer := time.NewTimer(r.config.ProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}
