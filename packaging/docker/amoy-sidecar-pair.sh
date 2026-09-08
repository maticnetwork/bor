#!/usr/bin/env sh

set -eu

compose_file="${COMPOSE_FILE:-docker-compose.amoy-sidecar-pair.yml}"
gocache_dir="${GOCACHE_DIR:-/Users/djones/Github/bor/.gocache}"
gomodcache_dir="${GOMODCACHE_DIR:-/Users/djones/Github/bor/.gomodcache}"
log_tail_lines="${LOG_TAIL_LINES:-2000}"
preserve_volumes="${PRESERVE_VOLUMES:-0}"

rpc() {
	service="$1"
	method="$2"
	params="${3:-[]}"
	payload=$(printf '{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}' "${method}" "${params}")
	docker compose -f "${compose_file}" exec -T "${service}" sh -lc \
		"wget -qO- --header='Content-Type: application/json' --post-data='${payload}' http://127.0.0.1:8545"
}

rpc_ok() {
	service="$1"
	method="$2"
	params="${3:-[]}"
	response=$(rpc "${service}" "${method}" "${params}")
	if printf '%s' "${response}" | grep -q '"error"'; then
		echo "rpc ${service} ${method} failed: ${response}" >&2
		return 1
	fi
	if ! printf '%s' "${response}" | grep -q '"result"'; then
		echo "rpc ${service} ${method} returned no result: ${response}" >&2
		return 1
	fi
	printf '%s\n' "${response}"
}

hex_result() {
	printf '%s' "$1" | sed -n 's/.*"result":"\([^"]*\)".*/\1/p'
}

string_field() {
	field="$1"
	sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p"
}

require_json_pattern() {
	json="$1"
	pattern="$2"
	label="$3"
	if ! printf '%s' "${json}" | grep -E -q "${pattern}"; then
		echo "missing expected ${label}: ${pattern}" >&2
		return 1
	fi
}

wait_for_rpc() {
	service="$1"
	attempt=0
	while [ "${attempt}" -lt 90 ]; do
		if rpc "${service}" "web3_clientVersion" >/dev/null 2>&1; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "rpc for ${service} did not become ready" >&2
	return 1
}

wait_for_log() {
	service="$1"
	pattern="$2"
	attempt_limit="${3:-90}"
	attempt=0
	while [ "${attempt}" -lt "${attempt_limit}" ]; do
		if docker compose -f "${compose_file}" logs --tail "${log_tail_lines}" "${service}" 2>/dev/null | grep -E -q "${pattern}"; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "log pattern not observed for ${service}: ${pattern}" >&2
	return 1
}

wait_for_log_since() {
	service="$1"
	pattern="$2"
	since="$3"
	attempt_limit="${4:-90}"
	attempt=0
	while [ "${attempt}" -lt "${attempt_limit}" ]; do
		if docker compose -f "${compose_file}" logs --since "${since}" --tail "${log_tail_lines}" "${service}" 2>/dev/null | grep -E -q "${pattern}"; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "log pattern not observed for ${service} since ${since}: ${pattern}" >&2
	return 1
}

wait_for_channel_open_since() {
	service="$1"
	channel="$2"
	since="$3"
	wait_for_log_since "${service}" "Bulk sidecar channel opened.*channel=${channel}" "${since}" 90
}

wait_for_session_since() {
	service="$1"
	since="$2"
	wait_for_log_since "${service}" 'Bulk sidecar session established' "${since}" 90
}

wait_for_log_either() {
	pattern="$1"
	attempt_limit="${2:-90}"
	attempt=0
	while [ "${attempt}" -lt "${attempt_limit}" ]; do
		if docker compose -f "${compose_file}" logs --tail "${log_tail_lines}" bor-a bor-b 2>/dev/null | grep -E -q "${pattern}"; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "log pattern not observed for bor-a or bor-b: ${pattern}" >&2
	return 1
}

now_ms() {
	python3 -c 'import time; print(int(time.time() * 1000))'
}

log_since_time() {
	python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(seconds=2)).strftime("%Y-%m-%dT%H:%M:%SZ"))'
}

wait_for_mutual_peer() {
	service="$1"
	target_id="$2"
	attempt=0
	while [ "${attempt}" -lt 90 ]; do
		peers_json=$(rpc "${service}" "admin_peers")
		if printf '%s' "${peers_json}" | grep -q "\"id\":\"${target_id}\""; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "peer ${target_id} not observed on ${service}" >&2
	return 1
}

pair_nodes() {
	bor_a_enr=$(rpc bor-a "admin_nodeInfo" | string_field "enr")
	bor_b_enr=$(rpc bor-b "admin_nodeInfo" | string_field "enr")
	if [ -z "${bor_a_enr}" ] || [ -z "${bor_b_enr}" ]; then
		echo "failed to read node enr" >&2
		exit 1
	fi
	rpc bor-b "admin_addPeer" "[\"${bor_a_enr}\"]" >/dev/null
	rpc bor-a "admin_addPeer" "[\"${bor_b_enr}\"]" >/dev/null
}

clean_stack() {
	docker compose -f "${compose_file}" down -v >/dev/null 2>&1 || true
}

print_sidecar_status() {
	label="${1:-status}"
	echo "${label} bor-a sidecar status: $(rpc_ok bor-a "admin_bulkSidecarStatus")"
	echo "${label} bor-b sidecar status: $(rpc_ok bor-b "admin_bulkSidecarStatus")"
}

snap_channel_for_code() {
	code="$1"
	case "${code}" in
	0|1) echo "snap-accounts" ;;
	2|3) echo "snap-storage" ;;
	4|5) echo "snap-code" ;;
	6|7) echo "snap-trie" ;;
	*) return 1 ;;
	esac
}

wait_for_snap_exchange_since() {
	request_code="$1"
	response_code="$2"
	trigger_since="$3"
	channel=$(snap_channel_for_code "${request_code}")
	wait_for_log_since bor-a "Bulk sidecar wrote message.*channel=${channel}.*code=${request_code}" "${trigger_since}" 30
	wait_for_log_since bor-b "Bulk sidecar read message.*channel=${channel}.*code=${request_code}" "${trigger_since}" 30
	wait_for_log_since bor-b "Bulk sidecar wrote message.*channel=${channel}.*code=${response_code}" "${trigger_since}" 30
	wait_for_log_since bor-a "Bulk sidecar read message.*channel=${channel}.*code=${response_code}" "${trigger_since}" 30
}

run_gossip_checks() {
	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerTxGossip" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-tx.*code=(2|8)' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-tx.*code=(2|8)' "${trigger_since}" 30

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerBlockAnnouncement" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-blocks.*code=(1|7)' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-blocks.*code=(1|7)' "${trigger_since}" 30
}

run_witness_checks() {
	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerWitnessAnnouncement" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=wit-bulk.*code=1' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=wit-bulk.*code=1' "${trigger_since}" 30

	seed_result=$(rpc_ok bor-b "admin_seedWitnessForHead")
	witness_hash=$(printf '%s' "${seed_result}" | string_field "blockHash")
	if [ -z "${witness_hash}" ]; then
		echo "failed to seed witness for deterministic fetch" >&2
		return 1
	fi

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerWitnessMetadataFetchByHash" "[\"${witness_hash}\"]" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=wit-bulk.*code=4' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=wit-bulk.*code=4' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=wit-bulk.*code=5' "${trigger_since}" 30
	wait_for_log_since bor-a 'Bulk sidecar read message.*channel=wit-bulk.*code=5' "${trigger_since}" 30

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerWitnessFetchByHash" "[\"${witness_hash}\"]" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=wit-bulk.*code=2' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=wit-bulk.*code=2' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=wit-bulk.*code=3' "${trigger_since}" 30
	wait_for_log_since bor-a 'Bulk sidecar read message.*channel=wit-bulk.*code=3' "${trigger_since}" 30
}

run_snap_checks() {
	rpc_ok bor-a "admin_seedSnapTriggerFixtures" >/dev/null
	rpc_ok bor-b "admin_seedSnapTriggerFixtures" >/dev/null

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerSnapAccountRangeFetch" >/dev/null
	wait_for_snap_exchange_since 0 1 "${trigger_since}"

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerSnapStorageRangeFetch" >/dev/null
	wait_for_snap_exchange_since 2 3 "${trigger_since}"

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerSnapByteCodeFetch" >/dev/null
	wait_for_snap_exchange_since 4 5 "${trigger_since}"

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerSnapTrieNodeFetch" >/dev/null
	wait_for_snap_exchange_since 6 7 "${trigger_since}"
}

run_health_checks() {
	status_json=$(rpc_ok bor-a "admin_bulkSidecarStatus")
	require_json_pattern "${status_json}" '"enabled":true' "sidecar enablement"
	require_json_pattern "${status_json}" '"activeSessions":[1-9]' "active sidecar sessions"
	require_json_pattern "${status_json}" '"eth-blocks"' "eth-blocks status"
	require_json_pattern "${status_json}" '"eth-bulk"' "eth-bulk status"
	require_json_pattern "${status_json}" '"eth-control"' "eth-control status"
	require_json_pattern "${status_json}" '"eth-tx"' "eth-tx status"
	require_json_pattern "${status_json}" '"eth-tx-fetch"' "eth-tx-fetch status"
	require_json_pattern "${status_json}" '"snap-accounts"' "snap-accounts status"
	require_json_pattern "${status_json}" '"snap-storage"' "snap-storage status"
	require_json_pattern "${status_json}" '"snap-code"' "snap-code status"
	require_json_pattern "${status_json}" '"snap-trie"' "snap-trie status"
	require_json_pattern "${status_json}" '"wit-bulk"' "wit-bulk status"
}

run_fetcher_checks() {
	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerTxFetch" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-tx-fetch.*code=9' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-tx-fetch.*code=9' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=eth-tx-fetch.*code=10' "${trigger_since}" 30
	wait_for_log_since bor-a 'Bulk sidecar read message.*channel=eth-tx-fetch.*code=10' "${trigger_since}" 30

	trigger_since=$(log_since_time)
	rpc_ok bor-a "admin_triggerBlockBodyFetch" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=5' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-bulk.*code=5' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=eth-bulk.*code=6' "${trigger_since}" 30
	wait_for_log_since bor-a 'Bulk sidecar read message.*channel=eth-bulk.*code=6' "${trigger_since}" 30
}

run_mixed_checks() {
	run_gossip_checks
	run_snap_checks
	run_witness_checks
	run_fetcher_checks
	run_health_checks
}

verify_pair_ready() {
	bor_a_id=$(rpc bor-a "admin_nodeInfo" | string_field "id")
	bor_b_id=$(rpc bor-b "admin_nodeInfo" | string_field "id")
	wait_for_mutual_peer bor-a "${bor_b_id}"
	wait_for_mutual_peer bor-b "${bor_a_id}"
}

wait_for_bulk_channels_since() {
	since="$1"
	wait_for_channel_open_since bor-a eth-blocks "${since}"
	wait_for_channel_open_since bor-b eth-blocks "${since}"
	wait_for_channel_open_since bor-a eth-bulk "${since}"
	wait_for_channel_open_since bor-b eth-bulk "${since}"
	wait_for_channel_open_since bor-a eth-control "${since}"
	wait_for_channel_open_since bor-b eth-control "${since}"
	wait_for_channel_open_since bor-a eth-tx "${since}"
	wait_for_channel_open_since bor-b eth-tx "${since}"
	wait_for_channel_open_since bor-a eth-tx-fetch "${since}"
	wait_for_channel_open_since bor-b eth-tx-fetch "${since}"
	wait_for_channel_open_since bor-a snap-accounts "${since}"
	wait_for_channel_open_since bor-b snap-accounts "${since}"
	wait_for_channel_open_since bor-a snap-storage "${since}"
	wait_for_channel_open_since bor-b snap-storage "${since}"
	wait_for_channel_open_since bor-a snap-code "${since}"
	wait_for_channel_open_since bor-b snap-code "${since}"
	wait_for_channel_open_since bor-a snap-trie "${since}"
	wait_for_channel_open_since bor-b snap-trie "${since}"
	wait_for_channel_open_since bor-a wit-bulk "${since}"
	wait_for_channel_open_since bor-b wit-bulk "${since}"
}

recover_peer() {
	service="${1:-bor-b}"
	recovery_since=$(log_since_time)
	print_sidecar_status "pre-recover"
	docker compose -f "${compose_file}" restart "${service}"
	wait_for_rpc "${service}"
	pair_nodes
	verify_pair_ready
	wait_for_session_since bor-a "${recovery_since}"
	wait_for_session_since "${service}" "${recovery_since}"
	wait_for_bulk_channels_since "${recovery_since}"
	run_mixed_checks
	print_sidecar_status "post-recover"
}

run_fallback_tests() {
	env GOCACHE="${gocache_dir}" GOMODCACHE="${gomodcache_dir}" \
		go test ./p2p -run '^TestRoutedMsgReadWriterFallsBackToPrimaryWhenBulkWriteFails$' -count=1
	env GOCACHE="${gocache_dir}" GOMODCACHE="${gomodcache_dir}" \
		go test ./p2p -run '^TestRoutedMsgReadWriterIgnoresBulkReadErrors$' -count=1
}

measure_trigger() {
	service="$1"
	method="$2"
	log_service="$3"
	pattern="$4"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok "${service}" "${method}" >/dev/null
	wait_for_log_since "${log_service}" "${pattern}" "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "$((end_ms - start_ms))"
}

measure_all() {
	rpc_ok bor-a "admin_seedSnapTriggerFixtures" >/dev/null
	rpc_ok bor-b "admin_seedSnapTriggerFixtures" >/dev/null
	echo "tx_gossip_ms=$(measure_trigger bor-a admin_triggerTxGossip bor-a 'Bulk sidecar wrote message.*channel=eth-tx.*code=(2|8)')"
	echo "block_announcement_ms=$(measure_trigger bor-a admin_triggerBlockAnnouncement bor-a 'Bulk sidecar wrote message.*channel=eth-blocks.*code=(1|7)')"
	echo "witness_announcement_ms=$(measure_trigger bor-a admin_triggerWitnessAnnouncement bor-a 'Bulk sidecar wrote message.*channel=wit-bulk.*code=1')"
	echo "snap_account_request_ms=$(measure_trigger bor-a admin_triggerSnapAccountRangeFetch bor-a 'Bulk sidecar wrote message.*channel=snap-accounts.*code=0')"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerSnapAccountRangeFetch" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=snap-accounts.*code=1' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "snap_account_response_ms=$((end_ms - start_ms))"
	echo "snap_storage_request_ms=$(measure_trigger bor-a admin_triggerSnapStorageRangeFetch bor-a 'Bulk sidecar wrote message.*channel=snap-storage.*code=2')"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerSnapStorageRangeFetch" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=snap-storage.*code=3' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "snap_storage_response_ms=$((end_ms - start_ms))"
	echo "snap_bytecode_request_ms=$(measure_trigger bor-a admin_triggerSnapByteCodeFetch bor-a 'Bulk sidecar wrote message.*channel=snap-code.*code=4')"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerSnapByteCodeFetch" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=snap-code.*code=5' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "snap_bytecode_response_ms=$((end_ms - start_ms))"
	echo "snap_trie_request_ms=$(measure_trigger bor-a admin_triggerSnapTrieNodeFetch bor-a 'Bulk sidecar wrote message.*channel=snap-trie.*code=6')"
	seed_result=$(rpc_ok bor-b "admin_seedWitnessForHead")
	witness_hash=$(printf '%s' "${seed_result}" | string_field "blockHash")
	if [ -z "${witness_hash}" ]; then
		echo "failed to seed witness for witness timing capture" >&2
		exit 1
	fi
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerSnapTrieNodeFetch" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=snap-trie.*code=7' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "snap_trie_response_ms=$((end_ms - start_ms))"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerWitnessMetadataFetchByHash" "[\"${witness_hash}\"]" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=wit-bulk.*code=4' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "witness_metadata_request_ms=$((end_ms - start_ms))"
	echo "tx_fetch_request_ms=$(measure_trigger bor-a admin_triggerTxFetch bor-a 'Bulk sidecar wrote message.*channel=eth-tx-fetch.*code=9')"
	echo "block_body_request_ms=$(measure_trigger bor-a admin_triggerBlockBodyFetch bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=5')"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerBlockBodyFetch" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=eth-bulk.*code=6' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "block_body_response_ms=$((end_ms - start_ms))"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerWitnessMetadataFetchByHash" "[\"${witness_hash}\"]" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=wit-bulk.*code=5' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "witness_metadata_response_ms=$((end_ms - start_ms))"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerWitnessFetchByHash" "[\"${witness_hash}\"]" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=wit-bulk.*code=2' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "witness_fetch_request_ms=$((end_ms - start_ms))"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok bor-a "admin_triggerWitnessFetchByHash" "[\"${witness_hash}\"]" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=wit-bulk.*code=3' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "witness_fetch_response_ms=$((end_ms - start_ms))"
}

measure_batch() {
	rounds="${1:-5}"
	tmp=$(mktemp)
	i=1
	while [ "${i}" -le "${rounds}" ]; do
		measure_all >>"${tmp}"
		i=$((i + 1))
	done
	python3 - "${tmp}" <<'PY'
import statistics
import sys
from collections import defaultdict

path = sys.argv[1]
values = defaultdict(list)
with open(path, "r", encoding="utf-8") as fh:
    for line in fh:
        line = line.strip()
        if not line or "=" not in line:
            continue
        key, raw = line.split("=", 1)
        values[key].append(int(raw))

for key in sorted(values):
    nums = sorted(values[key])
    q = statistics.quantiles(nums, n=100, method="inclusive") if len(nums) > 1 else [nums[0]] * 99
    p50 = q[49]
    p95 = q[94]
    print(f"{key}: count={len(nums)} min={nums[0]} p50={p50} p95={p95} max={nums[-1]}")
PY
	rm -f "${tmp}"
}

cmd="${1:-up}"

case "${cmd}" in
up)
	if [ "${preserve_volumes}" != "1" ]; then
		clean_stack
	fi
	docker compose -f "${compose_file}" build bor-a
	docker compose -f "${compose_file}" up -d bor-a bor-b
	wait_for_rpc bor-a
	wait_for_rpc bor-b
	pair_nodes
	;;
pair)
	wait_for_rpc bor-a
	wait_for_rpc bor-b
	pair_nodes
	;;
check)
	verify_pair_ready
	run_mixed_checks
	echo "amoy sidecar pair check passed"
	;;
check-witness)
	verify_pair_ready
	run_witness_checks
	echo "amoy sidecar witness check passed"
	;;
check-fetchers)
	verify_pair_ready
	run_fetcher_checks
	echo "amoy sidecar fetcher check passed"
	;;
soak)
	verify_pair_ready
	iterations="${2:-10}"
	i=1
	while [ "${i}" -le "${iterations}" ]; do
		run_mixed_checks
		wait_for_mutual_peer bor-a "$(rpc bor-b "admin_nodeInfo" | string_field "id")"
		wait_for_mutual_peer bor-b "$(rpc bor-a "admin_nodeInfo" | string_field "id")"
		echo "soak iteration ${i}/${iterations} passed"
		i=$((i + 1))
	done
	echo "amoy sidecar soak passed"
	;;
recover)
	verify_pair_ready
	recover_peer "${2:-bor-b}"
	echo "amoy sidecar recovery check passed"
	;;
soak-long)
	verify_pair_ready
	iterations="${2:-50}"
	recover_every="${3:-10}"
	print_sidecar_status "soak-start"
	i=1
	while [ "${i}" -le "${iterations}" ]; do
		run_mixed_checks
		wait_for_mutual_peer bor-a "$(rpc bor-b "admin_nodeInfo" | string_field "id")"
		wait_for_mutual_peer bor-b "$(rpc bor-a "admin_nodeInfo" | string_field "id")"
		if [ "${recover_every}" -gt 0 ] && [ $((i % recover_every)) -eq 0 ]; then
			recover_peer bor-b
			echo "soak-long recovery ${i}/${iterations} passed"
		fi
		echo "soak-long iteration ${i}/${iterations} passed"
		i=$((i + 1))
	done
	print_sidecar_status "soak-end"
	echo "amoy sidecar long soak passed"
	;;
soak-high-confidence)
	verify_pair_ready
	print_sidecar_status "soak-high-confidence-start"
	"${0}" soak-long 50 10
	;;
fallback)
	run_fallback_tests
	echo "fallback verification passed"
	;;
measure)
	verify_pair_ready
	measure_all
	;;
measure-batch)
	verify_pair_ready
	measure_batch "${2:-5}"
	;;
status)
	echo "bor-a peer count: $(hex_result "$(rpc bor-a "net_peerCount")")"
	echo "bor-b peer count: $(hex_result "$(rpc bor-b "net_peerCount")")"
	print_sidecar_status "status"
	;;
logs)
	docker compose -f "${compose_file}" logs -f bor-a bor-b
	;;
down)
	docker compose -f "${compose_file}" down -v
	;;
*)
	echo "usage: $0 {up|pair|check|check-witness|check-fetchers|soak [iterations]|recover [service]|soak-long [iterations] [recover-every]|soak-high-confidence|fallback|measure|measure-batch [rounds]|status|logs|down}" >&2
	echo "set PRESERVE_VOLUMES=1 to skip the clean-volume reset in up" >&2
	exit 1
	;;
esac
