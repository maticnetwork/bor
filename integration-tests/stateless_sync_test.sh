#!/bin/bash
set -e

echo "Starting stateless sync tests..."

# Check if required tools are available
if ! command -v cast &>/dev/null; then
	echo "Error: 'cast' command not found. Please install Foundry toolkit."
	exit 1
fi

if ! command -v kurtosis &>/dev/null; then
	echo "Error: 'kurtosis' command not found. Please install Kurtosis."
	exit 1
fi

# Define the enclave name
ENCLAVE_NAME=${ENCLAVE_NAME:-"stateless-sync-e2e"}

# Function to get RPC URL for a service
get_rpc_url() {
	local service_name=$1
	kurtosis port print $ENCLAVE_NAME $service_name rpc 2>/dev/null || echo ""
}

# Function to get block number from a specific service using cast
get_block_number() {
	local service_name=$1
	local rpc_url=$(get_rpc_url $service_name)
	if [ -n "$rpc_url" ]; then
		cast block --rpc-url "$rpc_url" 2>/dev/null | grep "number" | awk '{print $2}' | sed 's/,//' || echo "0"
	else
		echo "0"
	fi
}

# Function to get block timestamp using cast
get_block_timestamp() {
	local service_name=$1
	local block_number=$2
	local rpc_url=$(get_rpc_url $service_name)
	if [ -n "$rpc_url" ]; then
		cast block $block_number --rpc-url "$rpc_url" 2>/dev/null | grep "timestamp" | awk '{print $2}' | sed 's/,//' || echo "0"
	else
		echo "0"
	fi
}

# Function to get finalized block number using cast
get_finalized_block() {
	local service_name=$1
	local rpc_url=$(get_rpc_url $service_name)
	if [ -n "$rpc_url" ]; then
		cast block finalized --rpc-url "$rpc_url" 2>/dev/null | grep "number" | awk '{print $2}' | sed 's/,//' || echo "0"
	else
		echo "0"
	fi
}

# Function to get block hash using cast
get_block_hash() {
	local service_name=$1
	local block_number=$2
	local rpc_url=$(get_rpc_url $service_name)
	if [ -n "$rpc_url" ]; then
		cast block $block_number --rpc-url "$rpc_url" 2>/dev/null | grep "^hash" | awk '{print $2}' | sed 's/,//' || echo ""
	else
		echo ""
	fi
}

# Define services based on the kurtosis configuration
echo "Setting up service lists based on kurtosis configuration..."

# Based on the kurtosis config:
# - 8 validators with stateless_sync branch (l2-el-1 through l2-el-8)
# - 1 validator with bor:2.2.5 (l2-el-9)
# - 3 RPC nodes with stateless_sync branch (l2-el-10, l2-el-11, l2-el-12)

STATELESS_SYNC_VALIDATORS=()
LEGACY_VALIDATORS=()
STATELESS_RPC_SERVICES=()

# Stateless sync validators (1-8)
for i in {1..8}; do
	STATELESS_SYNC_VALIDATORS+=("l2-el-$i-bor-heimdall-v2-validator")
done

# Legacy validator (9)
LEGACY_VALIDATORS+=("l2-el-9-bor-heimdall-v2-validator")

# RPC nodes (10-12)
# Note: According to kurtosis config, l2-el-12 has el_bor_sync_with_witness: true
for i in {10..12}; do
	STATELESS_RPC_SERVICES+=("l2-el-$i-bor-heimdall-v2-rpc")
done

echo "Stateless sync validators (1-8): ${STATELESS_SYNC_VALIDATORS[*]}"
echo "Legacy validator (9): ${LEGACY_VALIDATORS[*]}"
echo "RPC services (10-12): ${STATELESS_RPC_SERVICES[*]}"

# Verify services are accessible
echo "Verifying service accessibility..."
for service in "${STATELESS_SYNC_VALIDATORS[@]}" "${LEGACY_VALIDATORS[@]}" "${STATELESS_RPC_SERVICES[@]}"; do
	rpc_url=$(get_rpc_url $service)
	if [ -n "$rpc_url" ]; then
		echo "✅ $service: $rpc_url"
	else
		echo "❌ $service: No RPC URL found"
	fi
done

# Test 1: Check all nodes reach block 399 and have same block hash
echo ""
echo "=== Test 1: Checking all the nodes reach block 399 and have the same block hash ==="

SECONDS=0
start_time=$SECONDS
TARGET_BLOCK=399

while true; do
	current_time=$SECONDS
	elapsed=$((current_time - start_time))

	# Timeout after 10 minutes
	if [ $elapsed -gt 600 ]; then
		echo "Timeout waiting for block $TARGET_BLOCK"
		exit 1
	fi

	# Get block numbers from all services
	block_numbers=()
	max_block=0

	# Check all services (stateless_sync validators + legacy validators + RPC)
	ALL_TEST_SERVICES=("${STATELESS_SYNC_VALIDATORS[@]}" "${LEGACY_VALIDATORS[@]}" "${STATELESS_RPC_SERVICES[@]}")

	for service in "${ALL_TEST_SERVICES[@]}"; do
		block_num=$(get_block_number $service)
		if [[ "$block_num" =~ ^[0-9]+$ ]]; then
			block_numbers+=($block_num)
			if [ $block_num -gt $max_block ]; then
				max_block=$block_num
			fi
		fi
	done

	echo "Current max block: $max_block ($(printf '%02dm:%02ds\n' $((elapsed / 60)) $((elapsed % 60)))) [${#block_numbers[@]} nodes responding]"

	# Check if all nodes have reached the target block
	min_block=${block_numbers[0]}
	for block in "${block_numbers[@]}"; do
		if [ $block -lt $min_block ]; then
			min_block=$block
		fi
	done

	if [ $min_block -ge $TARGET_BLOCK ]; then
		echo "All nodes have reached block $TARGET_BLOCK, checking block hash consensus..."

		# Get block hash for block 399 from all services
		block_hashes=()
		reference_hash=""
		hash_mismatch=false

		for service in "${ALL_TEST_SERVICES[@]}"; do
			block_hash=$(get_block_hash $service $TARGET_BLOCK)
			if [ -n "$block_hash" ]; then
				block_hashes+=("$service:$block_hash")

				# Set reference hash from first service
				if [ -z "$reference_hash" ]; then
					reference_hash=$block_hash
					echo "Reference hash from $service: $reference_hash"
				else
					# Compare with reference hash
					if [ "$block_hash" != "$reference_hash" ]; then
						echo "❌ Hash mismatch! $service has hash: $block_hash (expected: $reference_hash)"
						hash_mismatch=true
					else
						echo "✅ $service has matching hash: $block_hash"
					fi
				fi
			else
				echo "❌ Failed to get hash for block $TARGET_BLOCK from $service"
				hash_mismatch=true
			fi
		done

		if [ "$hash_mismatch" = true ]; then
			echo "❌ Block hash verification failed for block $TARGET_BLOCK"
			echo "All hashes collected:"
			for hash_entry in "${block_hashes[@]}"; do
				echo "  $hash_entry"
			done
			exit 1
		else
			echo "✅ All nodes have reached block $TARGET_BLOCK with the same hash: $reference_hash"
			break
		fi
	fi

	sleep 5
done

# Test 2: Check nodes continue syncing after block 400 (veblop HF)
echo ""
echo "=== Test 2: Checking post-veblop HF behavior (after block 400) ==="

# Wait for block 450 to ensure we're past the HF
TARGET_BLOCK_VEBLOP_HF=400
TARGET_BLOCK_POST_HF=450
echo "Waiting for block $TARGET_BLOCK_POST_HF to ensure we're past veblop HF..."

while true; do
	current_time=$SECONDS
	elapsed=$((current_time - start_time))

	# Timeout after 15 minutes total (extended for hash verification)
	if [ $elapsed -gt 900 ]; then
		echo "Timeout waiting for post-HF block $TARGET_BLOCK_POST_HF"
		exit 1
	fi

	# Check stateless_sync services (should continue syncing after HF)
	max_stateless_block=0
	for service in "${STATELESS_SYNC_VALIDATORS[@]}" "${STATELESS_RPC_SERVICES[@]}"; do
		block_num=$(get_block_number $service)
		if [[ "$block_num" =~ ^[0-9]+$ ]] && [ $block_num -gt $max_stateless_block ]; then
			max_stateless_block=$block_num
		fi
	done

	# Check legacy services (might stop syncing after HF)
	max_legacy_block=0
	for service in "${LEGACY_VALIDATORS[@]}"; do
		block_num=$(get_block_number $service)
		if [[ "$block_num" =~ ^[0-9]+$ ]] && [ $block_num -gt $max_legacy_block ]; then
			max_legacy_block=$block_num
		fi
	done

	echo "Current stateless_sync max block: $max_stateless_block"
	echo "Current legacy max block: $max_legacy_block"

	if [ $max_stateless_block -ge $TARGET_BLOCK_POST_HF ]; then
		echo "✅ Stateless sync nodes continued syncing past veblop HF"

		# Check if legacy nodes stopped progressing
		if [ $max_legacy_block -lt $TARGET_BLOCK_POST_HF ]; then
			echo "✅ Legacy nodes appropriately stopped syncing after veblop HF (at block $max_legacy_block)"
		else
			echo "⚠️  Legacy nodes are still running (at block $max_legacy_block) - forked off from stateless sync validators"
		fi

		# Check block hash consensus for stateless sync services at block 450
		echo "Checking block hash consensus for stateless sync services at block $TARGET_BLOCK_POST_HF..."

		# Only check stateless sync validators and RPC services (not legacy validators)
		STATELESS_SERVICES=("${STATELESS_SYNC_VALIDATORS[@]}" "${STATELESS_RPC_SERVICES[@]}")

		# Get block hash for block 450 from all stateless sync services
		block_hashes=()
		reference_hash=""
		hash_mismatch=false

		for service in "${STATELESS_SERVICES[@]}"; do
			block_hash=$(get_block_hash $service $TARGET_BLOCK_POST_HF)
			if [ -n "$block_hash" ]; then
				block_hashes+=("$service:$block_hash")

				# Set reference hash from first service
				if [ -z "$reference_hash" ]; then
					reference_hash=$block_hash
					echo "Reference hash from $service: $reference_hash"
				else
					# Compare with reference hash
					if [ "$block_hash" != "$reference_hash" ]; then
						echo "❌ Hash mismatch! $service has hash: $block_hash (expected: $reference_hash)"
						hash_mismatch=true
					else
						echo "✅ $service has matching hash: $block_hash"
					fi
				fi
			else
				echo "❌ Failed to get hash for block $TARGET_BLOCK_POST_HF from $service"
				hash_mismatch=true
			fi
		done

		if [ "$hash_mismatch" = true ]; then
			echo "❌ Block hash verification failed for block $TARGET_BLOCK_POST_HF"
			echo "All hashes collected:"
			for hash_entry in "${block_hashes[@]}"; do
				echo "  $hash_entry"
			done
			exit 1
		else
			echo "✅ All stateless sync services have the same hash for block $TARGET_BLOCK_POST_HF: $reference_hash"
		fi

		break
	fi

	sleep 5
done

# Test 3: Check milestone settlement latency
echo ""
echo "=== Test 3: Checking milestone settlement latency ==="

# Pick a representative service for latency testing (use the first stateless sync validator)
REPRESENTATIVE_SERVICE=${STATELESS_SYNC_VALIDATORS[0]}
echo "Using service $REPRESENTATIVE_SERVICE for latency testing"

# Check the last 10 finalized blocks for latency
for i in {1..10}; do
	# Get current finalized block
	finalized_block=$(get_finalized_block $REPRESENTATIVE_SERVICE)

	if [[ "$finalized_block" =~ ^[0-9]+$ ]] && [ $finalized_block -gt 0 ]; then
		# Get block timestamp
		block_timestamp=$(get_block_timestamp $REPRESENTATIVE_SERVICE $finalized_block)
		current_timestamp=$(date +%s)

		if [[ "$block_timestamp" =~ ^[0-9]+$ ]]; then
			latency=$((current_timestamp - block_timestamp))
			echo "Block $finalized_block: timestamp=$block_timestamp, current=$current_timestamp, latency=${latency}s"

			if [ $latency -gt 5 ]; then
				echo "❌ Settlement latency check failed: ${latency}s > 5s for block $finalized_block"
				exit 1
			fi
		fi
	fi

	sleep 2
done

echo "✅ All milestone settlement latency checks passed (< 5 seconds)"

echo ""
echo "🎉 All stateless sync tests passed successfully!"
echo "✅ All nodes reached block 399 with the same block hash"
echo "✅ Stateless sync services continued syncing after veblop HF with matching block hash at block 450"
echo "✅ Milestone settlement latency is under 5 seconds"
