package wit

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/require"
)

// newWit2PeerPair wires two WIT2 peers over an in-memory message pipe. The
// sender runs the real broadcast loop; the receiver's inbound messages are
// consumed by the caller via handleMessage.
func newWit2PeerPair(t *testing.T) (sender *Peer, receiver *Peer, cleanup func()) {
	t.Helper()

	var idA, idB enode.ID
	rand.Read(idA[:])
	rand.Read(idB[:])

	app, net := p2p.MsgPipe()
	sender = NewPeer(WIT2, p2p.NewPeer(idA, "sender", nil), net, log.New())
	receiver = NewPeer(WIT2, p2p.NewPeer(idB, "receiver", nil), app, log.New())

	cleanup = func() {
		app.Close()
		net.Close()
		sender.Close()
		receiver.Close()
	}
	return sender, receiver, cleanup
}

func testAnnouncement(b byte) SignedWitnessAnnouncement {
	return SignedWitnessAnnouncement{
		BlockHash:   common.BytesToHash([]byte{b}),
		BlockNumber: uint64(b),
		WitnessHash: common.BytesToHash([]byte{b, b}),
		Signature:   make([]byte, SignatureLength),
	}
}

// TestSignedAnnouncementWireRoundTrip drives a signed announcement through
// the full wire path: async queue → broadcast loop → message pipe →
// handleMessage dispatch (WIT2 handler map) → decode → backend.Handle. This
// is the end-to-end proof that the new message type is routable on a
// negotiated WIT2 connection.
func TestSignedAnnouncementWireRoundTrip(t *testing.T) {
	sender, receiver, cleanup := newWit2PeerPair(t)
	defer cleanup()

	ann := testAnnouncement(7)

	delivered := make(chan Packet, 1)
	backend := &mockBackend{handleFunc: func(peer *Peer, packet Packet) error {
		delivered <- packet
		return nil
	}}

	sender.AsyncSendSignedWitnessAnnouncement(ann)
	require.True(t, sender.KnownAnnounceContainsHash(ann.BlockHash),
		"queued announce must mark the hash announce-known on the sender")
	require.False(t, sender.KnownWitnessContainsHash(ann.BlockHash),
		"announce must not mark the sender's body-known set")

	require.NoError(t, handleMessage(backend, receiver))

	select {
	case packet := <-delivered:
		require.Equal(t, "SignedNewWitnessHashes", packet.Name())
		require.Equal(t, byte(SignedNewWitnessHashesMsg), packet.Kind())
		got, ok := packet.(*SignedNewWitnessHashesPacket)
		require.True(t, ok)
		require.Len(t, got.Announcements, 1)
		require.Equal(t, ann.BlockHash, got.Announcements[0].BlockHash)
		require.Equal(t, ann.WitnessHash, got.Announcements[0].WitnessHash)
	case <-time.After(5 * time.Second):
		t.Fatal("announcement was not delivered to the backend")
	}
}

// TestHandleSignedNewWitnessHashesRejectsMalformedPackets covers the decode-
// time guards: an empty announcement list and a list over the per-packet cap
// must both error out before reaching the backend.
func TestHandleSignedNewWitnessHashesRejectsMalformedPackets(t *testing.T) {
	backend := &mockBackend{handleFunc: func(peer *Peer, packet Packet) error {
		t.Fatal("malformed packet must not reach the backend")
		return nil
	}}

	send := func(packet *SignedNewWitnessHashesPacket) error {
		sender, receiver, cleanup := newWit2PeerPair(t)
		defer cleanup()

		errc := make(chan error, 1)
		go func() {
			errc <- p2p.Send(sender.rw, SignedNewWitnessHashesMsg, packet)
		}()
		err := handleMessage(backend, receiver)
		require.NoError(t, <-errc)
		return err
	}

	require.Error(t, send(&SignedNewWitnessHashesPacket{}), "empty announcement list must be rejected")

	over := make([]SignedWitnessAnnouncement, MaxSignedAnnouncesPerPacket+1)
	for i := range over {
		over[i] = testAnnouncement(byte(i))
	}
	require.Error(t, send(&SignedNewWitnessHashesPacket{Announcements: over}), "over-cap packet must be rejected")

	// Structurally invalid payload: RLP that does not decode into the packet
	// shape must error out at decode time.
	sender, receiver, cleanup := newWit2PeerPair(t)
	defer cleanup()
	errc := make(chan error, 1)
	go func() {
		errc <- p2p.Send(sender.rw, SignedNewWitnessHashesMsg, "not-a-packet")
	}()
	require.Error(t, handleMessage(backend, receiver), "undecodable payload must be rejected")
	require.NoError(t, <-errc)
}

// TestAddKnownAnnounce pins the announce-known set semantics: recording an
// announce marks only the announce set, never the body-holder set that
// drives fetch peer selection.
func TestAddKnownAnnounce(t *testing.T) {
	var id enode.ID
	rand.Read(id[:])

	app, net := p2p.MsgPipe()
	defer app.Close()
	defer net.Close()
	peer := NewPeer(WIT2, p2p.NewPeer(id, "wit2", nil), net, log.New())
	defer peer.Close()

	hash := common.HexToHash("0x77")
	require.False(t, peer.KnownAnnounceContainsHash(hash))
	peer.AddKnownAnnounce(hash)
	require.True(t, peer.KnownAnnounceContainsHash(hash))
	require.False(t, peer.KnownWitnessContainsHash(hash), "announce-known must not imply body-known")
}

// TestAsyncSendSignedWitnessAnnouncementGuards pins the two non-delivery
// branches: a WIT1 peer never gets the WIT2 message queued (version guard),
// and a full queue drops announcements instead of blocking the caller.
func TestAsyncSendSignedWitnessAnnouncementGuards(t *testing.T) {
	var id enode.ID
	rand.Read(id[:])

	// Version guard: WIT1 peers don't speak the message.
	app, net := p2p.MsgPipe()
	defer app.Close()
	defer net.Close()
	wit1Peer := NewPeer(WIT1, p2p.NewPeer(id, "wit1", nil), net, log.New())
	defer wit1Peer.Close()

	ann := testAnnouncement(9)
	wit1Peer.AsyncSendSignedWitnessAnnouncement(ann)
	require.False(t, wit1Peer.KnownAnnounceContainsHash(ann.BlockHash),
		"WIT1 peer must not queue a signed announcement")

	// Queue-full drop: nobody reads the remote end, so the broadcast loop
	// blocks on the first send and the queue fills; the overflow must be
	// dropped without blocking the caller.
	appB, netB := p2p.MsgPipe()
	defer appB.Close()
	defer netB.Close()
	blocked := NewPeer(WIT2, p2p.NewPeer(id, "blocked", nil), netB, log.New())
	defer blocked.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < maxQueuedWitnessAnns+16; i++ {
			blocked.AsyncSendSignedWitnessAnnouncement(testAnnouncement(byte(i)))
		}
		close(done)
	}()

	select {
	case <-done:
		// Caller never blocked — overflow was dropped.
	case <-time.After(5 * time.Second):
		t.Fatal("AsyncSendSignedWitnessAnnouncement blocked on a full queue")
	}
}
