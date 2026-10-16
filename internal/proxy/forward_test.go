package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestForwarderProxiesAndPreservesHost covers the contract a dashboard behind
// the LAN listener depends on: the path reaches the backend unchanged and the
// browser's Host survives, because dashboards build absolute URLs from it.
func TestForwarderProxiesAndPreservesHost(t *testing.T) {
	var gotHost, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.Header().Set("X-Backend", "dashboard")
		_, _ = io.WriteString(w, "dashboard body")
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := NewForwarder(upstreamURL)
	if forwarder.Upstream() != upstreamURL.String() {
		t.Fatalf("Upstream = %q, want %q", forwarder.Upstream(), upstreamURL)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = forwarder.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = forwarder.Shutdown(ctx)
	})

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/assets/app.js", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "192.168.0.151:9119"
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/assets/app.js" {
		t.Errorf("backend path = %q, want /assets/app.js", gotPath)
	}
	if gotHost != "192.168.0.151:9119" {
		t.Errorf("backend Host = %q, want the client Host preserved", gotHost)
	}
	if resp.Header.Get("X-Backend") != "dashboard" {
		t.Errorf("backend response header was not passed through")
	}
	if string(body) != "dashboard body" {
		t.Errorf("body = %q, want the backend body", body)
	}
}

// TestForwarderBadGatewayIsClear keeps a dead dashboard from looking like a
// dsh-remote crash.
func TestForwarderBadGatewayIsClear(t *testing.T) {
	dead, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	forwarder := NewForwarder(dead)

	rec := httptest.NewRecorder()
	forwarder.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	// The marker is what lets `status`/`doctor` tell a dead backend apart from
	// a 502 the dashboard returned itself.
	if got := rec.Header().Get(UpstreamUnreachableHeader); got != UpstreamUnreachableValue {
		t.Fatalf("%s = %q, want %q", UpstreamUnreachableHeader, got, UpstreamUnreachableValue)
	}
}
