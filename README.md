# agent-sandbox

An Ubuntu-based, persistent desktop sandbox for coding agents. A root-owned
service exposes REST and MCP execution interfaces, while every requested command
and code snippet runs as a runtime-created, unprivileged user.

## Features

- Ubuntu 24.04 base image with common shell, network, archive, Git, and text tools.
- Node.js LTS and the newest stable Python installed from apt repositories.
- Globally available uv, plus user-writable npm and uv tool/Python directories.
- Optional `@playwright/cli` backed by CloakBrowser's Chromium, available
  through `exec` when browser support is enabled.
- Optional supervised Chrome remote debugging endpoint for `chrome-devtools-mcp`.
- Noto CJK fonts for correctly rendering Chinese, Japanese, and Korean text.
- Optional xpra/Xvfb desktop with an HTML client on port 14500.
- REST endpoints for foreground and background shell/JavaScript/Python tasks.
- MCP Streamable HTTP tools for executing, inspecting, listing, and killing tasks.
- A persistent, runtime-selected home directory.

Node.js and Python are installed into the image through NodeSource and
deadsnakes apt repositories. uv is installed globally as a standalone binary.
Playwright determines and installs Chromium's Ubuntu system dependencies during
the image build, so the repository does not maintain a duplicate browser
dependency list. `@playwright/cli`, CloakBrowser, and its Chromium binary are
downloaded into the persisted user home during first browser initialization
when `SANDBOX_BROWSER_DISABLED=false`, which is the default.

## Start

```sh
cp .env.example .env
docker compose pull
docker compose up -d
docker compose logs -f sandbox
```

## Configuration

### Local image build configuration

The provided Compose file uses `ghcr.io/devld/agent-sandbox:latest`. When
building a custom image from this repository, the following Docker build
arguments are available:

| Variable | Default | Description |
| --- | --- | --- |
| `UBUNTU_VERSION` | `24.04` | Ubuntu base image version. |
| `NODEJS_MAJOR` | `24` | Node.js major version installed from NodeSource. |
| `PYTHON_VERSION` | `3.14` | Python version installed from deadsnakes. |

### Runtime configuration

These variables configure the Compose deployment, container initialization, or
running services.

| Variable | Default | Scope | Description |
| --- | --- | --- | --- |
| `SANDBOX_USERNAME` | `agent` | Entrypoint, API, Compose | Unprivileged runtime username and persistent-home mount target. Changing it for an existing volume is discouraged. |
| `SANDBOX_UID` | `1000` | Entrypoint, API | UID used for the sandbox account and child processes. Must be a positive integer. |
| `SANDBOX_GID` | `1000` | Entrypoint, API | GID used for the sandbox account and child processes. Must be a positive integer. |
| `SANDBOX_CHOWN_HOME` | `true` | Entrypoint | Recursively assigns the persistent home to the configured UID/GID during first home initialization. |
| `SANDBOX_API_HOST` | `0.0.0.0` | API | Address on which the control-plane HTTP server listens. |
| `SANDBOX_API_PORT` | `3000` | API, Compose | API listen port when passed to the container. In the provided Compose file it selects the host port published to container port `3000`. |
| `SANDBOX_API_TOKEN` | Empty | API | Bearer token required by execution and MCP routes. Empty disables authentication. |
| `SANDBOX_BROWSER_DISABLED` | `false` | Entrypoint, MCP | When `true`, skips browser tooling initialization and omits the built-in Playwright guidance. Existing browser files remain unchanged. |
| `CHROME_REMOTE_DEBUGGING_ENABLED` | `false` | Entrypoint, Chrome, Playwright CLI | Starts a supervised Chrome with remote debugging enabled and configures Playwright CLI to use its CDP endpoint by default. Chrome is headed when xpra auto-start is enabled and headless otherwise. Requires browser tooling. |
| `CHROME_REMOTE_DEBUGGING_READY_TIMEOUT` | `30` | Entrypoint, Chrome | Seconds to wait for the Chrome DevTools endpoint during container startup. |
| `CHROME_REMOTE_DEBUGGING_USER_DATA_DIR` | `$HOME/.config/agent-sandbox/chrome-devtools-profile` | Chrome | Absolute path of the dedicated remote-debugging browser profile. |
| `EXEC_INLINE_OUTPUT_LIMIT_BYTES` | `65536` | API | Per-stream threshold after which complete stdout or stderr moves to an output file. |
| `EXEC_OUTPUT_DIR` | `/tmp/agent-sandbox/tasks` | API | Root directory for task output files and completion markers. |
| `EXEC_OUTPUT_TTL` | `12h` | API | Retention duration for completed task output files. Uses Go duration syntax. |
| `EXEC_CLEANUP_INTERVAL` | `10m` | API | Interval between expired-output cleanup passes. Uses Go duration syntax. |
| `XPRA_AUTO_START` | `false` | Entrypoint | Starts the xpra/Xvfb desktop before the API when set to `true`. |
| `XPRA_PORT` | `14500` | xpra, Compose | xpra HTML server port when passed to the container. In the provided Compose file it selects the host port published to container port `14500`. |
| `XPRA_LOG_FILE` | `$XDG_RUNTIME_DIR/xpra-$DISPLAY.log` | xpra | Overrides the xpra server log path. |
| `XPRA_READY_TIMEOUT` | `30` | xpra | Seconds to wait for the X11 socket and live xpra session. |
| `DISPLAY` | `:100` | Entrypoint, xpra, child processes | X11 display used by headed browser commands and the optional desktop. |
| `TZ` | `UTC` | Container, child processes | Container timezone passed through to executions. |

The Compose volume is mounted at `/home/$SANDBOX_USERNAME`. Changing the username
after creating the volume is discouraged: use a separate Compose project or
volume for each runtime identity. The home mount must allow execution because
npm and uv install user-owned tools and runtimes there; do not mount it with
`noexec`.

The image defaults to Node.js 24 LTS and Python 3.14, with the latest patch
release supplied by their apt repositories. Set the `NODEJS_MAJOR` or
`PYTHON_VERSION` Docker build arguments to choose another available release.
Changing either requires building a custom image.

The sandbox user's npm prefix is `$HOME/.local`, and `$HOME/.local/bin` is on
`PATH`. This makes `npm install -g` and `npx` usable without root. uv itself is
global, while `uv tool install`, `uv python install`, and `uvx` store mutable
state under the user's home and expose installed commands through the same
user-writable bin directory.

Set `SANDBOX_BROWSER_DISABLED=true` to skip installing Playwright CLI,
CloakBrowser, its Chromium binary, and the Playwright configuration into the
persistent home. The switch does not uninstall or modify an existing browser
installation. Enabling it later installs any missing browser tooling. It also
controls whether the built-in MCP server instructions advertise
`playwright-cli`; it does not control xpra or the browser system libraries
already included in the image.

`XPRA_AUTO_START=true` starts xpra before the API. `DISPLAY` is exported to all
agent executions and written to `/etc/profile.d/agent-sandbox-display.sh`.
`start-xpra` is idempotent and waits for both the X11 socket and a live xpra
session, avoiding browser startup races.

Set `CHROME_REMOTE_DEBUGGING_ENABLED=true` to start a separate Chrome instance
at `about:blank`. The supervisor restarts Chrome with bounded backoff when its
last window is closed or the browser crashes. `XPRA_AUTO_START` remains the
single switch that controls whether xpra starts: when it is `true`, Chrome runs
headed on the configured `DISPLAY` and can be operated through the xpra HTML
client; when it is `false`, Chrome runs headless without starting an X server.
Chrome uses the CloakBrowser executable recorded in the Playwright CLI
configuration. The dedicated user data directory is required by current Chrome
versions when command-line remote debugging is enabled. Before each supervised
launch, stale Chromium singleton locks from a stopped or replaced container are
removed from that dedicated profile. The supervisor and its CDP proxy run as
root so sandbox-user processes cannot terminate them, while xpra and Chrome are
always launched through `setpriv` as the unprivileged sandbox user.

The entrypoint also reconciles Playwright CLI's global configuration on every
startup. When Chrome remote debugging is enabled, it sets
`browser.cdpEndpoint` to the supervised endpoint at `127.0.0.1` so normal
`playwright-cli open`, navigation, snapshot, and interaction commands use that
browser and its persistent profile. When remote debugging is disabled, the
managed `cdpEndpoint` field is removed and Playwright CLI returns to launching
its own isolated browser sessions. The generated `~/.playwright/README.md`
lists every managed configuration field that should not be edited manually.

Chrome itself accepts remote debugging connections only on the container
loopback interface. The image therefore uses `socat` to bridge the same port
from the container network interface. Compose publishes that port only on host
loopback at `127.0.0.1:9222`. The Chrome DevTools Protocol has no authentication
and provides complete control over the browser; do not publish it on an
untrusted network.

Configure `chrome-devtools-mcp` to connect to the supervised browser:

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "command": "npx",
      "args": [
        "--yes",
        "chrome-devtools-mcp@latest",
        "--browser-url=http://127.0.0.1:9222"
      ]
    }
  }
}
```

Open `http://localhost:14500/` to access the xpra HTML client. Agent commands can
use the browser directly:

```sh
playwright-cli open https://example.com --headed
playwright-cli snapshot
playwright-cli screenshot
```

The initialized home points Playwright CLI's `browser.launchOptions.executablePath`
at CloakBrowser's downloaded Chromium instead of Playwright's bundled browser.
With remote debugging enabled, Playwright CLI attaches to the supervised
browser by default and shares its tabs, cookies, and other profile state. CLI
session shutdown disconnects from the attached browser; the supervisor retains
browser ownership. With remote debugging disabled, normal CLI sessions use
separate temporary profiles. In that mode, when xpra is running,
`playwright-cli open --headed` uses the same `DISPLAY` and its separate browser
window is also visible through the xpra HTML client.
Run `install-or-update-cloakbrowser` as the sandbox user at any time to update
the browser binary and refresh that path as one operation. Running a raw
`npx cloakbrowser update` command instead does not rewrite the Playwright CLI
config. The CloakBrowser npm wrapper and its Playwright runtime are resolved at
their latest stable tags in an ephemeral `npx` environment; no global
`cloakbrowser` command is installed, although npm retains its normal download
cache in the persistent home. Playwright CLI is installed globally at its
latest stable tag. These versions and CloakBrowser licensing cannot be
controlled through environment variables. Chromium's own sandbox is disabled
because the browser already runs as the unprivileged sandbox user inside the
container; the Docker runtime still needs to be configured as the outer
security boundary.

## Execution API

Authentication is disabled when `SANDBOX_API_TOKEN` is empty, which is the
default. When it is set, all execution routes require
`Authorization: Bearer $SANDBOX_API_TOKEN`. `/healthz` is always
unauthenticated.

```sh
curl -sS http://localhost:3000/exec \
  -H "Content-Type: application/json" \
  -d '{"script":"id && printf \"%s\\n\" \"$DISPLAY\""}'

curl -sS http://localhost:3000/exec \
  -H "Content-Type: application/json" \
  -d '{"mode":"python","script":"print(sum(range(10)))","foregroundWaitMs":0}'
```

`mode` is `shell` (the default), `javascript`, or `python`. `cwd` must be an
absolute path and defaults to the sandbox home. `foregroundWaitMs` defaults to
120000; zero returns a task ID immediately. Reaching that foreground wait does
not stop the task.

Every task is assigned a `t<number>` ID and enters the in-memory task table.
Tasks that finish during their foreground wait return their result directly and
are removed. Background tasks can be managed with:

```sh
curl -sS http://localhost:3000/exec/tasks

curl -sS http://localhost:3000/exec/tasks/t123

curl -sS -X DELETE http://localhost:3000/exec/tasks/t123
```

The list endpoint accepts `?status=running` or `?status=completed`. Killing a
task sends SIGTERM to its process group and SIGKILL one second later if needed.
There is intentionally no execution-time, concurrency, or output-file size
limit.

stdout and stderr are tracked independently and returned live by the status
endpoint. Each remains inline until `EXEC_INLINE_OUTPUT_LIMIT_BYTES` (64 KiB by
default), then its complete contents move to a sandbox-user-readable file below
`/tmp/agent-sandbox/tasks/<task-id>/`. Completed output files expire after
`EXEC_OUTPUT_TTL` (12 hours by default), with cleanup performed every
`EXEC_CLEANUP_INTERVAL` (10 minutes by default).

The first status query that observes a completed background task starts a
10-minute in-memory retention period. Repeated queries work during that period;
removing the task record does not remove its output files. Environment passed
to children is constructed from a small allowlist, so the root service's API
token is not exposed.

## MCP

Use `http://localhost:3000/mcp` as a Streamable HTTP MCP endpoint and send the
same bearer token as an Authorization header when `SANDBOX_API_TOKEN` is set.
It publishes:

- `exec`: run shell, JavaScript, or Python.
- `exec_status`: inspect status and live stdout/stderr.
- `exec_list`: list retained tasks without consuming completed results.
- `exec_kill`: terminate a task process group.

The transport is stateless, which works well for these request/response tools.

The MCP `initialize` response always starts with built-in server instructions.
When `SANDBOX_BROWSER_DISABLED=false`, those defaults include guidance that
browser automation is available through `playwright-cli` via `exec`; the
browser guidance is omitted when the switch is false. To append custom
instructions, mount a UTF-8 text file at the fixed path
`/etc/agent-sandbox/mcp-instructions.md`. The path is not configurable. If the
file is absent or empty, only the applicable built-in instructions are returned.

For example, a Compose override can inject a repository-managed file:

```yaml
services:
  sandbox:
    volumes:
      - ./mcp-instructions.md:/etc/agent-sandbox/mcp-instructions.md:ro
```

## User initialization and privilege boundary

At container startup, the entrypoint validates `SANDBOX_USERNAME`,
`SANDBOX_UID`, and `SANDBOX_GID`, creates the account, and initializes its home.
The `$HOME/.config/agent-sandbox/home-initialized` marker records base home
initialization, while
`$HOME/.config/agent-sandbox/browser-initialized` is written only after
Playwright CLI, CloakBrowser, its Chromium binary, and the Playwright
configuration are ready. This split allows a home first started with browser
support disabled to install it later, without changing an existing installation
when the switch is turned off. Independently of these markers, each startup
checks `.bashrc` and `.bash_profile` and appends the user-local PATH, npm/uv
directory configuration, and login-shell sourcing lines only when missing.
Existing shell configuration is preserved and the injection is not duplicated.

The image runs `tini` as PID 1, so Compose does not need `init: true`. The
standalone API/MCP control-plane process and optional Chrome supervisor run as
root-owned descendants and do not depend on Node.js or Python. Executions,
xpra, and Chrome are launched through `setpriv` with the configured UID, GID,
supplementary groups, home, and runtime directory. Because an empty API token
allows unauthenticated command execution, publish the API only on an isolated
trusted network or set a strong token. Do not mount the Docker socket, host
credentials, or other privileged devices into this container; root inside a
container is not a substitute for careful mount and network policy.
