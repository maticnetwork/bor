package p2p

import (
	"net"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

func TestBulkSessionStoreChannelReplacesExistingLane(t *testing.T) {
	session := &bulkSession{
		sidecar:  &BulkSidecar{log: log.New()},
		remoteID: enode.ID{0x01},
		channels: make(map[string]MsgReadWriter),
		waiters:  make(map[string][]chan bulkChannelResult),
	}
	firstApp, firstNet := MsgPipe()
	defer firstApp.Close()
	defer firstNet.Close()
	secondApp, secondNet := MsgPipe()
	defer secondApp.Close()
	defer secondNet.Close()

	session.storeChannel("eth-bulk", firstNet)
	session.storeChannel("eth-bulk", secondNet)

	got, ok := session.getChannel("eth-bulk")
	if !ok {
		t.Fatal("expected stored bulk channel")
	}
	if got != secondNet {
		t.Fatal("expected latest bulk channel to replace the previous lane")
	}
}

type TestGetPooledTransactionsRequest []common.Hash

type testGetPooledTransactionsPacket struct {
	RequestId uint64
	TestGetPooledTransactionsRequest
}

type TestPooledTransactionsResponse []common.Hash

type testPooledTransactionsPacket struct {
	RequestId uint64
	TestPooledTransactionsResponse
}

func TestBulkSidecarOpenChannelRoundTrip(t *testing.T) {
	left := newTestBulkServer(t)
	defer left.close()

	right := newTestBulkServer(t)
	defer right.close()

	left.setQUICPort()
	right.setQUICPort()

	leftPeer := newTestTrackedPeer(right.localnode.Node())
	rightPeer := newTestTrackedPeer(left.localnode.Node())
	left.setPeer(leftPeer)
	right.setPeer(rightPeer)

	type openResult struct {
		side string
		rw   MsgReadWriter
		err  error
	}
	results := make(chan openResult, 2)

	go func() {
		rw, err := left.bulk.OpenChannel(leftPeer, "snap-bulk")
		results <- openResult{side: "left", rw: rw, err: err}
	}()
	go func() {
		rw, err := right.bulk.OpenChannel(rightPeer, "snap-bulk")
		results <- openResult{side: "right", rw: rw, err: err}
	}()

	var leftRW, rightRW MsgReadWriter
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s sidecar open failed: %v", result.side, result.err)
		}
		if result.side == "left" {
			leftRW = result.rw
		} else {
			rightRW = result.rw
		}
	}
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendItems(leftRW, 2, uint64(22)) }()
	if err := ExpectMsg(rightRW, 2, []uint64{22}); err != nil {
		t.Fatalf("bulk lane delivery failed: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("bulk lane send failed: %v", err)
	}
}

func TestBulkStreamReadClearsExpiredDeadline(t *testing.T) {
	senderConn, receiverConn := net.Pipe()
	defer senderConn.Close()
	defer receiverConn.Close()

	sender := &bulkStreamMsgRW{stream: senderConn, channel: "snap-trie", log: log.New()}
	receiver := &bulkStreamMsgRW{stream: receiverConn, channel: "snap-trie", log: log.New()}
	if err := receiverConn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("failed to install expired authentication deadline: %v", err)
	}

	readErr := make(chan error, 1)
	go func() { readErr <- ExpectMsg(receiver, 2, []uint64{22}) }()
	select {
	case err := <-readErr:
		t.Fatalf("read returned before a message was sent: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	sendErr := make(chan error, 1)
	go func() { sendErr <- SendItems(sender, 2, uint64(22)) }()
	if err := <-readErr; err != nil {
		t.Fatalf("bulk lane delivery failed: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("bulk lane send failed: %v", err)
	}
}

func TestBulkSidecarCarriesEmbeddedRequestResponsePackets(t *testing.T) {
	left := newTestBulkServer(t)
	defer left.close()

	right := newTestBulkServer(t)
	defer right.close()

	left.setQUICPort()
	right.setQUICPort()

	leftPeer := newTestTrackedPeer(right.localnode.Node())
	rightPeer := newTestTrackedPeer(left.localnode.Node())
	left.setPeer(leftPeer)
	right.setPeer(rightPeer)

	type openResult struct {
		side string
		rw   MsgReadWriter
		err  error
	}
	results := make(chan openResult, 2)

	go func() {
		rw, err := left.bulk.OpenChannel(leftPeer, "eth-bulk")
		results <- openResult{side: "left", rw: rw, err: err}
	}()
	go func() {
		rw, err := right.bulk.OpenChannel(rightPeer, "eth-bulk")
		results <- openResult{side: "right", rw: rw, err: err}
	}()

	var leftRW, rightRW MsgReadWriter
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s sidecar open failed: %v", result.side, result.err)
		}
		if result.side == "left" {
			leftRW = result.rw
		} else {
			rightRW = result.rw
		}
	}

	// Prime the stream with another bulk-routed message first to mirror the
	// live sequence where tx gossip arrives before pooled tx fetch.
	sendErr := make(chan error, 4)
	go func() { sendErr <- SendItems(leftRW, 2, uint64(22)) }()
	if err := ExpectMsg(rightRW, 2, []uint64{22}); err != nil {
		t.Fatalf("bulk lane delivery failed: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("bulk lane send failed: %v", err)
	}

	req := testGetPooledTransactionsPacket{
		RequestId: 123,
		TestGetPooledTransactionsRequest: []common.Hash{
			{0x01},
			{0x02},
		},
	}
	go func() { sendErr <- Send(leftRW, 9, &req) }()

	msg, err := rightRW.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read embedded request packet: %v", err)
	}
	if msg.Code != 9 {
		t.Fatalf("unexpected request code: got %d want 9", msg.Code)
	}
	var gotReq testGetPooledTransactionsPacket
	if err := msg.Decode(&gotReq); err != nil {
		t.Fatalf("failed to decode embedded request packet: %v", err)
	}
	if gotReq.RequestId != req.RequestId {
		t.Fatalf("request id mismatch: got %d want %d", gotReq.RequestId, req.RequestId)
	}
	if len(gotReq.TestGetPooledTransactionsRequest) != len(req.TestGetPooledTransactionsRequest) {
		t.Fatalf("request hash count mismatch: got %d want %d", len(gotReq.TestGetPooledTransactionsRequest), len(req.TestGetPooledTransactionsRequest))
	}
	for i := range req.TestGetPooledTransactionsRequest {
		if gotReq.TestGetPooledTransactionsRequest[i] != req.TestGetPooledTransactionsRequest[i] {
			t.Fatalf("request hash mismatch at %d", i)
		}
	}
	if err := msg.Discard(); err != nil {
		t.Fatalf("failed to discard embedded request packet: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("embedded request send failed: %v", err)
	}

	res := testPooledTransactionsPacket{
		RequestId:                      123,
		TestPooledTransactionsResponse: []common.Hash{},
	}
	go func() { sendErr <- Send(rightRW, 10, &res) }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		msg, err := leftRW.ReadMsg()
		if err != nil {
			t.Errorf("failed to read embedded response packet: %v", err)
			return
		}
		if msg.Code != 10 {
			t.Errorf("unexpected response code: got %d want 10", msg.Code)
			return
		}
		var gotRes testPooledTransactionsPacket
		if err := msg.Decode(&gotRes); err != nil {
			t.Errorf("failed to decode embedded response packet: %v", err)
			return
		}
		if gotRes.RequestId != res.RequestId {
			t.Errorf("response id mismatch: got %d want %d", gotRes.RequestId, res.RequestId)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for embedded response packet")
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("embedded response send failed: %v", err)
	}
}

func TestRoutedMsgReadWriterConsumesBulkPayloadBeforeNextRead(t *testing.T) {
	left := newTestBulkServer(t)
	defer left.close()

	right := newTestBulkServer(t)
	defer right.close()

	left.setQUICPort()
	right.setQUICPort()

	leftPeer := newTestTrackedPeer(right.localnode.Node())
	rightPeer := newTestTrackedPeer(left.localnode.Node())
	left.setPeer(leftPeer)
	right.setPeer(rightPeer)

	type openResult struct {
		side string
		rw   MsgReadWriter
		err  error
	}
	results := make(chan openResult, 2)

	go func() {
		rw, err := left.bulk.OpenChannel(leftPeer, "eth-bulk")
		results <- openResult{side: "left", rw: rw, err: err}
	}()
	go func() {
		rw, err := right.bulk.OpenChannel(rightPeer, "eth-bulk")
		results <- openResult{side: "right", rw: rw, err: err}
	}()

	var leftBulk, rightBulk MsgReadWriter
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s sidecar open failed: %v", result.side, result.err)
		}
		if result.side == "left" {
			leftBulk = result.rw
		} else {
			rightBulk = result.rw
		}
	}

	primaryApp, primaryNet := MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	routed := NewRoutedMsgReadWriter(primaryNet, rightBulk, func(code uint64) bool {
		return code == 9 || code == 10
	})

	req := testGetPooledTransactionsPacket{
		RequestId: 456,
		TestGetPooledTransactionsRequest: []common.Hash{
			{0x11},
			{0x22},
		},
	}
	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(leftBulk, 9, &req) }()

	msg, err := routed.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read routed bulk message: %v", err)
	}
	if msg.Code != 9 {
		t.Fatalf("unexpected routed request code: got %d want 9", msg.Code)
	}
	var gotReq testGetPooledTransactionsPacket
	if err := msg.Decode(&gotReq); err != nil {
		t.Fatalf("failed to decode routed bulk request: %v", err)
	}
	if gotReq.RequestId != req.RequestId {
		t.Fatalf("routed request id mismatch: got %d want %d", gotReq.RequestId, req.RequestId)
	}
	if len(gotReq.TestGetPooledTransactionsRequest) != len(req.TestGetPooledTransactionsRequest) {
		t.Fatalf("routed request hash count mismatch: got %d want %d", len(gotReq.TestGetPooledTransactionsRequest), len(req.TestGetPooledTransactionsRequest))
	}
	for i := range req.TestGetPooledTransactionsRequest {
		if gotReq.TestGetPooledTransactionsRequest[i] != req.TestGetPooledTransactionsRequest[i] {
			t.Fatalf("routed request hash mismatch at %d", i)
		}
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("routed bulk request send failed: %v", err)
	}
}

type testBulkServer struct {
	server    *Server
	db        *enode.DB
	localnode *enode.LocalNode
	bulk      *BulkSidecar
	peers     map[enode.ID]*Peer
}

func newTestBulkServer(t *testing.T) *testBulkServer {
	t.Helper()

	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	db, err := enode.OpenDB(t.TempDir())
	if err != nil {
		t.Fatalf("failed to open node db: %v", err)
	}
	localnode := enode.NewLocalNode(db, priv)
	localnode.SetFallbackIP(net.IP{127, 0, 0, 1})

	srv := &Server{
		Config:     Config{PrivateKey: priv, Logger: log.Root()},
		localnode:  localnode,
		log:        log.Root(),
		quit:       make(chan struct{}),
		peerOp:     make(chan peerOpFunc),
		peerOpDone: make(chan struct{}),
	}
	tb := &testBulkServer{
		server:    srv,
		db:        db,
		localnode: localnode,
		peers:     make(map[enode.ID]*Peer),
	}
	go func() {
		for {
			select {
			case op := <-srv.peerOp:
				op(tb.peers)
				srv.peerOpDone <- struct{}{}
			case <-srv.quit:
				return
			}
		}
	}()
	bulk, err := newBulkSidecar(srv, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start bulk sidecar: %v", err)
	}
	tb.bulk = bulk
	srv.bulk = bulk
	go bulk.run()
	return tb
}

func (s *testBulkServer) setPeer(peer *Peer) {
	s.peers[peer.ID()] = peer
}

func (s *testBulkServer) setQUICPort() {
	udp := s.bulk.Addr().(*net.UDPAddr)
	s.localnode.Set(enr.QUIC(udp.Port))
}

func (s *testBulkServer) close() {
	s.bulk.Close()
	close(s.server.quit)
	s.db.Close()
}

func newTestTrackedPeer(node *enode.Node) *Peer {
	return &Peer{
		rw: &conn{
			node: node,
		},
	}
}
