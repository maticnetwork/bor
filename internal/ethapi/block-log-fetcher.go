package ethapi

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"strings"

	"github.com/maticnetwork/bor/accounts/abi"
	"github.com/maticnetwork/bor/common"
	"github.com/maticnetwork/bor/core"
	"github.com/maticnetwork/bor/core/types"
	"github.com/maticnetwork/bor/eth/filters"
	"github.com/maticnetwork/bor/rpc"
)

const fetchStateSyncsABI = `[{"inputs":[{"internalType":"uint256","name":"start","type":"uint256"},{"internalType":"uint256","name":"to","type":"uint256"},{"internalType":"address[]","name":"addresses","type":"address[]"},{"internalType":"bytes32","name":"method","type":"bytes32"}],"name":"fetchBlockLogs","outputs":[{"internalType":"bytes","name":"","type":"bytes"}],"stateMutability":"view","type":"function"}]`

var zeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")

func interceptForStateSyncFetcher(ctx context.Context, backend Backend, args []byte) (*core.ExecutionResult, error) {
	fABI, err := abi.JSON(strings.NewReader(fetchStateSyncsABI))
	if err != nil {
		return nil, err
	}

	// check if calling method by checking data length
	if len(args) >= 4 {
		// fetch method
		method, err := fABI.MethodById(args[:4])
		if err != nil {
			// return if method is not in abi (other method apart from `fetchBlockLogs` is being called)
			return nil, nil
		}

		// handle fetch state syncs
		if bytes.Equal(method.ID, fABI.Methods["fetchBlockLogs"].ID) {
			fetcherInput := make(map[string]interface{})

			// unpack method inputs
			if err := method.Inputs.UnpackIntoMap(fetcherInput, args[4:]); err != nil {
				return nil, err
			}

			// fetch data
			start := fetcherInput["start"].(*big.Int)
			end := fetcherInput["to"].(*big.Int)
			addresses := fetcherInput["addresses"].([]common.Address)
			topic := fetcherInput["method"].([32]uint8)

			// match with list of addresses and first topic (only allow one topic at a time)
			logs, err := fetchBorBlockLogs(ctx, backend, start, end, addresses, [][]common.Hash{{topic}})
			if err != nil {
				return nil, err
			}

			// convert to json data
			result, err := json.Marshal(logs)
			if err != nil {
				return nil, err
			}

			// output
			output, err := method.Outputs.Pack(result)
			if err != nil {
				return nil, err
			}

			return &core.ExecutionResult{
				UsedGas:    0,
				Err:        nil,
				ReturnData: output,
			}, nil
		}
	}

	return nil, nil
}

func fetchBorBlockLogs(ctx context.Context, backend Backend, from *big.Int, to *big.Int, addresses []common.Address, topics [][]common.Hash) ([]*types.Log, error) {
	// Convert the RPC block numbers into internal representations
	begin := rpc.LatestBlockNumber.Int64()
	if from != nil {
		begin = from.Int64()
	}

	end := rpc.LatestBlockNumber.Int64()
	if to != nil {
		end = to.Int64()
	}

	// Construct the range filter
	filter := filters.NewBorBlockLogsRangeFilter(backend, begin, end, addresses, topics)
	// Run the filter and return all the logs
	return filter.Logs(ctx)
}
