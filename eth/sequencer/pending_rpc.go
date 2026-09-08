package sequencer

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

// PendingSnapshot returns one block/receipt/state view. A cancellable request
// context keeps the state-copy lease until that request finishes.
func (c *Consumer) PendingSnapshot(ctx context.Context) (*types.Block, types.Receipts, *state.StateDB, error) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil, nil, nil, nil
	}
	block, receipts, statedb, err := c.pendingStore().PendingSnapshot(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if !c.pendingReadAnchorValid(anchor) {
		return nil, nil, nil, nil
	}
	return block, receipts, statedb, nil
}

// HeadPendingView is an empty pending block on the head, the last-resort view
// for a non-mining RPC node in the import->next-open gap when no preconf entry
// is active. Without it such a node answers -32000 for ~150ms every block,
// breaking bind's default eth_getCode(addr,"pending"). The backend tries it
// only after the live view and the miner, so it never shadows either.
func (c *Consumer) HeadPendingView() (*types.Block, types.Receipts, *state.StateDB, error) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil, nil, nil, nil
	}
	block, receipts, statedb, err := c.headPendingView(anchor)
	if err != nil {
		return nil, nil, nil, err
	}
	if !c.pendingReadAnchorValid(anchor) {
		return nil, nil, nil, nil
	}
	return block, receipts, statedb, nil
}

// headPendingView builds the empty head+1 block on the head state, or nil when
// the head or its state is unavailable.
func (c *Consumer) headPendingView(head *types.Header) (*types.Block, types.Receipts, *state.StateDB, error) {
	if head == nil || c.chain == nil {
		return nil, nil, nil, nil
	}
	statedb, err := c.chain.StateAt(head.Root)
	if err != nil || statedb == nil {
		return nil, nil, nil, nil
	}
	number := new(big.Int).Add(head.Number, big.NewInt(1))
	header := &types.Header{
		ParentHash: head.Hash(),
		Number:     number,
		GasLimit:   head.GasLimit,
		Time:       head.Time + 1,
		Difficulty: big.NewInt(1),
		Root:       head.Root,
	}
	if config := c.chain.Config(); config.IsLondon(number) {
		header.BaseFee = eip1559.CalcBaseFee(config, head)
	}
	return types.NewBlockWithHeader(header), types.Receipts{}, statedb, nil
}

func (c *Consumer) PendingBlock() *types.Block {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil
	}
	block := c.pendingStore().PendingBlock()
	if !c.pendingReadAnchorValid(anchor) {
		return nil
	}
	return block
}

func (c *Consumer) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil, nil
	}
	block, receipts := c.pendingStore().PendingBlockAndReceipts()
	if !c.pendingReadAnchorValid(anchor) {
		return nil, nil
	}
	return block, receipts
}

func (c *Consumer) PendingLogRange() (*types.Header, []*types.Block, []types.Receipts) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil, nil, nil
	}
	blocks, receipts := c.pendingStore().PendingLogRange()
	if !c.pendingReadAnchorValid(anchor) {
		return nil, nil, nil
	}
	return types.CopyHeader(anchor), blocks, receipts
}

func (c *Consumer) PendingParentState(ctx context.Context, block *types.Block) (*state.StateDB, error) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil, nil
	}
	statedb, err := c.pendingStore().PendingParentState(block)
	if err != nil {
		return nil, err
	}
	if !c.pendingReadAnchorValid(anchor) {
		return nil, nil
	}
	return statedb, nil
}

func (c *Consumer) PendingNonce(address common.Address) (uint64, bool, error) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return 0, false, nil
	}
	nonce, found, err := c.pendingStore().PendingNonce(address)
	if !c.pendingReadAnchorValid(anchor) {
		return 0, false, nil
	}
	return nonce, found, err
}

func (c *Consumer) LookupPreconf(hash common.Hash) (*types.Transaction, *types.Receipt, bool) {
	anchor, ok := c.pendingReadAnchor()
	if !ok {
		return nil, nil, false
	}
	receipt, tx, found := c.index.Lookup(hash)
	if !c.pendingReadAnchorValid(anchor) {
		return nil, nil, false
	}
	return tx, cloneReceipt(receipt), found
}

func (c *Consumer) pendingHeadCurrent() bool {
	_, ok := c.pendingReadAnchor()
	return ok
}

func (c *Consumer) pendingReadAnchor() (*types.Header, bool) {
	if c.chain == nil {
		return nil, true
	}
	head := c.chain.CurrentBlock()
	marker := c.reconciled.Load()
	if marker == nil {
		c.reconciled.CompareAndSwap(nil, head)
		marker = c.reconciled.Load()
	}
	if marker == nil || marker != head && marker.Hash() != head.Hash() {
		return nil, false
	}
	return marker, true
}

func (c *Consumer) pendingReadAnchorValid(anchor *types.Header) bool {
	if c.chain == nil {
		return true
	}
	head := c.chain.CurrentBlock()
	return anchor != nil && c.reconciled.Load() == anchor && (head == anchor || head.Hash() == anchor.Hash())
}

// PendingSnapshot limits both concurrent copies and concurrently retained RPC
// states. RPC request contexts are cancelled by the server when handlers exit.
func (s *PendingStore) PendingSnapshot(ctx context.Context) (*types.Block, types.Receipts, *state.StateDB, error) {
	if !s.acquireStateCopy(ctx) {
		return nil, nil, nil, ctx.Err()
	}
	release := true
	defer func() {
		if release {
			s.releaseStateCopy()
		}
	}()

	s.mu.RLock()
	entry := s.entries[s.active]
	if !s.hasActive || entry == nil || entry.RPCView == nil {
		s.mu.RUnlock()
		return nil, nil, nil, nil
	}
	view := entry.RPCView
	s.mu.RUnlock()
	stateDB, err := view.State.NewStateDB()
	if err != nil {
		return nil, nil, nil, err
	}
	if ctx.Done() != nil {
		stop := context.AfterFunc(ctx, s.releaseStateCopy)
		if err := ctx.Err(); err != nil {
			if !stop() {
				release = false
			}
			return nil, nil, nil, err
		}
		release = false
	}
	return view.Block, receiptsFromView(view.Block, view), stateDB, nil
}

func (s *PendingStore) acquireStateCopy(ctx context.Context) bool {
	s.stateCopyOnce.Do(func() {
		s.stateCopy = make(chan struct{}, 1)
	})
	if err := ctx.Err(); err != nil {
		return false
	}
	select {
	case s.stateCopy <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *PendingStore) releaseStateCopy() {
	<-s.stateCopy
}

func (s *PendingStore) PendingBlock() *types.Block {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry := s.entries[s.active]
	if !s.hasActive || entry == nil || entry.RPCView == nil {
		return nil
	}
	return entry.RPCView.Block
}

func (s *PendingStore) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	s.mu.RLock()
	entry := s.entries[s.active]
	if !s.hasActive || entry == nil || entry.RPCView == nil {
		s.mu.RUnlock()
		return nil, nil
	}
	view := entry.RPCView
	s.mu.RUnlock()
	return view.Block, receiptsFromView(view.Block, view)
}

func (s *PendingStore) PendingLogRange() ([]*types.Block, []types.Receipts) {
	s.mu.RLock()
	entry := s.entries[s.active]
	if !s.hasActive || entry == nil || entry.RPCView == nil || entry.RPCView.Block == nil {
		s.mu.RUnlock()
		return nil, nil
	}
	byHash := make(map[common.Hash]*PendingRPCView, len(s.entries))
	for _, candidate := range s.entries {
		if candidate.RPCView != nil && candidate.RPCView.Block != nil {
			byHash[candidate.RPCView.Block.Hash()] = candidate.RPCView
		}
	}
	views := make([]*PendingRPCView, 0, len(byHash))
	for view := entry.RPCView; view != nil; {
		views = append(views, view)
		block := view.Block
		parent := byHash[block.ParentHash()]
		if parent == nil || parent.Block.NumberU64()+1 != block.NumberU64() {
			break
		}
		view = parent
	}
	s.mu.RUnlock()

	blocks := make([]*types.Block, len(views))
	receipts := make([]types.Receipts, len(views))
	for i := range views {
		view := views[len(views)-1-i]
		blocks[i] = view.Block
		receipts[i] = receiptsFromView(view.Block, view)
	}
	return blocks, receipts
}

func (s *PendingStore) PendingParentState(block *types.Block) (*state.StateDB, error) {
	if block == nil || block.NumberU64() == 0 {
		return nil, nil
	}
	s.mu.RLock()
	var reader PendingStateReader
	for key, entry := range s.entries {
		if key.number+1 == block.NumberU64() && entry.RPCView != nil &&
			entry.RPCView.Block != nil && entry.RPCView.Block.Hash() == block.ParentHash() {
			reader = entry.RPCView.State
			break
		}
	}
	s.mu.RUnlock()
	if reader == nil {
		return nil, nil
	}
	return reader.NewStateDB()
}

func (s *PendingStore) PendingNonce(address common.Address) (uint64, bool, error) {
	s.mu.RLock()
	entry := s.entries[s.active]
	if !s.hasActive || entry == nil || entry.RPCView == nil || entry.RPCView.State == nil {
		s.mu.RUnlock()
		return 0, false, nil
	}
	reader := entry.RPCView.State
	s.mu.RUnlock()
	nonce, err := reader.GetNonceWithError(address)
	return nonce, true, err
}
