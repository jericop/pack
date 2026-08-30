# cnb-like: mirrors the Paketo Java buildpacks — ubuntu base, download a JDK from a
# web release (the buildpacks download BellSoft Liberica from its GitHub releases)
# to BUILD, run the Maven build (./mvnw package), then run the fat jar on a clean
# ubuntu image with a downloaded JRE (the buildpacks install a JRE into the run
# image layer). Multi-arch: TARGETARCH -> Liberica arch naming.

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
ARG TARGETARCH
# Liberica JDK 21 (LTS) — the Java buildpacks default to a current LTS JDK.
ARG LIBERICA_VERSION=21.0.6+10
ARG LIBERICA_TAG=21.0.6

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl tar \
 && rm -rf /var/lib/apt/lists/*

# BellSoft Liberica uses "amd64"/"aarch64" in its asset names.
RUN case "$TARGETARCH" in \
      amd64) LARCH=amd64 ;; \
      arm64) LARCH=aarch64 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/bell-sw/Liberica/releases/download/${LIBERICA_VERSION}/bellsoft-jdk${LIBERICA_TAG}+10-linux-${LARCH}.tar.gz" -o /tmp/jdk.tgz \
 && mkdir -p /opt/jdk \
 && tar -C /opt/jdk --strip-components=1 -xzf /tmp/jdk.tgz \
 && rm /tmp/jdk.tgz
ENV JAVA_HOME=/opt/jdk
ENV PATH="/opt/jdk/bin:${PATH}"

WORKDIR /workspace
COPY . .
# Build the Spring Boot fat jar (the maven buildpack runs the maven build).
RUN ./mvnw -B -q -DskipTests package

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
ARG TARGETARCH
ARG LIBERICA_VERSION=21.0.6+10
ARG LIBERICA_TAG=21.0.6
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl tar \
 && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in amd64) LARCH=amd64 ;; arm64) LARCH=aarch64 ;; esac \
 && curl -fsSL "https://github.com/bell-sw/Liberica/releases/download/${LIBERICA_VERSION}/bellsoft-jre${LIBERICA_TAG}+10-linux-${LARCH}.tar.gz" -o /tmp/jre.tgz \
 && mkdir -p /opt/jre \
 && tar -C /opt/jre --strip-components=1 -xzf /tmp/jre.tgz \
 && rm /tmp/jre.tgz \
 && DEBIAN_FRONTEND=noninteractive apt-get purge -y curl tar || true
ENV JAVA_HOME=/opt/jre
ENV PATH="/opt/jre/bin:${PATH}"
COPY --from=build /workspace/target/*.jar /workspace/app.jar
USER 1000
ENV PORT=8080
WORKDIR /workspace
ENTRYPOINT ["java", "-jar", "/workspace/app.jar"]
