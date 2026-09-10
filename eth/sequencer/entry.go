package sequencer

import (
	"bytes"
	"errors"
	"math/big"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// contentEqual reports field-level equality of entry content, excluding the
// prefix commitment (design: byte-match). The outer protobuf encoding is
// never compared — the gateway re-marshals entries.
func contentEqual(a, b *pb.Entry) bool {
	switch ak := a.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		bo := b.GetBlockOpen()
		if bo == nil {
			return false
		}

		ao := ak.BlockOpen

		return ao.GetBlockNumber() == bo.GetBlockNumber() &&
			ao.GetBlockTimestamp() == bo.GetBlockTimestamp() &&
			bytes.Equal(ao.GetParentHash(), bo.GetParentHash()) &&
			ao.GetGasLimit() == bo.GetGasLimit() &&
			bytes.Equal(ao.GetBaseFee(), bo.GetBaseFee())
	case *pb.Entry_Record:
		br := b.GetRecord()
		if br == nil || len(ak.Record.GetTransactions()) != len(br.GetTransactions()) {
			return false
		}

		for i, tx := range ak.Record.GetTransactions() {
			if !bytes.Equal(tx, br.GetTransactions()[i]) {
				return false
			}
		}

		return true
	case *pb.Entry_BlockSeal:
		bs := b.GetBlockSeal()

		return bs != nil && bytes.Equal(ak.BlockSeal.GetHeader(), bs.GetHeader())
	default:
		return false
	}
}

// entryPrefix returns the prefix commitment carried by an entry.
func entryPrefix(e *pb.Entry) []byte {
	switch k := e.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		return k.BlockOpen.GetPrefixCommitment()
	case *pb.Entry_Record:
		return k.Record.GetPrefixCommitment()
	case *pb.Entry_BlockSeal:
		return k.BlockSeal.GetPrefixCommitment()
	default:
		return nil
	}
}

// foldEntry folds an entry carrying its existing prefix onto cur.
// openContext builds the fold input for an open entry. Both fold paths must
// construct it identically or a refold diverges from the original fold.
func openContext(bo *pb.BlockOpen) commitment.OpenContext {
	return commitment.OpenContext{
		Number:     bo.GetBlockNumber(),
		Timestamp:  bo.GetBlockTimestamp(),
		ParentHash: [32]byte(bo.GetParentHash()),
		GasLimit:   bo.GetGasLimit(),
		BaseFee:    new(big.Int).SetBytes(bo.GetBaseFee()),
	}
}

func validateEntryShape(entry *pb.Entry) error {
	if len(entryPrefix(entry)) != len(commitment.Head{}) {
		return errors.New("malformed entry prefix")
	}
	switch kind := entry.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		if len(kind.BlockOpen.GetParentHash()) != common.HashLength {
			return errors.New("malformed open parent hash")
		}
		if len(kind.BlockOpen.GetBaseFee()) > pendingOpenBaseFeeLimit {
			return errors.New("open base fee exceeds limit")
		}
	case *pb.Entry_Record:
		var inputBytes uint64
		for _, raw := range kind.Record.GetTransactions() {
			if uint64(len(raw)) > pendingInputLimit-inputBytes {
				return errors.New("record transaction input exceeds limit")
			}
			inputBytes += uint64(len(raw))
		}
	case *pb.Entry_BlockSeal:
		if size := len(kind.BlockSeal.GetHeader()); size == 0 || size > pendingSealHeaderLimit {
			return errors.New("seal header size outside limit")
		}
	default:
		return errRefold
	}
	return nil
}

// foldEntry advances a commitment head by one entry, dispatching on kind.
func foldEntry(cur commitment.Head, e *pb.Entry) (commitment.Head, error) {
	if err := validateEntryShape(e); err != nil {
		return commitment.Head{}, err
	}
	switch k := e.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		return commitment.FoldOpen(cur, openContext(k.BlockOpen))
	case *pb.Entry_Record:
		return commitment.FoldTxs(cur, k.Record.GetTransactions()), nil
	case *pb.Entry_BlockSeal:
		return commitment.FoldSeal(cur, commitment.SealedHash(k.BlockSeal.GetHeader())), nil
	default:
		return commitment.Head{}, errRefold
	}
}

// setEntryPrefix rewrites the prefix commitment an entry carries.
func setEntryPrefix(e *pb.Entry, cur commitment.Head) bool {
	switch k := e.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		k.BlockOpen.PrefixCommitment = cur.Bytes()
	case *pb.Entry_Record:
		k.Record.PrefixCommitment = cur.Bytes()
	case *pb.Entry_BlockSeal:
		k.BlockSeal.PrefixCommitment = cur.Bytes()
	default:
		return false
	}

	return true
}

// refoldEntry clones a journal item's entry onto a new prefix, returning
// the rewritten entry with its post-fold head. Folding through foldEntry
// guarantees a refold computes exactly what the original fold did.
func refoldEntry(cur commitment.Head, item journalItem) (*pb.Entry, commitment.Head, error) {
	entry, ok := proto.Clone(item.entry).(*pb.Entry)
	if !ok || !setEntryPrefix(entry, cur) {
		return nil, commitment.Head{}, errRefold
	}

	next, err := foldEntry(cur, entry)
	if err != nil {
		return nil, commitment.Head{}, err
	}

	return entry, next, nil
}

// openEntry builds the wire entry for a block open. All open publishers
// (live build and window rebuild) must construct it identically or the
// mirror check and the consumer's context pinning would see drift.
func openEntry(oc commitment.OpenContext, prefix commitment.Head) *pb.Entry {
	return &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
		BlockNumber:      oc.Number,
		BlockTimestamp:   oc.Timestamp,
		ParentHash:       oc.ParentHash[:],
		GasLimit:         oc.GasLimit,
		BaseFee:          baseFeeBytes(oc.BaseFee),
		PrefixCommitment: prefix.Bytes(),
	}}}
}

// recordEntry builds the wire entry for one committed transaction.
func recordEntry(raw []byte, prefix commitment.Head) *pb.Entry {
	return &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{raw},
		PrefixCommitment: prefix.Bytes(),
	}}}
}

// sealEntry builds the wire entry for a sealed block header.
func sealEntry(raw []byte, prefix commitment.Head) *pb.Entry {
	return &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{
		Header:           raw,
		PrefixCommitment: prefix.Bytes(),
	}}}
}

// decodeSealHeader decodes a seal entry's RLP header.
func decodeSealHeader(raw []byte) (*types.Header, error) {
	header := new(types.Header)
	if err := rlp.DecodeBytes(raw, header); err != nil {
		return nil, err
	}

	return header, nil
}

// entryHeight extracts a height from an open or seal entry; records carry
// none.
func entryHeight(e *pb.Entry) (uint64, bool) {
	switch k := e.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		return k.BlockOpen.GetBlockNumber(), true
	case *pb.Entry_BlockSeal:
		header, err := decodeSealHeader(k.BlockSeal.GetHeader())
		if err != nil {
			return 0, false
		}

		return header.Number.Uint64(), true
	default:
		return 0, false
	}
}
