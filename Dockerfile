# syntax=docker/dockerfile:1
ARG base_image=gcr.io/distroless/static

FROM golang:1.26 AS builder
ARG pack_version
ENV PACK_VERSION=$pack_version
# go.mod requires go >= 1.26.6; allow the toolchain to auto-resolve the exact
# patch version so the build doesn't fail with GOTOOLCHAIN=local on a base image
# that is slightly behind.
ENV GOTOOLCHAIN=auto
WORKDIR /app
COPY . .
RUN make build

FROM ${base_image}
COPY --from=builder /app/out/pack /usr/local/bin/pack
ENTRYPOINT [ "/usr/local/bin/pack" ]
