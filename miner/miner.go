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

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// Backend wraps all methods required for mining. Only full node is capable
// to offer all the functions here.
type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *txpool.TxPool
	PeerCount() int

	// WhitelistedMilestone is finality's view of the chain: the newest
	// Heimdall milestone the node has whitelisted, or false when none has
	// arrived yet. The worker's finality gate compares it against the local
	// chain before a producer builds.
	WhitelistedMilestone() (bool, uint64, common.Hash)
}

// AdoptedWindow is a previous producer's unsealed block handed back by the
// sequence store: the open context the block must inherit and the ordered
// transactions to commit before consulting the txpool.
type AdoptedWindow struct {
	Number     uint64
	Timestamp  uint64
	ParentHash common.Hash
	GasLimit   uint64
	BaseFee    *big.Int
	Txs        []*types.Transaction
}

// SealVerdict is the store's answer to "may this sealed block be broadcast".
// The zero value is SealUnknown so an implementation that never answers
// defaults to broadcasting — production must not gate on the store.
type SealVerdict int

const (
	// SealUnknown: no verdict in budget (store slow, unreachable, or catching
	// up). Broadcast anyway — the liveness override.
	SealUnknown SealVerdict = iota

	// SealConfirmed: the store took our seal (or the chain already holds our
	// exact block). Broadcast.
	SealConfirmed

	// SealRefused: another producer's block owns this height — its seal beat
	// ours in the store and its block is on our chain. Discard ours;
	// broadcasting it would fork the chain against an already-decided height.
	SealRefused
)

// BlockSequencer receives block-production progress for the sequence store:
// the block context when a build starts, each transaction as it commits, and
// the sealed header the moment sealing completes. Implementations must never
// block — they are called on the worker's hot paths.
type BlockSequencer interface {
	OpenBlock(number uint64, timestamp uint64, parent common.Hash, gasLimit uint64, baseFee *big.Int)
	PublishTx(tx *types.Transaction)

	// SealBlock delivers the complete sealed block: the seal flush needs
	// the body to complete or re-anchor the store window (design §3.5).
	SealBlock(block *types.Block)

	// AdoptWindow reads the store tail for the block about to be built
	// (bounded; design §3.4). A returned window is an unsealed incumbent
	// window on this tip that the build must follow: the header inherits
	// its context and its transactions are committed first. nil means
	// build normally.
	AdoptWindow(number uint64, parent common.Hash) *AdoptedWindow

	// AwaitSequenced blocks until the window being built is confirmed by
	// the store, so the block about to be sealed is provably the store's
	// sequence at this height. false means another producer holds the
	// height and this block must not be sealed. An unreachable store
	// returns true — production never waits on the store.
	AwaitSequenced(timeout time.Duration, number uint64, txs []*types.Transaction) bool

	// ResyncNeeded reports that another producer holds the height this
	// node is building and reached the store first. The build stops rather
	// than seal beside their sequence; the next work cycle adopts it.
	// Reading consumes the signal.
	ResyncNeeded() bool

	// ConfirmSeal reports whether the block just handed to SealBlock may be
	// broadcast, waiting up to timeout for the store's verdict. The store's
	// head CAS elects exactly one seal per height; the loser learns it lost
	// and withholds its block, so one height gets one broadcast.
	ConfirmSeal(timeout time.Duration) SealVerdict

	// RefreshInterval is the mempool re-snapshot cadence while a block is
	// open: when non-zero the worker keeps the block open until just before
	// its announce time, refilling from the pool on this cadence so
	// transactions are executed (and preconfirmed) as they arrive. Zero
	// keeps the default one-shot fill.
	RefreshInterval() time.Duration
}

// Config is the configuration parameters of mining.
type Config struct {
	AllowGasTipOverride bool           // Won't enforce the default min gas tip (25 gwei) if true and will use user provided value
	Etherbase           common.Address `toml:",omitempty"` // Public address for block mining rewards
	ExtraData           hexutil.Bytes  `toml:",omitempty"` // Block extra data set by the miner
	GasCeil             uint64         // Target gas ceiling for mined blocks.

	// Dynamic gas limit configuration
	EnableDynamicGasLimit bool   // Enable dynamic gas limit adjustment based on base fee
	GasLimitMin           uint64 // Minimum gas limit when dynamic gas limit is enabled
	GasLimitMax           uint64 // Maximum gas limit when dynamic gas limit is enabled
	TargetBaseFee         uint64 // Target base fee in wei for dynamic gas limit adjustment
	BaseFeeBuffer         uint64 // Buffer around target base fee in wei (no adjustment when within buffer)

	GasPrice            *big.Int      // Minimum gas price for mining a transaction
	Recommit            time.Duration // The time interval for miner to re-create mining work.
	CommitInterruptFlag bool          // Interrupt commit when time is up ( default = true)
	BlockTime           time.Duration // The block time defined by the miner. Needs to be larger or equal to the consensus block time. If not set (default = 0), the miner will use the consensus block time.

	NewPayloadTimeout       time.Duration  // The maximum time allowance for creating a new payload
	PendingFeeRecipient     common.Address `toml:"-"` // Address for pending block rewards.
	EnablePrefetch          bool           // Enable transaction prefetching from pool during block building
	PrefetchGasLimitPercent uint64         // Gas limit percentage for prefetching (e.g., 100 = 100%, 110 = 110%)

	DisablePendingBlock bool // Disable the pending block creation loop on non block producer nodes
}

// DefaultConfig contains default settings for miner.
var DefaultConfig = Config{
	// Polygon/bor: PIP-60 (increase gas limit to 45M)
	GasCeil:  45_000_000,
	GasPrice: big.NewInt(params.BorDefaultMinerGasPrice), // enforces minimum gas price of 25 gwei in bor

	// Dynamic gas limit defaults (disabled by default)
	EnableDynamicGasLimit: false,
	GasLimitMin:           50_000_000,      // 50M gas
	GasLimitMax:           65_000_000,      // 65M gas
	TargetBaseFee:         500_000_000_000, // 500 gwei
	BaseFeeBuffer:         300_000_000_000, // 300 gwei buffer

	// The default recommit time is chosen as two seconds since
	// consensus-layer usually will wait a half slot of time(6s)
	// for payload generation. It should be enough for Geth to
	// run 3 rounds.
	Recommit:                2 * time.Second,
	EnablePrefetch:          true,
	PrefetchGasLimitPercent: 100, // 100% of header gas limit

	DisablePendingBlock: false,
}

// Miner is the main object which takes care of submitting new work to consensus
// engine and gathering the sealing result.
// nolint:staticcheck
type Miner struct {
	confMu  sync.RWMutex // The lock used to protect the config fields: GasCeil, GasTip and Extradata
	mux     *event.TypeMux
	eth     Backend
	engine  consensus.Engine
	exitCh  chan struct{}
	startCh chan struct{}
	stopCh  chan chan struct{}
	worker  *worker
	prio    []common.Address // A list of senders to prioritize

	wg sync.WaitGroup
}

func New(eth Backend, config *Config, chainConfig *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine, isLocalBlock func(header *types.Header) bool, makeWitness bool) *Miner {
	miner := &Miner{
		mux:     mux,
		eth:     eth,
		engine:  engine,
		exitCh:  make(chan struct{}),
		stopCh:  make(chan chan struct{}),
		startCh: make(chan struct{}),
		worker:  newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, true, makeWitness),
	}
	miner.wg.Add(1)

	go miner.update()

	return miner
}

func (miner *Miner) GetWorker() *worker {
	return miner.worker
}

// SetSequencer attaches a sequence-store publisher to the worker. Call before
// the miner starts; the worker reads the field without synchronization.
func (miner *Miner) SetSequencer(s BlockSequencer) {
	miner.worker.sequencer = s
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
func (miner *Miner) update() {
	defer miner.wg.Done()

	events := miner.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	defer func() {
		if !events.Closed() {
			events.Unsubscribe()
		}
	}()

	shouldStart := false
	canStart := true
	dlEventCh := events.Chan()

	for {
		select {
		case ev := <-dlEventCh:
			if ev == nil {
				// Unsubscription done, stop listening
				dlEventCh = nil
				continue
			}

			switch ev.Data.(type) {
			case downloader.StartEvent:
				wasMining := miner.Mining()
				miner.worker.stop()

				canStart = false

				if wasMining {
					// Resume mining after sync was finished
					shouldStart = true

					log.Info("Mining aborted due to sync")
				}
				miner.worker.syncing.Store(true)

			case downloader.FailedEvent:
				canStart = true

				if shouldStart {
					miner.worker.start()
				}

				miner.worker.rearmFinalityGrace()
				miner.worker.syncing.Store(false)

			case downloader.DoneEvent:
				canStart = true

				if shouldStart {
					miner.worker.start()
				}

				miner.worker.rearmFinalityGrace()
				miner.worker.syncing.Store(false)

				// Stop reacting to downloader events
				events.Unsubscribe()
			}
		case <-miner.startCh:
			if canStart {
				miner.worker.start()
			}

			shouldStart = true
		case ch := <-miner.stopCh:
			shouldStart = false

			miner.worker.stop()
			close(ch)
		case <-miner.exitCh:
			miner.worker.close()
			return
		}
	}
}

func (miner *Miner) Start() {
	miner.startCh <- struct{}{}
}

func (miner *Miner) Stop(ch chan struct{}) {
	miner.stopCh <- ch
}

func (miner *Miner) Close() {
	close(miner.exitCh)
	miner.wg.Wait()
}

func (miner *Miner) Mining() bool {
	return miner.worker.IsRunning()
}

func (miner *Miner) Hashrate() uint64 {
	if pow, ok := miner.engine.(consensus.PoW); ok {
		return uint64(pow.Hashrate())
	}

	return 0
}

func (miner *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}

	miner.worker.setExtra(extra)

	return nil
}

func (miner *Miner) SetGasTip(tip *big.Int) error {
	miner.worker.setGasTip(tip)
	return nil
}

// SetRecommitInterval sets the interval for sealing work resubmitting.
func (miner *Miner) SetRecommitInterval(interval time.Duration) {
	miner.worker.setRecommitInterval(interval)
}

// Pending returns the currently pending block and associated state. The returned
// values can be nil in case the pending block is not initialized
func (miner *Miner) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	return miner.worker.pending()
}

// PendingBlock returns the currently pending block. The returned block can be
// nil in case the pending block is not initialized.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (miner *Miner) PendingBlock() *types.Block {
	return miner.worker.pendingBlock()
}

func (miner *Miner) SetEtherbase(addr common.Address) {
	miner.worker.setEtherbase(addr)
}

// SetPrioAddresses sets a list of addresses to prioritize for transaction inclusion.
func (miner *Miner) SetPrioAddresses(prio []common.Address) {
	miner.confMu.Lock()
	miner.prio = prio
	miner.confMu.Unlock()
	miner.worker.setPrio(prio)
}

// SetGasCeil sets the gaslimit to strive for when mining blocks post 1559.
// For pre-1559 blocks, it sets the ceiling.
func (miner *Miner) SetGasCeil(ceil uint64) {
	miner.worker.setGasCeil(ceil)
}

// SubscribePendingLogs starts delivering logs from pending transactions
// to the given channel.
func (miner *Miner) SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription {
	return miner.worker.pendingLogsFeed.Subscribe(ch)
}

// BuildPayload builds the payload according to the provided parameters.
func (miner *Miner) BuildPayload(args *BuildPayloadArgs, witness bool) (*Payload, error) {
	return miner.worker.buildPayload(args, witness)
}
