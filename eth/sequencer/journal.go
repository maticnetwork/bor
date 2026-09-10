package sequencer

import (
	"bytes"
	"sort"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func openMatchesHeader(open *pb.BlockOpen, header *types.Header) bool {
	return open != nil && open.GetBlockTimestamp() == header.Time &&
		open.GetGasLimit() == header.GasLimit &&
		common.BytesToHash(open.GetParentHash()) == header.ParentHash &&
		bytes.Equal(open.GetBaseFee(), baseFeeBytes(header.BaseFee))
}

func recordsMirrorTxs(items []journalItem, txs types.Transactions) bool {
	i := 0

	for _, it := range items {
		if it.kind != entryRecord {
			return false
		}

		for _, h := range it.txHashes {
			if i >= len(txs) || h != txs[i].Hash() {
				return false
			}

			i++
		}
	}

	return i == len(txs)
}

// How a window standing in the store relates to the one being built.
type windowRelation int

const (
	windowForeign     windowRelation = iota // a different lineage: contention
	windowOurs                              // ours, whole or a prefix of it
	windowExtendsOurs                       // ours, plus records we do not hold
)

// relateWindowLocked compares the window the read returned against the one
// this node holds, entry by entry. Length alone cannot separate the cases:
// a rival's window at the same height can be shorter, equal, or longer than
// ours, and only content says which lineage it belongs to.
func (p *Publisher) relateWindowLocked(info tailInfo, height uint64) windowRelation {
	ours := p.journal.suffixFromHeight(height)
	if len(info.window) == 0 || len(ours) == 0 || ours[0].kind != entryOpen {
		return windowForeign
	}
	if !contentEqual(info.window[0], ours[0].entry) {
		return windowForeign
	}

	theirs, ok := windowTxHashes(info.window)
	if !ok {
		return windowForeign
	}
	oursHashes := journalRecordHashes(ours[1:])
	for i := 0; i < min(len(theirs), len(oursHashes)); i++ {
		if theirs[i] != oursHashes[i] {
			return windowForeign
		}
	}
	if len(theirs) > len(oursHashes) {
		return windowExtendsOurs
	}

	return windowOurs
}

func journalRecordHashes(items []journalItem) []common.Hash {
	var hashes []common.Hash
	for _, item := range items {
		if item.kind != entryRecord {
			break
		}
		hashes = append(hashes, item.txHashes...)
	}
	return hashes
}

const (
	entryOpen = iota
	entryRecord
	entrySeal
)

// journalItem is one published (or provisionally folded, while the store is
// unreachable) entry with the fold heads around it. Items carry a monotonic
// seq so cursor positions survive eviction.
type journalItem struct {
	seq    uint64
	entry  *pb.Entry
	pre    commitment.Head
	post   commitment.Head
	kind   int
	height uint64
	// txHashes caches a record's transaction hashes at append time, so the
	// per-block mirror checks compare 32-byte values instead of re-decoding
	// and re-hashing the window's wire bytes under the publisher lock.
	txHashes []common.Hash
}

// journal is the publisher's window in flight: the open window, the
// sealed-but-undelivered flushes actively streaming (at most
// journalHotSeals — older ones collapse to a height range and are rebuilt
// from the chain database when the store can take them), and up to
// journalSealedBlocks delivered blocks kept for reconnect replay. The
// open window is always retained. The chain database is the archive;
// the journal never grows beyond the work in flight.
type journal struct {
	items   []journalItem
	seals   int
	nextSeq uint64
}

const (
	journalSealedBlocks = 8
	// journalHotSeals bounds the undelivered flushes kept in memory; older
	// ones collapse to a pending height range (rebuilt from the chain
	// database on reconcile).
	journalHotSeals = 2
	// backfillBatchBytes caps how much block rebuilding one drain cycle does
	// while holding the publisher lock. Every miner call — PublishTx most of
	// all — waits on that lock, so a large batch stalls production outright:
	// a 32MB batch cost a devnet 44 seconds on the cycle after a store
	// outage. Smaller batches drain over more cycles and never own the lock
	// long enough to be felt.
	backfillBatchBytes = 2 << 20

	// journalMaxBytes caps the adoption-collection read and one backfill
	// batch — not a retention bound.
	journalMaxBytes = 32 << 20

	// backfillDepthCap bounds the drain when no canonical milestone is
	// available to floor it (Heimdall down alongside the store): at most
	// this many blocks below the tip are rebuilt, the rest jumped. With a
	// milestone the floor is finality itself, normally far tighter.
	backfillDepthCap = 40
)

func newJournal() *journal {
	return &journal{nextSeq: 1}
}

func (r *journal) append(entry *pb.Entry, pre, post commitment.Head, kind int, height uint64, ackedThrough uint64, txHashes []common.Hash) {
	// Every record carries its hashes; decode here only for a caller that
	// could not supply them cheaper.
	if kind == entryRecord && txHashes == nil {
		for _, raw := range entry.GetRecord().GetTransactions() {
			tx := new(types.Transaction)
			if err := tx.UnmarshalBinary(raw); err != nil {
				break
			}

			txHashes = append(txHashes, tx.Hash())
		}
	}

	item := journalItem{
		seq:      r.nextSeq,
		entry:    entry,
		pre:      pre,
		post:     post,
		kind:     kind,
		height:   height,
		txHashes: txHashes,
	}

	r.nextSeq++
	r.items = append(r.items, item)

	if kind == entrySeal {
		r.seals++
	}

	r.evict(ackedThrough)
}

// openStart returns the index of the open entry starting the current
// (unsealed) window, or -1 when the journal's tail is sealed or empty.
func (r *journal) openStart() int {
	for i := len(r.items) - 1; i >= 0; i-- {
		switch r.items[i].kind {
		case entrySeal:
			return -1
		case entryOpen:
			return i
		}
	}

	return -1
}

// evict drops whole sealed blocks from the front, never touching the
// current open window. Delivered blocks evict past journalSealedBlocks (the
// gap-fill replay depth); undelivered seals are what a flush still owes
// the store and ride out retry pacing — until the hard count or byte cap,
// whose overflow is the documented forward-jump.
func (r *journal) evict(ackedThrough uint64) {
	for r.seals > journalSealedBlocks && r.oldestSealAcked(ackedThrough) && r.dropOldestSealed() {
	}
}

// collapseOldestUnacked drops the oldest sealed block whose seal is still
// undelivered, returning its height so the caller can record it for a
// chain-database rebuild. Delivered blocks in front of it drop too — the
// store already has them.
func (r *journal) collapseOldestUnacked(acked uint64) (uint64, int, bool) {
	i := r.firstSeal()
	for i >= 0 && r.items[i].seq <= acked {
		r.dropOldestSealed()
		i = r.firstSeal()
	}

	if i < 0 {
		return 0, 0, false
	}

	h := r.items[i].height
	removed := 0

	for j := 0; j <= i; j++ {
		if r.items[j].seq > acked {
			removed++
		}
	}

	r.dropOldestSealed()

	return h, removed, true
}

// firstSeal returns the index of the oldest seal, or -1 when none.
func (r *journal) firstSeal() int {
	for i := range r.items {
		if r.items[i].kind == entrySeal {
			return i
		}
	}

	return -1
}

// oldestSealAcked reports whether the front sealed block was delivered.
func (r *journal) oldestSealAcked(ackedThrough uint64) bool {
	i := r.firstSeal()

	return i >= 0 && r.items[i].seq <= ackedThrough
}

// dropOldestSealed removes items from the front through the first seal.
// Returns false when no sealed block remains to drop.
func (r *journal) dropOldestSealed() bool {
	i := r.firstSeal()
	if i < 0 {
		return false
	}

	r.items = append(r.items[:0], r.items[i+1:]...)
	r.seals--

	return true
}

// truncate drops every item from index n on, keeping the byte and seal
// accounting exact.
func (r *journal) truncate(n int) {
	for _, dropped := range r.items[n:] {
		if dropped.kind == entrySeal {
			r.seals--
		}
	}

	r.items = r.items[:n]
}

// cutFromHeight returns the index where the undelivered run at or above
// height begins — the truncation point that drops a refused flush and
// anything chained above it while keeping acked entries and older flushes.
func (r *journal) cutFromHeight(acked, height uint64) int {
	cut := len(r.items)

	for cut > 0 {
		it := r.items[cut-1]
		if it.seq <= acked || it.height < height {
			break
		}

		cut--
	}

	return cut
}

// rebuildCut returns the index just past the last seal or last acked item —
// the boundary a window rebuild must not rewind past (an undelivered flush
// is still owed to the store; confirmed entries never rewind).
func (r *journal) rebuildCut(acked uint64) int {
	for i := len(r.items) - 1; i >= 0; i-- {
		if it := r.items[i]; it.kind == entrySeal || it.seq <= acked {
			return i + 1
		}
	}

	return 0
}

// itemAt returns the journal item carrying seq, if it is still present —
// lineage swaps and rewinds replace or drop items, so a seq alone no
// longer identifies live content.
func (r *journal) itemAt(seq uint64) (journalItem, bool) {
	i := sort.Search(len(r.items), func(i int) bool { return r.items[i].seq >= seq })
	if i < len(r.items) && r.items[i].seq == seq {
		return r.items[i], true
	}

	return journalItem{}, false
}

// after returns the items strictly after seq, and whether that position is
// still covered by the journal (false means eviction created a gap).
func (r *journal) after(seq uint64) ([]journalItem, bool) {
	if len(r.items) == 0 {
		return nil, seq+1 >= r.nextSeq
	}

	if seq+1 < r.items[0].seq {
		return nil, false
	}

	i := sort.Search(len(r.items), func(i int) bool { return r.items[i].seq > seq })

	return r.items[i:], true
}

// findPost returns the seq of the item whose post-fold head equals h.
func (r *journal) findPost(h commitment.Head) (uint64, bool) {
	for i := range r.items {
		if r.items[i].post == h {
			return r.items[i].seq, true
		}
	}

	return 0, false
}

// suffixFromHeight returns the retained items starting at the first open
// entry with height >= h (a window boundary), or nil when none exists.
func (r *journal) suffixFromHeight(h uint64) []journalItem {
	for i := range r.items {
		if r.items[i].kind == entryOpen && r.items[i].height >= h {
			return r.items[i:]
		}
	}

	return nil
}

// journalWindowLocked lists the transactions this publisher has already
// published for a height, in order.
func (p *Publisher) journalWindowLocked(height uint64) []common.Hash {
	start := p.journal.openStart()
	if start < 0 || p.journal.items[start].height != height {
		return nil
	}

	var out []common.Hash

	for _, it := range p.journal.items[start+1:] {
		if it.kind == entryRecord {
			out = append(out, it.txHashes...)
		}
	}

	return out
}

// windowTxHashes lists the transactions a store window promises, in order.
func windowTxHashes(window []*pb.Entry) ([]common.Hash, bool) {
	var out []common.Hash

	for _, e := range window {
		rec := e.GetRecord()
		if rec == nil {
			continue
		}

		for _, raw := range rec.GetTransactions() {
			tx := new(types.Transaction)
			if err := tx.UnmarshalBinary(raw); err != nil {
				return nil, false
			}

			out = append(out, tx.Hash())
		}
	}

	return out, true
}

// windowLeadsHashes reports whether the block delivers the promised
// sequence: the window's transactions must be a leading run of the block's,
// so a block may still be filling past the window but may never drop or
// reorder what the store already acked.
func windowLeadsHashes(window, txs []common.Hash) bool {
	if len(window) > len(txs) {
		return false
	}

	for i, h := range window {
		if txs[i] != h {
			return false
		}
	}

	return true
}

// txHashes projects transactions onto their hashes.
func txHashes(txs types.Transactions) []common.Hash {
	out := make([]common.Hash, len(txs))
	for i, tx := range txs {
		out[i] = tx.Hash()
	}

	return out
}
