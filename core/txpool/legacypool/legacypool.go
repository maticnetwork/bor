// Copyright 2014 The go-ethereum Authors
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

// Package legacypool implements the normal EVM execution transaction pool.
package legacypool

import (
	"errors"
	"maps"
	"math"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// txSlotSize is used to calculate how many data slots a single transaction
	// takes up based on its size. The slots are used as DoS protection, ensuring
	// that validating a new transaction remains a constant operation (in reality
	// O(maxslots), where max slots are 4 currently).
	txSlotSize = 32 * 1024

	// txMaxSize is the maximum size a single transaction can have. This field has
	// non-trivial consequences: larger transactions are significantly harder and
	// more expensive to propagate; larger transactions also take more resources
	// to validate whether they fit into the pool or not.
	txMaxSize = 4 * txSlotSize // 128KB
)

var (
	// ErrTxPoolOverflow is returned if the transaction pool is full and can't accept
	// another remote transaction.
	ErrTxPoolOverflow = errors.New("txpool is full")

	// ErrOutOfOrderTxFromDelegated is returned when the transaction with gapped
	// nonce received from the accounts with delegation or pending delegation.
	ErrOutOfOrderTxFromDelegated = errors.New("gapped-nonce tx from delegated accounts")

	// ErrAuthorityReserved is returned if a transaction has an authorization
	// signed by an address which already has in-flight transactions known to the
	// pool.
	ErrAuthorityReserved = errors.New("authority already reserved")

	// ErrFutureReplacePending is returned if a future transaction replaces a pending
	// one. Future transactions should only be able to replace other future transactions.
	ErrFutureReplacePending = errors.New("future transaction tries to replace pending")

	// ErrTxFiltered is returned if a transaction is from a filtered address.
	ErrTxFiltered = errors.New("transaction from filtered address")

	// ErrReservedOccupancyExceeded is returned when admitting a reserved-blockspace
	// sender's transaction would push aggregate reserved occupancy over its cap,
	// independent of overall pool fullness. Distinct from ErrTxPoolOverflow (which
	// fires only once the entire pool is full): the operational fix differs, since
	// this fires purely on the reserved-specific ceiling.
	ErrReservedOccupancyExceeded = errors.New("reserved-sender pool occupancy exceeded")
)

var (
	evictionInterval    = time.Minute     // Time interval to check for evictable transactions
	statsReportInterval = 8 * time.Second // Time interval to report transaction pool stats
)

var (
	// Metrics for the pending pool
	pendingDiscardMeter   = metrics.NewRegisteredMeter("txpool/pending/discard", nil)
	pendingReplaceMeter   = metrics.NewRegisteredMeter("txpool/pending/replace", nil)
	pendingRateLimitMeter = metrics.NewRegisteredMeter("txpool/pending/ratelimit", nil) // Dropped due to rate limiting
	pendingNofundsMeter   = metrics.NewRegisteredMeter("txpool/pending/nofunds", nil)   // Dropped due to out-of-funds

	// Metrics for the queued pool
	queuedDiscardMeter   = metrics.NewRegisteredMeter("txpool/queued/discard", nil)
	queuedReplaceMeter   = metrics.NewRegisteredMeter("txpool/queued/replace", nil)
	queuedRateLimitMeter = metrics.NewRegisteredMeter("txpool/queued/ratelimit", nil) // Dropped due to rate limiting
	queuedNofundsMeter   = metrics.NewRegisteredMeter("txpool/queued/nofunds", nil)   // Dropped due to out-of-funds
	queuedEvictionMeter  = metrics.NewRegisteredMeter("txpool/queued/eviction", nil)  // Dropped due to lifetime

	// General tx metrics
	knownTxMeter       = metrics.NewRegisteredMeter("txpool/known", nil)
	validTxMeter       = metrics.NewRegisteredMeter("txpool/valid", nil)
	invalidTxMeter     = metrics.NewRegisteredMeter("txpool/invalid", nil)
	underpricedTxMeter = metrics.NewRegisteredMeter("txpool/underpriced", nil)
	overflowedTxMeter  = metrics.NewRegisteredMeter("txpool/overflowed", nil)
	filteredTxMeter    = metrics.NewRegisteredMeter("txpool/filtered", nil)

	// throttleTxMeter counts how many transactions are rejected due to too-many-changes between
	// txpool reorgs.
	throttleTxMeter = metrics.NewRegisteredMeter("txpool/throttle", nil)
	// reorgDurationTimer measures how long time a txpool reorg takes.
	reorgDurationTimer = metrics.NewRegisteredTimer("txpool/reorgtime", nil)
	// reorgLockDurationTimer measures how long the global lock was held during
	// a reorg. Note that this won't account for reheap as it happens without
	// the global lock.
	reorgLockDurationTimer = metrics.NewRegisteredTimer("txpool/reorglock", nil)
	// dropBetweenReorgHistogram counts how many drops we experience between two reorg runs. It is expected
	// that this number is pretty low, since txpool reorgs happen very frequently.
	dropBetweenReorgHistogram = metrics.NewRegisteredHistogram("txpool/dropbetweenreorg", nil, metrics.NewExpDecaySample(1028, 0.015))

	pendingGauge = metrics.NewRegisteredGauge("txpool/pending", nil)
	queuedGauge  = metrics.NewRegisteredGauge("txpool/queued", nil)
	slotsGauge   = metrics.NewRegisteredGauge("txpool/slots", nil)

	// reservedOccupancyGauge mirrors pendingGauge/queuedGauge for the combined
	// (pending+queued) occupancy of reserved-blockspace senders.
	reservedOccupancyGauge = metrics.NewRegisteredGauge("txpool/reserved/occupancy", nil)

	resetCacheGauge       = metrics.NewRegisteredGauge("txpool/resetcache", nil)
	reheapTimer           = metrics.NewRegisteredTimer("txpool/reheap", nil)
	urgentHeapInitTimer   = metrics.NewRegisteredTimer("txpool/heapinit/urgent", nil)
	floatingHeapInitTimer = metrics.NewRegisteredTimer("txpool/heapinit/floating", nil)
	urgentHeapPopTimer    = metrics.NewRegisteredTimer("txpool/heappop/urgent", nil)

	reheapDueToStaleCounter   = metrics.NewRegisteredCounter("txpool/reheap/stale", nil)
	reheapDueToBasefeeCounter = metrics.NewRegisteredCounter("txpool/reheap/basefee", nil)

	// pendingLockWaitTimer measures how long it took to acquire the pending lock. This is useful
	// to understand delay in block building and the impact of lock acquisition.
	pendingLockWaitTimer = metrics.NewRegisteredTimer("txpool/pendinglockwait", nil)
	// pendingWaitTimer measures the total time taken for a pending call. This is useful
	// to understand delay in block building.
	pendingWaitTimer = metrics.NewRegisteredTimer("txpool/pendingwait", nil)

	// metrics to capture time taken in adding transactions
	syncAddTimer                = metrics.NewRegisteredTimer("txpool/add/sync", nil)
	asyncAddStage0Timer         = metrics.NewRegisteredTimer("txpool/add/stage0", nil)
	asyncAddStage1Timer         = metrics.NewRegisteredTimer("txpool/add/stage1", nil)
	asyncAddStage2Timer         = metrics.NewRegisteredTimer("txpool/add/stage2", nil)
	asyncAddStage0LockWaitTimer = metrics.NewRegisteredTimer("txpool/add/stage0lockwait", nil)
	asyncAddStage2LockWaitTimer = metrics.NewRegisteredTimer("txpool/add/stage2lockwait", nil)

	// misc metrics for functions using global lock
	reportTimer = metrics.NewRegisteredTimer("txpool/misc/report", nil)
	evictTimer  = metrics.NewRegisteredTimer("txpool/misc/evict", nil)

	// rebroadcast metrics
	rebroadcastTxMeter       = metrics.NewRegisteredMeter("txpool/rebroadcast", nil)          // Transactions identified for rebroadcast
	rebroadcastIdentifyTimer = metrics.NewRegisteredTimer("txpool/rebroadcast/identify", nil) // Time to identify stuck transactions
	rebroadcastTrackingGauge = metrics.NewRegisteredGauge("txpool/rebroadcast/tracking", nil) // Transactions being tracked for rebroadcast
)

// BlockChain defines the minimal set of methods needed to back a tx pool with
// a chain. Exists to allow mocking the live chain out of tests.
type BlockChain interface {
	// Config retrieves the chain's fork configuration.
	Config() *params.ChainConfig

	// CurrentBlock returns the current head of the chain.
	CurrentBlock() *types.Header

	// GetBlock retrieves a specific block, used during pool resets.
	GetBlock(hash common.Hash, number uint64) *types.Block

	// StateAt returns a state database for a given root hash (generally the head).
	StateAt(root common.Hash) (*state.StateDB, error)

	// PostExecState returns a StateDB representing the post-execution
	// state of the given block header. Under pipelined SRC, uses a non-blocking
	// FlatDiff overlay when available; otherwise falls back to StateAt.
	PostExecState(header *types.Header) (*state.StateDB, error)
}

// Config are the configuration parameters of the transaction pool.
type Config struct {
	Locals    []common.Address // Addresses that should be treated by default as local
	NoLocals  bool             // Whether local transaction handling should be disabled
	Journal   string           // Journal of local transactions to survive node restarts
	Rejournal time.Duration    // Time interval to regenerate the local transaction journal

	PriceLimit uint64 // Minimum gas price to enforce for acceptance into the pool
	PriceBump  uint64 // Minimum price bump percentage to replace an already existing transaction (nonce)

	AccountSlots uint64 // Number of executable transaction slots guaranteed per account
	GlobalSlots  uint64 // Maximum number of executable transaction slots for all accounts
	AccountQueue uint64 // Maximum number of non-executable transaction slots permitted per account
	GlobalQueue  uint64 // Maximum number of non-executable transaction slots for all accounts

	// ReservedMaxOccupancyPercent bounds the percentage of GlobalSlots+GlobalQueue
	// that reserved-blockspace senders may occupy in aggregate, combined across
	// pending and queued. Guarantees normal senders at least (100-this)% of the
	// pool regardless of how many addresses a reserved client whitelists.
	ReservedMaxOccupancyPercent uint64

	Lifetime            time.Duration // Maximum amount of time non-executable transaction are queued
	AllowUnprotectedTxs bool          // Allow non-EIP-155 transactions

	// Transaction filtering configuration
	FilteredAddresses map[common.Address]struct{} // Pre-loaded filtered addresses (populated by config)

	// Rebroadcast configuration for stuck transactions
	Rebroadcast          bool          // Enable stuck transaction rebroadcast
	RebroadcastInterval  time.Duration // Interval between rebroadcast checks
	RebroadcastMaxAge    time.Duration // Max age for rebroadcast eligibility
	RebroadcastBatchSize int           // Max transactions per rebroadcast cycle
}

// DefaultConfig contains the default configurations for the transaction pool.
var DefaultConfig = Config{
	Journal:   "transactions.rlp",
	Rejournal: time.Hour,

	PriceLimit: params.BorDefaultTxPoolPriceLimit,
	PriceBump:  10,

	AccountSlots: 16,
	GlobalSlots:  4096 + 1024, // urgent + floating queue capacity with 4:1 ratio
	AccountQueue: 64,
	GlobalQueue:  1024,

	ReservedMaxOccupancyPercent: 50, // normal senders always keep at least half the pool

	Lifetime:            3 * time.Hour,
	AllowUnprotectedTxs: false,

	Rebroadcast:          true,
	RebroadcastInterval:  30 * time.Second,
	RebroadcastMaxAge:    10 * time.Minute,
	RebroadcastBatchSize: 200,
}

// sanitize checks the provided user configurations and changes anything that's
// unreasonable or unworkable.
func (config *Config) sanitize() Config {
	conf := *config
	// PIP-35: Enforce min price limit to 25 gwei
	if conf.PriceLimit != params.BorDefaultTxPoolPriceLimit {
		log.Warn("Sanitizing invalid txpool price limit", "provided", conf.PriceLimit, "updated", DefaultConfig.PriceLimit)
		conf.PriceLimit = DefaultConfig.PriceLimit
	}
	if conf.PriceBump < 1 {
		log.Warn("Sanitizing invalid txpool price bump", "provided", conf.PriceBump, "updated", DefaultConfig.PriceBump)
		conf.PriceBump = DefaultConfig.PriceBump
	}
	if conf.AccountSlots < 1 {
		log.Warn("Sanitizing invalid txpool account slots", "provided", conf.AccountSlots, "updated", DefaultConfig.AccountSlots)
		conf.AccountSlots = DefaultConfig.AccountSlots
	}
	if conf.GlobalSlots < 1 {
		log.Warn("Sanitizing invalid txpool global slots", "provided", conf.GlobalSlots, "updated", DefaultConfig.GlobalSlots)
		conf.GlobalSlots = DefaultConfig.GlobalSlots
	}
	if conf.AccountQueue < 1 {
		log.Warn("Sanitizing invalid txpool account queue", "provided", conf.AccountQueue, "updated", DefaultConfig.AccountQueue)
		conf.AccountQueue = DefaultConfig.AccountQueue
	}
	if conf.GlobalQueue < 1 {
		log.Warn("Sanitizing invalid txpool global queue", "provided", conf.GlobalQueue, "updated", DefaultConfig.GlobalQueue)
		conf.GlobalQueue = DefaultConfig.GlobalQueue
	}
	if conf.ReservedMaxOccupancyPercent < 1 || conf.ReservedMaxOccupancyPercent > 100 {
		log.Warn("Sanitizing invalid reserved occupancy percent", "provided", conf.ReservedMaxOccupancyPercent, "updated", DefaultConfig.ReservedMaxOccupancyPercent)
		conf.ReservedMaxOccupancyPercent = DefaultConfig.ReservedMaxOccupancyPercent
	}
	if conf.Lifetime < 1 {
		log.Warn("Sanitizing invalid txpool lifetime", "provided", conf.Lifetime, "updated", DefaultConfig.Lifetime)
		conf.Lifetime = DefaultConfig.Lifetime
	}
	// Sanitize rebroadcast configuration
	if conf.RebroadcastInterval < 1*time.Second {
		log.Warn("Sanitizing invalid txpool rebroadcast interval", "provided", conf.RebroadcastInterval, "updated", DefaultConfig.RebroadcastInterval)
		conf.RebroadcastInterval = DefaultConfig.RebroadcastInterval
	}
	if conf.RebroadcastMaxAge < conf.RebroadcastInterval {
		log.Warn("Sanitizing invalid txpool rebroadcast max age", "provided", conf.RebroadcastMaxAge, "updated", DefaultConfig.RebroadcastMaxAge)
		conf.RebroadcastMaxAge = DefaultConfig.RebroadcastMaxAge
	}
	if conf.RebroadcastBatchSize < 1 {
		log.Warn("Sanitizing invalid txpool rebroadcast batch size", "provided", conf.RebroadcastBatchSize, "updated", DefaultConfig.RebroadcastBatchSize)
		conf.RebroadcastBatchSize = DefaultConfig.RebroadcastBatchSize
	}
	return conf
}

// LegacyPool contains all currently known transactions. Transactions
// enter the pool when they are received from the network or submitted
// locally. They exit the pool when they are included in the blockchain.
//
// The pool separates processable transactions (which can be applied to the
// current state) and future transactions. Transactions move between those
// two states over time as they are received and processed.
//
// In addition to tracking transactions, the pool also tracks a set of pending SetCode
// authorizations (EIP7702). This helps minimize number of transactions that can be
// trivially churned in the pool. As a standard rule, any account with a deployed
// delegation or an in-flight authorization to deploy a delegation will only be allowed a
// single transaction slot instead of the standard number. This is due to the possibility
// of the account being sweeped by an unrelated account.
//
// Because SetCode transactions can have many authorizations included, we avoid explicitly
// checking their validity to save the state lookup. So long as the encompassing
// transaction is valid, the authorization will be accepted and tracked by the pool. In
// case the pool is tracking a pending / queued transaction from a specific account, it
// will reject new transactions with delegations from that account with standard in-flight
// transactions.
type LegacyPool struct {
	config      Config
	chainconfig *params.ChainConfig
	chain       BlockChain
	gasTip      atomic.Pointer[uint256.Int]
	txFeed      event.Feed
	signer      types.Signer
	mu          sync.RWMutex

	currentHead   atomic.Pointer[types.Header] // Current head of the blockchain
	currentState  *state.StateDB               // Current state in the blockchain head
	pendingNonces *noncer                      // Pending state tracking virtual nonces
	reserver      txpool.Reserver              // Address reserver to ensure exclusivity across subpools

	pending map[common.Address]*list // All currently processable transactions
	queue   *queue
	all     *lookup     // All transactions to allow lookups
	priced  *pricedList // All transactions sorted by price

	reqResetCh      chan *txpoolResetRequest
	reqPromoteCh    chan *accountSet
	queueTxEventCh  chan *types.Transaction
	reorgDoneCh     chan chan struct{}
	reorgShutdownCh chan struct{}  // requests shutdown of scheduleReorgLoop
	wg              sync.WaitGroup // tracks loop, scheduleReorgLoop
	initDoneCh      chan struct{}  // is closed once the pool is initialized (for tests)

	changesSinceReorg int // A counter for how many drops we've performed in-between reorg.

	promoteTxCh chan struct{} // should be used only for tests

	filteredAddrs map[common.Address]struct{} // Map of addresses to filter

	// Reserved-blockspace registry: reader is wired post-Init from the backend;
	// the snapshot is rebuilt at each reset from the new head's state so the
	// per-tx admission path never reads contract state.
	reservedRegistry registryreader.Reader
	reservedSnapshot atomic.Pointer[registryreader.Snapshot]

	// reservedOccupancy is the combined (pending+queued) slot count (see
	// numSlots) currently held by reserved-blockspace senders, guarded by
	// pool.mu like every other pool field. Slot-weighted, not a flat
	// transaction count, to match the same unit reservedOccupancyCap and the
	// pool's own real fullness check (pool.all.Slots()) are measured in —
	// otherwise a large-calldata reserved tx would count as "1" against a
	// slot-denominated cap while actually consuming many slots. Maintained
	// incrementally at each mutation site (Layer 1) and recomputed from
	// scratch once per reorg cycle in reset() (Layer 2) so a missed
	// touchpoint can't drift the figure indefinitely — see
	// reservedOccupancyCap and recomputeReservedOccupancy.
	reservedOccupancy int

	// Rebroadcast tracking
	rebroadcastTxFeed event.Feed                // Feed for stuck transaction events
	lastRebroadcast   map[common.Hash]time.Time // Track last rebroadcast time per tx hash
}

type txpoolResetRequest struct {
	oldHead, newHead *types.Header
}

// New creates a new transaction pool to gather, sort and filter inbound
// transactions from the network.
func New(config Config, chain BlockChain, options ...func(pool *LegacyPool)) *LegacyPool {
	// Sanitize the input to ensure no vulnerable gas prices are set
	config = (&config).sanitize()

	// Create the transaction pool with its initial settings
	signer := types.LatestSigner(chain.Config())
	pool := &LegacyPool{
		config:          config,
		chain:           chain,
		chainconfig:     chain.Config(),
		signer:          signer,
		pending:         make(map[common.Address]*list),
		queue:           newQueue(config, signer),
		all:             newLookup(),
		reqResetCh:      make(chan *txpoolResetRequest),
		reqPromoteCh:    make(chan *accountSet),
		queueTxEventCh:  make(chan *types.Transaction),
		reorgDoneCh:     make(chan chan struct{}),
		reorgShutdownCh: make(chan struct{}),
		initDoneCh:      make(chan struct{}),
		filteredAddrs:   make(map[common.Address]struct{}),
		lastRebroadcast: make(map[common.Hash]time.Time),
	}
	pool.priced = newPricedList(pool.all)
	pool.priced.isReserved = pool.reservedTx

	// Copy pre-loaded filtered addresses
	if config.FilteredAddresses != nil {
		for addr := range config.FilteredAddresses {
			pool.filteredAddrs[addr] = struct{}{}
		}
		log.Info("Loaded filtered addresses", "count", len(pool.filteredAddrs))
	}

	// apply options
	for _, fn := range options {
		fn(pool)
	}

	return pool
}

// Filter returns whether the given transaction can be consumed by the legacy
// pool, specifically, whether it is a Legacy, AccessList or Dynamic transaction.
func (pool *LegacyPool) Filter(tx *types.Transaction) bool {
	switch tx.Type() {
	case types.LegacyTxType, types.AccessListTxType, types.DynamicFeeTxType, types.SetCodeTxType:
		return true
	default:
		return false
	}
}

// Init sets the gas price needed to keep a transaction in the pool and the chain
// head to allow balance / nonce checks. The internal
// goroutines will be spun up and the pool deemed operational afterwards.
func (pool *LegacyPool) Init(gasTip uint64, head *types.Header, reserver txpool.Reserver) error {
	// Set the address reserver to request exclusive access to pooled accounts
	pool.reserver = reserver

	// Set the basic pool parameters
	pool.gasTip.Store(uint256.NewInt(gasTip))

	// Initialize the state with head block, or fallback to empty one in
	// case the head state is not available (might occur when node is not
	// fully synced).
	statedb, err := pool.chain.PostExecState(head)
	if err != nil {
		statedb, err = pool.chain.StateAt(types.EmptyRootHash)
	}
	if err != nil {
		return err
	}
	pool.currentHead.Store(head)
	pool.currentState = statedb
	pool.pendingNonces = newNoncer(statedb)

	pool.wg.Add(1)
	go pool.scheduleReorgLoop()

	pool.wg.Add(1)
	go pool.loop()
	return nil
}

// loop is the transaction pool's main event loop, waiting for and reacting to
// outside blockchain events as well as for various reporting and transaction
// eviction events.
func (pool *LegacyPool) loop() {
	defer pool.wg.Done()

	var (
		prevPending, prevQueued, prevStales int

		// Start the stats reporting and transaction eviction tickers
		report = time.NewTicker(statsReportInterval)
		evict  = time.NewTicker(evictionInterval)
	)
	defer report.Stop()
	defer evict.Stop()

	// Start the rebroadcast ticker if enabled
	var rebroadcast *time.Ticker
	if pool.config.Rebroadcast {
		rebroadcast = time.NewTicker(pool.config.RebroadcastInterval)
		defer rebroadcast.Stop()
	}

	// Notify tests that the init phase is done
	close(pool.initDoneCh)
	for {
		// Use a nil channel for rebroadcast if disabled
		var rebroadcastC <-chan time.Time
		if rebroadcast != nil {
			rebroadcastC = rebroadcast.C
		}

		select {
		// Handle pool shutdown
		case <-pool.reorgShutdownCh:
			return

		// Handle stats reporting ticks
		case <-report.C:
			pool.mu.RLock()
			start := time.Now()
			pending, queued := pool.stats()
			reportTimer.Update(time.Since(start))
			pool.mu.RUnlock()
			stales := int(pool.priced.stales.Load())

			if pending != prevPending || queued != prevQueued || stales != prevStales {
				log.Debug("Transaction pool status report", "executable", pending, "queued", queued, "stales", stales)
				prevPending, prevQueued, prevStales = pending, queued, stales
			}

		// Handle stuck transaction rebroadcast
		case <-rebroadcastC:
			// Use RLock for reading to minimize contention with add()/reorg()
			identifyStart := time.Now()
			pool.mu.RLock()
			stuckTxs := pool.identifyStuckTransactions()
			pool.mu.RUnlock()
			rebroadcastIdentifyTimer.Update(time.Since(identifyStart))

			if len(stuckTxs) > 0 {
				// Brief Lock only to update lastRebroadcast timestamps
				now := time.Now()
				pool.mu.Lock()
				for _, tx := range stuckTxs {
					pool.lastRebroadcast[tx.Hash()] = now
				}
				rebroadcastTrackingGauge.Update(int64(len(pool.lastRebroadcast)))
				pool.mu.Unlock()

				pool.rebroadcastTxFeed.Send(core.StuckTxsEvent{Txs: stuckTxs})
				rebroadcastTxMeter.Mark(int64(len(stuckTxs)))
				log.Debug("Identified stuck transactions for rebroadcast", "count", len(stuckTxs))
			}

		// Handle inactive account transaction eviction
		case <-evict.C:
			pool.mu.Lock()
			start := time.Now()
			for _, hash := range pool.queue.evictList() {
				// Any old enough should be removed
				pool.removeTx(hash, true, true)
			}
			evictTimer.Update(time.Since(start))
			pool.mu.Unlock()
		}
	}
}

// Close terminates the transaction pool.
func (pool *LegacyPool) Close() error {
	// Terminate the pool reorger and return
	close(pool.reorgShutdownCh)
	pool.wg.Wait()

	log.Info("Transaction pool stopped")
	return nil
}

// Reset implements txpool.SubPool, allowing the legacy pool's internal state to be
// kept in sync with the main transaction pool's internal state.
func (pool *LegacyPool) Reset(oldHead, newHead *types.Header) {
	wait := pool.requestReset(oldHead, newHead)
	<-wait
}

// SubscribeTransactions registers a subscription for new transaction events,
// supporting feeding only newly seen or also resurrected transactions.
func (pool *LegacyPool) SubscribeTransactions(ch chan<- core.NewTxsEvent, reorgs bool) event.Subscription {
	// The legacy pool has a very messed up internal shuffling, so it's kind of
	// hard to separate newly discovered transaction from resurrected ones. This
	// is because the new txs are added to the queue, resurrected ones too and
	// reorgs run lazily, so separating the two would need a marker.
	return pool.txFeed.Subscribe(ch)
}

// SubscribeRebroadcastTransactions registers a subscription for stuck transaction
// rebroadcast events.
func (pool *LegacyPool) SubscribeRebroadcastTransactions(ch chan<- core.StuckTxsEvent) event.Subscription {
	return pool.rebroadcastTxFeed.Subscribe(ch)
}

// identifyStuckTransactions identifies pending transactions that may be stuck
// and need rebroadcasting. It returns transactions that:
// - Have been pending longer than RebroadcastInterval but less than RebroadcastMaxAge
// - Have not been rebroadcast recently (within RebroadcastInterval)
// - Are immediately executable (gas price meets current requirements)
//
// Must be called with pool.mu.RLock held (read lock only - does not modify pool state).
func (pool *LegacyPool) identifyStuckTransactions() []*types.Transaction {
	now := time.Now()
	head := pool.currentHead.Load()
	if head == nil {
		return nil
	}

	// Calculate base fee only if London is enabled and header has base fee
	var baseFee *big.Int
	if pool.chainconfig.IsLondon(head.Number) && head.BaseFee != nil {
		baseFee = eip1559.CalcBaseFee(pool.chainconfig, head)
	}
	minTip := pool.gasTip.Load().ToBig()

	var stuckTxs []*types.Transaction

	for _, list := range pool.pending {
		for _, tx := range list.Flatten() {
			hash := tx.Hash()
			age := now.Sub(tx.Time())

			// Skip if too young (hasn't had time to propagate yet)
			if age < pool.config.RebroadcastInterval {
				continue
			}

			// Check rebroadcast history
			lastTime, wasRebroadcast := pool.lastRebroadcast[hash]
			if wasRebroadcast {
				// Skip if recently rebroadcast
				if now.Sub(lastTime) < pool.config.RebroadcastInterval {
					continue
				}
				// Skip if we've been rebroadcasting this tx for too long (max age applies
				// only to previously rebroadcast txs - new txs that just became executable
				// after a base fee drop should still be considered)
				if pool.config.RebroadcastMaxAge > 0 && age > pool.config.RebroadcastMaxAge {
					continue
				}
			}

			// Skip if not immediately executable (gas price too low for current conditions)
			// For EIP-1559 transactions, check gas fee cap against base fee
			if baseFee != nil && tx.GasFeeCap().Cmp(baseFee) < 0 {
				continue
			}
			if tx.GasTipCap().Cmp(minTip) < 0 {
				continue
			}

			stuckTxs = append(stuckTxs, tx)

			// Enforce batch limit
			if len(stuckTxs) >= pool.config.RebroadcastBatchSize {
				return stuckTxs
			}
		}
	}

	return stuckTxs
}

// SetGasTip updates the minimum gas tip required by the transaction pool for a
// new transaction, and drops all transactions below this threshold.
func (pool *LegacyPool) SetGasTip(tip *big.Int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	var (
		newTip = uint256.MustFromBig(tip)
		old    = pool.gasTip.Load()
	)
	pool.gasTip.Store(newTip)
	// If the min miner fee increased, remove transactions below the new threshold
	if newTip.Cmp(old) > 0 {
		// pool.priced is sorted by GasFeeCap, so we have to iterate through pool.all instead
		drop := pool.all.TxsBelowTip(tip)
		for _, tx := range drop {
			pool.removeTx(tx.Hash(), false, true)
		}
		pool.priced.Removed(len(drop))
	}
	log.Info("Legacy pool tip threshold updated", "tip", newTip)
}

// Nonce returns the next nonce of an account, with all transactions executable
// by the pool already applied on top.
func (pool *LegacyPool) Nonce(addr common.Address) uint64 {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.pendingNonces.get(addr)
}

// Stats retrieves the current pool stats, namely the number of pending and the
// number of queued (non-executable) transactions.
func (pool *LegacyPool) Stats() (int, int) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.stats()
}

// stats retrieves the current pool stats, namely the number of pending and the
// number of queued (non-executable) transactions.
func (pool *LegacyPool) stats() (int, int) {
	pending := 0
	for _, list := range pool.pending {
		pending += list.Len()
	}
	return pending, pool.queue.stats()
}

// Content retrieves the data content of the transaction pool, returning all the
// pending as well as queued transactions, grouped by account and sorted by nonce.
func (pool *LegacyPool) Content() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pending := make(map[common.Address][]*types.Transaction, len(pool.pending))
	for addr, list := range pool.pending {
		pending[addr] = list.Flatten()
	}
	queued := pool.queue.content()
	return pending, queued
}

// ContentFrom retrieves the data content of the transaction pool, returning the
// pending as well as queued transactions of this address, grouped by nonce.
func (pool *LegacyPool) ContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	var pending []*types.Transaction
	if list, ok := pool.pending[addr]; ok {
		pending = list.Flatten()
	}
	queued := pool.queue.contentFrom(addr)
	return pending, queued
}

// Pending retrieves all currently processable transactions, grouped by origin
// account and sorted by nonce.
//
// The transactions can also be pre-filtered by the dynamic fee components to
// reduce allocations and load on downstream subsystems. The retrieval is halted
// if interrupt is set (during block building timeout).
func (pool *LegacyPool) Pending(filter txpool.PendingFilter, interrupt *atomic.Bool) map[common.Address][]*txpool.LazyTransaction {
	defer func(t0 time.Time) {
		pendingWaitTimer.Update(time.Since(t0))
	}(time.Now())

	// If only blob transactions are requested, this pool is unsuitable as it
	// contains none, don't even bother.
	if filter.BlobTxs {
		return nil
	}

	// Capture the time taken to acquire the lock
	lockWait := time.Now()
	pool.mu.Lock()
	pendingLockWaitTimer.Update(time.Since(lockWait))
	defer pool.mu.Unlock()

	if interrupt == nil {
		interrupt = new(atomic.Bool)
	}

	pending := make(map[common.Address][]*txpool.LazyTransaction, len(pool.pending))
	for addr, list := range pool.pending {
		// Check for the flag to interrupt block building on timeout.
		if interrupt.Load() {
			// We could send partial set of pending transactions but they'll anyways
			// be rejected during commit transactions. Instead avoid sending anything.
			return map[common.Address][]*txpool.LazyTransaction{}
		}

		txs := list.Flatten()

		// Reserved-blockspace senders pay zero in-protocol fee; their txs must not
		// be capped by the tip floor or they would never reach the miner.
		reserved := pool.isReserved(addr)

		// If the miner requests tip enforcement, cap the lists now
		if filter.MinTip != nil || filter.GasLimitCap != 0 {
			for i, tx := range txs {
				if filter.MinTip != nil && !reserved {
					if tx.EffectiveGasTipIntCmp(filter.MinTip, filter.BaseFee) < 0 {
						txs = txs[:i]
						break
					}
				}
				if filter.GasLimitCap != 0 {
					if tx.Gas() > filter.GasLimitCap {
						txs = txs[:i]
						break
					}
				}
			}
		}
		if len(txs) > 0 {
			lazies := make([]*txpool.LazyTransaction, len(txs))
			for i := 0; i < len(txs); i++ {
				lazies[i] = &txpool.LazyTransaction{
					Pool:      pool,
					Hash:      txs[i].Hash(),
					Tx:        txs[i],
					Time:      txs[i].Time(),
					GasFeeCap: uint256.MustFromBig(txs[i].GasFeeCap()),
					GasTipCap: uint256.MustFromBig(txs[i].GasTipCap()),
					Gas:       txs[i].Gas(),
					BlobGas:   txs[i].BlobGas(),
				}
			}
			pending[addr] = lazies
		}
	}
	return pending
}

// ValidateTxBasics checks whether a transaction is valid according to the consensus
// rules, but does not check state-dependent validation such as sufficient balance.
// This check is meant as an early check which only needs to be performed once,
// and does not require the pool mutex to be held.
func (pool *LegacyPool) ValidateTxBasics(tx *types.Transaction) error {
	opts := &txpool.ValidationOptions{
		Config:              pool.chainconfig,
		AllowUnprotectedTxs: pool.config.AllowUnprotectedTxs,
		Accept: 0 |
			1<<types.LegacyTxType |
			1<<types.AccessListTxType |
			1<<types.DynamicFeeTxType |
			1<<types.SetCodeTxType,
		MaxSize:          txMaxSize,
		MinTip:           pool.gasTip.Load().ToBig(),
		ReservedSnapshot: pool.reservedSnapshot.Load(),
	}
	return txpool.ValidateTransaction(tx, pool.currentHead.Load(), pool.signer, opts)
}

// validateTx checks whether a transaction is valid according to the consensus
// rules and adheres to some heuristic limits of the local node (price and size).
func (pool *LegacyPool) validateTx(tx *types.Transaction) error {
	// Check if transaction sender is filtered
	from, err := types.Sender(pool.signer, tx)
	if err != nil {
		return err
	}

	if pool.isFiltered(from) {
		filteredTxMeter.Mark(1)
		log.Warn("Filtered transaction rejected", "hash", tx.Hash(), "from", from)
		return ErrTxFiltered
	}

	opts := &txpool.ValidationOptionsWithState{
		State: pool.currentState,

		FirstNonceGap:    nil, // Pool allows arbitrary arrival order, don't invalidate nonce gaps
		UsedAndLeftSlots: nil, // Pool has own mechanism to limit the number of transactions
		EffectiveCost:    pool.effectiveCost,
		ExistingExpenditure: func(addr common.Address) *big.Int {
			list := pool.pending[addr]
			if list == nil {
				return new(big.Int)
			}
			// Basis-consistent with EffectiveCost: a reserved sender's queued
			// expenditure is tracked and read on the value basis, same as its
			// incoming tx is priced above.
			if pool.isReserved(addr) {
				return list.totalvalue.ToBig()
			}
			return list.totalcost.ToBig()
		},
		ExistingCost: func(addr common.Address, nonce uint64) *big.Int {
			if list := pool.pending[addr]; list != nil {
				if tx := list.txs.Get(nonce); tx != nil {
					return pool.effectiveCost(addr, tx)
				}
			}
			return nil
		},
	}
	if err := txpool.ValidateTransactionWithState(tx, pool.signer, opts); err != nil {
		return err
	}
	return pool.validateAuth(tx)
}

// checkDelegationLimit determines if the tx sender is delegated or has a
// pending delegation, and if so, ensures they have at most one in-flight
// **executable** transaction, e.g. disallow stacked and gapped transactions
// from the account.
func (pool *LegacyPool) checkDelegationLimit(tx *types.Transaction) error {
	from, _ := types.Sender(pool.signer, tx) // validated

	// Short circuit if the sender has neither delegation nor pending delegation.
	if pool.currentState.GetCodeHash(from) == types.EmptyCodeHash && !pool.all.hasAuth(from) {
		return nil
	}
	pending := pool.pending[from]
	if pending == nil {
		// Transaction with gapped nonce is not supported for delegated accounts
		if pool.pendingNonces.get(from) != tx.Nonce() {
			return ErrOutOfOrderTxFromDelegated
		}
		return nil
	}
	// Transaction replacement is supported
	if pending.Contains(tx.Nonce()) {
		return nil
	}
	return txpool.ErrInflightTxLimitReached
}

// validateAuth verifies that the transaction complies with code authorization
// restrictions brought by SetCode transaction type.
func (pool *LegacyPool) validateAuth(tx *types.Transaction) error {
	// Allow at most one in-flight tx for delegated accounts or those with a
	// pending authorization.
	if err := pool.checkDelegationLimit(tx); err != nil {
		return err
	}
	// For symmetry, allow at most one in-flight tx for any authority with a
	// pending transaction.
	if auths := tx.SetCodeAuthorities(); len(auths) > 0 {
		for _, auth := range auths {
			var count int
			if pending := pool.pending[auth]; pending != nil {
				count += pending.Len()
			}
			if queue, ok := pool.queue.get(auth); ok {
				count += queue.Len()
			}
			if count > 1 {
				return ErrAuthorityReserved
			}
			// Because there is no exclusive lock held between different subpools
			// when processing transactions, the SetCode transaction may be accepted
			// while other transactions with the same sender address are also
			// accepted simultaneously in the other pools.
			//
			// This scenario is considered acceptable, as the rule primarily ensures
			// that attackers cannot easily stack a SetCode transaction when the sender
			// is reserved by other pools.
			if pool.reserver.Has(auth) {
				return ErrAuthorityReserved
			}
		}
	}
	return nil
}

// reportTxAddMetrics updates metrics captured in async tx addition.
func reportTxAddMetrics(stage uint8, stage0Duration, stage1Duration, stage2Duration time.Duration) {
	if stage > 2 {
		return
	}
	// default case in all stages
	if stage0Duration > 0 {
		asyncAddStage0Timer.Update(stage0Duration)
	}
	switch stage {
	case 1:
		if stage1Duration > 0 {
			asyncAddStage1Timer.Update(stage1Duration)
		}
	case 2:
		if stage1Duration > 0 {
			asyncAddStage1Timer.Update(stage1Duration)
		}
		if stage2Duration > 0 {
			asyncAddStage2Timer.Update(stage2Duration)
		}
	}
}

// add validates a transaction and inserts it into the non-executable queue for later
// pending promotion and execution. If the transaction is a replacement for an already
// pending or queued one, it overwrites the previous transaction if its price is higher.
// If async insertion is requested, it frees up the global pool lock to allow other
// functions to use the pending pool. It also performs some operations on the `pricedHeap`
// async to avoid waiting for reheap. The pool lock won't be held when called in async mode.
func (pool *LegacyPool) add(tx *types.Transaction, async bool) (replaced bool, err error) {
	var locked bool = true

	// For sync mode (i.e. async=false), lock is held in outer function so capture the
	// start and end of function directly.
	if !async {
		syncAddTime := time.Now()
		defer func() {
			syncAddTimer.Update(time.Since(syncAddTime))
		}()
	}

	// We split this function into 3 major stages. To avoid unexpected issues, the stages aren't
	// split into separate functions as the only purpose is capturing time spent in each stage.
	// Broadly, this function will either be called in sync mode (async=false) which has the lock
	// acquired for the entire duration, or in async mode (async=true) where lock is acquired
	// initially, released for a specific code path and re-acquired later. Changes needed to track
	// time taken in different code path are bit messy but prevents any behavioral changes.
	// Stage0: Defines the initial validation of the transaction (lock is held)
	// Stage1: Checking against the priced list for tx pricing (lock is released and re-acquired)
	// Stage2: Re-arranging txpool contents for inclusion/exclusion (lock is held)
	var (
		currentStage   uint8 = 0
		stage0Time     time.Time
		stage0Duration time.Duration
		stage1Time     time.Time
		stage1Duration time.Duration
		stage2Time     time.Time
		stage2Duration time.Duration
	)

	// If `async` is set, acquire the global lock.
	if async {
		lockStart := time.Now()
		pool.mu.Lock()
		asyncAddStage0LockWaitTimer.Update(time.Since(lockStart))
		defer func() {
			// Based on which stage the code exits, report metrics accordingly.
			reportTxAddMetrics(currentStage, stage0Duration, stage1Duration, stage2Duration)
			if locked {
				pool.mu.Unlock()
			}
		}()
	}

	// stage0 starts
	stage0Time = time.Now()

	// If the transaction is already known, discard it
	hash := tx.Hash()
	if pool.all.Get(hash) != nil {
		log.Trace("Discarding already known transaction", "hash", hash)
		knownTxMeter.Mark(1)
		stage0Duration = time.Since(stage0Time)
		return false, txpool.ErrAlreadyKnown
	}

	if pool.config.AllowUnprotectedTxs {
		pool.signer = types.NewFakeSigner(tx.ChainId())
	}

	// If the transaction fails basic validation, discard it
	if err := pool.validateTx(tx); err != nil {
		log.Trace("Discarding invalid transaction", "hash", hash, "err", err)
		invalidTxMeter.Mark(1)
		stage0Duration = time.Since(stage0Time)
		return false, err
	}
	// already validated by this point
	from, _ := types.Sender(pool.signer, tx)
	// Resolved once and reused below (the pending-replace branch, the
	// Underpriced exemption, and the post-enqueue occupancy bump): isReserved
	// and a pending map lookup are each cheap individually, but this tx is
	// looked at from several angles below and every one of them would
	// otherwise re-derive the same answer.
	reserved := pool.isReserved(from)
	pendingList := pool.pending[from]

	// Reserved-blockspace occupancy cap: reject outright, before any
	// Discard/eviction attempt, if this sender is reserved and admitting the
	// transaction would push aggregate reserved occupancy over its cap.
	// Weighted by numSlots (not a flat 1 per tx) to match the same
	// slot-based unit the cap itself is computed in (GlobalSlots+
	// GlobalQueue) and the pool's own real fullness check
	// (pool.all.Slots()) already use.
	//
	// A same-nonce replacement is NOT occupancy-neutral under slot
	// weighting the way it was under flat per-transaction counting: a
	// 1-slot incumbent can be replaced by up to a 4-slot (txMaxSize)
	// transaction at the same nonce, so the delta - not just "is this a
	// new nonce" - is what must be checked and later applied (see the two
	// bumpReservedOccupancy call sites below that use this same delta).
	// Runs unconditionally, independent of overall pool fullness - unlike
	// the Underpriced exemption below, which only applies once the pool is
	// already globally full.
	var reservedSlotDelta int
	if reserved {
		reservedSlotDelta = numSlots(tx)
		if old := pool.reservedSlotAt(pendingList, from, tx.Nonce()); old != nil {
			reservedSlotDelta -= numSlots(old)
		}
		if reservedSlotDelta > 0 && pool.reservedOccupancy+reservedSlotDelta > pool.reservedOccupancyCap() {
			stage0Duration = time.Since(stage0Time)
			return false, ErrReservedOccupancyExceeded
		}
	}

	// If the address is not yet known, request exclusivity to track the account
	// only by this subpool until all transactions are evicted
	var (
		hasPending   = pendingList != nil
		_, hasQueued = pool.queue.get(from)
	)
	if !hasPending && !hasQueued {
		if err := pool.reserver.Hold(from); err != nil {
			stage0Duration = time.Since(stage0Time)
			return false, err
		}
		defer func() {
			// If the transaction is rejected by some post-validation check, remove
			// the lock on the reservation set.
			//
			// Note, `err` here is the named error return, which will be initialized
			// by a return statement before running deferred methods. Take care with
			// removing or subscoping err as it will break this clause.
			if err != nil {
				pool.reserver.Release(from)
			}
		}()
	}

	// stage0 ends, update final duration, increment stage
	stage0Duration = time.Since(stage0Time)
	currentStage = 1

	// start stage2 incase stage1 never runs (due to the if below)
	stage2Time = time.Now()

	// If the transaction pool is full, discard underpriced transactions
	if uint64(pool.all.Slots()+numSlots(tx)) > pool.config.GlobalSlots+pool.config.GlobalQueue {
		if async {
			// The call below can take time due to internal lock if reheap is going on. Free up
			// the lock to allow other functions to operate.
			pool.mu.Unlock()
			locked = false
		}

		// stage1 starts
		stage1Time = time.Now()

		// If the new transaction is underpriced, don't accept it. Reserved-blockspace
		// senders pay zero fee, so they always look underpriced; exempt them (they
		// are also protected from eviction in the priced list's Discard).
		if !reserved && pool.priced.Underpriced(tx) {
			log.Trace("Discarding underpriced transaction", "hash", hash, "gasTipCap", tx.GasTipCap(), "gasFeeCap", tx.GasFeeCap())
			underpricedTxMeter.Mark(1)
			stage1Duration = time.Since(stage1Time)
			return false, txpool.ErrUnderpriced
		}

		// We're about to replace a transaction. The reorg does a more thorough
		// analysis of what to remove and how, but it runs async. We don't want to
		// do too many replacements between reorg-runs, so we cap the number of
		// replacements to 25% of the slots
		if pool.changesSinceReorg > int(pool.config.GlobalSlots/4) {
			throttleTxMeter.Mark(1)
			stage1Duration = time.Since(stage1Time)
			return false, ErrTxPoolOverflow
		}

		// New transaction is better than our worse ones, make room for it.
		// If we can't make enough room for new one, abort the operation. Also,
		// take a snapshot of reheap count as we've finished re-arrangements in
		// the priced list.
		drop, success, reheapCount := pool.priced.Discard(pool.all.Slots() - int(pool.config.GlobalSlots+pool.config.GlobalQueue) + numSlots(tx))

		// Special case, we still can't make the room for the new remote one.
		if !success {
			log.Trace("Discarding overflown transaction", "hash", hash)
			overflowedTxMeter.Mark(1)
			stage1Duration = time.Since(stage1Time)
			return false, ErrTxPoolOverflow
		}

		// stage1 ends, update final duration, increment stage
		stage1Duration = time.Since(stage1Time)
		currentStage = 2

		// update stage2 start time
		stage2Time = time.Now()

		if async {
			// We're done operating on the `pricedList`. Acquire the lock again
			// for rest of the operations.
			lockStart := time.Now()
			pool.mu.Lock()
			asyncAddStage2LockWaitTimer.Update(time.Since(lockStart))
			locked = true
		}

		// If the new transaction is a future transaction it should never churn pending transactions
		if pool.isGapped(from, tx) {
			var replacesPending bool
			for _, dropTx := range drop {
				dropSender, _ := types.Sender(pool.signer, dropTx)
				if list := pool.pending[dropSender]; list != nil && list.Contains(dropTx.Nonce()) {
					replacesPending = true
					break
				}
			}
			// Add all transactions back to the priced queue.
			if replacesPending {
				if async {
					// We don't want to get blocked on this due to internal lock, so
					// call the function to insert transactions into the heap async.
					go pool.priced.PutMany(drop, reheapCount)
				} else {
					pool.priced.PutMany(drop, reheapCount)
				}
				log.Trace("Discarding future transaction replacing pending tx", "hash", hash)
				stage2Duration = time.Since(stage2Time)
				return false, ErrFutureReplacePending
			}
		}

		// Kick out the underpriced remote transactions.
		for _, tx := range drop {
			log.Trace("Discarding freshly underpriced transaction", "hash", tx.Hash(), "gasTipCap", tx.GasTipCap(), "gasFeeCap", tx.GasFeeCap())
			underpricedTxMeter.Mark(1)

			sender, _ := types.Sender(pool.signer, tx)
			dropped := pool.removeTx(tx.Hash(), false, sender != from) // Don't unreserve the sender of the tx being added if last from the acc

			pool.changesSinceReorg += dropped
		}
	}

	// increment stage, stage2 time already captured above
	currentStage = 2

	// Try to replace an existing transaction in the pending pool. pendingList
	// (resolved once, above) is the same object pool.pending[from] would
	// still resolve to here: nothing in the full-pool branch above replaces
	// it wholesale, only mutates it in place or deletes the map entry once
	// it's empty, both visible through the reference already held.
	if list := pendingList; list != nil && list.Contains(tx.Nonce()) {
		// Nonce already pending, check if required price bump is met
		inserted, old := list.Add(tx, pool.config.PriceBump)
		if !inserted {
			pendingDiscardMeter.Mark(1)
			stage2Duration = time.Since(stage2Time)
			return false, txpool.ErrReplaceUnderpriced
		}
		// New transaction is better, replace old one
		if old != nil {
			pool.all.Remove(old.Hash())
			if async {
				go pool.priced.Removed(1)
			} else {
				pool.priced.Removed(1)
			}
			pendingReplaceMeter.Mark(1)
			delete(pool.lastRebroadcast, old.Hash())
			// A same-nonce replacement isn't occupancy-neutral once tx and
			// old can differ in size (see the admission-gate comment above);
			// apply the actual delta rather than leaving reservedOccupancy
			// stale for the rest of this transaction's pending lifetime.
			pool.bumpReservedOccupancy(reserved, numSlots(tx)-numSlots(old))
		}
		pool.all.Add(tx)
		reheapCount := pool.priced.reheaps.Load()
		if async {
			// We don't want to get blocked on this due to internal lock, so
			// call the function to insert transactions into the heap async.
			go pool.priced.Put(tx, reheapCount)
		} else {
			pool.priced.Put(tx, reheapCount)
		}
		pool.queueTxEvent(tx)
		log.Trace("Pooled new executable transaction", "hash", hash, "from", from, "to", tx.To())

		// Successful promotion, bump the heartbeat
		pool.queue.bump(from)
		stage2Duration = time.Since(stage2Time)
		return old != nil, nil
	}
	// New transaction isn't replacing a pending one, push into queue
	replaced, err = pool.enqueueTx(hash, tx, true)
	if err != nil {
		stage2Duration = time.Since(stage2Time)
		return false, err
	}
	// A genuinely new queue slot (not a same-nonce queue replacement): the
	// only addAll=true caller of enqueueTx, so the increment belongs here
	// rather than inside enqueueTx, reusing the reserved-ness already
	// resolved above instead of re-deriving the sender and re-checking it.
	if !replaced {
		pool.bumpReservedOccupancy(reserved, numSlots(tx))
	}

	stage2Duration = time.Since(stage2Time)
	log.Trace("Pooled new future transaction", "hash", hash, "from", from, "to", tx.To())
	return replaced, nil
}

// isGapped reports whether the given transaction is immediately executable.
func (pool *LegacyPool) isGapped(from common.Address, tx *types.Transaction) bool {
	// Short circuit if transaction falls within the scope of the pending list
	// or matches the next pending nonce which can be promoted as an executable
	// transaction afterwards. Note, the tx staleness is already checked in
	// 'validateTx' function previously.
	next := pool.pendingNonces.get(from)
	if tx.Nonce() <= next {
		return false
	}
	// The transaction has a nonce gap with pending list, it's only considered
	// as executable if transactions in queue can fill up the nonce gap.
	queue, ok := pool.queue.get(from)
	if !ok {
		return true
	}
	for nonce := next; nonce < tx.Nonce(); nonce++ {
		if !queue.Contains(nonce) {
			return true // txs in queue can't fill up the nonce gap
		}
	}
	return false
}

// reservedSlotAt returns the transaction currently occupying nonce for from,
// across both pending and queued, or nil if the nonce is free. Used to
// compute the actual reserved-occupancy delta a same-nonce replacement would
// apply (numSlots(new)-numSlots(old)) - a same-nonce replacement is NOT
// occupancy-neutral under slot weighting the way it was under flat
// per-transaction counting, so callers can no longer treat "occupies an
// existing nonce" as "no occupancy change". pending is the caller's own
// pool.pending[from] lookup (add's pending-replace branch needs the identical
// lookup moments later, so callers share one rather than each doing their
// own).
func (pool *LegacyPool) reservedSlotAt(pending *list, from common.Address, nonce uint64) *types.Transaction {
	if pending != nil {
		if old := pending.txs.Get(nonce); old != nil {
			return old
		}
	}
	if queued, ok := pool.queue.get(from); ok {
		if old := queued.txs.Get(nonce); old != nil {
			return old
		}
	}
	return nil
}

// reservedOccupancyCap returns the current combined pending+queued occupancy
// ceiling for reserved-blockspace senders: ReservedMaxOccupancyPercent of the
// pool's own combined slot ceiling, so normal senders always keep at least
// the complementary share of the pool.
func (pool *LegacyPool) reservedOccupancyCap() int {
	total := pool.config.GlobalSlots + pool.config.GlobalQueue
	return int(total * pool.config.ReservedMaxOccupancyPercent / 100)
}

// addReservedOccupancy adjusts the combined reserved-occupancy counter by
// delta and keeps its gauge in sync, so every mutation site updates both
// through a single call rather than risking the two drifting apart.
func (pool *LegacyPool) addReservedOccupancy(delta int) {
	pool.reservedOccupancy += delta
	reservedOccupancyGauge.Update(int64(pool.reservedOccupancy))
}

// bumpReservedOccupancy applies delta to the combined reserved-occupancy
// counter if reserved is true, a no-op otherwise. Centralizes the
// isReserved-then-addReservedOccupancy guard repeated at every mutation site;
// callers that already know an address's reserved-ness (typically because
// they needed it for something else moments earlier) pass it directly rather
// than paying for isReserved's atomic load and big.Int allocation again.
func (pool *LegacyPool) bumpReservedOccupancy(reserved bool, delta int) {
	if reserved {
		pool.addReservedOccupancy(delta)
	}
}

// bumpReservedOccupancyForTx is bumpReservedOccupancy(-numSlots(tx)) for a tx
// whose sender and reserved-ness the caller hasn't already resolved, doing
// both once. Only ever used for removals (both call sites drop a tx outright
// from the queue bucket), so there's no direction parameter to get wrong.
func (pool *LegacyPool) bumpReservedOccupancyForTx(tx *types.Transaction) {
	if from, err := types.Sender(pool.signer, tx); err == nil {
		pool.bumpReservedOccupancy(pool.isReserved(from), -numSlots(tx))
	}
}

// enqueueTx inserts a new transaction into the non-executable transaction queue.
//
// Note, this method assumes the pool lock is held!
func (pool *LegacyPool) enqueueTx(hash common.Hash, tx *types.Transaction, addAll bool) (bool, error) {
	replaced, err := pool.queue.add(tx)
	if err != nil {
		return false, err
	}
	if replaced != nil {
		// A same-nonce queue replacement isn't occupancy-neutral once the
		// old and new transactions can differ in slot count (see the
		// admission-gate comment in add()): apply that delta explicitly,
		// resolving the old tx via pool.all before removeTx below removes
		// it from there. removeTx's own queue-branch decrement is a
		// deliberate no-op for this case (its stale-hash guard sees the
		// new tx already occupying the nonce), so this is the only place
		// the delta gets applied - not a double-count.
		if old := pool.all.Get(*replaced); old != nil {
			if from, err := types.Sender(pool.signer, tx); err == nil {
				pool.bumpReservedOccupancy(pool.isReserved(from), numSlots(tx)-numSlots(old))
			}
		}
		pool.removeTx(*replaced, true, true)
	}
	// A genuinely new (non-replacing) queue slot's reserved-occupancy
	// increment is applied by add(), the only addAll=true caller: it already
	// has the sender's reserved-ness in hand, so there's no need to re-derive
	// it here. Internal reshuffles (demoteUnexecutables/removeTx postponing a
	// tx back into the queue) call this with addAll=false and are net-zero
	// for combined occupancy regardless: the tx already counted while pending,
	// and (being a pure reshuffle of the pool's own tx back into the queue,
	// never colliding with a distinct already-queued nonce) never hits the
	// replaced != nil branch above either.
	// If the transaction isn't in lookup set but it's expected to be there,
	// show the error log.
	if pool.all.Get(hash) == nil && !addAll {
		log.Error("Missing transaction in lookup set, please report the issue", "hash", hash)
	}
	if addAll {
		pool.all.Add(tx)
		go pool.priced.Put(tx, pool.priced.reheaps.Load())
	}
	return replaced != nil, nil
}

// promoteTx adds a transaction to the pending (processable) list of transactions
// and returns whether it was inserted or an older was better.
//
// Note, this method assumes the pool lock is held!
func (pool *LegacyPool) promoteTx(addr common.Address, hash common.Hash, tx *types.Transaction) bool {
	// Try to insert the transaction into the pending queue
	if pool.pending[addr] == nil {
		pool.pending[addr] = newList(true)
	}
	list := pool.pending[addr]

	inserted, old := list.Add(tx, pool.config.PriceBump)
	if !inserted {
		// An older transaction was better, discard this. Every promotion
		// candidate came from the queue's readies (see promoteExecutables),
		// so this is a genuine loss for combined occupancy rather than a
		// queue->pending move: the tx already left the queue bucket but never
		// entered the pending one.
		pool.all.Remove(hash)
		pool.priced.Removed(1)
		pendingDiscardMeter.Mark(1)
		delete(pool.lastRebroadcast, hash)
		pool.bumpReservedOccupancy(pool.isReserved(addr), -numSlots(tx))
		return false
	}
	// Otherwise discard any previous transaction and mark this
	if old != nil {
		pool.all.Remove(old.Hash())
		pool.priced.Removed(1)
		pendingReplaceMeter.Mark(1)
		delete(pool.lastRebroadcast, old.Hash())
	} else {
		// Nothing was replaced, bump the pending counter
		pendingGauge.Inc(1)
	}
	// Set the potentially new pending nonce and notify any subsystems of the new tx
	pool.pendingNonces.set(addr, tx.Nonce()+1)

	// Successful promotion, bump the heartbeat
	pool.queue.bump(addr)
	return true
}

// addRemotes enqueues a batch of transactions into the pool if they are valid.
// Full pricing constraints will apply.
//
// This method is used to add transactions from the p2p network and does not wait for pool
// reorganization and internal event propagation.
func (pool *LegacyPool) addRemotes(txs []*types.Transaction) []error {
	return pool.Add(txs, false)
}

// addRemote enqueues a single transaction into the pool if it is valid. This is a convenience
// wrapper around addRemotes.
func (pool *LegacyPool) addRemote(tx *types.Transaction) error {
	return pool.addRemotes([]*types.Transaction{tx})[0]
}

// addRemotesSync is like addRemotes, but waits for pool reorganization. Tests use this method.
func (pool *LegacyPool) addRemotesSync(txs []*types.Transaction) []error {
	return pool.Add(txs, true)
}

// This is like addRemotes with a single transaction, but waits for pool reorganization. Tests use this method.
func (pool *LegacyPool) addRemoteSync(tx *types.Transaction) error {
	return pool.Add([]*types.Transaction{tx}, true)[0]
}

// Add enqueues a batch of transactions into the pool if they are valid.
//
// Note, if sync is set the method will block until all internal maintenance
// related to the add is finished. Only use this during tests for determinism.
func (pool *LegacyPool) Add(txs []*types.Transaction, sync bool) []error {
	// Filter out known ones without obtaining the pool lock or recovering signatures
	var (
		errs = make([]error, len(txs))
		news = make([]*types.Transaction, 0, len(txs))
	)
	for i, tx := range txs {
		// If the transaction is known, pre-set the error slot
		if pool.all.Get(tx.Hash()) != nil {
			errs[i] = txpool.ErrAlreadyKnown
			knownTxMeter.Mark(1)
			continue
		}

		if pool.config.AllowUnprotectedTxs {
			pool.signer = types.NewFakeSigner(tx.ChainId())
		}

		// Exclude transactions with basic errors, e.g invalid signatures and
		// insufficient intrinsic gas as soon as possible and cache senders
		// in transactions before obtaining lock
		if err := pool.ValidateTxBasics(tx); err != nil {
			errs[i] = err
			log.Trace("Discarding invalid transaction", "hash", tx.Hash(), "err", err)
			invalidTxMeter.Mark(1)
			continue
		}
		// Accumulate all unknown transactions for deeper processing
		news = append(news, tx)
	}
	if len(news) == 0 {
		return errs
	}

	// Process all the new transaction and merge any errors into the original slice. Avoid
	// locking here as we'll use the global lock optimally.
	newErrs, dirtyAddrs := pool.addTxs(news, true)

	nilSlot := 0
	for _, err := range newErrs {
		for errs[nilSlot] != nil {
			nilSlot++
		}
		errs[nilSlot] = err
		nilSlot++
	}
	// Reorg the pool internals if needed and return
	done := pool.requestPromoteExecutables(dirtyAddrs)
	if sync {
		<-done
	}
	return errs
}

// addTxs attempts to queue a batch of transactions if they are valid.
// The transaction pool lock must not be held.
func (pool *LegacyPool) addTxs(txs []*types.Transaction, async bool) ([]error, *accountSet) {
	var (
		dirty = newAccountSet(pool.signer)
		errs  = make([]error, len(txs))
		valid int64
	)
	for i, tx := range txs {
		replaced, err := pool.add(tx, async)
		errs[i] = err
		if err == nil {
			if !replaced {
				dirty.addTx(tx)
			}
			valid++
		}
	}
	validTxMeter.Mark(valid)
	return errs, dirty
}

// Status returns the status (unknown/pending/queued) of a batch of transactions
// identified by their hashes.
func (pool *LegacyPool) Status(hash common.Hash) txpool.TxStatus {
	tx := pool.get(hash)
	if tx == nil {
		return txpool.TxStatusUnknown
	}
	from, _ := types.Sender(pool.signer, tx) // already validated

	pool.mu.RLock()
	defer pool.mu.RUnlock()

	if txList := pool.pending[from]; txList != nil && txList.txs.items[tx.Nonce()] != nil {
		return txpool.TxStatusPending
	} else if txList, ok := pool.queue.get(from); ok && txList.txs.items[tx.Nonce()] != nil {
		return txpool.TxStatusQueued
	}
	return txpool.TxStatusUnknown
}

// Get returns a transaction if it is contained in the pool and nil otherwise.
func (pool *LegacyPool) Get(hash common.Hash) *types.Transaction {
	tx := pool.get(hash)
	if tx == nil {
		return nil
	}
	return tx
}

// get returns a transaction if it is contained in the pool and nil otherwise.
func (pool *LegacyPool) get(hash common.Hash) *types.Transaction {
	return pool.all.Get(hash)
}

// GetRLP returns a RLP-encoded transaction if it is contained in the pool.
func (pool *LegacyPool) GetRLP(hash common.Hash) []byte {
	tx := pool.all.Get(hash)
	if tx == nil {
		return nil
	}
	encoded, err := rlp.EncodeToBytes(tx)
	if err != nil {
		log.Error("Failed to encoded transaction in legacy pool", "hash", hash, "err", err)
		return nil
	}
	return encoded
}

// GetMetadata returns the transaction type and transaction size with the
// given transaction hash.
func (pool *LegacyPool) GetMetadata(hash common.Hash) *txpool.TxMetadata {
	tx := pool.all.Get(hash)
	if tx == nil {
		return nil
	}
	return &txpool.TxMetadata{
		Type: tx.Type(),
		Size: tx.Size(),
	}
}

// Has returns an indicator whether txpool has a transaction cached with the
// given hash.
func (pool *LegacyPool) Has(hash common.Hash) bool {
	return pool.all.Get(hash) != nil
}

// removeTx removes a single transaction from the queue, moving all subsequent
// transactions back to the future queue.
//
// In unreserve is false, the account will not be relinquished to the main txpool
// even if there are no more references to it. This is used to handle a race when
// a tx being added, and it evicts a previously scheduled tx from the same account,
// which could lead to a premature release of the lock.
//
// Returns the number of transactions removed from the pending queue.
func (pool *LegacyPool) removeTx(hash common.Hash, outofbound bool, unreserve bool) int {
	// Fetch the transaction we wish to delete
	tx := pool.all.Get(hash)
	if tx == nil {
		return 0
	}
	addr, _ := types.Sender(pool.signer, tx) // already validated during insertion
	// Resolved once and reused below for whichever of the pending/queue
	// branches turns out to be the genuine removal.
	reserved := pool.isReserved(addr)

	// If after deletion there are no more transactions belonging to this account,
	// relinquish the address reservation. It's a bit convoluted do this, via a
	// defer, but it's safer vs. the many return pathways.
	if unreserve {
		defer func() {
			var (
				_, hasPending = pool.pending[addr]
				_, hasQueued  = pool.queue.get(addr)
			)
			if !hasPending && !hasQueued {
				pool.reserver.Release(addr)
			}
		}()
	}
	// Remove it from the list of known transactions
	pool.all.Remove(hash)
	if outofbound {
		pool.priced.Removed(1)
	}
	// Clean up rebroadcast tracking
	delete(pool.lastRebroadcast, hash)
	// Remove the transaction from the pending lists and reset the account nonce
	if pending := pool.pending[addr]; pending != nil {
		if removed, invalids := pending.Remove(tx); removed {
			// If no more pending transactions are left, remove the list
			if pending.Empty() {
				delete(pool.pending, addr)
			}
			// Postpone any invalidated transactions
			for _, tx := range invalids {
				// Internal shuffle shouldn't touch the lookup set.
				pool.enqueueTx(tx.Hash(), tx, false)
			}
			// Update the account nonce if needed
			pool.pendingNonces.setIfLower(addr, tx.Nonce())
			// Reduce the pending counter
			pendingGauge.Dec(int64(1 + len(invalids)))
			// Only the target tx is a genuine loss for combined occupancy;
			// each invalid was just re-enqueued above (pending->queue, net
			// zero), matching enqueueTx's addAll=false handling.
			pool.bumpReservedOccupancy(reserved, -numSlots(tx))
			return 1 + len(invalids)
		}
	}
	// Transaction is in the future queue. Mirror queue.remove's own
	// stale-hash guard: only a tx that's actually still at this nonce (not
	// already superseded by a replacement) is a genuine removal.
	if list, ok := pool.queue.get(addr); ok {
		if existing := list.txs.Get(tx.Nonce()); existing != nil && existing.Hash() == tx.Hash() {
			pool.bumpReservedOccupancy(reserved, -numSlots(tx))
		}
	}
	pool.queue.remove(addr, tx)
	return 0
}

// requestReset requests a pool reset to the new head block.
// The returned channel is closed when the reset has occurred.
func (pool *LegacyPool) requestReset(oldHead *types.Header, newHead *types.Header) chan struct{} {
	select {
	case pool.reqResetCh <- &txpoolResetRequest{oldHead, newHead}:
		return <-pool.reorgDoneCh
	case <-pool.reorgShutdownCh:
		return pool.reorgShutdownCh
	}
}

// requestPromoteExecutables requests transaction promotion checks for the given addresses.
// The returned channel is closed when the promotion checks have occurred.
func (pool *LegacyPool) requestPromoteExecutables(set *accountSet) chan struct{} {
	select {
	case pool.reqPromoteCh <- set:
		return <-pool.reorgDoneCh
	case <-pool.reorgShutdownCh:
		return pool.reorgShutdownCh
	}
}

// queueTxEvent enqueues a transaction event to be sent in the next reorg run.
func (pool *LegacyPool) queueTxEvent(tx *types.Transaction) {
	select {
	case pool.queueTxEventCh <- tx:
	case <-pool.reorgShutdownCh:
	}
}

// scheduleReorgLoop schedules runs of reset and promoteExecutables. Code above should not
// call those methods directly, but request them being run using requestReset and
// requestPromoteExecutables instead.
func (pool *LegacyPool) scheduleReorgLoop() {
	defer pool.wg.Done()

	var (
		curDone       chan struct{} // non-nil while runReorg is active
		nextDone      = make(chan struct{})
		launchNextRun bool
		reset         *txpoolResetRequest
		dirtyAccounts *accountSet
		queuedEvents  = make(map[common.Address]*SortedMap)
	)
	for {
		// Launch next background reorg if needed
		if curDone == nil && launchNextRun {
			// Run the background reorg and announcements
			go pool.runReorg(nextDone, reset, dirtyAccounts, queuedEvents)

			// Prepare everything for the next round of reorg
			curDone, nextDone = nextDone, make(chan struct{})
			launchNextRun = false

			reset, dirtyAccounts = nil, nil
			queuedEvents = make(map[common.Address]*SortedMap)
		}

		select {
		case req := <-pool.reqResetCh:
			// Reset request: update head if request is already pending.
			if reset == nil {
				reset = req
			} else {
				reset.newHead = req.newHead
			}
			launchNextRun = true
			pool.reorgDoneCh <- nextDone

		case req := <-pool.reqPromoteCh:
			// Promote request: update address set if request is already pending.
			if dirtyAccounts == nil {
				dirtyAccounts = req
			} else {
				dirtyAccounts.merge(req)
			}
			launchNextRun = true
			pool.reorgDoneCh <- nextDone

		case tx := <-pool.queueTxEventCh:
			// Queue up the event, but don't schedule a reorg. It's up to the caller to
			// request one later if they want the events sent.
			addr, _ := types.Sender(pool.signer, tx)
			if _, ok := queuedEvents[addr]; !ok {
				queuedEvents[addr] = NewSortedMap()
			}
			queuedEvents[addr].Put(tx)

		case <-curDone:
			curDone = nil

		case <-pool.reorgShutdownCh:
			// Wait for current run to finish.
			if curDone != nil {
				<-curDone
			}
			close(nextDone)
			return
		}
	}
}

// runReorg runs reset and promoteExecutables on behalf of scheduleReorgLoop.
func (pool *LegacyPool) runReorg(done chan struct{}, reset *txpoolResetRequest, dirtyAccounts *accountSet, events map[common.Address]*SortedMap) {
	defer func(t0 time.Time) {
		reorgDurationTimer.Update(time.Since(t0))
	}(time.Now())
	defer close(done)

	var promoteAddrs []common.Address
	if dirtyAccounts != nil && reset == nil {
		// Only dirty accounts need to be promoted, unless we're resetting.
		// For resets, all addresses in the tx queue will be promoted and
		// the flatten operation can be avoided.
		promoteAddrs = dirtyAccounts.flatten()
	}
	lockTime := time.Now()
	pool.mu.Lock()
	if reset != nil {
		if reset.newHead != nil && reset.oldHead != nil {
			// Bor: EIP-7825 at Madhugiri HF block
			isOsaka := pool.chainconfig.IsOsaka(reset.newHead.Number) && !pool.chainconfig.IsOsaka(reset.oldHead.Number)
			isMadhugiri := pool.chainconfig.Bor != nil && (pool.chainconfig.Bor.IsMadhugiri(reset.newHead.Number) && !pool.chainconfig.Bor.IsMadhugiri(reset.oldHead.Number))
			// Discard the transactions with the gas limit higher than the cap.
			if isOsaka || isMadhugiri {
				var hashes []common.Hash
				pool.all.Range(func(hash common.Hash, tx *types.Transaction) bool {
					if tx.Gas() > params.MaxTxGas {
						hashes = append(hashes, hash)
					}
					return true
				})
				for _, hash := range hashes {
					pool.removeTx(hash, true, true)
				}
			}
		}
		// Reset from the old head to the new, rescheduling any reorged transactions
		pool.reset(reset.oldHead, reset.newHead)

		// Nonces were reset, discard any events that became stale
		for addr := range events {
			events[addr].Forward(pool.pendingNonces.get(addr))
			if events[addr].Len() == 0 {
				delete(events, addr)
			}
		}
		// Reset needs promote for all addresses
		promoteAddrs = pool.queue.addresses()
	}
	// Check for pending transactions for every account that sent new ones
	promoted := pool.promoteExecutables(promoteAddrs)

	// If a new block appeared, validate the pool of pending transactions. This will
	// remove any transaction that has been included in the block or was invalidated
	// because of another transaction (e.g. higher gas price).
	if reset != nil {
		pool.demoteUnexecutables()
	}

	// Bor: TxPool in order to maintain strict consistency acquires a lock for
	// the entire call. To reduce the time lock is acquired, we do some truncation
	// operations first, release the lock, and then do reheap because of change
	// in base fee. This gives `Pending()` waiting for the lock a priority than
	// internal rearrangement.

	// Ensure pool.queue and pool.pending sizes stay within the configured limits.
	pool.truncatePending()
	pool.truncateQueue()
	// Defense-in-depth backstop for the reserved-occupancy cap; see
	// truncateReservedOccupancy. Ordered after both truncations above since it
	// spans both buckets and is a no-op unless something upstream let the
	// combined figure drift over the (possibly newly-lowered) cap.
	pool.truncateReservedOccupancy()

	// Update metrics
	dropBetweenReorgHistogram.Update(int64(pool.changesSinceReorg))
	pool.changesSinceReorg = 0 // Reset change counter

	pool.mu.Unlock()
	reorgLockDurationTimer.Update(time.Since(lockTime))

	// Reheap if needed
	if reset != nil {
		if reset.newHead != nil {
			if pool.chainconfig.IsLondon(new(big.Int).Add(reset.newHead.Number, big.NewInt(1))) {
				pendingBaseFee := eip1559.CalcBaseFee(pool.chainconfig, reset.newHead)
				reheapDueToBasefeeCounter.Inc(1)
				pool.priced.SetBaseFee(pendingBaseFee)
			} else {
				pool.priced.Reheap()
			}
		}
	}

	// Notify subsystems for newly added transactions
	for _, tx := range promoted {
		addr, _ := types.Sender(pool.signer, tx)
		if _, ok := events[addr]; !ok {
			events[addr] = NewSortedMap()
		}
		events[addr].Put(tx)
	}
	if len(events) > 0 {
		var txs []*types.Transaction
		for _, set := range events {
			txs = append(txs, set.Flatten()...)
		}
		pool.txFeed.Send(core.NewTxsEvent{Txs: txs})
	}
}

// reset retrieves the current state of the blockchain and ensures the content
// of the transaction pool is valid with regard to the chain state.
func (pool *LegacyPool) reset(oldHead, newHead *types.Header) {
	// If we're reorging an old state, reinject all dropped transactions
	var reinject types.Transactions

	if oldHead != nil && oldHead.Hash() != newHead.ParentHash {
		// If the reorg is too deep, avoid doing it (will happen during fast sync)
		oldNum := oldHead.Number.Uint64()
		newNum := newHead.Number.Uint64()

		if depth := uint64(math.Abs(float64(oldNum) - float64(newNum))); depth > 64 {
			log.Debug("Skipping deep transaction reorg", "depth", depth)
		} else {
			// Reorg seems shallow enough to pull in all transactions into memory
			var (
				rem = pool.chain.GetBlock(oldHead.Hash(), oldHead.Number.Uint64())
				add = pool.chain.GetBlock(newHead.Hash(), newHead.Number.Uint64())
			)
			if rem == nil {
				// This can happen if a setHead is performed, where we simply discard the old
				// head from the chain.
				// If that is the case, we don't have the lost transactions anymore, and
				// there's nothing to add
				if newNum >= oldNum {
					// If we reorged to a same or higher number, then it's not a case of setHead
					log.Warn("Transaction pool reset with missing old head",
						"old", oldHead.Hash(), "oldnum", oldNum, "new", newHead.Hash(), "newnum", newNum)
					return
				}
				// If the reorg ended up on a lower number, it's indicative of setHead being the cause
				log.Debug("Skipping transaction reset caused by setHead",
					"old", oldHead.Hash(), "oldnum", oldNum, "new", newHead.Hash(), "newnum", newNum)
				// We still need to update the current state s.th. the lost transactions can be readded by the user
			} else {
				if add == nil {
					// if the new head is nil, it means that something happened between
					// the firing of newhead-event and _now_: most likely a
					// reorg caused by sync-reversion or explicit sethead back to an
					// earlier block.
					log.Warn("Transaction pool reset with missing new head", "number", newHead.Number, "hash", newHead.Hash())
					return
				}
				var discarded, included types.Transactions
				for rem.NumberU64() > add.NumberU64() {
					discarded = append(discarded, rem.Transactions()...)
					if rem = pool.chain.GetBlock(rem.ParentHash(), rem.NumberU64()-1); rem == nil {
						log.Error("Unrooted old chain seen by tx pool", "block", oldHead.Number, "hash", oldHead.Hash())
						return
					}
				}
				for add.NumberU64() > rem.NumberU64() {
					included = append(included, add.Transactions()...)
					if add = pool.chain.GetBlock(add.ParentHash(), add.NumberU64()-1); add == nil {
						log.Error("Unrooted new chain seen by tx pool", "block", newHead.Number, "hash", newHead.Hash())
						return
					}
				}
				for rem.Hash() != add.Hash() {
					discarded = append(discarded, rem.Transactions()...)
					if rem = pool.chain.GetBlock(rem.ParentHash(), rem.NumberU64()-1); rem == nil {
						log.Error("Unrooted old chain seen by tx pool", "block", oldHead.Number, "hash", oldHead.Hash())
						return
					}
					included = append(included, add.Transactions()...)
					if add = pool.chain.GetBlock(add.ParentHash(), add.NumberU64()-1); add == nil {
						log.Error("Unrooted new chain seen by tx pool", "block", newHead.Number, "hash", newHead.Hash())
						return
					}
				}
				lost := make([]*types.Transaction, 0, len(discarded))
				for _, tx := range types.TxDifference(discarded, included) {
					if pool.Filter(tx) {
						lost = append(lost, tx)
					}
				}
				reinject = lost
			}
		}
	}
	// Initialize the internal state to the current head
	if newHead == nil {
		newHead = pool.chain.CurrentBlock() // Special case during testing
	}
	statedb, err := pool.chain.PostExecState(newHead)
	if err != nil {
		log.Error("Failed to reset txpool state", "err", err)
		return
	}
	pool.currentHead.Store(newHead)
	pool.currentState = statedb
	pool.pendingNonces = newNoncer(statedb)

	// Refresh the reserved-set snapshot from the new head's state so per-tx
	// admission classifies without a contract read.
	pool.rebuildReservedSnapshot(statedb, newHead)

	// Inject any transactions discarded due to reorgs
	log.Debug("Reinjecting stale transactions", "count", len(reinject))
	core.SenderCacher().Recover(pool.signer, reinject)

	// Add transactions synchronously as we're already holding the lock
	pool.addTxs(reinject, false)

	// Layer 2 of the reserved-occupancy counter: recompute it from scratch
	// once per reorg cycle. This is the self-correcting anchor for the
	// incremental Layer-1 updates applied at each mutation site above — any
	// drift from a missed touchpoint cannot accumulate past one reorg cycle.
	pool.reconcileReservedOccupancy()
}

// SetSpeculativeState updates the pool's internal state to reflect a new
// block that hasn't been written to the chain yet. This is used by pipelined
// SRC: after block N's transactions are executed but before block N is sealed,
// the miner calls this to update the txpool so that speculative execution of
// block N+1 gets correct pending transactions (with block N's nonces/balances).
//
// Unlike the full reset() path, this does NOT walk the chain for included/
// discarded transactions (the block isn't in the chain DB). It only:
//  1. Updates currentState and pendingNonces from the provided statedb
//  2. Sets currentHead to the new header
//  3. Demotes transactions with stale nonces
//  4. Promotes newly executable transactions
func (pool *LegacyPool) SetSpeculativeState(newHead *types.Header, statedb *state.StateDB) {
	pool.mu.Lock()

	pool.currentHead.Store(newHead)
	pool.currentState = statedb
	pool.pendingNonces = newNoncer(statedb)

	// Demote transactions that are no longer valid with the new nonces
	pool.demoteUnexecutables()

	// Promote transactions that are now executable
	promoted := pool.promoteExecutables(nil)
	pool.mu.Unlock()

	// Fire events for promoted transactions after releasing the pool lock,
	// matching the regular promotion flow — a subscriber calling back into
	// the pool must not deadlock, and Send on a hot lock is contention.
	if len(promoted) > 0 {
		pool.txFeed.Send(core.NewTxsEvent{Txs: promoted})
	}
}

// promoteExecutables moves transactions that have become processable from the
// future queue to the set of pending transactions. During this process, all
// invalidated transactions (low nonce, low balance) are deleted.
func (pool *LegacyPool) promoteExecutables(accounts []common.Address) []*types.Transaction {
	gasLimit := pool.currentHead.Load().GasLimit
	promotable, dropped, removedAddresses := pool.queue.promoteExecutables(accounts, gasLimit, pool.currentState, pool.pendingNonces, pool.isReserved)

	// promote all promotable transactions
	promoted := make([]*types.Transaction, 0, len(promotable))
	for _, tx := range promotable {
		from, _ := pool.signer.Sender(tx)
		if pool.promoteTx(from, tx.Hash(), tx) {
			promoted = append(promoted, tx)
		}
	}

	// remove all removable transactions. dropped merges the queue's forwards
	// (stale nonce), drops (unpayable/over gas limit) and caps (over
	// AccountQueue) — every one of these is an outright removal from the
	// queue bucket, never a move, so each reserved-sender hash here is a
	// genuine -1. Resolve sender before removing from pool.all.
	for _, hash := range dropped {
		if tx := pool.all.Get(hash); tx != nil {
			pool.bumpReservedOccupancyForTx(tx)
		}
		pool.all.Remove(hash)
		delete(pool.lastRebroadcast, hash)
	}
	pool.priced.Removed(len(dropped))

	// release all accounts that have no more transactions in the pool
	for _, addr := range removedAddresses {
		_, hasPending := pool.pending[addr]
		if !hasPending {
			pool.reserver.Release(addr)
		}
	}
	return promoted
}

// truncatePending removes transactions from the pending queue if the pool is above the
// pending limit. The algorithm tries to reduce transaction counts by an approximately
// equal number for all for accounts with many pending transactions.
func (pool *LegacyPool) truncatePending() {
	pending := uint64(0)

	// Assemble a spam order to penalize large transactors first
	spammers := prque.New[uint64, common.Address](nil)
	for addr, list := range pool.pending {
		// Only evict transactions from high rollers
		length := uint64(list.Len())
		pending += length
		if length > pool.config.AccountSlots {
			spammers.Push(addr, length)
		}
	}
	if pending <= pool.config.GlobalSlots {
		return
	}
	pendingBeforeCap := pending

	// Gradually drop transactions from offenders
	offenders := []common.Address{}
	for pending > pool.config.GlobalSlots && !spammers.Empty() {
		// Retrieve the next offender
		offender, _ := spammers.Pop()
		offenders = append(offenders, offender)

		// Equalize balances until all the same or below threshold
		if len(offenders) > 1 {
			// Calculate the equalization threshold for all current offenders
			threshold := pool.pending[offender].Len()

			// Iteratively reduce all offenders until below limit or threshold reached
			for pending > pool.config.GlobalSlots && pool.pending[offenders[len(offenders)-2]].Len() > threshold {
				for i := 0; i < len(offenders)-1; i++ {
					list := pool.pending[offenders[i]]

					caps := list.Cap(list.Len() - 1)
					for _, tx := range caps {
						// Drop the transaction from the global pools too
						hash := tx.Hash()
						pool.all.Remove(hash)
						delete(pool.lastRebroadcast, hash)

						// Update the account nonce to the dropped transaction
						pool.pendingNonces.setIfLower(offenders[i], tx.Nonce())
						log.Trace("Removed fairness-exceeding pending transaction", "hash", hash)
					}
					pool.priced.Removed(len(caps))
					pendingGauge.Dec(int64(len(caps)))
					pool.bumpReservedOccupancy(pool.isReserved(offenders[i]), -slotsOf(caps))

					pending--
				}
			}
		}
	}

	// If still above threshold, reduce to limit or min allowance
	if pending > pool.config.GlobalSlots && len(offenders) > 0 {
		for pending > pool.config.GlobalSlots && uint64(pool.pending[offenders[len(offenders)-1]].Len()) > pool.config.AccountSlots {
			for _, addr := range offenders {
				list := pool.pending[addr]

				caps := list.Cap(list.Len() - 1)
				for _, tx := range caps {
					// Drop the transaction from the global pools too
					hash := tx.Hash()
					pool.all.Remove(hash)
					delete(pool.lastRebroadcast, hash)

					// Update the account nonce to the dropped transaction
					pool.pendingNonces.setIfLower(addr, tx.Nonce())
					log.Trace("Removed fairness-exceeding pending transaction", "hash", hash)
				}
				pool.priced.Removed(len(caps))
				pendingGauge.Dec(int64(len(caps)))
				pool.bumpReservedOccupancy(pool.isReserved(addr), -slotsOf(caps))
				pending--
			}
		}
	}
	pendingRateLimitMeter.Mark(int64(pendingBeforeCap - pending))
}

// truncateQueue drops the oldest transactions in the queue if the pool is above the global queue limit.
func (pool *LegacyPool) truncateQueue() {
	removed, removedAddresses := pool.queue.truncate()

	// Remove all removable transactions from the lookup and global price list.
	// Resolve each hash's sender before removing it from pool.all, so reserved
	// drops can be attributed even for accounts that were only partially
	// truncated (and so don't appear in removedAddresses).
	for _, hash := range removed {
		if tx := pool.all.Get(hash); tx != nil {
			pool.bumpReservedOccupancyForTx(tx)
		}
		pool.all.Remove(hash)
		delete(pool.lastRebroadcast, hash)
	}
	pool.priced.Removed(len(removed))

	for _, addr := range removedAddresses {
		_, hasPending := pool.pending[addr]
		if !hasPending {
			pool.reserver.Release(addr)
		}
	}
}

// demoteUnexecutables removes invalid and processed transactions from the pools
// executable/pending queue and any subsequent transactions that become unexecutable
// are moved back into the future queue.
//
// Note: transactions are not marked as removed in the priced list because re-heaping
// is always explicitly triggered by SetBaseFee and it would be unnecessary and wasteful
// to trigger a re-heap is this function
func (pool *LegacyPool) demoteUnexecutables() {
	nonces := make(map[common.Address]uint64, len(pool.pending))

	// Iterate over all accounts and demote any non-executable transactions
	currentHeader := pool.currentHead.Load()
	gasLimit := currentHeader.GasLimit
	for addr, list := range pool.pending {
		nonce := pool.currentState.GetNonce(addr)
		reserved := pool.isReserved(addr)

		// Drop all transactions that are deemed too old (low nonce)
		olds := list.Forward(nonce)
		for _, tx := range olds {
			hash := tx.Hash()
			pool.all.Remove(hash)
			delete(pool.lastRebroadcast, hash)
			log.Trace("Removed old pending transaction", "hash", hash)
		}
		// Drop all transactions that are too costly (low balance or out of gas), and queue any invalids back for later
		drops, invalids := list.Filter(pool.currentState.GetBalance(addr), gasLimit, reserved)
		for _, tx := range drops {
			hash := tx.Hash()
			pool.all.Remove(hash)
			delete(pool.lastRebroadcast, hash)
			log.Trace("Removed unpayable pending transaction", "hash", hash)
		}
		pendingNofundsMeter.Mark(int64(len(drops)))

		for _, tx := range invalids {
			hash := tx.Hash()
			log.Trace("Demoting pending transaction", "hash", hash)

			// Internal shuffle shouldn't touch the lookup set.
			pool.enqueueTx(hash, tx, false)
		}
		// bor: Drop all transactions that no longer have valid TxOptions
		txConditionalsRemoved := list.FilterTxConditional(pool.currentState, currentHeader)

		for _, tx := range txConditionalsRemoved {
			hash := tx.Hash()
			pool.all.Remove(hash)
			delete(pool.lastRebroadcast, hash)
			log.Trace("Removed invalid conditional transaction", "hash", hash)
		}

		pendingGauge.Dec(int64(len(olds) + len(drops) + len(invalids) + len(txConditionalsRemoved)))
		// invalids move to the queue (enqueueTx above, addAll=false) and net
		// zero for combined occupancy; only outright removals are a loss,
		// weighted by slots like every other mutation site.
		pool.bumpReservedOccupancy(reserved, -(slotsOf(olds) + slotsOf(drops) + slotsOf(txConditionalsRemoved)))
		// If there's a gap in front, alert (should never happen) and postpone all transactions
		if list.Len() > 0 && list.txs.Get(nonce) == nil {
			gapped := list.Cap(0)
			for _, tx := range gapped {
				hash := tx.Hash()
				log.Warn("Demoting invalidated transaction", "hash", hash)

				// Internal shuffle shouldn't touch the lookup set.
				pool.enqueueTx(hash, tx, false)
			}
			pendingGauge.Dec(int64(len(gapped)))
		}
		if list.Empty() {
			// Delete the entire pending entry if it became empty.
			delete(pool.pending, addr)
			if _, ok := pool.queue.get(addr); !ok {
				pool.reserver.Release(addr)
			}
		} else {
			// Update the latest known pending nonce
			highestPending := list.LastElement()
			nonces[addr] = highestPending.Nonce() + 1
			pool.pendingNonces.setAll(nonces)
		}
	}
}

// accountSet is simply a set of addresses to check for existence, and a signer
// capable of deriving addresses from transactions.
type accountSet struct {
	accounts map[common.Address]struct{}
	signer   types.Signer
	cache    []common.Address
}

// newAccountSet creates a new address set with an associated signer for sender
// derivations.
func newAccountSet(signer types.Signer, addrs ...common.Address) *accountSet {
	as := &accountSet{
		accounts: make(map[common.Address]struct{}, len(addrs)),
		signer:   signer,
	}
	for _, addr := range addrs {
		as.add(addr)
	}
	return as
}

// add inserts a new address into the set to track.
func (as *accountSet) add(addr common.Address) {
	as.accounts[addr] = struct{}{}
	as.cache = nil
}

// addTx adds the sender of tx into the set.
func (as *accountSet) addTx(tx *types.Transaction) {
	if addr, err := types.Sender(as.signer, tx); err == nil {
		as.add(addr)
	}
}

// flatten returns the list of addresses within this set, also caching it for later
// reuse. The returned slice should not be changed!
func (as *accountSet) flatten() []common.Address {
	if as.cache == nil {
		as.cache = slices.Collect(maps.Keys(as.accounts))
	}
	return as.cache
}

// merge adds all addresses from the 'other' set into 'as'.
func (as *accountSet) merge(other *accountSet) {
	maps.Copy(as.accounts, other.accounts)
	as.cache = nil
}

// lookup is used internally by LegacyPool to track transactions while allowing
// lookup without mutex contention.
//
// Note, although this type is properly protected against concurrent access, it
// is **not** a type that should ever be mutated or even exposed outside of the
// transaction pool, since its internal state is tightly coupled with the pools
// internal mechanisms. The sole purpose of the type is to permit out-of-bound
// peeking into the pool in LegacyPool.Get without having to acquire the widely scoped
// LegacyPool.mu mutex.
type lookup struct {
	slots int
	lock  sync.RWMutex
	txs   map[common.Hash]*types.Transaction

	auths map[common.Address][]common.Hash // All accounts with a pooled authorization
}

// newLookup returns a new lookup structure.
func newLookup() *lookup {
	return &lookup{
		txs:   make(map[common.Hash]*types.Transaction),
		auths: make(map[common.Address][]common.Hash),
	}
}

// Range calls f on each key and value present in the map. The callback passed
// should return the indicator whether the iteration needs to be continued.
// Callers need to specify which set (or both) to be iterated.
func (t *lookup) Range(f func(hash common.Hash, tx *types.Transaction) bool) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	for key, value := range t.txs {
		if !f(key, value) {
			return
		}
	}
}

// Get returns a transaction if it exists in the lookup, or nil if not found.
func (t *lookup) Get(hash common.Hash) *types.Transaction {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.txs[hash]
}

// Count returns the current number of transactions in the lookup.
func (t *lookup) Count() int {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return len(t.txs)
}

// Slots returns the current number of slots used in the lookup.
func (t *lookup) Slots() int {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.slots
}

// Add adds a transaction to the lookup.
func (t *lookup) Add(tx *types.Transaction) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.slots += numSlots(tx)
	slotsGauge.Update(int64(t.slots))

	t.txs[tx.Hash()] = tx
	t.addAuthorities(tx)
}

// Remove removes a transaction from the lookup.
func (t *lookup) Remove(hash common.Hash) {
	t.lock.Lock()
	defer t.lock.Unlock()

	tx, ok := t.txs[hash]
	if !ok {
		log.Error("No transaction found to be deleted", "hash", hash)
		return
	}
	t.removeAuthorities(tx)
	t.slots -= numSlots(tx)
	slotsGauge.Update(int64(t.slots))

	delete(t.txs, hash)
}

// Clear resets the lookup structure, removing all stored entries.
func (t *lookup) Clear() {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.slots = 0
	t.txs = make(map[common.Hash]*types.Transaction)
	t.auths = make(map[common.Address][]common.Hash)
}

// TxsBelowTip finds all remote transactions below the given tip threshold.
func (t *lookup) TxsBelowTip(threshold *big.Int) types.Transactions {
	found := make(types.Transactions, 0, 128)
	t.Range(func(hash common.Hash, tx *types.Transaction) bool {
		if tx.GasTipCapIntCmp(threshold) < 0 {
			found = append(found, tx)
		}
		return true
	})
	return found
}

// addAuthorities tracks the supplied tx in relation to each authority it
// specifies.
func (t *lookup) addAuthorities(tx *types.Transaction) {
	for _, addr := range tx.SetCodeAuthorities() {
		list, ok := t.auths[addr]
		if !ok {
			list = []common.Hash{}
		}
		if slices.Contains(list, tx.Hash()) {
			// Don't add duplicates.
			continue
		}
		list = append(list, tx.Hash())
		t.auths[addr] = list
	}
}

// removeAuthorities stops tracking the supplied tx in relation to its
// authorities.
func (t *lookup) removeAuthorities(tx *types.Transaction) {
	hash := tx.Hash()
	for _, addr := range tx.SetCodeAuthorities() {
		list := t.auths[addr]
		// Remove tx from tracker.
		if i := slices.Index(list, hash); i >= 0 {
			list = append(list[:i], list[i+1:]...)
		} else {
			log.Error("Authority with untracked tx", "addr", addr, "hash", hash)
		}
		if len(list) == 0 {
			// If list is newly empty, delete it entirely.
			delete(t.auths, addr)
			continue
		}
		t.auths[addr] = list
	}
}

// hasAuth returns a flag indicating whether there are pending authorizations
// from the specified address.
func (t *lookup) hasAuth(addr common.Address) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return len(t.auths[addr]) > 0
}

// numSlots calculates the number of slots needed for a single transaction.
func numSlots(tx *types.Transaction) int {
	return int((tx.Size() + txSlotSize - 1) / txSlotSize)
}

// slotsOf sums numSlots across txs, for the bulk reserved-occupancy
// adjustments below that remove more than one transaction at a time.
func slotsOf(txs types.Transactions) int {
	var slots int
	for _, tx := range txs {
		slots += numSlots(tx)
	}
	return slots
}

// Clear implements txpool.SubPool, removing all tracked txs from the pool
// and rotating the journal.
//
// Note, do not use this in production / live code. In live code, the pool is
// meant to reset on a separate thread to avoid DoS vectors.
func (pool *LegacyPool) Clear() {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	// unreserve each tracked account. Ideally, we could just clear the
	// reservation map in the parent txpool context. However, if we clear in
	// parent context, to avoid exposing the subpool lock, we have to lock the
	// reservations and then lock each subpool.
	//
	// This creates the potential for a deadlock situation:
	//
	// * TxPool.Clear locks the reservations
	// * a new transaction is received which locks the subpool mutex
	// * TxPool.Clear attempts to lock subpool mutex
	//
	// The transaction addition may attempt to reserve the sender addr which
	// can't happen until Clear releases the reservation lock. Clear cannot
	// acquire the subpool lock until the transaction addition is completed.

	for addr := range pool.pending {
		if _, ok := pool.queue.get(addr); !ok {
			pool.reserver.Release(addr)
		}
	}
	for _, addr := range pool.queue.addresses() {
		pool.reserver.Release(addr)
	}
	pool.all.Clear()
	pool.priced.Reheap()
	pool.pending = make(map[common.Address]*list)
	pool.queue = newQueue(pool.config, pool.signer)
	pool.pendingNonces = newNoncer(pool.currentState)
	pool.addReservedOccupancy(-pool.reservedOccupancy)
}

// HasPendingAuth returns a flag indicating whether there are pending
// authorizations from the specific address cached in the pool.
func (pool *LegacyPool) HasPendingAuth(addr common.Address) bool {
	return pool.all.hasAuth(addr)
}

// isFiltered checks if an address is in the filtered list.
func (pool *LegacyPool) isFiltered(addr common.Address) bool {
	_, exists := pool.filteredAddrs[addr]
	return exists
}

// isReserved reports whether addr is a reserved-blockspace client for the block
// being built on top of the current head. Reserved senders' zero-fee
// transactions bypass the pool's fee floors (PIP-35 min tip, base-fee tip floor)
// so they are admitted, kept, and surfaced to the miner. The fork height gate
// comes from chain config; the reserved set comes from the registry snapshot
// (rebuilt per head), so the source of truth matches the EVM and base-fee paths.
func (pool *LegacyPool) isReserved(addr common.Address) bool {
	cfg := pool.chainconfig
	if cfg.Bor == nil {
		return false
	}
	head := pool.currentHead.Load()
	if head == nil {
		return false
	}
	// Classify for the next block (the one this tx targets).
	number := new(big.Int).Add(head.Number, common.Big1)
	if !cfg.Bor.IsReservedBlockspace(number) {
		return false
	}
	return pool.reservedSnapshot.Load().IsReserved(addr)
}

// effectiveCost prices tx for the pool's balance checks: value alone for a
// reserved-blockspace sender (execution waives the gas debit up to quota, see
// reservedZeroFeeGas in core/state_transition.go), full cost otherwise. The
// pool is quota-unaware, so this is a superset of what will actually execute
// fee-free; an overflowing tx that doesn't fit is caught at execution instead
// (see the design note on isReserved). addr is the caller's already-recovered
// sender; unlike reservedTx (the priced heap's bare-tx predicate), this is
// never called without one, so it has no signer-recovery fallback.
func (pool *LegacyPool) effectiveCost(addr common.Address, tx *types.Transaction) *big.Int {
	if pool.isReserved(addr) {
		return tx.Value()
	}
	return tx.Cost()
}

// reservedTx is the tx-level reserved predicate handed to the priced list so it
// can protect reserved-blockspace transactions from price-based eviction. The
// sender is read from the tx's cached recovery (set during admission), so it
// stays safe to call without the pool lock held.
func (pool *LegacyPool) reservedTx(tx *types.Transaction) bool {
	from, err := types.Sender(pool.signer, tx)
	if err != nil {
		return false
	}
	return pool.isReserved(from)
}

// SetReservedRegistry installs the reserved-blockspace registry reader and
// rebuilds the snapshot from the current head. Called once post-Init from the
// backend (the consensus engine isn't available at pool construction).
func (pool *LegacyPool) SetReservedRegistry(r registryreader.Reader) {
	pool.mu.Lock()
	pool.reservedRegistry = r
	statedb, head := pool.currentState, pool.currentHead.Load()
	pool.mu.Unlock()
	pool.rebuildReservedSnapshot(statedb, head)
}

// rebuildReservedSnapshot reads the reserved set from the registry at the given
// block state and stores it. A read error leaves the previous snapshot in place
// rather than dropping classification. No-op when no registry is configured.
func (pool *LegacyPool) rebuildReservedSnapshot(statedb *state.StateDB, head *types.Header) {
	if pool.reservedRegistry == nil || statedb == nil || head == nil {
		return
	}
	// The pool classifies for the next block (head+1) — see isReserved.
	snap, err := registryreader.BuildSnapshot(pool.reservedRegistry, statedb, head.Number.Uint64(), head.Hash(), head.Number.Uint64()+1)
	if err != nil {
		log.Warn("Failed to build reserved-blockspace snapshot", "number", head.Number, "err", err)
		return
	}
	pool.reservedSnapshot.Store(snap)
}

// reconcileReservedOccupancy recomputes reservedOccupancy from scratch and
// overwrites the incrementally-tracked value with it, logging any disagreement
// first so a Layer-1 touchpoint bug is observable rather than silently
// self-correcting into invisibility. Called once per reorg cycle from reset.
func (pool *LegacyPool) reconcileReservedOccupancy() {
	recomputed := pool.recomputeReservedOccupancy()
	if delta := recomputed - pool.reservedOccupancy; delta != 0 {
		log.Warn("Reserved pool occupancy drifted from its incremental tally, correcting",
			"incremental", pool.reservedOccupancy, "recomputed", recomputed, "delta", delta)
	}
	pool.reservedOccupancy = recomputed
	reservedOccupancyGauge.Update(int64(recomputed))
}

// reservedSlots returns addr's combined pending+queued slot count (the same
// unit reservedOccupancy and its cap are tracked in — see numSlots),
// regardless of whether addr is currently reserved. Callers gate on
// isReserved themselves, since the two places this is used (a from-scratch
// recompute and the reorg-time backstop's spam ordering) each need to
// combine that gate with the count differently. Reads list.totalslots
// directly rather than summing list.Flatten() — the latter nonce-sorts and
// copies the whole list on every call once its cache is invalidated, which
// truncateReservedOccupancy's eviction loop would otherwise pay on every
// single iteration (removeTx invalidates the cache it just read).
func (pool *LegacyPool) reservedSlots(addr common.Address) int {
	count := 0
	if list := pool.pending[addr]; list != nil {
		count += list.totalslots
	}
	if list, ok := pool.queue.get(addr); ok {
		count += list.totalslots
	}
	return count
}

// recomputeReservedOccupancy walks the addresses currently tracked by the
// pool (pending and queued) and re-sums combined occupancy for whichever of
// them are reserved. An address that is reserved but holds nothing in the
// pool contributes zero either way, so this is equivalent to walking the full
// registry-reserved address set, but costs only O(distinct addresses
// currently in the pool) — the same bound truncatePending/demoteUnexecutables
// already pay once per reorg cycle — with no extra registry state read.
//
// pending and queue are disjoint maps, so summing reservedSlots once per
// address across the two loops below can never double-count; the only care
// needed is not visiting an address (and so its already-combined count)
// twice, which the pool.pending membership check in the second loop handles
// without a separate address set.
func (pool *LegacyPool) recomputeReservedOccupancy() int {
	var occupancy int
	for addr := range pool.pending {
		if pool.isReserved(addr) {
			occupancy += pool.reservedSlots(addr)
		}
	}
	for _, addr := range pool.queue.addresses() {
		if _, ok := pool.pending[addr]; ok {
			continue // already counted above
		}
		if pool.isReserved(addr) {
			occupancy += pool.reservedSlots(addr)
		}
	}
	return occupancy
}

// truncateReservedOccupancy is the reorg-time backstop for the reserved
// occupancy cap, mirroring truncatePending's own "assemble a spam order,
// penalize large transactors first" shape (down to reusing the same prque):
// build a priority queue of reserved addresses keyed by combined
// pending+queue slot count once, then repeatedly pop the largest, evict its
// newest transaction, and re-push it with its updated slot count. This is
// O(D log D + N log D) — D distinct reserved addresses, N evicted — rather
// than rescanning every reserved address per eviction.
//
// The synchronous admission gate in add() is the primary defense; this
// defense-in-depth pass only acts if occupancy is ever found over cap here
// regardless — e.g. because ReservedMaxOccupancyPercent was just lowered, or
// a registry change shrank the cap.
func (pool *LegacyPool) truncateReservedOccupancy() {
	limit := pool.reservedOccupancyCap()
	if pool.reservedOccupancy <= limit {
		return
	}

	spammers := prque.New[int, common.Address](nil)
	push := func(addr common.Address) {
		if !pool.isReserved(addr) {
			return
		}
		if count := pool.reservedSlots(addr); count > 0 {
			spammers.Push(addr, count)
		}
	}
	for addr := range pool.pending {
		push(addr)
	}
	for _, addr := range pool.queue.addresses() {
		if _, ok := pool.pending[addr]; ok {
			continue // already pushed above with its full combined count
		}
		push(addr)
	}

	before := pool.reservedOccupancy
	for pool.reservedOccupancy > limit && !spammers.Empty() {
		addr, _ := spammers.Pop()
		hash, ok := pool.newestReservedTx(addr)
		if !ok {
			continue // this account's transactions were already accounted for elsewhere
		}
		pool.removeTx(hash, true, true)
		if count := pool.reservedSlots(addr); count > 0 {
			spammers.Push(addr, count)
		}
	}
	if dropped := before - pool.reservedOccupancy; dropped > 0 {
		log.Warn("Trimmed reserved-sender transactions to enforce the occupancy cap", "dropped", dropped, "cap", limit)
	}
}

// newestReservedTx returns the hash of addr's highest-nonce transaction across
// both pending and queued, or ok=false if addr holds none. Trimming the
// highest nonce first never opens a gap in either list.
func (pool *LegacyPool) newestReservedTx(addr common.Address) (hash common.Hash, ok bool) {
	var newest *types.Transaction
	if list := pool.pending[addr]; list != nil && !list.Empty() {
		newest = list.LastElement()
	}
	if list, has := pool.queue.get(addr); has && !list.Empty() {
		if candidate := list.LastElement(); newest == nil || candidate.Nonce() > newest.Nonce() {
			newest = candidate
		}
	}
	if newest == nil {
		return common.Hash{}, false
	}
	return newest.Hash(), true
}
