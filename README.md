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
brew trust tkoizumi/tap
brew install dsh-remote
```

This works on macOS and on [Homebrew for Linux](https://docs.brew.sh/Homebrew-on-Linux)
(including Ubuntu). It installs a prebuilt binary, so it needs no Go toolchain;
Node.js/npm is still required at runtime for `npx`.

The `brew trust` step is required once. Homebrew refuses to load formulae or
casks from a non-official tap until it is trusted, and otherwise fails with:

```text
Error: Refusing to load cask tkoizumi/tap/dsh-remote from untrusted tap tkoizumi/tap.
```

Trust decisions persist in `~/.homebrew/trust.json` (or under `$XDG_CONFIG_HOME`).
To trust only this cask rather than the whole tap:

```bash
brew trust --cask tkoizumi/tap/dsh-remote
```

### Prebuilt binaries

Every [tagged release](https://github.com/tkoizumi/dsh-remote/releases) attaches
static binaries for Linux and macOS:

```text
dsh-remote_<version>_linux_amd64.tar.gz
dsh-remote_<version>_linux_arm64.tar.gz
dsh-remote_<version>_darwin_amd64.tar.gz
dsh-remote_<version>_darwin_arm64.tar.gz
```

Replace `<version>` with a release tag, for example:

```bash
ver=0.1.5
curl -fsSL -o dsh-remote.tar.gz \
  "https://github.com/tkoizumi/dsh-remote/releases/download/v${ver}/dsh-remote_${ver}_linux_amd64.tar.gz"
tar -xzf dsh-remote.tar.gz
install -m755 dsh-remote ~/.local/bin/dsh-remote
```

Each release also ships `checksums.txt`.

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

`dsh-remote start` checks this *before* it launches DeepSeek Harness, so a
missing grant fails immediately rather than leaving a half-started process. When
a terminal is available it offers to run the command for you:

```text
Tailscale Serve needs root or an operator.
Run `sudo tailscale set --operator=you` now? [y/N]
```

The question is asked on the controlling terminal, never on stdin, and the
default is no. With no terminal — for example under the systemd user service —
`dsh-remote` never attempts an unattended `sudo`; it prints the command and
exits nonzero instead.

### One-time DeepSeek API key setup

`dsh-remote` launches DeepSeek Harness; it does not hold a model credential
itself. DeepSeek Harness resolves `DEEPSEEK_API_KEY` on its own, in one fixed
order, and the file-based store is the layer to use here:

| Source | Wins over | Persists across restart |
| --- | --- | --- |
| The environment you launched in (`DEEPSEEK_API_KEY=… dsh`) | everything | no — and it shadows every other layer |
| The stored file `~/.dsh/.credentials.yaml` | both `.env` files | yes |
| `<invocation cwd>/.env` | `~/.dsh/.env` | yes, but the directory is not fixed |
| `~/.dsh/.env` | nothing | yes |

Store the key once through the UI, which writes it to
`~/.dsh/.credentials.yaml` (mode `0600`) under the reference
`DEEPSEEK_API_KEY`:

1. Start DeepSeek Harness and open the stable `/dsh` URL.
2. Go to **Settings → Models** and open the **DeepSeek** card.
3. Paste the key into **API key** and apply.

The first-run dialog offers the same DeepSeek step, so a fresh machine can be
set up without touching a terminal. A key saved there is used by the very next
request — no restart and no configuration edit — which is what makes key
rotation painless.

**Why not a `.env` file.** It works, but it is the wrong layer for a service:

- The *launch environment* is the highest-precedence source. A key exported in
  your shell profile is not a durable answer: a systemd user service does not
  read `~/.bashrc` or `~/.profile`, so the same machine behaves differently in
  the foreground and at boot. When it does apply, it wins for that whole run,
  cannot be overwritten from inside DeepSeek Harness, and makes the stored
  reference read-only — so a key you rotate in the UI appears to be ignored.
- A *project* `.env` is resolved from the invocation directory, and the unit
  `dsh-remote` installs sets no `WorkingDirectory=`, so "where the service was
  started" is not a stable place to keep a secret.
- `~/.dsh/.env` does work and survives reboots, and is a fine way to bootstrap a
  key before the first launch. It just loses to the stored file, which is
  private, reloadable, and rotatable without editing text.

Because the store is a file under the harness home, the key is read identically
in the foreground and under `dsh-remote install` — there is no environment
variable for a systemd unit to carry, and nothing for a reboot to lose. If
DeepSeek Harness later reports `MISSING_CREDENTIAL`, the reference is
unconfigured in every layer above; if it reports a key that is not the one you
expected, check for a `DEEPSEEK_API_KEY` left in the service's environment.

**In the Lima VM** the guest is a separate machine with its own harness home, so
it needs its own key even when the Mac's is already configured. Set it the same
way, from the guest: open `http://127.0.0.1:3081/dsh` and use the first-run
DeepSeek step or **Settings → Models**. It is stored in the guest's
`~/.dsh/.credentials.yaml` and survives VM restarts.

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
| `dsh-remote status` | Report DSH, proxy, and Tailscale state, why remote access is or is not available, and what to run next |
| `dsh-remote doctor` | Run end-to-end checks (port, upstream, proxy, token, Serve, supervision, orphans) and print a verdict |
| `dsh-remote reconcile` | Clear a leftover DeepSeek Harness that is holding the port, then start a fresh proxy |
| `dsh-remote stop` | Stop DSH and the proxy, restore the previous Serve mapping |
| `dsh-remote qr` | Print a QR code for the stable `/dsh` URL |
| `dsh-remote vm` | Run DeepSeek Harness in a Lima VM and serve it on this Mac |
| `dsh-remote install` | Install a systemd user service that starts at login/boot |
| `dsh-remote uninstall` | Remove that service |

Useful `start` flags:

- `--dsh-port` / `--proxy-port` — change the loopback ports (defaults 3080/3081).
- `--dsh-exec <path>` — run an already-installed `dsh` directly instead of via `npx`.
- `--lan` — also serve the stable URL on this host's local network address.
- `--lan-address <ip>` — bind a specific local address for `--lan` (default: detected).
- `--no-serve` — manage only the local proxy; you configure Tailscale Serve yourself.
- `--tailnet-host <name>` — override the detected Tailscale hostname.
- `--trusted-host <authority>` — extra authority to pass to DSH (repeatable).

### Local network access (no Tailscale on the client)

`--lan` adds a second listener on this host's local network address, so a laptop
on the same WiFi can use DSH **without Tailscale**:

```bash
dsh-remote start --lan
```

```text
Remote URL: https://ubuntu-server.tail6d6db9.ts.net/dsh
Local network URL: http://192.168.0.151:3081/dsh
```

The first is for your phone (anywhere, via Tailscale). The second is for a
browser on the same network — it needs no Tailscale at all. Both are served by
this host, so both keep working when the other device is asleep.

`--lan` binds only the named address, never `0.0.0.0`, and the address is added
to DSH's `--trusted-host` list so the `/api` fence accepts it. To have the
systemd service do this at boot:

```bash
dsh-remote install --lan
```

**Read the security note below before using `--lan`.** It is off by default.

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

The DeepSeek credential is not part of this dance. Because it lives in
`~/.dsh/.credentials.yaml` rather than in the service environment, the key
survives reboots, `systemctl --user restart`, and unit regeneration by
`install --force` with nothing extra to carry — see
[One-time DeepSeek API key setup](#one-time-deepseek-api-key-setup).

### When the proxy stops: supervision and leftover processes

The stable URL works only while the proxy is listening. If the proxy exits — a
closed terminal, a crash, a `SIGKILL` — both access paths stop at once, even
though DeepSeek Harness may still be perfectly healthy.

`dsh-remote install` is what makes that self-healing. The generated unit uses:

```ini
[Service]
Type=simple
Restart=on-failure
RestartSec=3
```

`dsh-remote status` always states whether that supervision is actually in place,
because a unit that is installed with `Restart=no`, or a user service without
lingering, does not recover:

```text
Remote access: UNAVAILABLE

Reason:
The proxy is gone, but DeepSeek Harness (pid 118695) that dsh-remote launched is
still holding port 3080. Both access paths go through the proxy, so the remote
URL is unreachable.
A plain restart would fail: port 3080 is already in use.

Service supervision:
systemd unit: not installed (/home/taka/.config/systemd/user/dsh-remote.service)
no automatic restart is configured

Suggested action:
Install a supervised service so this cannot persist:
    dsh-remote install --lan
For unattended starts after reboot, also run:
    sudo loginctl enable-linger $USER
```

That example is the failure mode worth understanding. DeepSeek Harness is started
in its **own process group**, so that Ctrl+C and `stop` can signal the whole
`npx` → `node` tree at once. The trade-off is that a proxy killed without a
chance to run its cleanup handler leaves that child running. The orphan keeps
port 3080, and every supervised restart then fails on the port conflict it
creates.

`start` handles this. Before launching anything it inspects the DeepSeek Harness
port, and when the holder is a DeepSeek Harness that a previous `dsh-remote` run
recorded in its own state file, it stops that process group and clears the stale
state:

```text
dsh-remote: stopping leftover DeepSeek Harness (pid 118695) still holding port 3080 from an earlier run
Starting DeepSeek Harness...
```

A DeepSeek Harness you started yourself — `dsh web` by hand, or on a port that
was never recorded in the state file — is never touched. `start` refuses with a
message telling you which pid holds the port instead.

`dsh-remote reconcile` does the same cleanup on demand and then starts a fresh
proxy. Use it when `status` names it:

```bash
dsh-remote reconcile          # asks before stopping anything
dsh-remote reconcile --yes    # unattended
dsh-remote reconcile --no-start  # clean up only
```

Both paths are safe to run repeatedly, and neither ever prints or moves the
DeepSeek Harness token.

### The persistent log

Every run appends to a rotating log, so a failure can still be explained after
the terminal is gone, the process is gone, and the machine has rebooted:

```text
~/.local/state/dsh-remote/dsh-remote.log        # mode 0600, 2 MiB, 2 rotations
```

`XDG_STATE_HOME` is honoured, and `DSH_REMOTE_STATE_FILE` moves the log next to
whatever state file you point it at. Each line is timestamped, which is the point
— whether the proxy died a minute ago or has been down for hours is written down:

```text
2026-10-08T13:26:49.371Z dsh-remote 0.1.0 starting: dsh port 3090, proxy port 3091, serve not managed (--no-serve)
2026-10-08T13:26:49.375Z dsh: launching …/dsh web --no-open --port 3090
2026-10-08T13:26:49.376Z dsh: started with pid 18
2026-10-08T13:26:50.831Z dsh web: http://127.0.0.1:3090/?token=REDACTED
2026-10-08T13:26:50.831Z ready: proxy on http://127.0.0.1:3091, dsh on http://127.0.0.1:3090
2026-10-08T13:26:53.424Z dsh-remote 0.1.0 starting: dsh port 3090, proxy port 3091, serve not managed (--no-serve)
2026-10-08T13:26:53.424Z preflight: stopping leftover DeepSeek Harness (pid 18) still holding port 3090 from an earlier run
```

Notice what the first run does **not** have: a `shutdown complete` line. That
absence is the signature of an unclean exit, and it is how you tell a killed
proxy from a stopped one without guessing. `status` and `doctor` always report
the log's path, size, age, and last entry:

```text
Diagnostic log:
/home/taka/.local/state/dsh-remote/dsh-remote.log
size: 1.7 KiB, last written: 4s ago
last entry: 2026-10-08T13:26:55.025Z ready: proxy on http://127.0.0.1:3081, dsh on http://127.0.0.1:3080
```

**The token is never written.** Two layers enforce that: the output relayed from
DeepSeek Harness is redacted by value (the token is stripped as soon as it has
been parsed), and every line logged also passes a shape-based backstop that
catches `?token=…`, `Set-Cookie: …`, and `token: …`. `status` and `doctor` have a
test asserting that neither prints a token, a cookie, or a token-bound redirect.

This is separate from the systemd journal, which is the systemd-level view
(restart reasons, exit codes, `Restart=` behaviour):

```bash
journalctl --user -u dsh-remote -n 200 --no-pager
```

The journal only survives a reboot when `/var/log/journal` exists, because
journald's default `Storage=auto` keeps logs in `/run` otherwise — many Ubuntu
and Debian images do not create that directory. The dsh-remote log above lives in
the state directory, so it persists regardless. `scripts/verify-supervision.sh`
checks both and tells you how to make the journal persistent if it is not.

### Installing and verifying supervision

Two scripts cover the install and the acceptance test.

**[`scripts/setup-supervision.sh`](scripts/setup-supervision.sh)** installs the
supervised service in the order that avoids the port-conflict loop:

```bash
bash scripts/setup-supervision.sh --lan   # or without --lan for Tailscale only
```

It stops the foreground instance first, confirms ports 3080/3081 are actually
free, runs `install --force`, enables lingering, restarts the unit, and finishes
with a `doctor` verdict. Safe to re-run.

The ordering is the point. A foreground `start` owns both ports; installing and
enabling the unit first makes the service fail to bind, and `Restart=on-failure`
then loops on a conflict it can never clear. (A build old enough to lack orphan
reclamation cannot clear it at all — 0.1.9 and earlier.)

**[`scripts/verify-supervision.sh`](scripts/verify-supervision.sh)** then proves
the recovery path rather than trusting it:

```bash
scripts/verify-supervision.sh --no-reboot
```

It checks the unit and its restart policy, confirms the proxy and `/dsh`
redirect, then `SIGKILL`s the proxy's own pid and verifies systemd brings it back
and that no leftover DeepSeek Harness survives. Without `--no-reboot` it offers
to reboot the machine; reconnect afterwards and re-run with `--no-reboot` to
confirm the service came up on its own.

### Why the unit uses `Restart=on-failure`

Both branches are deliberate, and both are covered by tests:

- A failed start exits non-zero — a taken port, a missing `npx`, an unreachable
  Tailscale — so systemd retries.
- An intentional `systemctl --user stop` exits 0, so systemd does not fight you
  by restarting a service you just stopped.

`Restart=always` would restart on the second case too; `on-failure` is what keeps
`stop` meaningful while still recovering every unexpected exit.

### Running DeepSeek Harness in a VM (macOS)

`dsh-remote vm` runs DeepSeek Harness inside a persistent
[Lima](https://lima-vm.io) Linux VM on your Mac and serves the same stable `/dsh`
URL on your Mac's loopback:

```bash
dsh-remote vm start
```

```text
Open:  http://127.0.0.1:3081/dsh
Shell: dsh-remote vm shell --name dsh
Stop:  dsh-remote vm stop --name dsh
```

The first run creates the VM (this downloads an image and can take a few
minutes), installs Node.js and DeepSeek Harness inside it, copies this
`dsh-remote` binary in, and enables a systemd user service that runs
`dsh-remote start --no-serve`. Lima forwards the guest's loopback ports to the
host's loopback, so the URL is an ordinary `127.0.0.1` address and DeepSeek
Harness trusts it without any extra configuration.

| Command | What it does |
| --- | --- |
| `dsh-remote vm start` | Create or start the VM, install/refresh the service, report the URL |
| `dsh-remote vm stop` | Stop DeepSeek Harness in the VM (`--vm` also shuts the VM down) |
| `dsh-remote vm shell` | Open a shell (or run a command) in the VM |
| `dsh-remote vm status` | Report the VM state and whether the URL is answering |

`vm start` flags: `--name` (default `dsh`), `--cpus`, `--memory`, `--disk`,
`--mount` (host directory shared writable, default `~/src`), `--config` (use
your own Lima YAML), `--trusted-host` (repeatable), and `--proxy-port`.

Requirements: macOS. If [Lima](https://lima-vm.io) is missing, `dsh-remote vm
start` offers to run `brew install lima` on the terminal (the default is no);
with no terminal it prints the command and exits instead of installing
unattended. No Tailscale is installed or used inside the VM, and nothing in the
VM is reachable from your network: the forward is guest loopback to Mac
loopback.

`examples/lima/dsh.yaml` is the same VM as a plain Lima config, for reading and
hand-editing; point `vm start --config` at it if you prefer to own the file.

To reach that VM from your phone, run Tailscale on the Mac (not in the VM) and
point `tailscale serve` at the forwarded port. The browser then arrives under
your Mac's tailnet name, so tell the VM to trust that authority:

```bash
dsh-remote vm start --trusted-host <your-mac>.ts.net
tailscale serve --bg --yes --set-path=/ http://127.0.0.1:3081
```

Tailscale stays entirely on the Mac; the VM only learns the authority it must
accept.

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

### `--lan` lowers that boundary, deliberately

By default the proxy binds `127.0.0.1` only, so the sole way in is Tailscale
Serve — and the gate is tailnet membership, authenticated and encrypted.

`--lan` adds a listener on your local network address over **plain HTTP**, and
`/dsh` is an unauthenticated token dispenser: it hands a working DSH session to
anyone who can reach it. On a home network you control that may be an acceptable
trade. On a shared, guest, campus or café network it is not — anyone on that
network could open `http://<your-ip>:3081/dsh` and get in.

If you use `--lan`, prefer `--lan-address` with a firewall rule that restricts
port 3081 to the specific device you intend to use, and never enable it on a
network you do not trust. It is off by default for this reason.

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
- state-file round trips and the guarantee that no token is written,
- port attribution through `/proc` (own child vs. an unrelated holder),
- zombie detection, so a killed-but-unreaped child does not stall `stop` or
  block `start`,
- reclaiming an orphaned DSH on start, and refusing to touch one that
  dsh-remote did not launch,
- the installed unit's restart policy and `--no-serve` intent,
- the persistent log: timestamping, rotation, reopening across runs, and the
  guarantee that neither a token nor a cookie value reaches the file,
- `status` and `doctor` classification of the incident state, including a check
  that neither ever prints a token or cookie.

To smoke-test against a real DSH without Tailscale:

```bash
dsh-remote start --dsh-exec "$(command -v dsh)" --no-serve --no-qr
curl -i http://127.0.0.1:3081/dsh
```

For the systemd-side acceptance test, see
[Installing and verifying supervision](#installing-and-verifying-supervision).

## Repository layout

```text
dsh-remote/
  cmd/dsh-remote/        CLI: start, status, doctor, reconcile, stop, qr, vm, install
  internal/dsh/          launch DSH, parse the startup token
  internal/proxy/        stable loopback proxy and /dsh redirect
  internal/tailscale/    hostname detection and Serve management
  internal/process/      port checks, liveness (including zombies), run state file
  internal/socket/       attribute a loopback port to the process holding it
  internal/systemd/      read-only supervision state for diagnostics
  internal/logging/      rotating persistent log with credential redaction
  internal/lima/         Lima VM control for `dsh-remote vm`
  internal/qr/           terminal QR rendering
  scripts/               systemd supervision install and acceptance tests
```

## License

MIT
