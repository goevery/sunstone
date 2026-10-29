package routing

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
)

// ServerConfig contains the listeners, the router, and the shutdown
// policy. All fields are mandatory. Construction acquires nothing; when
// Serve ends, it shuts both listeners down and closes the router.
type ServerConfig struct {
	TrafficListener net.Listener
	ControlListener net.Listener
	Router          *Router
	ShutdownTimeout time.Duration
}

// Server serves public HTTP traffic through the router and private
// route-control requests on the control listener until its context is
// canceled or a listener fails, then shuts both listeners down and
// releases the router.
type Server struct {
	config ServerConfig
}

// NewServer constructs the HTTP server over the router. It acquires no
// resources; the listeners stay open until Serve ends, which shuts them
// down and releases the router.
func NewServer(config ServerConfig) (*Server, error) {
	if config.TrafficListener == nil || config.ControlListener == nil {
		return nil, errors.New("traffic and control listeners are required")
	}
	if config.Router == nil {
		return nil, errors.New("router is required")
	}
	if config.ShutdownTimeout <= 0 {
		return nil, errors.New("shutdown timeout must be positive")
	}

	return &Server{config: config}, nil
}

// Serve runs both listeners until the context is canceled or one listener
// fails. On exit it drains both listeners within the shutdown timeout,
// closes the router, and returns the terminal listener error if any, else
// the shutdown error.
func (s *Server) Serve(ctx context.Context) error {
	trafficServer := &http.Server{Handler: http.HandlerFunc(s.config.Router.Proxy)}
	path, controlHandler := sunbeampbconnect.NewSunbeamHandler(newControlHandler(s.config.Router))
	controlMux := http.NewServeMux()
	controlMux.Handle(path, controlHandler)
	controlServer := &http.Server{Handler: controlMux}

	errorsCh := make(chan error, 2)
	go func() { errorsCh <- serve(trafficServer, s.config.TrafficListener) }()
	go func() { errorsCh <- serve(controlServer, s.config.ControlListener) }()

	var terminal error
	select {
	case <-ctx.Done():
	case terminal = <-errorsCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
	defer cancel()
	shutdownErr := errors.Join(controlServer.Shutdown(shutdownCtx), trafficServer.Shutdown(shutdownCtx))
	s.config.Router.Close()
	if terminal != nil {
		return terminal
	}

	return shutdownErr
}

// serve accepts connections on the listener until the server closes.
func serve(server *http.Server, listener net.Listener) error {
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}
