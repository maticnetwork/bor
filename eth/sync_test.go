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
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
)

// Tests that snap sync is disabled after a successful sync cycle.
func TestSnapSyncDisabling69(t *testing.T) { testSnapSyncDisabling(t, eth.ETH69, snap.SNAP1) }

// Skipping as eth/68 nodes are filtered out during snap sync
// func TestSnapSyncDisabling68(t *testing.T) { testSnapSyncDisabling(t, eth.ETH68, snap.SNAP1) }

func TestChainSyncerNextSyncOpStates(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	syncer.doneCh = make(chan error, 1)
	if op, wait := syncer.nextSyncOp(); op != nil || wait != 0 {
		t.Fatalf("running sync mismatch: op %v wait %v, want nil/0", op, wait)
	}

	syncer.doneCh = nil
	peer := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}

	op, wait := syncer.nextSyncOp()
	if op == nil {
		t.Fatal("expected sync operation")
	}
	if op.peer.ID() != peer.ID() {
		t.Fatalf("sync peer mismatch: have %v, want %v", op.peer.ID(), peer.ID())
	}
	if wait != 0 {
		t.Fatalf("sync wait mismatch: have %v, want 0", wait)
	}
}

func TestChainSyncerNextSyncOpSkipsBackedOffPeer(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	peer := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}

	setDownloaderPeerBackoff(t, handler.downloader, peer.ID(), time.Hour)
	op, wait := syncer.nextSyncOp()
	if op != nil {
		t.Fatalf("expected no sync op while peer backed off, got %v", op)
	}
	if wait <= 0 || wait > time.Hour {
		t.Fatalf("retry wait mismatch: have %v, want (0, 1h]", wait)
	}

	setDownloaderPeerBackoff(t, handler.downloader, peer.ID(), 0)
	op, _ = syncer.nextSyncOp()
	if op == nil {
		t.Fatal("expected sync op after backoff cleared")
	}
}

func TestChainSyncerCoolsOffOnPeersUnavailable(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	syncer.force = time.NewTimer(forceSyncCycle)
	defer syncer.force.Stop()

	peer := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}

	syncer.onSyncDone(downloader.ErrPeersUnavailable)
	if op, wait := syncer.nextSyncOp(); op != nil || wait <= 0 {
		t.Fatalf("expected no sync op and a positive cooldown wait, got op=%v wait=%v", op, wait)
	}

	syncer.peersUnavailableUntil = time.Time{}
	if op, _ := syncer.nextSyncOp(); op == nil {
		t.Fatal("expected a sync op once the peers-unavailable cooldown is cleared")
	}
}

func TestChainSyncerArmsRetryForBenchedHigherTDPeer(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	syncer.forced = true

	low := registerPeerWithTD(t, handler.peers, 0)
	if err := handler.downloader.RegisterPeer(low.ID(), eth.ETH68, &ethPeer{Peer: low}); err != nil {
		t.Fatal(err)
	}
	high := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(high.ID(), eth.ETH68, &ethPeer{Peer: high}); err != nil {
		t.Fatal(err)
	}
	setDownloaderPeerBackoff(t, handler.downloader, high.ID(), time.Hour)

	op, wait := syncer.nextSyncOp()
	if op != nil {
		t.Fatalf("expected no sync op when in sync with the only eligible peer, got %v", op)
	}
	if wait <= 0 {
		t.Fatal("retry timer must be armed for the benched higher-TD peer's backoff expiry")
	}
}

func TestChainSyncerCoolsOffOnPeerBackedOff(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	syncer.force = time.NewTimer(forceSyncCycle)
	defer syncer.force.Stop()

	peer := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}

	syncer.onSyncDone(downloader.ErrPeerBackedOff)
	if op, wait := syncer.nextSyncOp(); op != nil || wait <= 0 {
		t.Fatalf("ErrPeerBackedOff must arm the retry cooldown, got op=%v wait=%v", op, wait)
	}
}

func TestChainSyncerCoolsOffOnNoRemote(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	syncer.force = time.NewTimer(forceSyncCycle)
	defer syncer.force.Stop()

	peer := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}

	syncer.onSyncDone(whitelist.ErrNoRemote)
	if op, wait := syncer.nextSyncOp(); op != nil || wait <= 0 {
		t.Fatalf("ErrNoRemote must arm the retry cooldown without benching, got op=%v wait=%v", op, wait)
	}
}

func TestChainSyncerCooldownSurvivesBlockAnnounce(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	syncer := newChainSyncer(handler)
	syncer.force = time.NewTimer(forceSyncCycle)
	defer syncer.force.Stop()

	peer := registerPeerWithTD(t, handler.peers, 1_000_000)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}

	syncer.onSyncDone(downloader.ErrPeersUnavailable)
	if syncer.peersUnavailableUntil.IsZero() {
		t.Fatal("cooldown should be armed after ErrPeersUnavailable")
	}

	syncer.onPeerEvent()
	if syncer.peersUnavailableUntil.IsZero() {
		t.Fatal("a peer event with no peer-set change (e.g. a block announce) must not clear the cooldown")
	}

	peer2 := registerPeerWithTD(t, handler.peers, 2_000_000)
	if err := handler.downloader.RegisterPeer(peer2.ID(), eth.ETH68, &ethPeer{Peer: peer2}); err != nil {
		t.Fatal(err)
	}
	if err := handler.peers.unregisterPeer(peer.ID()); err != nil {
		t.Fatal(err)
	}
	if handler.peers.len() != 1 {
		t.Fatalf("peer replacement should preserve peer count, have %d", handler.peers.len())
	}
	syncer.onPeerEvent()
	if !syncer.peersUnavailableUntil.IsZero() {
		t.Fatal("a peer replacement must clear the cooldown")
	}
}

func TestChainSyncerLoopRetriesBackedOffPeer(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	peer := registerPeerWithTD(t, handler.peers, 0)
	if err := handler.downloader.RegisterPeer(peer.ID(), eth.ETH68, &ethPeer{Peer: peer}); err != nil {
		t.Fatal(err)
	}
	setDownloaderPeerBackoff(t, handler.downloader, peer.ID(), 5*time.Millisecond)

	handler.wg.Add(1)
	done := make(chan struct{})
	go func() {
		handler.chainSync.loop()
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for handler.downloader.PeerBackoff(peer.ID()) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(handler.quitSync)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("chain syncer did not stop")
	}
}

func TestResettableTimer(t *testing.T) {
	rt := newResettableTimer()

	select {
	case <-rt.C():
		t.Fatal("fresh timer should not fire")
	case <-time.After(20 * time.Millisecond):
	}

	rt.reset(10 * time.Millisecond)
	select {
	case <-rt.C():
	case <-time.After(time.Second):
		t.Fatal("reset timer did not fire")
	}
	rt.markFired()

	rt.reset(10 * time.Millisecond)
	rt.stop()
	select {
	case <-rt.C():
		t.Fatal("stopped timer should not fire")
	case <-time.After(30 * time.Millisecond):
	}

	rt.reset(0)
	select {
	case <-rt.C():
		t.Fatal("reset(0) should not arm the timer")
	case <-time.After(20 * time.Millisecond):
	}

	rt.reset(5 * time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	rt.markFired()
	rt.stop()
	select {
	case <-rt.C():
	default:
		t.Fatal("stop on inactive timer should leave the pending tick untouched")
	}
}

func TestResettableTimerActiveState(t *testing.T) {
	rt := newResettableTimer()
	if rt.active {
		t.Fatal("a fresh timer must be inactive")
	}

	rt.reset(time.Hour)
	if !rt.active {
		t.Fatal("reset with a positive wait must arm the timer")
	}

	rt.stop()
	if rt.active {
		t.Fatal("stop must mark the timer inactive")
	}

	rt.reset(time.Hour)
	rt.markFired()
	if rt.active {
		t.Fatal("markFired must mark the timer inactive")
	}
	rt.stop()
}

func TestResettableTimerResetDrainsStaleTick(t *testing.T) {
	rt := newResettableTimer()
	rt.reset(5 * time.Millisecond)
	time.Sleep(25 * time.Millisecond)

	rt.reset(time.Hour)
	select {
	case <-rt.C():
		t.Fatal("reset must drain a stale tick before re-arming the timer")
	case <-time.After(40 * time.Millisecond):
	}
	rt.stop()
}

func TestChainSyncerOnSyncDone(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	cs := newChainSyncer(handler)
	cs.force = time.NewTimer(time.Hour)
	defer cs.force.Stop()
	cs.doneCh = make(chan error, 1)
	cs.forced = true

	cs.onSyncDone(nil)

	if cs.doneCh != nil {
		t.Fatal("onSyncDone should clear doneCh")
	}
	if cs.forced {
		t.Fatal("onSyncDone should reset forced to false")
	}
}

func TestChainSyncerOnSyncDoneMergeWarning(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	newSyncer := func() *chainSyncer {
		cs := newChainSyncer(handler)
		cs.force = time.NewTimer(time.Hour)
		t.Cleanup(func() { cs.force.Stop() })
		return cs
	}

	t.Run("recent warning is not refreshed", func(t *testing.T) {
		cs := newSyncer()
		before := time.Now()
		cs.warned = before

		cs.onSyncDone(downloader.ErrMergeTransition)

		if !cs.warned.Equal(before) {
			t.Fatalf("recent warning timestamp should be untouched: have %v, want %v", cs.warned, before)
		}
	})

	t.Run("stale warning is refreshed", func(t *testing.T) {
		cs := newSyncer()
		before := time.Now().Add(-11 * time.Second)
		cs.warned = before

		cs.onSyncDone(downloader.ErrMergeTransition)

		if !cs.warned.After(before) {
			t.Fatalf("stale warning timestamp should be refreshed: have %v, want after %v", cs.warned, before)
		}
	})

	t.Run("non-merge error never warns", func(t *testing.T) {
		cs := newSyncer()
		before := time.Now().Add(-11 * time.Second)
		cs.warned = before

		cs.onSyncDone(errors.New("some other failure"))

		if !cs.warned.Equal(before) {
			t.Fatalf("non-merge error should not touch warning timestamp: have %v, want %v", cs.warned, before)
		}
	})
}

func TestChainSyncerShutdownReturnsWithoutPendingSync(t *testing.T) {
	handler, cleanup := newChainSyncerTestHandler(t)
	defer cleanup()

	cs := newChainSyncer(handler)
	done := make(chan struct{})
	go func() {
		cs.shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return")
	}
}

func newChainSyncerTestHandler(t *testing.T) (*handler, func()) {
	t.Helper()

	db := rawdb.NewMemoryDatabase()
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{testAddr: {Balance: big.NewInt(1000000)}},
	}
	chain, err := core.NewBlockChain(db, genesis, ethash.NewFaker(), nil)
	if err != nil {
		t.Fatal(err)
	}

	handler, err := newHandler(&handlerConfig{
		Database:   db,
		Chain:      chain,
		TxPool:     newTestTxPool(),
		Network:    1,
		Sync:       downloader.SnapSync,
		BloomCache: 1,
	})
	if err != nil {
		chain.Stop()
		db.Close()
		t.Fatal(err)
	}
	return handler, func() {
		handler.downloader.Terminate()
		chain.Stop()
		db.Close()
	}
}

func setDownloaderPeerBackoff(t *testing.T, d *downloader.Downloader, id string, duration time.Duration) {
	t.Helper()

	if !d.SetPeerBackoffForTesting(id, duration) {
		t.Fatalf("downloader peer %q not found", id)
	}
}

// Tests that snap sync gets disabled as soon as a real block is successfully
// imported into the blockchain.
func testSnapSyncDisabling(t *testing.T, ethVer uint, snapVer uint) {
	t.Helper()
	// Create an empty handler and ensure it's in snap sync mode
	empty := newTestHandler()
	if !empty.handler.snapSync.Load() {
		t.Fatalf("snap sync disabled on pristine blockchain")
	}
	defer empty.close()

	// Create a full handler and ensure snap sync ends up disabled
	full := newTestHandlerWithBlocks(1024)
	if full.handler.snapSync.Load() {
		t.Fatalf("snap sync not disabled on non-empty blockchain")
	}
	defer full.close()

	// Sync up the two handlers via both `eth` and `snap`
	caps := []p2p.Cap{{Name: "eth", Version: ethVer}, {Name: "snap", Version: snapVer}}

	emptyPipeEth, fullPipeEth := p2p.MsgPipe()
	defer emptyPipeEth.Close()
	defer fullPipeEth.Close()

	emptyPeerEth := eth.NewPeer(ethVer, p2p.NewPeer(enode.ID{1}, "", caps), emptyPipeEth, empty.txpool)
	fullPeerEth := eth.NewPeer(ethVer, p2p.NewPeer(enode.ID{2}, "", caps), fullPipeEth, full.txpool)

	defer emptyPeerEth.Close()
	defer fullPeerEth.Close()

	go empty.handler.runEthPeer(emptyPeerEth, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(empty.handler), peer)
	})
	go full.handler.runEthPeer(fullPeerEth, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(full.handler), peer)
	})

	emptyPipeSnap, fullPipeSnap := p2p.MsgPipe()
	defer emptyPipeSnap.Close()
	defer fullPipeSnap.Close()

	emptyPeerSnap := snap.NewPeer(snapVer, p2p.NewPeer(enode.ID{1}, "", caps), emptyPipeSnap)
	fullPeerSnap := snap.NewPeer(snapVer, p2p.NewPeer(enode.ID{2}, "", caps), fullPipeSnap)

	go empty.handler.runSnapExtension(emptyPeerSnap, func(peer *snap.Peer) error {
		return snap.Handle((*snapHandler)(empty.handler), peer)
	})
	go full.handler.runSnapExtension(fullPeerSnap, func(peer *snap.Peer) error {
		return snap.Handle((*snapHandler)(full.handler), peer)
	})
	// Wait a bit for the above handlers to start
	time.Sleep(250 * time.Millisecond)

	// Check that snap sync was disabled
	if err := empty.handler.downloader.BeaconSync(downloader.SnapSync, full.chain.CurrentBlock(), nil); err != nil {
		t.Fatal("sync failed:", err)
	}
	// Downloader internally has to wait for a timer (3s) to be expired before
	// exiting. Poll after to determine if sync is disabled.
	time.Sleep(time.Second * 3)
	for timeout := time.After(time.Second); ; {
		select {
		case <-timeout:
			t.Fatalf("snap sync not disabled after successful synchronisation")
		case <-time.After(100 * time.Millisecond):
			if !empty.handler.snapSync.Load() {
				return
			}
		}
	}
}
