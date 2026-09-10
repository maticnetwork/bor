package state

import (
	"math/big"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
)

// SettleTo applies this tx's local state to the final StateDB — applied
// in tx-index order by the V2 executor's settle callback. This is where
// parallel per-tx state becomes part of the block's canonical state.
func (s *ParallelStateDB) SettleTo(final *StateDB) {
	s.assertSettleNotPanicked()
	// Capture pre-tx coinbase balance BEFORE applying any of this tx's
	// changes. The serial path snapshots coinbase balance before execution
	// and uses that for the fee transfer log; we must match for parity.
	preTxCoinbaseBal := final.GetBalance(s.Coinbase)

	s.settleNonces(final)
	s.settleStorage(final)
	s.settleCode(final)
	s.settleBalanceOpsAndLogs(final)
	s.settleAccountSet(final)
	s.applyFeeData(final, preTxCoinbaseBal)

	// FinaliseFast with prefetcher: moves dirty→pending, sets
	// uncommittedStorage, then triggers prefetcher for storage tries
	// (same as serial Finalise).
	final.FinaliseFastWithPrefetch(true)
}

// settleNonces applies pending nonce writes to final.
func (s *ParallelStateDB) settleNonces(final *StateDB) {
	for addr, nonce := range s.localNonces {
		final.SetNonceDirect(addr, nonce)
	}
}

// settleStorage applies pending storage writes to final. Origins may be
// stale (from pathdb), but that only affects the skip optimization in
// commitStorage — which we've removed.
func (s *ParallelStateDB) settleStorage(final *StateDB) {
	for addr, slots := range s.localStorage {
		origins := make(map[common.Hash]common.Hash, len(slots))
		for key := range slots {
			origin, berr := s.base.GetState(addr, key)
			s.noteBaseReadErr(berr)
			origins[key] = origin
		}
		final.SetStorageDirectWithOrigins(addr, slots, origins)
	}
}

// settleCode applies pending code writes (rare; uses normal hash-computing path).
func (s *ParallelStateDB) settleCode(final *StateDB) {
	for addr, code := range s.localCode {
		final.SetCode(addr, code, tracing.CodeChangeUnspecified)
	}
}

// settleBalanceOpsAndLogs replays balance operations in order, interleaving
// transfer-log emission so logs and balance changes happen in the same order
// the serial path produced them.
func (s *ParallelStateDB) settleBalanceOpsAndLogs(final *StateDB) {
	transferIdx, logIdx := 0, 0
	for opIdx := 0; opIdx < len(s.BalanceOps); opIdx++ {
		if s.tryEmitTransferAt(final, opIdx, &transferIdx, &logIdx) {
			opIdx++ // consume the paired AddBalance op
			continue
		}
		op := &s.BalanceOps[opIdx]
		amt := op.Amount
		if op.IsAdd {
			final.AddBalanceDirect(op.Addr, &amt)
		} else {
			final.SubBalanceDirect(op.Addr, &amt)
		}
	}
	// Emit any execution logs that didn't precede a transfer.
	for ; logIdx < len(s.logs); logIdx++ {
		final.AddLog(s.logs[logIdx])
	}
}

// tryEmitTransferAt detects whether the pair of balance ops at opIdx is the
// next recorded Transfer's Sub/Add. If so it applies the transfer, emits any
// preceding execution logs + the transfer log, advances *transferIdx and
// *logIdx, and returns true. Returns false otherwise.
func (s *ParallelStateDB) tryEmitTransferAt(final *StateDB, opIdx int, transferIdx, logIdx *int) bool {
	if *transferIdx >= len(s.Transfers) {
		return false
	}
	tr := &s.Transfers[*transferIdx]
	// Match by recorded position, not by op shape: RecordTransfer runs
	// immediately before the transfer's SubBalance/AddBalance, so the
	// transfer's ops live at exactly [BalanceOpsIdx, BalanceOpsIdx+1]
	// (RevertToSnapshot truncates BalanceOps and Transfers in lockstep, so
	// surviving indices never shift). Shape matching alone mispairs with
	// look-alike Sub/Add pairs — e.g. an EIP-6780 selfdestruct payout with
	// the same sender, recipient and amount as a later recorded transfer —
	// emitting the transfer log at the wrong replay point with wrong
	// input/output balances.
	if opIdx != tr.BalanceOpsIdx {
		return false
	}
	op := &s.BalanceOps[opIdx]
	if op.IsAdd || op.Addr != tr.Sender || !op.Amount.Eq(&tr.Amount) {
		return false
	}
	if opIdx+1 >= len(s.BalanceOps) {
		return false
	}
	pair := &s.BalanceOps[opIdx+1]
	if !pair.IsAdd || pair.Addr != tr.Recipient || !pair.Amount.Eq(&tr.Amount) {
		return false
	}
	for *logIdx < len(s.logs) && *logIdx < tr.LogIdx {
		final.AddLog(s.logs[*logIdx])
		*logIdx++
	}
	amt := tr.Amount
	final.SubBalanceDirect(tr.Sender, &amt)
	final.AddBalanceDirect(tr.Recipient, &amt)
	s.emitTransferLog(final, tr, &amt)
	*transferIdx++
	return true
}

// emitTransferLog calls TransferLogFn with arithmetically-derived pre/post
// balances (avoids extra GetBalance calls).
func (s *ParallelStateDB) emitTransferLog(final *StateDB, tr *TransferRecord, amt *uint256.Int) {
	if amt.Sign() <= 0 || s.TransferLogFn == nil {
		return
	}
	if tr.Sender == tr.Recipient {
		// Self-transfer: Sub+Add cancel out.
		in1 := final.GetBalance(tr.Sender).ToBig()
		s.TransferLogFn(final, tr.Sender, tr.Recipient, amt.ToBig(), in1, in1, in1, in1)
		return
	}
	post1 := final.GetBalance(tr.Sender)
	post2 := final.GetBalance(tr.Recipient)
	in1 := new(big.Int).Add(post1.ToBig(), amt.ToBig())
	in2 := new(big.Int).Sub(post2.ToBig(), amt.ToBig())
	s.TransferLogFn(final, tr.Sender, tr.Recipient, amt.ToBig(),
		in1, in2, post1.ToBig(), post2.ToBig())
}

// settleAccountSet applies self-destructs, account creations, and preimages.
func (s *ParallelStateDB) settleAccountSet(final *StateDB) {
	for addr := range s.destructed {
		final.SelfDestruct(addr)
	}
	for addr := range s.created {
		if !final.Exist(addr) {
			final.CreateAccount(addr)
		}
	}
	for hash, preimage := range s.preimages {
		final.AddPreimage(hash, preimage)
	}
}

// applyFeeData applies any deferred fee burn + tip and generates the fee
// transfer log. preTxCoinbaseBal is the coinbase balance captured before
// any of this tx's changes were applied.
func (s *ParallelStateDB) applyFeeData(final *StateDB, preTxCoinbaseBal *uint256.Int) {
	if s.FeeData == nil {
		return
	}
	if !s.FeeData.BalancesApplied {
		if s.FeeData.FeeBurnt != nil && s.FeeData.FeeBurnt.Sign() > 0 {
			amt, _ := uint256.FromBig(s.FeeData.FeeBurnt)
			final.AddBalanceDirect(s.FeeData.BurntContractAddress, amt)
		}
		if s.FeeData.FeeTipped != nil && s.FeeData.FeeTipped.Sign() > 0 {
			amt, _ := uint256.FromBig(s.FeeData.FeeTipped)
			final.AddBalanceDirect(s.Coinbase, amt)
		}
	}
	// Use preTxCoinbaseBal to match the serial path
	// (ApplyTransactionWithEVM snapshots coinbase before tx execution).
	if s.FeeLogFn == nil || s.FeeData.FeeTipped == nil || s.FeeData.FeeTipped.Sign() <= 0 {
		return
	}
	senderOut := new(big.Int).Sub(
		new(big.Int).SetBytes(s.FeeData.SenderInitBalance.Bytes()),
		s.FeeData.FeeTipped)
	input2 := preTxCoinbaseBal.ToBig()
	coinbaseOut := new(big.Int).Add(input2, s.FeeData.FeeTipped)
	s.FeeLogFn(final, s.Sender, s.Coinbase,
		s.FeeData.FeeTipped, s.FeeData.SenderInitBalance, input2,
		senderOut, coinbaseOut)
}

// GetLogs returns logs for a specific tx hash (for receipt building).
// Signature mirrors *StateDB.GetLogs — including blockTime, which feeds
// log.BlockTimestamp. The two implementations must stay in lockstep
// (enforced by TestPDBMethodParity) so PDB-built receipts carry the
// same fields as serial-built receipts.
func (s *ParallelStateDB) GetLogs(txHash common.Hash, blockNumber uint64, blockHash common.Hash, blockTime uint64) []*types.Log {
	for _, l := range s.logs {
		l.TxHash = txHash
		l.BlockNumber = blockNumber
		l.BlockHash = blockHash
		l.BlockTimestamp = blockTime
	}
	return s.logs
}
