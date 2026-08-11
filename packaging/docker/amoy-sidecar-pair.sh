#!/usr/bin/env sh

set -eu

compose_file="${COMPOSE_FILE:-docker-compose.amoy-sidecar-pair.yml}"
gocache_dir="${GOCACHE_DIR:-/Users/djones/Github/bor/.gocache}"
gomodcache_dir="${GOMODCACHE_DIR:-/Users/djones/Github/bor/.gomodcache}"
log_tail_lines="${LOG_TAIL_LINES:-2000}"

rpc() {
	service="$1"
	method="$2"
	params="${3:-[]}"
	payload=$(printf '{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}' "${method}" "${params}")
	docker compose -f "${compose_file}" exec -T "${service}" sh -lc \
		"wget -qO- --header='Content-Type: application/json' --post-data='${payload}' http://127.0.0.1:8545"
}

hex_result() {
	printf '%s' "$1" | sed -n 's/.*"result":"\([^"]*\)".*/\1/p'
}

string_field() {
	field="$1"
	sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p"
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

run_gossip_checks() {
	trigger_since=$(log_since_time)
	rpc bor-a "admin_triggerTxGossip" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=(2|8)' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-bulk.*code=(2|8)' "${trigger_since}" 30

	trigger_since=$(log_since_time)
	rpc bor-a "admin_triggerBlockAnnouncement" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=(1|7)' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-bulk.*code=(1|7)' "${trigger_since}" 30
}

run_fetcher_checks() {
	trigger_since=$(log_since_time)
	rpc bor-a "admin_triggerTxFetch" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=9' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-bulk.*code=9' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=eth-bulk.*code=10' "${trigger_since}" 30
	wait_for_log_since bor-a 'Bulk sidecar read message.*channel=eth-bulk.*code=10' "${trigger_since}" 30

	trigger_since=$(log_since_time)
	rpc bor-a "admin_triggerBlockBodyFetch" >/dev/null
	wait_for_log_since bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=5' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar read message.*channel=eth-bulk.*code=5' "${trigger_since}" 30
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=eth-bulk.*code=6' "${trigger_since}" 30
	wait_for_log_since bor-a 'Bulk sidecar read message.*channel=eth-bulk.*code=6' "${trigger_since}" 30
}

verify_pair_ready() {
	bor_a_id=$(rpc bor-a "admin_nodeInfo" | string_field "id")
	bor_b_id=$(rpc bor-b "admin_nodeInfo" | string_field "id")
	wait_for_mutual_peer bor-a "${bor_b_id}"
	wait_for_mutual_peer bor-b "${bor_a_id}"
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
	rpc "${service}" "${method}" >/dev/null
	wait_for_log_since "${log_service}" "${pattern}" "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "$((end_ms - start_ms))"
}

cmd="${1:-up}"

case "${cmd}" in
up)
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
	run_gossip_checks
	run_fetcher_checks
	echo "amoy sidecar pair check passed"
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
		run_gossip_checks
		run_fetcher_checks
		wait_for_mutual_peer bor-a "$(rpc bor-b "admin_nodeInfo" | string_field "id")"
		wait_for_mutual_peer bor-b "$(rpc bor-a "admin_nodeInfo" | string_field "id")"
		echo "soak iteration ${i}/${iterations} passed"
		i=$((i + 1))
	done
	echo "amoy sidecar soak passed"
	;;
fallback)
	run_fallback_tests
	echo "fallback verification passed"
	;;
measure)
	verify_pair_ready
	echo "tx_gossip_ms=$(measure_trigger bor-a admin_triggerTxGossip bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=(2|8)')"
	echo "block_announcement_ms=$(measure_trigger bor-a admin_triggerBlockAnnouncement bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=(1|7)')"
	echo "tx_fetch_request_ms=$(measure_trigger bor-a admin_triggerTxFetch bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=9')"
	echo "block_body_request_ms=$(measure_trigger bor-a admin_triggerBlockBodyFetch bor-a 'Bulk sidecar wrote message.*channel=eth-bulk.*code=5')"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc bor-a "admin_triggerBlockBodyFetch" >/dev/null
	wait_for_log_since bor-b 'Bulk sidecar wrote message.*channel=eth-bulk.*code=6' "${trigger_since}" 30
	end_ms=$(now_ms)
	echo "block_body_response_ms=$((end_ms - start_ms))"
	;;
status)
	echo "bor-a peer count: $(hex_result "$(rpc bor-a "net_peerCount")")"
	echo "bor-b peer count: $(hex_result "$(rpc bor-b "net_peerCount")")"
	;;
logs)
	docker compose -f "${compose_file}" logs -f bor-a bor-b
	;;
down)
	docker compose -f "${compose_file}" down -v
	;;
*)
	echo "usage: $0 {up|pair|check|check-fetchers|soak [iterations]|fallback|measure|status|logs|down}" >&2
	exit 1
	;;
esac
