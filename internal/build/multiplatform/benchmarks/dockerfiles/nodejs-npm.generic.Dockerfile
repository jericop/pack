# generic: standard idiomatic Dockerfile — ubuntu base, install node/npm via apt,
# npm install, run. The "what most people write" baseline (no buildpack mirroring).

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates nodejs npm \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
RUN npm install --omit=dev

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates nodejs \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /app /app
ENV PORT=8080
WORKDIR /app
ENTRYPOINT ["node", "server.js"]
