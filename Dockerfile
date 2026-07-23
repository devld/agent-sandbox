# syntax=docker/dockerfile:1.7

ARG UBUNTU_VERSION=24.04
ARG NODEJS_MAJOR=24
ARG PYTHON_VERSION=3.14

FROM docker.io/library/golang:1.25-bookworm AS control-plane-builder
WORKDIR /src
COPY go.mod ./
COPY api-service ./api-service
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent-sandbox ./api-service

FROM docker.io/library/ubuntu:${UBUNTU_VERSION}
ARG DEBIAN_FRONTEND=noninteractive
ARG NODEJS_MAJOR
ARG PYTHON_VERSION

# ── Layer order: most-stable → least-stable ──────────────────────────

# L1  Bootstrap: minimal tools for repo setup (essentially never changes)
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg

# L2  Repo keys + xpra/deadsnakes source lists (no ARG dependency, rarely changes)
RUN curl -fsSL --retry 5 --retry-all-errors --connect-timeout 15 --max-time 120 \
      https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
      | gpg --batch --yes --dearmor -o /usr/share/keyrings/nodesource.gpg \
    && curl -fsSL --retry 5 --retry-all-errors --connect-timeout 15 --max-time 120 \
      'https://keyserver.ubuntu.com/pks/lookup?op=get&search=0xF23C5A6CF475977595C89F51BA6932366A755776' \
      | gpg --batch --yes --dearmor -o /usr/share/keyrings/deadsnakes.gpg \
    && curl -fsSL --retry 5 --retry-all-errors --connect-timeout 15 --max-time 120 \
      -o /usr/share/keyrings/xpra.asc https://xpra.org/xpra.asc \
    && . /etc/os-release \
    && printf '%s\n' \
      "deb [signed-by=/usr/share/keyrings/deadsnakes.gpg] https://ppa.launchpadcontent.net/deadsnakes/ppa/ubuntu ${VERSION_CODENAME} main" \
      >/etc/apt/sources.list.d/deadsnakes.list \
    && curl -fsSL --retry 5 --retry-all-errors --connect-timeout 15 --max-time 120 \
      -o /etc/apt/sources.list.d/xpra.sources \
      "https://raw.githubusercontent.com/Xpra-org/xpra/master/packaging/repos/${VERSION_CODENAME}/xpra.sources"

# L3  xpra (rarely changes, no ARG dependency)
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends xpra xpra-html5 xpra-x11

# L4  nodesource source list (invalidated only when NODEJS_MAJOR changes)
RUN printf '%s\n' \
      "deb [signed-by=/usr/share/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODEJS_MAJOR}.x nodistro main" \
      >/etc/apt/sources.list.d/nodesource.list

# L5  nodejs + python (invalidated when NODEJS_MAJOR or PYTHON_VERSION changes)
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends \
      nodejs \
      "python${PYTHON_VERSION}" \
      "python${PYTHON_VERSION}-venv" \
    && ln -s "/usr/bin/python${PYTHON_VERSION}" /usr/local/bin/python \
    && ln -s "/usr/bin/python${PYTHON_VERSION}" /usr/local/bin/python3 \
    && node --version && npm --version && python --version

COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/

# L6  Playwright browser deps (rarely changes, depends on playwright version)
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    npx --loglevel=info --yes playwright@latest install-deps chromium \
    && uv --version

# L7  Runtime apt packages (changes frequently — keep last among apt layers)
COPY docker/apt-runtime-packages.txt /tmp/apt-runtime-packages.txt
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' /tmp/apt-runtime-packages.txt \
      | xargs -r apt-get install -y --no-install-recommends \
    && locale-gen en_US.UTF-8 \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && rm -f /tmp/apt-runtime-packages.txt

COPY --from=control-plane-builder /out/agent-sandbox /usr/local/bin/agent-sandbox
COPY docker/playwright-cli.config.json /etc/agent-sandbox/playwright-cli.config.json
COPY docker/entrypoint.sh docker/install-or-update-cloakbrowser docker/start-xpra /usr/local/bin/
RUN chmod 0755 \
      /usr/local/bin/agent-sandbox \
      /usr/local/bin/entrypoint.sh \
      /usr/local/bin/install-or-update-cloakbrowser \
      /usr/local/bin/start-xpra

ENV LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8 \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

ENV SANDBOX_USERNAME=agent \
    SANDBOX_UID=1000 \
    SANDBOX_GID=1000 \
    SANDBOX_API_HOST=0.0.0.0 \
    SANDBOX_API_PORT=3000 \
    SANDBOX_BROWSER_DISABLED=false \
    DISPLAY=:100 \
    XPRA_AUTO_START=false \
    XPRA_PORT=14500

EXPOSE 3000 14500
HEALTHCHECK --interval=10s --timeout=3s --start-period=4h --start-interval=2s --retries=3 \
    CMD test -e "/home/${SANDBOX_USERNAME}/.config/agent-sandbox/home-initialized" \
        && curl --fail --silent --show-error --max-time 2 \
            "http://127.0.0.1:${SANDBOX_API_PORT}/healthz" >/dev/null \
        || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["agent-sandbox"]
