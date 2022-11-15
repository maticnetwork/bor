package whitelist

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

type finality[T rawdb.BlockFinality] struct {
	sync.RWMutex
	db       ethdb.Database
	Hash     common.Hash // Whitelisted Hash, populated by reaching out to heimdall
	Number   uint64      // Number , populated by reaching out to heimdall
	interval uint64      // Interval, until which we can allow importing
	doExist  bool
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor finality submitted to mainchain.
func (f *finality[T]) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	// We want to validate the chain by comparing the last finalized block
	f.RLock()

	doExist := f.doExist
	number := f.Number
	hash := f.Hash

	f.RUnlock()

	return isValidPeer(remoteHeader, fetchHeadersByNumber, doExist, number, hash)
}

// IsValidChain checks the validity of chain by comparing it
// against the local checkpoint entry
// todo: need changes
func (f *finality[T]) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	// Return if we've received empty chain
	if len(chain) == 0 {
		return false
	}

	return isValidChain(currentHeader, chain, f.doExist, f.Number, f.Hash, f.interval)
}

func (f *finality[T]) Process(block uint64, hash common.Hash) {
	f.doExist = true
	f.Hash = hash
	f.Number = block

	rawdb.WriteLastFinality[T](f.db, block, hash)
}

// Get returns the existing whitelisted
// entries of checkpoint of the form (doExist,block number,block hash.)
func (f *finality[T]) Get() (bool, uint64, common.Hash) {
	f.RLock()
	defer f.RUnlock()

	if f.doExist {
		return f.doExist, f.Number, f.Hash
	}

	checkpoint, err := rawdb.ReadFinality[*rawdb.Checkpoint](f.db)
	if err != nil {
		return false, f.Number, f.Hash
	}

	return true, checkpoint.Block, checkpoint.Hash
}

// Purge purges the whitlisted checkpoint
func (f *finality[T]) Purge() {
	f.Lock()
	defer f.Unlock()

	f.doExist = false
}
