package snap

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestPeerAttachBulkRWRoutesRequests(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := p2p.MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	peer := NewFakePeer(SNAP1, "snap-test", primaryNet)
	peer.AttachBulkRW(bulkNet)

	errc := make(chan error, 1)
	go func() {
		errc <- peer.RequestByteCodes(7, []common.Hash{{0x01}}, 1024)
	}()
	if err := p2p.ExpectMsg(bulkApp, GetByteCodesMsg, &GetByteCodesPacket{
		ID:     7,
		Hashes: []common.Hash{{0x01}},
		Bytes:  1024,
	}); err != nil {
		t.Fatalf("bulk lane mismatch: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send bytecode request: %v", err)
	}
}

func TestHandleConsumesBulkLanePackets(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := p2p.MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	peer := NewFakePeer(SNAP1, "snap-test", primaryNet)
	peer.AttachBulkRW(bulkNet)

	backend := &snapBackendStub{
		handled: make(chan Packet, 1),
	}
	done := make(chan error, 1)
	go func() {
		done <- Handle(backend, peer)
	}()

	packet := &ByteCodesPacket{ID: 9, Codes: [][]byte{{0xaa, 0xbb}}}
	if err := p2p.Send(bulkApp, ByteCodesMsg, packet); err != nil {
		t.Fatalf("failed to send bulk response: %v", err)
	}

	select {
	case got := <-backend.handled:
		res, ok := got.(*ByteCodesPacket)
		if !ok {
			t.Fatalf("unexpected packet type %T", got)
		}
		if res.ID != packet.ID {
			t.Fatalf("response id mismatch: got %d want %d", res.ID, packet.ID)
		}
	case err := <-done:
		t.Fatalf("handler exited early: %v", err)
	}

	if err := bulkApp.Close(); err != nil {
		t.Fatalf("failed to close bulk lane: %v", err)
	}
	if err := primaryApp.Close(); err != nil {
		t.Fatalf("failed to close primary lane: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("expected handler exit after closing lanes")
	}
}

type snapBackendStub struct {
	handled chan Packet
}

func (b *snapBackendStub) Chain() *core.BlockChain { return nil }

func (b *snapBackendStub) RunPeer(peer *Peer, handler Handler) error {
	return handler(peer)
}

func (b *snapBackendStub) PeerInfo(id enode.ID) interface{} { return nil }

func (b *snapBackendStub) Handle(peer *Peer, packet Packet) error {
	b.handled <- packet
	return nil
}
