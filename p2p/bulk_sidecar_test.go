package p2p

import (
	"net"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

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

type testBulkServer struct {
	server *Server
	db     *enode.DB
	localnode *enode.LocalNode
	bulk   *BulkSidecar
	peers  map[enode.ID]*Peer
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
		Config: Config{PrivateKey: priv, Logger: log.Root()},
		localnode: localnode,
		log:       log.Root(),
		quit:      make(chan struct{}),
		peerOp:    make(chan peerOpFunc),
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
