package snap

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestPeerAttachBulkRWRoutesSnapTraffic(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	accountApp, accountNet := p2p.MsgPipe()
	defer accountApp.Close()
	defer accountNet.Close()

	storageApp, storageNet := p2p.MsgPipe()
	defer storageApp.Close()
	defer storageNet.Close()

	codeApp, codeNet := p2p.MsgPipe()
	defer codeApp.Close()
	defer codeNet.Close()

	trieApp, trieNet := p2p.MsgPipe()
	defer trieApp.Close()
	defer trieNet.Close()

	peer := NewFakePeer(SNAP1, "snap-test", primaryNet)
	peer.AttachBulkChannelRW(snapAccountsChannel, accountNet)
	peer.AttachBulkChannelRW(snapStorageChannel, storageNet)
	peer.AttachBulkChannelRW(snapCodeChannel, codeNet)
	peer.AttachBulkChannelRW(snapTrieChannel, trieNet)

	errc := make(chan error, 1)
	go func() {
		errc <- peer.RequestByteCodes(7, []common.Hash{{0x01}}, 1024)
	}()
	if err := p2p.ExpectMsg(codeApp, GetByteCodesMsg, &GetByteCodesPacket{
		ID:     7,
		Hashes: []common.Hash{{0x01}},
		Bytes:  1024,
	}); err != nil {
		t.Fatalf("bytecode lane mismatch: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send bytecode request: %v", err)
	}

	errc = make(chan error, 1)
	go func() {
		errc <- p2p.Send(peer.rw, AccountRangeMsg, &AccountRangePacket{ID: 8})
	}()
	if err := p2p.ExpectMsg(accountApp, AccountRangeMsg, &AccountRangePacket{ID: 8}); err != nil {
		t.Fatalf("account lane mismatch: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send account range response: %v", err)
	}

	errc = make(chan error, 1)
	go func() {
		errc <- peer.RequestStorageRanges(9, common.Hash{0x02}, []common.Hash{{0x03}}, nil, nil, 2048)
	}()
	if err := p2p.ExpectMsg(storageApp, GetStorageRangesMsg, &GetStorageRangesPacket{
		ID:       9,
		Root:     common.Hash{0x02},
		Accounts: []common.Hash{{0x03}},
		Origin:   nil,
		Limit:    nil,
		Bytes:    2048,
	}); err != nil {
		t.Fatalf("storage lane mismatch: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send storage request: %v", err)
	}

	errc = make(chan error, 1)
	go func() {
		errc <- peer.RequestTrieNodes(10, common.Hash{0x04}, []TrieNodePathSet{{[]byte{0xaa}}}, 4096)
	}()
	if err := p2p.ExpectMsg(trieApp, GetTrieNodesMsg, &GetTrieNodesPacket{
		ID:    10,
		Root:  common.Hash{0x04},
		Paths: []TrieNodePathSet{{[]byte{0xaa}}},
		Bytes: 4096,
	}); err != nil {
		t.Fatalf("trie lane mismatch: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send trie request: %v", err)
	}

	errc = make(chan error, 1)
	go func() {
		errc <- p2p.Send(peer.rw, TrieNodesMsg, &TrieNodesPacket{ID: 11})
	}()
	if err := p2p.ExpectMsg(trieApp, TrieNodesMsg, &TrieNodesPacket{ID: 11}); err != nil {
		t.Fatalf("trie response did not use trie lane: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("failed to send trie response: %v", err)
	}
}

func TestHandleConsumesBulkLanePackets(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	codeApp, codeNet := p2p.MsgPipe()
	defer codeApp.Close()
	defer codeNet.Close()

	peer := NewFakePeer(SNAP1, "snap-test", primaryNet)
	peer.AttachBulkChannelRW(snapCodeChannel, codeNet)

	backend := &snapBackendStub{
		handled: make(chan Packet, 1),
	}
	done := make(chan error, 1)
	go func() {
		done <- Handle(backend, peer)
	}()

	packet := &ByteCodesPacket{ID: 9, Codes: [][]byte{{0xaa, 0xbb}}}
	if err := p2p.Send(codeApp, ByteCodesMsg, packet); err != nil {
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

	if err := codeApp.Close(); err != nil {
		t.Fatalf("failed to close bulk lane: %v", err)
	}
	if err := primaryApp.Close(); err != nil {
		t.Fatalf("failed to close primary lane: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("expected handler exit after closing lanes")
	}
}

func TestPeerAttachBulkChannelRWKeepsRoutedRW(t *testing.T) {
	primaryApp, primaryNet := p2p.MsgPipe()
	defer primaryApp.Close()
	defer primaryNet.Close()

	codeApp, codeNet := p2p.MsgPipe()
	defer codeApp.Close()
	defer codeNet.Close()

	peer := NewFakePeer(SNAP1, "snap-test", primaryNet)
	original := peer.rw
	peer.AttachBulkChannelRW(snapCodeChannel, codeNet)

	if peer.rw != original {
		t.Fatal("expected late bulk attach to preserve the routed read-writer")
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

func TestSnapSidecarChannelForMsg(t *testing.T) {
	tests := []struct {
		code uint64
		want string
	}{
		{GetAccountRangeMsg, snapAccountsChannel},
		{AccountRangeMsg, snapAccountsChannel},
		{GetStorageRangesMsg, snapStorageChannel},
		{StorageRangesMsg, snapStorageChannel},
		{GetByteCodesMsg, snapCodeChannel},
		{ByteCodesMsg, snapCodeChannel},
		{GetTrieNodesMsg, snapTrieChannel},
		{TrieNodesMsg, snapTrieChannel},
		{0xff, ""},
	}
	for _, test := range tests {
		if got := snapSidecarChannelForMsg(test.code); got != test.want {
			t.Fatalf("snapSidecarChannelForMsg(%d) = %q, want %q", test.code, got, test.want)
		}
	}
}
