package blockstm

import (
	"sort"
	"sync"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
)

// ---------------------------------------------------------------------------
// MVBalanceStore — commutative delta store for balance
// ---------------------------------------------------------------------------

const (
	mvBalanceShards = 64
	// initialMVBalanceShardCap is the per-shard map capacity hint —
	// distinct from mvBalanceShards. Balance deltas are sparser than
	// MVStore entries (only addresses with explicit AddBalance / SubBalance
	// in a tx land here), so 16 starting capacity per shard is enough to
	// avoid early growth without over-allocating.
	initialMVBalanceShardCap = 16
)

type BalanceDelta struct {
	TxIdx int
	Add   uint256.Int
	Sub   uint256.Int
}

type mvBalanceShard struct {
	mu   sync.RWMutex
	data map[common.Address][]BalanceDelta
}

type MVBalanceStore struct {
	shards [mvBalanceShards]mvBalanceShard
}

func NewMVBalanceStore() *MVBalanceStore {
	s := &MVBalanceStore{}
	for i := range s.shards {
		s.shards[i].data = make(map[common.Address][]BalanceDelta, initialMVBalanceShardCap)
	}
	return s
}

func (s *MVBalanceStore) shard(addr common.Address) *mvBalanceShard {
	return &s.shards[addrShardIndex(addr[:])%mvBalanceShards]
}

// WriteDelta accumulates add/sub into the (addr, txIdx) entry, creating it
// if necessary. Multiple WriteDelta calls for the same key sum together.
func (s *MVBalanceStore) WriteDelta(addr common.Address, txIdx int, add, sub *uint256.Int) {
	sh := s.shard(addr)
	sh.mu.Lock()
	entries := sh.data[addr]
	pos := sort.Search(len(entries), func(i int) bool { return entries[i].TxIdx >= txIdx })

	var a, su uint256.Int
	if add != nil {
		a.Set(add)
	}
	if sub != nil {
		su.Set(sub)
	}

	if pos < len(entries) && entries[pos].TxIdx == txIdx {
		entries[pos].Add.Add(&entries[pos].Add, &a)
		entries[pos].Sub.Add(&entries[pos].Sub, &su)
	} else {
		entries = append(entries, BalanceDelta{})
		copy(entries[pos+1:], entries[pos:])
		entries[pos] = BalanceDelta{TxIdx: txIdx, Add: a, Sub: su}
		sh.data[addr] = entries
	}
	sh.mu.Unlock()
}

// ReadDelta returns accumulated (add, sub) from entries before txIdx.
func (s *MVBalanceStore) ReadDelta(addr common.Address, txIdx int) (add, sub uint256.Int) {
	return s.ReadDeltaAfter(addr, -1, txIdx)
}

// ReadDeltaAfter returns accumulated (add, sub) from entries whose TxIdx is
// in (afterIdx, beforeIdx). Entries at or before afterIdx are skipped: a
// prior tx destroyed the account at afterIdx, deleting it at that tx's
// finalisation, so the base balance and every delta up to and including the
// destroying tx are discarded (EIP-6780 same-tx create/destruct burns even
// value received after the opcode). Only deltas from txs after the
// destruction recreate a balance. afterIdx == -1 means no prior destruction,
// so all entries before beforeIdx are summed.
func (s *MVBalanceStore) ReadDeltaAfter(addr common.Address, afterIdx, beforeIdx int) (add, sub uint256.Int) {
	sh := s.shard(addr)
	sh.mu.RLock()
	for _, e := range sh.data[addr] {
		if e.TxIdx <= afterIdx {
			continue
		}
		if e.TxIdx >= beforeIdx {
			break
		}
		add.Add(&add, &e.Add)
		sub.Add(&sub, &e.Sub)
	}
	sh.mu.RUnlock()
	return
}

// GetTxDelta returns this specific tx's delta.
func (s *MVBalanceStore) GetTxDelta(addr common.Address, txIdx int) (add, sub uint256.Int, found bool) {
	sh := s.shard(addr)
	sh.mu.RLock()
	entries := sh.data[addr]
	pos := sort.Search(len(entries), func(i int) bool { return entries[i].TxIdx >= txIdx })
	if pos < len(entries) && entries[pos].TxIdx == txIdx {
		add.Set(&entries[pos].Add)
		sub.Set(&entries[pos].Sub)
		found = true
	}
	sh.mu.RUnlock()
	return
}

// ZeroDelta zeros the delta at txIdx but keeps the entry, so a subsequent
// WriteDelta from the re-executed incarnation accumulates from zero
// (matching serial's post-Finalise semantics).
func (s *MVBalanceStore) ZeroDelta(txIdx int, addrs []common.Address) {
	for _, addr := range addrs {
		sh := s.shard(addr)
		sh.mu.Lock()
		entries := sh.data[addr]
		pos := sort.Search(len(entries), func(i int) bool { return entries[i].TxIdx >= txIdx })
		if pos < len(entries) && entries[pos].TxIdx == txIdx {
			entries[pos].Add.Clear()
			entries[pos].Sub.Clear()
		}
		sh.mu.Unlock()
	}
}

// DeleteSingle removes the balance delta for a specific (txIdx, addr).
func (s *MVBalanceStore) DeleteSingle(addr common.Address, txIdx int) {
	sh := s.shard(addr)
	sh.mu.Lock()
	entries := sh.data[addr]
	pos := sort.Search(len(entries), func(i int) bool { return entries[i].TxIdx >= txIdx })
	if pos < len(entries) && entries[pos].TxIdx == txIdx {
		entries = append(entries[:pos], entries[pos+1:]...)
		sh.data[addr] = entries
	}
	sh.mu.Unlock()
}
