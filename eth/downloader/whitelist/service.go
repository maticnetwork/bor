package whitelist

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

var (
	ErrMismatch = errors.New("mismatch error")
	ErrNoRemote = errors.New("remote peer doesn't have a target block number")

	ErrCheckpointMismatch = errors.New("checkpoint mismatch")
	ErrLongFutureChain    = errors.New("received future chain of unacceptable length")
	ErrNoRemoteCheckpoint = errors.New("remote peer doesn't have a checkpoint")
)

var (
	// DefaultMaxForkCorrectnessLimit defines the default max number of blocks to iterate
	// backwards in db for checking fork correctness instead of blindly accepting the chain.
	DefaultMaxForkCorrectnessLimit = uint64(256)
)

func forkValidationInitialCapacity(limit uint64) uint64 {
	return min(limit, 2*DefaultMaxForkCorrectnessLimit)
}

type Service struct {
	db ethdb.Database
	checkpointService
	milestoneService

	disableBlindForkValidation bool   // Flag to disable additional fork validation and accept blind forks without tracing back to last whitelisted entry
	maxForkCorrectnessLimit    uint64 // Maximum number of blocks to iterate backwards for fork correctness check

	lastValidForkBlock    atomic.Uint64
	forkValidationCache   map[common.Hash]bool
	forkValidationCacheMu sync.RWMutex
}

func NewService(db ethdb.Database, disableBlindForkValidation bool, maxBlindForkValidationLimit uint64) *Service {
	if maxBlindForkValidationLimit == 0 {
		maxBlindForkValidationLimit = DefaultMaxForkCorrectnessLimit
		log.Info("Invalid max blind fork validation limit, falling back to default", "limit", DefaultMaxForkCorrectnessLimit)
	}

	// Avoid allocating large map if the limit set by user is too high (allow 2x of the default limit)
	var forkValidationCacheSize = maxBlindForkValidationLimit
	if forkValidationCacheSize > 2*DefaultMaxForkCorrectnessLimit {
		forkValidationCacheSize = 2 * DefaultMaxForkCorrectnessLimit
	}

	if disableBlindForkValidation {
		forkValidationCacheSize = 0
		log.Info("Disabling blind fork validation")
	}

	// Fetch last whitelisted checkpoint entry from db. Ignore in case of error or if
	// the whitelisted entry has empty hash.
	var checkpointDoExist = true
	checkpointNumber, checkpointHash, err := rawdb.ReadFinality[*rawdb.Checkpoint](db)
	if err != nil || checkpointHash == (common.Hash{}) {
		checkpointDoExist = false
	}

	// Fetch last whitelisted milestone entry from db. Ignore in case of error or if
	// the whitelisted entry has empty hash.
	var milestoneDoExist = true
	milestoneNumber, milestoneHash, err := rawdb.ReadFinality[*rawdb.Milestone](db)
	if err != nil || milestoneHash == (common.Hash{}) {
		milestoneDoExist = false
	}

	locked, lockedMilestoneNumber, lockedMilestoneHash, lockedMilestoneIDs, err := rawdb.ReadLockField(db)
	if err != nil || !locked {
		locked = false
		lockedMilestoneIDs = make(map[string]struct{})
	}

	// Discard the locked state if the stored milestone number is out of the safe range (corrupted data).
	if locked && lockedMilestoneNumber > math.MaxInt64 {
		log.Warn("Discarding invalid locked milestone loaded from DB", "lockedMilestoneNumber", lockedMilestoneNumber)
		locked = false
		lockedMilestoneNumber = 0
		lockedMilestoneHash = common.Hash{}
		lockedMilestoneIDs = make(map[string]struct{})

		if err := rawdb.WriteLockField(db, locked, lockedMilestoneNumber, lockedMilestoneHash, lockedMilestoneIDs); err != nil {
			log.Error("Error clearing invalid lock data from db", "err", err)
		}
	}

	order, list, err := rawdb.ReadFutureMilestoneList(db)
	if err != nil {
		order = make([]uint64, 0)
		list = make(map[uint64]common.Hash)
	}

	return &Service{
		db: db,
		checkpointService: &checkpoint{
			finality[*rawdb.Checkpoint]{
				doExist:  checkpointDoExist,
				Number:   checkpointNumber,
				Hash:     checkpointHash,
				interval: 256,
				db:       db,
				name:     "checkpoint",
			},
		},
		milestoneService: &milestone{
			finality: finality[*rawdb.Milestone]{
				doExist:  milestoneDoExist,
				Number:   milestoneNumber,
				Hash:     milestoneHash,
				interval: 256,
				db:       db,
				name:     "milestone",
			},

			Locked:                locked,
			LockedMilestoneNumber: lockedMilestoneNumber,
			LockedMilestoneHash:   lockedMilestoneHash,
			LockedMilestoneIDs:    lockedMilestoneIDs,
			FutureMilestoneList:   list,
			FutureMilestoneOrder:  order,
			MaxCapacity:           10,
			blockchain:            nil, // Will be set after blockchain creation
		},
		disableBlindForkValidation: disableBlindForkValidation,
		maxForkCorrectnessLimit:    maxBlindForkValidationLimit,
		forkValidationCache:        make(map[common.Hash]bool, forkValidationCacheSize),
	}
}

// SetBlockchain sets the blockchain reference for the milestone service
func (s *Service) SetBlockchain(blockchain ChainReader) {
	if milestone, ok := s.milestoneService.(*milestone); ok {
		milestone.finality.Lock()
		defer milestone.finality.Unlock()
		milestone.blockchain = blockchain
	}
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor checkpoint submitted to mainchain and last milestone voted in the heimdall
func (s *Service) IsValidPeer(fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	checkpointBool, err := s.checkpointService.IsValidPeer(fetchHeadersByNumber)
	if !checkpointBool {
		return checkpointBool, err
	}

	milestoneBool, err := s.milestoneService.IsValidPeer(fetchHeadersByNumber)
	if !milestoneBool {
		return milestoneBool, err
	}

	return true, nil
}

func (s *Service) PurgeWhitelistedCheckpoint() {
	s.checkpointService.Purge()
}

func (s *Service) PurgeWhitelistedMilestone() {
	s.milestoneService.Purge()
}

// PurgeMilestonesAfter drops milestone state above block, clears the fork
// validation cache, and caps lastValidForkBlock at block. checkForkCorrectness
// uses max(milestoneNumber, lastValidForkBlock) as its canonical bound, so a
// stale lastValidForkBlock would blind-accept peer chains past the rewind anchor.
func (s *Service) PurgeMilestonesAfter(block uint64) {
	s.milestoneService.PurgeAfter(block)
	s.forkValidationCacheMu.Lock()
	clear(s.forkValidationCache)
	s.forkValidationCacheMu.Unlock()
	for {
		cur := s.lastValidForkBlock.Load()
		if cur <= block || s.lastValidForkBlock.CompareAndSwap(cur, block) {
			break
		}
	}
}

func (s *Service) GetWhitelistedCheckpoint() (bool, uint64, common.Hash) {
	return s.checkpointService.Get()
}

func (s *Service) GetWhitelistedMilestone() (bool, uint64, common.Hash) {
	return s.milestoneService.Get()
}

func (s *Service) ProcessMilestone(endBlockNum uint64, endBlockHash common.Hash) {
	s.milestoneService.Process(endBlockNum, endBlockHash)
	s.resetForkValidationCache()
}

func (s *Service) ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash) {
	s.checkpointService.Process(endBlockNum, endBlockHash)
	s.resetForkValidationCache()
}

func (s *Service) IsValidChain(currentHeader *types.Header, chain []*types.Header) (bool, error) {
	checkpointBool, err := s.checkpointService.IsValidChain(currentHeader, chain)
	if !checkpointBool {
		return checkpointBool, err
	}

	milestoneBool, err := s.milestoneService.IsValidChain(currentHeader, chain)
	if !milestoneBool {
		return milestoneBool, err
	}

	if s.disableBlindForkValidation {
		return true, nil
	}

	// At last, check for fork correctness
	return s.checkForkCorrectness(chain), nil
}

// checkForkCorrectness checks if the chain being imported belongs to the correct fork
// by iterating backwards until the last whitelisted milestone or checkpoint. This helps
// in cases where the chain being imported doesn't overlap with any whitelisted entry and
// we have to blindly accept it.
func (s *Service) checkForkCorrectness(chain []*types.Header) bool {
	if len(chain) == 0 {
		return true
	}
	firstBlock := chain[0]
	headerNumber := firstBlock.Number.Uint64()
	lastHeaderNumber := chain[len(chain)-1].Number.Uint64()

	// Recent most whitelisted entry
	var (
		number uint64
		hash   common.Hash
	)

	// Fetch the latest whitelisted entry using both checkpoint and milestone
	milestoneExists, milestoneNumber, milestoneHash := s.milestoneService.Get()
	checkpointExists, checkpointNumber, checkpointHash := s.checkpointService.Get()
	if !milestoneExists && !checkpointExists {
		return true
	}
	if milestoneExists {
		number = milestoneNumber
		hash = milestoneHash
	}
	if checkpointExists {
		// Only choose checkpoint if it's more recent than last milestone
		if checkpointNumber > number {
			number = checkpointNumber
			hash = checkpointHash
		}
	}

	var lastKnownValidBlock uint64 = number
	if v := s.lastValidForkBlock.Load(); v > number {
		lastKnownValidBlock = v
	}

	// Blind accept the chain if we've to iterate more than `maxForkCorrectnessLimit` blocks
	if headerNumber > lastKnownValidBlock && headerNumber-lastKnownValidBlock > s.maxForkCorrectnessLimit {
		log.Debug("Skipping fork correctness check as block is too far ahead", "block", headerNumber, "last whitelisted", number)
		return true
	}

	// Track all blocks iterated for caching. Keep the configured traversal limit
	// unchanged, but bound the initial allocation to avoid oversized preallocation.
	blocksCheckedCapacity := forkValidationInitialCapacity(s.maxForkCorrectnessLimit)
	blocksChecked := make([]common.Hash, 0, blocksCheckedCapacity)
	// Cache the incoming chain by default
	for _, header := range chain {
		blocksChecked = append(blocksChecked, header.Hash())
	}

	// Acquire lock over the cache
	s.forkValidationCacheMu.Lock()
	defer s.forkValidationCacheMu.Unlock()

	// Only perform checks if first block being imported is ahead of last whitelisted entry. We can
	// skip checking for fork correctness otherwise as chain would be already validated before.
	if headerNumber > number {
		parentNumber := headerNumber - 1
		parentHash := firstBlock.ParentHash
		for {
			// Check against cache if this block has been already validated
			if s.forkValidationCache[parentHash] {
				// Cache suggests that this fork is already validated. Accept the fork
				// and update the cache.
				s.updateForkValidationCache(blocksChecked)
				s.lastValidForkBlock.Store(lastHeaderNumber)
				return true
			}
			// Fetch the parent block by number and hash
			header := rawdb.ReadHeader(s.db, parentHash, parentNumber)
			if header == nil {
				// This shouldn't happen as the db should have all blocks before the first block
				// Log the issue and accept blindly (falling back to previous behaviour instead of rejecting)
				log.Warn("Missing parent block while checking fork correctness", "number", parentNumber, "hash", parentHash, "import block", headerNumber, "import hash", firstBlock.Hash())
				return true
			}

			// Check if we're at the whitelisted block
			if header.Number.Uint64() == number {
				// Check if hash matches and return
				res := header.Hash() == hash
				if res {
					// If valid, cache the blocks checked already to avoid re-checking
					// them in next import.
					s.updateForkValidationCache(blocksChecked)
					s.lastValidForkBlock.Store(lastHeaderNumber)
				} else {
					log.Info("Rejecting invalid fork after validating against last whitelisted entry", "number", number, "expected", hash, "got", header.Hash())
				}
				return res
			} else {
				blocksChecked = append(blocksChecked, header.Hash())
			}

			// Header not reached yet, move to parent
			parentHash = header.ParentHash
			parentNumber--
		}
	}

	return true
}

// updateForkValidationCache inserts all block hashes which have been
// validated to the cache. Caller must hold the lock over the cache.
func (s *Service) updateForkValidationCache(blocks []common.Hash) {
	for _, hash := range blocks {
		s.forkValidationCache[hash] = true
	}
}

// resetForkValidationCache resets the cache when a new block is whitelisted.
func (s *Service) resetForkValidationCache() {
	s.forkValidationCacheMu.Lock()
	clear(s.forkValidationCache)
	s.forkValidationCacheMu.Unlock()
}

func (s *Service) GetMilestoneIDsList() []string {
	return s.milestoneService.GetMilestoneIDsList()
}

func splitChain(current uint64, chain []*types.Header) ([]*types.Header, []*types.Header) {
	var (
		pastChain   []*types.Header
		futureChain []*types.Header
		first       = chain[0].Number.Uint64()
		last        = chain[len(chain)-1].Number.Uint64()
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

//nolint:unparam
func isValidChain(currentHeader *types.Header, chain []*types.Header, doExist bool, number uint64, hash common.Hash) (bool, error) {
	// Check if we have milestone to validate incoming chain in memory
	if !doExist {
		// We don't have any entry, no additional validation will be possible
		return true, nil
	}

	current := currentHeader.Number.Uint64()

	// Check if imported chain is less than whitelisted number
	if chain[len(chain)-1].Number.Uint64() < number {
		if current >= number { //If current tip of the chain is greater than whitelist number then return false
			return false, nil
		} else {
			return true, nil
		}
	}

	// Split the chain into past and future chain
	pastChain, _ := splitChain(current, chain)

	// Iterate over the chain and validate against the last milestone
	// It will handle all cases when the incoming chain has at least one milestone
	for i := len(pastChain) - 1; i >= 0; i-- {
		if pastChain[i].Number.Uint64() == number {
			res := pastChain[i].Hash() == hash

			return res, nil
		}
	}

	return true, nil
}

// FIXME: remoteHeader is not used
func isValidPeer(fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error), doExist bool, number uint64, hash common.Hash) (bool, error) {
	// Check for availability of the last milestone block.
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
