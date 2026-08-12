package wit

import (
	"crypto/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestPeerAttachBulkRWRoutesWitTraffic(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := p2p.MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	var id enode.ID
	rand.Read(id[:])

	peer := NewPeer(WIT1, p2p.NewPeer(id, "wit-test", nil), primaryNet, log.New())
	defer peer.Close()
	peer.AttachBulkRW(bulkNet)

	sink := make(chan *Response, 1)
	hash := common.Hash{0x01}
	reqc := make(chan *Request, 1)
	errc := make(chan error, 3)
	go func() {
		req, err := peer.RequestWitnessMetadata([]common.Hash{hash}, sink)
		if err == nil {
			reqc <- req
		}
		errc <- err
	}()
	msg, err := bulkApp.ReadMsg()
	if err != nil {
		t.Fatalf("failed to read witness metadata request: %v", err)
	}
	var reqPacket GetWitnessMetadataPacket
	if err := msg.Decode(&reqPacket); err != nil {
		t.Fatalf("failed to decode witness metadata request: %v", err)
	}
	if len(reqPacket.Hashes) != 1 || reqPacket.Hashes[0] != hash {
		t.Fatalf("metadata request hashes mismatch: got %v want [%v]", reqPacket.Hashes, hash)
	}
	req := <-reqc
	defer req.Close()
	if req.id != reqPacket.RequestId {
		t.Fatalf("metadata request id mismatch: got %d want %d", req.id, reqPacket.RequestId)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to request witness metadata: %v", err)
	}

	metadata := []WitnessMetadataResponse{{
		Hash:        hash,
		TotalPages:  2,
		WitnessSize: 128,
		BlockNumber: 99,
		Available:   true,
	}}
	go func() {
		errc <- peer.ReplyWitnessMetadata(7, metadata)
	}()
	if err := p2p.ExpectMsg(bulkApp, WitnessMetadataMsg, &WitnessMetadataPacket{
		RequestId: 7,
		Metadata:  metadata,
	}); err != nil {
		t.Fatalf("metadata reply did not use sidecar lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to reply witness metadata: %v", err)
	}

	witness, _ := stateless.NewWitness(&types.Header{Number: testHeader1.Number}, nil)
	witness.Headers = []*types.Header{testHeader1}
	go func() {
		errc <- peer.sendNewWitness(witness)
	}()
	if err := p2p.ExpectMsg(bulkApp, NewWitnessMsg, &NewWitnessPacket{Witness: witness}); err != nil {
		t.Fatalf("witness broadcast did not use sidecar lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send witness: %v", err)
	}
}

func TestHandleConsumesBulkLanePackets(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	bulkApp, bulkNet := p2p.MsgPipe()
	defer bulkApp.Close()
	defer bulkNet.Close()

	var id enode.ID
	rand.Read(id[:])

	peer := NewPeer(WIT1, p2p.NewPeer(id, "wit-test", nil), primaryNet, log.New())
	defer peer.Close()
	peer.AttachBulkRW(bulkNet)

	backend := &witBackendStub{
		handled: make(chan Packet, 1),
	}
	done := make(chan error, 1)
	go func() {
		done <- Handle(backend, peer)
	}()

	packet := &NewWitnessHashesPacket{
		Hashes:  []common.Hash{{0xaa}},
		Numbers: []uint64{88},
	}
	if err := p2p.Send(bulkApp, NewWitnessHashesMsg, packet); err != nil {
		t.Fatalf("failed to send bulk witness hashes packet: %v", err)
	}

	select {
	case got := <-backend.handled:
		res, ok := got.(*NewWitnessHashesPacket)
		if !ok {
			t.Fatalf("unexpected packet type %T", got)
		}
		if len(res.Hashes) != 1 || res.Hashes[0] != packet.Hashes[0] {
			t.Fatalf("witness hashes mismatch: got %v want %v", res.Hashes, packet.Hashes)
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

func TestIsBulkWitMsgRoutesAllMessages(t *testing.T) {
	tests := []struct {
		code uint64
		want bool
	}{
		{NewWitnessMsg, true},
		{NewWitnessHashesMsg, true},
		{GetMsgWitness, true},
		{MsgWitness, true},
		{GetWitnessMetadataMsg, true},
		{WitnessMetadataMsg, true},
		{0xff, false},
	}
	for _, test := range tests {
		if got := isBulkWitMsg(test.code); got != test.want {
			t.Fatalf("isBulkWitMsg(%d) = %v, want %v", test.code, got, test.want)
		}
	}
}

type witBackendStub struct {
	handled chan Packet
}

func (b *witBackendStub) Chain() *core.BlockChain { return nil }

func (b *witBackendStub) RunPeer(peer *Peer, handler Handler) error {
	return handler(peer)
}

func (b *witBackendStub) PeerInfo(id enode.ID) interface{} { return nil }

func (b *witBackendStub) Handle(peer *Peer, packet Packet) error {
	b.handled <- packet
	return nil
}
