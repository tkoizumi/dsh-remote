package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/dsh"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/tailscale"
)

type statusOptions struct {
	dshPort   int
	proxyPort int
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var opts statusOptions
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port DeepSeek Harness listens on")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port the stable proxy listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	state, err := process.LoadState()
	if err != nil {
		return err
	}

	proxyAddr := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort)
	dshAddr := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.dshPort)
	dshPID := 0
	remoteURL := ""
	lanURL := ""
	tailnetHost := ""
	if state != nil {
		if state.ProxyAddr != "" {
			proxyAddr = state.ProxyAddr
		}
		if state.DSHAddr != "" {
			dshAddr = state.DSHAddr
		}
		dshPID = state.DSHPID
		remoteURL = state.RemoteURL
		lanURL = state.LANURL
		tailnetHost = state.TailnetHost
	}

	health, healthy := fetchHealth(proxyAddr)
	dshRunning := (state != nil && process.Alive(state.DSHPID)) || portOpen(dshAddr)
	proxyRunning := healthy

	fmt.Println()
	if dshRunning {
		fmt.Println("DSH: running")
		if dshPID > 0 {
			fmt.Printf("DSH PID: %d\n", dshPID)
		} else {
			fmt.Println("DSH PID: unknown (not started by a recorded dsh-remote run)")
		}
		fmt.Printf("DSH local address: %s\n", dshAddr)
	} else {
		fmt.Println("DSH: not running")
		fmt.Printf("DSH local address: %s (nothing is listening)\n", dshAddr)
	}
	fmt.Println()

	if proxyRunning {
		fmt.Println("Proxy: running")
		fmt.Printf("Proxy local address: %s\n", proxyAddr)
		fmt.Printf("Proxy token captured: %t\n", health.TokenCaptured)
	} else {
		fmt.Println("Proxy: not running")
		fmt.Printf("Proxy local address: %s (nothing is listening)\n", proxyAddr)
	}

	// Tailscale section: report only, never mutate here.
	ts, tsErr := tailscale.New()
	fmt.Println()
	fmt.Println("Tailscale:")
	switch {
	case tsErr != nil:
		fmt.Printf("unavailable: %v\n", tsErr)
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if tailnetHost == "" {
			if host, err := ts.DNSName(ctx); err == nil {
				tailnetHost = host
			} else {
				fmt.Printf("unavailable: %v\n", err)
			}
		}
		if tailnetHost != "" {
			fmt.Printf("https://%s\n", tailnetHost)
		}
		if target, err := ts.RootTarget(ctx); err == nil {
			expected := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort)
			switch {
			case target == expected:
				// ours
			case target == "":
				fmt.Println("serve: no handler is mounted at / (run `dsh-remote start`)")
			default:
				fmt.Printf("serve: / is mapped to %s, not the dsh-remote proxy at %s\n", target, expected)
			}
		}
	}

	fmt.Println()
	fmt.Println("Remote DSH:")
	if remoteURL != "" {
		fmt.Println(remoteURL)
	} else if tailnetHost != "" {
		fmt.Println("https://" + tailnetHost + proxy.BootstrapPath)
	} else {
		fmt.Println("unknown (Tailscale hostname not detected)")
	}
	if lanURL != "" {
		fmt.Println()
		fmt.Println("Local network DSH (no Tailscale required):")
		fmt.Println(lanURL)
	}
	fmt.Println()
	return nil
}

// fetchHealth queries the loopback health endpoint.
func fetchHealth(proxyAddr string) (proxy.Health, bool) {
	var health proxy.Health
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(proxyAddr + proxy.HealthPath)
	if err != nil {
		return health, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return health, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return health, false
	}
	return health, true
}

// portOpen reports whether something is accepting TCP connections at addr.
func portOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(addr, "http://"), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
