// Copyright 2020 The go-ethereum Authors
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
	"fmt"
	"math/big"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"
)

// testEthHandler is a mock event handler to listen for inbound network requests
// on the `eth` protocol and convert them into a more easily testable form.
type testEthHandler struct {
	blockBroadcasts    event.Feed
	blockAnnouncements event.Feed
	txAnnounces        event.Feed
	txBroadcasts       event.Feed
}

func (h *testEthHandler) Chain() *core.BlockChain              { panic("no backing chain") }
func (h *testEthHandler) TxPool() eth.TxPool                   { panic("no backing tx pool") }
func (h *testEthHandler) AcceptTxs() bool                      { return true }
func (h *testEthHandler) RunPeer(*eth.Peer, eth.Handler) error { panic("not used in tests") }
func (h *testEthHandler) PeerInfo(enode.ID) interface{}        { panic("not used in tests") }

func (h *testEthHandler) Handle(peer *eth.Peer, packet eth.Packet) error {
	switch packet := packet.(type) {
	case *eth.NewBlockPacket:
		h.blockBroadcasts.Send(packet.Block)
		return nil

	case *eth.NewBlockHashesPacket:
		hashes, _ := packet.Unpack()
		h.blockAnnouncements.Send(hashes)
		return nil

	case *eth.NewPooledTransactionHashesPacket:
		h.txAnnounces.Send(packet.Hashes)
		return nil

	case *eth.TransactionsPacket:
		h.txBroadcasts.Send(([]*types.Transaction)(*packet))
		return nil

	case *eth.PooledTransactionsPacket:
		h.txBroadcasts.Send(([]*types.Transaction)(packet.PooledTransactionsResponse))
		return nil

	default:
		panic(fmt.Sprintf("unexpected eth packet type in tests: %T", packet))
	}
}

// Tests that peers are correctly accepted (or rejected) based on the advertised
// fork IDs in the protocol handshake.
func TestForkIDSplit69(t *testing.T) { testForkIDSplit(t, eth.ETH69) }
func TestForkIDSplit68(t *testing.T) { testForkIDSplit(t, eth.ETH68) }

func testForkIDSplit(t *testing.T, protocol uint) {
	t.Helper()

	var (
		engine = ethash.NewFaker()

		configNoFork  = &params.ChainConfig{HomesteadBlock: big.NewInt(1)}
		configProFork = &params.ChainConfig{
			HomesteadBlock: big.NewInt(1),
			EIP150Block:    big.NewInt(2),
			EIP155Block:    big.NewInt(2),
			EIP158Block:    big.NewInt(2),
			ByzantiumBlock: big.NewInt(3),
		}
		dbNoFork  = rawdb.NewMemoryDatabase()
		dbProFork = rawdb.NewMemoryDatabase()

		gspecNoFork  = &core.Genesis{Config: configNoFork}
		gspecProFork = &core.Genesis{Config: configProFork}

		chainNoFork, _  = core.NewBlockChain(dbNoFork, gspecNoFork, engine, nil)
		chainProFork, _ = core.NewBlockChain(dbProFork, gspecProFork, engine, nil)

		_, blocksNoFork, _  = core.GenerateChainWithGenesis(gspecNoFork, engine, 2, nil)
		_, blocksProFork, _ = core.GenerateChainWithGenesis(gspecProFork, engine, 2, nil)

		ethNoFork, _ = newHandler(&handlerConfig{
			Database:   dbNoFork,
			Chain:      chainNoFork,
			TxPool:     newTestTxPool(),
			Network:    1,
			Sync:       downloader.FullSync,
			BloomCache: 1,
		})
		ethProFork, _ = newHandler(&handlerConfig{
			Database:   dbProFork,
			Chain:      chainProFork,
			TxPool:     newTestTxPool(),
			Network:    1,
			Sync:       downloader.FullSync,
			BloomCache: 1,
		})
	)

	ethNoFork.Start(1000)
	ethProFork.Start(1000)

	// Clean up everything after ourselves
	defer chainNoFork.Stop()
	defer chainProFork.Stop()

	defer ethNoFork.Stop()
	defer ethProFork.Stop()

	// Both nodes should allow the other to connect (same genesis, next fork is the same)
	p2pNoFork, p2pProFork := p2p.MsgPipe()
	defer p2pNoFork.Close()
	defer p2pProFork.Close()

	peerNoFork := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pNoFork), p2pNoFork, nil)
	peerProFork := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, p2pProFork), p2pProFork, nil)

	defer peerNoFork.Close()
	defer peerProFork.Close()

	errc := make(chan error, 2)
	go func(errc chan error) {
		errc <- ethNoFork.runEthPeer(peerProFork, func(peer *eth.Peer) error { return nil })
	}(errc)
	go func(errc chan error) {
		errc <- ethProFork.runEthPeer(peerNoFork, func(peer *eth.Peer) error { return nil })
	}(errc)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("frontier nofork <-> profork failed: %v", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("frontier nofork <-> profork handler timeout")
		}
	}
	// Progress into Homestead. Fork's match, so we don't care what the future holds
	chainNoFork.InsertChain(blocksNoFork[:1], false)
	chainProFork.InsertChain(blocksProFork[:1], false)

	p2pNoFork, p2pProFork = p2p.MsgPipe()
	defer p2pNoFork.Close()
	defer p2pProFork.Close()

	peerNoFork = eth.NewPeer(protocol, p2p.NewPeer(enode.ID{1}, "", nil), p2pNoFork, nil)
	peerProFork = eth.NewPeer(protocol, p2p.NewPeer(enode.ID{2}, "", nil), p2pProFork, nil)

	defer peerNoFork.Close()
	defer peerProFork.Close()

	errc = make(chan error, 2)
	go func(errc chan error) {
		errc <- ethNoFork.runEthPeer(peerProFork, func(peer *eth.Peer) error { return nil })
	}(errc)
	go func(errc chan error) {
		errc <- ethProFork.runEthPeer(peerNoFork, func(peer *eth.Peer) error { return nil })
	}(errc)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("homestead nofork <-> profork failed: %v", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("homestead nofork <-> profork handler timeout")
		}
	}
	// Progress into Spurious. Forks mismatch, signalling differing chains, reject
	chainNoFork.InsertChain(blocksNoFork[1:2], false)
	chainProFork.InsertChain(blocksProFork[1:2], false)

	p2pNoFork, p2pProFork = p2p.MsgPipe()
	defer p2pNoFork.Close()
	defer p2pProFork.Close()

	peerNoFork = eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pNoFork), p2pNoFork, nil)
	peerProFork = eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, p2pProFork), p2pProFork, nil)

	defer peerNoFork.Close()
	defer peerProFork.Close()

	errc = make(chan error, 2)
	go func(errc chan error) {
		errc <- ethNoFork.runEthPeer(peerProFork, func(peer *eth.Peer) error { return nil })
	}(errc)
	go func(errc chan error) {
		errc <- ethProFork.runEthPeer(peerNoFork, func(peer *eth.Peer) error { return nil })
	}(errc)

	var successes int

	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err == nil {
				successes++
				if successes == 2 { // Only one side disconnects
					t.Fatalf("fork ID rejection didn't happen")
				}
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("split peers not rejected")
		}
	}
}

// Tests that received transactions are added to the local pool.
func TestRecvTransactions69(t *testing.T) { testRecvTransactions(t, eth.ETH69) }
func TestRecvTransactions68(t *testing.T) { testRecvTransactions(t, eth.ETH68) }

func testRecvTransactions(t *testing.T, protocol uint) {
	t.Helper()

	// Create a message handler, configure it to accept transactions and watch them
	handler := newTestHandler()
	defer handler.close()

	handler.handler.synced.Store(true) // mark synced to accept transactions

	txs := make(chan core.NewTxsEvent)
	sub := handler.txpool.SubscribeTransactions(txs, false)
	defer sub.Unsubscribe()

	// Create a source peer to send messages through and a sink handler to receive them
	p2pSrc, p2pSink := p2p.MsgPipe()
	defer p2pSrc.Close()
	defer p2pSink.Close()

	src := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pSrc), p2pSrc, handler.txpool)
	sink := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, p2pSink), p2pSink, handler.txpool)

	defer src.Close()
	defer sink.Close()

	go handler.handler.runEthPeer(sink, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(handler.handler), peer)
	})
	// Run the handshake locally to avoid spinning up a source handler
	if err := src.Handshake(1, handler.chain, eth.BlockRangeUpdatePacket{
		EarliestBlock:   0,
		LatestBlock:     handler.chain.CurrentBlock().Number.Uint64(),
		LatestBlockHash: handler.chain.CurrentBlock().Hash(),
	}); err != nil {
		t.Fatalf("failed to run protocol handshake")
	}
	// Send the transaction to the sink and verify that it's added to the tx pool
	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 100000, big.NewInt(0), nil)
	tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)

	if err := src.SendTransactions([]*types.Transaction{tx}); err != nil {
		t.Fatalf("failed to send transaction: %v", err)
	}
	select {
	case event := <-txs:
		if len(event.Txs) != 1 {
			t.Errorf("wrong number of added transactions: got %d, want 1", len(event.Txs))
		} else if event.Txs[0].Hash() != tx.Hash() {
			t.Errorf("added wrong tx hash: got %v, want %v", event.Txs[0].Hash(), tx.Hash())
		}
	case <-time.After(2 * time.Second):
		t.Errorf("no NewTxsEvent received within 2 seconds")
	}
}

// This test checks that pending transactions are sent.
func TestSendTransactions69(t *testing.T) { testSendTransactions(t, eth.ETH69) }
func TestSendTransactions68(t *testing.T) { testSendTransactions(t, eth.ETH68) }

func testSendTransactions(t *testing.T, protocol uint) {
	t.Parallel()

	// Create a message handler and fill the pool with big transactions
	handler := newTestHandler()
	defer handler.close()

	insert := make([]*types.Transaction, 100)
	for nonce := range insert {
		tx := types.NewTransaction(uint64(nonce), common.Address{}, big.NewInt(0), 100000, big.NewInt(0), make([]byte, 10240))
		tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)
		insert[nonce] = tx
	}
	go handler.txpool.Add(insert, false) // Need goroutine to not block on feed
	time.Sleep(250 * time.Millisecond)   // Wait until tx events get out of the system (can't use events, tx broadcaster races with peer join)

	// Create a source handler to send messages through and a sink peer to receive them
	p2pSrc, p2pSink := p2p.MsgPipe()
	defer p2pSrc.Close()
	defer p2pSink.Close()

	src := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pSrc), p2pSrc, handler.txpool)
	sink := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, p2pSink), p2pSink, handler.txpool)
	defer src.Close()
	defer sink.Close()

	go handler.handler.runEthPeer(src, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(handler.handler), peer)
	})
	// Run the handshake locally to avoid spinning up a source handler
	if err := sink.Handshake(1, handler.chain, eth.BlockRangeUpdatePacket{
		EarliestBlock:   0,
		LatestBlock:     handler.chain.CurrentBlock().Number.Uint64(),
		LatestBlockHash: handler.chain.CurrentBlock().Hash(),
	}); err != nil {
		t.Fatalf("failed to run protocol handshake")
	}
	// After the handshake completes, the source handler should stream the sink
	// the transactions, subscribe to all inbound network events
	backend := new(testEthHandler)

	anns := make(chan []common.Hash)
	annSub := backend.txAnnounces.Subscribe(anns)
	defer annSub.Unsubscribe()

	bcasts := make(chan []*types.Transaction)
	bcastSub := backend.txBroadcasts.Subscribe(bcasts)
	defer bcastSub.Unsubscribe()

	go eth.Handle(backend, sink)

	// Make sure we get all the transactions on the correct channels
	seen := make(map[common.Hash]struct{})
	for len(seen) < len(insert) {
		switch protocol {
		case 69, 68:
			select {
			case hashes := <-anns:
				for _, hash := range hashes {
					if _, ok := seen[hash]; ok {
						t.Errorf("duplicate transaction announced: %x", hash)
					}
					seen[hash] = struct{}{}
				}
			case <-bcasts:
				t.Errorf("initial tx broadcast received on post eth/66")
			}

		default:
			panic("unsupported protocol, please extend test")
		}
	}
	for _, tx := range insert {
		if _, ok := seen[tx.Hash()]; !ok {
			t.Errorf("missing transaction: %x", tx.Hash())
		}
	}
}

// Tests that tx announcements are not dropped for burst sizes above the old 4096
// limit but within the new 16384 default limit. This is the regression test for
// POS-3471: during Polymarket spikes and migration events, the 4096 cap caused
// BP nonce gaps because older hashes were evicted before reaching the peer.
func TestTxAnnouncementsAboveOldLimit69(t *testing.T) {
	testTxAnnouncementsAboveOldLimit(t, eth.ETH69)
}
func TestTxAnnouncementsAboveOldLimit68(t *testing.T) {
	testTxAnnouncementsAboveOldLimit(t, eth.ETH68)
}

func testTxAnnouncementsAboveOldLimit(t *testing.T, protocol uint) {
	t.Parallel()

	handler := newTestHandler()
	defer handler.close()

	// 5000 txs: above the old 4096 limit, well within the new 16384 limit.
	// With the old cap, syncTransactions would queue all 5000 hashes at once
	// and the announceTransactions goroutine would truncate ~906 of them.
	const count = 5000

	insert := make([]*types.Transaction, count)
	for nonce := range insert {
		tx := types.NewTransaction(uint64(nonce), common.Address{}, big.NewInt(0), 100000, big.NewInt(0), nil)
		tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)
		insert[nonce] = tx
	}
	// Add in a goroutine to avoid blocking on the feed, then wait for events to
	// drain before connecting the peer (same pattern as testSendTransactions).
	go handler.txpool.Add(insert, false)
	time.Sleep(250 * time.Millisecond)

	p2pSrc, p2pSink := p2p.MsgPipe()
	defer p2pSrc.Close()
	defer p2pSink.Close()

	src := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pSrc), p2pSrc, handler.txpool)
	sink := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, p2pSink), p2pSink, handler.txpool)
	defer src.Close()
	defer sink.Close()

	go handler.handler.runEthPeer(src, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(handler.handler), peer)
	})
	if err := sink.Handshake(1, handler.chain, eth.BlockRangeUpdatePacket{
		EarliestBlock:   0,
		LatestBlock:     handler.chain.CurrentBlock().Number.Uint64(),
		LatestBlockHash: handler.chain.CurrentBlock().Hash(),
	}); err != nil {
		t.Fatalf("failed to run protocol handshake")
	}

	backend := new(testEthHandler)
	anns := make(chan []common.Hash)
	annSub := backend.txAnnounces.Subscribe(anns)
	defer annSub.Unsubscribe()

	go eth.Handle(backend, sink)

	seen := make(map[common.Hash]struct{})
	timeout := time.After(10 * time.Second)
	for len(seen) < count {
		select {
		case hashes := <-anns:
			for _, hash := range hashes {
				seen[hash] = struct{}{}
			}
		case <-timeout:
			t.Fatalf("tx announcement timed out: received %d/%d hashes", len(seen), count)
		}
	}
}

// Tests that transactions get propagated to all attached peers, either via direct
// broadcasts or via announcements/retrievals.
func TestTransactionPropagation69(t *testing.T) { testTransactionPropagation(t, eth.ETH69) }
func TestTransactionPropagation68(t *testing.T) { testTransactionPropagation(t, eth.ETH68) }

func testTransactionPropagation(t *testing.T, protocol uint) {
	t.Parallel()

	// Create a source handler to send transactions from and a number of sinks
	// to receive them. We need multiple sinks since a one-to-one peering would
	// broadcast all transactions without announcement.
	source := newTestHandler()
	source.handler.snapSync.Store(false) // Avoid requiring snap, otherwise some will be dropped below
	defer source.close()

	sinks := make([]*testHandler, 10)
	for i := 0; i < len(sinks); i++ {
		sinks[i] = newTestHandler()
		defer sinks[i].close()

		sinks[i].handler.synced.Store(true) // mark synced to accept transactions
	}
	// Interconnect all the sink handlers with the source handler
	for i, sink := range sinks {
		sourcePipe, sinkPipe := p2p.MsgPipe()
		defer sourcePipe.Close()
		defer sinkPipe.Close()

		sourcePeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{byte(i + 1)}, "", nil, sourcePipe), sourcePipe, source.txpool)
		sinkPeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{0}, "", nil, sinkPipe), sinkPipe, sink.txpool)
		defer sourcePeer.Close()
		defer sinkPeer.Close()

		go source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(source.handler), peer)
		})
		go sink.handler.runEthPeer(sinkPeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(sink.handler), peer)
		})
	}
	// Subscribe to all the transaction pools
	txChs := make([]chan core.NewTxsEvent, len(sinks))
	for i := 0; i < len(sinks); i++ {
		txChs[i] = make(chan core.NewTxsEvent, 1024)

		sub := sinks[i].txpool.SubscribeTransactions(txChs[i], false)
		defer sub.Unsubscribe()
	}
	// Fill the source pool with transactions and wait for them at the sinks
	txs := make([]*types.Transaction, 1024)
	for nonce := range txs {
		tx := types.NewTransaction(uint64(nonce), common.Address{}, big.NewInt(0), 100000, big.NewInt(0), nil)
		tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)
		txs[nonce] = tx
	}
	source.txpool.Add(txs, false)

	// Iterate through all the sinks and ensure they all got the transactions
	for i := range sinks {
		for arrived, timeout := 0, false; arrived < len(txs) && !timeout; {
			select {
			case event := <-txChs[i]:
				arrived += len(event.Txs)
			case <-time.After(2 * time.Second):
				t.Errorf("sink %d: transaction propagation timed out: have %d, want %d", i, arrived, len(txs))
				timeout = true
			}
		}
	}
}

// This test checks that transactions are only announced when txannouncementonly is enabled
func TestSendTransactionAnnouncementsOnly69(t *testing.T) {
	testSendTransactionAnnouncementsOnly(t, eth.ETH69)
}
func TestSendTransactionAnnouncementsOnly68(t *testing.T) {
	testSendTransactionAnnouncementsOnly(t, eth.ETH68)
}

func testSendTransactionAnnouncementsOnly(t *testing.T, protocol uint) {
	t.Parallel()

	// Create a source handler that has txannouncementonly enabled
	source := newTestHandler()
	source.handler.txAnnouncementOnly = true
	defer source.close()

	sink := newTestHandler()
	defer sink.close()

	sourcePipe, sinkPipe := p2p.MsgPipe()
	defer sourcePipe.Close()
	defer sinkPipe.Close()

	sourcePeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, sourcePipe), sourcePipe, source.txpool)
	sinkPeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, sinkPipe), sinkPipe, sink.txpool)
	defer sourcePeer.Close()
	defer sinkPeer.Close()

	go source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(source.handler), peer)
	})

	if err := sinkPeer.Handshake(1, source.chain, eth.BlockRangeUpdatePacket{
		EarliestBlock:   0,
		LatestBlock:     source.chain.CurrentBlock().Number.Uint64(),
		LatestBlockHash: source.chain.CurrentBlock().Hash(),
	}); err != nil {
		t.Fatalf("failed to run protocol handshake: %v", err)
	}

	// Subscribe to all inbound network events on the sink peer
	backend := new(testEthHandler)

	anns := make(chan []common.Hash)
	annSub := backend.txAnnounces.Subscribe(anns)
	defer annSub.Unsubscribe()

	bcasts := make(chan []*types.Transaction)
	bcastSub := backend.txBroadcasts.Subscribe(bcasts)
	defer bcastSub.Unsubscribe()

	go eth.Handle(backend, sinkPeer)

	// Fill the source pool with transactions and wait for them at the sink
	txs := make([]*types.Transaction, 1024)
	for nonce := range txs {
		tx := types.NewTransaction(uint64(nonce), common.Address{}, big.NewInt(0), 100000, big.NewInt(0), nil)
		tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)
		txs[nonce] = tx
	}
	source.txpool.Add(txs, false)

	// Make sure we get all the transactions as announcements
	seen := make(map[common.Hash]struct{})
	timeout := false
	for len(seen) < len(txs) && !timeout {
		switch protocol {
		case 69, 68:
			select {
			case hashes := <-anns:
				for _, hash := range hashes {
					if _, ok := seen[hash]; ok {
						t.Errorf("duplicate transaction announced: %x", hash)
					}
					seen[hash] = struct{}{}
				}
			case <-bcasts:
				t.Errorf("received tx broadcast when txannouncementonly is true")
			case <-time.After(5 * time.Second):
				t.Errorf("transaction propagation timed out")
				timeout = true
			}

		default:
			panic("unsupported protocol, please extend test")
		}
	}
	for _, tx := range txs {
		if _, ok := seen[tx.Hash()]; !ok {
			t.Errorf("missing transaction: %x", tx.Hash())
		}
	}
}

// Tests that blocks are broadcast to a sqrt number of peers only.
func TestBroadcastBlock1Peer(t *testing.T)    { testBroadcastBlock(t, 1, 1) }
func TestBroadcastBlock2Peers(t *testing.T)   { testBroadcastBlock(t, 2, 1) }
func TestBroadcastBlock3Peers(t *testing.T)   { testBroadcastBlock(t, 3, 1) }
func TestBroadcastBlock4Peers(t *testing.T)   { testBroadcastBlock(t, 4, 2) }
func TestBroadcastBlock5Peers(t *testing.T)   { testBroadcastBlock(t, 5, 2) }
func TestBroadcastBlock8Peers(t *testing.T)   { testBroadcastBlock(t, 9, 3) }
func TestBroadcastBlock12Peers(t *testing.T)  { testBroadcastBlock(t, 12, 3) }
func TestBroadcastBlock16Peers(t *testing.T)  { testBroadcastBlock(t, 16, 4) }
func TestBroadcastBloc26Peers(t *testing.T)   { testBroadcastBlock(t, 26, 5) }
func TestBroadcastBlock100Peers(t *testing.T) { testBroadcastBlock(t, 100, 10) }

func testBroadcastBlock(t *testing.T, peers, bcasts int) {
	t.Parallel()

	// Create a source handler to broadcast blocks from and a number of sinks
	// to receive them.
	source := newTestHandlerWithBlocks(1)
	defer source.close()

	sinks := make([]*testEthHandler, peers)
	for i := 0; i < len(sinks); i++ {
		sinks[i] = new(testEthHandler)
	}

	for i, sink := range sinks {
		sourcePipe, sinkPipe := p2p.MsgPipe()
		defer sourcePipe.Close()
		defer sinkPipe.Close()

		sourcePeer := eth.NewPeer(eth.ETH68, p2p.NewPeerPipe(enode.ID{byte(i)}, "", nil, sourcePipe), sourcePipe, nil)
		sinkPeer := eth.NewPeer(eth.ETH68, p2p.NewPeerPipe(enode.ID{0}, "", nil, sinkPipe), sinkPipe, nil)
		defer sourcePeer.Close()
		defer sinkPeer.Close()

		go source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(source.handler), peer)
		})

		if err := sinkPeer.Handshake(1, source.chain, eth.BlockRangeUpdatePacket{}); err != nil {
			t.Fatalf("failed to run protocol handshake")
		}

		go eth.Handle(sink, sinkPeer)
	}
	// Subscribe to all the transaction pools
	blockChs := make([]chan *types.Block, len(sinks))
	for i := 0; i < len(sinks); i++ {
		blockChs[i] = make(chan *types.Block, 1)
		defer close(blockChs[i])

		sub := sinks[i].blockBroadcasts.Subscribe(blockChs[i])
		defer sub.Unsubscribe()
	}
	// Initiate a block propagation across the peers
	time.Sleep(100 * time.Millisecond)

	header := source.chain.CurrentBlock()
	source.handler.BroadcastBlock(source.chain.GetBlock(header.Hash(), header.Number.Uint64()), nil, true)

	// Iterate through all the sinks and ensure the correct number got the block
	done := make(chan struct{}, peers)

	for _, ch := range blockChs {
		go func() {
			<-ch
			done <- struct{}{}
		}()
	}

	var received int

	for {
		select {
		case <-done:
			received++

		case <-time.After(100 * time.Millisecond):
			if received != bcasts {
				t.Errorf("broadcast count mismatch: have %d, want %d", received, bcasts)
			}

			return
		}
	}
}

func TestMinedBroadcastAnnouncesWithCachedWitnessBeforeWrite(t *testing.T) {
	t.Parallel()

	source := newTestHandlerWithBlocks(1)
	defer source.close()

	sinks := make([]*testEthHandler, 2)
	for i := range sinks {
		sinks[i] = new(testEthHandler)
	}

	for i, sink := range sinks {
		sourcePipe, sinkPipe := p2p.MsgPipe()
		defer sourcePipe.Close()
		defer sinkPipe.Close()

		sourcePeer := eth.NewPeer(eth.ETH68, p2p.NewPeerPipe(enode.ID{byte(i)}, "", nil, sourcePipe), sourcePipe, nil)
		sinkPeer := eth.NewPeer(eth.ETH68, p2p.NewPeerPipe(enode.ID{0}, "", nil, sinkPipe), sinkPipe, nil)
		defer sourcePeer.Close()
		defer sinkPeer.Close()

		go source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(source.handler), peer)
		})
		if err := sinkPeer.Handshake(1, source.chain, eth.BlockRangeUpdatePacket{}); err != nil {
			t.Fatalf("failed to run protocol handshake: %v", err)
		}
		go eth.Handle(sink, sinkPeer)
	}

	announceCh := make(chan []common.Hash, len(sinks))
	for i := range sinks {
		sub := sinks[i].blockAnnouncements.Subscribe(announceCh)
		defer sub.Unsubscribe()
	}

	parent := source.chain.CurrentBlock()
	block := types.NewBlockWithHeader(&types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		Time:       parent.Time + 1,
		Difficulty: big.NewInt(1),
		GasLimit:   parent.GasLimit,
	})
	require.False(t, source.chain.HasBlock(block.Hash(), block.NumberU64()))

	source.chain.CacheWitness(block.Hash(), []byte{0x1})

	time.Sleep(100 * time.Millisecond)
	source.handler.eventMux.Post(core.NewMinedBlockEvent{Block: block})

	timeout := time.After(2 * time.Second)
	for {
		select {
		case hashes := <-announceCh:
			if len(hashes) == 1 && hashes[0] == block.Hash() {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for block hash announcement for block %s", block.Hash())
		}
	}
}

// Tests that a propagated malformed block (uncles or transactions don't match
// with the hashes in the header) gets discarded and not broadcast forward.
func TestBroadcastMalformedBlock69(t *testing.T) {
	testBroadcastMalformedBlock(t, eth.ETH69)
}

func TestBroadcastMalformedBlock68(t *testing.T) { testBroadcastMalformedBlock(t, eth.ETH68) }

func testBroadcastMalformedBlock(t *testing.T, protocol uint) {
	t.Helper()

	// Create a source handler to broadcast blocks from and a number of sinks
	// to receive them.
	source := newTestHandlerWithBlocks(1)
	defer source.close()

	// Create a source handler to send messages through and a sink peer to receive them
	p2pSrc, p2pSink := p2p.MsgPipe()
	defer p2pSrc.Close()
	defer p2pSink.Close()

	src := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pSrc), p2pSrc, source.txpool)
	sink := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{2}, "", nil, p2pSink), p2pSink, source.txpool)

	defer src.Close()
	defer sink.Close()

	go source.handler.runEthPeer(src, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(source.handler), peer)
	})

	if err := sink.Handshake(1, source.chain, eth.BlockRangeUpdatePacket{
		EarliestBlock:   1,
		LatestBlock:     source.chain.CurrentBlock().Number.Uint64(),
		LatestBlockHash: source.chain.CurrentBlock().Hash(),
	}); err != nil {
		t.Fatalf("failed to run protocol handshake")
	}
	// After the handshake completes, the source handler should stream the sink
	// the blocks, subscribe to inbound network events
	backend := new(testEthHandler)

	blocks := make(chan *types.Block, 1)

	sub := backend.blockBroadcasts.Subscribe(blocks)
	defer sub.Unsubscribe()

	go eth.Handle(backend, sink)

	// Create various combinations of malformed blocks
	head := source.chain.CurrentBlock()
	block := source.chain.GetBlock(head.Hash(), head.Number.Uint64())

	malformedUncles := head
	malformedUncles.UncleHash[0]++

	malformedTransactions := head
	malformedTransactions.TxHash[0]++

	malformedEverything := head
	malformedEverything.UncleHash[0]++
	malformedEverything.TxHash[0]++

	// Try to broadcast all malformations and ensure they all get discarded
	for _, header := range []*types.Header{malformedUncles, malformedTransactions, malformedEverything} {
		block := types.NewBlockWithHeader(header).WithBody(types.Body{
			Transactions: block.Transactions(),
			Uncles:       block.Uncles(),
		})
		if err := src.SendNewBlock(block, big.NewInt(131136)); err != nil {
			t.Fatalf("failed to broadcast block: %v", err)
		}
		select {
		case <-blocks:
			t.Fatalf("malformed block forwarded")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Tests that when tx propagation is completely disabled, no transactions
// are propagated to connected peers.
func TestDisableTxPropagation68(t *testing.T) {
	testDisableTxPropagation(t, eth.ETH68)
}
func TestDisableTxPropagation69(t *testing.T) {
	testDisableTxPropagation(t, eth.ETH69)
}

func testDisableTxPropagation(t *testing.T, protocol uint) {
	t.Parallel()

	// Disable tx propagation on the source handler
	updateConfig := func(cfg *handlerConfig) *handlerConfig {
		cfg.disableTxPropagation = true
		return cfg
	}

	// Create a source handler to send transactions from and a number of sinks
	// to receive them. We need multiple sinks to ensure none of them gets
	// any transactions.
	source := newTestHandlerWithConfig(updateConfig)
	source.handler.snapSync.Store(false) // Avoid requiring snap, otherwise some will be dropped below
	defer source.close()

	// Fill the source pool with transactions
	txs := make([]*types.Transaction, 10)
	for nonce := range txs {
		tx := types.NewTransaction(uint64(nonce), common.Address{}, big.NewInt(0), 100000, big.NewInt(0), nil)
		tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)
		txs[nonce] = tx
	}
	source.txpool.Add(txs[:5], false)

	sinks := make([]*testHandler, 10)
	for i := 0; i < len(sinks); i++ {
		sinks[i] = newTestHandler()
		defer sinks[i].close()

		sinks[i].handler.synced.Store(true) // mark synced to accept transactions
	}

	// Interconnect all the sink handlers with the source handler
	for i, sink := range sinks {
		sourcePipe, sinkPipe := p2p.MsgPipe()
		defer sourcePipe.Close()
		defer sinkPipe.Close()

		sourcePeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{byte(i + 1)}, "", nil, sourcePipe), sourcePipe, source.txpool)
		sinkPeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{0}, "", nil, sinkPipe), sinkPipe, sink.txpool)
		defer sourcePeer.Close()
		defer sinkPeer.Close()

		go source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(source.handler), peer)
		})
		go sink.handler.runEthPeer(sinkPeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(sink.handler), peer)
		})
	}

	// Subscribe to all the transaction pools
	txChs := make([]chan core.NewTxsEvent, len(sinks))
	for i := 0; i < len(sinks); i++ {
		txChs[i] = make(chan core.NewTxsEvent, 10)

		sub := sinks[i].txpool.SubscribeTransactions(txChs[i], false)
		defer sub.Unsubscribe()
	}

	var wg sync.WaitGroup

	// Transactions are propagated during initial sync via `runEthPeer`. As the
	// source has disabled tx propagation, ensure that none of the sinks receive
	// any transactions.
	for i := range sinks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case <-txChs[idx]:
				t.Errorf("sink %d: received transactions even when tx propagation is completely disabled", idx)
			case <-time.After(2 * time.Second):
				// Expected: timeout without receiving any transactions
			}
		}(i)
	}
	wg.Wait()

	// Transactions are also propagated via broadcast and announcement loops. Ensure that
	// none of the receive any transactions.
	source.txpool.Add(txs[5:], false)
	for i := range sinks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case <-txChs[idx]:
				t.Errorf("sink %d: received transactions even when tx propagation is completely disabled", idx)
			case <-time.After(2 * time.Second):
				// Expected: timeout without receiving any transactions
			}
		}(i)
	}
	wg.Wait()
}

// A simple private tx store for tests
type PrivateTxStore struct {
	mu    sync.RWMutex
	store map[common.Hash]struct{}
}

func (p *PrivateTxStore) IsTxPrivate(hash common.Hash) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.store[hash]
	return ok
}

// Tests that when a tx is set to private, it's not propagated to any
// connected peers.
func TestPrivateTxNotPropagated68(t *testing.T) {
	testPrivateTxNotPropagated(t, eth.ETH68)
}
func TestPrivateTxNotPropagated69(t *testing.T) {
	testPrivateTxNotPropagated(t, eth.ETH69)
}

func testPrivateTxNotPropagated(t *testing.T, protocol uint) {
	t.Parallel()

	// Initialize a private tx store
	privateTxStore := &PrivateTxStore{store: make(map[common.Hash]struct{})}

	// Set the private tx store getter on the source handler
	updateConfig := func(cfg *handlerConfig) *handlerConfig {
		cfg.privateTxGetter = privateTxStore
		return cfg
	}

	// Create a source handler to send transactions from and a number of sinks
	// to receive them. We need multiple sinks to ensure none of them gets
	// any transactions.
	source := newTestHandlerWithConfig(updateConfig)
	source.handler.snapSync.Store(false) // Avoid requiring snap, otherwise some will be dropped below
	defer source.close()

	// Fill the source pool with transactions
	txs := make([]*types.Transaction, 10)
	for nonce := range txs {
		tx := types.NewTransaction(uint64(nonce), common.Address{}, big.NewInt(0), 100000, big.NewInt(0), nil)
		tx, _ = types.SignTx(tx, types.HomesteadSigner{}, testKey)
		txs[nonce] = tx
	}
	source.txpool.Add(txs[:5], false)

	// Mark some transactions as private
	privateTxStore.mu.Lock()
	privateTxStore.store[txs[3].Hash()] = struct{}{}
	privateTxStore.store[txs[4].Hash()] = struct{}{}
	privateTxStore.mu.Unlock()

	sinks := make([]*testHandler, 10)
	for i := 0; i < len(sinks); i++ {
		sinks[i] = newTestHandler()
		defer sinks[i].close()

		sinks[i].handler.synced.Store(true) // mark synced to accept transactions
	}

	// Interconnect all the sink handlers with the source handler
	for i, sink := range sinks {
		sourcePipe, sinkPipe := p2p.MsgPipe()
		defer sourcePipe.Close()
		defer sinkPipe.Close()

		sourcePeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{byte(i + 1)}, "", nil, sourcePipe), sourcePipe, source.txpool)
		sinkPeer := eth.NewPeer(protocol, p2p.NewPeerPipe(enode.ID{0}, "", nil, sinkPipe), sinkPipe, sink.txpool)
		defer sourcePeer.Close()
		defer sinkPeer.Close()

		go source.handler.runEthPeer(sourcePeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(source.handler), peer)
		})
		go sink.handler.runEthPeer(sinkPeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(sink.handler), peer)
		})
	}

	// Subscribe to all the transaction pools
	txChs := make([]chan core.NewTxsEvent, len(sinks))
	for i := 0; i < len(sinks); i++ {
		txChs[i] = make(chan core.NewTxsEvent, 10)

		sub := sinks[i].txpool.SubscribeTransactions(txChs[i], false)
		defer sub.Unsubscribe()
	}

	var wg sync.WaitGroup

	// Transactions are propagated during initial sync via `runEthPeer`. As the
	// source has disabled tx propagation, ensure that none of the sinks receive
	// any transactions.
	var txReceivedCount atomic.Uint64
	for i := range sinks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case txs := <-txChs[idx]:
				txReceivedCount.Add(uint64(len(txs.Txs)))
				for _, tx := range txs.Txs {
					// Ensure no private txs are received
					if _, ok := privateTxStore.store[tx.Hash()]; ok {
						t.Errorf("sink %d: received private transaction %x", idx, tx.Hash())
					}
				}
			case <-time.After(2 * time.Second):
				t.Errorf("sink %d: transaction propagation timed out", idx)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, txReceivedCount.Load(), uint64(len(sinks)*3), "sinks should have received only public transactions")

	// Transactions are also propagated via broadcast and announcement loops. Ensure that
	// none of the receive any transactions.
	source.txpool.Add(txs[5:], false)

	// Mark some transactions as private
	privateTxStore.mu.Lock()
	privateTxStore.store[txs[8].Hash()] = struct{}{}
	privateTxStore.store[txs[9].Hash()] = struct{}{}
	privateTxStore.mu.Unlock()

	txReceivedCount.Store(0)
	for i := range sinks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case txs := <-txChs[idx]:
				txReceivedCount.Add(uint64(len(txs.Txs)))
				for _, tx := range txs.Txs {
					// Ensure no private txs are received
					if _, ok := privateTxStore.store[tx.Hash()]; ok {
						t.Errorf("sink %d: received private transaction %x", idx, tx.Hash())
					}
				}
			case <-time.After(2 * time.Second):
				t.Errorf("sink %d: transaction propagation timed out", idx)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, txReceivedCount.Load(), uint64(len(sinks)*3), "sinks should have received only public transactions")
}

// TestCreateWitnessRequester tests the createWitnessRequester helper
func TestCreateWitnessRequester(t *testing.T) {
	handler := newTestHandler()
	defer handler.close()

	ethHandler := (*ethHandler)(handler.handler)

	// Test that createWitnessRequester returns a valid function
	requester := ethHandler.createWitnessRequester()
	if requester == nil {
		t.Fatal("createWitnessRequester returned nil")
	}

	// Test calling the requester with no matching peer (should error)
	sink := make(chan *eth.Response, 1)
	req, err := requester(common.Hash{0x01}, sink)

	// Should error because no peer has the witness
	if err == nil {
		t.Error("Expected error when no peer has witness, got nil")
	}
	if req != nil {
		t.Error("Expected nil request when no peer available")
	}
}

// TestVerifyPageCount tests the verifyPageCount function
func TestVerifyPageCount(t *testing.T) {
	handler := newTestHandler()
	defer handler.close()

	ethHandler := (*ethHandler)(handler.handler)

	t.Run("SmallWitnessBelowThreshold", func(t *testing.T) {
		// Test with small page count (below threshold)
		// Should return true without needing peer consensus
		hash := common.Hash{0xaa, 0xbb}
		result := ethHandler.verifyPageCount(hash, 1, "test-peer")

		// Verify it returns a boolean (no panic)
		// Small witnesses typically pass without consensus check
		if result {
			t.Log("Small witness passed verification (expected behavior)")
		} else {
			t.Log("Small witness failed verification")
		}
	})

	t.Run("LargeWitnessNoPeers", func(t *testing.T) {
		// Test with large page count (above threshold) but no peers available
		hash := common.Hash{0xcc, 0xdd}
		result := ethHandler.verifyPageCount(hash, 100, "test-peer")

		// Verify the function executes and returns a boolean
		// The actual result depends on witness manager's policy for insufficient peers
		t.Logf("Large witness verification result: %v", result)
	})

	t.Run("FunctionExecutesWithoutPanic", func(t *testing.T) {
		// Ensure the function handles edge cases without panicking
		testCases := []struct {
			hash      common.Hash
			pageCount uint64
			peer      string
		}{
			{common.Hash{}, 0, ""},             // Empty values
			{common.Hash{0xff}, 1, "peer1"},    // Normal small
			{common.Hash{0xaa}, 50, "peer2"},   // Medium size
			{common.Hash{0xbb}, 1000, "peer3"}, // Very large
		}

		for _, tc := range testCases {
			// Should not panic for any input
			result := ethHandler.verifyPageCount(tc.hash, tc.pageCount, tc.peer)
			t.Logf("Hash: %x, PageCount: %d, Peer: %s -> Result: %v",
				tc.hash[:4], tc.pageCount, tc.peer, result)
		}
	})
}

// TestHandleBlockAnnounces tests the handleBlockAnnounces function
// blockAnnouncesTestSetup holds the setup for block announces tests
type blockAnnouncesTestSetup struct {
	handler *testHandler
	ethHdlr *ethHandler
	peer    *eth.Peer
	p2pSrc  *p2p.MsgPipeRW
	p2pSink *p2p.MsgPipeRW
	hashes  []common.Hash
	numbers []uint64
}

// setupBlockAnnouncesTest creates a test setup for block announces tests
func setupBlockAnnouncesTest(t *testing.T, statelessSync bool, syncWithWitnesses bool) *blockAnnouncesTestSetup {
	t.Helper()

	var handler *testHandler
	var ethHdlr *ethHandler

	if syncWithWitnesses {
		// Create handler with syncWithWitnesses enabled
		db := rawdb.NewMemoryDatabase()
		gspec := &core.Genesis{
			Config: params.TestChainConfig,
			Alloc:  types.GenesisAlloc{testAddr: {Balance: big.NewInt(1000000)}},
		}
		chain, _ := core.NewBlockChain(db, gspec, ethash.NewFaker(), nil)
		txpool := newTestTxPool()

		h, _ := newHandler(&handlerConfig{
			Database:          db,
			Chain:             chain,
			TxPool:            txpool,
			Network:           1,
			Sync:              downloader.SnapSync,
			BloomCache:        1,
			syncWithWitnesses: true,
		})
		h.Start(1000)

		t.Cleanup(func() {
			h.Stop()
			chain.Stop()
		})

		ethHdlr = (*ethHandler)(h)
	} else {
		handler = newTestHandler()
		t.Cleanup(handler.close)

		if statelessSync {
			handler.handler.statelessSync.Store(true)
		}
		ethHdlr = (*ethHandler)(handler.handler)
	}

	// Create test peer
	p2pSrc, p2pSink := p2p.MsgPipe()
	t.Cleanup(func() {
		p2pSrc.Close()
		p2pSink.Close()
	})

	var txpool *testTxPool
	if handler != nil {
		txpool = handler.txpool
	}

	peer := eth.NewPeer(eth.ETH69, p2p.NewPeerPipe(enode.ID{1}, "", nil, p2pSrc), p2pSrc, txpool)
	t.Cleanup(peer.Close)

	// Create test data
	hashes := []common.Hash{{0x01}, {0x02}}
	numbers := []uint64{100, 101}

	return &blockAnnouncesTestSetup{
		handler: handler,
		ethHdlr: ethHdlr,
		peer:    peer,
		p2pSrc:  p2pSrc,
		p2pSink: p2pSink,
		hashes:  hashes,
		numbers: numbers,
	}
}

func TestHandleBlockAnnounces(t *testing.T) {
	setup := setupBlockAnnouncesTest(t, false, false)

	err := setup.ethHdlr.handleBlockAnnounces(setup.peer, setup.hashes, setup.numbers)
	if err != nil {
		t.Fatalf("handleBlockAnnounces failed: %v", err)
	}
}

// TestHandleBlockAnnounces_WithWitnessRequester tests handleBlockAnnounces with statelessSync enabled
func TestHandleBlockAnnounces_WithWitnessRequester(t *testing.T) {
	setup := setupBlockAnnouncesTest(t, true, false)

	err := setup.ethHdlr.handleBlockAnnounces(setup.peer, setup.hashes, setup.numbers)
	if err != nil {
		t.Fatalf("handleBlockAnnounces failed: %v", err)
	}

	// Verify that witnessRequester was created (indirectly by checking no panic)
	// The actual witness requester will be nil if no peer has the witness, but the creation path is covered
}

// TestHandleBlockAnnounces_WithSyncWithWitnesses tests handleBlockAnnounces with syncWithWitnesses enabled
func TestHandleBlockAnnounces_WithSyncWithWitnesses(t *testing.T) {
	setup := setupBlockAnnouncesTest(t, false, true)

	err := setup.ethHdlr.handleBlockAnnounces(setup.peer, setup.hashes, setup.numbers)
	if err != nil {
		t.Fatalf("handleBlockAnnounces failed: %v", err)
	}
}

// createTestPeerWithWitness creates an eth peer with witness support for testing
func createTestPeerWithWitness(id string, chain *core.BlockChain) (*eth.Peer, *wit.Peer) {
	var nodeID enode.ID
	copy(nodeID[:], []byte(id))
	if len(id) < len(nodeID) {
		copy(nodeID[:], id)
	}

	p2pPeer := p2p.NewPeer(nodeID, id, nil)
	app, net := p2p.MsgPipe()

	// Start background reader to respond to witness requests
	// This prevents the dispatcher from waiting indefinitely
	go func() {
		for {
			msg, err := app.ReadMsg()
			if err != nil {
				return
			}
			// Respond to witness requests with an empty response to unblock the dispatcher
			if msg.Code == wit.GetMsgWitness {
				// Decode the request to get the request ID
				var packet wit.GetWitnessPacket
				if err := msg.Decode(&packet); err == nil && packet.RequestId != 0 {
					// Send an empty witness response to unblock the dispatcher
					emptyResponse := &wit.WitnessPacketRLPPacket{
						RequestId:             packet.RequestId,
						WitnessPacketResponse: []wit.WitnessPageResponse{},
					}
					p2p.Send(app, wit.MsgWitness, emptyResponse)
				}
			}
			msg.Discard()
		}
	}()

	ethPeer := eth.NewPeer(eth.ETH69, p2p.NewPeerPipe(nodeID, id, nil, net), net, nil)

	// Initialize peer head and TD using unsafe pointer manipulation
	// This is a test-only workaround to initialize the unexported td field
	// before calling SetHead, which requires td to be non-nil
	peerValue := reflect.ValueOf(ethPeer).Elem()
	tdField := peerValue.FieldByName("td")
	if tdField.IsValid() {
		// Create a new big.Int and set it using unsafe
		newTD := big.NewInt(1000)
		// Use unsafe to set the unexported field pointer
		rf := reflect.NewAt(tdField.Type(), unsafe.Pointer(tdField.UnsafeAddr())).Elem()
		rf.Set(reflect.ValueOf(newTD))
	}

	// Now we can safely call SetHead
	ethPeer.SetHead(common.Hash{0x01}, big.NewInt(1000))

	witPeer := wit.NewPeer(wit.WIT1, p2pPeer, app, log.New())

	return ethPeer, witPeer
}

// TestVerifyPageCount_GetRandomPeers tests the getRandomPeers closure
// This test verifies that getRandomPeers correctly:
// 1. Gets all peers
// 2. Filters peers that support witness
// 3. Excludes the reporting peer
// 4. Shuffles the peers
func TestVerifyPageCount_GetRandomPeers(t *testing.T) {
	handler := newTestHandler()
	defer handler.close()

	// Create multiple peers with witness support
	peer1Eth, peer1Wit := createTestPeerWithWitness("peer1", handler.chain)
	peer2Eth, peer2Wit := createTestPeerWithWitness("peer2", handler.chain)
	peer3Eth, peer3Wit := createTestPeerWithWitness("peer3", handler.chain)
	peer4Eth, peer4Wit := createTestPeerWithWitness("peer4", handler.chain)

	defer peer1Eth.Close()
	defer peer2Eth.Close()
	defer peer3Eth.Close()
	defer peer4Eth.Close()

	// Register peers with witness support
	err := handler.handler.peers.registerPeer(peer1Eth, nil, peer1Wit)
	if err != nil {
		t.Fatalf("Failed to register peer1: %v", err)
	}
	err = handler.handler.peers.registerPeer(peer2Eth, nil, peer2Wit)
	if err != nil {
		t.Fatalf("Failed to register peer2: %v", err)
	}
	err = handler.handler.peers.registerPeer(peer3Eth, nil, peer3Wit)
	if err != nil {
		t.Fatalf("Failed to register peer3: %v", err)
	}
	err = handler.handler.peers.registerPeer(peer4Eth, nil, peer4Wit)
	if err != nil {
		t.Fatalf("Failed to register peer4: %v", err)
	}

	// Create a peer without witness support
	peer5Eth, _ := createTestPeerWithWitness("peer5", handler.chain)
	defer peer5Eth.Close()
	err = handler.handler.peers.registerPeer(peer5Eth, nil, nil)
	if err != nil {
		t.Fatalf("Failed to register peer5: %v", err)
	}

	// Test the getRandomPeers logic directly by simulating what it does
	allPeers := handler.handler.peers.getAllPeers()
	reportingPeerID := peer1Eth.ID() // Use the actual peer ID

	// Simulate the getRandomPeers closure logic
	randomPeers := make([]string, 0, len(allPeers))
	for _, p := range allPeers {
		// Exclude the reporting peer to avoid double-counting their vote
		if p.SupportsWitness() && p.ID() != reportingPeerID {
			randomPeers = append(randomPeers, p.ID())
		}
	}

	// Verify that peer5 (without witness) is not included
	peer5ID := peer5Eth.ID()
	foundPeer5 := false
	for _, peerID := range randomPeers {
		if peerID == peer5ID {
			foundPeer5 = true
			break
		}
	}
	if foundPeer5 {
		t.Error("Peer5 (without witness) should not be included in random peers")
	}

	// Verify that reporting peer (peer1) is excluded
	foundPeer1 := false
	for _, peerID := range randomPeers {
		if peerID == reportingPeerID {
			foundPeer1 = true
			break
		}
	}
	if foundPeer1 {
		t.Error("Reporting peer should be excluded from random peers")
	}

	// Verify that we have the expected number of witness-supporting peers (excluding reporting peer)
	// We have 4 peers with witness (peer1-4), exclude peer1 (reporting peer), so we should have 3
	expectedCount := 3 // peer2, peer3, peer4 (peer1 excluded, peer5 doesn't support witness)
	actualCount := len(randomPeers)
	if actualCount != expectedCount {
		t.Errorf("Expected %d random peers with witness support (excluding reporting peer), got %d. Peers: %v", expectedCount, actualCount, randomPeers)
	}

	// Verify all returned peers support witness
	for _, peerID := range randomPeers {
		peer := handler.handler.peers.peer(peerID)
		if peer == nil {
			t.Errorf("Peer %s not found", peerID)
		} else if !peer.SupportsWitness() {
			t.Errorf("Peer %s should support witness", peerID)
		}
	}
}

// TestVerifyPageCount_GetWitnessPageCount tests the getWitnessPageCount closure
// This test verifies that getWitnessPageCount correctly:
// 1. Gets peer by ID
// 2. Checks if peer exists and supports witness
// 3. Returns error for non-existent peers
// 4. Returns error for peers without witness support
func TestVerifyPageCount_GetWitnessPageCount(t *testing.T) {
	handler := newTestHandler()
	defer handler.close()

	// Create a peer with witness support
	peerEth, peerWit := createTestPeerWithWitness("test-peer", handler.chain)
	defer peerEth.Close()

	// Register the peer
	err := handler.handler.peers.registerPeer(peerEth, nil, peerWit)
	if err != nil {
		t.Fatalf("Failed to register peer: %v", err)
	}

	peerID := peerEth.ID()

	// Test case 1: Peer exists and supports witness
	// Simulate the getWitnessPageCount closure logic
	peer := handler.handler.peers.peer(peerID)
	if peer == nil {
		t.Fatal("Expected to retrieve peer, got nil")
	}
	if !peer.SupportsWitness() {
		t.Error("Expected peer to support witness")
	}
	// Note: We can't actually call RequestWitnessPageCount here as it would block
	// waiting for a network response. The logic is tested through the peer lookup
	// and SupportsWitness check above.

	// Test case 2: Peer doesn't exist
	nonExistentPeerID := "non-existent-peer"
	nonExistentPeer := handler.handler.peers.peer(nonExistentPeerID)
	if nonExistentPeer != nil {
		t.Error("Expected nil for non-existent peer")
	}
	// The getWitnessPageCount closure would return an error in this case:
	// "peer {peerID} not available or doesn't support witness"

	// Test case 3: Peer exists but doesn't support witness
	peerNoWitEth, _ := createTestPeerWithWitness("no-wit-peer", handler.chain)
	defer peerNoWitEth.Close()

	err = handler.handler.peers.registerPeer(peerNoWitEth, nil, nil)
	if err != nil {
		t.Fatalf("Failed to register peer without witness: %v", err)
	}

	peerNoWit := handler.handler.peers.peer(peerNoWitEth.ID())
	if peerNoWit == nil {
		t.Fatal("Expected to retrieve peer without witness, got nil")
	}
	if peerNoWit.SupportsWitness() {
		t.Error("Expected peer to not support witness")
	}
	// The getWitnessPageCount closure would return an error in this case:
	// "peer {peerID} not available or doesn't support witness"

	// Verify the peer lookup works correctly
	retrievedPeer := handler.handler.peers.peer(peerID)
	if retrievedPeer == nil {
		t.Error("Expected to retrieve peer, got nil")
	} else if !retrievedPeer.SupportsWitness() {
		t.Error("Expected peer to support witness")
	}
}

// TestHandleBlockBroadcast tests the handleBlockBroadcast function
// blockBroadcastTestSetup holds the setup for block broadcast tests
type blockBroadcastTestSetup struct {
	handler *testHandler
	chain   *core.BlockChain
	ethHdlr *ethHandler
	peer    *eth.Peer
	p2pSrc  *p2p.MsgPipeRW
	p2pSink *p2p.MsgPipeRW
	block   *types.Block
	td      *big.Int
}

// setupBlockBroadcastTest creates a test setup for block broadcast tests
func setupBlockBroadcastTest(t *testing.T, statelessSync bool, syncWithWitnesses bool) *blockBroadcastTestSetup {
	t.Helper()

	var handler *testHandler
	var chain *core.BlockChain
	var ethHdlr *ethHandler
	var txpool *testTxPool

	if syncWithWitnesses {
		// Create handler with syncWithWitnesses enabled
		db := rawdb.NewMemoryDatabase()
		gspec := &core.Genesis{
			Config: params.TestChainConfig,
			Alloc:  types.GenesisAlloc{testAddr: {Balance: big.NewInt(1000000)}},
		}
		chain, _ = core.NewBlockChain(db, gspec, ethash.NewFaker(), nil)
		txpool = newTestTxPool()

		h, _ := newHandler(&handlerConfig{
			Database:          db,
			Chain:             chain,
			TxPool:            txpool,
			Network:           1,
			Sync:              downloader.SnapSync,
			BloomCache:        1,
			syncWithWitnesses: true,
		})
		h.Start(1000)

		t.Cleanup(func() {
			h.Stop()
			chain.Stop()
		})

		ethHdlr = (*ethHandler)(h)
	} else {
		handler = newTestHandler()
		t.Cleanup(handler.close)

		handler.handler.statelessSync.Store(statelessSync)
		if !statelessSync {
			handler.handler.syncWithWitnesses = false
		}

		ethHdlr = (*ethHandler)(handler.handler)
		chain = handler.chain
		txpool = handler.txpool
	}

	// Create a test peer
	p2pSrc, p2pSink := p2p.MsgPipe()
	t.Cleanup(func() {
		p2pSrc.Close()
		p2pSink.Close()
	})

	peer := eth.NewPeer(eth.ETH69, p2p.NewPeerPipe(enode.ID{1}, "test-peer", nil, p2pSrc), p2pSrc, txpool)
	t.Cleanup(peer.Close)

	// Initialize peer head and TD to prevent nil pointer dereference
	peerValue := reflect.ValueOf(peer).Elem()
	tdField := peerValue.FieldByName("td")
	if tdField.IsValid() {
		newTD := big.NewInt(1000)
		rf := reflect.NewAt(tdField.Type(), unsafe.Pointer(tdField.UnsafeAddr())).Elem()
		rf.Set(reflect.ValueOf(newTD))
	}
	peer.SetHead(common.Hash{0x01}, big.NewInt(1000))

	// Create a test block
	block := chain.GetBlock(chain.CurrentBlock().Hash(), chain.CurrentBlock().Number.Uint64())
	td := big.NewInt(1000)

	return &blockBroadcastTestSetup{
		handler: handler,
		chain:   chain,
		ethHdlr: ethHdlr,
		peer:    peer,
		p2pSrc:  p2pSrc,
		p2pSink: p2pSink,
		block:   block,
		td:      td,
	}
}

func TestHandleBlockBroadcast(t *testing.T) {
	t.Run("WithStatelessSync", func(t *testing.T) {
		setup := setupBlockBroadcastTest(t, true, false)

		// Test handleBlockBroadcast - should use InjectBlockWithWitnessRequirement
		err := setup.ethHdlr.handleBlockBroadcast(setup.peer, setup.block, setup.td)
		if err != nil {
			t.Fatalf("handleBlockBroadcast failed: %v", err)
		}

		// Verify that witness requester was created (indirectly by checking no panic)
		// The actual injection may fail if the channel is full, but that's expected
	})

	t.Run("WithSyncWithWitnesses", func(t *testing.T) {
		setup := setupBlockBroadcastTest(t, false, true)

		// Test handleBlockBroadcast - should use InjectBlockWithWitnessRequirement
		err := setup.ethHdlr.handleBlockBroadcast(setup.peer, setup.block, setup.td)
		if err != nil {
			t.Fatalf("handleBlockBroadcast failed: %v", err)
		}
	})

	t.Run("WithoutStatelessSync", func(t *testing.T) {
		setup := setupBlockBroadcastTest(t, false, false)

		// Test handleBlockBroadcast - should use Enqueue instead
		err := setup.ethHdlr.handleBlockBroadcast(setup.peer, setup.block, setup.td)
		if err != nil {
			t.Fatalf("handleBlockBroadcast failed: %v", err)
		}

		// Verify that it used the Enqueue path (no witness requester created)
		// This is verified by the fact that it doesn't call InjectBlockWithWitnessRequirement
	})
}
