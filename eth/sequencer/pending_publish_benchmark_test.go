package sequencer

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

var benchmarkPendingView *PendingRPCView

func BenchmarkBuildRPCViewReceiptPrefix(b *testing.B) {
	for _, count := range []int{100, 1_000, 6_000} {
		txs := make(types.Transactions, count)
		receipts := make(types.Receipts, count)
		for index := range count {
			tx := types.NewTransaction(uint64(index), common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
			txs[index] = tx
			receipts[index] = &types.Receipt{
				TxHash:    tx.Hash(),
				PostState: make([]byte, common.HashLength),
				Logs: []*types.Log{{
					TxHash: tx.Hash(),
					Topics: []common.Hash{{1}, {2}, {3}},
					Data:   make([]byte, 256),
				}},
			}
		}
		statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		if err != nil {
			b.Fatal(err)
		}
		header := &types.Header{Number: big.NewInt(1), GasLimit: 30_000_000}
		block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
		prefixBlock := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs[:count-1]})
		previous := buildRPCView(prefixBlock, receipts[:count-1], statedb, nil, common.Hash{})

		for _, test := range []struct {
			name     string
			previous *PendingRPCView
		}{
			{name: "full"},
			{name: "reused", previous: previous},
		} {
			b.Run(fmt.Sprintf("%d/%s", count, test.name), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					benchmarkPendingView = buildRPCView(block, receipts, statedb, test.previous, common.Hash{})
				}
			})
		}
	}
}
