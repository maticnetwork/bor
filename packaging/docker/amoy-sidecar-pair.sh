#!/usr/bin/env sh

set -eu

compose_file="${COMPOSE_FILE:-docker-compose.amoy-sidecar-pair.yml}"

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
	attempt=0
	while [ "${attempt}" -lt 90 ]; do
		if docker compose -f "${compose_file}" logs "${service}" 2>/dev/null | grep -q "${pattern}"; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "log pattern not observed for ${service}: ${pattern}" >&2
	return 1
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
	bor_a_id=$(rpc bor-a "admin_nodeInfo" | string_field "id")
	bor_b_id=$(rpc bor-b "admin_nodeInfo" | string_field "id")
	wait_for_mutual_peer bor-a "${bor_b_id}"
	wait_for_mutual_peer bor-b "${bor_a_id}"
	wait_for_log bor-a "Bulk sidecar session established"
	wait_for_log bor-b "Bulk sidecar session established"
	wait_for_log bor-a "Bulk sidecar channel opened.*eth-bulk"
	wait_for_log bor-b "Bulk sidecar channel opened.*eth-bulk"
	wait_for_log bor-a "Bulk sidecar channel opened.*snap-bulk"
	wait_for_log bor-b "Bulk sidecar channel opened.*snap-bulk"
	echo "amoy sidecar pair check passed"
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
	echo "usage: $0 {up|pair|check|status|logs|down}" >&2
	exit 1
	;;
esac
