#!/usr/bin/env bash

set -euo pipefail

BOR_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HEIMDALL_ROOT="${HEIMDALL_ROOT:-$(cd "${BOR_ROOT}/../heimdall-v2" && pwd)}"

BOR_GOCACHE="${BOR_GOCACHE:-/private/tmp/bor-gocache}"
BOR_GOMODCACHE="${BOR_GOMODCACHE:-/private/tmp/bor-gomodcache}"
HEIMDALL_GOCACHE="${HEIMDALL_GOCACHE:-/private/tmp/heimdall-v2-gocache}"
HEIMDALL_GOMODCACHE="${HEIMDALL_GOMODCACHE:-/private/tmp/heimdall-v2-gomodcache}"
TEST_CHAIN="${TEST_CHAIN:-local}"
SOAK_ITERATIONS="${SOAK_ITERATIONS:-1}"
SOAK_SLEEP_SECONDS="${SOAK_SLEEP_SECONDS:-0}"
RUN_REAL_BOR="${RUN_REAL_BOR:-false}"
BOR_SERVER_SECONDS="${BOR_SERVER_SECONDS:-120}"

HEIMDALL_HOME="$(mktemp -d /private/tmp/heimdall-quic-home.XXXXXX)"
HEIMDALL_LOG="${HEIMDALL_HOME}/heimdalld.log"
HEIMDALL_PID=""
BOR_DATADIR=""
BOR_LOG=""
BOR_PID=""

reserve_port() {
  python3 - <<'PY'
import socket

sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
}

API_PORT="${API_PORT:-$(reserve_port)}"
COMET_RPC_PORT="${COMET_RPC_PORT:-$(reserve_port)}"
P2P_PORT="${P2P_PORT:-$(reserve_port)}"
GRPC_PORT="${GRPC_PORT:-$(reserve_port)}"
PPROF_PORT="${PPROF_PORT:-$(reserve_port)}"
PROM_PORT="${PROM_PORT:-$(reserve_port)}"
BOR_HTTP_PORT="${BOR_HTTP_PORT:-$(reserve_port)}"
BOR_WS_PORT="${BOR_WS_PORT:-$(reserve_port)}"
BOR_AUTHRPC_PORT="${BOR_AUTHRPC_PORT:-$(reserve_port)}"
BOR_GRPC_PORT="${BOR_GRPC_PORT:-$(reserve_port)}"
BOR_P2P_PORT="${BOR_P2P_PORT:-$(reserve_port)}"
BOR_BULK_PORT="${BOR_BULK_PORT:-$(reserve_port)}"

cleanup() {
  stop_bor || true
  stop_heimdall || true
}
trap cleanup EXIT

build_heimdall() {
  (
    cd "${HEIMDALL_ROOT}"
    env GOCACHE="${HEIMDALL_GOCACHE}" GOMODCACHE="${HEIMDALL_GOMODCACHE}" make build
  )
}

build_bor_tests() {
  (
    cd "${BOR_ROOT}"
    env GOCACHE="${BOR_GOCACHE}" GOMODCACHE="${BOR_GOMODCACHE}" \
      go test ./consensus/bor/heimdall -run '^Test(NewHeimdallClientFetchStatusOverQUIC|ExternalHeimdallFetchStatusOverQUIC|ExternalHeimdallEndpointSuiteOverQUIC)$' -count=1 >/dev/null
  )
}

chain_id_for_test() {
  case "${TEST_CHAIN}" in
    amoy)
      printf '%s\n' "heimdallv2-80002"
      ;;
    *)
      printf '%s\n' "heimdall-local"
      ;;
  esac
}

bor_chain_id_for_test() {
  case "${TEST_CHAIN}" in
    amoy)
      printf '%s\n' "80002"
      ;;
    *)
      printf '%s\n' "15001"
      ;;
  esac
}

heimdall_chain_id_for_test() {
  case "${TEST_CHAIN}" in
    amoy)
      printf '%s\n' "heimdallv2-80002"
      ;;
    *)
      printf '%s\n' "heimdall-15001"
      ;;
  esac
}

init_heimdall_home() {
  local init_chain_id
  init_chain_id="$(chain_id_for_test)"

  (
    cd "${HEIMDALL_ROOT}"
    ./build/heimdalld init "quic-${TEST_CHAIN}" --home "${HEIMDALL_HOME}" --chain-id "${init_chain_id}" >/dev/null
  )

  python3 - "${HEIMDALL_HOME}/config/genesis.json" "$(bor_chain_id_for_test)" "$(heimdall_chain_id_for_test)" <<'PY'
import json
import sys

path = sys.argv[1]
bor_chain_id = sys.argv[2]
heimdall_chain_id = sys.argv[3]
with open(path, "r", encoding="utf-8") as fh:
    genesis = json.load(fh)

app_state = genesis["app_state"]
validator_set = app_state["stake"]["current_validator_set"]
proposer = validator_set["proposer"]
app_state["chainmanager"]["params"]["chain_params"]["bor_chain_id"] = bor_chain_id
app_state["chainmanager"]["params"]["chain_params"]["heimdall_chain_id"] = heimdall_chain_id

app_state["bor"]["spans"] = [
    {
        "id": "0",
        "start_block": "0",
        "end_block": "255",
        "validator_set": validator_set,
        "selected_producers": [proposer],
        "bor_chain_id": bor_chain_id,
    },
    {
        "id": "1",
        "start_block": "256",
        "end_block": "511",
        "validator_set": validator_set,
        "selected_producers": [proposer],
        "bor_chain_id": bor_chain_id,
    },
    {
        "id": "2",
        "start_block": "512",
        "end_block": "767",
        "validator_set": validator_set,
        "selected_producers": [proposer],
        "bor_chain_id": bor_chain_id,
    },
]

app_state["checkpoint"]["ack_count"] = "2"
app_state["checkpoint"]["checkpoints"] = [
    {
        "id": "1",
        "proposer": proposer["signer"],
        "start_block": "0",
        "end_block": "255",
        "root_hash": "ERERERERERERERERERERERERERERERERERERERERERE=",
        "bor_chain_id": bor_chain_id,
        "timestamp": "1726150007",
    },
    {
        "id": "2",
        "proposer": proposer["signer"],
        "start_block": "256",
        "end_block": "511",
        "root_hash": "IiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiI=",
        "bor_chain_id": bor_chain_id,
        "timestamp": "1726150549",
    },
]

app_state["milestone"]["milestones"] = [
    {
        "proposer": proposer["signer"],
        "start_block": "0",
        "end_block": "127",
        "hash": "qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqo=",
        "bor_chain_id": bor_chain_id,
        "milestone_id": "quic-milestone-1",
        "timestamp": "1726150300",
    },
    {
        "proposer": proposer["signer"],
        "start_block": "128",
        "end_block": "255",
        "hash": "u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7s=",
        "bor_chain_id": bor_chain_id,
        "milestone_id": "quic-milestone-2",
        "timestamp": "1726150600",
    },
]

app_state["clerk"]["event_records"] = [
    {
        "id": "1",
        "contract": "0x0000000000000000000000000000000000001010",
        "data": "EREiIjMzRERVVWZmd3eIiJmZqqq7u8zM3d3u7v//8AAA",
        "tx_hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "log_index": "0",
        "bor_chain_id": bor_chain_id,
        "record_time": "2024-09-13T07:37:37.502805347Z",
    },
    {
        "id": "2",
        "contract": "0x0000000000000000000000000000000000001010",
        "data": "mZmqqru7zMzd3e7u//8AABERIiIzM0REVVVmdnd3iIg=",
        "tx_hash": "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        "log_index": "1",
        "bor_chain_id": bor_chain_id,
        "record_time": "2024-09-13T07:47:34.598919470Z",
    },
]
app_state["clerk"]["record_sequences"] = ["1000000000", "1000000001"]

with open(path, "w", encoding="utf-8") as fh:
    json.dump(genesis, fh, indent=2)
    fh.write("\n")
PY

  perl -0pi -e \
    "s/address = \"tcp:\\/\\/0\\.0\\.0\\.0:1317\"/address = \"tcp:\\/\\/127.0.0.1:${API_PORT}\"/; \
     s/address = \"localhost:9090\"/address = \"127.0.0.1:${GRPC_PORT}\"/; \
     s/enable_quic_sidecar = \"false\"/enable_quic_sidecar = \"true\"/; \
     s/comet_bft_rpc_url = \"http:\\/\\/0\\.0\\.0\\.0:26657\"/comet_bft_rpc_url = \"http:\\/\\/127.0.0.1:${COMET_RPC_PORT}\"/; \
     s/bor_rpc_url = \"http:\\/\\/localhost:8545\"/bor_rpc_url = \"http:\\/\\/127.0.0.1:${BOR_HTTP_PORT}\"/; \
     s/chain = \"amoy\"/chain = \"${TEST_CHAIN}\"/" \
    "${HEIMDALL_HOME}/config/app.toml"

  perl -0pi -e \
    "s/laddr = \"tcp:\\/\\/0\\.0\\.0\\.0:26657\"/laddr = \"tcp:\\/\\/127.0.0.1:${COMET_RPC_PORT}\"/; \
     s/laddr = \"tcp:\\/\\/0\\.0\\.0\\.0:26656\"/laddr = \"tcp:\\/\\/127.0.0.1:${P2P_PORT}\"/; \
     s/pprof_laddr = \"localhost:6060\"/pprof_laddr = \"localhost:${PPROF_PORT}\"/; \
     s/prometheus_listen_addr = \":26660\"/prometheus_listen_addr = \":${PROM_PORT}\"/; \
     s/seeds = \".*\"/seeds = \"\"/; \
     s/pex = true/pex = false/" \
    "${HEIMDALL_HOME}/config/config.toml"
}

start_heimdall() {
  (
    cd "${HEIMDALL_ROOT}"
    ./build/heimdalld start --home "${HEIMDALL_HOME}" >"${HEIMDALL_LOG}" 2>&1
  ) &
  HEIMDALL_PID=$!
}

heimdall_listener_pids() {
  lsof -tiTCP:"${COMET_RPC_PORT}" -sTCP:LISTEN 2>/dev/null || true
}

read_heimdall_listener_pids() {
  local pid
  HEIMDALL_LISTENER_PIDS=()
  while IFS= read -r pid; do
    [[ -n "${pid}" ]] && HEIMDALL_LISTENER_PIDS+=("${pid}")
  done < <(heimdall_listener_pids)
}

stop_heimdall() {
  read_heimdall_listener_pids

  if [[ -n "${HEIMDALL_PID}" ]]; then
    kill "${HEIMDALL_PID}" 2>/dev/null || true
    wait "${HEIMDALL_PID}" 2>/dev/null || true
  fi

  if (( ${#HEIMDALL_LISTENER_PIDS[@]} > 0 )); then
    kill "${HEIMDALL_LISTENER_PIDS[@]}" 2>/dev/null || true
  fi

  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    read_heimdall_listener_pids
    if (( ${#HEIMDALL_LISTENER_PIDS[@]} == 0 )); then
      HEIMDALL_PID=""
      sleep 1
      return 0
    fi
    sleep 1
  done

  if (( ${#HEIMDALL_LISTENER_PIDS[@]} > 0 )); then
    kill -9 "${HEIMDALL_LISTENER_PIDS[@]}" 2>/dev/null || true
  fi

  wait_for_ports_to_close
  HEIMDALL_PID=""
}

bor_listener_pids() {
  {
    lsof -tiTCP:"${BOR_P2P_PORT}" -sTCP:LISTEN 2>/dev/null || true
    lsof -tiTCP:"${BOR_HTTP_PORT}" -sTCP:LISTEN 2>/dev/null || true
    lsof -tiTCP:"${BOR_GRPC_PORT}" -sTCP:LISTEN 2>/dev/null || true
    lsof -tiTCP:"${BOR_AUTHRPC_PORT}" -sTCP:LISTEN 2>/dev/null || true
    lsof -tiTCP:"${BOR_WS_PORT}" -sTCP:LISTEN 2>/dev/null || true
  } | sort -u
}

read_bor_listener_pids() {
  local pid
  BOR_LISTENER_PIDS=()
  while IFS= read -r pid; do
    [[ -n "${pid}" ]] && BOR_LISTENER_PIDS+=("${pid}")
  done < <(bor_listener_pids)
}

start_bor() {
  local chain_arg
  BOR_DATADIR="$(mktemp -d /private/tmp/bor-quic-node.XXXXXX)"
  BOR_LOG="${BOR_DATADIR}/bor.log"

  case "${TEST_CHAIN}" in
    amoy)
      chain_arg="amoy"
      ;;
    *)
      chain_arg="${BOR_ROOT}/tests/bor/testdata/genesis_2val.json"
      ;;
  esac

  (
    cd "${BOR_ROOT}"
    ./build/bin/bor server \
      -chain "${chain_arg}" \
      -datadir "${BOR_DATADIR}" \
      -bor.heimdall "h3://127.0.0.1:${API_PORT}" \
      -bor.heimdalltimeout 5s \
      -maxpeers 0 \
      -nodiscover \
      -port "${BOR_P2P_PORT}" \
      -http -http.addr 127.0.0.1 -http.port "${BOR_HTTP_PORT}" \
      -ws -ws.addr 127.0.0.1 -ws.port "${BOR_WS_PORT}" \
      -authrpc.addr 127.0.0.1 -authrpc.port "${BOR_AUTHRPC_PORT}" \
      -grpc.addr "127.0.0.1:${BOR_GRPC_PORT}" \
      -bulk-sidecar -bulk-port "${BOR_BULK_PORT}" \
      -verbosity 4 >"${BOR_LOG}" 2>&1
  ) &
  BOR_PID=$!
}

stop_bor() {
  read_bor_listener_pids

  if [[ -n "${BOR_PID}" ]]; then
    kill "${BOR_PID}" 2>/dev/null || true
    wait "${BOR_PID}" 2>/dev/null || true
  fi

  if (( ${#BOR_LISTENER_PIDS[@]} > 0 )); then
    kill "${BOR_LISTENER_PIDS[@]}" 2>/dev/null || true
  fi

  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    read_bor_listener_pids
    if (( ${#BOR_LISTENER_PIDS[@]} == 0 )); then
      BOR_PID=""
      sleep 1
      return 0
    fi
    sleep 1
  done

  if (( ${#BOR_LISTENER_PIDS[@]} > 0 )); then
    kill -9 "${BOR_LISTENER_PIDS[@]}" 2>/dev/null || true
  fi

  wait_for_bor_ports_to_close
  BOR_PID=""
}

wait_for_bor() {
  local deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if curl -sSf "http://127.0.0.1:${BOR_HTTP_PORT}/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  echo "Bor failed to become ready. Recent log output:" >&2
  tail -n 120 "${BOR_LOG}" >&2 || true
  return 1
}

wait_for_bor_ports_to_close() {
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if ! lsof -iTCP:"${BOR_P2P_PORT}" -sTCP:LISTEN >/dev/null 2>&1 \
      && ! lsof -iTCP:"${BOR_HTTP_PORT}" -sTCP:LISTEN >/dev/null 2>&1 \
      && ! lsof -iTCP:"${BOR_GRPC_PORT}" -sTCP:LISTEN >/dev/null 2>&1 \
      && ! lsof -iTCP:"${BOR_AUTHRPC_PORT}" -sTCP:LISTEN >/dev/null 2>&1 \
      && ! lsof -iTCP:"${BOR_WS_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  echo "Timed out waiting for Bor ports to close" >&2
  return 1
}

wait_for_ports_to_close() {
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if ! lsof -iTCP:"${COMET_RPC_PORT}" -sTCP:LISTEN >/dev/null 2>&1 \
      && ! lsof -iTCP:"${API_PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  echo "Timed out waiting for Heimdall ports to close" >&2
  return 1
}

wait_for_heimdall() {
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    if curl -sSf "http://127.0.0.1:${API_PORT}/status" >/dev/null 2>&1; then
      if grep -q "Starting Heimdall QUIC sidecar" "${HEIMDALL_LOG}"; then
        return 0
      fi
    fi
    sleep 1
  done

  echo "Heimdall failed to become ready. Recent log output:" >&2
  tail -n 120 "${HEIMDALL_LOG}" >&2 || true
  return 1
}

run_bor_live_suite() {
  (
    cd "${BOR_ROOT}"
    env BOR_HEIMDALL_H3_URL="h3://127.0.0.1:${API_PORT}" \
      BOR_HEIMDALL_EXPECT_RICH_STATE=1 \
      GOCACHE="${BOR_GOCACHE}" \
      GOMODCACHE="${BOR_GOMODCACHE}" \
      go test ./consensus/bor/heimdall \
        -run '^Test(ExternalHeimdallFetchStatusOverQUIC|ExternalHeimdallEndpointSuiteOverQUIC)$' \
        -count=1 -v
  )
}

run_live_suite_iterations() {
  local iteration
  for (( iteration = 1; iteration <= SOAK_ITERATIONS; iteration++ )); do
    echo "running Bor live QUIC endpoint suite (${iteration}/${SOAK_ITERATIONS})"
    run_bor_live_suite
    if (( SOAK_SLEEP_SECONDS > 0 && iteration < SOAK_ITERATIONS )); then
      sleep "${SOAK_SLEEP_SECONDS}"
    fi
  done
}

run_real_bor_soak() {
  if [[ "${RUN_REAL_BOR}" != "true" ]]; then
    return 0
  fi

  echo "starting real Bor process against Heimdall QUIC sidecar"
  start_bor
  wait_for_bor

  echo "soaking real Bor process for ${BOR_SERVER_SECONDS}s"
  sleep "${BOR_SERVER_SECONDS}"

  echo "recent Bor log output"
  tail -n 60 "${BOR_LOG}"

  stop_bor
}

main() {
  echo "building Heimdall binary for chain=${TEST_CHAIN}"
  build_heimdall

  echo "priming Bor live QUIC tests"
  build_bor_tests

  echo "initializing throwaway Heimdall home at ${HEIMDALL_HOME}"
  init_heimdall_home

  echo "starting Heimdall with QUIC sidecar"
  start_heimdall
  wait_for_heimdall

  run_live_suite_iterations
  run_real_bor_soak

  echo "restarting Heimdall to verify QUIC recovery"
  stop_heimdall

  start_heimdall
  wait_for_heimdall

  run_live_suite_iterations
  run_real_bor_soak

  echo "QUIC Heimdall/Bor integration check passed"
}

main "$@"
