package bor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/metrics"

	ttlcache "github.com/jellydator/ttlcache/v3"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor/api"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	borSpan "github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/statefull"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"
)

const (
	defaultSpanLength  = params.DefaultSpanLength
	zerothSpanEnd      = 255             // End block of 0th span
	checkpointInterval = 1024            // Number of blocks after which to save the vote snapshot to the database
	inmemorySnapshots  = 128             // Number of recent vote snapshots to keep in memory
	inmemorySignatures = 4096            // Number of recent block signatures to keep in memory
	veblopBlockTimeout = time.Second * 8 // Timeout for new span check. DO NOT CHANGE THIS VALUE.
	// minBlockBuildTime is the minimum remaining time before Prepare() extends
	// the block deadline to avoid producing empty blocks. If time.Until(target)
	// is less than this value, the target timestamp is pushed forward by one
	// blockTime period.
	//
	// Abort-recovery rebuilds from pipelined SRC are exempt from this push. By the
	// time speculative execution is discarded, most of the slot may already be
	// gone; moving the header to the next slot would create avoidable 3-second
	// blocks on 2-second devnets.
	minBlockBuildTime = 1 * time.Second
)

// Bor protocol constants.
var (
	defaultSprintLength = map[string]uint64{
		"0": 64,
	} // Default number of blocks after which to checkpoint and reset the pending votes

	uncleHash = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.

	validatorHeaderBytesLength = common.AddressLength + 20 // address + power
)

// belowMinBuildTimeCounter increments when a block's remaining build budget fell below minBlockBuildTime
// and we pushed the header time forward to avoid empty blocks.
var belowMinBuildTimeCounter = metrics.NewRegisteredCounter("bor/prepare/header_time_pushed", nil)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte signature suffix missing")

	// errNonCanonicalSeal is returned if a block's seal signature is invalid or
	// not in the canonical low-S encoding.
	errNonCanonicalSeal = errors.New("invalid or non-canonical seal signature")

	// errExtraValidators is returned if non-sprint-end block contain validator data in
	// their extra-data fields.
	errExtraValidators = errors.New("non-sprint-end block contains extra validator list")

	// errInvalidSpanValidators is returned if a block contains an
	// invalid list of validators (i.e. non divisible by 40 bytes).
	errInvalidSpanValidators = errors.New("invalid validator list on sprint end block")

	// errMissingGiuglianoFields is returned if a post-Giugliano block is missing
	// the gas target or base fee change denominator in its extra data.
	errMissingGiuglianoFields = errors.New("missing gas target or base fee change denominator in extra data")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errInvalidDifficulty is returned if the difficulty of a block neither 1 or 2.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	// errOutOfRangeChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errOutOfRangeChain = errors.New("out of range or non-contiguous chain")

	errUncleDetected     = errors.New("uncles not allowed")
	errUnknownValidators = errors.New("unknown validators")

	// errReorgDuringRootComputation indicates a reorganization occurred while calculating the checkpoint root.
	errReorgDuringRootComputation = errors.New("reorg occurred while computing checkpoint root")

	// errNonContiguousHeaderRange is returned when the header range [start,end]
	// is not contiguous in terms of parent-child relationships.
	errNonContiguousHeaderRange = errors.New("non-contiguous headers in checkpoint range")
)

// maxAllowedFutureBlockTimeSeconds is the maximum number of seconds that a block
// timestamp may exceed the local clock.
const maxAllowedFutureBlockTimeSeconds = uint64(30)

// SignerFn is a signer callback function to request a header to be signed by a
// backing account.
type SignerFn func(accounts.Account, string, []byte) ([]byte, error)

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache, c *params.BorConfig) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < types.ExtraSealLength {
		return common.Address{}, errMissingSignature
	}

	signature := header.Extra[len(header.Extra)-types.ExtraSealLength:]

	// Enforce the canonical low-S signature encoding before recovery. crypto.Sign
	// always produces low-S seals, so this never rejects a seal a validator
	// legitimately produced.
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:64])
	if !crypto.ValidateSignatureValues(signature[64], r, s, true) {
		return common.Address{}, errNonCanonicalSeal
	}

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(SealHash(header, c).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}

	var signer common.Address

	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)

	return signer, nil
}

// SealHash returns the hash of a block prior to it being sealed.
func SealHash(header *types.Header, c *params.BorConfig) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeSigHeader(hasher, header, c)
	hasher.Sum(hash[:0])

	return hash
}

func encodeSigHeader(w io.Writer, header *types.Header, c *params.BorConfig) {
	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-65], // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
	}

	if c.IsJaipur(header.Number) {
		if header.BaseFee != nil {
			enc = append(enc, header.BaseFee)
		}
	}

	if err := rlp.Encode(w, enc); err != nil {
		panic("can't encode: " + err.Error())
	}
}

// CalcProducerDelay is the block delay algorithm based on block time, period, producerDelay and turn-ness of a signer
func CalcProducerDelay(number uint64, succession int, c *params.BorConfig) uint64 {
	// When the block is the first block of the sprint, it is expected to be delayed by `producerDelay`.
	// That is to allow time for block propagation in the last sprint
	delay := c.CalculatePeriod(number)

	// Since there is only one producer in veblop, we don't need to add producer delay and backup multiplier
	if c.IsRio(big.NewInt(int64(number))) {
		return delay
	}

	if number%c.CalculateSprint(number) == 0 {
		delay = c.CalculateProducerDelay(number)
	}

	if succession > 0 {
		delay += uint64(succession) * c.CalculateBackupMultiplier(number)
	}

	return delay
}

// BorRLP returns the rlp bytes which needs to be signed for the bor
// sealing. The RLP to sign consists of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func BorRLP(header *types.Header, c *params.BorConfig) []byte {
	b := new(bytes.Buffer)
	encodeSigHeader(b, header, c)

	return b.Bytes()
}

// Bor is the matic-bor consensus engine
type Bor struct {
	chainConfig *params.ChainConfig // Chain config
	config      *params.BorConfig   // Consensus engine configuration parameters for bor consensus
	vmConfig    vm.Config           // VM config (optional) for system transactions
	db          ethdb.Database      // Database to store and retrieve snapshot checkpoints

	recents               *ttlcache.Cache[common.Hash, *Snapshot]     // Snapshots for recent block to speed up reorgs
	recentVerifiedHeaders *ttlcache.Cache[common.Hash, *types.Header] // Headers for recent blocks to speed up reorgs
	signatures            *lru.ARCCache                               // Signatures of recent blocks to speed up mining

	authorizedSigner atomic.Pointer[signer] // Ethereum address and sign function of the signing key

	ethAPI                 api.Caller
	spanner                Spanner
	GenesisContractsClient GenesisContract
	HeimdallClient         IHeimdallClient
	HeimdallWSClient       IHeimdallWSClient

	spanStore            *SpanStore // Store to save previous span data from heimdall
	latestMilestoneBlock atomic.Uint64

	// The fields below are for testing only
	fakeDiff      bool // Skip difficulty verifications
	DevFakeAuthor bool

	// The block time defined by the miner. Needs to be larger or equal to the consensus block time. If not set (default = 0), the miner will use the consensus block time.
	blockTime time.Duration

	// Cache to store the actual times of the parent blocks
	parentActualTimeCache *lru.Cache

	quit      chan struct{}
	closeOnce sync.Once

	// ctx is cancelled when Close() is called, allowing in-flight operations to abort promptly.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// api is the bor engine API instance reused across all callers (JSON-RPC and gRPC).
	api     *API
	apiOnce sync.Once
}

type signer struct {
	signer common.Address // Ethereum address of the signing key
	signFn SignerFn       // Signer function to authorize hashes with
}

// New creates a Matic Bor consensus engine.
func New(
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	ethAPI api.Caller,
	spanner Spanner,
	heimdallClient IHeimdallClient,
	heimdallWSClient IHeimdallWSClient,
	genesisContracts GenesisContract,
	devFakeAuthor bool,
	blockTime time.Duration,
	vmConfig vm.Config,
) *Bor {
	// get bor config
	borConfig := chainConfig.Bor

	// Set any missing consensus parameters to their defaults
	if borConfig != nil && borConfig.CalculateSprint(0) == 0 {
		borConfig.Sprint = defaultSprintLength
	}
	// Allocate the snapshot caches and create the engine
	recents := ttlcache.New[common.Hash, *Snapshot](
		ttlcache.WithTTL[common.Hash, *Snapshot](veblopBlockTimeout),
		ttlcache.WithCapacity[common.Hash, *Snapshot](inmemorySnapshots),
		ttlcache.WithDisableTouchOnHit[common.Hash, *Snapshot](),
	)

	signatures, _ := lru.NewARC(inmemorySignatures)

	recentVerifiedHeaders := ttlcache.New[common.Hash, *types.Header](
		ttlcache.WithTTL[common.Hash, *types.Header](veblopBlockTimeout),
		ttlcache.WithCapacity[common.Hash, *types.Header](inmemorySignatures),
		ttlcache.WithDisableTouchOnHit[common.Hash, *types.Header](),
	)

	// Create a new span store
	spanStore := NewSpanStore(heimdallClient, spanner, chainConfig.ChainID.String())

	ctx, ctxCancel := context.WithCancel(context.Background())

	c := &Bor{
		chainConfig:            chainConfig,
		config:                 borConfig,
		vmConfig:               vmConfig,
		db:                     db,
		ethAPI:                 ethAPI,
		recents:                recents,
		recentVerifiedHeaders:  recentVerifiedHeaders,
		signatures:             signatures,
		spanner:                spanner,
		GenesisContractsClient: genesisContracts,
		HeimdallClient:         heimdallClient,
		HeimdallWSClient:       heimdallWSClient,
		spanStore:              spanStore,
		DevFakeAuthor:          devFakeAuthor,
		blockTime:              blockTime,
		quit:                   make(chan struct{}),
		ctx:                    ctx,
		ctxCancel:              ctxCancel,
	}

	c.authorizedSigner.Store(&signer{
		common.Address{},
		func(_ accounts.Account, _ string, i []byte) ([]byte, error) {
			// return an error to prevent panics
			return nil, &UnauthorizedSignerError{0, common.Address{}.Bytes(), []*valset.Validator{}}
		},
	})

	c.parentActualTimeCache, _ = lru.New(10)

	// make sure we can decode all the GenesisAlloc in the BorConfig.
	for key, genesisAlloc := range c.config.BlockAlloc {
		if _, err := decodeGenesisAlloc(genesisAlloc); err != nil {
			panic(fmt.Sprintf("BUG: Block alloc '%s' in genesis is not correct: %v", key, err))
		}
	}

	go c.runMilestoneFetcher()

	return c
}

// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (c *Bor) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, c.signatures, c.config)
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (c *Bor) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return c.verifyHeader(chain, header, nil)
}

func (c *Bor) GetSpanner() Spanner {
	return c.spanner
}

func (c *Bor) SetSpanner(spanner Spanner) {
	c.spanner = spanner
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (c *Bor) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := c.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()

	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (c *Bor) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}

	number := header.Number.Uint64()
	now := uint64(time.Now().Unix())

	if c.config.IsGiugliano(header.Number) {
		// Rio introduced flexible blocktime (can be set larger than consensus without approval).
		// Using strict CalcProducerDelay for early block announcement (introduced back in Giugliano)
		// would reject valid blocks, so we just ensure announcement time comes after parent time to
		// allow for flexible blocktime.
		var parent *types.Header

		if len(parents) > 0 {
			parent = parents[len(parents)-1]
		} else {
			parent = chain.GetHeader(header.ParentHash, number-1)
		}
		if parent == nil || now < parent.Time {
			log.Error("Block announced too early post giugliano", "number", number, "headerTime", header.Time, "now", now)
			return consensus.ErrFutureBlock
		}
		// Upper-bound check: a block whose timestamp is more than maxAllowedFutureBlockTimeSeconds
		// ahead of the local clock is rejected.
		if header.Time > now+maxAllowedFutureBlockTimeSeconds {
			log.Error("Block timestamp too far in future post giugliano", "number", number, "headerTime", header.Time, "now", now)
			return consensus.ErrFutureBlock
		}
	} else if c.config.IsBhilai(header.Number) {
		// TODO: Once Amoy and Mainnet supports Giugliano HF, we are safe to remove this check (since it only works for block future blocks)
		// Don't waste time checking blocks from the future but allow a buffer of block time for
		// early block announcements. Note that this is a loose check and would allow early blocks
		// from non-primary producer. Such blocks will be rejected later when we know the succession
		// number of the signer in the current sprint.
		// Uses CalcProducerDelay instead of block period to account for producer delay on sprint start blocks.
		// We assume succession 0 (primary producer) to not be much restrictive for early block announcements.
		if header.Time-CalcProducerDelay(number, 0, c.config) > now {
			log.Error("Block announced too early post bhilai", "number", number, "headerTime", header.Time, "now", now)
			return consensus.ErrFutureBlock
		}
	} else {
		// Don't waste time checking blocks from the future
		if header.Time > now {
			log.Error("Block announced too early", "number", number, "headerTime", header.Time, "now", now)
			return consensus.ErrFutureBlock
		}
	}

	if err := validateHeaderExtraField(header.Extra); err != nil {
		return err
	}

	// Check extra data.
	isSprintEnd := IsSprintStart(number+1, c.config.CalculateSprint(number))

	// Decode validator bytes and base-fee params.
	validatorBytes, gasTarget, bfcd := header.GetValidatorBytesAndBaseFeeParams(c.chainConfig)

	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise.
	signersBytes := len(validatorBytes)

	if !isSprintEnd && signersBytes != 0 {
		return errExtraValidators
	}

	if isSprintEnd && signersBytes%validatorHeaderBytesLength != 0 {
		log.Warn("Invalid validator set", "number", number, "signersBytes", signersBytes)
		return errInvalidSpanValidators
	}

	// Post-Giugliano: verify that gas target and base fee change denominator are present.
	// We only check presence, not correctness, because post-Lisovo these parameters are
	// configurable per-node via CLI flags. Validating values would cause nodes with
	// different configurations to reject each other's blocks. The actual base fee
	// calculation in CalcBaseFee uses its own computation and does not read these fields.
	if c.config.IsGiugliano(header.Number) {
		if gasTarget == nil || bfcd == nil {
			return errMissingGiuglianoFields
		}
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}

	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	if number > 0 {
		if header.Difficulty == nil {
			return errInvalidDifficulty
		}
	}

	// Verify that the gas limit is <= 2^63-1
	gasCap := uint64(0x7fffffffffffffff)
	if header.GasLimit > gasCap {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, gasCap)
	}

	if header.WithdrawalsHash != nil {
		return consensus.ErrUnexpectedWithdrawals
	}

	if header.RequestsHash != nil {
		return consensus.ErrUnexpectedRequests
	}

	// All basic checks passed, verify cascading fields
	err := c.verifyCascadingFields(chain, header, parents)
	if err != nil {
		return err
	}

	// Calculate TTL for the header cache entry
	// If the header time is in the future (early announced block), add extra time to TTL
	cacheTTL := veblopBlockTimeout
	nowTime := time.Now()
	headerTime := time.Unix(int64(header.Time), 0)
	if headerTime.After(nowTime) && c.config.IsGiugliano(header.Number) {
		// Add the time from now until header time as extra to the base timeout
		extraTime := headerTime.Sub(nowTime)
		cacheTTL = veblopBlockTimeout + extraTime
	}

	c.recentVerifiedHeaders.Set(header.Hash(), header, cacheTTL)
	return nil
}

// validateHeaderExtraField validates that the extra-data contains both the vanity and signature.
// header.Extra = header.Vanity + header.ProducerBytes (optional) + header.Seal
func validateHeaderExtraField(extraBytes []byte) error {
	if len(extraBytes) < types.ExtraVanityLength {
		return errMissingVanity
	}

	if len(extraBytes) < types.ExtraVanityLength+types.ExtraSealLength {
		return errMissingSignature
	}

	return nil
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
func (c *Bor) verifyCascadingFields(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()

	if number == 0 {
		return nil
	}

	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header

	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}

	if parent == nil || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}

	// Verify block number continuity
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}

	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}

	if !chain.Config().IsLondon(header.Number) {
		// Verify BaseFee not present before EIP-1559 fork.
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, want <nil>", header.BaseFee)
		}

		if err := misc.VerifyGaslimit(parent.GasLimit, header.GasLimit); err != nil {
			return err
		}
	} else if err := eip1559.VerifyEIP1559Header(chain.Config(), parent, header); err != nil {
		// Verify the header's EIP-1559 attributes.
		return err
	}

	if parent.Time+c.config.CalculatePeriod(number) > header.Time {
		return ErrInvalidTimestamp
	}

	if !c.config.IsRio(header.Number) && !slices.Contains(c.config.SkipValidatorByteCheck, number) {
		// Retrieve the snapshot needed to verify this header and cache it
		snap, err := c.snapshot(chain, header, parents, false)
		if err != nil {
			return err
		}

		// Verify if the producer set in header's extra data matches with the list in span.
		// We skip the check for 0th span as the producer set in contract v/s producer set
		// in heimdall span is different which will lead a mismatch. Moreover, to make the
		// validation stateless, we use the span from heimdall (via span store) instead of
		// span from validator set genesis contract as both are supposed to be equivalent.
		if number > zerothSpanEnd && IsSprintStart(number+1, c.config.CalculateSprint(number)) {
			span, err := c.spanStore.spanByBlockNumber(c.ctx, number+1)
			if err != nil {
				return err
			}

			// Use producer set from span as it's equivalent to the data we get from genesis contract
			selectedProducers := borSpan.ConvertHeimdallValidatorsToBorValidators(span.SelectedProducers)
			newValidators := make([]*valset.Validator, len(selectedProducers))
			for i, val := range selectedProducers {
				newValidators[i] = &val
			}
			sort.Sort(valset.ValidatorsByAddress(newValidators))

			headerVals, err := valset.ParseValidators(header.GetValidatorBytes(c.chainConfig))
			if err != nil {
				return err
			}

			if len(newValidators) != len(headerVals) {
				log.Warn("Invalid validator set", "block number", number, "newValidators", newValidators, "headerVals", headerVals)
				return errInvalidSpanValidators
			}

			for i, val := range newValidators {
				if !bytes.Equal(val.HeaderBytes(), headerVals[i].HeaderBytes()) {
					log.Warn("Invalid validator set", "block number", number, "index", i, "local validator", val, "header validator", headerVals[i])
					return errInvalidSpanValidators
				}
			}
		}

		// verify the validator list in the last sprint block
		if IsSprintStart(number, c.config.CalculateSprint(number)) && !slices.Contains(c.config.SkipValidatorByteCheck, number-1) {
			parentValidatorBytes := parent.GetValidatorBytes(c.chainConfig)
			validatorsBytes := make([]byte, len(snap.ValidatorSet.Validators)*validatorHeaderBytesLength)

			currentValidators := snap.ValidatorSet.Copy().Validators
			// sort validator by address
			sort.Sort(valset.ValidatorsByAddress(currentValidators))

			for i, validator := range currentValidators {
				copy(validatorsBytes[i*validatorHeaderBytesLength:], validator.HeaderBytes())
			}
			// len(header.Extra) >= extraVanity+extraSeal has already been validated in validateHeaderExtraField, so this won't result in a panic
			if !bytes.Equal(parentValidatorBytes, validatorsBytes) {
				return &MismatchingValidatorsError{number - 1, validatorsBytes, parentValidatorBytes}
			}
		}
	}

	// All basic checks passed, verify the seal and return
	return c.verifySeal(chain, header, parents)
}

// snapshot retrieves the authorization snapshot at a given point in time.
// nolint:gocognit
func (c *Bor) snapshot(chain consensus.ChainHeaderReader, targetHeader *types.Header, parents []*types.Header, checkNewSpan bool) (snap *Snapshot, err error) {
	// Search for a snapshot in memory or on disk for checkpoints
	signer := common.BytesToAddress(c.authorizedSigner.Load().signer.Bytes())
	if c.DevFakeAuthor && signer.String() != "0x0000000000000000000000000000000000000000" {
		log.Info("👨‍💻Using DevFakeAuthor", "signer", signer)

		val := valset.NewValidator(signer, 1000)
		validatorset := valset.NewValidatorSet([]*valset.Validator{val})

		snapshot := newSnapshot(c.chainConfig, c.signatures, targetHeader.Number.Uint64(), targetHeader.Hash(), validatorset.Validators)

		return snapshot, nil
	}

	if c.config.IsRio(targetHeader.Number) {
		return c.getVeBlopSnapshot(chain, targetHeader, parents, checkNewSpan)
	}

	headers := make([]*types.Header, 0, 16)

	hash := targetHeader.ParentHash
	number := targetHeader.Number.Uint64() - 1

	//nolint:govet
	for snap == nil {
		// If an in-memory snapshot was found, use that
		if v := c.recents.Get(hash); v != nil {
			snap = v.Value()
			break
		}

		// If an on-disk checkpoint snapshot can be found, use that
		if number%checkpointInterval == 0 {
			if s, err := loadSnapshot(c.chainConfig, c.config, c.signatures, c.db, hash); err == nil {
				log.Trace("Loaded snapshot from disk", "number", number, "hash", hash)

				snap = s

				break
			}
		}

		// If we're at the genesis, snapshot the initial state. Alternatively if we're
		// at a checkpoint block without a parent (light client CHT), or we have piled
		// up more headers than allowed to be reorged (chain reinit from a freezer),
		// consider the checkpoint trusted and snapshot it.
		// nolint:nestif
		if number == 0 {
			checkpoint := chain.GetHeaderByNumber(number)
			if checkpoint != nil {
				// get checkpoint data
				hash := checkpoint.Hash()

				// get validators from span
				span, err := c.spanStore.spanByBlockNumber(c.ctx, number+1)
				if err != nil {
					return nil, err
				}

				// new snap shot
				borValSet := borSpan.ConvertHeimdallValSetToBorValSet(span.ValidatorSet)
				snap = newSnapshot(c.chainConfig, c.signatures, number, hash, borValSet.Validators)
				if err := snap.store(c.db); err != nil {
					return nil, err
				}

				log.Info("Stored checkpoint snapshot to disk", "number", number, "hash", hash)

				break
			}
		}

		// No snapshot for this header, gather the header and move backward
		var header *types.Header
		if len(parents) > 0 {
			// If we have explicit parents, pick from there (enforced)
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}

			parents = parents[:len(parents)-1]
		} else {
			// No explicit parents (or no more left), reach out to the database
			header = chain.GetHeader(hash, number)
			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}

		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}

	// check if snapshot is nil
	if snap == nil {
		return nil, fmt.Errorf("unknown error while retrieving snapshot at block number %v", number)
	}

	// Previous snapshot found, apply any pending headers on top of it
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}

	snap, err = snap.apply(headers, c)
	if err != nil {
		return nil, err
	}

	c.recents.Set(snap.Hash, snap, ttlcache.DefaultTTL)

	// If we've generated a new checkpoint snapshot, save to disk
	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err = snap.store(c.db); err != nil {
			return nil, err
		}

		log.Trace("Stored snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}

	return snap, err
}

func (c *Bor) getVeBlopSnapshot(chain consensus.ChainHeaderReader, targetHeader *types.Header, parents []*types.Header, checkNewSpan bool) (*Snapshot, error) {
	number := targetHeader.Number.Uint64()

	if checkNewSpan {
		err := c.performSpanCheck(chain, targetHeader, parents)
		if err != nil {
			return nil, err
		}
	}

	span, err := c.spanStore.spanByBlockNumber(c.ctx, number)
	if err != nil {
		return nil, err
	}

	producers := make([]*valset.Validator, len(span.SelectedProducers))
	for i, validator := range span.SelectedProducers {
		producers[i] = &valset.Validator{
			Address:     common.HexToAddress(validator.Signer),
			VotingPower: validator.VotingPower,
		}
	}

	sortedProducers := valset.ValidatorsByAddress(producers)
	sort.Sort(sortedProducers)

	snap := newSnapshot(c.chainConfig, c.signatures, number, targetHeader.Hash(), sortedProducers)

	c.recents.Set(snap.Hash, snap, ttlcache.DefaultTTL)
	return snap, nil
}

// Check if the node needs to wait for a new span
func (c *Bor) performSpanCheck(chain consensus.ChainHeaderReader, targetHeader *types.Header, parents []*types.Header) error {
	if targetHeader.Number.Uint64()-1 == 0 {
		return nil
	}

	if c.latestMilestoneBlock.Load() >= targetHeader.Number.Uint64() {
		return nil
	}

	targetHeaderAuthor, missingTargetSignature := c.Author(targetHeader)
	if missingTargetSignature != nil {
		return nil
	}

	var parentHeader *types.Header
	if len(parents) > 0 {
		parentHeader = parents[len(parents)-1]
	} else {
		parentHeader = chain.GetHeader(targetHeader.ParentHash, targetHeader.Number.Uint64()-1)
	}

	// If parent header isn't known or a milestone check is requested, we need to wait for a new span
	if parentHeader == nil {
		log.Info("Parent header is nil", "targetHeader", targetHeader.Number.Uint64(), "parentHeaderHash", targetHeader.ParentHash)
		log.Info("Waiting for new span", "target block", targetHeader.Number.Uint64(), "targetHeaderAuthor", targetHeaderAuthor)
		_, err := c.spanStore.waitForNewSpan(targetHeader.Number.Uint64(), targetHeaderAuthor, veblopBlockTimeout*2)
		if err != nil {
			log.Warn("Error while waiting for new span", "error", err)
			return err
		}

		return nil
	}

	parentHeaderAuthor, missingParentSignature := c.Author(parentHeader)
	if missingParentSignature != nil {
		return missingParentSignature
	}

	if !c.recentVerifiedHeaders.Has(targetHeader.ParentHash) && targetHeaderAuthor == parentHeaderAuthor {
		log.Info("Starting span check due to longer than expected block time", "target block", targetHeader.Number.Uint64(), "parentHeader", targetHeader.ParentHash, "parentHeaderAuthor", parentHeaderAuthor, "targetHeaderAuthor", targetHeaderAuthor)

		// Keep getting the latest span until we get a new span or a new milestone
		foundNewSpan, err := c.spanStore.waitForNewSpan(targetHeader.Number.Uint64(), targetHeaderAuthor, veblopBlockTimeout*2)
		if err != nil {
			log.Warn("Error while waiting for new span", "error", err)
			return err
		}

		log.Info("Span check complete", "foundNewSpan", foundNewSpan)

		return nil
	}
	return nil
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (c *Bor) VerifyUncles(_ consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errUncleDetected
	}

	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (c *Bor) VerifySeal(chain consensus.ChainHeaderReader, header *types.Header) error {
	return c.verifySeal(chain, header, nil)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (c *Bor) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(chain, header, parents, true)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against signers
	signer, err := ecrecover(header, c.signatures, c.config)
	if err != nil {
		return err
	}

	if !snap.ValidatorSet.HasAddress(signer) && !snap.isAllowedByValidatorSetOverride(signer, header.Number.Uint64()) {
		// Check the UnauthorizedSignerError.Error() msg to see why we pass number-1
		return &UnauthorizedSignerError{number, signer.Bytes(), snap.ValidatorSet.Validators}
	}

	succession, err := snap.GetSignerSuccessionNumber(signer)
	if err != nil {
		return err
	}

	var parent *types.Header
	if len(parents) > 0 { // if parents is nil, len(parents) is zero
		parent = parents[len(parents)-1]
	} else if number > 0 {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}

	// Post Bhilai HF, reject blocks form non-primary producers if they're earlier than the expected time
	if c.config.IsBhilai(header.Number) && succession != 0 {
		now := uint64(time.Now().Unix())
		if header.Time > now {
			log.Error("Block announced too early by non-primary producer post bhilai", "number", number, "headerTime", header.Time, "now", now)
			return consensus.ErrFutureBlock
		}
	}

	if IsBlockEarly(parent, header, number, succession, c.config) {
		return &BlockTooSoonError{number, succession}
	}

	// Ensure that the difficulty corresponds to the turn-ness of the signer
	if !c.fakeDiff {
		expected := Difficulty(snap.ValidatorSet, signer)
		// range check: difficulty must fit in uint64 (no high bits allowed).
		if header.Difficulty == nil || !header.Difficulty.IsUint64() {
			// reject the block.
			return &WrongDifficultyError{
				Number:   header.Number.Uint64(),
				Expected: expected,
				Actual:   math.MaxUint64, // invalid sentinel
				Signer:   signer.Bytes(),
			}
		}

		// value check, now it's safe to use Uint64().
		actual := header.Difficulty.Uint64()
		if actual != expected {
			return &WrongDifficultyError{
				Number:   header.Number.Uint64(),
				Expected: expected,
				Actual:   actual,
				Signer:   signer.Bytes(),
			}
		}
	}

	return nil
}

// IsBlockEarly returns true if the header time is earlier than expected (according to consensus rules). This
// can happen if the producer maliciously updates the header time.
func IsBlockEarly(parent *types.Header, header *types.Header, number uint64, succession int, cfg *params.BorConfig) bool {
	return parent != nil && header.Time < parent.Time+CalcProducerDelay(number, succession, cfg)
}

// giuglianoExtraFields returns the post-Giugliano EIP-1559 gas target and
// base fee change denominator computed from parent, or (nil, nil) pre-Giugliano.
func (c *Bor) giuglianoExtraFields(header *types.Header, parent *types.Header) (gasTarget *uint64, baseFeeChangeDenom *uint64) {
	if !c.config.IsGiugliano(header.Number) {
		return nil, nil
	}

	gt := eip1559.CalcGasTarget(c.chainConfig, parent)
	bfcd := params.BaseFeeChangeDenominator(c.config, parent.Number)

	return &gt, &bfcd
}

func (c *Bor) parentActualTime(parent *types.Header, parentHash common.Hash) time.Time {
	parentBlockTime := time.Unix(int64(parent.Time), 0)
	parentActualBlockTime := parentBlockTime
	if c.parentActualTimeCache != nil {
		if v, ok := c.parentActualTimeCache.Get(parentHash); ok {
			if at, ok := v.(time.Time); ok && at.After(parentBlockTime) {
				parentActualBlockTime = at
			}
		}
	}
	return parentActualBlockTime
}

// EarliestAnnounceTime returns the earliest local time at which a prepared
// block can be announced without violating Bor's post-Giugliano future-block
// checks. Primary producers may announce before the block's own timestamp, but
// not before the parent slot boundary.
func (c *Bor) EarliestAnnounceTime(chain consensus.ChainHeaderReader, header *types.Header) time.Time {
	if header == nil || header.Number == nil || header.Number.Sign() == 0 {
		return time.Now()
	}
	if !c.config.IsGiugliano(header.Number) {
		return header.GetActualTime()
	}
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return header.GetActualTime()
	}
	return c.parentActualTime(parent, header.ParentHash)
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (c *Bor) Prepare(chain consensus.ChainHeaderReader, header *types.Header, waitOnPrepare bool) error {
	// If the block isn't a checkpoint, cast a random vote (good enough for now)
	header.Coinbase = common.Address{}
	header.Nonce = types.BlockNonce{}

	number := header.Number.Uint64()
	// Assemble the validator snapshot to check which votes make sense
	snap, err := c.snapshot(chain, header, nil, false)
	if err != nil {
		return err
	}

	currentSigner := *c.authorizedSigner.Load()

	// Set the correct difficulty
	header.Difficulty = new(big.Int).SetUint64(Difficulty(snap.ValidatorSet, currentSigner.signer))

	// Ensure the extra data has all it's components
	if len(header.Extra) < types.ExtraVanityLength {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, types.ExtraVanityLength-len(header.Extra))...)
	}

	header.Extra = header.Extra[:types.ExtraVanityLength]

	// Fetch parent early — needed for Giugliano extra fields and timestamp calculation
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	// get validator set if number
	if IsSprintStart(number+1, c.config.CalculateSprint(number)) && !c.config.IsRio(header.Number) {
		newValidators, err := c.spanner.GetCurrentValidatorsByHash(context.Background(), header.ParentHash, number+1)
		if err != nil {
			return errUnknownValidators
		}

		// sort validator by address
		sort.Sort(valset.ValidatorsByAddress(newValidators))

		if c.chainConfig.IsCancun(header.Number) {
			var tempValidatorBytes []byte

			for _, validator := range newValidators {
				tempValidatorBytes = append(tempValidatorBytes, validator.HeaderBytes()...)
			}

			gasTarget, baseFeeChangeDenom := c.giuglianoExtraFields(header, parent)

			blockExtraDataBytes, err := types.EncodeBlockExtraData(c.chainConfig, header.Number, tempValidatorBytes, gasTarget, baseFeeChangeDenom)
			if err != nil {
				log.Error("error while encoding block extra data", "err", err)
				return fmt.Errorf("error while encoding block extra data: %v", err)
			}

			header.Extra = append(header.Extra, blockExtraDataBytes...)
		} else {
			for _, validator := range newValidators {
				header.Extra = append(header.Extra, validator.HeaderBytes()...)
			}
		}
	} else if c.chainConfig.IsCancun(header.Number) {
		gasTarget, baseFeeChangeDenom := c.giuglianoExtraFields(header, parent)

		blockExtraDataBytes, err := types.EncodeBlockExtraData(c.chainConfig, header.Number, nil, gasTarget, baseFeeChangeDenom)
		if err != nil {
			log.Error("error while encoding block extra data", "err", err)
			return fmt.Errorf("error while encoding block extra data: %v", err)
		}

		header.Extra = append(header.Extra, blockExtraDataBytes...)
	}

	// add extra seal space
	header.Extra = append(header.Extra, make([]byte, types.ExtraSealLength)...)

	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}

	// Ensure the timestamp has the correct delay
	var succession int
	// if signer is not empty
	if currentSigner.signer != (common.Address{}) {
		succession, err = snap.GetSignerSuccessionNumber(currentSigner.signer)
		if err != nil {
			// If the signer is not in the active validator set, use succession 0
			// so that the pending block header is still valid for RPC queries.
			// Seal() will independently reject the block if unauthorized.
			succession = 0
		}
	}

	if c.blockTime > 0 && uint64(c.blockTime.Seconds()) < c.config.CalculatePeriod(number) {
		return fmt.Errorf("the floor of custom mining block time (%v) is less than the consensus block time: %v < %v", c.blockTime, c.blockTime.Seconds(), c.config.CalculatePeriod(number))
	}

	var delay time.Duration

	if c.blockTime > 0 && c.config.IsRio(header.Number) {
		// Only enable custom block time for Rio and later

		parentActualBlockTime := c.parentActualTime(parent, header.ParentHash)
		actualNewBlockTime := parentActualBlockTime.Add(c.blockTime)
		header.Time = uint64(actualNewBlockTime.Unix())
		header.ActualTime = actualNewBlockTime
		delay = time.Until(parentActualBlockTime)
	} else {
		header.Time = parent.Time + CalcProducerDelay(number, succession, c.config)
		delay = time.Until(time.Unix(int64(parent.Time), 0))
	}

	now := time.Now()
	blockTime := time.Duration(c.config.CalculatePeriod(number)) * time.Second
	if c.blockTime > 0 && c.config.IsRio(header.Number) {
		blockTime = c.blockTime
	}
	// Ensure minimum build time so the block has enough time to include transactions.
	// The interrupt timer reserves 500ms for state root computation, so without
	// sufficient remaining time the block would end up empty.
	//
	// Abort-recovery rebuilds are different: speculative execution has already
	// spent most of the slot, so pushing them again would create an avoidable
	// extra block-time gap. Those late rebuilds should keep their original slot.
	if !header.AbortRecovery && time.Until(header.GetActualTime()) < minBlockBuildTime {
		header.Time = uint64(now.Add(blockTime).Unix())
		belowMinBuildTimeCounter.Inc(1)
		if c.blockTime > 0 && c.config.IsRio(header.Number) {
			header.ActualTime = now.Add(blockTime)
		}
	}

	// Giugliano introduced early block announcements: primary producers wait
	// until the parent slot boundary before building, then Seal can return
	// immediately and announce the block before its own timestamp. Speculative
	// and prefetch callers pass waitOnPrepare=false because they intentionally
	// build ahead and perform their own parent-boundary wait before sealing.
	if c.config.IsGiugliano(header.Number) && waitOnPrepare {
		if currentSigner.signer != (common.Address{}) {
			// Avoid allocating a timer when the parent boundary has already
			// passed. This is equivalent to develop's immediate time.After path
			// for non-positive delays, just cheaper and more explicit.
			if succession == 0 && delay > 0 {
				<-time.After(delay)
			}
		}
	}

	return nil
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given.
func (c *Bor) Finalize(chain consensus.ChainHeaderReader, header *types.Header, wrappedState vm.StateDB, body *types.Body, receipts []*types.Receipt) ([]*types.Receipt, error) {
	// Reject the block if it has withdrawals or requests
	if body.Withdrawals != nil || header.WithdrawalsHash != nil {
		return nil, consensus.ErrUnexpectedWithdrawals
	}
	if header.RequestsHash != nil {
		return nil, consensus.ErrUnexpectedRequests
	}

	var (
		headerNumber  = header.Number.Uint64()
		stateSyncData []*types.StateSyncData
		err           error
	)
	if IsSprintStart(headerNumber, c.config.CalculateSprint(headerNumber)) {
		start := time.Now()
		cx := statefull.ChainContext{Chain: chain, Bor: c}
		// check and commit span
		if !c.config.IsRio(header.Number) {
			if err := c.checkAndCommitSpan(wrappedState, header, cx); err != nil {
				return nil, fmt.Errorf("error while committing span: %w", err)
			}
		}

		if c.HeimdallClient != nil {
			// commit states
			stateSyncData, err = c.CommitStates(wrappedState, header, cx)
			if err != nil {
				return nil, fmt.Errorf("%w: error while committing states: %w", core.ErrStateSyncProcessing, err)
			}
		}
		// Get the underlying state for updating consensus time
		state := wrappedState.Inner()
		state.BorConsensusTime = time.Since(start)
	}

	// Check if any hardfork needs change in genesis contract code. Note that we use
	// the wrapped state here as it may have a hooked state db instance which can help
	// in tracing if it's enabled. Note: when live tracing of state-sync is active,
	// OnCodeChange events from these block-alloc upgrades are emitted *inside* the
	// state-sync tx's OnTxStart/OnTxEnd window in the caller's trace stream. This
	// is a known minor attribution quirk; events are emitted correctly, only their
	// containing tx-scope is the state-sync tx rather than a block-level system context.
	if err = c.changeContractCodeIfNeeded(headerNumber, wrappedState); err != nil {
		return nil, fmt.Errorf("error changing contract code: %w", err)
	}

	// Set state-sync in any case
	if hc, ok := chain.(*core.HeaderChain); ok {
		hc.SetStateSync(stateSyncData)
	}

	if len(stateSyncData) == 0 {
		return receipts, nil
	}

	txs := body.Transactions
	isMadhugiri := c.config != nil && c.config.IsMadhugiri(header.Number)

	// Pre-Madhugiri, state-sync transactions were not included in block body so we can safely return
	if !isMadhugiri {
		return receipts, nil
	}

	// Reject the block as heimdall suggests presence of state-sync event(s) but state-sync
	// transaction is missing from block body.
	if len(txs) == 0 || txs[len(txs)-1].Type() != types.StateSyncTxType {
		return nil, fmt.Errorf("%w: block body missing state-sync transaction, heimdall reported %d event(s)", core.ErrStateSyncMismatch, len(stateSyncData))
	}

	// Craft a state-sync tx to validate it against the tx in block body
	stateSyncTx := types.NewTx(&types.StateSyncTx{
		StateSyncData: stateSyncData,
	})
	lastTx := txs[len(txs)-1]
	if stateSyncTx.Hash() != lastTx.Hash() {
		return nil, fmt.Errorf("%w: hash mismatch, got %s want %s", core.ErrStateSyncMismatch, lastTx.Hash(), stateSyncTx.Hash())
	}
	receipts = insertStateSyncTransactionAndCalculateReceipt(lastTx, header, body, wrappedState, receipts)
	return receipts, nil
}

func insertStateSyncTransactionAndCalculateReceipt(stateSyncTx *types.Transaction, header *types.Header, body *types.Body, state vm.StateDB, receipts []*types.Receipt) []*types.Receipt {
	allLogs := state.Logs()
	sort.SliceStable(allLogs, func(i, j int) bool {
		return allLogs[i].Index < allLogs[j].Index
	})
	logsFromReceiptCount := countLogsFromReceipts(receipts)
	stateSyncLogs := allLogs[logsFromReceiptCount:]

	txIndex := uint(len(body.Transactions) - 1)
	for _, l := range stateSyncLogs {
		l.TxIndex = txIndex
	}

	var cumulativeGasUsed uint64
	if len(receipts) > 0 {
		cumulativeGasUsed = receipts[len(receipts)-1].CumulativeGasUsed
	}

	stateSyncReceipt := &types.Receipt{
		// Consensus fields
		Type:              types.StateSyncTxType,
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: cumulativeGasUsed,
		Logs:              stateSyncLogs,
		// Implementation fields
		TxHash:  stateSyncTx.Hash(),
		GasUsed: 0,
		// Inclusion information
		BlockNumber:      header.Number,
		TransactionIndex: txIndex,
	}

	stateSyncReceipt.Bloom = types.CreateBloom(stateSyncReceipt)
	receipts = append(receipts, stateSyncReceipt)

	return receipts
}

func decodeGenesisAlloc(i interface{}) (types.GenesisAlloc, error) {
	var alloc types.GenesisAlloc

	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(b, &alloc); err != nil {
		return nil, err
	}

	return alloc, nil
}

func (c *Bor) changeContractCodeIfNeeded(headerNumber uint64, state vm.StateDB) error {
	for blockNumber, genesisAlloc := range c.config.BlockAlloc {
		if blockNumber == strconv.FormatUint(headerNumber, 10) {
			allocs, err := decodeGenesisAlloc(genesisAlloc)
			if err != nil {
				return fmt.Errorf("failed to decode genesis alloc: %w", err)
			}

			for addr, account := range allocs {
				log.Info("change contract code", "address", addr)
				state.SetCode(addr, account.Code, tracing.CodeChangeUnspecified)

				if state.GetBalance(addr).Cmp(uint256.NewInt(0)) == 0 {
					state.SetBalance(addr, uint256.MustFromBig(account.Balance), tracing.BalanceChangeUnspecified)
				}
			}
		}
	}

	return nil
}

// FinalizeAndAssemble implements consensus.Engine, ensuring no uncles are set,
// nor block rewards given, and returns the final block.
func (c *Bor) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt) (*types.Block, []*types.Receipt, time.Duration, error) {
	return c.finalizeAndAssemble(chain, header, state, body, receipts, false)
}

// FinalizeAndAssembleForSimulation is FinalizeAndAssemble for simulated blocks
// (eth_simulateV1). It skips the sprint-start span and state-sync commits:
// their internal genesis contract calls resolve the parent header by hash from
// the database, but a simulated block's parent may be a phantom header that is
// never persisted (the pending block, or an earlier simulated block). The
// skipped data is external Heimdall input that cannot be known for a future
// block anyway.
func (c *Bor) FinalizeAndAssembleForSimulation(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt) (*types.Block, []*types.Receipt, time.Duration, error) {
	return c.finalizeAndAssemble(chain, header, state, body, receipts, true)
}

func (c *Bor) finalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt, simulated bool) (*types.Block, []*types.Receipt, time.Duration, error) {
	headerNumber := header.Number.Uint64()
	if body.Withdrawals != nil || header.WithdrawalsHash != nil {
		return nil, nil, 0, consensus.ErrUnexpectedWithdrawals
	}
	if header.RequestsHash != nil {
		return nil, nil, 0, consensus.ErrUnexpectedRequests
	}

	var (
		stateSyncData []*types.StateSyncData
		err           error
	)

	if !simulated && IsSprintStart(headerNumber, c.config.CalculateSprint(headerNumber)) {
		stateSyncData, err = c.commitSprintWork(chain, header, state)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	if err = c.changeContractCodeIfNeeded(headerNumber, state); err != nil {
		log.Error("Error changing contract code", "error", err)
		return nil, nil, 0, err
	}

	// No block rewards in PoA, so the state remains as it is
	start := time.Now()
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	commitTime := time.Since(start)

	// Uncles are dropped
	header.UncleHash = types.CalcUncleHash(nil)

	if len(stateSyncData) > 0 && c.config != nil && c.config.IsMadhugiri(big.NewInt(int64(headerNumber))) {
		stateSyncTx := types.NewTx(&types.StateSyncTx{
			StateSyncData: stateSyncData,
		})
		body.Transactions = append(body.Transactions, stateSyncTx)
		receipts = insertStateSyncTransactionAndCalculateReceipt(stateSyncTx, header, body, state, receipts)
	} else {
		// set state sync
		bc := chain.(core.BorStateSyncer)
		bc.SetStateSync(stateSyncData)
	}

	// Assemble block
	block := types.NewBlock(header, body, receipts, trie.NewStackTrie(nil))

	// return the final block for sealing
	return block, receipts, commitTime, nil
}

// FinalizeForPipeline runs the same post-transaction state modifications as
// FinalizeAndAssemble (state sync, span commits, contract code changes) but
// does NOT compute IntermediateRoot or assemble the block. It returns the
// stateSyncData so the caller can pass it to AssembleBlock later after the
// background SRC goroutine has computed the state root.
//
// This is the pipelined SRC equivalent of the first half of FinalizeAndAssemble.
func (c *Bor) FinalizeForPipeline(chain consensus.ChainHeaderReader, header *types.Header, statedb *state.StateDB, body *types.Body, receipts []*types.Receipt) ([]*types.StateSyncData, error) {
	headerNumber := header.Number.Uint64()
	if body.Withdrawals != nil || header.WithdrawalsHash != nil {
		return nil, consensus.ErrUnexpectedWithdrawals
	}
	if header.RequestsHash != nil {
		return nil, consensus.ErrUnexpectedRequests
	}

	var (
		stateSyncData []*types.StateSyncData
		err           error
	)

	if IsSprintStart(headerNumber, c.config.CalculateSprint(headerNumber)) {
		stateSyncData, err = c.commitSprintWork(chain, header, statedb)
		if err != nil {
			return nil, err
		}
	}

	if err = c.changeContractCodeIfNeeded(headerNumber, statedb); err != nil {
		log.Error("Error changing contract code", "error", err)
		return nil, err
	}

	return stateSyncData, nil
}

// AssembleBlock constructs the final block from a pre-computed state root,
// without calling IntermediateRoot. This is used by pipelined SRC where the
// state root is computed by a background goroutine.
//
// stateSyncData is the state sync data collected during Finalize(). If non-nil
// and the Madhugiri fork is active, a StateSyncTx is appended to the body.
func (c *Bor) AssembleBlock(chain consensus.ChainHeaderReader, header *types.Header, statedb *state.StateDB, body *types.Body, receipts []*types.Receipt, stateRoot common.Hash, stateSyncData []*types.StateSyncData) (*types.Block, []*types.Receipt, error) {
	headerNumber := header.Number.Uint64()

	header.Root = stateRoot
	header.UncleHash = types.CalcUncleHash(nil)

	if len(stateSyncData) > 0 && c.config != nil && c.config.IsMadhugiri(big.NewInt(int64(headerNumber))) {
		stateSyncTx := types.NewTx(&types.StateSyncTx{
			StateSyncData: stateSyncData,
		})
		body.Transactions = append(body.Transactions, stateSyncTx)
		receipts = insertStateSyncTransactionAndCalculateReceipt(stateSyncTx, header, body, statedb, receipts)
	} else {
		bc := chain.(core.BorStateSyncer)
		bc.SetStateSync(stateSyncData)
	}

	block := types.NewBlock(header, body, receipts, trie.NewStackTrie(nil))
	return block, receipts, nil
}

// commitSprintWork commits the span (pre-Rio) and state-sync data at a
// sprint-start block during block assembly.
func (c *Bor) commitSprintWork(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB) ([]*types.StateSyncData, error) {
	borStart := time.Now()
	cx := statefull.ChainContext{Chain: chain, Bor: c}

	// check and commit span
	if !c.config.IsRio(header.Number) {
		if err := c.checkAndCommitSpan(state, header, cx); err != nil {
			log.Error("Error while committing span", "error", err)
			return nil, err
		}
	}

	var stateSyncData []*types.StateSyncData

	if c.HeimdallClient != nil {
		// commit states
		var err error
		stateSyncData, err = c.CommitStates(state, header, cx)
		if err != nil {
			log.Error("Error while committing states", "error", err)
			return nil, err
		}
	}

	state.BorConsensusTime = time.Since(borStart)

	return stateSyncData, nil
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (c *Bor) Authorize(currentSigner common.Address, signFn SignerFn) {
	c.authorizedSigner.Store(&signer{
		signer: currentSigner,
		signFn: signFn,
	})
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (c *Bor) Seal(chain consensus.ChainHeaderReader, block *types.Block, witness *stateless.Witness, results chan<- *consensus.NewSealedBlockEvent, stop <-chan struct{}) error {
	return c.SealWithStopHook(chain, block, witness, results, stop, nil)
}

// SealWithStopHook is identical to Seal but invokes onStopExit (if non-nil)
// from the sealing goroutine on stop-branch exits only. The hook is NOT
// called on the successful-delivery path.
func (c *Bor) SealWithStopHook(chain consensus.ChainHeaderReader, block *types.Block, witness *stateless.Witness, results chan<- *consensus.NewSealedBlockEvent, stop <-chan struct{}, onStopExit func()) error {
	header := block.Header()
	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// For 0-period chains, refuse to seal empty blocks (no reward but would spin sealing)
	if c.config.CalculatePeriod(number) == 0 && len(block.Transactions()) == 0 {
		log.Info("Sealing paused, waiting for transactions")
		return nil
	}

	// Don't hold the signer fields for the entire sealing procedure
	currentSigner := *c.authorizedSigner.Load()

	snap, err := c.snapshot(chain, header, nil, false)
	if err != nil {
		return err
	}

	// Bail out if we're unauthorized to sign a block
	if !snap.ValidatorSet.HasAddress(currentSigner.signer) && !snap.isAllowedByValidatorSetOverride(currentSigner.signer, header.Number.Uint64()) {
		// Check the UnauthorizedSignerError.Error() msg to see why we pass number-1
		return &UnauthorizedSignerError{number, currentSigner.signer.Bytes(), snap.ValidatorSet.Validators}
	}

	successionNumber, err := snap.GetSignerSuccessionNumber(currentSigner.signer)
	if err != nil {
		return err
	}

	var delay time.Duration

	// Sweet, the protocol permits us to sign the block, wait for our time.
	// On Giugliano+ primary producers, the wait is performed before building
	// in Prepare (or explicitly by the pipeline at the parent boundary), so Seal
	// returns immediately and preserves early block announcement. Backups still
	// wait until the block timestamp.
	if c.config.IsGiugliano(header.Number) && successionNumber == 0 {
		delay = 0
	} else {
		delay = time.Until(header.GetActualTime())
	}

	// wiggle was already accounted for in header.Time, this is just for logging
	wiggle := time.Duration(successionNumber) * time.Duration(c.config.CalculateBackupMultiplier(number)) * time.Second

	// Sign all the things!
	err = Sign(currentSigner.signFn, currentSigner.signer, header, c.config)
	if err != nil {
		return err
	}

	if c.parentActualTimeCache != nil && !header.ActualTime.IsZero() {
		c.parentActualTimeCache.Add(header.Hash(), header.ActualTime)
	}

	// Wait until sealing is terminated or delay timeout.
	log.Info(
		"Waiting for slot to sign and propagate",
		"number", number,
		"hash", header.Hash(),
		"delay-ms", float64(delay)/float64(time.Millisecond),
		"delay", common.PrettyDuration(delay),
	)

	go func() {
		select {
		case <-stop:
			log.Debug("Discarding sealing operation for block", "number", number)
			if onStopExit != nil {
				onStopExit()
			}
			return
		case <-time.After(delay):
			if wiggle > 0 {
				log.Info(
					"Sealing out-of-turn",
					"number", number,
					"hash", header.Hash,
					"wiggle-ms", float64(wiggle)/float64(time.Millisecond),
					"wiggle", common.PrettyDuration(wiggle),
					"in-turn-signer", snap.ValidatorSet.GetProposer().Address.Hex(),
				)
			}

			log.Info(
				"Sealing successful",
				"number", number,
				"delay", delay,
				"headerDifficulty", header.Difficulty,
			)
		}
		// Block on send (or exit on stop). A default branch here would
		// drop the result silently when results is full, leaking the
		// miner's pendingTasks entry.
		select {
		case results <- &consensus.NewSealedBlockEvent{Block: block.WithSeal(header), Witness: witness}:
		case <-stop:
			log.Info("Seal interrupted before result delivery", "number", number, "sealhash", SealHash(header, c.config))
			if onStopExit != nil {
				onStopExit()
			}
		}
	}()

	return nil
}

func Sign(signFn SignerFn, signer common.Address, header *types.Header, c *params.BorConfig) error {
	sighash, err := signFn(accounts.Account{Address: signer}, accounts.MimetypeBor, BorRLP(header, c))
	if err != nil {
		return err
	}

	copy(header.Extra[len(header.Extra)-types.ExtraSealLength:], sighash)

	return nil
}

// SignBytes signs the supplied preimage bytes under a context-specific
// mimetype using the engine's currently authorized signer. The mimetype is the
// domain tag the underlying signer (clef, keystore) sees, so callers MUST pass
// a context-specific value (e.g. accounts.MimetypeBorWitnessAnnounce) and
// never reuse accounts.MimetypeBor outside of header sealing — that would let
// a signature produced here be replayed as a block-seal signature on any
// header BorRLP that hashes to the same digest.
//
// Callers pass the unhashed preimage; the wallet's SignData implementation
// applies keccak256 once before signing. Verifiers must independently hash
// the same preimage and ecrecover against the resulting digest.
func (c *Bor) SignBytes(mimetype string, digest []byte) (signer common.Address, sig []byte, err error) {
	if mimetype == "" || mimetype == accounts.MimetypeBor {
		return common.Address{}, nil, errors.New("bor: SignBytes requires a non-empty, non-header mimetype")
	}
	current := c.authorizedSigner.Load()
	if current == nil || current.signer == (common.Address{}) {
		return common.Address{}, nil, errors.New("bor: no authorized signer configured")
	}
	sig, err = current.signFn(accounts.Account{Address: current.signer}, mimetype, digest)
	if err != nil {
		return common.Address{}, nil, err
	}
	return current.signer, sig, nil
}

// CurrentSigner returns the address of the currently authorized signer, or
// the zero address if none has been configured.
func (c *Bor) CurrentSigner() common.Address {
	current := c.authorizedSigner.Load()
	if current == nil {
		return common.Address{}
	}
	return current.signer
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func (c *Bor) CalcDifficulty(chain consensus.ChainHeaderReader, _ uint64, parent *types.Header) *big.Int {
	snap, err := c.snapshot(chain, parent, nil, true)
	if err != nil {
		return nil
	}

	return new(big.Int).SetUint64(Difficulty(snap.ValidatorSet, c.authorizedSigner.Load().signer))
}

// SealHash returns the hash of a block prior to it being sealed.
func (c *Bor) SealHash(header *types.Header) common.Hash {
	return SealHash(header, c.config)
}

// APIs implements consensus.Engine, returning the user facing RPC API to allow
// controlling the signer voting.
//
// The returned *API is cached on the first call so that per-API state (e.g.,
// rootHashCache) persists across calls. JSON-RPC only invokes APIs() once at
// node startup, but the gRPC backend fetches it on every handler call — without
// the cache those calls would each start from an empty state.
//
// rootHashCache is initialized here (inside the sync.Once) rather than lazily
// in GetRootHash so that concurrent gRPC handlers sharing the cached *API
// cannot race in initializeRootHashCache.
func (c *Bor) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	c.apiOnce.Do(func() {
		a := &API{chain: chain, bor: c}
		if err := a.initializeRootHashCache(); err != nil {
			// log.Crit logs at the highest severity and then exits the process;
			// This is currently unreachable (size is a constant in initializeRootHashCache),
			log.Crit("bor: failed to initialize rootHashCache", "err", err)
		}
		c.api = a
	})
	return []rpc.API{{
		Namespace: "bor",
		Version:   "1.0",
		Service:   c.api,
		Public:    false,
	}}
}

// Close implements consensus.Engine.
func (c *Bor) Close() error {
	c.closeOnce.Do(func() {
		c.ctxCancel()
		close(c.quit)
		if c.HeimdallClient != nil {
			c.HeimdallClient.Close()
		}

		if c.spanStore != nil {
			c.spanStore.Close()
		}
	})

	return nil
}

func (c *Bor) runMilestoneFetcher() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if c.HeimdallClient != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				milestone, err := c.HeimdallClient.FetchMilestone(ctx)
				cancel()
				if err != nil {
					log.Warn("Error while fetching milestone", "error", err)
					continue
				}

				if milestone != nil {
					c.latestMilestoneBlock.Store(milestone.EndBlock)
				}
			}
		case <-c.quit:
			return
		}
	}
}

// stateTracingHooks is implemented by state wrappers (state.NewHookedState)
// that emit tracing hooks for the state they wrap.
type stateTracingHooks interface {
	Hooks() *tracing.Hooks
}

// systemTxVMConfig returns the vm.Config to use when applying bor system
// transactions (span commits and state-sync events) over the given state.
//
// The tracer is derived from the state itself rather than taken from
// c.vmConfig: canonical block import passes a hooked state when a live tracer
// is configured, so system transactions keep being traced there. Every other
// caller — the miner and eth_simulateV1 via FinalizeAndAssemble, or historical
// state regeneration via Finalize — passes a plain state and must not fire the
// node-wide live tracer: those run outside the import goroutine, and invoking
// the singleton live tracer concurrently corrupts its state and can crash the
// node.
func (c *Bor) systemTxVMConfig(state vm.StateDB) vm.Config {
	cfg := c.vmConfig
	if hooked, ok := state.(stateTracingHooks); ok {
		cfg.Tracer = hooked.Hooks()
	} else {
		cfg.Tracer = nil
	}

	return cfg
}

func (c *Bor) checkAndCommitSpan(
	state vm.StateDB,
	header *types.Header,
	chain core.ChainContext,
) error {
	var ctx = context.Background()
	headerNumber := header.Number.Uint64()

	tempState := state.Inner().Copy()
	tempState.ResetPrefetcher()
	tempState.StartPrefetcher("bor", state.Witness(), nil)

	span, err := c.spanner.GetCurrentSpan(ctx, header.ParentHash, tempState)
	if err != nil {
		return err
	}

	tempState.IntermediateRoot(false)

	// Propagate addresses accessed during GetCurrentSpan back to the original
	// state so they appear in the FlatDiff ReadSet. Without this, the pipelined
	// SRC goroutine's witness won't capture their trie proof nodes (the copy's
	// reads aren't tracked on the original), causing stateless execution to fail
	// with missing trie nodes for the validator contract.
	tempState.PropagateReadsTo(state.Inner())

	if c.needToCommitSpan(span, headerNumber) {
		return c.FetchAndCommitSpan(ctx, span.Id+1, state, header, chain)
	}

	return nil
}

func (c *Bor) needToCommitSpan(currentSpan *borTypes.Span, headerNumber uint64) bool {
	// If span is nil, return false.
	if currentSpan == nil {
		return false
	}

	// Check if span is not set initially, we commit the span with spanId 1, which will also commit the 0th span.
	// Check: https://github.com/0xPolygon/genesis-contracts/blob/5dcbcc72f10ab847276586e629f96b8a6d369e1d/contracts/BorValidatorSet.template#L229
	if currentSpan.EndBlock == 0 {
		return true
	}

	// If the current block is the first block of the last sprint in the current span.
	// But here we should skip the check for the 0th span, as it will cause the span to be committed twice.
	sprintLength := c.config.CalculateSprint(headerNumber)
	if currentSpan.EndBlock > sprintLength && currentSpan.EndBlock-sprintLength+1 == headerNumber {
		if currentSpan.Id == 0 {
			// If the current span is the 0th span, we will skip committing the span.
			log.Info("Skipping the last sprint commit for 0th span", "spanID", currentSpan.Id, "headerNumber", headerNumber)
			return false
		}
		return true
	}

	return false
}

func (c *Bor) FetchAndCommitSpan(
	ctx context.Context,
	newSpanID uint64,
	state vm.StateDB,
	header *types.Header,
	chain core.ChainContext,
) error {
	var (
		minSpan    borTypes.Span
		chainId    string
		validators []stakeTypes.MinimalVal
		producers  []stakeTypes.MinimalVal
	)

	if c.HeimdallClient == nil {
		// fixme: move to a new mock or fake and remove c.HeimdallClient completely
		s, err := c.getNextHeimdallSpanForTest(ctx, newSpanID, header, chain)
		if err != nil {
			return err
		}

		minSpan = borTypes.Span{
			Id:         s.Id,
			StartBlock: s.StartBlock,
			EndBlock:   s.EndBlock,
		}
		chainId = s.BorChainId

		for _, val := range s.ValidatorSet.Validators {
			validators = append(validators, val.MinimalVal())
		}

		for _, val := range s.SelectedProducers {
			producers = append(producers, val.MinimalVal())
		}
	} else {
		response, err := c.spanStore.spanById(ctx, newSpanID)
		if err != nil {
			return fmt.Errorf("failed to get span by id %d: %w", newSpanID, err)
		}
		if response == nil {
			return fmt.Errorf("span with id %d not found", newSpanID)
		}

		minSpan = borTypes.Span{
			Id:         response.Id,
			StartBlock: response.StartBlock,
			EndBlock:   response.EndBlock,
		}
		chainId = response.BorChainId

		for _, val := range response.ValidatorSet.Validators {
			validators = append(validators, val.MinimalVal())
		}

		for _, val := range response.SelectedProducers {
			producers = append(producers, val.MinimalVal())
		}
	}

	// check if chain id matches with Heimdall span
	if chainId != c.chainConfig.ChainID.String() {
		return fmt.Errorf(
			"chain id proposed span, %s, and bor chain id, %s, doesn't match",
			chainId,
			c.chainConfig.ChainID,
		)
	}

	return c.spanner.CommitSpan(ctx, minSpan, validators, producers, state, header, chain, c.systemTxVMConfig(state))
}

// CommitStates commit states
func (c *Bor) CommitStates(
	state vm.StateDB,
	header *types.Header,
	chain statefull.ChainContext,
) ([]*types.StateSyncData, error) {
	fetchStart := time.Now()
	number := header.Number.Uint64()

	// Check for override state sync records before fetching event records
	if c.config.OverrideStateSyncRecordsInRange != nil {
		overrideStateSyncRecord, ok := c.config.GetOverrideStateSyncRecord(number)
		if ok && overrideStateSyncRecord == 0 {
			// If override value is 0, skip fetching event records entirely
			log.Info("Skipping state sync events fetch due to override value 0", "number", number)
			return make([]*types.StateSyncData, 0), nil
		}
	}

	var (
		lastStateIDBig *big.Int
		from           uint64
		to             time.Time
		err            error
	)

	if c.config.IsIndore(header.Number) {
		// Fetch the LastStateId from contract via current state instance
		tempState := state.Inner().Copy()
		tempState.ResetPrefetcher()
		tempState.StartPrefetcher("bor", state.Witness(), nil)

		lastStateIDBig, err = c.GenesisContractsClient.LastStateId(tempState, number-1, header.ParentHash)
		if err != nil {
			return nil, err
		}

		tempState.IntermediateRoot(false)

		// Propagate addresses accessed during LastStateId back to the original
		// state so they appear in the FlatDiff ReadSet. Without this, the
		// pipelined SRC goroutine's witness won't capture their trie proof
		// nodes, causing stateless execution to fail with missing trie nodes.
		tempState.PropagateReadsTo(state.Inner())

		stateSyncDelay := c.config.CalculateStateSyncDelay(number)
		to = time.Unix(int64(header.Time-stateSyncDelay), 0)
	} else {
		lastStateIDBig, err = c.GenesisContractsClient.LastStateId(nil, number-1, header.ParentHash)
		if err != nil {
			return nil, err
		}

		to = time.Unix(int64(chain.Chain.GetHeaderByNumber(number-c.config.CalculateSprint(number)).Time), 0)
	}

	lastStateID := lastStateIDBig.Uint64()
	from = lastStateID + 1

	log.Info(
		"Fetching state updates from Heimdall",
		"fromID", from,
		"to", to.Format(time.RFC3339))

	var eventRecords []*clerk.EventRecordWithTime

	// Wait for heimdall to be synced before fetching state sync events
	c.spanStore.waitUntilHeimdallIsSynced(c.ctx)

	eventRecords, err = c.HeimdallClient.StateSyncEvents(c.ctx, from, to.Unix())
	if err != nil {
		log.Error("Error occurred when fetching state sync events", "fromID", from, "to", to.Unix(), "err", err)

		stateSyncs := make([]*types.StateSyncData, 0)
		return stateSyncs, nil
	}

	// This if statement checks if there are any state sync record overrides configured for the current block number.
	// If there are, it truncates the eventRecords array to the specified number of records.
	if c.config.OverrideStateSyncRecords != nil {
		if val, ok := c.config.OverrideStateSyncRecords[strconv.FormatUint(number, 10)]; ok {
			eventRecords = eventRecords[0:val]
		}
	}

	// This if statement checks if there are any state sync record overrides configured for the current block number.
	// If there are, it truncates the eventRecords array to the specified number of records.
	if c.config.OverrideStateSyncRecordsInRange != nil {
		overrideStateSyncRecord, ok := c.config.GetOverrideStateSyncRecord(number)
		if ok {
			eventRecords = eventRecords[0:overrideStateSyncRecord]
		}
	}

	fetchTime := time.Since(fetchStart)
	processStart := time.Now()

	var totalGas uint64
	chainID := c.chainConfig.ChainID.String()
	stateSyncs := make([]*types.StateSyncData, 0, len(eventRecords))

	enforceStateSyncBudget := c.config.IsValencia(header.Number)
	enforceStateSyncGasBudget := c.config.IsAustin(header.Number)
	stateReceiver := common.HexToAddress(c.config.StateReceiverContract)
	var stateSyncBytes uint64

	var gasUsed uint64

	vmConfig := c.systemTxVMConfig(state)

	for _, eventRecord := range eventRecords {
		if eventRecord.ID <= lastStateID {
			continue
		}

		if err = validateEventRecord(eventRecord, number, to, lastStateID, chainID); err != nil {
			log.Error("while validating event record", "block", number, "to", to, "stateID", lastStateID+1, "error", err.Error())
			break
		}

		// From Valencia on, cap the state-sync bytes committed per block; records over
		// the budget wait for a later sprint (lastStateID only advances for the ones we
		// include). The first record always goes in, so a single over-budget record
		// can't stall state sync forever.
		recordSize := uint64(len(eventRecord.Data))
		if len(stateSyncs) > 0 && stateSyncBudgetExceeded(enforceStateSyncBudget, stateSyncBytes, recordSize) {
			log.Info("state-sync byte budget reached, deferring remaining records", "number", number, "includedBytes", stateSyncBytes, "deferredFromID", eventRecord.ID)
			break
		}

		// totalGas starts at zero, so the first pending record is always admitted.
		if enforceStateSyncGasBudget && totalGas >= params.MaxStateSyncGasPerBlock {
			log.Info("state-sync gas budget reached, deferring remaining records", "number", number, "includedGas", totalGas, "deferredFromID", eventRecord.ID)
			break
		}

		// A record over Heimdall's per-record cap shouldn't happen; log it if one does.
		if enforceStateSyncBudget && recordSize > params.MaxStateSyncRecordBytes {
			log.Error("state-sync record exceeds expected per-record cap", "number", number, "id", eventRecord.ID, "size", recordSize, "cap", params.MaxStateSyncRecordBytes)
		}

		stateSyncBytes += recordSize

		stateData := types.StateSyncData{
			ID:       eventRecord.ID,
			Contract: eventRecord.Contract,
			Data:     eventRecord.Data,
			TxHash:   eventRecord.TxHash,
		}

		stateSyncs = append(stateSyncs, &stateData)
		statefull.PrepareStateSyncContext(state, c.chainConfig, header.Number, header.Time, header.Coinbase, stateReceiver)

		// Receipt construction expects the receiver call to emit at least one log.
		// A receiver without code can complete without producing one.
		gasUsed, err = c.GenesisContractsClient.CommitState(eventRecord, state, header, chain, vmConfig)
		if err != nil {
			return nil, err
		}

		totalGas += gasUsed

		lastStateID++
	}

	processTime := time.Since(processStart)

	log.Info("StateSyncData", "gas", totalGas, "number", number, "lastStateID", lastStateID, "total records", len(eventRecords), "fetch time", int(fetchTime.Milliseconds()), "process time", int(processTime.Milliseconds()))

	return stateSyncs, nil
}

func validateEventRecord(eventRecord *clerk.EventRecordWithTime, number uint64, to time.Time, lastStateID uint64, chainID string) error {
	// event id should be sequential and event.Time should lie in the range [from, to)
	if lastStateID+1 != eventRecord.ID || eventRecord.ChainID != chainID || !eventRecord.Time.Before(to) {
		return &InvalidStateReceivedError{number, lastStateID, &to, eventRecord}
	}

	return nil
}

// stateSyncBudgetExceeded reports whether committing a record of recordSize bytes
// on top of includedBytes already committed would push the block's state-sync data
// past params.MaxStateSyncBytesPerBlock. When enforce is false (pre-Valencia) the
// batch stays unbounded, preserving the historical state transition.
func stateSyncBudgetExceeded(enforce bool, includedBytes, recordSize uint64) bool {
	if !enforce {
		return false
	}

	return includedBytes+recordSize > params.MaxStateSyncBytesPerBlock
}

func (c *Bor) SetHeimdallClient(h IHeimdallClient) {
	c.HeimdallClient = h
	// Update the heimdall client in span store
	c.spanStore.setHeimdallClient(h)
}

// PurgeCache clears all cached snapshots and span data. This is useful in tests
// when the mock heimdall client is changed and old cached data needs to be invalidated.
func (c *Bor) PurgeCache() {
	// Clear the recents cache (snapshots)
	c.recents.DeleteAll()
	// Clear the recent verified headers cache
	c.recentVerifiedHeaders.DeleteAll()
	// Clear the span store cache
	c.spanStore.PurgeCache()
}

func (c *Bor) GetCurrentValidators(ctx context.Context, headerHash common.Hash, blockNumber uint64) ([]*valset.Validator, error) {
	return c.spanner.GetCurrentValidatorsByHash(ctx, headerHash, blockNumber)
}

//
// Private methods
//

func (c *Bor) getNextHeimdallSpanForTest(
	ctx context.Context,
	newSpanID uint64,
	header *types.Header,
	chain core.ChainContext,
) (*borTypes.Span, error) {
	headerNumber := header.Number.Uint64()

	spanBor, err := c.spanner.GetCurrentSpan(ctx, header.ParentHash, nil)
	if err != nil {
		return nil, err
	}

	// get local chain context object
	localContext := chain.(statefull.ChainContext)
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.snapshot(localContext.Chain, header, nil, false)
	if err != nil {
		return nil, err
	}

	// new span
	spanBor.Id = newSpanID
	if spanBor.EndBlock == 0 {
		spanBor.StartBlock = 256
	} else {
		spanBor.StartBlock = spanBor.EndBlock + 1
	}

	spanBor.EndBlock = spanBor.StartBlock + (100 * c.config.CalculateSprint(headerNumber)) - 1
	spanBor.BorChainId = c.chainConfig.ChainID.String()
	spanBor.ValidatorSet = borSpan.ConvertBorValSetToHeimdallValSet(snap.ValidatorSet)
	spanBor.SelectedProducers = borSpan.ConvertBorValidatorsToHeimdallValidators(snap.ValidatorSet.Validators)

	return spanBor, nil
}

func validatorContains(a []*valset.Validator, x *valset.Validator) (*valset.Validator, bool) {
	for _, n := range a {
		if n.Address == x.Address {
			return n, true
		}
	}

	return nil, false
}

func getUpdatedValidatorSet(oldValidatorSet *valset.ValidatorSet, newVals []*valset.Validator) *valset.ValidatorSet {
	v := oldValidatorSet
	oldVals := v.Validators

	changes := make([]*valset.Validator, 0, len(oldVals))

	for _, ov := range oldVals {
		if f, ok := validatorContains(newVals, ov); ok {
			ov.VotingPower = f.VotingPower
		} else {
			ov.VotingPower = 0
		}

		changes = append(changes, ov)
	}

	for _, nv := range newVals {
		if _, ok := validatorContains(changes, nv); !ok {
			changes = append(changes, nv)
		}
	}

	if err := v.UpdateWithChangeSet(changes); err != nil {
		changesStr := ""
		for _, change := range changes {
			changesStr += fmt.Sprintf("Address: %s, VotingPower: %d\n", change.Address, change.VotingPower)
		}
		log.Warn("Changes in validator set", "changes", changesStr)
		log.Error("Error while updating change set", "error", err)
	}

	return v
}

func IsSprintStart(number, sprint uint64) bool {
	return number%sprint == 0
}

func countLogsFromReceipts(receipts []*types.Receipt) int {
	total := 0
	for _, receipt := range receipts {
		if receipt != nil {
			total += len(receipt.Logs)
		}
	}
	return total
}
