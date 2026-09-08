#!/usr/bin/env sh

set -eu

compose_file="${COMPOSE_FILE:-docker-compose.amoy-sidecar-pair.yml}"
project_name="${COMPOSE_PROJECT_NAME:-bor}"
log_tail_lines="${LOG_TAIL_LINES:-2000}"
rounds="${ROUNDS:-5}"
benchmark_dir="${BENCHMARK_DIR:-/private/tmp/bor-amoy-sidecar-bench}"

template_a="${TEMPLATE_A:-/Users/djones/Github/bor/packaging/docker/bor-amoy-sidecar-a.toml}"
template_b="${TEMPLATE_B:-/Users/djones/Github/bor/packaging/docker/bor-amoy-sidecar-b.toml}"
template_observer="${TEMPLATE_OBSERVER:-/Users/djones/Github/bor/packaging/docker/bor-amoy-sidecar-observer.toml}"

config_dir="${benchmark_dir}/configs"
mkdir -p "${config_dir}"

bor_a_config="${config_dir}/bor-a.toml"
bor_b_config="${config_dir}/bor-b.toml"
bor_observer_config="${config_dir}/bor-observer.toml"

volume_a="${project_name}_bor-amoy-hash-a-data"
volume_observer="${project_name}_bor-amoy-hash-observer-data"

compose() {
	BOR_A_CONFIG="${bor_a_config}" \
	BOR_B_CONFIG="${bor_b_config}" \
	BOR_OBSERVER_CONFIG="${bor_observer_config}" \
	docker compose -p "${project_name}" -f "${compose_file}" "$@"
}

rpc() {
	service="$1"
	method="$2"
	params="${3:-[]}"
	payload=$(printf '{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}' "${method}" "${params}")
	compose exec -T "${service}" sh -lc \
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

string_field() {
	field="$1"
	sed -n "s/.*\"${field}\":\"\\([^\"]*\\)\".*/\\1/p"
}

hex_result() {
	printf '%s' "$1" | sed -n 's/.*"result":"\([^"]*\)".*/\1/p'
}

hex_to_dec() {
	python3 - "$1" <<'PY'
import sys
value = sys.argv[1]
print(int(value, 16))
PY
}

now_ms() {
	python3 -c 'import time; print(int(time.time() * 1000))'
}

log_since_time() {
	python3 -c 'import datetime; print((datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(seconds=2)).strftime("%Y-%m-%dT%H:%M:%SZ"))'
}

wait_for_rpc() {
	service="$1"
	attempt=0
	while [ "${attempt}" -lt 120 ]; do
		if rpc "${service}" "web3_clientVersion" >/dev/null 2>&1; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "rpc for ${service} did not become ready" >&2
	return 1
}

wait_for_peer_id() {
	service="$1"
	target_id="$2"
	attempt=0
	while [ "${attempt}" -lt 120 ]; do
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

wait_for_log_since() {
	service="$1"
	pattern="$2"
	since="$3"
	attempt_limit="${4:-90}"
	attempt=0
	while [ "${attempt}" -lt "${attempt_limit}" ]; do
		if compose logs --since "${since}" --tail "${log_tail_lines}" "${service}" 2>/dev/null | grep -E -q "${pattern}"; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "log pattern not observed for ${service} since ${since}: ${pattern}" >&2
	return 1
}

wait_for_block() {
	service="$1"
	target="$2"
	attempt=0
	while [ "${attempt}" -lt 600 ]; do
		current_hex=$(hex_result "$(rpc_ok "${service}" "eth_blockNumber")")
		current_dec=$(hex_to_dec "${current_hex}")
		if [ "${current_dec}" -ge "${target}" ]; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "${service} did not reach block ${target}" >&2
	return 1
}

write_profile_config() {
	profile="$1"
	template="$2"
	output="$3"
	if [ "${profile}" = "tcp" ]; then
		sed 's/bulk-sidecar = true/bulk-sidecar = false/' "${template}" >"${output}"
	else
		cp "${template}" "${output}"
	fi
}

set_profile() {
	profile="$1"
	write_profile_config "${profile}" "${template_a}" "${bor_a_config}"
	write_profile_config "${profile}" "${template_b}" "${bor_b_config}"
	write_profile_config "${profile}" "${template_observer}" "${bor_observer_config}"
}

pair_nodes() {
	a_enr=$(rpc_ok bor-a "admin_nodeInfo" | string_field "enr")
	b_enr=$(rpc_ok bor-b "admin_nodeInfo" | string_field "enr")
	observer_enr=$(rpc_ok bor-observer "admin_nodeInfo" | string_field "enr")
	a_id=$(rpc_ok bor-a "admin_nodeInfo" | string_field "id")
	b_id=$(rpc_ok bor-b "admin_nodeInfo" | string_field "id")
	observer_id=$(rpc_ok bor-observer "admin_nodeInfo" | string_field "id")
	if [ -z "${a_enr}" ] || [ -z "${b_enr}" ] || [ -z "${observer_enr}" ]; then
		echo "failed to read node enrs" >&2
		return 1
	fi
	rpc bor-a "admin_addPeer" "[\"${b_enr}\"]" >/dev/null
	rpc bor-a "admin_addPeer" "[\"${observer_enr}\"]" >/dev/null
	rpc bor-b "admin_addPeer" "[\"${a_enr}\"]" >/dev/null
	rpc bor-b "admin_addPeer" "[\"${observer_enr}\"]" >/dev/null
	rpc bor-observer "admin_addPeer" "[\"${a_enr}\"]" >/dev/null
	rpc bor-observer "admin_addPeer" "[\"${b_enr}\"]" >/dev/null
	wait_for_peer_id bor-a "${b_id}"
	wait_for_peer_id bor-a "${observer_id}"
	wait_for_peer_id bor-b "${a_id}"
	wait_for_peer_id bor-b "${observer_id}"
	wait_for_peer_id bor-observer "${a_id}"
	wait_for_peer_id bor-observer "${b_id}"
}

ensure_stack() {
	profile="$1"
	set_profile "${profile}"
	compose up -d bor-a bor-b bor-observer >/dev/null
	wait_for_rpc bor-a
	wait_for_rpc bor-b
	wait_for_rpc bor-observer
	pair_nodes
}

reset_service_volume() {
	service="$1"
	volume="$2"
	compose stop "${service}" >/dev/null 2>&1 || true
	compose rm -sf "${service}" >/dev/null 2>&1 || true
	docker volume rm -f "${volume}" >/dev/null 2>&1 || true
}

record_series() {
	key="$1"
	file="$2"
	shift 2
	for value in "$@"; do
		printf '%s=%s\n' "${key}" "${value}" >>"${file}"
	done
}

summarize_file() {
	file="$1"
	python3 - "${file}" <<'PY'
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
    avg = sum(nums) / len(nums)
    print(f"{key}: count={len(nums)} min={nums[0]} p50={p50} p95={p95} mean={avg:.1f} max={nums[-1]}")
PY
}

measure_rpc_ms() {
	service="$1"
	method="$2"
	params="${3:-[]}"
	start_ms=$(now_ms)
	rpc_ok "${service}" "${method}" "${params}" >/dev/null
	end_ms=$(now_ms)
	echo "$((end_ms - start_ms))"
}

run_fetcher_matrix() {
	results="${benchmark_dir}/fetcher-matrix.txt"
	: >"${results}"
	for profile in tcp quic; do
		ensure_stack "${profile}"
		i=1
		while [ "${i}" -le "${rounds}" ]; do
			tx_only=$(measure_rpc_ms bor-a admin_triggerTxFetch)
			body_only=$(measure_rpc_ms bor-a admin_triggerBlockBodyFetch)

			concurrent_tmp="${benchmark_dir}/concurrent-${profile}-${i}"
			rm -f "${concurrent_tmp}.tx" "${concurrent_tmp}.body"
			(
				measure_rpc_ms bor-a admin_triggerTxFetch >"${concurrent_tmp}.tx"
			) &
			tx_pid=$!
			(
				measure_rpc_ms bor-a admin_triggerBlockBodyFetch >"${concurrent_tmp}.body"
			) &
			body_pid=$!
			wait "${tx_pid}"
			wait "${body_pid}"
			tx_concurrent=$(cat "${concurrent_tmp}.tx")
			body_concurrent=$(cat "${concurrent_tmp}.body")
			rm -f "${concurrent_tmp}.tx" "${concurrent_tmp}.body"

			record_series "${profile}_tx_fetch_only_ms" "${results}" "${tx_only}"
			record_series "${profile}_block_body_only_ms" "${results}" "${body_only}"
			record_series "${profile}_tx_fetch_concurrent_ms" "${results}" "${tx_concurrent}"
			record_series "${profile}_block_body_concurrent_ms" "${results}" "${body_concurrent}"
			i=$((i + 1))
		done
	done
	echo "fetcher contention matrix"
	summarize_file "${results}"
}

announcement_once() {
	source="$1"
	method="$2"
	observer_pattern="$3"
	trigger_since=$(log_since_time)
	start_ms=$(now_ms)
	rpc_ok "${source}" "${method}" >/dev/null
	wait_for_log_since bor-observer "${observer_pattern}" "${trigger_since}" 60
	end_ms=$(now_ms)
	echo "$((end_ms - start_ms))"
}

prepare_observer() {
	source_head_hex=$(hex_result "$(rpc_ok bor-b "eth_blockNumber")")
	source_head=$(hex_to_dec "${source_head_hex}")
	wait_for_block bor-observer "${source_head}"
}

run_announcement_profiles() {
	results="${benchmark_dir}/announcement-profiles.txt"
	: >"${results}"
	for profile in tcp quic; do
		ensure_stack "${profile}"
		prepare_observer
		i=1
		while [ "${i}" -le 5 ]; do
			tx_ms=$(announcement_once bor-b admin_triggerTxGossip 'Observed transaction announcements')
			block_ms=$(announcement_once bor-b admin_triggerBlockAnnouncement 'Observed block announcements')
			record_series "${profile}_observer_tx_announcement_ms" "${results}" "${tx_ms}"
			record_series "${profile}_observer_block_announcement_ms" "${results}" "${block_ms}"
			i=$((i + 1))
		done
	done
	echo "announcement observer profiles"
	summarize_file "${results}"
}

run_downloader_counterbalanced() {
	results="${benchmark_dir}/downloader-counterbalanced.txt"
	: >"${results}"
	for profile in tcp quic quic tcp; do
		set_profile "${profile}"
		compose up -d bor-b bor-observer >/dev/null
		wait_for_rpc bor-b
		wait_for_rpc bor-observer
		reset_service_volume bor-a "${volume_a}"
		compose up -d bor-a >/dev/null
		wait_for_rpc bor-a
		pair_nodes
		source_head_hex=$(hex_result "$(rpc_ok bor-b "eth_blockNumber")")
		source_head=$(hex_to_dec "${source_head_hex}")
		start_ms=$(now_ms)
		wait_for_block bor-a "${source_head}"
		end_ms=$(now_ms)
		record_series "${profile}_downloader_to_tip_ms" "${results}" "$((end_ms - start_ms))"
	done
	echo "downloader counterbalanced benchmark"
	summarize_file "${results}"
}

usage() {
	echo "usage: $0 {fetcher-matrix|downloader-counterbalanced|announcement-profiles|all}" >&2
	exit 1
}

cmd="${1:-all}"

case "${cmd}" in
fetcher-matrix)
	run_fetcher_matrix
	;;
downloader-counterbalanced)
	run_downloader_counterbalanced
	;;
announcement-profiles)
	run_announcement_profiles
	;;
all)
	run_fetcher_matrix
	run_downloader_counterbalanced
	run_announcement_profiles
	;;
*)
	usage
	;;
esac
