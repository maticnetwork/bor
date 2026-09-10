# Out-of-sync transaction rebroadcast observation

## Automated end-to-end test

Run from the repository root with Docker, Kurtosis, Git, and Python 3 available:

```bash
tests/kurtosis/rebroadcast/run.sh
```

The runner builds the current checkout, downloads Kurtosis PoS v1.4.2 into a
temporary directory, and starts a dedicated enclave with one validator and one
RPC node. It stops that enclave on exit and retains its data for inspection.
Use `BOR_IMAGE=<already-built-image>` to skip building and `KEEP_ENCLAVE=true`
to keep the test network running. Existing enclaves are not reused or stopped.

The test asserts all three phases:

1. The RPC node catches up and rebroadcasts a pending transaction at least three
   times, proving that rebroadcast is enabled and the transaction is eligible.
2. The test briefly isolates P2P traffic and removes the RPC node's validator
   peer while the validator advances 20 blocks. It then changes the same
   targeted `netem` rule to a small delay and bandwidth limit and reconnects.
   During catch-up, the test requires the node to report active sync, remain
   connected, advance its own block height, identify at least three new stuck-tx
   batches, and emit zero rebroadcast batches.
3. Removing the network rules lets the node catch up and rebroadcast the same
   pending transaction at least three more times.

The fixture uses a two-second rebroadcast interval and higher validator gas-tip
thresholds so the transaction stays executable in the RPC pool without being
mined. These changes apply only to the temporary devnet configuration. Network
rules match only TCP P2P traffic from the validator to the test RPC node;
Heimdall connections and HTTP RPC stay intact.
The test removes its rules on success, failure, or interruption.

The default impairment is 1.5 seconds of delay with a 64 kbit/s rate. The test
fails if it cannot establish or maintain the connected-but-behind state; zero
rebroadcast from an idle pool or disconnected node does not count as a pass.
Adjust the impairment or observation timeout on faster/slower machines:

```bash
tests/kurtosis/rebroadcast/run.sh --delay 1500ms --rate 64kbit --timeout 240
```

Results are written under `build/rebroadcast-e2e/<enclave>/`: `summary.json`
contains the pass/fail result and phase evidence, `samples.jsonl` records
heights, peer counts, sync status, and batch counts, and `target.log` contains
the RPC node logs. `ARTIFACTS` overrides the output directory. Counts represent
handler rebroadcast batches, not per-peer gossip deliveries or mined transactions.

To exercise the assertions and cleanup handling without Docker:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s tests/kurtosis/rebroadcast -p 'test_*.py' -v
```

## Manual comparison with an older release

This scenario runs a local Polygon PoS network with transaction load, one RPC
node built from the current checkout, and one RPC node on Bor v2.9.0 for a
before/after comparison. The older node does not activate the devnet's Austin
fork at block 128, which models a connected node halted at a hard fork.

## Start the network

Build the candidate image from the repository root:

```bash
docker build -t bor:local --file Dockerfile .
```

Clone the Polygon PoS Kurtosis package and launch the scenario:

```bash
git clone --branch v1.4.2 --depth 1 https://github.com/0xPolygon/kurtosis-pos.git /tmp/kurtosis-pos
kurtosis run --enclave rebroadcast \
  --args-file "$PWD/tests/kurtosis/rebroadcast/params.yml" \
  /tmp/kurtosis-pos
```

Wait until the current Bor nodes pass block 128. The v2.9.0 node should stop at
block 127 when it rejects the Austin fork boundary. The `tx_spammer` service
keeps transaction gossip active during the observation.

## Add delay and observe

The baseline RPC node uses Bor v2.9.0. Apply 1.5 seconds of delay for 90
seconds:

```bash
tests/kurtosis/rebroadcast/observe.sh
```

For an in-sync control, run the same observation against the candidate RPC
node. Its seeded transaction will normally be mined before it becomes eligible
for rebroadcast:

```bash
SERVICE=l2-el-4-bor-heimdall-v2-rpc tests/kurtosis/rebroadcast/observe.sh
```

The script submits one transaction directly to the selected node, then reports
both batches identified by the txpool and batches emitted by the P2P handler.
The hard-fork-halted baseline remains connected and repeatedly emits that
transaction. The candidate's suppression at the sync transition is covered by
the focused `eth` package tests; delay alone may be too small to make this local
network require a catch-up sync.

The delay is applied from a short-lived container sharing the target's network
namespace, so the Bor image does not need `tc` or additional Linux capabilities.
The cleanup trap removes the rule when the script exits. Set `SEED_TX=false` to
observe an existing txpool without submitting another transaction.

Remove the devnet when finished:

```bash
kurtosis enclave rm --force rebroadcast
```
