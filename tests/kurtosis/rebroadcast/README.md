# Out-of-sync transaction rebroadcast observation

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
