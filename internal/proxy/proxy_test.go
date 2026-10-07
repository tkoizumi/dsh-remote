package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamRecorder is a fake DeepSeek Harness that records what it received.
type upstreamRecorder struct {
	mu       sync.Mutex
	path     string
	rawQuery string
	host     string
}

func (u *upstreamRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			u.serveUpgrade(w, r)
			return
		}
		u.mu.Lock()
		u.path, u.rawQuery, u.host = r.URL.Path, r.URL.RawQuery, r.Host
		u.mu.Unlock()
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, "hello from upstream")
	})
}

// serveUpgrade completes a raw 101 handshake and echoes four bytes.
func (u *upstreamRecorder) serveUpgrade(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	_ = buf.Flush()
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err == nil {
		_, _ = conn.Write(echo)
	}
}

func newTestProxy(t *testing.T, token string) (*httptest.Server, *upstreamRecorder) {
	t.Helper()
	recorder := &upstreamRecorder{}
	upstream := httptest.NewServer(recorder.handler())
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := New(upstreamURL, func() string { return token }, "test")
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	return front, recorder
}

func TestBootstrapRedirectsToTokenURL(t *testing.T) {
	front, _ := newTestProxy(t, "ABC123")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(front.URL + BootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Location"), "/?token=ABC123"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestBootstrapTrailingSlashAlsoRedirects(t *testing.T) {
	front, _ := newTestProxy(t, "tok")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(front.URL + BootstrapPath + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
}

func TestBootstrapEscapesToken(t *testing.T) {
	front, _ := newTestProxy(t, "a+b/c=")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(front.URL + BootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got, want := resp.Header.Get("Location"), "/?token=a%2Bb%2Fc%3D"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestBootstrapBeforeTokenIsUnavailable(t *testing.T) {
	front, _ := newTestProxy(t, "")
	resp, err := http.Get(front.URL + BootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("expected a Retry-After hint")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "has not reported its authentication token") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestProxyForwardsPathQueryAndHost(t *testing.T) {
	front, recorder := newTestProxy(t, "tok")
	resp, err := http.Get(front.URL + "/some/path?x=1&y=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "hello from upstream" {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Fatal("upstream header was not relayed")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.path != "/some/path" {
		t.Fatalf("upstream path = %q, want /some/path", recorder.path)
	}
	if recorder.rawQuery != "x=1&y=2" {
		t.Fatalf("upstream query = %q, want x=1&y=2", recorder.rawQuery)
	}
	// DeepSeek Harness names its auth cookie after the request authority, so the
	// proxy must preserve the client's Host header for the token flow to work.
	frontURL, _ := url.Parse(front.URL)
	if recorder.host != frontURL.Host {
		t.Fatalf("upstream Host = %q, want %q (the client authority)", recorder.host, frontURL.Host)
	}
}

func TestProxyForwardsTokenQueryToUpstream(t *testing.T) {
	front, recorder := newTestProxy(t, "tok")
	resp, err := http.Get(front.URL + "/?token=ABC123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.path != "/" || recorder.rawQuery != "token=ABC123" {
		t.Fatalf("upstream saw path=%q query=%q, want / and token=ABC123", recorder.path, recorder.rawQuery)
	}
}

func TestHealthReportsTokenState(t *testing.T) {
	front, _ := newTestProxy(t, "tok")
	resp, err := http.Get(front.URL + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var health Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || !health.TokenCaptured {
		t.Fatalf("health = %+v", health)
	}
}

func TestHealthRejectsNonLoopbackHost(t *testing.T) {
	front, _ := newTestProxy(t, "tok")
	// Tailscale Serve connects from 127.0.0.1, so the peer check alone would
	// let a tailnet browser read health behind the tailnet name.
	req, err := http.NewRequest(http.MethodGet, front.URL+HealthPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ubuntu-server.tail6d6db9.ts.net"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-loopback Host", resp.StatusCode)
	}
}

func TestProxyReportsBadGatewayWhenUpstreamDown(t *testing.T) {
	upstreamURL, _ := url.Parse("http://127.0.0.1:1") // nothing listens here
	server := New(upstreamURL, func() string { return "tok" }, "test")
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// TestWebSocketUpgradeIsProxied is the regression guard for DeepSeek Harness's
// /api/remote.mux WebSocket multiplexer: a 101 must pass through and the
// hijacked connection must stay usable in both directions.
func TestWebSocketUpgradeIsProxied(t *testing.T) {
	front, _ := newTestProxy(t, "tok")

	frontURL, _ := url.Parse(front.URL)
	conn, err := net.DialTimeout("tcp", frontURL.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	request := fmt.Sprintf("GET /api/remote.mux HTTP/1.1\r\nHost: %s\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", frontURL.Host)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("echo = %q, want ping", echo)
	}
}
