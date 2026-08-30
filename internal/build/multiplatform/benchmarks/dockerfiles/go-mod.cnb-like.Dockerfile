# cnb-like: mirrors what the Paketo Go buildpacks actually do, but as a Dockerfile.
#   - ubuntu base (same family as the CNB build/run images)
#   - download the Go distribution from its official web release and install it
#     (paketo go-dist does exactly this), rather than apt-get
#   - go mod vendor + go build a PIE, -trimpath binary (paketo go-build flags)
#   - final image is a clean ubuntu run image with just the binary (paketo layers
#     the app onto the run image)
# Multi-arch: TARGETARCH drives the Go dist download so amd64/arm64 both work.

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
ARG TARGETARCH
ARG GO_VERSION=1.26.2

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# Download + install the Go distribution from the official web release (the
# go-dist buildpack downloads a release tarball and unpacks it — same idea).
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" -o /tmp/go.tgz \
 && tar -C /usr/local -xzf /tmp/go.tgz \
 && rm /tmp/go.tgz
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOTOOLCHAIN=local

WORKDIR /workspace
COPY . .
# go-mod-vendor buildpack: vendor deps; go-build buildpack: PIE + trimpath.
RUN go mod vendor \
 && go build -mod=vendor -buildmode=pie -trimpath -o /workspace/app .

# ---- run stage -----------------------------------------------------------
# Match the CNB run image family (ubuntu). Buildpacks layer the app onto the run
# image and set a default process; we mirror that with a minimal ubuntu + binary.
FROM ubuntu:noble AS run
# ubuntu:noble already ships a UID 1000 user ("ubuntu"); reuse it as the non-root
# runtime user (the CNB run image likewise runs the app as UID 1000).
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /workspace/app /workspace/app
USER 1000
ENV PORT=8080
WORKDIR /workspace
ENTRYPOINT ["/workspace/app"]
