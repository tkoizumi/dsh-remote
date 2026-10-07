# dsh-remote

One stable Tailscale URL for a locally running DeepSeek Harness (`dsh web`).

```
https://ubuntu-server.tail6d6db9.ts.net/dsh   ← bookmark this, forever
```

`dsh-remote` launches DeepSeek Harness, reads the bootstrap token it prints,
and keeps redirecting your one bookmarked URL to that token — even after every
restart generates a new one. No SSH port forwarding, no copying fresh tokens,
no browser extension, no changes to DeepSeek Harness.

## Why this exists

Running DSH remotely from a phone normally means dealing with two annoyances:

- `dsh web` prints a URL like `http://127.0.0.1:3080/?token=<generated>` and
  generates a **new token on every restart**.
- Reaching a loopback-only server from a phone usually means an **SSH tunnel**
  that you have to re-create constantly.

`dsh-remote` turns that into a single stable HTTPS URL on your tailnet. The
phone only ever needs to know `/dsh`.

## Installation

Requires Node.js/npm (for `npx`) and an authenticated Tailscale. Go 1.24+ is
only needed to build from source.

### Homebrew

```bash
brew tap tkoizumi/tap
brew install dsh-remote
```

This works on macOS and on [Homebrew for Linux](https://docs.brew.sh/Homebrew-on-Linux)
(including Ubuntu). The formula builds from source and declares `go` (build)
and `node` as dependencies.

### Go

```bash
go install github.com/tkoizumi/dsh-remote/cmd/dsh-remote@latest
```

Or build locally:

```bash
git clone https://github.com/tkoizumi/dsh-remote
cd dsh-remote
go build -o dsh-remote ./cmd/dsh-remote
```

### One-time Tailscale setup

Tailscale Serve needs root or an operator. Enable it once so `dsh-remote` can
run as your normal user (and under a user systemd service):

```bash
sudo tailscale set --operator=$USER
```

## Usage

```bash
dsh-remote start
```

```text
Starting DeepSeek Harness...
dsh web: http://127.0.0.1:3080/?token=REDACTED
DSH listening on 127.0.0.1:3080
DSH PID: 31761
Proxy listening on 127.0.0.1:3081
Remote URL: https://ubuntu-server.example.ts.net/dsh
Scan this QR code from your phone:
<terminal QR code>
```

Then open or scan:

```text
https://<tailscale-host>/dsh
```

`start` runs in the foreground, relays DeepSeek Harness's own log output (with
the token redacted), and stops everything cleanly on Ctrl+C.

| Command | What it does |
| --- | --- |
| `dsh-remote start` | Launch DSH, the stable proxy, and Tailscale Serve |
| `dsh-remote status` | Report DSH, proxy, and Tailscale state |
| `dsh-remote stop` | Stop DSH and the proxy, restore the previous Serve mapping |
| `dsh-remote qr` | Print a QR code for the stable `/dsh` URL |
| `dsh-remote install` | Install a systemd user service that starts at login/boot |
| `dsh-remote uninstall` | Remove that service |

Useful `start` flags:

- `--dsh-port` / `--proxy-port` — change the loopback ports (defaults 3080/3081).
- `--dsh-exec <path>` — run an already-installed `dsh` directly instead of via `npx`.
- `--no-serve` — manage only the local proxy; you configure Tailscale Serve yourself.
- `--tailnet-host <name>` — override the detected Tailscale hostname.
- `--trusted-host <authority>` — extra authority to pass to DSH (repeatable).

### Surviving reboot

```bash
dsh-remote install
```

This writes `~/.config/systemd/user/dsh-remote.service` and enables it. For the
service to come up before you log in, allow lingering once:

```bash
sudo loginctl enable-linger $USER
```

After a reboot: Tailscale comes up, `dsh-remote` starts, it launches DSH,
captures the **newly generated** token, and the same bookmarked `/dsh` URL
works again.

## Architecture

```text
iPhone / laptop
      │
      │ Tailscale
      ▼
stable HTTPS URL
      │
      ▼
dsh-remote :3081
      │
      ├── /dsh → redirect using current token
      │
      ▼
DSH :3080
```

Concretely:

1. `dsh-remote start` runs `npx --yes @deepseek-ai/dsh web --no-open --port 3080`
   as a child process and scans its stdout/stderr for the startup line:
   `dsh web: http://127.0.0.1:3080/?token=...`
2. The token is held **in memory only**.
3. A tiny reverse proxy listens on `127.0.0.1:3081`. `GET /dsh` answers
   `302 Found` with `Location: /?token=<current token>`. Every other request is
   proxied to `127.0.0.1:3080`, WebSocket upgrades included.
4. Tailscale Serve maps the root of your node to the proxy:
   `tailscale serve --bg --yes --set-path=/ http://127.0.0.1:3081`.
   Funnel is never used.

```text
stable remote URL
    ↓
dsh-remote
    ↓
current DSH token
    ↓
DSH
```

### Two DeepSeek Harness details that shaped the design

These were confirmed by inspecting and exercising the installed DSH, and they
are why the implementation looks the way it does:

- **The token exchange only happens at pathname `/`.** DSH accepts the bootstrap
  token only on `GET /` and then replies `303` to `./` with an
  **authority-bound** `HttpOnly` cookie. That is why `dsh-remote` mounts the
  proxy at the **root** of the Tailscale endpoint and why `/dsh` redirects to
  `/?token=...` on the same authority rather than to a subpath.
- **The cookie is signed for the `Host` authority**, and the `/api` fence only
  accepts loopback or explicitly trusted authorities. So:
  - the proxy **preserves the original `Host` header** when forwarding, and
  - `dsh-remote` passes the Tailscale name to DSH as
    `--trusted-host <tailnet-host>`, otherwise the browser UI loads but every
    `/api` call is rejected with `403`.

DSH also uses a WebSocket multiplexer at `/api/remote.mux`; the proxy passes
`101 Switching Protocols` through untouched.

## Security

- DSH stays bound to `127.0.0.1`.
- `dsh-remote` stays bound to `127.0.0.1`; it can only listen on loopback, and
  that host is deliberately not configurable.
- Tailscale is the network boundary. The service is reachable only inside your
  tailnet.
- **Tailscale Funnel is not used.**
- DSH's authentication is **not** disabled or replaced. `dsh-remote` does not
  invent a second password system; it automates the existing token bootstrap.
- The DSH token is kept in memory and is not persisted to disk. The small state
  file under `$XDG_STATE_HOME/dsh-remote/state.json` records process ids and the
  Serve mapping only. DSH's startup line is redacted when relayed to your
  terminal.
- The proxy's health endpoint (`/__dsh_remote/health`) is loopback-only and
  never returns the token.

### Tailscale Serve ownership

`dsh-remote` records whatever was mapped at `/` before it started. On `stop` it
restores that mapping, so an existing `tailscale serve /` configuration is not
silently lost.

If there was **no** previous root mapping, `dsh-remote` leaves its own mapping
in place rather than running `tailscale serve reset`, which would wipe unrelated
Serve configuration. Remove it deliberately with `tailscale serve set-config` or
by resetting Serve yourself. It never touches Funnel.

## Development

```bash
go build ./...
go vet ./...
go test -race ./...
```

The tests do not require a real DSH or Tailscale installation. They cover:

- parsing the startup URL/token (including the documented
  `dsh web: http://127.0.0.1:3080/?token=ykUIFiyl...` line),
- `/dsh` redirect behavior, including before a token is captured,
- reverse-proxy forwarding (path, query, and preserved `Host`),
- WebSocket `101` upgrade passthrough,
- a DSH child exiting unexpectedly,
- signal and whole-process-group cleanup,
- state-file round trips and the guarantee that no token is written.

To smoke-test against a real DSH without Tailscale:

```bash
dsh-remote start --dsh-exec "$(command -v dsh)" --no-serve --no-qr
curl -i http://127.0.0.1:3081/dsh
```

## Repository layout

```text
dsh-remote/
  cmd/dsh-remote/        CLI: start, status, stop, qr, install
  internal/dsh/          launch DSH, parse the startup token
  internal/proxy/        stable loopback proxy and /dsh redirect
  internal/tailscale/    hostname detection and Serve management
  internal/process/      port checks and the run state file
  internal/qr/           terminal QR rendering
```

## License

MIT
