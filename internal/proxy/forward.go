package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// UpstreamUnreachableHeader marks the 502 response dsh-remote itself generates
// when the dashboard behind a Forwarder cannot be reached. Diagnostics use it to
// tell "the LAN listener is up but the backend is down" apart from a 502 the
// dashboard returned on its own.
const (
	UpstreamUnreachableHeader = "X-Dsh-Remote-Upstream"
	UpstreamUnreachableValue  = "unreachable"
)

// Forwarder is a plain reverse proxy for a local dashboard that already has its
// own bootstrap, routing, and authentication.
//
// It exists because a dashboard bound to loopback has no URL other machines can
// open. Unlike Server it adds no routes, holds no DeepSeek Harness token, and
// knows nothing about the service behind it: it forwards every request, keeps
// the client's Host header (dashboards and WebSocket origins depend on it), and
// streams responses so long-lived SSE and WebSocket connections work.
type Forwarder struct {
	upstream *url.URL
	http     *http.Server
}

// NewForwarder builds a Forwarder that proxies everything to upstream, for
// example http://127.0.0.1:9119.
func NewForwarder(upstream *url.URL) *Forwarder {
	f := &Forwarder{upstream: upstream}

	reverse := httputil.NewSingleHostReverseProxy(upstream)
	// Preserve the client's Host header: a dashboard may build absolute URLs or
	// bind sessions to the authority the browser used.
	inner := reverse.Director
	reverse.Director = func(req *http.Request) {
		host := req.Host
		inner(req)
		req.Host = host
	}
	// Stream promptly: dashboards commonly use WebSockets and SSE.
	reverse.FlushInterval = -1
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set(UpstreamUnreachableHeader, UpstreamUnreachableValue)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "dsh-remote: could not reach %s\n", f.upstream)
	}

	f.http = &http.Server{
		Handler:           reverse,
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return f
}

// Upstream returns the target this forwarder proxies to.
func (f *Forwarder) Upstream() string { return f.upstream.String() }

// Serve accepts connections on ln until Shutdown or Close is called.
func (f *Forwarder) Serve(ln net.Listener) error {
	err := f.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the forwarder, waiting for in-flight requests up to timeout.
func (f *Forwarder) Shutdown(ctx context.Context) error {
	return f.http.Shutdown(ctx)
}
