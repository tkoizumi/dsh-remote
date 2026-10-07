// Package proxy serves the stable local endpoint that fronts DeepSeek Harness.
//
// It listens on loopback only and does two things:
//
//   - GET /dsh redirects the browser to DeepSeek Harness's current bootstrap
//     URL (/?token=<latest token>), keeping one bookmark working across every
//     DeepSeek Harness restart.
//   - every other request is reverse-proxied to DeepSeek Harness, preserving
//     the original Host header (DeepSeek Harness binds its auth cookie to the
//     request authority) and streaming WebSocket upgrades through untouched.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// BootstrapPath is the stable path users bookmark. It never changes while
// DeepSeek Harness rotates its token.
const BootstrapPath = "/dsh"

// HealthPath is a loopback-only JSON status endpoint used by `dsh-remote
// status`. It is intentionally not part of the public surface.
const HealthPath = "/__dsh_remote/health"

// TokenFunc returns the current DeepSeek Harness bootstrap token, or "" when no
// token has been captured yet.
type TokenFunc func() string

// Server is the stable local front end for DeepSeek Harness.
type Server struct {
	upstream *url.URL
	token    TokenFunc
	version  string

	http *http.Server
}

// New builds a Server that forwards to upstream (for example
// http://127.0.0.1:3080) and bootstraps with the token returned by token.
func New(upstream *url.URL, token TokenFunc, version string) *Server {
	s := &Server{upstream: upstream, token: token, version: version}
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return s
}

// Handler returns the routing handler. It is exported for tests.
func (s *Server) Handler() http.Handler {
	reverse := httputil.NewSingleHostReverseProxy(s.upstream)
	// Preserve the client's Host header: DeepSeek Harness names its auth cookie
	// after the request authority, so rewriting it here would break the session.
	inner := reverse.Director
	reverse.Director = func(req *http.Request) {
		host := req.Host
		inner(req)
		req.Host = host
	}
	// Stream promptly: the UI uses a WebSocket multiplexer and may use SSE.
	reverse.FlushInterval = -1
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		status := http.StatusBadGateway
		msg := "dsh-remote: could not reach DeepSeek Harness at " + s.upstream.String() + "\n"
		if errors.Is(err, context.Canceled) {
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, msg)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(BootstrapPath, s.handleBootstrap)
	mux.HandleFunc(BootstrapPath+"/", s.handleBootstrap)
	mux.HandleFunc(HealthPath, s.handleHealth)
	mux.Handle("/", reverse)
	return mux
}

// handleBootstrap redirects to DeepSeek Harness's current token URL. Until a
// token exists it answers with a retryable 503 instead of a broken redirect.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "dsh-remote: method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := s.token()
	if token == "" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "2")
		http.Error(w, "dsh-remote: DeepSeek Harness has not reported its authentication token yet; retry in a moment.", http.StatusServiceUnavailable)
		return
	}
	// The token must land on the authority root: DeepSeek Harness only performs
	// the token exchange for GET "/" and then mints an authority-bound cookie.
	location := "/?token=" + url.QueryEscape(token)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, location, http.StatusFound)
}

// handleHealth reports loopback-only JSON status for `dsh-remote status`.
//
// Both the peer and the requested Host must be loopback. Tailscale Serve
// connects from 127.0.0.1, so checking the peer alone would still expose this
// endpoint to the tailnet behind the tailnet name.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) || !isLoopback(r.Host) {
		http.Error(w, "dsh-remote: health endpoint is loopback only", http.StatusForbidden)
		return
	}
	body, err := json.MarshalIndent(Health{
		Status:        "ok",
		Version:       s.version,
		Upstream:      s.upstream.String(),
		TokenCaptured: s.token() != "",
	}, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(body, '\n'))
}

// Health is the JSON shape of HealthPath.
type Health struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Upstream      string `json:"upstream"`
	TokenCaptured bool   `json:"tokenCaptured"`
}

func isLoopback(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip != nil {
		return ip.IsLoopback()
	}
	// An explicit loopback hostname (as `dsh-remote status` uses) is fine too.
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

// Serve accepts connections on ln until Shutdown or Close is called.
func (s *Server) Serve(ln net.Listener) error {
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server, waiting for in-flight requests up to timeout.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// Listen binds the stable endpoint on loopback only.
//
// Loopback is the default because this endpoint turns a token into a session
// for anyone who can reach it: on loopback the only path in is Tailscale Serve,
// where tailnet membership is the gate. Binding a LAN address is possible via
// ListenOn, but it is an explicit, documented trade-off (see --lan).
func Listen(port int) (net.Listener, error) {
	return ListenOn("127.0.0.1", port)
}

// ListenOn binds the stable endpoint on the given host. The host is a literal
// address, never 0.0.0.0, so callers must name each interface they expose.
func ListenOn(host string, port int) (net.Listener, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}
