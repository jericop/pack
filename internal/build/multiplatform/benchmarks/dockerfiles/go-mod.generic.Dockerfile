# generic: a standard, idiomatic multi-stage Dockerfile the way a typical developer
# would write one — ubuntu base, install the toolchain the simple way (apt), build
# the app, copy the binary into a clean ubuntu runtime image. No attempt to mirror
# buildpack internals; this is the "what most people write" baseline.

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates golang-go \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN go build -o /out/app .

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/app /app
ENV PORT=8080
ENTRYPOINT ["/app"]
