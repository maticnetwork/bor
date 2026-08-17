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
packaging/docker/amoy-sidecar-pair.sh check-witness
packaging/docker/amoy-sidecar-pair.sh check-fetchers
packaging/docker/amoy-sidecar-pair.sh soak 10
packaging/docker/amoy-sidecar-pair.sh recover
packaging/docker/amoy-sidecar-pair.sh soak-long 50 10
packaging/docker/amoy-sidecar-pair.sh soak-high-confidence
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
- pins the pair to `state.scheme = "hash"` so deterministic snap fixture seeding
  and snapshot-backed QUIC bulk checks can run end-to-end
- also passes `--state.scheme hash` on the container command line so the live
  test path is not dependent on TOML decoding quirks
- assigns stable nodekeys and stable container IPs
- pairs the nodes with `admin_addPeer` using full ENRs so QUIC metadata is preserved
- verifies `eth-blocks`, `eth-bulk`, `eth-control`, `eth-tx`,
  `eth-tx-fetch`,
  `snap-accounts`, `snap-storage`, `snap-code`, `snap-trie`, and `wit-bulk`
  channel bring-up from logs
- actively triggers tx gossip and block announcements through the local `admin`
  RPC and verifies the resulting `eth-bulk` sidecar message logs
- actively triggers deterministic snap account-range, storage-range, bytecode,
  and trie-node requests through the local `admin` RPC and verifies the
  resulting named-lane request/response logs on `snap-accounts`,
  `snap-storage`, `snap-code`, and `snap-trie`
- actively triggers witness announcements and witness metadata requests through
  the local `admin` RPC and verifies the resulting `wit-bulk` sidecar message logs
- seeds a deterministic witness on one node and verifies both witness metadata
  fetch and full witness page fetch over `wit-bulk`
- actively triggers tx fetch and block body fetch requests so fetcher-style
  sidecar traffic is verified on both `eth-tx-fetch` and `eth-bulk`
- restarts one node on demand, waits for devp2p and QUIC sidecar recovery, and
  reruns the mixed traffic checks so recovery is validated instead of assumed
- captures `admin_bulkSidecarStatus` snapshots before and after each recovery
  bounce so churn runs leave channel/session evidence, not just pass/fail output
- exposes `soak`, `fallback`, and `measure` commands for repeated checks,
  fallback-path verification, and simple timing capture
- exposes `soak-long` for a heavier soak that injects periodic node restarts
  during the run
- exposes `soak-high-confidence` as the default longer churn preset
  (`soak-long 50 10`)
- exposes `status` output with the live `admin_bulkSidecarStatus` snapshot from
  each node so session/channel health, UDP socket buffers, and QUIC packet
  counters are visible without log spelunking

Notes:

- the nodes still attempt normal Amoy discovery and outbound peering
- the configured `extip` values are the internal Docker bridge IPs so the pair can
  open QUIC sidecar channels directly to each other
- public Amoy peers are not expected to reach these bridge IPs; this stack is for
  controlled live-network testing of our own pair
- `check` is deterministic now: once the pair is connected, it injects the
  verification traffic immediately instead of waiting on ambient Amoy activity
- `up` resets the pair volumes by default so old path-scheme state cannot leak
  into the hash-scheme verification flow; set `PRESERVE_VOLUMES=1` if you want
  to keep the existing data volume intentionally
- `fallback` runs the local routed-message fallback tests that prove bulk-lane
  write/read failures fall back safely to the primary devp2p lane
- `measure` prints rough end-to-end timing in milliseconds for the forced
  sidecar checks so you can compare behavior across builds or environments
- `recover` defaults to restarting `bor-b`; pass `bor-a` or `bor-b` explicitly
  if you want to choose which side is bounced
- `soak-long 50 10` runs 50 mixed-check iterations and restarts `bor-b` every
  10 iterations to confirm the sidecar keeps recovering under repeated churn
- `soak-high-confidence` is a convenience wrapper for the same `50 / 10` churn
  profile when you want the longer confidence run without remembering arguments
