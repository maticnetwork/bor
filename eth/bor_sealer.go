package eth

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Miner is the interface required by core.Ethereum
type Miner interface {
	consensus.Engine

	SetEtherbase(addr common.Address)
	SetExtra(extra []byte) error
	Setup(b *core.BlockChain, tx *core.TxPool) error
	Start(coinbase common.Address)
	Stop()
	Mining() bool
	Pending() (*types.Block, *state.StateDB)
	PendingBlock() *types.Block
	SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription
	PendingBlockAndReceipts() (*types.Block, types.Receipts)
	SetGasCeil(ceil uint64)
	// Only used in PoW (deprecated)
	SetRecommitInterval(interval time.Duration)
	// Only used in PoW (deprecated)
	Hashrate() uint64
}
