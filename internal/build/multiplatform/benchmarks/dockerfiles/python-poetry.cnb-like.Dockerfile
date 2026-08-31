# cnb-like: mirrors the Paketo Python buildpacks — ubuntu base, download a CPython
# distribution from a web release (the cpython/python-runtime buildpack downloads a
# prebuilt CPython; here we use the python-build-standalone release, the same idea:
# a prebuilt interpreter tarball, not apt), install poetry, then `poetry install`
# the locked deps. Final image is a clean ubuntu run image with CPython + the venv
# and the gunicorn entrypoint (Procfile: web: gunicorn server:app).
# Multi-arch: TARGETARCH -> standalone CPython arch naming (x86_64/aarch64).

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
ARG TARGETARCH
ARG PY_TAG=20260825
ARG PY_VERSION=3.12.14

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# Download + install a prebuilt CPython from its web release (buildpack-style).
RUN case "$TARGETARCH" in \
      amd64) PARCH=x86_64 ;; \
      arm64) PARCH=aarch64 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/astral-sh/python-build-standalone/releases/download/${PY_TAG}/cpython-${PY_VERSION}+${PY_TAG}-${PARCH}-unknown-linux-gnu-install_only.tar.gz" -o /tmp/py.tgz \
 && tar -C /opt -xzf /tmp/py.tgz \
 && rm /tmp/py.tgz
# tarball unpacks to /opt/python
ENV PATH="/opt/python/bin:${PATH}"

WORKDIR /workspace
COPY . .
# poetry buildpack: install poetry, then install locked deps into a venv.
RUN python -m pip install --no-cache-dir poetry \
 && python -m venv /workspace/.venv \
 && . /workspace/.venv/bin/activate \
 && poetry config virtualenvs.create false \
 && poetry install --only main --no-root

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
ARG TARGETARCH
ARG PY_TAG=20260825
ARG PY_VERSION=3.12.14
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in amd64) PARCH=x86_64 ;; arm64) PARCH=aarch64 ;; esac \
 && curl -fsSL "https://github.com/astral-sh/python-build-standalone/releases/download/${PY_TAG}/cpython-${PY_VERSION}+${PY_TAG}-${PARCH}-unknown-linux-gnu-install_only.tar.gz" -o /tmp/py.tgz \
 && tar -C /opt -xzf /tmp/py.tgz \
 && rm /tmp/py.tgz \
 && DEBIAN_FRONTEND=noninteractive apt-get purge -y curl || true
ENV PATH="/workspace/.venv/bin:/opt/python/bin:${PATH}"
COPY --from=build /workspace /workspace
USER 1000
ENV PORT=8080
WORKDIR /workspace
ENTRYPOINT ["gunicorn", "--bind", "0.0.0.0:8080", "server:app"]
