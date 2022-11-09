package whitelist

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrMismatch = errors.New("Mismatch Error")
	ErrNoRemote = errors.New("remote peer doesn't have a target block number")
)

type WhitelistService struct {
	checkpoint
	milestone
}

func NewService() *WhitelistService {
	return &WhitelistService{

		checkpoint{
			doExist:  false,
			interval: 256,
		},

		milestone{
			doExist:            false,
			interval:           256,
			LockedMilestoneIds: make(map[string]bool),
		},
	}
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor checkpoint submitted to mainchain and last milestone voted in the heimdall
func (s *WhitelistService) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	checkpointBool, err := s.checkpoint.IsValidPeer(remoteHeader, fetchHeadersByNumber)
	if !checkpointBool {
		return checkpointBool, err
	}

	milestoneBool, err := s.milestone.IsValidPeer(remoteHeader, fetchHeadersByNumber)
	if !milestoneBool {
		return milestoneBool, err
	}

	return true, nil
}

// IsValidChain checks the validity of chain by comparing it
// against the local checkpoint entries and milestone entries
func (s *WhitelistService) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	checkpointBool := s.checkpoint.IsValidChain(currentHeader, chain)

	if !checkpointBool {
		return checkpointBool
	}

	milestoneBool := s.milestone.IsValidChain(currentHeader, chain)
	if !milestoneBool {
		return milestoneBool
	}

	return true
}

func splitChain(current uint64, chain []*types.Header) ([]*types.Header, []*types.Header) {
	var (
		pastChain   []*types.Header
		futureChain []*types.Header
		first       uint64 = chain[0].Number.Uint64()
		last        uint64 = chain[len(chain)-1].Number.Uint64()
	)

	if current >= first {
		if len(chain) == 1 || current >= last {
			pastChain = chain
		} else {
			pastChain = chain[:current-first+1]
		}
	}

	if current < last {
		if len(chain) == 1 || current < first {
			futureChain = chain
		} else {
			futureChain = chain[current-first+1:]
		}
	}

	return pastChain, futureChain
}

func isValidChain(currentHeader *types.Header, chain []*types.Header, doExist bool, number uint64, hash common.Hash, interval uint64) bool {

	// Check if we have milestone to validate incoming chain in memory
	if !doExist {
		// We don't have any entry, no additional validation will be possible
		return true
	}

	current := currentHeader.Number.Uint64()

	// Check if we have milestoneList entries in required range
	if chain[len(chain)-1].Number.Uint64() < number {
		// We have future milestone entries, so we don't need to receive the past chain

		return false
	}

	// Split the chain into past and future chain
	pastChain, futureChain := splitChain(current, chain)

	// Add an offset to future chain if it's not in continuity
	offset := 0
	if len(futureChain) != 0 {
		offset += int(futureChain[0].Number.Uint64()-currentHeader.Number.Uint64()) - 1
	}

	// Don't accept future chain of unacceptable length (from current block)
	if len(futureChain)+offset > int(interval) {
		return false
	}

	// Iterate over the chain and validate against the last milestone
	// It will handle all cases when the incoming chain has atleast one milestone
	for i := len(pastChain) - 1; i >= 0; i-- {
		if pastChain[i].Number.Uint64() == number {
			return pastChain[i].Hash() == hash
		}
	}

	return true
}

func isValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error), doExist bool, number uint64, hash common.Hash) (bool, error) {

	// Check for availaibility of the last milestone block.
	// This can be also be empty if our heimdall is not responding
	// or we're running without it.
	if !doExist {
		// worst case, we don't have the milestone in memory
		return true, nil
	}

	// todo: we can extract this as an interface and mock as well or just test IsValidChain in isolation from downloader passing fake fetchHeadersByNumber functions
	headers, hashes, err := fetchHeadersByNumber(number, 1, 0, false)
	if err != nil {
		return false, fmt.Errorf("%w: last whitelisted block number %d, err %v", ErrNoRemote, number, err)
	}

	if len(headers) == 0 {
		return false, fmt.Errorf("%w: last whitlisted block number %d", ErrNoRemote, number)
	}

	reqBlockNum := headers[0].Number.Uint64()
	reqBlockHash := hashes[0]

	// Check against the whitelisted blocks
	if reqBlockNum == number && reqBlockHash == hash {
		return true, nil
	}

	return false, ErrMismatch
}
