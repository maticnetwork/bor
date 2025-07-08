#!/bin/bash
set -e

echo "Starting smoke tests for the local docker devnet..."

cd matic-cli/devnet/devnet
ADDRESS=$(jq -r '.[0].address' signer-dump.json)
PRIVATE_KEY=$(jq -r '.[0].priv_key' signer-dump.json)

cd ../code/pos-contracts
MATIC_CONTRACT_ADDRESS=$(jq -r .root.tokens.MaticToken contractAddresses.json)

echo "Checking MATIC token balance before executing deposits..."
export PATH="$HOME/.foundry/bin:$PATH"

# Check MATIC token balance of the account.
REQUIRED_BALANCE="100000000000000000000" # 100 MATIC tokens.
CURRENT_BALANCE=$(cast call $MATIC_CONTRACT_ADDRESS "balanceOf(address)" $ADDRESS --rpc-url http://localhost:9545 | cast --to-dec)

echo "Current MATIC balance: $CURRENT_BALANCE"
echo "Required MATIC balance: $REQUIRED_BALANCE (100 MATIC)"

# Use bc for large number comparison since bash can't handle wei amounts
if [ $(echo "$CURRENT_BALANCE < $REQUIRED_BALANCE" | bc) -eq 1 ]; then
	echo "❌ Error: Insufficient MATIC balance!"
	echo "Current: $CURRENT_BALANCE"
	echo "Required: $REQUIRED_BALANCE"
	echo "Please fund the account with more MATIC tokens before running the smoke tests."
	exit 1
fi

echo "✅ Balance check passed! Account has sufficient MATIC tokens."

# Check the initial state sync event count in Heimdall before deposits.
echo "Checking initial state sync event count in Heimdall..."
INITIAL_EVENT_COUNT=$(curl -s localhost:1317/clerk/event-records/count | jq -r '.count' || echo "0")
echo "Initial state sync event count: $INITIAL_EVENT_COUNT"

# Check the initial BOR balance before deposits.
INITIAL_BOR_BALANCE=$(docker exec bor0 bash -c "bor attach /var/lib/bor/data/bor.ipc --exec \"Math.round(web3.fromWei(eth.getBalance(eth.accounts[0]), 'ether'))\"")
echo "Initial balance in BOR: $INITIAL_BOR_BALANCE"

echo "Executing 100 deposits..."

for i in {1..100}; do
	echo "Executing deposit $i/100..."
	forge script scripts/matic-cli-scripts/Deposit.s.sol:MaticDeposit --sig "run(address,address,uint256)" $ADDRESS $MATIC_CONTRACT_ADDRESS 1000000000000000000 --rpc-url http://localhost:9545 --private-key $PRIVATE_KEY --legacy --broadcast &>/dev/null
	echo "✅ Deposit $i/100 executed successfully!"
	sleep 0.5
done

echo "✅ All 100 deposits executed successfully! Waiting for Heimdall to process events..."

# Wait for Heimdall to process all 100 events.
EXPECTED_EVENT_COUNT=$((INITIAL_EVENT_COUNT + 100))
echo "Waiting for Heimdall's clerk event count to reach: $EXPECTED_EVENT_COUNT"

SECONDS=0
start_time=$SECONDS

while true; do
	CURRENT_EVENT_COUNT=$(curl -s localhost:1317/clerk/event-records/count | jq -r '.count' || echo "0")

	if [ "$CURRENT_EVENT_COUNT" -eq "$EXPECTED_EVENT_COUNT" ]; then
		event_processing_time=$((SECONDS - start_time))
		echo "✅ All 100 events processed by Heimdall!"
		echo "Initial event count: $INITIAL_EVENT_COUNT"
		echo "Final event count: $CURRENT_EVENT_COUNT"
		echo "Events processed: $((CURRENT_EVENT_COUNT - INITIAL_EVENT_COUNT))"
		echo "Time taken: $(printf '%02dm:%02ds\n' $((event_processing_time % 3600 / 60)) $((event_processing_time % 60)))"
		break
	fi

	echo "Current event count: $CURRENT_EVENT_COUNT (expected: $EXPECTED_EVENT_COUNT)"
	sleep 5
done

# Now check BOR balance bumped by exactly 100 MATIC.
EXPECTED_BOR_BALANCE=$((INITIAL_BOR_BALANCE + 100))
echo "Waiting for BOR balance to reach exactly: $EXPECTED_BOR_BALANCE"

while true; do
	CURRENT_BOR_BALANCE=$(docker exec bor0 bash -c "bor attach /var/lib/bor/data/bor.ipc --exec \"Math.round(web3.fromWei(eth.getBalance(eth.accounts[0]), 'ether'))\"")

	if ! [[ $CURRENT_BOR_BALANCE =~ ^[0-9]+$ ]]; then
		echo "❌ Error reading BOR balance (got: $CURRENT_BOR_BALANCE)"
		exit 1
	fi

	if [ "$CURRENT_BOR_BALANCE" -eq "$EXPECTED_BOR_BALANCE" ]; then
		echo "✅ BOR balance updated correctly: $CURRENT_BOR_BALANCE"
		break
	else
		echo "Current BOR balance: $CURRENT_BOR_BALANCE (expected: $EXPECTED_BOR_BALANCE)"
	fi

	sleep 5
done

echo "🎉 All state sync tests passed — Heimdall clerk events and BOR balance look good!"

echo "Starting checkpoint test…"
while true; do
	checkpointID=$(curl -s localhost:1317/checkpoints/latest | jq -r '.checkpoint.id' || echo "null")
	if [ "$checkpointID" != "null" ]; then
		echo "✅ Checkpoint created! ID: $checkpointID"
		break
	else
		echo "Current checkpoint: none (polling…)"
		sleep 5
	fi
done

echo "🎉 Checkpoint test passed — Heimdall checkpoint looks good!"
