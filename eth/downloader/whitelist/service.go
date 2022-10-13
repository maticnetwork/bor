package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
