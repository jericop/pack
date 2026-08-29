---
inclusion: manual
---

# Local Test Environment for BuildKit Multi-Arch + OCI Layout Testing

This documents the local setup used to test the BuildKit multi-arch build feature, including the LLB OCI layout native push work. It solves two problems: (1) the `docker-container` builder's network isolation from a local registry, and (2) a macOS port-5000 conflict.

## Summary

- **Builder**: `pack-multiplatform` (docker-container driver) recreated on a user-defined network `pack-test`
- **Registry**: `pack-test-registry` (registry:2) on the same `pack-test` network
- **Builder reaches registry by name**: `pack-test-registry:5000` (over the `pack-test` network)
- **Host reaches registry**: `localhost:5001` (port 5001 avoids the macOS AirPlay conflict on 5000)

## Why This Setup Is Needed

### Problem 1: Builder network isolation

The `docker-container` buildx driver runs BuildKit in its own container. By default it's on the `bridge` network and cannot resolve or reach a registry container by name. A registry published to `localhost:5000` on the host is not reachable from inside the builder in a consistent way (on Docker Desktop/Mac, "host" is the VM, not your Mac).

Fix: put the builder AND the registry on the same user-defined network (`pack-test`). The builder resolves the registry by container name/alias.

### Problem 2: macOS port 5000 conflict

On macOS, port 5000 is occupied by Control Center (AirPlay Receiver). `lsof -nP -iTCP:5000 -sTCP:LISTEN` shows `ControlCe` listening. A registry published to host port 5000 conflicts and produces confusing responses.

Fix: publish the registry to host port **5001** (`-p 5001:5000`). The in-container port stays 5000, so the builder still uses `pack-test-registry:5000` over the network; only the host mapping differs.

## Setup Commands

### 1. Create the shared network

```bash
docker network create pack-test
```

### 2. Start the registry (detached daemon, persistent)

```bash
docker run -d --name pack-test-registry \
  --network pack-test \
  --network-alias pack-test-registry \
  -p 5001:5000 \
  --restart unless-stopped \
  registry:2
```

- `--network-alias pack-test-registry`: builder resolves it by this name on `pack-test`
- `-p 5001:5000`: host access at `localhost:5001` (avoids macOS AirPlay on 5000)
- `--restart unless-stopped`: survives Docker restarts

### 3. Recreate the builder on the shared network

The `network` driver option cannot be added to an existing builder, so recreation is required. Recreating drops the builder's BuildKit cache (acceptable for a test builder).

```bash
docker buildx rm pack-multiplatform   # may report a state-volume timeout but still removes the builder

docker buildx create --name pack-multiplatform \
  --driver docker-container \
  --driver-opt network=pack-test \
  --buildkitd-flags '--allow-insecure-entitlement=network.host' \
  --bootstrap
```

- `--driver-opt network=pack-test`: puts the builder on the shared network
- `--buildkitd-flags '--allow-insecure-entitlement=network.host'`: preserves the host-network entitlement the original builder had

Note: `docker buildx rm` may print `context deadline exceeded` while deleting the state volume but still removes the builder (verify with `docker buildx ls`). No leftover `buildx_buildkit_pack-multiplatform0_state` volume should remain.

## Verification

```bash
# Builder is on pack-test
docker inspect buildx_buildkit_pack-multiplatform0 \
  --format '{{range $net, $conf := .NetworkSettings.Networks}}{{$net}} {{$conf.IPAddress}}{{"\n"}}{{end}}'

# Entitlement preserved
docker buildx inspect pack-multiplatform | grep -i 'daemon flags'

# Host access (expect {})
curl -s http://localhost:5001/v2/

# Builder access by name (expect {})
docker exec buildx_buildkit_pack-multiplatform0 wget -qO- http://pack-test-registry:5000/v2/
```

All four should succeed; the two `/v2/` checks return `{}` (healthy empty registry).

## Using the Registry in Builds

Because the registry is plain HTTP, it must be treated as insecure:

- pack flag: `--insecure-registry pack-test-registry:5000`
- or env: `CNB_INSECURE_REGISTRIES=pack-test-registry:5000`

Reference images by the in-network name so the builder resolves them:

```
pack-test-registry:5000/<name>:<tag>
```

From your Mac (e.g., to inspect or pull), use the host mapping:

```
localhost:5001/<name>:<tag>
```

Note: the in-network name (`pack-test-registry:5000`) and the host mapping (`localhost:5001`) point at the same registry but use different host:port. Tags pushed by a build use the name the build referenced. For a build that pushes to `pack-test-registry:5000/...`, inspect from the host by querying `localhost:5001` for the same repository/tag path.

## Relationship to the Spec

This environment supports the `oci-layout-tag-elimination` spec's testing tiers:
- Tier 2 (on-disk parity) does NOT need this registry — it inspects OCI layouts on disk
- Tier 3 (optional, env-var gated registry integration) uses this registry to verify the native push produces no intermediate tags and that pushed artifacts match registry mode

The env-var gate (e.g., `PACK_TEST_REGISTRY_ENABLED` + a registry ref) keeps registry tests skipped by default; this setup is what you enable them against locally.

## Teardown

```bash
docker rm -f pack-test-registry
docker buildx rm pack-multiplatform
docker network rm pack-test
```

Recreate with the setup commands above when needed.
