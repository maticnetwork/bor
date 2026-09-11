// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

// witnessWalkItem is one cached read key claimed for a witness trie walk.
type witnessWalkItem struct {
	addr    common.Address
	slot    common.Hash
	account bool
}

// resolveCachedKeysIntoTrie re-resolves cached accounts and storage slots
// through the trie reader so its tracers hold their trie paths. Keys whose
// first resolution came from a flat reader (snapshot / pathdb) — or that were
// warmed by the prefetcher and then served to execution from this cache —
// never touch the trie during execution, so witness collection has to walk
// them explicitly or the produced witness misses their paths.
//
// Each entry is walked at most once per block (atomic walked flag), so the
// method is incremental: a prewalker can call it repeatedly while execution
// is still running to keep the walk off the block's critical path, and the
// settle-time call only drains whatever accumulated since the last sweep.
// Resolution errors are ignored: the trie reader is the gatekeeper for
// committed state, and a stateless consumer validates the resulting witness
// anyway. Returns the number of keys walked in this sweep.
func (r *readerWithCache) resolveCachedKeysIntoTrie(tr *trieReader, workers int, force bool) int {
	// Generation fast path: skip Ranging the maps when nothing new was
	// cached since the last sweep — the prewalker ticks far more often than
	// keys arrive. gen is captured before claiming, so an insert racing the
	// sweep leaves insertGen ahead of sweptGen and re-arms the next sweep.
	//
	// force defeats it for the final drain. A key held back by the witness
	// filter becomes walkable when its incarnation settles, which promotes it
	// without inserting anything — so generation alone would leave committed
	// reads unwalked and the witness short of nodes it genuinely needs.
	gen := r.insertGen.Load()
	if !force && gen == r.sweptGen.Load() {
		return 0
	}
	pending := r.claimUnwalkedItems()
	r.sweptGen.Store(gen)
	if len(pending) == 0 {
		return 0
	}
	walkWitnessItems(tr, pending, workers)
	return len(pending)
}

// claimUnwalkedItems flips the walked flag on every cached entry that hasn't
// been walked yet and returns those keys. Claiming first keeps a sweep that
// finds nothing down to one Range pass with no goroutines spawned.
func (r *readerWithCache) claimUnwalkedItems() []witnessWalkItem {
	var pending []witnessWalkItem

	// The filter is consulted before the claim, never after: claiming flips
	// walked, and a walk resolves the key's nodes into the trie tracers
	// permanently. A key held back here stays unclaimed and is reconsidered
	// on the next sweep, once its incarnation has settled.
	f := r.witnessFilter.Load()

	r.accounts.Range(func(k, v any) bool {
		addr := k.(common.Address)
		if !f.AccountWalkable(addr) {
			return true
		}
		if v.(*accountCacheEntry).walked.CompareAndSwap(false, true) {
			pending = append(pending, witnessWalkItem{addr: addr, account: true})
		}
		return true
	})
	r.storageCache.Range(func(k, v any) bool {
		key := k.(storageKey)
		if !f.SlotWalkable(key.addr, key.slot) {
			return true
		}
		if v.(*storageCacheEntry).walked.CompareAndSwap(false, true) {
			pending = append(pending, witnessWalkItem{addr: key.addr, slot: key.slot})
		}
		return true
	})
	return pending
}

// walkWitnessItems resolves the claimed keys through the trie reader,
// fanning out across workers when the batch is large enough to benefit.
func walkWitnessItems(tr *trieReader, pending []witnessWalkItem, workers int) {
	walk := func(it witnessWalkItem) {
		if it.account {
			_, _ = tr.Account(it.addr)
		} else {
			_, _ = tr.Storage(it.addr, it.slot)
		}
	}
	workers = min(workers, len(pending))
	if workers <= 1 {
		for _, it := range pending {
			walk(it)
		}
		return
	}

	items := make(chan witnessWalkItem)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range items {
				walk(it)
			}
		}()
	}
	for _, it := range pending {
		items <- it
	}
	close(items)
	wg.Wait()
}
