#!/usr/bin/env bash
# Literal shell initialization snippets are expanded later by the sandbox shell.
# shellcheck disable=SC2016,SC2317
set -Eeuo pipefail

sandbox_user="${SANDBOX_USERNAME:-agent}"
sandbox_uid="${SANDBOX_UID:-1000}"
sandbox_gid="${SANDBOX_GID:-1000}"
sandbox_home="/home/${sandbox_user}"
display="${DISPLAY:-:100}"
display_number="${display#:}"
display_number="${display_number%%.*}"
browser_disabled="${SANDBOX_BROWSER_DISABLED:-false}"
chrome_remote_debugging_enabled="${CHROME_REMOTE_DEBUGGING_ENABLED:-false}"

if [[ ! "${sandbox_user}" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || [[ "${sandbox_user}" == "root" ]]; then
  printf 'SANDBOX_USERNAME must be a valid non-root Linux username\n' >&2
  exit 64
fi
if [[ ! "${sandbox_uid}" =~ ^[0-9]+$ ]] || (( sandbox_uid == 0 )); then
  printf 'SANDBOX_UID must be a positive integer\n' >&2
  exit 64
fi
if [[ ! "${sandbox_gid}" =~ ^[0-9]+$ ]] || (( sandbox_gid == 0 )); then
  printf 'SANDBOX_GID must be a positive integer\n' >&2
  exit 64
fi
browser_disabled="${browser_disabled,,}"
if [[ "${browser_disabled}" != "true" && "${browser_disabled}" != "false" ]]; then
  printf 'SANDBOX_BROWSER_DISABLED must be true or false\n' >&2
  exit 64
fi
chrome_remote_debugging_enabled="${chrome_remote_debugging_enabled,,}"
if [[ "${chrome_remote_debugging_enabled}" != "true" && "${chrome_remote_debugging_enabled}" != "false" ]]; then
  printf 'CHROME_REMOTE_DEBUGGING_ENABLED must be true or false\n' >&2
  exit 64
fi
if [[ "${chrome_remote_debugging_enabled}" == "true" && "${browser_disabled}" == "true" ]]; then
  printf 'CHROME_REMOTE_DEBUGGING_ENABLED requires SANDBOX_BROWSER_DISABLED=false\n' >&2
  exit 64
fi
export CHROME_REMOTE_DEBUGGING_ENABLED="${chrome_remote_debugging_enabled}"

if ! getent group "${sandbox_gid}" >/dev/null; then
  groupadd --gid "${sandbox_gid}" "${sandbox_user}"
fi
group_name="$(getent group "${sandbox_gid}" | cut -d: -f1)"

if id "${sandbox_user}" >/dev/null 2>&1; then
  actual_uid="$(id -u "${sandbox_user}")"
  if [[ "${actual_uid}" != "${sandbox_uid}" ]]; then
    printf 'existing user %s has uid %s, expected %s\n' "${sandbox_user}" "${actual_uid}" "${sandbox_uid}" >&2
    exit 65
  fi
else
  existing_user="$(getent passwd "${sandbox_uid}" | cut -d: -f1 || true)"
  if [[ -n "${existing_user}" ]]; then
    if (( sandbox_uid < 1000 )); then
      printf 'SANDBOX_UID %s is assigned to system user %s and cannot be reused\n' "${sandbox_uid}" "${existing_user}" >&2
      exit 65
    fi
    usermod --login "${sandbox_user}" --home "${sandbox_home}" "${existing_user}"
    printf 'renamed pre-existing uid %s account %s to %s\n' "${sandbox_uid}" "${existing_user}" "${sandbox_user}"
  else
    useradd --uid "${sandbox_uid}" --gid "${sandbox_gid}" --home-dir "${sandbox_home}" --shell /bin/bash "${sandbox_user}"
  fi
fi

install -d -m 0750 -o "${sandbox_user}" -g "${group_name}" "${sandbox_home}"
install -d -m 0700 -o "${sandbox_user}" -g "${group_name}" "/run/user/${sandbox_uid}"

touch "${sandbox_home}/.bashrc" "${sandbox_home}/.bash_profile"
chown "${sandbox_uid}:${sandbox_gid}" "${sandbox_home}/.bashrc" "${sandbox_home}/.bash_profile"
chmod 0644 "${sandbox_home}/.bashrc" "${sandbox_home}/.bash_profile"

if ! grep -Fqx 'export PATH="${HOME}/.local/bin:${PATH}"' "${sandbox_home}/.bashrc"; then
  printf '\n%s\n' 'export PATH="${HOME}/.local/bin:${PATH}"' >>"${sandbox_home}/.bashrc"
fi
if ! grep -Fqx 'export NPM_CONFIG_PREFIX="${HOME}/.local"' "${sandbox_home}/.bashrc"; then
  printf '%s\n' 'export NPM_CONFIG_PREFIX="${HOME}/.local"' >>"${sandbox_home}/.bashrc"
fi
if ! grep -Fqx 'export UV_TOOL_BIN_DIR="${HOME}/.local/bin"' "${sandbox_home}/.bashrc"; then
  printf '%s\n' 'export UV_TOOL_BIN_DIR="${HOME}/.local/bin"' >>"${sandbox_home}/.bashrc"
fi
if ! grep -Fqx 'export UV_PYTHON_INSTALL_DIR="${HOME}/.local/share/uv/python"' "${sandbox_home}/.bashrc"; then
  printf '%s\n' 'export UV_PYTHON_INSTALL_DIR="${HOME}/.local/share/uv/python"' >>"${sandbox_home}/.bashrc"
fi
if ! grep -Fqx 'source "${HOME}/.bashrc"' "${sandbox_home}/.bash_profile"; then
  cat >>"${sandbox_home}/.bash_profile" <<'EOF'

if [[ -f "${HOME}/.bashrc" ]]; then
  source "${HOME}/.bashrc"
fi
EOF
fi

state_dir="${sandbox_home}/.config/agent-sandbox"
install -d -m 0755 -o "${sandbox_user}" -g "${group_name}" \
  "${sandbox_home}/.config" \
  "${state_dir}"

home_marker="${state_dir}/home-initialized"
if [[ ! -e "${home_marker}" ]]; then
  if [[ "${SANDBOX_CHOWN_HOME:-true}" == "true" ]]; then
    chown -R "${sandbox_uid}:${sandbox_gid}" "${sandbox_home}"
  fi

  install -d -m 0755 -o "${sandbox_user}" -g "${group_name}" \
    "${sandbox_home}/.local/bin" \
    "${sandbox_home}/.local/lib" \
    "${sandbox_home}/.local/share"

  setpriv --reuid="${sandbox_uid}" --regid="${sandbox_gid}" --init-groups -- \
    touch "${home_marker}"
  printf 'initialized sandbox home for %s (%s:%s)\n' "${sandbox_user}" "${sandbox_uid}" "${sandbox_gid}"
else
  printf 'sandbox home for %s is already initialized; skipping home bootstrap\n' "${sandbox_user}"
fi

browser_marker="${state_dir}/browser-initialized"
if [[ "${browser_disabled}" == "false" ]]; then
  if [[ ! -e "${browser_marker}" ]]; then
    install -d -m 0755 -o "${sandbox_user}" -g "${group_name}" "${sandbox_home}/.playwright"
    install -m 0644 -o "${sandbox_user}" -g "${group_name}" \
      /etc/agent-sandbox/playwright-cli.config.json \
      "${sandbox_home}/.playwright/cli.config.json"

    setpriv --reuid="${sandbox_uid}" --regid="${sandbox_gid}" --init-groups -- \
      env \
        HOME="${sandbox_home}" \
        USER="${sandbox_user}" \
        LOGNAME="${sandbox_user}" \
        PATH="${sandbox_home}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
        NPM_CONFIG_PREFIX="${sandbox_home}/.local" \
        UV_TOOL_BIN_DIR="${sandbox_home}/.local/bin" \
        UV_PYTHON_INSTALL_DIR="${sandbox_home}/.local/share/uv/python" \
        bash --noprofile --norc -Eeuo pipefail -c '
          npm install --global "@playwright/cli@latest"
          install-or-update-cloakbrowser
          node --version
          python --version
          uv --version
          uvx --version
          playwright-cli --version
        '

    setpriv --reuid="${sandbox_uid}" --regid="${sandbox_gid}" --init-groups -- \
      touch "${browser_marker}"
    printf 'initialized browser tooling for %s\n' "${sandbox_user}"
  else
    printf 'browser tooling for %s is already initialized; skipping browser bootstrap\n' "${sandbox_user}"
  fi
else
  printf 'browser tooling initialization is disabled; existing browser files are unchanged\n'
fi

cat >/etc/profile.d/agent-sandbox-display.sh <<EOF
export DISPLAY='${display}'
export XDG_RUNTIME_DIR='/run/user/${sandbox_uid}'
EOF
chmod 0644 /etc/profile.d/agent-sandbox-display.sh

export SANDBOX_HOME="${sandbox_home}"
export SANDBOX_UID="${sandbox_uid}"
export SANDBOX_GID="${sandbox_gid}"
export HOME="/root"
export DISPLAY="${display}"
export XDG_RUNTIME_DIR="/run/user/${sandbox_uid}"

if [[ "${XPRA_AUTO_START:-false}" == "true" || "${chrome_remote_debugging_enabled}" == "true" ]]; then
  setpriv --reuid="${sandbox_uid}" --regid="${sandbox_gid}" --init-groups -- \
    env \
      HOME="${sandbox_home}" \
      USER="${sandbox_user}" \
      LOGNAME="${sandbox_user}" \
      DISPLAY="${DISPLAY}" \
      XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR}" \
      XPRA_PORT="${XPRA_PORT:-14500}" \
      start-xpra
fi

if [[ "${chrome_remote_debugging_enabled}" == "false" ]]; then
  exec "$@"
fi

chrome_remote_debugging_port="${CHROME_REMOTE_DEBUGGING_PORT:-9222}"
chrome_remote_debugging_ready_timeout="${CHROME_REMOTE_DEBUGGING_READY_TIMEOUT:-30}"
if [[ ! "${chrome_remote_debugging_port}" =~ ^[0-9]+$ ]] ||
  (( chrome_remote_debugging_port < 1 || chrome_remote_debugging_port > 65535 )); then
  printf 'CHROME_REMOTE_DEBUGGING_PORT must be an integer between 1 and 65535\n' >&2
  exit 64
fi
if [[ ! "${chrome_remote_debugging_ready_timeout}" =~ ^[0-9]+$ ]] ||
  (( chrome_remote_debugging_ready_timeout < 1 )); then
  printf 'CHROME_REMOTE_DEBUGGING_READY_TIMEOUT must be a positive integer\n' >&2
  exit 64
fi

setpriv --reuid="${sandbox_uid}" --regid="${sandbox_gid}" --init-groups -- \
  env -i \
    HOME="${sandbox_home}" \
    USER="${sandbox_user}" \
    LOGNAME="${sandbox_user}" \
    PATH="${sandbox_home}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    LANG="${LANG:-en_US.UTF-8}" \
    LC_ALL="${LC_ALL:-en_US.UTF-8}" \
    TZ="${TZ:-UTC}" \
    DISPLAY="${DISPLAY}" \
    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR}" \
    XPRA_PORT="${XPRA_PORT:-14500}" \
    XPRA_LOG_FILE="${XPRA_LOG_FILE:-${XDG_RUNTIME_DIR}/xpra-${display_number}.log}" \
    XPRA_READY_TIMEOUT="${XPRA_READY_TIMEOUT:-30}" \
    CHROME_REMOTE_DEBUGGING_PORT="${chrome_remote_debugging_port}" \
    CHROME_REMOTE_DEBUGGING_USER_DATA_DIR="${CHROME_REMOTE_DEBUGGING_USER_DATA_DIR:-${state_dir}/chrome-devtools-profile}" \
    supervise-chrome &
chrome_supervisor_pid=$!

stop_chrome_startup() {
  kill -TERM "${chrome_supervisor_pid}" 2>/dev/null || true
  wait "${chrome_supervisor_pid}" 2>/dev/null || true
  exit 143
}

trap stop_chrome_startup INT TERM

chrome_ready_deadline=$((SECONDS + chrome_remote_debugging_ready_timeout))
until curl --fail --silent --show-error --max-time 2 \
  "http://127.0.0.1:${chrome_remote_debugging_port}/json/version" >/dev/null; do
  if ! kill -0 "${chrome_supervisor_pid}" 2>/dev/null; then
    wait "${chrome_supervisor_pid}" || true
    printf 'Chrome remote debugging supervisor exited before becoming ready\n' >&2
    exit 1
  fi
  if (( SECONDS >= chrome_ready_deadline )); then
    kill -TERM "${chrome_supervisor_pid}" 2>/dev/null || true
    wait "${chrome_supervisor_pid}" || true
    printf 'Chrome remote debugging did not become ready within %ss\n' \
      "${chrome_remote_debugging_ready_timeout}" >&2
    exit 1
  fi
  sleep 1
done
printf 'Chrome remote debugging is ready on port %s\n' "${chrome_remote_debugging_port}"

"$@" &
service_pid=$!
shutdown_requested=false

forward_shutdown() {
  shutdown_requested=true
  kill -TERM "${service_pid}" "${chrome_supervisor_pid}" 2>/dev/null || true
}

trap forward_shutdown INT TERM

completed_pid=""
set +e
wait -n -p completed_pid "${service_pid}" "${chrome_supervisor_pid}"
completed_status=$?
set -e

if [[ "${shutdown_requested}" == "true" ]]; then
  wait "${service_pid}" 2>/dev/null || true
  wait "${chrome_supervisor_pid}" 2>/dev/null || true
  exit 143
fi

if [[ "${completed_pid}" == "${service_pid}" ]]; then
  kill -TERM "${chrome_supervisor_pid}" 2>/dev/null || true
  wait "${chrome_supervisor_pid}" 2>/dev/null || true
  exit "${completed_status}"
fi

printf 'Chrome remote debugging supervisor exited unexpectedly with status %s\n' \
  "${completed_status}" >&2
kill -TERM "${service_pid}" 2>/dev/null || true
wait "${service_pid}" 2>/dev/null || true
if (( completed_status == 0 )); then
  exit 1
fi
exit "${completed_status}"
