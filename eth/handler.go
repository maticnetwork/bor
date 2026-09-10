// Copyright 2015 The go-ethereum Authors
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

package eth

import (
	"cmp"
	crand "crypto/rand"
	"errors"
	"maps"
	"math"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dchest/siphash"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/fetcher"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/eth/relay"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 128

	// txMaxBroadcastSize is the max size of a transaction that will be broadcasted.
	// All transactions with a higher size will be announced and need to be fetched
	// by the peer.
	txMaxBroadcastSize = 4096
)

var (
	syncChallengeTimeout = 15 * time.Second // Time allowance for a node to reply to the sync progress challenge
	// sealToBroadcastTimer measures latency from seal+write completion to broadcast start.
	// This captures event delivery delay through the TypeMux subscription channel.
	sealToBroadcastTimer = metrics.NewRegisteredTimer("eth/seal2broadcast", nil)
	// broadcastLoopTimer measures the time spent in each iteration of minedBroadcastLoop,
	// covering both block propagation and witness hash announcement to all peers.
	broadcastLoopTimer = metrics.NewRegisteredTimer("eth/broadcast_loop_duration", nil)
)

// txPool defines the methods needed from a transaction pool implementation to
// support all the operations needed by the Ethereum chain protocols.
type txPool interface {
	// Has returns an indicator whether txpool has a transaction
	// cached with the given hash.
	Has(hash common.Hash) bool

	// Get retrieves the transaction from local txpool with given
	// tx hash.
	Get(hash common.Hash) *types.Transaction

	// GetRLP retrieves the RLP-encoded transaction from local txpool
	// with given tx hash.
	GetRLP(hash common.Hash) []byte

	// GetMetadata returns the transaction type and transaction size with the
	// given transaction hash.
	GetMetadata(hash common.Hash) *txpool.TxMetadata

	// Add should add the given transactions to the pool.
	Add(txs []*types.Transaction, sync bool) []error

	// Pending should return pending transactions.
	// The slice should be modifiable by the caller.
	Pending(filter txpool.PendingFilter, interrupt *atomic.Bool) map[common.Address][]*txpool.LazyTransaction

	// SubscribeTransactions subscribes to new transaction events. The subscriber
	// can decide whether to receive notifications only for newly seen transactions
	// or also for reorged out ones.
	SubscribeTransactions(ch chan<- core.NewTxsEvent, reorgs bool) event.Subscription

	// SubscribeRebroadcastTransactions subscribes to stuck transaction rebroadcast events.
	SubscribeRebroadcastTransactions(ch chan<- core.StuckTxsEvent) event.Subscription
}

// handlerConfig is the collection of initialization parameters to create a full
// node network handler.
type handlerConfig struct {
	NodeID                  enode.ID            // P2P node ID used for tx propagation topology
	Database                ethdb.Database      // Database for direct sync insertions
	Chain                   *core.BlockChain    // Blockchain to serve data from
	TxPool                  txPool              // Transaction pool to propagate from
	Network                 uint64              // Network identifier to advertise
	Sync                    downloader.SyncMode // Whether to snap or full sync
	BloomCache              uint64              // Megabytes to alloc for snap sync bloom
	EventMux                *event.TypeMux      // Legacy event mux, deprecate for `feed`
	checker                 ethereum.ChainValidator
	RequiredBlocks          map[uint64]common.Hash // Hard coded map of required block hashes for sync challenges
	EthAPI                  *ethapi.BlockChainAPI  // EthAPI to interact
	enableBlockTracking     bool                   // Whether to log information collected while tracking block lifecycle
	txAnnouncementOnly      bool                   // Whether to only announce txs to peers
	disableTxPropagation    bool                   // Whether to disable broadcasting and announcement of txs to peers
	witnessProtocol         bool                   // Whether to enable witness protocol
	syncWithWitnesses       bool                   // Whether to sync blocks with witnesses
	syncAndProduceWitnesses bool                   // Whether to sync blocks and produce witnesses simultaneously
	fastForwardThreshold    uint64                 // Minimum necessary distance between local header and peer to fast forward
	gasCeil                 uint64                 // Gas ceiling for dynamic witness page threshold calculation
	p2pServer               *p2p.Server            // P2P server for jailing peers
	privateTxGetter         relay.PrivateTxGetter  // privateTxGetter to check if a transaction needs to be treated as private or not
}

type handler struct {
	nodeID     enode.ID
	networkID  uint64
	forkFilter forkid.Filter // Fork ID filter, constant across the lifetime of the node

	snapSync      atomic.Bool // Flag whether snap sync is enabled (gets disabled if we already have blocks)
	statelessSync atomic.Bool // Flag whether stateless sync is enabled
	synced        atomic.Bool // Flag whether we're considered synchronised (enables transaction processing)

	database ethdb.Database
	txpool   txPool
	chain    *core.BlockChain
	maxPeers int

	downloader     *downloader.Downloader
	blockFetcher   *fetcher.BlockFetcher
	txFetcher      *fetcher.TxFetcher
	peers          *peerSet
	txBroadcastKey [16]byte

	ethAPI *ethapi.BlockChainAPI // EthAPI to interact

	// privateTxGetter to check if a transaction needs to be treated as private or not
	privateTxGetter relay.PrivateTxGetter

	eventMux        *event.TypeMux
	txsCh           chan core.NewTxsEvent
	txsSub          event.Subscription
	stuckTxsCh      chan core.StuckTxsEvent
	stuckTxsSub     event.Subscription
	minedBlockSub   *event.TypeMuxSubscription
	witnessReadyCh  chan core.WitnessReadyEvent
	witnessReadySub event.Subscription
	blockRange      *blockRangeState

	requiredBlocks map[uint64]common.Hash

	enableBlockTracking  bool
	txAnnouncementOnly   bool
	disableTxPropagation bool

	p2pServer *p2p.Server // P2P server for jailing peers

	// Witness protocol related fields
	syncWithWitnesses       bool
	syncAndProduceWitnesses bool // Whether to sync blocks and produce witnesses simultaneously

	// WIT2: cache of BP-signed witness announcements, keyed by block hash.
	// Populated by both produced (signed locally) and received-and-verified
	// announcements. Consulted by the relay path to dedup, by the body
	// broadcast path to re-emit signed announces, and by the fetch path to
	// supply the byte-correctness comparison hash.
	signedWitnesses *signedWitnessCache

	// WIT2: in-flight witness bodies received via NewWitness broadcast but
	// not yet written to chain storage. Lets serving peers answer GetWitness
	// requests during the import gap, which is what unlocks fast multi-hop
	// propagation — without it, only the producer/post-import nodes can
	// serve and stateless nodes more than 1 hop away wait per-hop on full
	// validation before they can pull from anyone.
	pendingWitnessBodies *pendingWitnessBodyCache
	wit2PeerTracker      *peerWit2Tracker

	// WIT2: separate, much tighter per-peer budget gating how often a peer's
	// GetWitness request for a body we don't have can trigger a NEW
	// triggerRelayFetch goroutine (see recordWitnessWaiter). Deliberately
	// distinct from wit2PeerTracker, which only rate-limits cheap inbound
	// announce ingestion — a fetch trigger costs a real round trip against
	// another peer, so it needs its own, smaller budget.
	wit2FetchTriggerTracker *peerWit2Tracker

	// WIT2: signed announcements whose producer-binding could not be checked
	// at receive time because the matching block header wasn't local yet.
	// Drained from the chain-head subscription on each new block so the race
	// between block and announce gossip streams self-heals once the chain
	// catches up.
	deferredAnnounces *deferredAnnounceCache

	// WIT2: peers that asked us for a witness body we did not yet hold (we
	// answered GetWitness empty for a hash with a BP-signed announcement on
	// file). When we obtain the body we push it straight to them, restoring
	// the WIT1-style hand-off the fast announce removed.
	witnessWaiters *witnessWaiterRegistry

	// WIT2: dedup guard for relayFetchOnDemand — a pure relay node (no
	// produce_witness, no sync_with_witness) has no reason of its own to
	// ever fetch witness bytes, so without this it can register waiters via
	// recordWitnessWaiter forever without any path to satisfy them. Ensures
	// at most one outstanding upstream fetch per hash.
	relayFetchInFlight sync.Map

	// WIT2: global concurrency cap on triggerRelayFetch, independent of the
	// per-peer wit2FetchTriggerTracker budget above. That tracker only
	// bounds a single requesting peer's trigger rate — it does nothing to
	// stop several distinct peers from each independently bursting their
	// own budget at once. On a relay node with multiple downstream peers
	// (the exact multi-hop topology this fetch mechanism serves), enough
	// simultaneous triggers can transiently exhaust file descriptors —
	// confirmed on a real devnet run, where a burst of concurrent fetches
	// produced "too many open files" and permanently dropped peer
	// connections and the heimdall HTTP client. Sized well above any single
	// relay hop's realistic concurrent demand while bounding the total
	// number of simultaneous outbound fetch attempts network-wide.
	relayFetchSem chan struct{}

	wit2HeadCh  chan core.ChainHeadEvent
	wit2HeadSub event.Subscription

	// channels for fetcher, syncer, txsyncLoop
	quitSync chan struct{}

	chainSync *chainSyncer
	wg        sync.WaitGroup

	handlerStartCh chan struct{}
	handlerDoneCh  chan struct{}
}

// newHandler returns a handler for all Ethereum chain management protocol.
func newHandler(config *handlerConfig) (*handler, error) {
	// Create the protocol manager with the base fields
	if config.EventMux == nil {
		config.EventMux = new(event.TypeMux) // Nicety initialization for tests
	}

	h := &handler{
		nodeID:                  config.NodeID,
		networkID:               config.Network,
		forkFilter:              forkid.NewFilter(config.Chain),
		eventMux:                config.EventMux,
		database:                config.Database,
		txpool:                  config.TxPool,
		chain:                   config.Chain,
		peers:                   newPeerSet(),
		txBroadcastKey:          newBroadcastChoiceKey(),
		ethAPI:                  config.EthAPI,
		requiredBlocks:          config.RequiredBlocks,
		enableBlockTracking:     config.enableBlockTracking,
		txAnnouncementOnly:      config.txAnnouncementOnly,
		disableTxPropagation:    config.disableTxPropagation,
		p2pServer:               config.p2pServer,
		quitSync:                make(chan struct{}),
		handlerDoneCh:           make(chan struct{}),
		handlerStartCh:          make(chan struct{}),
		syncWithWitnesses:       config.syncWithWitnesses,
		syncAndProduceWitnesses: config.syncAndProduceWitnesses,
		privateTxGetter:         config.privateTxGetter,
		signedWitnesses:         newSignedWitnessCache(),
		pendingWitnessBodies:    newPendingWitnessBodyCache(witnessBodyCacheCapacity),
		wit2PeerTracker:         newPeerWit2Tracker(),
		wit2FetchTriggerTracker: newPeerWit2TrackerWithBudget(wit2FetchTriggerBurstCap, wit2FetchTriggerRefillPerSecond),
		relayFetchSem:           make(chan struct{}, wit2RelayFetchGlobalConcurrencyCap),
		deferredAnnounces:       newDeferredAnnounceCache(deferredAnnounceCapacity),
		witnessWaiters:          newWitnessWaiterRegistry(),
	}

	log.Info("Sync with witnesses", "enabled", config.syncWithWitnesses)

	if config.Sync == downloader.StatelessSync {
		// For stateless sync, we don't need to check state availability since
		// we'll be verifying blocks using witnesses
		h.statelessSync.Store(true)
		log.Info("Enabled stateless sync")
	} else if config.Sync == downloader.FullSync {
		// The database seems empty as the current block is the genesis. Yet the snap
		// block is ahead, so snap sync was enabled for this node at a certain point.
		// The scenarios where this can happen is
		// * if the user manually (or via a bad block) rolled back a snap sync node
		//   below the sync point.
		// * the last snap sync is not finished while user specifies a full sync this
		//   time. But we don't have any recent state for full sync.
		// In these cases however it's safe to reenable snap sync.
		fullBlock, snapBlock := h.chain.CurrentBlock(), h.chain.CurrentSnapBlock()
		if fullBlock.Number.Uint64() == 0 && snapBlock.Number.Uint64() > 0 {
			h.snapSync.Store(true)
			log.Warn("Switch sync mode from full sync to snap sync", "reason", "snap sync incomplete")
		} else if !h.chain.HasState(fullBlock.Root) {
			h.snapSync.Store(true)
			log.Warn("Switch sync mode from full sync to snap sync", "reason", "head state missing")
		}
	} else {
		// This is snap sync mode
		head := h.chain.CurrentBlock()
		if head.Number.Uint64() > 0 && h.chain.HasState(head.Root) {
			log.Info("Switch sync mode from snap sync to full sync", "reason", "snap sync complete")
		} else {
			// If snap sync was requested and our database is empty, grant it
			h.snapSync.Store(true)
			log.Info("Enabled snap sync", "head", head.Number, "hash", head.Hash())
		}
	}

	// If snap sync is requested but snapshots are disabled, fail loudly
	if h.snapSync.Load() && (config.Chain.Snapshots() == nil && config.Chain.TrieDB().Scheme() == rawdb.HashScheme) {
		return nil, errors.New("snap sync not supported with snapshots disabled")
	}

	// Construct the downloader (long sync)
	h.downloader = downloader.New(config.Database, h.eventMux, h.chain, nil, h.removePeer, h.enableSyncedFeatures, config.checker, config.fastForwardThreshold, config.syncAndProduceWitnesses)
	if ttd := h.chain.Config().TerminalTotalDifficulty; ttd != nil {
		head := h.chain.CurrentBlock()
		if td := h.chain.GetTd(head.Hash(), head.Number.Uint64()); td.Cmp(ttd) >= 0 {
			log.Info("Chain post-TTD, sync via beacon client")
		} else {
			log.Warn("Chain pre-merge, sync via PoW (ensure beacon client is ready)")
		}
	}
	// Construct the fetcher (short sync)
	validator := func(header *types.Header) error {
		// Reject all the PoS style headers in the first place. No matter
		// the chain has finished the transition or not, the PoS headers
		// should only come from the trusted consensus layer instead of
		// p2p network.
		if beacon, ok := h.chain.Engine().(*beacon.Beacon); ok {
			if beacon.IsPoSHeader(header) {
				return errors.New("unexpected post-merge header")
			}
		}
		return h.chain.Engine().VerifyHeader(h.chain, header)
	}
	heighter := func() uint64 {
		return h.chain.CurrentBlock().Number.Uint64()
	}
	inserter := func(blocks types.Blocks, witnesses []*stateless.Witness) (int, error) {
		// If sync is running, deny importing weird blocks.
		if !h.synced.Load() {
			log.Warn("Syncing, discarded propagated block", "number", blocks[0].Number(), "hash", blocks[0].Hash())
			return 0, nil
		}

		if h.statelessSync.Load() {
			return h.chain.InsertChainStateless(blocks, witnesses)
		} else {
			return h.chain.InsertChainWithWitnesses(blocks, config.witnessProtocol, witnesses)
		}
	}

	h.blockFetcher = fetcher.NewBlockFetcher(false, nil, h.chain.GetBlockByHash, validator, h.BroadcastBlock, heighter, h.chain.CurrentHeader, nil, inserter, h.removePeer, h.jailPeer, h.enableBlockTracking, h.statelessSync.Load() || h.syncWithWitnesses, config.gasCeil, h.lookupSignedWitnessHash, h.cacheVerifiedWitnessForServing)
	// WIT2: penalize a peer that serves a non-empty witness whose bytes mismatch
	// the BP-signed commitment (strike, not drop — see strikeWit2PeerByID).
	h.blockFetcher.SetWitnessServerStriker(h.strikeWit2PeerByID)

	fetchTx := func(peer string, requestID uint64, hashes []common.Hash) error {
		p := h.peers.peer(peer)
		if p == nil {
			return errors.New("unknown peer")
		}

		return p.RequestTxs(requestID, hashes)
	}
	addTxs := func(txs []*types.Transaction) []error {
		return h.txpool.Add(txs, false)
	}
	h.txFetcher = fetcher.NewTxFetcher(h.txpool.Has, addTxs, fetchTx, h.removePeer)
	h.chainSync = newChainSyncer(h)

	return h, nil
}

// protoTracker tracks the number of active protocol handlers.
func (h *handler) protoTracker() {
	defer h.wg.Done()
	var active int
	for {
		select {
		case <-h.handlerStartCh:
			active++
		case <-h.handlerDoneCh:
			active--
		case <-h.quitSync:
			// Wait for all active handlers to finish.
			for ; active > 0; active-- {
				<-h.handlerDoneCh
			}
			return
		}
	}
}

// incHandlers signals to increment the number of active handlers if not
// quitting.
func (h *handler) incHandlers() bool {
	select {
	case h.handlerStartCh <- struct{}{}:
		return true
	case <-h.quitSync:
		return false
	}
}

// decHandlers signals to decrement the number of active handlers.
func (h *handler) decHandlers() {
	h.handlerDoneCh <- struct{}{}
}

// runEthPeer registers an eth peer into the joint eth/snap peerset, adds it to
// various subsystems and starts handling messages.
func (h *handler) runEthPeer(peer *eth.Peer, handler eth.Handler) error {
	if !h.incHandlers() {
		return p2p.DiscQuitting
	}
	defer h.decHandlers()

	// If the peer has a `snap` extension, wait for it to connect so we can have
	// a uniform initialization/teardown mechanism
	snap, err := h.peers.waitSnapExtension(peer)
	if err != nil {
		peer.Log().Error("Snapshot extension barrier failed", "err", err)
		return err
	}

	// If the peer has a `wit` extension, wait for it to connect so we can have
	// a uniform initialization/teardown mechanism
	wit, err := h.peers.waitWitExtension(peer)
	if err != nil {
		peer.Log().Error("Witness extension barrier failed", "err", err)
		return err
	}

	// Execute the Ethereum handshake
	if err := peer.Handshake(h.networkID, h.chain, h.blockRange.currentRange()); err != nil {
		peer.Log().Debug("Ethereum handshake failed", "err", err)
		return err
	}
	reject := false // reserved peer slots
	if h.snapSync.Load() {
		if snap == nil {
			// If we are running snap-sync, we want to reserve roughly half the peer
			// slots for peers supporting the snap protocol.
			// The logic here is; we only allow up to 5 more non-snap peers than snap-peers.
			if all, snp := h.peers.len(), h.peers.snapLen(); all-snp > snp+5 {
				reject = true
			}
		}
	}
	// Ignore maxPeers if this is a trusted peer
	if !peer.Peer.Info().Network.Trusted {
		if reject || h.peers.len() >= h.maxPeers {
			return p2p.DiscTooManyPeers
		}
	}
	peer.Log().Debug("Ethereum peer connected", "name", peer.Name())

	// Register the peer locally
	if err := h.peers.registerPeer(peer, snap, wit); err != nil {
		peer.Log().Error("Ethereum peer registration failed", "err", err)
		return err
	}
	defer h.unregisterPeer(peer.ID())

	p := h.peers.peer(peer.ID())
	if p == nil {
		return errors.New("peer dropped during handling")
	}
	// Register the peer in the downloader. If the downloader considers it banned, we disconnect
	if err := h.downloader.RegisterPeer(p.ID(), p.Version(), p); err != nil {
		peer.Log().Error("Failed to register peer in eth syncer", "err", err)
		return err
	}
	if snap != nil {
		if err := h.downloader.SnapSyncer.Register(snap); err != nil {
			peer.Log().Error("Failed to register peer in snap syncer", "err", err)
			return err
		}
	}
	h.chainSync.handlePeerEvent()

	// Bor: skip propagating transactions if flag is set
	if !h.disableTxPropagation {
		// Propagate existing transactions. new transactions appearing
		// after this will be sent via broadcasts.
		h.syncTransactions(peer)
	}

	// Create a notification channel for pending requests if the peer goes down
	dead := make(chan struct{})
	defer close(dead)

	// If we have any explicit peer required block hashes, request them
	for number, hash := range h.requiredBlocks {
		resCh := make(chan *eth.Response)

		req, err := peer.RequestHeadersByNumber(number, 1, 0, false, resCh)
		if err != nil {
			return err
		}
		go func(number uint64, hash common.Hash, req *eth.Request) {
			// Ensure the request gets cancelled in case of error/drop
			defer req.Close()

			timeout := time.NewTimer(syncChallengeTimeout)
			defer timeout.Stop()

			select {
			case res := <-resCh:
				headers := ([]*types.Header)(*res.Res.(*eth.BlockHeadersRequest))
				if len(headers) == 0 {
					// Required blocks are allowed to be missing if the remote
					// node is not yet synced
					res.Done <- nil
					return
				}
				// Validate the header and either drop the peer or continue
				if len(headers) > 1 {
					res.Done <- errors.New("too many headers in required block response")
					return
				}
				if headers[0].Number.Uint64() != number || headers[0].Hash() != hash {
					peer.Log().Info("Required block mismatch, dropping peer", "number", number, "hash", headers[0].Hash(), "want", hash)
					res.Done <- errors.New("required block mismatch")
					return
				}
				peer.Log().Debug("Peer required block verified", "number", number, "hash", hash)
				res.Done <- nil
			case <-timeout.C:
				peer.Log().Warn("Required block challenge timed out, dropping", "timeout", syncChallengeTimeout, "addr", peer.RemoteAddr(), "type", peer.Name())
				h.removePeer(peer.ID())
			case <-dead:
				// Peer handler terminated, abort all goroutines
			}
		}(number, hash, req)
	}
	// Handle incoming messages until the connection is torn down
	return handler(peer)
}

// runSnapExtension registers a `snap` peer into the joint eth/snap peerset and
// starts handling inbound messages. As `snap` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
func (h *handler) runSnapExtension(peer *snap.Peer, handler snap.Handler) error {
	if !h.incHandlers() {
		return p2p.DiscQuitting
	}
	defer h.decHandlers()

	if err := h.peers.registerSnapExtension(peer); err != nil {
		if metrics.Enabled() {
			if peer.Inbound() {
				snap.IngressRegistrationErrorMeter.Mark(1)
			} else {
				snap.EgressRegistrationErrorMeter.Mark(1)
			}
		}
		peer.Log().Debug("Snapshot extension registration failed", "err", err)
		return err
	}

	return handler(peer)
}

// runWitExtension registers a `wit` peer into the joint eth/wit peerset and
// starts handling inbound messages. As `wit` is only a satellite protocol to
// `eth`, all subsystem registrations and lifecycle management will be done by
// the main `eth` handler to prevent strange races.
func (h *handler) runWitExtension(peer *wit.Peer, handler wit.Handler) error {
	if !h.incHandlers() {
		return p2p.DiscQuitting
	}
	defer h.decHandlers()

	if err := h.peers.registerWitExtension(peer); err != nil {
		peer.Log().Debug("Witness extension registration failed", "err", err)
		return err
	}

	return handler(peer)
}

// jailPeer jails a peer to prevent reconnection for a period of time
func (h *handler) jailPeer(id string) {
	if h.p2pServer == nil {
		return
	}
	// Convert peer ID (string) to enode.ID
	nodeID, err := enode.ParseID(id)
	if err != nil {
		log.Warn("Failed to parse peer ID for jailing", "peer", id, "err", err)
		return
	}
	h.p2pServer.JailPeer(nodeID)
}

// removePeer requests disconnection of a peer.
func (h *handler) removePeer(id string) {
	peer := h.peers.peer(id)
	if peer != nil {
		log.Debug("Handler: removing peer", "peer", peer.ID(), "inbound", peer.Peer.Inbound(), "duration", common.PrettyDuration(peer.Peer.Lifetime()))
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
	if h.wit2PeerTracker != nil {
		h.wit2PeerTracker.forget(id)
	}
	if h.wit2FetchTriggerTracker != nil {
		h.wit2FetchTriggerTracker.forget(id)
	}
}

// unregisterPeer removes a peer from the downloader, fetchers and main peer set.
func (h *handler) unregisterPeer(id string) {
	// Create a custom logger to avoid printing the entire id
	var logger log.Logger
	if len(id) < 16 {
		// Tests use short IDs, don't choke on them
		logger = log.New("peer", id)
	} else {
		logger = log.New("peer", id[:8])
	}
	// Forget any WIT2 per-peer tracker state on the guaranteed teardown path.
	// This runs for every disconnect (clean/remote-initiated or our own drop),
	// unlike removePeer which only fires on proactive drops — so the per-peer
	// announce-budget/strike map cannot leak one entry per disconnecting peer.
	// Done before the registration check below: tracker state is independent of
	// peer-set membership and must be cleared even for a half-registered peer.
	if h.wit2PeerTracker != nil {
		h.wit2PeerTracker.forget(id)
	}
	if h.wit2FetchTriggerTracker != nil {
		h.wit2FetchTriggerTracker.forget(id)
	}
	// Likewise drop any witness-waiter entries this peer holds, on the same
	// guaranteed teardown path. Otherwise a peer that asked for a not-yet-held
	// witness and then disconnected leaves its *wit.Peer recorded until the TTL.
	if h.witnessWaiters != nil {
		h.witnessWaiters.forget(id)
	}
	// Abort if the peer does not exist
	peer := h.peers.peer(id)
	if peer == nil {
		logger.Warn("Ethereum peer removal failed", "err", errPeerNotRegistered)
		return
	}
	// Remove the `eth` peer if it exists
	logger.Debug("Removing Ethereum peer", "snap", peer.snapExt != nil)

	// Remove the `snap` extension if it exists
	if peer.snapExt != nil {
		h.downloader.SnapSyncer.Unregister(id)
	}

	h.downloader.UnregisterPeer(id)
	h.txFetcher.Drop(id)

	if err := h.peers.unregisterPeer(id); err != nil {
		logger.Error("Ethereum peer removal failed", "err", err)
	}
}

func (h *handler) Start(maxPeers int) {
	h.maxPeers = maxPeers

	if h.disableTxPropagation {
		log.Info("Disabling transaction propagation completely")
	}

	// Bor: block producers can choose to not propagate transactions to save p2p overhead
	// broadcast and announce transactions (only new ones, not resurrected ones) only
	// if transaction propagation is enabled
	if !h.disableTxPropagation {
		h.wg.Add(1)
		h.txsCh = make(chan core.NewTxsEvent, txChanSize)
		h.txsSub = h.txpool.SubscribeTransactions(h.txsCh, false)
		go h.txBroadcastLoop()
	}

	// rebroadcast stuck transactions
	if !h.disableTxPropagation {
		h.wg.Add(1)
		h.stuckTxsCh = make(chan core.StuckTxsEvent, txChanSize)
		h.stuckTxsSub = h.txpool.SubscribeRebroadcastTransactions(h.stuckTxsCh)
		go h.stuckTxBroadcastLoop()
	}

	// broadcast mined blocks
	h.wg.Add(1)
	h.minedBlockSub = h.eventMux.Subscribe(core.NewMinedBlockEvent{})
	go h.minedBroadcastLoop()

	// announce witnesses from pipelined import SRC
	h.witnessReadyCh = make(chan core.WitnessReadyEvent, 10)
	h.witnessReadySub = h.chain.SubscribeWitnessReadyEvent(h.witnessReadyCh)
	h.wg.Add(1)
	go h.witnessReadyBroadcastLoop()

	h.wg.Add(1)
	go h.chainSync.loop()

	// broadcast block range
	h.wg.Add(1)
	h.blockRange = newBlockRangeState(h.chain, h.eventMux)
	go h.blockRangeLoop(h.blockRange)

	// start peer handler tracker
	h.wg.Add(1)
	go h.protoTracker()

	// WIT2: drain deferred signed announces on each new chain head. This
	// closes the cosend race: when a signed announcement arrives ahead of
	// its block, we hold it in deferredAnnounces and re-evaluate as soon as
	// the matching header lands.
	h.wit2HeadCh = make(chan core.ChainHeadEvent, chainHeadChanSize)
	h.wit2HeadSub = h.chain.SubscribeChainHeadEvent(h.wit2HeadCh)
	h.wg.Add(1)
	go h.deferredAnnouncesLoop()
}

func (h *handler) Stop() {
	if h.txsSub != nil {
		h.txsSub.Unsubscribe() // quits txBroadcastLoop
	}
	if h.stuckTxsSub != nil {
		h.stuckTxsSub.Unsubscribe() // quits stuckTxBroadcastLoop
	}
	h.minedBlockSub.Unsubscribe()
	if h.witnessReadySub != nil {
		h.witnessReadySub.Unsubscribe()
	}
	h.blockRange.stop()

	// Quit chainSync and txsync64.
	// After this is done, no new peers will be accepted.
	close(h.quitSync)

	// Disconnect existing sessions.
	// This also closes the gate for any new registrations on the peer set.
	// sessions which are already established but not added to h.peers yet
	// will exit when they try to register.
	h.peers.close()
	h.wg.Wait()

	log.Info("Ethereum protocol stopped")
}

// TODO(@pratikspatil024) - use this to broadcast the witness
// BroadcastWitness broadcasts the witness to all peers who are not aware of it
func (h *handler) BroadcastWitness(witness *stateless.Witness) {
	// broadcast the witness to all peers who are not aware of it
	for _, peer := range h.peers.peersWithoutWitness(witness.Headers[0].Hash()) {
		peer.Peer.AsyncSendNewWitness(witness)
	}
}

// BroadcastBlock will either propagate a block to a subset of its peers, or
// will only announce its availability (depending what's requested).
func (h *handler) BroadcastBlock(block *types.Block, witness *stateless.Witness, propagate bool) {
	// Disable the block propagation if it's the post-merge block.
	if beacon, ok := h.chain.Engine().(*beacon.Beacon); ok {
		if beacon.IsPoSHeader(block.Header()) {
			return
		}
	}

	hash := block.Hash()
	peers := h.peers.peersWithoutBlock(hash)
	peersWithoutWitness := h.peers.peersWithoutWitness(hash)

	// If propagation is requested, send to a subset of the peer
	if propagate {
		// Calculate the TD of the block (it's not imported yet, so block.Td is not valid)
		var td *big.Int
		if parent := h.chain.GetBlock(block.ParentHash(), block.NumberU64()-1); parent != nil {
			td = new(big.Int).Add(block.Difficulty(), h.chain.GetTd(block.ParentHash(), block.NumberU64()-1))
		} else {
			log.Error("Propagating dangling block", "number", block.Number(), "hash", hash)
			return
		}

		// These are the static and trusted peers which are not
		// in `transfer := peers[:int(math.Sqrt(float64(len(peers))))]`
		staticAndTrustedPeers := []*ethPeer{}

		for _, peer := range peers[int(math.Sqrt(float64(len(peers)))):] {
			if peer.IsTrusted() || peer.IsStatic() {
				staticAndTrustedPeers = append(staticAndTrustedPeers, peer)
			}
		}

		// Send the block to a subset of our peers
		transfer := peers[:int(math.Sqrt(float64(len(peers))))]
		sentBlockPeers := make(map[string]bool)
		for _, peer := range transfer {
			peer.AsyncSendNewBlock(block, td)
			sentBlockPeers[peer.Peer.ID()] = true
		}

		// Send the block to the trusted and static peers
		for _, peer := range staticAndTrustedPeers {
			log.Trace("Propagating block to static and trusted peer", "hash", hash, "peerID", peer.ID())
			peer.AsyncSendNewBlock(block, td)
		}

		// WIT2: co-send the witness announcement to every direct block
		// recipient that doesn't yet have the witness. Closes the gap where
		// blocks fan out at sqrt(N) but witnesses didn't.
		h.cosendWitnessAnnouncement(hash, block.NumberU64(), transfer, staticAndTrustedPeers)

		log.Debug("Propagated block", "hash", hash, "recipients", len(transfer), "static and trusted recipients", len(staticAndTrustedPeers), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))

		return
	}
	// Otherwise, announce the block if it is already written locally or if the
	// witness is cached and the block is in-flight on the local write path.
	if h.chain.HasBlock(hash, block.NumberU64()) || h.chain.HasWitness(hash) {
		for _, peer := range peers {
			peer.AsyncSendNewBlockHash(block)
		}
	}

	if h.chain.HasWitness(hash) {
		// Try to attach a BP signature so WIT2 peers can fast-validate and
		// transitively relay. Falls through to unsigned WIT1 announces for
		// peers below WIT2 (and for any peer if signing is unavailable, e.g.,
		// non-producer nodes that didn't receive a signed announce upstream).
		signedAnn, hasSigned := h.signLocalWitnessAnnouncement(hash, block.NumberU64())
		for _, peer := range peersWithoutWitness {
			if hasSigned && peer.Peer.Version() >= wit.WIT2 {
				peer.Peer.AsyncSendSignedWitnessAnnouncement(signedAnn)
			} else {
				peer.Peer.AsyncSendNewWitnessHash(block.Header().Hash(), block.NumberU64())
			}
		}
		log.Debug("Announced witness", "hash", hash, "recipients", len(peers), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
	}
}

func EthPeersContainsID(ethPeers []*ethPeer, id string) bool {
	for _, peer := range ethPeers {
		if peer.ID() == id {
			return true
		}
	}
	return false
}

// BroadcastTransactions will propagate a batch of transactions
// - To a square root of all peers for non-blob transactions
// - And, separately, as announcements to all peers which are not known to
// already have the given transaction.
func (h *handler) BroadcastTransactions(txs types.Transactions) {
	var (
		blobTxs  int // Number of blob transactions to announce only
		largeTxs int // Number of large transactions to announce only

		directCount int // Number of transactions sent directly to peers (duplicates included)
		annCount    int // Number of transactions announced across all peers (duplicates included)

		txset = make(map[*ethPeer][]common.Hash) // Set peer->hash to transfer directly
		annos = make(map[*ethPeer][]common.Hash) // Set peer->hash to announce

		signer = types.LatestSigner(h.chain.Config())
		choice = newBroadcastChoice(h.nodeID, h.txBroadcastKey)
		peers  = h.peers.all()
	)

	for _, tx := range txs {
		// Skip gossip if transaction is marked as private
		if h.privateTxGetter != nil && h.privateTxGetter.IsTxPrivate(tx.Hash()) {
			log.Debug("[tx-relay] skip tx broadcast for private tx", "hash", tx.Hash())
			continue
		}
		var directSet map[*ethPeer]struct{}
		switch {
		case tx.Type() == types.BlobTxType:
			blobTxs++
		case tx.Size() > txMaxBroadcastSize:
			largeTxs++
		default:
			// bor: respect announce-only mode
			// If enabled, skip selecting direct peers so we only announce hashes.
			if !h.txAnnouncementOnly {
				// Get transaction sender address. Here we can ignore any error
				// since we're just interested in any value.
				txSender, _ := types.Sender(signer, tx)
				directSet = choice.choosePeers(peers, txSender)
			}
		}

		for _, peer := range peers {
			if peer.KnownTransaction(tx.Hash()) {
				continue
			}
			if _, ok := directSet[peer]; ok {
				// Send direct.
				txset[peer] = append(txset[peer], tx.Hash())
			} else {
				// Send announcement.
				annos[peer] = append(annos[peer], tx.Hash())
			}
		}
	}

	for peer, hashes := range txset {
		directCount += len(hashes)
		peer.AsyncSendTransactions(hashes)
	}

	for peer, hashes := range annos {
		annCount += len(hashes)
		peer.AsyncSendPooledTransactionHashes(hashes)
	}
	log.Debug("Distributed transactions", "plaintxs", len(txs)-blobTxs-largeTxs, "blobtxs", blobTxs, "largetxs", largeTxs,
		"bcastcount", directCount, "anncount", annCount)
}

// minedBroadcastLoop sends mined blocks to connected peers.
func (h *handler) minedBroadcastLoop() {
	defer h.wg.Done()

	for obj := range h.minedBlockSub.Chan() {
		if ev, ok := obj.Data.(core.NewMinedBlockEvent); ok {
			now := time.Now()
			var sealToBcast time.Duration
			if !ev.SealedAt.IsZero() {
				sealToBcast = now.Sub(ev.SealedAt)
				sealToBroadcastTimer.Update(sealToBcast)
			}
			if h.enableBlockTracking {
				delayInMs := now.UnixMilli() - int64(ev.Block.Time())*1000
				delay := common.PrettyDuration(time.Millisecond * time.Duration(delayInMs))
				log.Info("[block tracker] Broadcasting mined block", "number", ev.Block.NumberU64(), "hash", ev.Block.Hash(), "blockTime", ev.Block.Time(), "now", now.Unix(), "delay", delay, "delayInMs", delayInMs, "sealToBroadcast", common.PrettyDuration(sealToBcast))
			}
			loopStart := time.Now()
			h.BroadcastBlock(ev.Block, ev.Witness, true) // First propagate block to peers
			// Tracked on h.wg so Stop waits it out (bounded by the 500ms
			// visibility poll) instead of racing peer shutdown.
			h.wg.Add(1)
			go func(block *types.Block, witness *stateless.Witness) {
				defer h.wg.Done()
				h.announceMinedBlock(block, witness)
			}(ev.Block, ev.Witness)
			broadcastLoopTimer.Update(time.Since(loopStart))
		}
	}
}

// announceMinedBlock announces a locally mined block after it becomes visible
// through the local chain reader.
//
// The pipelined inline path broadcasts before its async DB write completes, so
// announcing immediately can race with HasBlock() and silently skip the hash
// announcement to non-propagation peers. Wait briefly for the write to land,
// then announce. If the block still isn't visible but the witness is cached,
// fall back to the witness-gated path so stateless peers can still progress.
func (h *handler) announceMinedBlock(block *types.Block, witness *stateless.Witness) {
	const (
		pollInterval = 10 * time.Millisecond
		maxWait      = 500 * time.Millisecond
	)

	hash := block.Hash()
	number := block.NumberU64()
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if h.chain.HasBlock(hash, number) {
			h.BroadcastBlock(block, witness, false)
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			if h.chain.HasWitness(hash) {
				h.BroadcastBlock(block, witness, false)
			} else {
				log.Debug("Skipping mined block announce before local write became visible", "hash", hash, "number", number)
			}
			return
		}
	}
}

// witnessReadyBroadcastLoop announces witness availability from the pipelined
// import SRC goroutine. Without this, the stateless node would have to poll
// for witnesses with 10-second retry intervals.
func (h *handler) witnessReadyBroadcastLoop() {
	defer h.wg.Done()

	for {
		select {
		case ev := <-h.witnessReadyCh:
			for _, peer := range h.peers.peersWithoutWitness(ev.BlockHash) {
				peer.Peer.AsyncSendNewWitnessHash(ev.BlockHash, ev.BlockNumber)
			}
		case <-h.witnessReadySub.Err():
			return
		}
	}
}

// txBroadcastLoop announces new transactions to connected peers.
func (h *handler) txBroadcastLoop() {
	defer h.wg.Done()

	for {
		select {
		case event := <-h.txsCh:
			h.BroadcastTransactions(event.Txs)
		case <-h.txsSub.Err():
			return
		}
	}
}

// stuckTxBroadcastLoop handles rebroadcasting of stuck transactions.
// It clears the transaction hashes from peers' knownTxs caches and
// rebroadcasts them to the network.
func (h *handler) stuckTxBroadcastLoop() {
	defer h.wg.Done()

	for {
		select {
		case event := <-h.stuckTxsCh:
			// Only rebroadcast when synced
			if !h.synced.Load() {
				continue
			}

			// Collect hashes to clear from knownTxs
			hashes := make([]common.Hash, len(event.Txs))
			for i, tx := range event.Txs {
				hashes[i] = tx.Hash()
			}

			// Clear from all peers' knownTxs
			h.peers.ForgetTransactions(hashes)

			// Rebroadcast
			h.BroadcastTransactions(event.Txs)

			log.Debug("Rebroadcast stuck transactions", "count", len(event.Txs))

		case <-h.stuckTxsSub.Err():
			return
		}
	}
}

// enableSyncedFeatures enables the post-sync functionalities when the initial
// sync is finished.
func (h *handler) enableSyncedFeatures() {
	// Mark the local node as synced.
	h.synced.Store(true)

	// If we were running snap sync and it finished, disable doing another
	// round on next sync cycle
	if h.snapSync.Load() {
		log.Info("Snap sync complete, auto disabling")
		h.snapSync.Store(false)
	}
}

// PeerStats represents a short summary of the information known about a connected
// peer. Specifically, it contains details about the head hash and total difficulty
// of a peer which makes it a bit different from the PeerInfo.
type PeerStats struct {
	Enode  string `json:"enode"`  // Node URL
	ID     string `json:"id"`     // Unique node identifier
	Name   string `json:"name"`   // Name of the node, including client type, version, OS, custom data
	Hash   string `json:"hash"`   // Head hash of the peer
	Number uint64 `json:"number"` // Head number of the peer
	Td     uint64 `json:"td"`     // Total difficulty of the peer
}

// GetPeerStats returns the current head height and td of all the connected peers
// along with few additional identifiers.
func (h *handler) GetPeerStats() []*PeerStats {
	peers := h.peers.getAllPeers()
	info := make([]*PeerStats, 0, len(peers))

	for _, peer := range peers {
		hash, td := peer.Head()
		block := h.chain.GetBlockByHash(hash)
		number := uint64(0)
		if block != nil {
			number = block.NumberU64()
		}
		info = append(info, &PeerStats{
			Enode:  peer.Node().URLv4(),
			ID:     peer.ID(),
			Name:   peer.Name(),
			Hash:   hash.String(),
			Number: number,
			Td:     td.Uint64(),
		})
	}

	return info
}

// blockRangeState holds the state of the block range update broadcasting mechanism.
type blockRangeState struct {
	prev    eth.BlockRangeUpdatePacket
	next    atomic.Pointer[eth.BlockRangeUpdatePacket]
	headCh  chan core.ChainHeadEvent
	headSub event.Subscription
	syncSub *event.TypeMuxSubscription
}

func newBlockRangeState(chain *core.BlockChain, typeMux *event.TypeMux) *blockRangeState {
	headCh := make(chan core.ChainHeadEvent, chainHeadChanSize)
	headSub := chain.SubscribeChainHeadEvent(headCh)
	syncSub := typeMux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	st := &blockRangeState{
		headCh:  headCh,
		headSub: headSub,
		syncSub: syncSub,
	}
	st.update(chain, chain.CurrentBlock())
	st.prev = *st.next.Load()
	return st
}

// blockRangeLoop announces changes in locally-available block range to peers.
// The range to announce is the range that is available in the store, so it's not just
// about imported blocks.
func (h *handler) blockRangeLoop(st *blockRangeState) {
	defer h.wg.Done()

	for {
		select {
		case ev := <-st.syncSub.Chan():
			if ev == nil {
				continue
			}
			if _, ok := ev.Data.(downloader.StartEvent); ok && h.snapSync.Load() {
				h.blockRangeWhileSnapSyncing(st)
			}
		case <-st.headCh:
			st.update(h.chain, h.chain.CurrentBlock())
			if st.shouldSend() {
				h.broadcastBlockRange(st)
			}
		case <-st.headSub.Err():
			return
		}
	}
}

// blockRangeWhileSnapSyncing announces block range updates during snap sync.
// Here we poll the CurrentSnapBlock on a timer and announce updates to it.
func (h *handler) blockRangeWhileSnapSyncing(st *blockRangeState) {
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			st.update(h.chain, h.chain.CurrentSnapBlock())
			if st.shouldSend() {
				h.broadcastBlockRange(st)
			}
		// back to processing head block updates when sync is done
		case ev := <-st.syncSub.Chan():
			if ev == nil {
				continue
			}
			switch ev.Data.(type) {
			case downloader.FailedEvent, downloader.DoneEvent:
				return
			}
		// ignore head updates, but exit when the subscription ends
		case <-st.headCh:
		case <-st.headSub.Err():
			return
		}
	}
}

// broadcastBlockRange sends a range update when one is due.
func (h *handler) broadcastBlockRange(state *blockRangeState) {
	h.peers.lock.Lock()
	peerlist := slices.Collect(maps.Values(h.peers.peers))
	h.peers.lock.Unlock()
	if len(peerlist) == 0 {
		return
	}
	msg := state.currentRange()
	log.Debug("Sending BlockRangeUpdate", "peers", len(peerlist), "earliest", msg.EarliestBlock, "latest", msg.LatestBlock)
	for _, p := range peerlist {
		p.SendBlockRangeUpdate(msg)
	}
	state.prev = *state.next.Load()
}

// update assigns the values of the next block range update from the chain.
func (st *blockRangeState) update(chain *core.BlockChain, latest *types.Header) {
	earliest, _ := chain.HistoryPruningCutoff()
	st.next.Store(&eth.BlockRangeUpdatePacket{
		EarliestBlock:   min(latest.Number.Uint64(), earliest),
		LatestBlock:     latest.Number.Uint64(),
		LatestBlockHash: latest.Hash(),
	})
}

// shouldSend decides whether it is time to send a block range update. We don't want to
// send these updates constantly, so they will usually only be sent every 32 blocks.
// However, there is a special case: if the range would move back, i.e. due to SetHead, we
// want to send it immediately.
func (st *blockRangeState) shouldSend() bool {
	next := st.next.Load()
	return next.LatestBlock < st.prev.LatestBlock ||
		next.LatestBlock-st.prev.LatestBlock >= 32
}

func (st *blockRangeState) stop() {
	st.syncSub.Unsubscribe()
	st.headSub.Unsubscribe()
}

// currentRange returns the current block range.
// This is safe to call from any goroutine.
func (st *blockRangeState) currentRange() eth.BlockRangeUpdatePacket {
	return *st.next.Load()
}

// broadcastChoice implements a deterministic random choice of peers. This is designed
// specifically for choosing which peer receives a direct broadcast of a transaction.
//
// The choice is made based on the involved p2p node IDs and the transaction sender,
// ensuring that the flow of transactions is grouped by account to (try and) avoid nonce
// gaps.
type broadcastChoice struct {
	self   enode.ID
	key    [16]byte
	buffer map[*ethPeer]struct{}
	tmp    []broadcastPeer
}

type broadcastPeer struct {
	p     *ethPeer
	score uint64
}

func newBroadcastChoiceKey() (k [16]byte) {
	crand.Read(k[:])
	return k
}

func newBroadcastChoice(self enode.ID, key [16]byte) *broadcastChoice {
	return &broadcastChoice{
		self:   self,
		key:    key,
		buffer: make(map[*ethPeer]struct{}),
	}
}

// choosePeers selects the peers that will receive a direct transaction broadcast message.
// Note the return value will only stay valid until the next call to choosePeers.
func (bc *broadcastChoice) choosePeers(peers []*ethPeer, txSender common.Address) map[*ethPeer]struct{} {
	// Compute randomized scores.
	bc.tmp = slices.Grow(bc.tmp[:0], len(peers))[:len(peers)]
	hash := siphash.New(bc.key[:])
	for i, peer := range peers {
		hash.Reset()
		hash.Write(bc.self[:])
		hash.Write(peer.Peer.Peer.ID().Bytes())
		hash.Write(txSender[:])
		bc.tmp[i] = broadcastPeer{peer, hash.Sum64()}
	}

	// Sort by score.
	slices.SortFunc(bc.tmp, func(a, b broadcastPeer) int {
		return cmp.Compare(a.score, b.score)
	})

	// Take top n.
	clear(bc.buffer)
	n := int(math.Ceil(math.Sqrt(float64(len(bc.tmp)))))
	for i := range n {
		bc.buffer[bc.tmp[i].p] = struct{}{}
	}
	return bc.buffer
}
