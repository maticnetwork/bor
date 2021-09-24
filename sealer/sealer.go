package sealer

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/sealer/bor"
)

// Backend wraps all methods required for mining.
type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
}

type Sealer struct {
	stopCh chan struct{}
	bor    *bor.Bor
	mux    event.Mux
	chain  *core.BlockChain
}

// New creates a new sealer. We are going to use miner.Config for now so not to mess too much (for now). Extend that config if necessary
func New(eth Backend, config *miner.Config, chainConfig *params.ChainConfig, mux event.Mux, engine consensus.Engine, isLocalBlock func(block *types.Block) bool) *Sealer {
	s := &Sealer{
		stopCh: make(chan struct{}),
		mux:    mux,
	}
	return s
}

func (s *Sealer) Setup(b *core.BlockChain, tx *core.TxPool) error {
	// create the bor client now since now we have all the data available

	// go s.start()
	return nil
}

func (s *Sealer) start() {
	// This is the sealer loop, ONLY required if you are sealing blocks.

	events := s.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	defer func() {
		if !events.Closed() {
			events.Unsubscribe()
		}
	}()

	subs := events.Chan()

	// Sync step
SYNCING:
	syncing := true

	for syncing {
		select {
		case ev := <-subs:
			switch ev.Data.(type) {
			case downloader.FailedEvent:
				// something failed in the sync, we can try to seal now
				syncing = false
			case downloader.DoneEvent:
				// sync is done, we can try to sync now
				syncing = false
			}
		case <-s.stopCh:
			return
		}
	}

	// Seal step
	for {
		block := s.chain.CurrentBlock()

		snap, err := s.bor.GetSnapshot(block.NumberU64(), block.Hash())
		if err != nil {
			// ??? Go back to syncing
			panic(err)
		}

		// check if we are the validator or not
		// if we are the validator we sign and wait for others
		// otherwise? Just wait block by block?
		fmt.Println(snap)

		// TODO: Sealer needs to know the sealing address now (also when it creates bor)

		select {
		case ev := <-subs:
			switch ev.Data.(type) {
			case downloader.StartEvent:
				// sync has started again for some reason, stop seal cycle
				goto SYNCING
			}
		case <-s.stopCh:
			return
		}
	}
}

func (s *Sealer) SetExtra(extra []byte) error {
	return nil
}

func (s *Sealer) Start(coinbase common.Address) {

}

func (s *Sealer) Stop() {
	close(s.stopCh)
}

func (s *Sealer) Mining() bool {
	return false
}

func (s *Sealer) SetGasCeil(ceil uint64) {

}

func (s *Sealer) SetEtherbase(addr common.Address) {
	// etherbase only changes in PoW
}

// Only required for specific Api endpoints. Disable for now.

func (s *Sealer) Pending() (*types.Block, *state.StateDB) {
	return nil, nil
}

func (s *Sealer) PendingBlock() *types.Block {
	return nil
}

func (s *Sealer) SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription {
	return nil
}

func (s *Sealer) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return nil, nil
}

// Only required for PoW

func (s *Sealer) SetRecommitInterval(interval time.Duration) {
	panic("unreachable")
}

func (s *Sealer) Hashrate() uint64 {
	panic("unreachable")
}
