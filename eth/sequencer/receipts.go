package sequencer

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// maxIndexedHeights bounds the index when canonical imports stall (or a
// misbehaving store streams far ahead): the lowest height is dropped to
// admit a new one.
const maxIndexedHeights = 256

// Index holds preconfirmation receipts built from re-executing the sequence
// stream, keyed by transaction hash. Receipts carry a zero BlockHash until
// the block's seal record arrives; the canonical receipt path always takes
// precedence, so entries here are only consulted for transactions the chain
// does not yet have.
type Index struct {
	mu      sync.RWMutex
	byHash  map[common.Hash]*indexed
	byBlock map[uint64][]common.Hash
}

type indexed struct {
	receipt *types.Receipt
	tx      *types.Transaction
}

// NewIndex returns an empty preconf receipt index.
func NewIndex() *Index {
	return &Index{
		byHash:  map[common.Hash]*indexed{},
		byBlock: map[uint64][]common.Hash{},
	}
}

// Add records one preconf receipt for a speculative, not-yet-sealed block.
// The receipt must not be mutated after this call — Lookup hands the stored
// pointer to concurrent RPC readers.
func (ix *Index) Add(tx *types.Transaction, receipt *types.Receipt) {
	ix.AddBatch(types.Transactions{tx}, types.Receipts{receipt})
}

func (ix *Index) AddBatch(txs types.Transactions, receipts types.Receipts) bool {
	if len(txs) == 0 || len(txs) != len(receipts) {
		return false
	}
	if txs[0] == nil || receipts[0] == nil || receipts[0].BlockNumber == nil {
		return false
	}
	number := receipts[0].BlockNumber.Uint64()
	for index, tx := range txs {
		receipt := receipts[index]
		if tx == nil || receipt == nil || receipt.BlockNumber == nil || receipt.BlockNumber.Uint64() != number {
			return false
		}
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()

	if _, held := ix.byBlock[number]; !held && len(ix.byBlock) >= maxIndexedHeights {
		ix.dropLowestLocked()
	}

	for index, tx := range txs {
		receipt := receipts[index]
		hash := tx.Hash()
		ix.byHash[hash] = &indexed{receipt: receipt, tx: tx}
		ix.byBlock[number] = append(ix.byBlock[number], hash)
	}
	preconfPublishedReceipts.Inc(int64(len(txs)))
	return true
}

// Seal fills the sealed block hash into the height's receipts and their
// logs. Stored receipts are immutable once inserted (RPC readers hold the
// same pointers), so sealing swaps in copies.
func (ix *Index) Seal(number uint64, hash common.Hash) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	for _, txHash := range ix.byBlock[number] {
		entry, ok := ix.byHash[txHash]
		if !ok {
			continue
		}

		receipt := *entry.receipt
		receipt.BlockHash = hash
		receipt.Logs = make([]*types.Log, len(entry.receipt.Logs))

		for i, old := range entry.receipt.Logs {
			l := *old
			l.BlockHash = hash
			receipt.Logs[i] = &l
		}

		ix.byHash[txHash] = &indexed{receipt: &receipt, tx: entry.tx}
	}
}

func (ix *Index) dropLowestLocked() {
	lowest := uint64(0)
	first := true

	for height := range ix.byBlock {
		if first || height < lowest {
			lowest = height
			first = false
		}
	}

	if first {
		return
	}

	for _, h := range ix.byBlock[lowest] {
		delete(ix.byHash, h)
	}

	delete(ix.byBlock, lowest)
}

// Lookup returns the preconf receipt and transaction for a hash, if held.
func (ix *Index) Lookup(hash common.Hash) (*types.Receipt, *types.Transaction, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	entry, ok := ix.byHash[hash]
	if !ok {
		return nil, nil, false
	}

	preconfServedMeter.Mark(1)

	return entry.receipt, entry.tx, true
}

func (ix *Index) CountCanonical(block *types.Block) int {
	if block == nil {
		return 0
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	number := block.NumberU64()
	count := 0
	for _, tx := range block.Transactions() {
		entry := ix.byHash[tx.Hash()]
		if entry != nil && entry.receipt != nil && entry.receipt.BlockNumber != nil && entry.receipt.BlockNumber.Uint64() == number {
			count++
		}
	}
	return count
}

// ClearFrom drops all entries at or above a height — a re-anchor voided them.
func (ix *Index) ClearFrom(number uint64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	for height, hashes := range ix.byBlock {
		if height < number {
			continue
		}

		for _, h := range hashes {
			delete(ix.byHash, h)
		}

		delete(ix.byBlock, height)
	}
}

// EvictThrough drops all entries at or below a height — the canonical chain
// now serves those receipts (or the transactions never landed and must not
// linger).
func (ix *Index) EvictThrough(number uint64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	for height, hashes := range ix.byBlock {
		if height > number {
			continue
		}

		for _, h := range hashes {
			delete(ix.byHash, h)
		}

		delete(ix.byBlock, height)
	}
}

// Reset drops everything — the consumer lost stream consistency and is
// re-anchoring from canonical state.
func (ix *Index) Reset() {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	ix.byHash = map[common.Hash]*indexed{}
	ix.byBlock = map[uint64][]common.Hash{}
}
