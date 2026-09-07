// Copyright 2016 The go-ethereum Authors
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

package core

import (
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// ChainContext supports retrieving headers and consensus parameters from the
// current blockchain to be used during transaction processing.
type ChainContext interface {
	consensus.ChainHeaderReader

	// Engine retrieves the chain's consensus engine.
	Engine() consensus.Engine
}

// NewEVMBlockContext creates a new context for use in the EVM.
func NewEVMBlockContext(header *types.Header, chain ChainContext, author *common.Address) vm.BlockContext {
	var (
		beneficiary common.Address
		baseFee     *big.Int
		blobBaseFee *big.Int
		random      *common.Hash
	)

	// If we don't have an explicit author (i.e. not mining), extract from the header
	if author == nil {
		if chain.Config().Bor != nil && chain.Config().Bor.IsRio(header.Number) {
			beneficiary = common.HexToAddress(chain.Config().Bor.CalculateCoinbase(header.Number.Uint64()))

			// In case of coinbase is not set post Rio, use the default coinbase
			if beneficiary == (common.Address{}) {
				beneficiary, _ = chain.Engine().Author(header)
			}
		} else {
			beneficiary, _ = chain.Engine().Author(header) // Ignore error, we're past header validation
		}
	} else {
		beneficiary = *author
	}

	if header.BaseFee != nil {
		baseFee = new(big.Int).Set(header.BaseFee)
	}
	// Only calculate blob fee if the fork actually supports blob transactions (Cancun or later)
	// and the chain has a BlobScheduleConfig configured
	if header.ExcessBlobGas != nil && chain.Config().BlobScheduleConfig != nil {
		if chain.Config().IsCancun(header.Number) {
			blobBaseFee = eip4844.CalcBlobFee(chain.Config(), header)
		}
	}
	if header.Difficulty.Sign() == 0 {
		random = &header.MixDigest
	}

	// Bor emits a synthetic "transfer log" on every value movement (see
	// core/bor_fee_log.go). On non-Bor chain configs (e.g. when running
	// Ethereum execution-spec-tests) those logs aren't part of the protocol
	// and pollute the block bloom, so swap Transfer for the no-log variant.
	// A nil chain (TestProcessParentBlockHash passes one) also falls into
	// the no-log branch since we can't read a Bor config off it.
	transferFn := EthereumTransfer
	if chain != nil {
		if cfg := chain.Config(); cfg != nil && cfg.Bor != nil {
			transferFn = Transfer
		}
	}

	return vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    transferFn,
		GetHash:     GetHashFn(header, chain),
		Coinbase:    beneficiary,
		BlockNumber: new(big.Int).Set(header.Number),
		Time:        header.Time,
		Difficulty:  new(big.Int).Set(header.Difficulty),
		BaseFee:     baseFee,
		BlobBaseFee: blobBaseFee,
		GasLimit:    header.GasLimit,
		Random:      random,
	}
}

// EthereumTransfer subtracts amount from sender and adds it to recipient,
// matching upstream go-ethereum semantics — no Bor transfer-log emission.
// Used by NewEVMBlockContext when ChainConfig.Bor is nil.
func EthereumTransfer(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
	db.SubBalance(sender, amount, tracing.BalanceChangeTransfer)
	db.AddBalance(recipient, amount, tracing.BalanceChangeTransfer)
}

// NewEVMTxContext creates a new transaction context for a single transaction.
func NewEVMTxContext(msg *Message) vm.TxContext {
	ctx := vm.TxContext{
		Origin:     msg.From,
		GasPrice:   uint256.MustFromBig(msg.GasPrice),
		BlobHashes: msg.BlobHashes,
	}
	return ctx
}

// NewEVMTxContextForStateSync returns a minimal TxContext for executing
// state-sync transactions.
func NewEVMTxContextForStateSync() vm.TxContext {
	return vm.TxContext{GasPrice: uint256.NewInt(0)}
}

// GetHashFn returns a GetHashFunc which retrieves header hashes by number
func GetHashFn(ref *types.Header, chain ChainContext) func(n uint64) common.Hash {
	// Cache will initially contain [refHash.parent],
	// Then fill up with [refHash.p, refHash.pp, refHash.ppp, ...]
	var cache []common.Hash

	cacheMutex := &sync.Mutex{}

	return func(n uint64) common.Hash {
		if ref.Number.Uint64() <= n {
			// This situation can happen if we're doing tracing and using
			// block overrides.
			return common.Hash{}
		}

		cacheMutex.Lock()
		defer cacheMutex.Unlock()

		// If there's no hash cache yet, make one
		if len(cache) == 0 {
			cache = append(cache, ref.ParentHash)
		}

		if idx := ref.Number.Uint64() - n - 1; idx < uint64(len(cache)) {
			return cache[idx]
		}
		// No luck in the cache, but we can start iterating from the last element we already know
		lastKnownHash := cache[len(cache)-1]
		lastKnownNumber := ref.Number.Uint64() - uint64(len(cache))

		for {
			header := chain.GetHeader(lastKnownHash, lastKnownNumber)
			if header == nil {
				break
			}

			cache = append(cache, header.ParentHash)
			lastKnownHash = header.ParentHash
			lastKnownNumber = header.Number.Uint64() - 1

			if n == lastKnownNumber {
				return lastKnownHash
			}
		}

		return common.Hash{}
	}
}

// SpeculativeGetHashFn returns a GetHashFunc for use during pipelined SRC
// speculative execution of block N+1, where block N's hash is not yet known
// (SRC(N) is still computing root_N).
//
// It uses three-tier resolution:
//   - Tier 1 (n == pendingBlockN): lazy-resolves by calling srcDone(), which
//     blocks until SRC(N) completes and returns hash(block_N). Cached after
//     first call.
//   - Tier 2 (n == pendingBlockN-1): returns blockN1Header.Hash() directly.
//     Block N-1 is fully committed and in the chain DB.
//   - Tier 3 (n < pendingBlockN-1): delegates to GetHashFn anchored at
//     block N-1. Its cache seeds from blockN1Header.ParentHash = hash(block_{N-2}),
//     so index 0 gives BLOCKHASH(N-2), which is correct.
//
// srcDone is called at most once and must return hash(block_N) after SRC(N)
// completes. It may block.
func SpeculativeGetHashFn(blockN1Header *types.Header, chain ChainContext, pendingBlockN uint64, srcDone func() common.Hash, blockhashNAccessed *atomic.Bool) func(uint64) common.Hash {
	blockN1Hash := blockN1Header.Hash()
	olderFn := GetHashFn(blockN1Header, chain) // blocks N-2 and below
	resolveN := newPendingBlockNResolver(srcDone, blockhashNAccessed)
	return func(n uint64) common.Hash {
		switch {
		case n >= pendingBlockN+1:
			return common.Hash{} // future block
		case n == pendingBlockN:
			return resolveN()
		case n == pendingBlockN-1:
			return blockN1Hash
		default:
			return olderFn(n)
		}
	}
}

// newPendingBlockNResolver returns a closure that lazily resolves pending
// block N's hash via srcDone. On every invocation it flags blockhashNAccessed
// so the caller knows the speculative block read BLOCKHASH(N) — the resolved
// hash is pre-seal (no signature in Extra) and will differ from the final
// on-chain hash, so the speculative execution must be aborted.
func newPendingBlockNResolver(srcDone func() common.Hash, blockhashNAccessed *atomic.Bool) func() common.Hash {
	var (
		resolvedHash common.Hash
		resolved     bool
		mu           sync.Mutex
	)
	return func() common.Hash {
		if blockhashNAccessed != nil {
			blockhashNAccessed.Store(true)
		}
		mu.Lock()
		defer mu.Unlock()
		if !resolved {
			resolvedHash = srcDone()
			resolved = true
		}
		return resolvedHash
	}
}

// CanTransfer checks whether there are enough funds in the address' account to make a transfer.
// This does not take the necessary gas in to account to make the transfer valid.
func CanTransfer(db vm.StateDB, addr common.Address, amount *uint256.Int) bool {
	return db.GetBalance(addr).Cmp(amount) >= 0
}

// Transfer subtracts amount from sender and adds amount to recipient using the given Db
func Transfer(db vm.StateDB, sender, recipient common.Address, amount *uint256.Int) {
	// In V2 BlockSTM, ParallelStateDB.RecordTransfer returns true and captures
	// the transfer for log generation during settlement. The serial StateDB
	// returns false, falling through to the original snapshot-based log path.
	// Skipping the GetBalance/ToBig calls during V2 execution avoids the #1
	// allocation hotspot (7 big.Ints per transfer, 819K allocs per block set).
	if db.RecordTransfer(sender, recipient, amount) {
		db.SubBalance(sender, amount, tracing.BalanceChangeTransfer)
		db.AddBalance(recipient, amount, tracing.BalanceChangeTransfer)
		return
	}

	// Serial path: full transfer log with balance snapshots.
	input1 := db.GetBalance(sender)
	input2 := db.GetBalance(recipient)

	db.SubBalance(sender, amount, tracing.BalanceChangeTransfer)
	db.AddBalance(recipient, amount, tracing.BalanceChangeTransfer)

	output1 := db.GetBalance(sender)
	output2 := db.GetBalance(recipient)

	AddTransferLog(db, sender, recipient, amount.ToBig(), input1.ToBig(), input2.ToBig(), output1.ToBig(), output2.ToBig())
}
