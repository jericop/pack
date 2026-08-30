# generic: standard idiomatic Dockerfile — ubuntu base, install a JDK via apt to
# build the Spring Boot jar, run it on ubuntu with an apt-installed JRE. The "what
# most people write" baseline (no buildpack mirroring).

# ---- build stage ---------------------------------------------------------
FROM ubuntu:noble AS build
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates openjdk-21-jdk-headless \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN ./mvnw -B -q -DskipTests package

# ---- run stage -----------------------------------------------------------
FROM ubuntu:noble AS run
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates openjdk-21-jre-headless \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/*.jar /app.jar
ENV PORT=8080
ENTRYPOINT ["java", "-jar", "/app.jar"]
