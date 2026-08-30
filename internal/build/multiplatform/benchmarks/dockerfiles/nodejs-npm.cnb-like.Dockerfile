# cnb-like: mirrors the Paketo Node.js buildpacks — ubuntu base, download the Node
# distribution from its official web release (nodejs.org) and install it (the
# node-engine buildpack downloads a release tarball), then npm ci. Final image is a
# clean ubuntu run image with node + the app (npm-install/node-start buildpacks).
# Multi-arch: map TARGETARCH -> node's arch naming (x64/arm64).

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
ARG TARGETARCH
ARG NODE_VERSION=22.14.0

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils \
 && rm -rf /var/lib/apt/lists/*

# Node uses "x64"/"arm64"; Docker TARGETARCH is "amd64"/"arm64".
RUN case "$TARGETARCH" in \
      amd64) NARCH=x64 ;; \
      arm64) NARCH=arm64 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NARCH}.tar.xz" -o /tmp/node.tar.xz \
 && tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz \
 && rm /tmp/node.tar.xz
ENV PATH="/usr/local/bin:${PATH}"

WORKDIR /workspace
COPY . .
# npm-install buildpack installs production deps from the lockfile.
RUN npm ci --omit=dev || npm install --omit=dev

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
ARG TARGETARCH
ARG NODE_VERSION=22.14.0
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in amd64) NARCH=x64 ;; arm64) NARCH=arm64 ;; esac \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NARCH}.tar.xz" -o /tmp/node.tar.xz \
 && tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz \
 && rm /tmp/node.tar.xz \
 && DEBIAN_FRONTEND=noninteractive apt-get purge -y curl xz-utils || true
COPY --from=build /workspace /workspace
USER 1000
ENV PORT=8080
WORKDIR /workspace
ENTRYPOINT ["node", "server.js"]
