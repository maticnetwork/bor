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

// This file contains some shares testing functionality, common to  multiple
// different files and modules being tested.

package eth

import (
	"crypto/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// testPeer is a simulated peer to allow testing direct network calls.
type testPeer struct {
	*Peer

	net p2p.MsgReadWriter // Network layer reader/writer to simulate remote messaging
	app *p2p.MsgPipeRW    // Application layer reader/writer to simulate the local side
}

// newTestPeer creates a new peer registered at the given data backend.
func newTestPeer(name string, version uint, backend Backend) (*testPeer, <-chan error) {
	// Create a message pipe to communicate through
	app, net := p2p.MsgPipe()

	// Start the peer on a new thread
	var id enode.ID

	rand.Read(id[:])

	peer := NewPeer(version, p2p.NewPeer(id, name, nil), net, backend.TxPool())
	errc := make(chan error, 1)

	go func() {
		defer app.Close()

		errc <- backend.RunPeer(peer, func(peer *Peer) error {
			return Handle(backend, peer)
		})
	}()

	return &testPeer{app: app, net: net, Peer: peer}, errc
}

// close terminates the local side of the peer, notifying the remote protocol
// manager of termination.
func (p *testPeer) close() {
	p.Peer.Close()
	p.app.Close()
}

func TestPeerSet(t *testing.T) {
	size := 5
	s := newKnownCache(size)

	// add 10 items
	for i := 0; i < size*2; i++ {
		s.Add(common.Hash{byte(i)})
	}

	if s.Cardinality() != size {
		t.Fatalf("wrong size, expected %d but found %d", size, s.Cardinality())
	}

	vals := []common.Hash{}
	for i := 10; i < 20; i++ {
		vals = append(vals, common.Hash{byte(i)})
	}

	// add item in batch
	s.Add(vals...)

	if s.Cardinality() < size {
		t.Fatalf("bad size")
	}
}

func TestKnownCacheRemove(t *testing.T) {
	size := 10
	s := newKnownCache(size)

	// Add some items
	hashes := make([]common.Hash, 5)
	for i := 0; i < 5; i++ {
		hashes[i] = common.Hash{byte(i)}
		s.Add(hashes[i])
	}

	if s.Cardinality() != 5 {
		t.Fatalf("wrong size after add, expected 5 but found %d", s.Cardinality())
	}

	// Remove some items
	s.Remove(hashes[0], hashes[2], hashes[4])

	if s.Cardinality() != 2 {
		t.Fatalf("wrong size after remove, expected 2 but found %d", s.Cardinality())
	}

	// Verify the correct items were removed
	if s.Contains(hashes[0]) {
		t.Error("hash[0] should have been removed")
	}
	if !s.Contains(hashes[1]) {
		t.Error("hash[1] should still be present")
	}
	if s.Contains(hashes[2]) {
		t.Error("hash[2] should have been removed")
	}
	if !s.Contains(hashes[3]) {
		t.Error("hash[3] should still be present")
	}
	if s.Contains(hashes[4]) {
		t.Error("hash[4] should have been removed")
	}
}

func TestPeerForgetTransactions(t *testing.T) {
	// Create a peer with a known tx cache
	app, _ := p2p.MsgPipe()
	defer app.Close()

	var id enode.ID
	rand.Read(id[:])

	peer := NewPeer(ETH68, p2p.NewPeer(id, "test", nil), app, nil)
	defer peer.Close()

	// Add some transaction hashes to the known set
	hashes := make([]common.Hash, 5)
	for i := 0; i < 5; i++ {
		hashes[i] = common.Hash{byte(i + 100)}
		peer.knownTxs.Add(hashes[i])
	}

	if peer.knownTxs.Cardinality() != 5 {
		t.Fatalf("wrong size after add, expected 5 but found %d", peer.knownTxs.Cardinality())
	}

	// Forget some transactions
	peer.ForgetTransactions([]common.Hash{hashes[0], hashes[2], hashes[4]})

	if peer.knownTxs.Cardinality() != 2 {
		t.Fatalf("wrong size after forget, expected 2 but found %d", peer.knownTxs.Cardinality())
	}

	// Verify the transactions were forgotten
	if peer.knownTxs.Contains(hashes[0]) {
		t.Error("hash[0] should have been forgotten")
	}
	if !peer.knownTxs.Contains(hashes[1]) {
		t.Error("hash[1] should still be known")
	}
}

func TestPeerAttachBulkRWRoutesEthTraffic(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	controlApp, controlNet := p2p.MsgPipe()
	defer controlApp.Close()
	defer controlNet.Close()

	blocksApp, blocksNet := p2p.MsgPipe()
	defer blocksApp.Close()
	defer blocksNet.Close()

	txApp, txNet := p2p.MsgPipe()
	defer txApp.Close()
	defer txNet.Close()

	bulkApp, bulkNet := p2p.MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	var id enode.ID
	rand.Read(id[:])

	peer := NewPeer(ETH69, p2p.NewPeer(id, "test", nil), primaryNet, nil)
	defer peer.Close()
	peer.AttachBulkChannelRW(ethControlChannel, controlNet)
	peer.AttachBulkChannelRW(ethBlocksChannel, blocksNet)
	peer.AttachBulkChannelRW(ethTxChannel, txNet)
	peer.AttachBulkChannelRW(ethBulkChannel, bulkNet)

	resCh := make(chan *Response, 1)
	hashes := []common.Hash{{0x01}, {0x02}}

	reqc := make(chan *Request, 1)
	errc := make(chan error, 4)
	go func() {
		req, err := peer.RequestBodies(hashes, resCh)
		if err == nil {
			reqc <- req
		}
		errc <- err
	}()

	msg, err := bulkApp.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read block bodies request: %v", err)
	}
	var bodiesReq GetBlockBodiesPacket
	if err := msg.Decode(&bodiesReq); err != nil {
		t.Fatalf("failed to decode block bodies request: %v", err)
	}
	if len(bodiesReq.GetBlockBodiesRequest) != len(hashes) {
		t.Fatalf("unexpected block bodies request size: got %d want %d", len(bodiesReq.GetBlockBodiesRequest), len(hashes))
	}
	for i := range hashes {
		if bodiesReq.GetBlockBodiesRequest[i] != hashes[i] {
			t.Fatalf("block bodies hash mismatch at %d", i)
		}
	}
	req := <-reqc
	defer req.Close()
	if err := <-errc; err != nil {
		t.Fatalf("failed to request bodies: %v", err)
	}
	if req.id != bodiesReq.RequestId {
		t.Fatalf("block bodies request id mismatch: got %d want %d", req.id, bodiesReq.RequestId)
	}

	go func() {
		req, err := peer.RequestReceipts(hashes, resCh)
		if err == nil {
			reqc <- req
		}
		errc <- err
	}()
	msg, err = bulkApp.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read receipts request: %v", err)
	}
	var receiptsReq GetReceiptsPacket
	if err := msg.Decode(&receiptsReq); err != nil {
		t.Fatalf("failed to decode receipts request: %v", err)
	}
	if len(receiptsReq.GetReceiptsRequest) != len(hashes) {
		t.Fatalf("unexpected receipt request size: got %d want %d", len(receiptsReq.GetReceiptsRequest), len(hashes))
	}
	for i := range hashes {
		if receiptsReq.GetReceiptsRequest[i] != hashes[i] {
			t.Fatalf("receipt hash mismatch at %d", i)
		}
	}
	req = <-reqc
	defer req.Close()
	if err := <-errc; err != nil {
		t.Fatalf("failed to request receipts: %v", err)
	}
	if req.id != receiptsReq.RequestId {
		t.Fatalf("receipt request id mismatch: got %d want %d", req.id, receiptsReq.RequestId)
	}

	go func() { errc <- peer.ReplyBlockBodiesRLP(7, nil) }()
	if err := p2p.ExpectMsg(bulkApp, BlockBodiesMsg, &BlockBodiesRLPPacket{
		RequestId:              7,
		BlockBodiesRLPResponse: nil,
	}); err != nil {
		t.Fatalf("block body reply did not use bulk lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to reply with bodies: %v", err)
	}

	go func() { errc <- peer.ReplyReceiptsRLP(8, nil) }()
	if err := p2p.ExpectMsg(bulkApp, ReceiptsMsg, &ReceiptsRLPPacket{
		RequestId:           8,
		ReceiptsRLPResponse: nil,
	}); err != nil {
		t.Fatalf("receipt reply did not use bulk lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to reply with receipts: %v", err)
	}

	go func() { errc <- peer.SendTransactions(types.Transactions{}) }()
	if err := p2p.ExpectMsg(txApp, TransactionsMsg, types.Transactions{}); err != nil {
		t.Fatalf("transactions gossip did not use tx lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send transactions: %v", err)
	}

	go func() { errc <- peer.SendNewBlockHashes(hashes, []uint64{1, 2}) }()
	if err := p2p.ExpectMsg(blocksApp, NewBlockHashesMsg, NewBlockHashesPacket{
		{Hash: hashes[0], Number: 1},
		{Hash: hashes[1], Number: 2},
	}); err != nil {
		t.Fatalf("block announcement did not use block lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send block hashes: %v", err)
	}

	go func() { errc <- peer.RequestTxs(hashes) }()
	msg, err = bulkApp.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read pooled transaction request: %v", err)
	}
	if msg.Code != GetPooledTransactionsMsg {
		t.Fatalf("unexpected pooled transaction request code: got %d want %d", msg.Code, GetPooledTransactionsMsg)
	}
	var txReq GetPooledTransactionsPacket
	if err := msg.Decode(&txReq); err != nil {
		t.Fatalf("failed to decode pooled transaction request: %v", err)
	}
	if len(txReq.GetPooledTransactionsRequest) != len(hashes) {
		t.Fatalf("unexpected pooled transaction request size: got %d want %d", len(txReq.GetPooledTransactionsRequest), len(hashes))
	}
	for i := range hashes {
		if txReq.GetPooledTransactionsRequest[i] != hashes[i] {
			t.Fatalf("pooled transaction hash mismatch at %d", i)
		}
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to request transactions: %v", err)
	}

	go func() {
		req, err := peer.RequestHeadersByHash(hashes[0], 2, 0, false, resCh)
		if err == nil {
			reqc <- req
		}
		errc <- err
	}()
	msg, err = controlApp.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read header request: %v", err)
	}
	if msg.Code != GetBlockHeadersMsg {
		t.Fatalf("unexpected header request code: got %d want %d", msg.Code, GetBlockHeadersMsg)
	}
	var headersReq GetBlockHeadersPacket
	if err := msg.Decode(&headersReq); err != nil {
		t.Fatalf("failed to decode header request: %v", err)
	}
	req = <-reqc
	defer req.Close()
	if err := <-errc; err != nil {
		t.Fatalf("failed to request headers: %v", err)
	}
	if req.id != headersReq.RequestId {
		t.Fatalf("header request id mismatch: got %d want %d", req.id, headersReq.RequestId)
	}

	go func() { errc <- p2p.Send(peer.rw, StatusMsg, &StatusPacket68{}) }()
	if err := p2p.ExpectMsg(primaryApp, StatusMsg, &StatusPacket68{}); err != nil {
		t.Fatalf("status message should remain on primary lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send status: %v", err)
	}
}

func TestPeerAttachBulkChannelRWKeepsRoutedRW(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := p2p.MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	var id enode.ID
	rand.Read(id[:])

	peer := NewPeer(ETH69, p2p.NewPeer(id, "test", nil), primaryNet, nil)
	defer peer.Close()

	original := peer.rw
	peer.AttachBulkChannelRW(ethBulkChannel, bulkNet)

	if peer.rw != original {
		t.Fatal("expected late bulk attach to preserve the routed read-writer")
	}
}

func TestEthSidecarChannelForMsg(t *testing.T) {
	tests := []struct {
		code uint64
		want string
	}{
		{StatusMsg, ""},
		{NewBlockHashesMsg, ethBlocksChannel},
		{TransactionsMsg, ethTxChannel},
		{GetBlockHeadersMsg, ethControlChannel},
		{BlockHeadersMsg, ethControlChannel},
		{GetBlockBodiesMsg, ethBulkChannel},
		{BlockBodiesMsg, ethBulkChannel},
		{NewBlockMsg, ethBlocksChannel},
		{NewPooledTransactionHashesMsg, ethTxChannel},
		{GetPooledTransactionsMsg, ethBulkChannel},
		{PooledTransactionsMsg, ethBulkChannel},
		{GetReceiptsMsg, ethBulkChannel},
		{ReceiptsMsg, ethBulkChannel},
		{BlockRangeUpdateMsg, ethControlChannel},
		{0xff, ""},
	}
	for _, test := range tests {
		if got := ethSidecarChannelForMsg(test.code); got != test.want {
			t.Fatalf("ethSidecarChannelForMsg(%d) = %q, want %q", test.code, got, test.want)
		}
	}
}
