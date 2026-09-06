# syntax=docker/dockerfile:1
# Stella sandbox image
#
# Build locally (dev):
# mise run sandbox:docker:build
#
# The image tag is version-locked to the stella version used at build time:
# - dev builds      → stella-sandbox:dev            (built locally)
# - tagged releases → ghcr.io/cherryhq/stella-sandbox:<version>

# Cross-compile from the build host's arch (BUILDPLATFORM) to the target arch so
# buildx multi-arch builds don't run the Go toolchain under QEMU. TARGETOS/ARCH
# are auto-populated by BuildKit (default to the host arch for a plain build).
FROM --platform=$BUILDPLATFORM golang:1.25 AS stella-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN TARGET_GOOS=${TARGETOS} TARGET_GOARCH=${TARGETARCH} go run ./internal/tools/syncembeddedbinaries
# -s -w: this image is pulled per deployment, and the symbol and DWARF tables
# are ~35 MB nothing in it reads. gopclntab survives, so panic tracebacks keep
# function names and line numbers.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X github.com/CherryHQ/stella/internal/platform/version.Version=${VERSION}" \
    -o /out/stellad ./cmd/stellad

FROM debian:13-slim

ARG BUILTIN_BUNDLE_REVISION
LABEL org.cherryhq.stella.builtin-bundle-revision=${BUILTIN_BUNDLE_REVISION}

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates git curl wget \
        python3 python3-venv \
        nodejs npm && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

ARG USER_UID=1000
ARG USER_GID=1000

RUN groupadd --non-unique -g ${USER_GID} stella && \
    useradd --non-unique -u ${USER_UID} -g ${USER_GID} -m -s /bin/bash stella && \
    mkdir -p /workspace /user /opt/stella/bin /opt/stella/.mise-tools/configs /opt/stella/user-tools && \
    ln -s /opt/stella/bin/.stella-shell-env /etc/profile.d/99-stella-mise.sh && \
    chown -R stella:stella /home/stella /workspace /user /opt/stella

USER stella
# The image mise tree lives under ABSOLUTE /opt/stella — the SAME scheme the
# bwrap sandbox exposes (STELLA_HOME → /opt/stella) — so an agent sees identical
# mise paths whichever backend runs it, and switching backends has no effect. At
# runtime RuntimeMiseEnv overrides MISE_DATA_DIR to the per-user writable tree
# (/opt/stella/users/{id}/.mise-tools) mounted from the host; the baked mise
# assets remain immutable runtime internals that per-user relative symlinks can
# resolve against. Snapshot-selected config is injected at session creation;
# there is no shared config fallback. HOME stays /home/stella for the build,
# then flips to /workspace.
ENV HOME=/home/stella
ENV BASH_ENV=/opt/stella/core-runtime/.stella-shell-env
ENV MISE_DATA_DIR=/opt/stella/.mise-tools
ENV MISE_CONFIG_DIR=/opt/stella/.mise-tools/config
ENV MISE_STATE_DIR=/opt/stella/.mise-tools/state
ENV MISE_CACHE_DIR=/opt/stella/.mise-tools/cache
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# `system-bundle install` extracts the exact release-owned core runtimes
# (mise, Xberg, fd, and ripgrep) and shell environment. stellad is
# bind-mounted only for this build step; it is not shipped in the runtime
# image. Snapshot selection later decides which optional plugin binaries are
# visible to each runner, while trusted core runtimes are republished into
# every selection volume.
RUN --mount=type=bind,from=stella-builder,source=/out/stellad,target=/usr/local/bin/stellad \
    --mount=type=secret,id=github_token,env=GITHUB_TOKEN \
	STELLA_HOME=/opt/stella stellad system-bundle install --core-path /opt/stella/core-runtime --prepare-builtin-artifacts && \
	REVISION="$(STELLA_HOME=/opt/stella stellad system-bundle revision)" && \
	test "$REVISION" = "$BUILTIN_BUNDLE_REVISION" && \
	mkdir -p /opt/stella/skills && ln -s "../bundles/$REVISION" /opt/stella/skills/builtin && \
    /opt/stella/core-runtime/mise --version && \
    /opt/stella/core-runtime/xberg --version && \
    /opt/stella/core-runtime/fd --version && \
    /opt/stella/core-runtime/rg --version && \
    rm -rf /opt/stella/.mise-tools/cache \
           /opt/stella/.mise-tools/config \
           /opt/stella/.mise-tools/state \
           /opt/stella/.mise-tools/home \
           /opt/stella/.mise-tools/xdg \
           /opt/stella/.mise-private

# The install above runs as the owning UID, so an owner-only permission bit on a
# bundled runtime cannot fail it — that is how a 0700 Xberg bundle directory once
# shipped while `xberg --version` passed here. USER_UID is a build arg and the
# image is routinely run under a different UID, so re-run the release-owned
# runtimes as an unrelated user (nobody). Runtime sessions expose these binaries
# only through selection-local aliases. This check distinguishes "installed"
# from "reachable by the process that will actually use it".
USER root
RUN HOME=/tmp setpriv --reuid=65534 --regid=65534 --clear-groups /opt/stella/core-runtime/mise --version && \
    HOME=/tmp setpriv --reuid=65534 --regid=65534 --clear-groups /opt/stella/core-runtime/xberg --version && \
    HOME=/tmp setpriv --reuid=65534 --regid=65534 --clear-groups /opt/stella/core-runtime/fd --version && \
    HOME=/tmp setpriv --reuid=65534 --regid=65534 --clear-groups /opt/stella/core-runtime/rg --version
USER stella

ENV HOME=/workspace
WORKDIR /workspace
