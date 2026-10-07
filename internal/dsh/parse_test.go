package dsh

import "testing"

func TestParseStartupToken(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantToken string
		wantOK    bool
	}{
		{
			name:      "documented format",
			line:      "dsh web: http://127.0.0.1:3080/?token=ykUIFiyl2GcfrTIaUAth_iT7l_yZZkKhjvEABM6WxAo",
			wantToken: "ykUIFiyl2GcfrTIaUAth_iT7l_yZZkKhjvEABM6WxAo",
			wantOK:    true,
		},
		{
			name:      "with LAN suffix",
			line:      "dsh web: http://127.0.0.1:3080/?token=abc123 (LAN: http://192.168.0.5:3080/?token=abc123)",
			wantToken: "abc123",
			wantOK:    true,
		},
		{
			name:      "https and non default port",
			line:      "dsh web: https://box.example.ts.net:8443/?token=Zm9vLWJhci16",
			wantToken: "Zm9vLWJhci16",
			wantOK:    true,
		},
		{
			name:      "plain URL without dsh prefix",
			line:      "listening on http://127.0.0.1:3080/?token=plaintoken",
			wantToken: "plaintoken",
			wantOK:    true,
		},
		{
			name:      "ansi colour codes",
			line:      "\x1b[32mdsh web: http://127.0.0.1:3080/?token=coloured\x1b[0m",
			wantToken: "coloured",
			wantOK:    true,
		},
		{
			name:      "token with characters needing encoding",
			line:      "dsh web: http://127.0.0.1:3080/?token=a%2Bb%2Fc%3D",
			wantToken: "a+b/c=",
			wantOK:    true,
		},
		{
			name:   "empty token value",
			line:   "dsh web: http://127.0.0.1:3080/?token=",
			wantOK: false,
		},
		{
			name:   "no token parameter",
			line:   "dsh web: http://127.0.0.1:3080/",
			wantOK: false,
		},
		{
			name:   "unrelated output",
			line:   "Server ready",
			wantOK: false,
		},
		{
			name:   "empty input",
			line:   "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseStartupToken(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (token=%q)", ok, tt.wantOK, got.Token)
			}
			if ok && got.Token != tt.wantToken {
				t.Fatalf("token = %q, want %q", got.Token, tt.wantToken)
			}
		})
	}
}

// TestParseStartupTokenPrefersAnnouncement guards the case where both a
// canonical line and an incidental URL appear in the same chunk.
func TestParseStartupTokenPrefersAnnouncement(t *testing.T) {
	text := "helper saw http://example.com/?token=wrong\n" +
		"dsh web: http://127.0.0.1:3080/?token=right\n"
	got, ok := ParseStartupToken(text)
	if !ok {
		t.Fatal("expected a token")
	}
	if got.Token != "right" {
		t.Fatalf("token = %q, want %q", got.Token, "right")
	}
}

func TestSpecArgs(t *testing.T) {
	spec := Spec{Port: 3080, TrustedHosts: []string{"ubuntu-server.tail6d6db9.ts.net"}}
	got := spec.Args()
	want := []string{"web", "--no-open", "--port", "3080", "--trusted-host", "ubuntu-server.tail6d6db9.ts.net"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

func TestSpecArgsSkipsEmptyTrustedHosts(t *testing.T) {
	spec := Spec{Port: 3080, TrustedHosts: []string{"", "host"}}
	got := spec.Args()
	if len(got) != 6 {
		t.Fatalf("args = %v, want the empty trusted host to be dropped", got)
	}
}
