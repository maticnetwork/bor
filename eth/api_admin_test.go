// Copyright 2026 The go-ethereum Authors
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
	"net"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	ethproto "github.com/ethereum/go-ethereum/eth/protocols/eth"
	snapproto "github.com/ethereum/go-ethereum/eth/protocols/snap"
	witproto "github.com/ethereum/go-ethereum/eth/protocols/wit"
)

func TestTriggerTxGossipToPeers(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWithBlocks(1)
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	tx, err := api.syntheticTransaction()
	if err != nil {
		t.Fatalf("failed to build synthetic tx: %v", err)
	}
	peerA := &fakeTxGossipPeer{id: "a"}
	peerB := &fakeTxGossipPeer{id: "b"}

	if err := triggerTxGossipToPeers([]txGossipPeer{peerA, peerB}, tx); err != nil {
		t.Fatalf("failed to trigger tx gossip: %v", err)
	}
	for _, peer := range []*fakeTxGossipPeer{peerA, peerB} {
		if len(peer.txs) != 1 {
			t.Fatalf("peer %s received %d tx batches, want 1", peer.id, len(peer.txs))
		}
		if len(peer.txs[0]) != 1 {
			t.Fatalf("peer %s received %d txs, want 1", peer.id, len(peer.txs[0]))
		}
		if peer.txs[0][0].Hash() != tx.Hash() {
			t.Fatalf("peer %s received tx %s, want %s", peer.id, peer.txs[0][0].Hash(), tx.Hash())
		}
	}
}

func TestTriggerTxGossipToPeersPropagatesError(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWithBlocks(1)
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	tx, err := api.syntheticTransaction()
	if err != nil {
		t.Fatalf("failed to build synthetic tx: %v", err)
	}
	wantErr := errors.New("boom")
	if err := triggerTxGossipToPeers([]txGossipPeer{&fakeTxGossipPeer{id: "broken", err: wantErr}}, tx); !errors.Is(err, wantErr) {
		t.Fatalf("unexpected error: got %v want %v", err, wantErr)
	}
}

func TestTriggerBlockAnnouncementToPeers(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0x1234")
	number := uint64(99)
	peerA := &fakeBlockAnnouncementPeer{id: "a"}
	peerB := &fakeBlockAnnouncementPeer{id: "b"}

	if err := triggerBlockAnnouncementToPeers([]blockAnnouncementPeer{peerA, peerB}, hash, number); err != nil {
		t.Fatalf("failed to trigger block announcement: %v", err)
	}
	for _, peer := range []*fakeBlockAnnouncementPeer{peerA, peerB} {
		if len(peer.hashes) != 1 {
			t.Fatalf("peer %s received %d announcement batches, want 1", peer.id, len(peer.hashes))
		}
		if len(peer.hashes[0]) != 1 {
			t.Fatalf("peer %s received %d hashes, want 1", peer.id, len(peer.hashes[0]))
		}
		if peer.hashes[0][0] != hash {
			t.Fatalf("peer %s received hash %s, want %s", peer.id, peer.hashes[0][0], hash)
		}
		if len(peer.numbers) != 1 || len(peer.numbers[0]) != 1 || peer.numbers[0][0] != number {
			t.Fatalf("peer %s received unexpected block number payload", peer.id)
		}
	}
}

func TestAdminAPITargetEthPeersRequiresConnectedPeer(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	if _, err := api.TriggerTxGossip(nil); err == nil {
		t.Fatal("expected missing peer error")
	}
}

func TestTriggerTxFetchToPeers(t *testing.T) {
	t.Parallel()

	hashes := []common.Hash{common.HexToHash("0x1"), common.HexToHash("0x2")}
	peerA := &fakeTxFetchPeer{id: "a"}
	peerB := &fakeTxFetchPeer{id: "b"}

	if err := triggerTxFetchToPeers([]txFetchPeer{peerA, peerB}, hashes); err != nil {
		t.Fatalf("failed to trigger tx fetch: %v", err)
	}
	for _, peer := range []*fakeTxFetchPeer{peerA, peerB} {
		if len(peer.hashes) != 1 {
			t.Fatalf("peer %s received %d tx fetch batches, want 1", peer.id, len(peer.hashes))
		}
		if len(peer.hashes[0]) != len(hashes) {
			t.Fatalf("peer %s received %d tx fetch hashes, want %d", peer.id, len(peer.hashes[0]), len(hashes))
		}
	}
}

func TestTriggerBlockBodyFetchToPeers(t *testing.T) {
	t.Parallel()

	hashes := []common.Hash{common.HexToHash("0x3"), common.HexToHash("0x4")}
	peerA := &fakeBlockBodyFetchPeer{id: "a", respond: true}
	peerB := &fakeBlockBodyFetchPeer{id: "b", respond: true}

	if err := triggerBlockBodyFetchToPeers([]blockBodyFetchPeer{peerA, peerB}, hashes); err != nil {
		t.Fatalf("failed to trigger block body fetch: %v", err)
	}
	if len(peerA.hashes) != 1 || len(peerA.hashes[0]) != 1 || peerA.hashes[0][0] != hashes[0] {
		t.Fatalf("unexpected block body request for peerA: %+v", peerA.hashes)
	}
	if len(peerB.hashes) != 1 || len(peerB.hashes[0]) != 1 || peerB.hashes[0][0] != hashes[1] {
		t.Fatalf("unexpected block body request for peerB: %+v", peerB.hashes)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for (peerA.responsesAcked != 1 || peerB.responsesAcked != 1 || peerA.requestsClosed != 1 || peerB.requestsClosed != 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if peerA.responsesAcked != 1 || peerB.responsesAcked != 1 {
		t.Fatalf("expected response acknowledgements, got peerA=%d peerB=%d", peerA.responsesAcked, peerB.responsesAcked)
	}
	if peerA.requestsClosed != 1 || peerB.requestsClosed != 1 {
		t.Fatalf("expected requests to close, got peerA=%d peerB=%d", peerA.requestsClosed, peerB.requestsClosed)
	}
}

func TestTriggerSnapAccountRangeFetchToPeers(t *testing.T) {
	t.Parallel()

	root := common.HexToHash("0x90")
	peerA := &fakeSnapAccountRangeFetchPeer{id: "a", respond: true}
	peerB := &fakeSnapAccountRangeFetchPeer{id: "b", respond: true}

	if err := triggerSnapAccountRangeFetchToPeers([]snapAccountRangeFetchPeer{peerA, peerB}, root); err != nil {
		t.Fatalf("failed to trigger snap account range fetch: %v", err)
	}
	for _, peer := range []*fakeSnapAccountRangeFetchPeer{peerA, peerB} {
		if len(peer.requests) != 1 {
			t.Fatalf("peer %s received %d snap account requests, want 1", peer.id, len(peer.requests))
		}
		if peer.requests[0].root != root || peer.requests[0].bytes != 4096 {
			t.Fatalf("peer %s received unexpected account request: %+v", peer.id, peer.requests[0])
		}
	}
	assertSnapAcks(t, func() int { return peerA.responsesAcked }, func() int { return peerB.responsesAcked }, "snap account range")
}

func TestTriggerSnapStorageRangeFetchToPeers(t *testing.T) {
	t.Parallel()

	root := common.HexToHash("0x91")
	account := common.HexToHash("0x92")
	peerA := &fakeSnapStorageRangeFetchPeer{id: "a", respond: true}
	peerB := &fakeSnapStorageRangeFetchPeer{id: "b", respond: true}

	if err := triggerSnapStorageRangeFetchToPeers([]snapStorageRangeFetchPeer{peerA, peerB}, root, account); err != nil {
		t.Fatalf("failed to trigger snap storage range fetch: %v", err)
	}
	for _, peer := range []*fakeSnapStorageRangeFetchPeer{peerA, peerB} {
		if len(peer.requests) != 1 {
			t.Fatalf("peer %s received %d snap storage requests, want 1", peer.id, len(peer.requests))
		}
		req := peer.requests[0]
		if req.root != root || req.bytes != 4096 || len(req.accounts) != 1 || req.accounts[0] != account {
			t.Fatalf("peer %s received unexpected storage request: %+v", peer.id, req)
		}
	}
	assertSnapAcks(t, func() int { return peerA.responsesAcked }, func() int { return peerB.responsesAcked }, "snap storage range")
}

func TestTriggerSnapByteCodeFetchToPeers(t *testing.T) {
	t.Parallel()

	codeHash := common.HexToHash("0x93")
	peerA := &fakeSnapByteCodeFetchPeer{id: "a", respond: true}
	peerB := &fakeSnapByteCodeFetchPeer{id: "b", respond: true}

	if err := triggerSnapByteCodeFetchToPeers([]snapByteCodeFetchPeer{peerA, peerB}, codeHash); err != nil {
		t.Fatalf("failed to trigger snap bytecode fetch: %v", err)
	}
	for _, peer := range []*fakeSnapByteCodeFetchPeer{peerA, peerB} {
		if len(peer.requests) != 1 {
			t.Fatalf("peer %s received %d snap bytecode requests, want 1", peer.id, len(peer.requests))
		}
		req := peer.requests[0]
		if req.bytes != 4096 || len(req.hashes) != 1 || req.hashes[0] != codeHash {
			t.Fatalf("peer %s received unexpected bytecode request: %+v", peer.id, req)
		}
	}
	assertSnapAcks(t, func() int { return peerA.responsesAcked }, func() int { return peerB.responsesAcked }, "snap bytecode")
}

func TestTriggerSnapTrieNodeFetchToPeers(t *testing.T) {
	t.Parallel()

	root := common.HexToHash("0x99")
	paths := []snapproto.TrieNodePathSet{{{}}}
	peerA := &fakeSnapTrieNodeFetchPeer{id: "a", respond: true}
	peerB := &fakeSnapTrieNodeFetchPeer{id: "b", respond: true}

	if err := triggerSnapTrieNodeFetchToPeers([]snapTrieNodeFetchPeer{peerA, peerB}, root, paths, 4096); err != nil {
		t.Fatalf("failed to trigger snap trie node fetch: %v", err)
	}
	for _, peer := range []*fakeSnapTrieNodeFetchPeer{peerA, peerB} {
		if len(peer.requests) != 1 {
			t.Fatalf("peer %s received %d snap requests, want 1", peer.id, len(peer.requests))
		}
		if peer.requests[0].root != root {
			t.Fatalf("peer %s received root %s, want %s", peer.id, peer.requests[0].root, root)
		}
		if peer.requests[0].bytes != 4096 {
			t.Fatalf("peer %s received byte cap %d, want 4096", peer.id, peer.requests[0].bytes)
		}
		if len(peer.requests[0].paths) != 1 || len(peer.requests[0].paths[0]) != 1 || len(peer.requests[0].paths[0][0]) != 0 {
			t.Fatalf("peer %s received unexpected trie paths: %#v", peer.id, peer.requests[0].paths)
		}
	}
	assertSnapAcks(t, func() int { return peerA.responsesAcked }, func() int { return peerB.responsesAcked }, "snap trie node")
}

func TestSnapTriggerStorageAccountFallbackUsesSnapshotStorage(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	head := handler.chain.CurrentBlock()
	if head == nil {
		t.Fatal("expected current head")
	}
	account := common.HexToHash("0x1234")
	slot := common.HexToHash("0x5678")
	rawdb.WriteStorageSnapshot(handler.db, account, slot, []byte{0x01})

	storageAccount, err := api.snapTriggerStorageAccountFallback(head.Root)
	if err != nil {
		t.Fatalf("failed to resolve snap trigger storage account fallback: %v", err)
	}
	if storageAccount != account {
		t.Fatalf("unexpected storage account %s, want %s", storageAccount, account)
	}
}

func TestSeedSnapTriggerFixtures(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	res, err := api.SeedSnapTriggerFixtures()
	if err != nil {
		t.Fatalf("failed to seed snap trigger fixtures: %v", err)
	}
	if res.StorageAccount != snapTriggerStorageFixtureAccount || res.StorageSlot != snapTriggerStorageFixtureSlot {
		t.Fatalf("unexpected storage fixture result: %+v", res)
	}
	if res.CodeAccount != snapTriggerCodeFixtureAccount || res.CodeHash != snapTriggerCodeFixtureHash {
		t.Fatalf("unexpected code fixture result: %+v", res)
	}
	samples, err := api.snapTriggerSamples()
	if err != nil {
		t.Fatalf("failed to collect snap trigger samples: %v", err)
	}
	if samples.storageAccount != snapTriggerStorageFixtureAccount {
		t.Fatalf("unexpected seeded storage account %s, want %s", samples.storageAccount, snapTriggerStorageFixtureAccount)
	}
	if samples.codeHash != snapTriggerCodeFixtureHash {
		t.Fatalf("unexpected seeded code hash %s, want %s", samples.codeHash, snapTriggerCodeFixtureHash)
	}
	if code := rawdb.ReadCodeWithPrefix(handler.db, snapTriggerCodeFixtureHash); len(code) == 0 {
		t.Fatal("expected seeded code fixture to be stored")
	}
}

func TestTriggerWitnessAnnouncementToPeers(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0x55")
	number := uint64(44)
	peerA := &fakeWitnessAnnouncementPeer{id: "a"}
	peerB := &fakeWitnessAnnouncementPeer{id: "b"}

	triggerWitnessAnnouncementToPeers([]witnessAnnouncementPeer{peerA, peerB}, hash, number)

	for _, peer := range []*fakeWitnessAnnouncementPeer{peerA, peerB} {
		if len(peer.hashes) != 1 || peer.hashes[0] != hash {
			t.Fatalf("peer %s received unexpected witness hashes: %+v", peer.id, peer.hashes)
		}
		if len(peer.numbers) != 1 || peer.numbers[0] != number {
			t.Fatalf("peer %s received unexpected witness numbers: %+v", peer.id, peer.numbers)
		}
	}
}

func TestTriggerWitnessMetadataFetchToPeers(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0x77")
	peerA := &fakeWitnessMetadataFetchPeer{id: "a", respond: true, available: true}
	peerB := &fakeWitnessMetadataFetchPeer{id: "b", respond: true, available: true}

	if err := triggerWitnessMetadataFetchToPeers([]witnessMetadataFetchPeer{peerA, peerB}, hash, true); err != nil {
		t.Fatalf("failed to trigger witness metadata fetch: %v", err)
	}
	if len(peerA.hashes) != 1 || len(peerA.hashes[0]) != 1 || peerA.hashes[0][0] != hash {
		t.Fatalf("unexpected witness metadata request for peerA: %+v", peerA.hashes)
	}
	if len(peerB.hashes) != 1 || len(peerB.hashes[0]) != 1 || peerB.hashes[0][0] != hash {
		t.Fatalf("unexpected witness metadata request for peerB: %+v", peerB.hashes)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for (peerA.responsesAcked != 1 || peerB.responsesAcked != 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if peerA.responsesAcked != 1 || peerB.responsesAcked != 1 {
		t.Fatalf("expected witness metadata acknowledgements, got peerA=%d peerB=%d", peerA.responsesAcked, peerB.responsesAcked)
	}
}

func TestSeedWitnessForHead(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWithBlocks(1)
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	res, err := api.SeedWitnessForHead()
	if err != nil {
		t.Fatalf("failed to seed witness: %v", err)
	}
	if res.BlockHash == (common.Hash{}) {
		t.Fatal("expected seeded witness hash")
	}
	if !handler.chain.HasWitness(res.BlockHash) {
		t.Fatalf("expected witness store to contain %s", res.BlockHash)
	}
}

func TestTriggerWitnessFetchToPeers(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0x88")
	peerA := &fakeWitnessFetchPeer{id: "a", respond: true}
	peerB := &fakeWitnessFetchPeer{id: "b", respond: true}

	if err := triggerWitnessFetchToPeers([]witnessFetchPeer{peerA, peerB}, hash); err != nil {
		t.Fatalf("failed to trigger witness fetch: %v", err)
	}
	for _, peer := range []*fakeWitnessFetchPeer{peerA, peerB} {
		if len(peer.requests) != 1 || len(peer.requests[0]) != 1 {
			t.Fatalf("peer %s received unexpected witness requests: %+v", peer.id, peer.requests)
		}
		if peer.requests[0][0].Hash != hash || peer.requests[0][0].Page != 0 {
			t.Fatalf("peer %s received unexpected witness page request: %+v", peer.id, peer.requests[0][0])
		}
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for (peerA.responsesAcked != 1 || peerB.responsesAcked != 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if peerA.responsesAcked != 1 || peerB.responsesAcked != 1 {
		t.Fatalf("expected witness fetch acknowledgements, got peerA=%d peerB=%d", peerA.responsesAcked, peerB.responsesAcked)
	}
}

func TestSelectTriggerPeersPrefersPrivateBulkPeers(t *testing.T) {
	t.Parallel()

	peers := []fakeTriggerTargetPeer{
		{id: "public-bulk", bulk: true, addr: &net.TCPAddr{IP: net.ParseIP("54.1.2.3"), Port: 30303}},
		{id: "private-bulk", bulk: true, addr: &net.TCPAddr{IP: net.ParseIP("172.30.0.3"), Port: 30303}},
		{id: "public-plain", addr: &net.TCPAddr{IP: net.ParseIP("18.1.2.3"), Port: 30303}},
	}

	selected, selection := selectTriggerPeers(peers)
	if selection != "private-bulk" {
		t.Fatalf("unexpected selection mode %q", selection)
	}
	if len(selected) != 1 || selected[0].id != "private-bulk" {
		t.Fatalf("unexpected selected peers: %+v", selected)
	}
}

func TestSelectTriggerPeersFallsBackToBulkPeers(t *testing.T) {
	t.Parallel()

	peers := []fakeTriggerTargetPeer{
		{id: "plain-a", addr: &net.TCPAddr{IP: net.ParseIP("18.1.2.3"), Port: 30303}},
		{id: "bulk-a", bulk: true, addr: &net.TCPAddr{IP: net.ParseIP("54.1.2.3"), Port: 30303}},
		{id: "bulk-b", bulk: true, addr: &net.TCPAddr{IP: net.ParseIP("35.4.5.6"), Port: 30303}},
	}

	selected, selection := selectTriggerPeers(peers)
	if selection != "bulk" {
		t.Fatalf("unexpected selection mode %q", selection)
	}
	if len(selected) != 2 || selected[0].id != "bulk-a" || selected[1].id != "bulk-b" {
		t.Fatalf("unexpected selected peers: %+v", selected)
	}
}

func TestSelectTriggerPeersFallsBackToAllPeers(t *testing.T) {
	t.Parallel()

	peers := []fakeTriggerTargetPeer{
		{id: "b", inbound: true, addr: &net.TCPAddr{IP: net.ParseIP("18.1.2.3"), Port: 30303}},
		{id: "a", inbound: false, addr: &net.TCPAddr{IP: net.ParseIP("18.1.2.4"), Port: 30303}},
	}

	selected, selection := selectTriggerPeers(peers)
	if selection != "all" {
		t.Fatalf("unexpected selection mode %q", selection)
	}
	if len(selected) != 2 || selected[0].id != "a" || selected[1].id != "b" {
		t.Fatalf("unexpected selected peers: %+v", selected)
	}
}

type fakeTxGossipPeer struct {
	id  string
	txs []types.Transactions
	err error
}

func (p *fakeTxGossipPeer) ID() string { return p.id }

func (p *fakeTxGossipPeer) SendTransactions(txs types.Transactions) error {
	if p.err != nil {
		return p.err
	}
	p.txs = append(p.txs, txs)
	return nil
}

type fakeBlockAnnouncementPeer struct {
	id      string
	hashes  [][]common.Hash
	numbers [][]uint64
	err     error
}

func (p *fakeBlockAnnouncementPeer) ID() string { return p.id }

func (p *fakeBlockAnnouncementPeer) SendNewBlockHashes(hashes []common.Hash, numbers []uint64) error {
	if p.err != nil {
		return p.err
	}
	p.hashes = append(p.hashes, append([]common.Hash(nil), hashes...))
	p.numbers = append(p.numbers, append([]uint64(nil), numbers...))
	return nil
}

type fakeTxFetchPeer struct {
	id     string
	hashes [][]common.Hash
	err    error
}

func (p *fakeTxFetchPeer) ID() string { return p.id }

func (p *fakeTxFetchPeer) RequestTxs(hashes []common.Hash) error {
	if p.err != nil {
		return p.err
	}
	p.hashes = append(p.hashes, append([]common.Hash(nil), hashes...))
	return nil
}

type fakeBlockBodyFetchPeer struct {
	id             string
	hashes         [][]common.Hash
	err            error
	respond        bool
	requestsClosed int
	responsesAcked int
}

func (p *fakeBlockBodyFetchPeer) ID() string { return p.id }

func (p *fakeBlockBodyFetchPeer) RequestBodies(hashes []common.Hash, sink chan *ethproto.Response) (*ethproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.hashes = append(p.hashes, append([]common.Hash(nil), hashes...))
	req := &ethproto.Request{Cancel: make(chan struct{})}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &ethproto.Response{Done: done}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	go func() {
		<-req.Cancel
		p.requestsClosed++
	}()
	return req, nil
}

type fakeSnapAccountRangeFetchPeer struct {
	id             string
	err            error
	respond        bool
	responsesAcked int
	requests       []snapAccountRangeRequest
}

type snapAccountRangeRequest struct {
	root   common.Hash
	origin common.Hash
	limit  common.Hash
	bytes  uint64
}

func (p *fakeSnapAccountRangeFetchPeer) ID() string { return p.id }

func (p *fakeSnapAccountRangeFetchPeer) RequestAccountRangeWithSink(root common.Hash, origin, limit common.Hash, bytes uint64, sink chan *snapproto.Response) (*snapproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.requests = append(p.requests, snapAccountRangeRequest{root: root, origin: origin, limit: limit, bytes: bytes})
	req := &snapproto.Request{}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &snapproto.Response{
				Res: &snapproto.AccountRangePacket{
					Accounts: []*snapproto.AccountData{{Hash: common.HexToHash("0x01"), Body: []byte{0xc0}}},
				},
				Done: done,
			}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	return req, nil
}

type fakeSnapStorageRangeFetchPeer struct {
	id             string
	err            error
	respond        bool
	responsesAcked int
	requests       []snapStorageRangeRequest
}

type snapStorageRangeRequest struct {
	root     common.Hash
	accounts []common.Hash
	origin   []byte
	limit    []byte
	bytes    uint64
}

func (p *fakeSnapStorageRangeFetchPeer) ID() string { return p.id }

func (p *fakeSnapStorageRangeFetchPeer) RequestStorageRangesWithSink(root common.Hash, accounts []common.Hash, origin, limit []byte, bytes uint64, sink chan *snapproto.Response) (*snapproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.requests = append(p.requests, snapStorageRangeRequest{
		root:     root,
		accounts: append([]common.Hash(nil), accounts...),
		origin:   append([]byte(nil), origin...),
		limit:    append([]byte(nil), limit...),
		bytes:    bytes,
	})
	req := &snapproto.Request{}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &snapproto.Response{
				Res: &snapproto.StorageRangesPacket{
					Slots: [][]*snapproto.StorageData{{{Hash: common.HexToHash("0x02"), Body: []byte{0x01}}}},
				},
				Done: done,
			}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	return req, nil
}

type fakeSnapByteCodeFetchPeer struct {
	id             string
	err            error
	respond        bool
	responsesAcked int
	requests       []snapByteCodeRequest
}

type snapByteCodeRequest struct {
	hashes []common.Hash
	bytes  uint64
}

func (p *fakeSnapByteCodeFetchPeer) ID() string { return p.id }

func (p *fakeSnapByteCodeFetchPeer) RequestByteCodesWithSink(hashes []common.Hash, bytes uint64, sink chan *snapproto.Response) (*snapproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.requests = append(p.requests, snapByteCodeRequest{hashes: append([]common.Hash(nil), hashes...), bytes: bytes})
	req := &snapproto.Request{}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &snapproto.Response{
				Res: &snapproto.ByteCodesPacket{
					Codes: [][]byte{{0x60, 0x00}},
				},
				Done: done,
			}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	return req, nil
}

type fakeSnapTrieNodeFetchPeer struct {
	id             string
	err            error
	respond        bool
	responsesAcked int
	requests       []snapTrieNodeRequest
}

type snapTrieNodeRequest struct {
	root  common.Hash
	paths []snapproto.TrieNodePathSet
	bytes uint64
}

func (p *fakeSnapTrieNodeFetchPeer) ID() string { return p.id }

func (p *fakeSnapTrieNodeFetchPeer) RequestTrieNodesWithSink(root common.Hash, paths []snapproto.TrieNodePathSet, bytes uint64, sink chan *snapproto.Response) (*snapproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	copied := make([]snapproto.TrieNodePathSet, len(paths))
	for i, pathset := range paths {
		copied[i] = make(snapproto.TrieNodePathSet, len(pathset))
		for j, path := range pathset {
			copied[i][j] = append([]byte(nil), path...)
		}
	}
	p.requests = append(p.requests, snapTrieNodeRequest{root: root, paths: copied, bytes: bytes})
	req := &snapproto.Request{}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &snapproto.Response{
				Res: &snapproto.TrieNodesPacket{
					Nodes: [][]byte{{0x01, 0x02}},
				},
				Done: done,
			}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	return req, nil
}

func assertSnapAcks(t *testing.T, gotA, gotB func() int, label string) {
	t.Helper()

	deadline := time.Now().Add(500 * time.Millisecond)
	for (gotA() != 1 || gotB() != 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gotA() != 1 || gotB() != 1 {
		t.Fatalf("expected %s acknowledgements, got peerA=%d peerB=%d", label, gotA(), gotB())
	}
}

type fakeWitnessAnnouncementPeer struct {
	id      string
	hashes  []common.Hash
	numbers []uint64
}

func (p *fakeWitnessAnnouncementPeer) ID() string { return p.id }

func (p *fakeWitnessAnnouncementPeer) AsyncSendNewWitnessHash(hash common.Hash, number uint64) {
	p.hashes = append(p.hashes, hash)
	p.numbers = append(p.numbers, number)
}

type fakeWitnessMetadataFetchPeer struct {
	id             string
	hashes         [][]common.Hash
	err            error
	respond        bool
	responsesAcked int
	available      bool
}

func (p *fakeWitnessMetadataFetchPeer) ID() string { return p.id }

func (p *fakeWitnessMetadataFetchPeer) RequestWitnessMetadata(hashes []common.Hash, sink chan *witproto.Response) (*witproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.hashes = append(p.hashes, append([]common.Hash(nil), hashes...))
	req := &witproto.Request{}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &witproto.Response{
				Res: &witproto.WitnessMetadataPacket{
					Metadata: []witproto.WitnessMetadataResponse{{
						Hash:      hashes[0],
						Available: p.available,
					}},
				},
				Done: done,
			}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	return req, nil
}

type fakeWitnessFetchPeer struct {
	id             string
	requests       [][]witproto.WitnessPageRequest
	err            error
	respond        bool
	responsesAcked int
}

func (p *fakeWitnessFetchPeer) ID() string { return p.id }

func (p *fakeWitnessFetchPeer) RequestWitness(witnessPages []witproto.WitnessPageRequest, sink chan *witproto.Response) (*witproto.Request, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.requests = append(p.requests, append([]witproto.WitnessPageRequest(nil), witnessPages...))
	req := &witproto.Request{}
	if p.respond {
		go func() {
			done := make(chan error, 1)
			sink <- &witproto.Response{
				Res: &witproto.WitnessPacketRLPPacket{
					WitnessPacketResponse: []witproto.WitnessPageResponse{{
						Hash:       witnessPages[0].Hash,
						Page:       witnessPages[0].Page,
						TotalPages: 1,
						Data:       []byte{0x01, 0x02, 0x03},
					}},
				},
				Done: done,
			}
			if err := <-done; err == nil {
				p.responsesAcked++
			}
		}()
	}
	return req, nil
}

type fakeTriggerTargetPeer struct {
	id      string
	bulk    bool
	inbound bool
	addr    net.Addr
}

func (p fakeTriggerTargetPeer) ID() string           { return p.id }
func (p fakeTriggerTargetPeer) HasBulkRW() bool      { return p.bulk }
func (p fakeTriggerTargetPeer) RemoteAddr() net.Addr { return p.addr }
func (p fakeTriggerTargetPeer) Inbound() bool        { return p.inbound }
