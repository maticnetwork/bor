package sequencer

import (
	"context"
	"fmt"
	"math/big"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type streamReceiver interface {
	Recv() (*pb.StreamResponse, error)
}

type preparedStreamFrame struct {
	entry        *pb.Entry
	transactions []preparedTransaction
	fold         preparedFold
	recvErr      error
	openApplied  chan struct{}
}

type preparedFold struct {
	prefix commitment.Head
	next   commitment.Head
	cold   bool
	err    error
}

type preparedTransaction struct {
	tx             *types.Transaction
	sender         common.Address
	senderVerified bool
	err            error
}

type streamPreparationState struct {
	head        commitment.Head
	seeded      bool
	blockNumber uint64
	hasBlock    bool
	signer      types.Signer
}

func (c *Consumer) prepareStream(ctx context.Context, stream streamReceiver, state streamPreparationState, out chan<- preparedStreamFrame) {
	defer close(out)
	for {
		frame, err := stream.Recv()
		if err != nil {
			c.handoffPreparedFrame(ctx, out, preparedStreamFrame{recvErr: err})
			return
		}

		entry := frame.GetEntry()
		if entry == nil {
			if !c.handoffPreparedFrame(ctx, out, preparedStreamFrame{}) {
				return
			}
			continue
		}

		prepared, ok := c.prepareStreamEntry(ctx, entry, &state)
		if !ok {
			return
		}
		if !c.handoffPreparedFrame(ctx, out, prepared) {
			return
		}
		if prepared.fold.err != nil {
			return
		}
	}
}

func (c *Consumer) prepareStreamEntry(ctx context.Context, entry *pb.Entry, state *streamPreparationState) (preparedStreamFrame, bool) {
	fold := prepareFoldAt(state.head, state.seeded, entry)
	prepared := preparedStreamFrame{entry: entry, fold: fold}
	if fold.err != nil {
		return prepared, true
	}

	state.head, state.seeded = fold.next, true
	if open := entry.GetBlockOpen(); open != nil {
		state.blockNumber = open.GetBlockNumber()
		state.hasBlock = true
		state.signer = types.MakeSigner(c.chain.Config(), new(big.Int).SetUint64(state.blockNumber), open.GetBlockTimestamp())
		prepared.openApplied = make(chan struct{})
	}
	if record := entry.GetRecord(); record != nil && state.hasBlock {
		var ok bool
		prepared.transactions, ok = c.prepareTransactions(ctx, record.GetTransactions(), state.signer, state.blockNumber)
		if !ok {
			return preparedStreamFrame{}, false
		}
	}
	if entry.GetBlockSeal() != nil {
		state.hasBlock = false
		state.signer = nil
	}
	return prepared, true
}

func (c *Consumer) handoffPreparedFrame(ctx context.Context, out chan<- preparedStreamFrame, prepared preparedStreamFrame) bool {
	select {
	case out <- prepared:
	case <-ctx.Done():
		return false
	}
	if prepared.openApplied == nil {
		return true
	}
	select {
	case <-prepared.openApplied:
		return true
	case <-ctx.Done():
		return false
	}
}

func prepareFoldAt(head commitment.Head, seeded bool, entry *pb.Entry) preparedFold {
	next, err := foldEntry(head, entry)
	if err != nil {
		return preparedFold{err: err}
	}
	claimed := commitment.Head(entryPrefix(entry))
	if !seeded {
		next, err = foldEntry(claimed, entry)
		return preparedFold{prefix: claimed, next: next, cold: true, err: err}
	}
	if claimed != head {
		return preparedFold{err: fmt.Errorf("commitment gap: entry prefix %x != running head %x", claimed[:8], head[:8])}
	}
	return preparedFold{prefix: claimed, next: next}
}

func (c *Consumer) prepareTransactions(ctx context.Context, rawTransactions [][]byte, signer types.Signer, blockNumber uint64) ([]preparedTransaction, bool) {
	if !c.activeWorkerAt(blockNumber) {
		return nil, true
	}
	prepared := make([]preparedTransaction, len(rawTransactions))
	for index, raw := range rawTransactions {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		if !c.activeWorkerAt(blockNumber) {
			return nil, true
		}
		prepared[index] = c.prepareTransaction(raw, signer)
		if !c.activeWorkerAt(blockNumber) {
			return nil, true
		}
	}
	return prepared, true
}

func (c *Consumer) activeWorkerAt(blockNumber uint64) bool {
	worker := c.worker.Load()
	return worker != nil && worker.env != nil && worker.header != nil && worker.header.Number != nil &&
		!worker.env.interrupt.Load() && worker.header.Number.Uint64() == blockNumber
}

func (c *Consumer) prepareTransaction(raw []byte, signer types.Signer) preparedTransaction {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return preparedTransaction{err: fmt.Errorf("decode streamed transaction: %w", err)}
	}
	if signer != nil {
		sender, err := c.verifiedSender(tx, signer)
		if err != nil {
			return preparedTransaction{tx: tx, err: fmt.Errorf("recover streamed transaction %s sender: %w", tx.Hash(), err)}
		}
		return preparedTransaction{tx: tx, sender: sender, senderVerified: true}
	}
	return preparedTransaction{tx: tx}
}

func (c *Consumer) verifiedSender(tx *types.Transaction, signer types.Signer) (common.Address, error) {
	hash := tx.Hash()
	if cached := c.cachedPreconfTransaction(hash); cached != nil {
		if sender, err := types.Sender(signer, cached); err == nil {
			preconfSenderCacheHit.Inc(1)
			return sender, nil
		}
	}
	if c.txLookup != nil {
		if pooled := c.txLookup.Get(hash); pooled != nil && pooled.Hash() == hash {
			if sender, err := types.Sender(signer, pooled); err == nil {
				preconfSenderCacheHit.Inc(1)
				return sender, nil
			}
		}
	}
	preconfSenderCacheMiss.Inc(1)
	return types.Sender(signer, tx)
}
