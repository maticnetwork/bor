# Amoy Sidecar Pair

Reusable two-node Bor setup for live Amoy sidecar testing.

Files:

- `docker-compose.amoy-sidecar-pair.yml`
- `packaging/docker/bor-amoy-sidecar-a.toml`
- `packaging/docker/bor-amoy-sidecar-b.toml`
- `packaging/docker/amoy-sidecar-pair.sh`

Usage:

```bash
packaging/docker/amoy-sidecar-pair.sh up
packaging/docker/amoy-sidecar-pair.sh check
packaging/docker/amoy-sidecar-pair.sh check-fetchers
packaging/docker/amoy-sidecar-pair.sh soak 10
packaging/docker/amoy-sidecar-pair.sh fallback
packaging/docker/amoy-sidecar-pair.sh measure
packaging/docker/amoy-sidecar-pair.sh status
packaging/docker/amoy-sidecar-pair.sh logs
packaging/docker/amoy-sidecar-pair.sh down
```

Host RPC endpoints:

- `bor-a`: `http://127.0.0.1:18545`
- `bor-b`: `http://127.0.0.1:28545`

What this does:

- starts two sidecar-enabled Bor nodes on Amoy
- assigns stable nodekeys and stable container IPs
- pairs the nodes with `admin_addPeer` using full ENRs so QUIC metadata is preserved
- verifies `eth-bulk` and `snap-bulk` channels from logs
- actively triggers tx gossip and block announcements through the local `admin`
  RPC and verifies the resulting `eth-bulk` sidecar message logs
- actively triggers tx fetch and block body fetch requests so fetcher-style bulk
  traffic is also verified on the sidecar
- exposes `soak`, `fallback`, and `measure` commands for repeated checks,
  fallback-path verification, and simple timing capture

Notes:

- the nodes still attempt normal Amoy discovery and outbound peering
- the configured `extip` values are the internal Docker bridge IPs so the pair can
  open QUIC sidecar channels directly to each other
- public Amoy peers are not expected to reach these bridge IPs; this stack is for
  controlled live-network testing of our own pair
- `check` is deterministic now: once the pair is connected, it injects the
  verification traffic immediately instead of waiting on ambient Amoy activity
- `fallback` runs the local routed-message fallback tests that prove bulk-lane
  write/read failures fall back safely to the primary devp2p lane
- `measure` prints rough end-to-end timing in milliseconds for the forced
  sidecar checks so you can compare behavior across builds or environments
