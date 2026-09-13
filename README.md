# Cortex

Cortex is a self-hosted browser workspace and coding-agent interface.

## Command line

```sh
cortex version
cortex --version
cortex service status
```

Unknown commands and unsupported options fail with a non-zero exit status.
Run the binary without a subcommand to start the integrated server, or use
`cortex serve` where that compatibility alias is supported.

Cortex is a self-hosted, agent-first coding agent with a small Go backend and a Nift-built browser UI. It can run locally on a development machine or be hosted remotely, while OpenCode provides the underlying coding-agent execution loop.

## First version

- OpenCode-backed coding agent with streamed tool activity and response recovery.
- OpenCode-backed provider execution for OpenCode Zen, OpenRouter, OpenAI, Anthropic, Google AI and DeepSeek, with editable model IDs.
- Browser-selected workspaces under a configurable root.
- Multiple independent browser sessions across different workspaces.
- Settings popup with provider/model selection, masked API-key storage, and OpenCode OAuth reuse for ChatGPT Plus/Pro and GitHub Copilot.
- Copy session, Stop, Enter-to-run / Shift+Enter newline, tool status highlighting.
- Workspace-root confinement and isolated per-session OpenCode data.

## Install

GitHub Releases publish prebuilt Linux, macOS and Windows binaries. On Linux or macOS, the website installer installs to `~/.local/bin` by default without requiring `sudo`:

```sh
curl -fsSL https://crtx.dev/install.sh | sh
```

For a deliberate system-wide installation to `/usr/local/bin`:

```sh
curl -fsSL https://crtx.dev/install.sh | sudo sh -s -- --system
```

If `~/.local/bin` is not currently in `PATH`, the per-user installer prints the shell-profile line needed to add it. You can also set `CORTEX_INSTALL_DIR` for a custom per-user destination.

Go users can also install directly from the public module:

```sh
go install github.com/crtx-dev/cortex/cmd/cortex@latest
```

Cortex currently expects `opencode` to be installed separately and available in its `PATH`.

## Build and run

Cortex dogfoods [Nift](https://nift.dev) for its application frontend. `content/` and `templates/` are the source of truth; `public/` is generated and embedded into the Go binary. Build the frontend before compiling Go after changing frontend source:

```sh
nift build
go test ./...
go build -o cortex ./cmd/cortex
./cortex
```

`nift status` should report the three tracked frontend outputs as current before committing. Do not edit `public/` directly.

Cortex listens on `127.0.0.1:7331` and uses your home directory as the default workspace root. Use `--root ~/Repositories` when you want a tighter browser-visible boundary.

The listener is configured with `--host`/`--port` (or the legacy single-address `--listen`) and the `CORTEX_HOST`/`CORTEX_PORT` environment variables, in that precedence order:

```sh
cortex                        # 127.0.0.1:7331
cortex --port 7401            # 127.0.0.1:7401
CORTEX_PORT=7401 cortex       # 127.0.0.1:7401
cortex --host 0.0.0.0 --port 7401
```

Loopback (`127.0.0.1`) is recommended behind a reverse proxy. Binding `0.0.0.0` exposes Cortex on all IPv4 interfaces and should only be used deliberately. IPv6 hosts are accepted and bracketed automatically. Ports must be integers from 1 through 65535; an invalid or empty value fails rather than silently falling back.

> Remote binding is intentionally not the default. Cortex now requires browser authentication after first-run password setup, with optional TOTP and Google sign-in. For an Internet-facing deployment, TLS and normal host/reverse-proxy hardening are still recommended.

When Cortex is behind Caddy or nginx on the same host, enable proxy trust
explicitly and pin the external origin:

```sh
cortex --host 127.0.0.1 --port 7331 \
  --trust-proxy \
  --public-origin https://cortex.example.com
```

`--trust-proxy` accepts forwarding headers only from Cortex's direct loopback
peer. `--public-origin` pins Host and same-origin checks. Do not enable proxy
trust when clients can connect directly to the Cortex HTTP port.

Open **Settings**, choose a provider and model, then save the provider credential. API-key providers are injected only into the isolated OpenCode process.

For ChatGPT Plus/Pro or GitHub Copilot subscriptions, authenticate OpenCode once on the Cortex host:

```sh
opencode auth login --provider openai
opencode auth login --provider github-copilot
```

Cortex detects those OAuth credentials and copies the selected provider credential into the isolated session data when that subscription is used. Exact model availability remains determined by the installed OpenCode version and provider account.

## Run as a systemd user service

Run Cortex in the foreground with `cortex` or `cortex serve`. To keep it running without a terminal, install a per-user systemd unit:

```sh
cortex service install            # --host, --port, --root, --data, --public-origin, --trust-proxy accepted
cortex service install --port 7401  # install on 127.0.0.1:7401
cortex service status
cortex service logs               # or: cortex service logs --follow
cortex service restart
cortex service uninstall          # stops the service but keeps all Cortex data
```

`service install` records the resolved host and port directly in the unit command, so the selected listener survives login, restart and reboot. It accepts `--host`/`--port` (defaulting to `127.0.0.1:7331` or the current `CORTEX_HOST`/`CORTEX_PORT` values), plus the legacy single-address `--listen`, `--root`, `--data`, `--public-origin` and `--trust-proxy`.

The user unit is written to `~/.config/systemd/user/cortex.service` and managed with `systemctl --user` and `journalctl --user-unit cortex.service`. `service install` resolves the executable to a stable absolute path, refuses empty, relative or transient paths, and writes the unit atomically with a versioned integrity header. An existing unit that is not managed by Cortex is never overwritten or removed silently. Install is transactional: the prior managed unit bytes are preserved, prior systemd enablement and activity are inspected before mutation, only exactly-recreatable states are accepted (`enabled`, `enabled-runtime`, `disabled` × `active`, `inactive`; a start-limited `failed` unit is accepted because `reset-failed` clears it deterministically before start), and rollback reproduces the exact prior enablement and activity states, distinguishing persistent from runtime enablement. Masked, static, linked, generated, transient, dead, unknown, reloading and transitional states are refused before mutation — unmask or stop first. A byte-identical unit already enabled and active is a genuine no-op; an unchanged unit that is inactive, disabled or failed receives only the lifecycle steps needed (a failed unit is reset before start), and a changed configuration reloads systemd and restarts the service. Install clears any prior start-limit failure with `reset-failed` immediately before starting or restarting, so a crash-looped service can be reinstalled and recovered without manual `systemctl` intervention. Install also verifies readiness transactionally: after starting or restarting it polls the unit's active state and the public `/api/health` endpoint within a bounded deadline, and even the byte-identical enabled-and-active no-op must pass the same health verification before success is reported (with no rollback, because it makes no mutation). A service that immediately fails, never becomes active, times out, or answers health outside the `{"ok":true}` JSON contract is reported as an install failure and rolls the transaction back, so a foreign process occupying the configured port cannot impersonate Cortex merely by returning 2xx JSON. A failed fresh install is stopped and disabled while the unit is still loaded, then removed and systemd is reloaded, leaving a clean retryable state; a failed reinstall restores the previous managed unit and its exact enablement and activity. Lifecycle failures are reported by category — an invalid generated unit (for example one systemd cannot parse), a systemd reload failure, a reset-failed failure, a start/restart failure, or a health failure. `cortex service status` reports enabled/running state, PID, version, listen address, effective listener URL and a live health check, and exits nonzero when the service is failed or missing. The health check targets the public, read-only `GET /api/health` (a minimal `{"ok":true}` JSON response); the richer `/api/status` endpoint stays behind browser authentication.

`service install --system` (system-wide units) is a documented follow-up and is not yet supported; user mode is the default.

### Terminal, user-service and system-service execution

* **Terminal:** running `cortex` from your login shell inherits your session environment. `gh auth status` and other CLI tools work exactly as in your shell, including the GitHub token stored in your login keyring.
* **User service:** systemd user units run as your OS user but start with a minimal environment. Cortex resolves stable paths and preserves `HOME`, and when `~/.config/gh/hosts.yml` exists the unit records `GH_CONFIG_DIR` so the GitHub CLI keeps working. Because a user service runs inside your user session, it can still reach your login keyring; if that keyring is locked, run `gh auth login` once in the session first.
* **System service:** not yet supported. A future system-wide service would run under a dedicated account with no login keyring, so GitHub CLI authentication would need `gh auth login` for that account or an explicit `GH_TOKEN`/`GITHUB_TOKEN` in its environment.

Cortex's agent subprocesses inherit the host GitHub CLI configuration directory (`GH_CONFIG_DIR`) when `hosts.yml` exists, without copying any file or token; the token itself stays in your keyring. OpenCode's own configuration and session data remain isolated in per-run temporary directories.

### Persistence and lingering

The installed service runs independently of the terminal that launched it. Closing that terminal does not stop the service; `cortex service uninstall` (or `systemctl --user stop cortex.service`) is how you stop it deliberately.

The unit belongs to your OS user's systemd user manager, so it normally starts when that manager starts (your first login or boot, depending on the distribution). If you want Cortex to keep running after you log out, or to start at boot before any interactive login, the user manager itself must be allowed to run without a session — that is what *lingering* enables:

```sh
loginctl show-user "$USER" -p Linger
loginctl enable-linger "$USER"
```

Cortex never enables lingering automatically, because it changes what the host runs without a login session — enable it deliberately only when unattended operation is actually required. The recorded unit also contains the absolute executable path that was current at install time; moving or deleting that executable breaks the service until you reinstall.

### Historical workspaces

Cortex keeps the transcript and metadata of every conversation even when its historical workspace is no longer available. A workspace may be missing (the repository was moved or deleted), renamed, inaccessible, or outside the current `--root`. Cortex stores the recorded path verbatim and never silently replaces it with the current root.

The browser marks such conversations visibly as unavailable and disables **Run** for them; the transcript stays intact. To keep working with an old conversation, open the workspace picker and select a valid replacement — Run re-enables only after the new workspace passes the same strict root and symlink checks. No filesystem details beyond the path you already chose are shown. The execution boundary itself is unchanged: a missing, renamed, inaccessible, out-of-root or symlink-escaping workspace is never used for browsing or agent execution.

A session with no explicit workspace is a "Default workspace" session: Run is enabled and executes in the configured Cortex root (the `--root` value), and the conversation keeps the explicit empty default-root state rather than rewriting its stored workspace. Selecting a specific directory later still clears an incompatible OpenCode/provider session identifier as before.

## Headless administration

Use `cortex setup --password-file FILE`, `cortex config show --json`, and the `service` commands from provisioning agents. With the service stopped, `cortex reset --auth` or `cortex reset --all` requires the typed phrase `CORTEX AUTH` or `CORTEX ALL`; `--confirm` supplies the same phrase non-interactively. Resets retain timestamped backups, and full reset never removes workspace repositories.
