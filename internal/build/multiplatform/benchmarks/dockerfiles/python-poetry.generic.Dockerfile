# generic: standard idiomatic Dockerfile — ubuntu base, install python3 + pip via
# apt, install poetry with pip, poetry install, run gunicorn. The "what most people
# write" baseline (no buildpack mirroring, distro Python).

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
RUN python3 -m venv /app/.venv \
 && . /app/.venv/bin/activate \
 && pip install --no-cache-dir poetry \
 && poetry config virtualenvs.create false \
 && poetry install --only main --no-root

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates python3 \
 && rm -rf /var/lib/apt/lists/*
ENV PATH="/app/.venv/bin:${PATH}"
COPY --from=build /app /app
ENV PORT=8080
WORKDIR /app
ENTRYPOINT ["gunicorn", "--bind", "0.0.0.0:8080", "server:app"]
