package miner

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/metrics"
)

// Producer-side reserved-region signals that leave no trace in the block
// (they reflect the local pool view and build timing), so they can't be
// reconstructed later from chain data.
var (
	// reservedGasUsedGauge records each build's reserved-region gas total
	// (the value written into BlockExtraData.ReservedGasUsed).
	reservedGasUsedGauge = metrics.NewRegisteredGauge("worker/reserved/gasused", nil)
	// reservedCapacityGauge records each build's effective reserved capacity
	// (the value written into BlockExtraData.ReservedCapacity).
	reservedCapacityGauge = metrics.NewRegisteredGauge("worker/reserved/capacity", nil)
	// reservedOverflowMeter counts reserved-eligible transactions diverted to
	// the normal pass because they exceeded their client's per-client quota.
	reservedOverflowMeter = metrics.NewRegisteredMeter("worker/reserved/overflow", nil)
)

type transactionsByPriceAndNonceFn func(txs map[common.Address][]*txpool.LazyTransaction) *transactionsByPriceAndNonce

// excludeReserved drops registry-registered senders from the priority list so
// they flow through the reserved pass instead. Returns prio unchanged when no
// registry snapshot is effective.
func excludeReserved(prio []common.Address, snap *registryreader.Snapshot) []common.Address {
	if snap == nil || len(prio) == 0 {
		return prio
	}
	out := prio[:0:0]
	for _, addr := range prio {
		if !snap.IsReserved(addr) {
			out = append(out, addr)
		}
	}
	return out
}

// extractPriorityTxs filters the transactions from priority senders and
// returns them grouped by price and nonce.
func extractPriorityTxs(prio []common.Address, pendingTxs map[common.Address][]*txpool.LazyTransaction, newNormalTxSet transactionsByPriceAndNonceFn) *transactionsByPriceAndNonce {
	prioPlainTxs := make(map[common.Address][]*txpool.LazyTransaction)
	for _, account := range prio {
		if txs := pendingTxs[account]; len(txs) > 0 {
			delete(pendingTxs, account)
			prioPlainTxs[account] = txs
		}
	}
	return newNormalTxSet(prioPlainTxs)
}

// filterReservedTxs removes the transactions from reserved-eligible senders from
// pending transactions and returns them grouped by client ID.
func filterReservedTxs(pendingTxs map[common.Address][]*txpool.LazyTransaction, snap *registryreader.Snapshot) map[uint64]map[common.Address][]*txpool.LazyTransaction {
	var reserved = make(map[uint64]map[common.Address][]*txpool.LazyTransaction)
	for addr, txs := range pendingTxs {
		cid, ok := snap.Lookup(addr)
		if !ok {
			continue
		}
		if reserved[cid] == nil {
			reserved[cid] = make(map[common.Address][]*txpool.LazyTransaction)
		}
		reserved[cid][addr] = txs
		delete(pendingTxs, addr)
	}
	return reserved
}

// selectReservedTxs picks the best transactions of a reserved client capped by their
// quota. Transactions are ordered in ascending order of price (opposite from usual logic)
// to ensure that zero or below base fee transactions win over transactions carrying
// fallback fees (who can anyways compete in EIP-1559 fee market).
//
// The quota is measured by the transaction gas limit since actual gas used is only known
// after execution; used reports the declared gas the selection consumed so the caller
// can charge it against the global reserved ceiling. Transactions overflowing the quota
// are handed back to the caller for the normal pass.
//
// A sender's first quota breach diverts that transaction and every later nonce of
// the same sender to overflow: reserved groups commit before the normal pass, so
// selecting nonce n+1 after diverting nonce n would break the sender's nonce order.
//
// If block building is interrupted while the scan heap is being constructed, the
// constructor yields an empty heap, so this client contributes nothing to the
// build and its transactions stay in the pool for a later block. No overflow is
// re-added in that case, and it doesn't need to be: pendingTxs here is a local
// snapshot that's discarded once the (likewise-interrupted) commit pass returns,
// and the real pool is untouched — matching how an interrupted commit defers the
// remaining transactions.
func selectReservedTxs(
	clientTxs map[common.Address][]*txpool.LazyTransaction, quota uint64, newReservedTxSet transactionsByPriceAndNonceFn,
) (selected *transactionsByPriceAndNonce, used uint64, overflow map[common.Address][]*txpool.LazyTransaction) {
	scan := newReservedTxSet(clientTxs)
	selectedTxs := make(map[common.Address][]*txpool.LazyTransaction)
	overflow = make(map[common.Address][]*txpool.LazyTransaction)
	blocked := make(map[common.Address]bool)

	for {
		ltx, _ := scan.Peek()
		if ltx == nil {
			break
		}
		from, _ := scan.PeekFrom()
		// used <= quota is a loop invariant: it starts at 0 and only grows in the
		// else branch, where the guard guarantees ltx.Gas <= quota-used, capping the
		// new total at quota. Hence quota-used never underflows, and unlike
		// used+ltx.Gas the comparison can't wrap for any registry-supplied quota.
		if blocked[from] || ltx.Gas > quota-used {
			blocked[from] = true
			overflow[from] = append(overflow[from], ltx)
		} else {
			selectedTxs[from] = append(selectedTxs[from], ltx)
			used += ltx.Gas
		}
		scan.Shift()
	}

	selected = newReservedTxSet(selectedTxs)
	return selected, used, overflow
}

// extractReservedTxs pulls reserved-eligible transactions out of pendingTxs and
// returns one quota-trimmed, ordered group per client, in the deterministic
// parent-hash-keyed client order. snap is the caller's per-build registry
// snapshot; nil (pre-fork, or no registry configured) or one with no clients
// is a no-op. Per-client quota overflow is re-added to pendingTxs for the
// normal pass.
//
// Placement is bounded by per-client quota only — the single classification
// rule the verifier's registryreader.ClassifyReserved also applies. There is
// deliberately no global capacity ceiling here: the registry guarantees
// Σ(active client quotas) == Capacity() (asserted at snapshot build, see
// registryreader.BuildSnapshot), so a cross-client cap could never bind, and
// applying one on the producer while the verifier does not would reintroduce a
// produce/verify classification asymmetry.
func extractReservedTxs(snap *registryreader.Snapshot, parentHash common.Hash, pendingTxs map[common.Address][]*txpool.LazyTransaction, newReservedTxSet transactionsByPriceAndNonceFn) []*transactionsByPriceAndNonce {
	clients := snap.Clients() // nil-safe; returns nil for a nil snapshot
	if len(clients) == 0 {
		return nil
	}
	reservedTxs := filterReservedTxs(pendingTxs, snap)
	if len(reservedTxs) == 0 {
		return nil
	}

	clientOrder := registryreader.OrderClients(parentHash, clients)
	var clientGroups = make([]*transactionsByPriceAndNonce, 0, len(clientOrder))
	for _, cid := range clientOrder {
		clientTxs := reservedTxs[cid]
		if len(clientTxs) == 0 {
			continue
		}

		selected, _, overflow := selectReservedTxs(clientTxs, snap.Quota(cid), newReservedTxSet)
		for addr, txs := range overflow {
			pendingTxs[addr] = append(pendingTxs[addr], txs...)
			reservedOverflowMeter.Mark(int64(len(txs)))
		}
		if selected.Empty() {
			continue
		}
		clientGroups = append(clientGroups, selected)
	}

	return clientGroups
}

// sequenceTxs orders the pending transactions into the groups the block is
// filled from, in commit order: priority senders first, then each reserved
// client (deterministic, parent-hash-keyed order), then the remaining normal
// transactions. Each group is an independently-ordered transactionsByPriceAndNonce
// the caller commits in turn. Empty groups are omitted so the caller never spins
// up a commit pass for nothing. snap is the caller's per-build registry
// snapshot (see worker.sequencingSnapshot); nil disables the reserved pass.
//
// There is no error path — sequencing only groups and orders an in-memory
// snapshot; interruption is surfaced later, by commitTransactions.
func (w *worker) sequenceTxs(env *environment, snap *registryreader.Snapshot, pendingTxs map[common.Address][]*txpool.LazyTransaction) []*transactionsByPriceAndNonce {
	w.mu.RLock()
	prio := w.prio
	w.mu.RUnlock()

	// The heaps observe the per-build timeout flag, so a pipelined build's
	// interrupt stops batch iteration exactly like the global override does.
	timeoutInterrupt, _ := w.interruptStateForEnv(env)

	newNormalTxSet := func(txs map[common.Address][]*txpool.LazyTransaction) *transactionsByPriceAndNonce {
		return newTransactionsByPriceAndNonce(env.signer, txs, env.header.BaseFee, timeoutInterrupt)
	}
	newReservedTxSet := func(txs map[common.Address][]*txpool.LazyTransaction) *transactionsByPriceAndNonce {
		return newReservedTransactionsByNonce(env.signer, txs, env.header.BaseFee, timeoutInterrupt)
	}

	var txBatches = make([]*transactionsByPriceAndNonce, 0)

	// Registered senders are never taken by the priority pass: they must be
	// classified by the quota walk so the producer and a verifier agree. A
	// verifier rederives classification from the ordered body and cannot see the
	// operator-local priority list, so priority-placing a registered sender would
	// split produce vs verify. Dropping them from prio keeps them in pendingTxs
	// for the reserved pass instead.
	prio = excludeReserved(prio, snap)

	// Priority transactions (operator override) commit first, paying normal fees.
	if prioTxs := extractPriorityTxs(prio, pendingTxs, newNormalTxSet); !prioTxs.Empty() {
		txBatches = append(txBatches, prioTxs)
	}

	// Reserved transactions, one ordered group per client. No emptiness check
	// here, unlike the neighbouring groups: extractReservedTxs already omits
	// empty groups from the slice it returns, so every element is committable.
	txBatches = append(txBatches, extractReservedTxs(snap, env.header.ParentHash, pendingTxs, newReservedTxSet)...)

	// Everything left (including reserved quota overflow added back above) is
	// normal. Heap-init time is recorded for the normal batch only, keeping
	// the metric's historical meaning.
	if len(pendingTxs) > 0 {
		heapInitTime := time.Now()
		normalTxs := newNormalTxSet(pendingTxs)
		txHeapInitTimer.UpdateSince(heapInitTime)

		if !normalTxs.Empty() {
			txBatches = append(txBatches, normalTxs)
		}
	}

	return txBatches
}
