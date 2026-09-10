package registryreader

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// ReservedKey identifies a transaction by sender and nonce. The pair is unique
// within a block and is carried on the execution Message, so a classification
// computed once from the ordered block body can be looked up per transaction
// during (parallel) execution without depending on execution order.
type ReservedKey struct {
	From  common.Address
	Nonce uint64
}

// OrderClients returns ids sorted ascending by
// keccak256(8-byte-bigendian(id) || parentHash). Rotating per block keeps the
// reserved-pass quota/ceiling race from systematically favouring low-id
// clients. Producer sequencing and verifier classification must use the
// identical order, so this is the single source of truth for both.
func OrderClients(parentHash common.Hash, ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	type ranked struct {
		id     uint64
		digest common.Hash
	}
	var buf [40]byte
	copy(buf[8:], parentHash[:])
	ranks := make([]ranked, len(ids))
	for i, id := range ids {
		binary.BigEndian.PutUint64(buf[:8], id)
		ranks[i] = ranked{id: id, digest: crypto.Keccak256Hash(buf[:])}
	}
	sort.Slice(ranks, func(i, j int) bool {
		return bytes.Compare(ranks[i].digest[:], ranks[j].digest[:]) < 0
	})
	out := make([]uint64, len(ranks))
	for i, r := range ranks {
		out[i] = r.id
	}
	return out
}

// ReservedWalk carries the incremental state of the reserved-classification
// scan: per-client declared gas consumed and the set of senders past their
// first quota breach. The producer advances it tx-by-tx as it commits (so a
// skipped tx never consumes quota); ClassifyReserved runs the identical scan in
// one pass over the final block. Because classification is per-client only (see
// ClassifyReserved), the two produce the same result regardless of interleaving.
type ReservedWalk struct {
	snap    *Snapshot
	used    map[uint64]uint64       // per-client declared gas consumed by reserved txs
	blocked map[common.Address]bool // senders diverted to normal after a quota breach
}

// NewReservedWalk starts a scan against snap. A nil/empty snapshot yields a walk
// that classifies nothing.
func NewReservedWalk(snap *Snapshot) *ReservedWalk {
	return &ReservedWalk{snap: snap, used: make(map[uint64]uint64), blocked: make(map[common.Address]bool)}
}

// Peek reports whether a transaction from `from` with declared `gas` would be
// reserved (fee-free) at the current scan state, without advancing it.
//
// A sender is reserved only while its client's per-client quota has room; the
// first breach blocks that sender's remaining transactions (matching the
// producer's first-breach-blocks-later-nonces rule). There is no global ceiling:
// the registry guarantees the sum of active client quotas equals the reported
// capacity, so a cross-client cap can never bind and would only reintroduce an
// order dependence between producer and verifier.
func (w *ReservedWalk) Peek(from common.Address, gas uint64) bool {
	if w == nil || w.snap == nil {
		return false
	}
	cid, ok := w.snap.Lookup(from)
	if !ok {
		return false
	}
	// used <= quota is a loop invariant (used only grows in Commit under this
	// same guard), so quota-used never underflows.
	return !w.blocked[from] && gas <= w.snap.Quota(cid)-w.used[cid]
}

// Commit advances the scan for an included transaction whose classification
// (from Peek) is `reserved`. Call it only for transactions actually included in
// the block and in block order, so consumed quota reflects exactly the block's
// contents — a skipped candidate must not advance the scan.
func (w *ReservedWalk) Commit(from common.Address, gas uint64, reserved bool) {
	if w == nil || w.snap == nil {
		return
	}
	cid, ok := w.snap.Lookup(from)
	if !ok {
		return
	}
	if reserved {
		w.used[cid] += gas
	} else {
		w.blocked[from] = true
	}
}

// Reserved peeks and, when reserved, advances the scan in one step — the
// verifier's single-pass form over the final block.
func (w *ReservedWalk) Reserved(from common.Address, gas uint64) bool {
	reserved := w.Peek(from, gas)
	w.Commit(from, gas, reserved)
	return reserved
}

// ClientUsage reports one registry client's reserved-region gas usage for a
// single block, alongside its quota. Used is declared gas (tx.Gas()) summed
// over that client's committed reserved transactions - the same basis
// ReservedWalk.Peek/Commit charges quota against, not the executed gas a
// receipt reports afterward.
type ClientUsage struct {
	Used  uint64
	Quota uint64
}

// ClassifyReserved returns the set of transactions in txs (in final block
// order) that are reserved - i.e. execute fee-free - under snap, keyed by
// (sender, nonce), together with per-client usage for every client in snap's
// effective set (idle clients included, with Used 0). It runs a single
// ReservedWalk over the block, so a verifier and the producer (which advances
// the same walk as it commits) derive the identical split by construction. A
// nil or empty snapshot classifies nothing and returns (nil, nil). An empty
// txs classifies nothing but still reports every effective client at zero
// usage, so per-block gauges built from the map reset on empty blocks instead
// of holding the previous block's values.
//
// signer must be the same signer execution uses, so sender recovery agrees.
func ClassifyReserved(txs []*types.Transaction, signer types.Signer, snap *Snapshot) (map[ReservedKey]struct{}, map[uint64]ClientUsage) {
	if snap == nil || len(snap.clientIDs) == 0 {
		return nil, nil
	}
	walk := NewReservedWalk(snap)
	var reserved map[ReservedKey]struct{}
	if len(txs) > 0 {
		reserved = make(map[ReservedKey]struct{})
		for _, tx := range txs {
			from, err := types.Sender(signer, tx)
			if err != nil {
				continue
			}
			if walk.Reserved(from, tx.Gas()) {
				reserved[ReservedKey{From: from, Nonce: tx.Nonce()}] = struct{}{}
			}
		}
	}
	clients := snap.Clients()
	usage := make(map[uint64]ClientUsage, len(clients))
	for _, id := range clients {
		usage[id] = ClientUsage{Used: walk.used[id], Quota: snap.Quota(id)}
	}
	return reserved, usage
}
