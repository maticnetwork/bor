package sequencer

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// awaitOurWindow runs the seal barrier for a block whose content is exactly
// what this publisher has published — the healthy case, and what every
// barrier test meant before the barrier compared content. Tests that need a
// block diverging from the journal call AwaitSequenced directly.
func awaitOurWindow(p *Publisher, timeout time.Duration) bool {
	p.mu.Lock()
	height := p.curHeight
	txs := journalTxs(p)
	p.mu.Unlock()

	return p.AwaitSequenced(timeout, height, txs)
}

func journalTxs(p *Publisher) []*types.Transaction {
	start := p.journal.openStart()
	if start < 0 {
		return nil
	}

	var txs []*types.Transaction

	for _, it := range p.journal.items[start+1:] {
		rec := it.entry.GetRecord()
		if rec == nil {
			continue
		}

		for _, raw := range rec.GetTransactions() {
			tx := new(types.Transaction)
			if err := tx.UnmarshalBinary(raw); err == nil {
				txs = append(txs, tx)
			}
		}
	}

	return txs
}

// sealOnChain seals a block and records it as this node's block at that
// height — what the miner does, since resultLoop writes the block before it
// announces. A flush only displaces a live foreign window on proof the chain
// kept our block, so a test producer that seals without a chain write is
// modelling a block nobody accepted.
func sealOnChain(p *Publisher, fc *fakeChain, header *types.Header, txs []*types.Transaction) {
	p.SealBlock(blockFor(header, txs))

	if fc.canonical == nil {
		fc.canonical = map[uint64]common.Hash{}
	}

	fc.canonical[header.Number.Uint64()] = header.Hash()
}
