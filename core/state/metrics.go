// Copyright 2021 The go-ethereum Authors
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

import "github.com/ethereum/go-ethereum/metrics"

var (
	accountReadMeters        = metrics.NewRegisteredMeter("state/read/account", nil)
	storageReadMeters        = metrics.NewRegisteredMeter("state/read/storage", nil)
	accountUpdatedMeter      = metrics.NewRegisteredMeter("state/update/account", nil)
	storageUpdatedMeter      = metrics.NewRegisteredMeter("state/update/storage", nil)
	accountDeletedMeter      = metrics.NewRegisteredMeter("state/delete/account", nil)
	storageDeletedMeter      = metrics.NewRegisteredMeter("state/delete/storage", nil)
	accountTrieUpdatedMeter  = metrics.NewRegisteredMeter("state/update/accountnodes", nil)
	storageTriesUpdatedMeter = metrics.NewRegisteredMeter("state/update/storagenodes", nil)
	accountTrieDeletedMeter  = metrics.NewRegisteredMeter("state/delete/accountnodes", nil)
	storageTriesDeletedMeter = metrics.NewRegisteredMeter("state/delete/storagenodes", nil)

	// FlatDiff overlay hit meters — fire when a state read is satisfied by the
	// previous block's FlatDiff instead of falling through to the committed trie.
	// Non-zero rate confirms the pipelined SRC overlay is active on this statedb
	// (applies to both block import and speculative build paths).
	//
	// These also serve as the build-side cache-visibility substitute under
	// pipelining: the speculative build path uses NewWithFlatBase, which creates
	// a plain StateDB without the instrumented prefetch/process readers that
	// populate chain/*/reads/cache/*. Those meters therefore receive no
	// build-side contribution when pipelining is enabled. Use the flatdiff
	// meters here for overlay efficiency signals in pipelined build mode.
	flatDiffAccountHitsMeter = metrics.NewRegisteredMeter("state/flatdiff/account_hits", nil)
	flatDiffStorageHitsMeter = metrics.NewRegisteredMeter("state/flatdiff/storage_hits", nil)
)
