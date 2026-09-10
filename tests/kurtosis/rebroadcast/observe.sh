#!/usr/bin/env bash
set -euo pipefail

enclave=${ENCLAVE:-rebroadcast}
service=${SERVICE:-l2-el-5-bor-heimdall-v2-rpc}
delay=${DELAY:-1500ms}
duration=${DURATION:-90}
tc_image=${TC_IMAGE:-gaiadocker/iproute2:3.3}
cast_image=${CAST_IMAGE:-ghcr.io/foundry-rs/foundry@sha256:0c00cb0bda1ab1b91c9a6bf60f4c76c09c1a8870824b6d4718afbabacf6f9a17}
seed_tx=${SEED_TX:-true}

containers=$(docker ps --format '{{.Names}}')
container=$(printf '%s\n' "$containers" | grep -F -- "$service" | head -n 1 || true)
if [[ -z "$container" ]]; then
  echo "No running container found for service $service" >&2
  exit 1
fi

cleanup() {
  docker run --rm --network "container:$container" --cap-add NET_ADMIN \
    "$tc_image" qdisc del dev eth0 root >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

before=$(kurtosis service logs "$enclave" "$service" --all --match 'Rebroadcast stuck transactions' | wc -l | tr -d ' ')

if [[ "$seed_tx" == "true" ]]; then
  private_key=$(kurtosis service logs "$enclave" l2-tx-spammer --all --match 'PRIVATE_KEY' | sed -n 's/.*PRIVATE_KEY: \(0x[0-9a-fA-F]*\).*/\1/p' | head -1)
  if [[ -z "$private_key" ]]; then
    echo "Could not find the devnet transaction-spammer key" >&2
    exit 1
  fi
  rpc_url=$(kurtosis port print "$enclave" "$service" rpc)
  docker_rpc_url=${rpc_url/127.0.0.1/host.docker.internal}
  sender=$(docker run --rm --entrypoint cast "$cast_image" wallet address --private-key "$private_key")
  nonce_hex=$(curl -s "$rpc_url" -H 'content-type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getTransactionCount\",\"params\":[\"$sender\",\"pending\"],\"id\":1}" \
    | sed -n 's/.*"result":"\(0x[0-9a-fA-F]*\)".*/\1/p')
  if [[ -z "$nonce_hex" ]]; then
    echo "Could not read the pending nonce for $sender" >&2
    exit 1
  fi
  tx_hash=$(docker run --rm --add-host host.docker.internal:host-gateway --entrypoint cast "$cast_image" send --async \
    --rpc-url "$docker_rpc_url" --private-key "$private_key" --legacy \
    --nonce "$((nonce_hex))" --gas-price 30000000000 \
    0x000000000000000000000000000000000000dEaD \
    --value 1)
  echo "Submitted observation transaction $tx_hash"
fi

docker run --rm --network "container:$container" --cap-add NET_ADMIN \
  "$tc_image" qdisc add dev eth0 root netem delay "$delay"

echo "Applied $delay delay to $service for ${duration}s"
sleep "$duration"

identified=$(kurtosis service logs "$enclave" "$service" --all --match 'Identified stuck transactions for rebroadcast' | wc -l | tr -d ' ')
after=$(kurtosis service logs "$enclave" "$service" --all --match 'Rebroadcast stuck transactions' | wc -l | tr -d ' ')

echo "Stuck-transaction batches identified: $identified"
echo "Rebroadcast batches during observation: $((after - before))"
echo "Inspect current sync status with:"
echo "curl \"$(kurtosis port print "$enclave" "$service" rpc)\" -H 'content-type: application/json' --data '{\"jsonrpc\":\"2.0\",\"method\":\"eth_syncing\",\"params\":[],\"id\":1}'"
