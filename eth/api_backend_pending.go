package eth

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

func (b *EthAPIBackend) sequencerPendingBlock() *types.Block {
	if b.eth.seqConsumer == nil {
		return nil
	}
	return b.eth.seqConsumer.PendingBlock()
}

func (b *EthAPIBackend) sequencerPendingBlockAndReceipts() (*types.Block, types.Receipts) {
	if b.eth.seqConsumer == nil {
		return nil, nil
	}
	return b.eth.seqConsumer.PendingBlockAndReceipts()
}

func (b *EthAPIBackend) PendingBlock() *types.Block {
	if block := b.sequencerPendingBlock(); block != nil {
		return block
	}
	if b.eth.miner != nil {
		if block := b.eth.miner.PendingBlock(); block != nil {
			return block
		}
	}
	// Keep the "pending" block accessors consistent with the pending state
	// below during the import->next-open gap.
	block, _ := b.headPendingBlock()
	return block
}

func (b *EthAPIBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	if block, receipts := b.sequencerPendingBlockAndReceipts(); block != nil {
		return block, receipts
	}
	if b.eth.miner != nil {
		if block, receipts, _ := b.eth.miner.Pending(); block != nil {
			return block, receipts
		}
	}
	return b.headPendingBlock()
}

// headPendingBlock is the block half of the head fallback, shared by the
// pending block accessors.
func (b *EthAPIBackend) headPendingBlock() (*types.Block, types.Receipts) {
	if b.eth.seqConsumer == nil {
		return nil, nil
	}
	block, receipts, statedb, err := b.eth.seqConsumer.HeadPendingView()
	if err != nil || block == nil || statedb == nil {
		return nil, nil
	}
	return block, receipts
}

func (b *EthAPIBackend) PendingLogRange() (*types.Header, []*types.Block, []types.Receipts) {
	if b.eth.seqConsumer != nil {
		if anchor, blocks, receipts := b.eth.seqConsumer.PendingLogRange(); anchor != nil && len(blocks) != 0 {
			return anchor, blocks, receipts
		}
	}
	anchor := b.eth.blockchain.CurrentBlock()
	if anchor == nil || b.eth.miner == nil {
		return nil, nil, nil
	}
	block, receipts, _ := b.eth.miner.Pending()
	if block == nil || block.NumberU64() != anchor.Number.Uint64()+1 || block.ParentHash() != anchor.Hash() {
		return nil, nil, nil
	}
	return types.CopyHeader(anchor), []*types.Block{block}, []types.Receipts{receipts}
}

func (b *EthAPIBackend) PendingSnapshot(ctx context.Context) (*types.Block, types.Receipts, *state.StateDB, error) {
	if b.eth.seqConsumer != nil {
		block, receipts, statedb, err := b.eth.seqConsumer.PendingSnapshot(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		if block != nil && statedb != nil {
			return block, receipts, statedb, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if b.eth.miner != nil {
		block, receipts, statedb := b.eth.miner.Pending()
		if block != nil && statedb != nil {
			return block, receipts, statedb, nil
		}
	}
	// Last resort for a non-mining sequencer RPC node in the import->next-open
	// gap: an empty head block keeps pending state available instead of
	// erroring for ~150ms every block.
	if b.eth.seqConsumer != nil {
		block, receipts, statedb, err := b.eth.seqConsumer.HeadPendingView()
		if err == nil && block != nil && statedb != nil {
			return block, receipts, statedb, nil
		}
	}
	return nil, nil, nil, errors.New("pending state is not available")
}

func (b *EthAPIBackend) PendingParentState(ctx context.Context, block *types.Block) (*state.StateDB, error) {
	if b.eth.seqConsumer == nil {
		return nil, nil
	}
	return b.eth.seqConsumer.PendingParentState(ctx, block)
}

func (b *EthAPIBackend) pendingStateAndHeader(ctx context.Context) (*state.StateDB, *types.Header, error) {
	block, _, statedb, err := b.PendingSnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	return statedb, block.Header(), nil
}
