#!/usr/bin/env sh

set -eu

compose_file="${COMPOSE_FILE:-docker-compose.bulk-test.yml}"

cleanup() {
	docker compose -f "${compose_file}" down -v >/dev/null 2>&1 || true
}

trap cleanup EXIT

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
	while [ "${attempt}" -lt 60 ]; do
		if rpc "${service}" "web3_clientVersion" >/dev/null 2>&1; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "rpc for ${service} did not become ready" >&2
	return 1
}

wait_for_peer() {
	service="$1"
	attempt=0
	while [ "${attempt}" -lt 60 ]; do
		count_hex=$(hex_result "$(rpc "${service}" "net_peerCount")")
		count_dec=$((16#${count_hex#0x}))
		if [ "${count_dec}" -ge 1 ]; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "peer count for ${service} did not reach 1" >&2
	return 1
}

wait_for_log() {
	service="$1"
	pattern="$2"
	attempt=0
	while [ "${attempt}" -lt 60 ]; do
		if docker compose -f "${compose_file}" logs "${service}" 2>/dev/null | grep -q "${pattern}"; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "log pattern not observed for ${service}: ${pattern}" >&2
	return 1
}

docker compose -f "${compose_file}" build bor-a
docker compose -f "${compose_file}" up -d bor-a bor-b

wait_for_rpc bor-a
wait_for_rpc bor-b

bor_a_enr=$(rpc bor-a "admin_nodeInfo" | string_field "enr")
bor_b_enr=$(rpc bor-b "admin_nodeInfo" | string_field "enr")
if [ -z "${bor_a_enr}" ] || [ -z "${bor_b_enr}" ]; then
	echo "failed to read node enr" >&2
	exit 1
fi

rpc bor-b "admin_addPeer" "[\"${bor_a_enr}\"]" >/dev/null
rpc bor-a "admin_addPeer" "[\"${bor_b_enr}\"]" >/dev/null

wait_for_peer bor-a
wait_for_peer bor-b

wait_for_log bor-a "Bulk sidecar session established"
wait_for_log bor-b "Bulk sidecar session established"
wait_for_log bor-a "Bulk sidecar channel opened.*eth-bulk"
wait_for_log bor-b "Bulk sidecar channel opened.*eth-bulk"
wait_for_log bor-a "Bulk sidecar channel opened.*snap-bulk"
wait_for_log bor-b "Bulk sidecar channel opened.*snap-bulk"
wait_for_log bor-a "Bulk sidecar channel opened.*wit-bulk"
wait_for_log bor-b "Bulk sidecar channel opened.*wit-bulk"

echo "bulk sidecar docker test passed"
