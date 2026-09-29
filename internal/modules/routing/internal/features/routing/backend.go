package routing

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type backend struct {
	proxy     *httputil.ReverseProxy
	transport *http.Transport
	ctx       context.Context
	cancel    context.CancelFunc

	mu       sync.Mutex
	draining bool
	active   int
	drained  chan struct{}
}

func newBackend(route Route) *backend {
	target := &url.URL{Scheme: "http", Host: route.Backend.Address}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	ctx, cancel := context.WithCancel(context.Background())
	result := &backend{transport: transport, ctx: ctx, cancel: cancel, drained: closedChannel()}
	result.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			host := proxyRequest.In.Host
			proxyRequest.SetURL(target)
			proxyRequest.Out.Host = host
			proxyRequest.SetXForwarded()
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, "bad gateway", http.StatusBadGateway)
		},
	}
	return result
}

func (b *backend) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining {
		return false
	}
	if b.active == 0 {
		b.drained = make(chan struct{})
	}
	b.active++
	return true
}

func (b *backend) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active--
	if b.active == 0 {
		close(b.drained)
	}
}

func (b *backend) serve(writer http.ResponseWriter, request *http.Request) {
	requestCtx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(b.ctx, cancel)
	defer stop()
	defer cancel()
	b.proxy.ServeHTTP(writer, request.WithContext(requestCtx))
}

func (b *backend) drain(timeout time.Duration, activate func()) {
	b.mu.Lock()
	b.draining = true
	drained := b.drained
	activate()
	b.mu.Unlock()

	timer := time.NewTimer(timeout)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
		b.cancel()
		<-drained
	}
	b.cancel()
	b.transport.CloseIdleConnections()
}

func (b *backend) stop() {
	b.cancel()
	b.transport.CloseIdleConnections()
}

func closedChannel() chan struct{} {
	result := make(chan struct{})
	close(result)
	return result
}
