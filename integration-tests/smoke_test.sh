#!/bin/bash
set -e

echo "Starting the smoke test for the local docker devnet..."

echo ""
balanceInit=$(docker exec bor0 bash -c "bor attach /var/lib/bor/data/bor.ipc -exec 'Math.round(web3.fromWei(eth.getBalance(eth.accounts[0])))'")

echo "Initial balance of first account: $balanceInit"

stateSyncFound="false"
checkpointFound="false"
SECONDS=0
start_time=$SECONDS

while true
do

    balance=$(docker exec bor0 bash -c "bor attach /var/lib/bor/data/bor.ipc -exec 'Math.round(web3.fromWei(eth.getBalance(eth.accounts[0])))'")

    if ! [[ "$balance" =~ ^[0-9]+$ ]]; then
        echo "Something is wrong! Can't find the balance of first account in bor network."
        exit 1
    fi

    if (( balance > balanceInit )); then
        if [ "$stateSyncFound" != "true" ]; then
            stateSyncTime=$(( SECONDS - start_time ))
            stateSyncFound="true"
            echo "State sync went through. Time taken: $(printf '%02dm:%02ds\n' $((stateSyncTime%3600/60)) $((stateSyncTime%60)))"
        fi
    fi

    checkpointID=$(curl -sL http://localhost:1317/checkpoints/latest | jq .checkpoint.id)

    if [ "$checkpointID" != "null" ]; then
        if [ "$checkpointFound" != "true" ]; then
            checkpointTime=$(( SECONDS - start_time ))
            checkpointFound="true"
            echo "Checkpoint went through. Time taken: $(printf '%02dm:%02ds\n' $((checkpointTime%3600/60)) $((checkpointTime%60)))"
        fi
    fi

    if [ "$stateSyncFound" == "true" ] && [ "$checkpointFound" == "true" ]; then
        break
    fi

done
echo "Both state sync and checkpoint went through. All tests have passed!"
echo "Time taken for state sync: $(printf '%02dm:%02ds\n' $((stateSyncTime%3600/60)) $((stateSyncTime%60)))"
echo "Time taken for checkpoint: $(printf '%02dm:%02ds\n' $((checkpointTime%3600/60)) $((checkpointTime%60)))"
