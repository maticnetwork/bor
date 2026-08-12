# QUIC Sidecar On A VM

This sidecar path does not need a special build flag or build tag.

Build Bor normally:

```bash
make bor
```

or:

```bash
go build -o build/bin/bor ./cmd/cli/main.go
```

Enable the feature at runtime in your config:

```toml
syncmode = "full"
snapshot = true

[p2p]
  bind = "0.0.0.0"
  port = 30303
  bulk-sidecar = true
  bulk-port = 30304
  nat = "extip:<reachable-ip-or-dns>"

[jsonrpc]
  [jsonrpc.http]
    enabled = true
    host = "0.0.0.0"
    port = 8545
    api = ["eth", "net", "web3", "txpool", "bor", "admin"]

[witness]
  enable = true
```

What matters:

- `p2p.bulk-sidecar = true` turns on the QUIC sidecar listener and dialer.
- `p2p.bulk-port` must be reachable between the two Bor nodes over UDP.
- `p2p.nat = "extip:..."` should advertise the real address the other node can
  dial for both devp2p and the sidecar metadata.
- `[witness].enable = true` is required if you want to validate `wit-bulk`.
- No extra compile-time switch is required for `eth-bulk`, `snap-bulk`, or
  `wit-bulk`.

Minimal two-node process:

1. Start `bor-a` and `bor-b` with sidecar-enabled configs.
2. Open TCP/UDP `30303` and UDP `30304` between them.
3. Use `admin_nodeInfo` on each node and capture the full `enr`.
4. Call `admin_addPeer(["<other-enr>"])` on both sides.
5. Wait for `admin_peers` to show the opposite node ID on each side.
6. Confirm logs show `Bulk sidecar session established`.
7. Confirm logs show `Bulk sidecar channel opened` for `eth-bulk`,
   `snap-bulk`, and `wit-bulk`.
8. Trigger traffic over RPC:

```bash
curl -s http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_triggerTxGossip","params":[]}'

curl -s http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_triggerBlockAnnouncement","params":[]}'

curl -s http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_triggerTxFetch","params":[]}'

curl -s http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_triggerBlockBodyFetch","params":[]}'

curl -s http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_triggerSnapTrieNodeFetch","params":[]}'

curl -s http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"admin_triggerWitnessAnnouncement","params":[]}'
```

Deterministic witness fetch validation:

1. On the serving node, call `admin_seedWitnessForHead`.
2. Read `blockHash` from the result.
3. On the requesting node, call
   `admin_triggerWitnessMetadataFetchByHash(["<blockHash>"])`.
4. On the requesting node, call
   `admin_triggerWitnessFetchByHash(["<blockHash>"])`.
5. Confirm logs show `wit-bulk` codes `4/5` for metadata and `2/3` for the
   full page fetch.

Useful log checks:

```bash
rg "Bulk sidecar session established|Bulk sidecar channel opened|Bulk sidecar (wrote|read) message" /path/to/bor.log
```

Expected live signals:

- `eth-bulk` carries tx gossip, block announcements, tx fetches, and block body
  fetches.
- `snap-bulk` carries the deterministic trie-node request/response triggered by
  `admin_triggerSnapTrieNodeFetch`.
- `wit-bulk` carries witness announcements, metadata probes, and witness page
  fetches.

For a reusable live setup, the Docker helper remains the easiest harness:

```bash
packaging/docker/amoy-sidecar-pair.sh up
packaging/docker/amoy-sidecar-pair.sh check
packaging/docker/amoy-sidecar-pair.sh recover
packaging/docker/amoy-sidecar-pair.sh soak-long 50 10
```
