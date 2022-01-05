// Copyright 2014 The go-ethereum Authors
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

package p2p

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/assert"
)

var discard = Protocol{
	Name:   "discard",
	Length: 1,
	Run: func(p *Peer, rw MsgReadWriter) error {
		for {
			msg, err := rw.ReadMsg()
			if err != nil {
				return err
			}
			fmt.Printf("discarding %d\n", msg.Code)
			if err = msg.Discard(); err != nil {
				return err
			}
		}
	},
}

// uintID encodes i into a node ID.
func uintID(i uint16) enode.ID {
	var id enode.ID
	binary.BigEndian.PutUint16(id[:], i)
	return id
}

// newNode creates a node record with the given address.
func newNode(id enode.ID, addr string) *enode.Node {
	var r enr.Record
	if addr != "" {
		// Set the port if present.
		if strings.Contains(addr, ":") {
			hs, ps, err := net.SplitHostPort(addr)
			if err != nil {
				panic(fmt.Errorf("invalid address %q", addr))
			}
			port, err := strconv.Atoi(ps)
			if err != nil {
				panic(fmt.Errorf("invalid port in %q", addr))
			}
			r.Set(enr.TCP(port))
			r.Set(enr.UDP(port))
			addr = hs
		}
		// Set the IP.
		ip := net.ParseIP(addr)
		if ip == nil {
			panic(fmt.Errorf("invalid IP %q", addr))
		}
		r.Set(enr.IP(ip))
	}
	return enode.SignNull(&r, id)
}

type mockRlpxTransport struct {
	recvCh chan Msg
	sendCh chan Msg

	// declare closeCh if you want to simulate the remote peer dropping the connection
	closeCh chan struct{}
	closed  error
}

func (m *mockRlpxTransport) WriteMsg(msg Msg) error {
	m.sendCh <- msg
	return nil
}

func (m *mockRlpxTransport) ReadMsg() (Msg, error) {
	select {
	case <-m.closeCh:
		return Msg{}, io.EOF
	case msg := <-m.recvCh:
		return msg, nil
	}
}

func (m *mockRlpxTransport) Close() {
	if m.closeCh != nil {
		close(m.closeCh)
	}
}

func testSessionWithProto(protos []Protocol) {

}

func testPeer(protos []Protocol) (func(), *Peer, *devP2PSession, <-chan error) {

	newRlpxSession(log.Root(), nil, nil, nil)

	panic("X")

	/*
		var (
			fd1, fd2   = net.Pipe()
			key1, key2 = newkey(), newkey()
			t1         = newTestTransport(&key2.PublicKey, fd1, nil)
			t2         = newTestTransport(&key1.PublicKey, fd2, &key1.PublicKey)
		)

		c1 := &conn{fd: fd1, node: newNode(uintID(1), ""), transport: t1}
		c2 := &conn{fd: fd2, node: newNode(uintID(2), ""), transport: t2}
		for _, p := range protos {
			c1.caps = append(c1.caps, p.cap())
			c2.caps = append(c2.caps, p.cap())
		}

		peer := newPeer(log.Root(), c1, protos)
		errc := make(chan error, 1)
		go func() {
			_, err := peer.run()
			errc <- err
		}()

		closer := func() { c2.close(errors.New("close func called")) }
		return closer, c2, peer, errc
	*/
}

func TestDevP2PSession_ProtoWriteReadMsg(t *testing.T) {

	/*
		proto := Protocol{
			Name:   "a",
			Length: 5,
			Run: func(peer *Peer, rw MsgReadWriter) error {
				if err := ExpectMsg(rw, 2, []uint{1}); err != nil {
					t.Error(err)
				}
				if err := ExpectMsg(rw, 3, []uint{2}); err != nil {
					t.Error(err)
				}
				if err := ExpectMsg(rw, 4, []uint{3}); err != nil {
					t.Error(err)
				}
				return nil
			},
		}

		closer, _, _, errc := testPeer([]Protocol{proto})
		defer closer()

		Send(rw.transport, baseProtocolLength+2, []uint{1})
		Send(rw.transport, baseProtocolLength+3, []uint{2})
		Send(rw.transport, baseProtocolLength+4, []uint{3})

		select {
		case err := <-errc:
			if err != errProtocolReturned {
				t.Errorf("peer returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("receive timeout")
		}
	*/
}

func TestDevP2P_EncodeMsg(t *testing.T) {
	t.Skip("not sure what is this doing")
	/*
		proto := Protocol{
			Name:   "a",
			Length: 2,
			Run: func(peer *Peer, rw MsgReadWriter) error {
				if err := SendItems(rw, 2); err == nil {
					t.Error("expected error for out-of-range msg code, got nil")
				}
				if err := SendItems(rw, 1, "foo", "bar"); err != nil {
					t.Errorf("write error: %v", err)
				}
				return nil
			},
		}
		closer, rw, _, _ := testPeer([]Protocol{proto})
		defer closer()

		if err := ExpectMsg(rw.transport, 17, []string{"foo", "bar"}); err != nil {
			t.Error(err)
		}
	*/
}

func TestDevP2PSession_PingPong(t *testing.T) {
	// This test validates the ping/pong protocol

	tr := &mockRlpxTransport{
		sendCh: make(chan Msg),
		recvCh: make(chan Msg),
	}

	// Note that for now if we do not match any protocol is okay, in production
	// a session is not created if the peer does not share any protocol
	session := newRlpxSession(log.Root(), &Peer{node: newNode(uintID(1), "")}, tr, []Protocol{})
	go session.run()

	// 1. validate that when we send a ping request we get
	// back a pong reply

	// send a ping message
	tr.recvCh <- Msg{
		Code:    pingMsg,
		Payload: bytes.NewReader(nil),
	}

	// expect a pong message
	select {
	case msg := <-tr.sendCh:
		assert.Equal(t, msg.Code, uint64(pongMsg))

	case <-time.After(1 * time.Second):
		t.Fatal("pong timeout")
	}

	// 2. validate that if we do not send back a pong message
	// after a ping the connection is dropped
	// ??? Is this happening right now ??? I cannot find any code reference
	// that the connection is disconnected if pong messages are not sent back
}

func TestDevP2PSession_DisconnectRemote(t *testing.T) {
	// This test checks that a disconnect message sent by a peer is returned
	// as the error from Peer.run.

	tr := &mockRlpxTransport{
		sendCh: make(chan Msg),
		recvCh: make(chan Msg),
	}

	doneCh := make(chan struct{})

	session := newRlpxSession(log.Root(), &Peer{node: newNode(uintID(1), "")}, tr, []Protocol{})
	go func() {
		remoteRequested, err := session.run()
		assert.True(t, remoteRequested)
		assert.Equal(t, err, DiscTooManyPeers)
		doneCh <- struct{}{}
	}()

	// send a disconnect message
	raw, _ := rlp.EncodeToBytes([]DiscReason{DiscTooManyPeers})

	tr.recvCh <- Msg{
		Code:    discMsg,
		Size:    uint32(len(raw)),
		Payload: bytes.NewReader(raw),
	}

	select {
	case <-doneCh:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout to stop the connection")
	}
}

func TestDevP2PSession_DisconnectLocal(t *testing.T) {
	// This test checks that a disconnection from the local peer
	// sends a disconnect message and stops Peer.run.

	tr := &mockRlpxTransport{
		sendCh:  make(chan Msg, 1),
		recvCh:  make(chan Msg, 1),
		closeCh: make(chan struct{}),
	}

	doneCh := make(chan struct{})

	session := newRlpxSession(log.Root(), &Peer{node: newNode(uintID(1), "")}, tr, []Protocol{})
	go func() {
		remoteRequested, err := session.run()
		assert.False(t, remoteRequested)
		assert.Equal(t, err, DiscTooManyPeers)
		doneCh <- struct{}{}
	}()

	session.Close(DiscTooManyPeers)

	// We should receive a disconnect message in the remote peer
	msg := <-tr.sendCh
	assert.Equal(t, msg.Code, uint64(discMsg))

	select {
	case <-doneCh:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout to stop the connection")
	}
}

func TestDevP2PSession_Protocol_Disconnect(t *testing.T) {
	// This test is supposed to verify that a Protocol can reliable disconnect
	// the session.

	// session := newRlpxSession(log.Root(), &Peer{node: newNode(uintID(1), "")}, tr, []Protocol{})

	/*
		maybe := func() bool { return rand.Intn(2) == 1 }

		for i := 0; i < 1000; i++ {
			protoclose := make(chan error)
			protodisc := make(chan DiscReason)
			closer, rw, p, disc := testPeer([]Protocol{
				{
					Name:   "closereq",
					Run:    func(p *Peer, rw MsgReadWriter) error { return <-protoclose },
					Length: 1,
				},
				{
					Name:   "disconnect",
					Run:    func(p *Peer, rw MsgReadWriter) error { p.Disconnect(<-protodisc); return nil },
					Length: 1,
				},
			})

			// Simulate incoming messages.
			go SendItems(rw.transport, baseProtocolLength+1)
			go SendItems(rw.transport, baseProtocolLength+2)
			// Close the network connection.
			go closer()
			// Make protocol "closereq" return.
			protoclose <- errors.New("protocol closed")
			// Make protocol "disconnect" call peer.Disconnect
			protodisc <- DiscAlreadyConnected
			// In some cases, simulate something else calling peer.Disconnect.
			if maybe() {
				go p.disconnect(DiscInvalidIdentity)
			}
			// In some cases, simulate remote requesting a disconnect.
			if maybe() {
				go SendItems(rw.transport, discMsg, DiscQuitting)
			}

			select {
			case <-disc:
			case <-time.After(2 * time.Second):
				// Peer.run should return quickly. If it doesn't the Peer
				// goroutines are probably deadlocked. Call panic in order to
				// show the stacks.
				panic("Peer.run took to long to return.")
			}
		}
	*/
}

func TestDevP2PSession_Protocol_MultipleDisconnect(t *testing.T) {
	// This test is supposed to verify that Peer can reliably handle
	// multiple causes of disconnection occurring at the same time from protocols.
}

func TestNewPeer(t *testing.T) {
	t.Skip("I think this is not used") // is this only for the mock?

	name := "nodename"
	caps := []Cap{{"foo", 2, 0}, {"bar", 3, 0}}
	id := randomID()
	p := NewPeer(id, name, caps)
	if p.ID() != id {
		t.Errorf("ID mismatch: got %v, expected %v", p.ID(), id)
	}
	if p.Name() != name {
		t.Errorf("Name mismatch: got %v, expected %v", p.Name(), name)
	}
	if !reflect.DeepEqual(p.Caps(), caps) {
		t.Errorf("Caps mismatch: got %v, expected %v", p.Caps(), caps)
	}

	p.Disconnect(DiscAlreadyConnected) // Should not hang
}

func TestDevP2PSession_MatchProtocols(t *testing.T) {
	tests := []struct {
		Remote []Cap
		Local  []Protocol
		Match  map[string]devP2PStream
	}{
		{
			// No remote capabilities
			Local: []Protocol{{Name: "a"}},
		},
		{
			// No local protocols
			Remote: []Cap{{Name: "a"}},
		},
		{
			// No mutual protocols
			Remote: []Cap{{Name: "a"}},
			Local:  []Protocol{{Name: "b"}},
		},
		{
			// Some matches, some differences
			Remote: []Cap{{Name: "local"}, {Name: "match1"}, {Name: "match2"}},
			Local:  []Protocol{{Name: "match1"}, {Name: "match2"}, {Name: "remote"}},
			Match:  map[string]devP2PStream{"match1": {proto: Protocol{Name: "match1"}}, "match2": {proto: Protocol{Name: "match2"}}},
		},
		{
			// Various alphabetical ordering
			Remote: []Cap{{Name: "aa"}, {Name: "ab"}, {Name: "bb"}, {Name: "ba"}},
			Local:  []Protocol{{Name: "ba"}, {Name: "bb"}, {Name: "ab"}, {Name: "aa"}},
			Match:  map[string]devP2PStream{"aa": {proto: Protocol{Name: "aa"}}, "ab": {proto: Protocol{Name: "ab"}}, "ba": {proto: Protocol{Name: "ba"}}, "bb": {proto: Protocol{Name: "bb"}}},
		},
		{
			// No mutual versions
			Remote: []Cap{{Version: 1}},
			Local:  []Protocol{{Version: 2}},
		},
		{
			// Multiple versions, single common
			Remote: []Cap{{Version: 1}, {Version: 2}},
			Local:  []Protocol{{Version: 2}, {Version: 3}},
			Match:  map[string]devP2PStream{"": {proto: Protocol{Version: 2}}},
		},
		{
			// Multiple versions, multiple common
			Remote: []Cap{{Version: 1}, {Version: 2}, {Version: 3}, {Version: 4}},
			Local:  []Protocol{{Version: 2}, {Version: 3}},
			Match:  map[string]devP2PStream{"": {proto: Protocol{Version: 3}}},
		},
		{
			// Various version orderings
			Remote: []Cap{{Version: 4}, {Version: 1}, {Version: 3}, {Version: 2}},
			Local:  []Protocol{{Version: 2}, {Version: 3}, {Version: 1}},
			Match:  map[string]devP2PStream{"": {proto: Protocol{Version: 3}}},
		},
		{
			// Versions overriding sub-protocol lengths
			Remote: []Cap{{Version: 1}, {Version: 2}, {Version: 3}, {Name: "a"}},
			Local:  []Protocol{{Version: 1, Length: 1}, {Version: 2, Length: 2}, {Version: 3, Length: 3}, {Name: "a"}},
			Match:  map[string]devP2PStream{"": {proto: Protocol{Version: 3}}, "a": {proto: Protocol{Name: "a"}, offset: 3}},
		},
	}

	for i, tt := range tests {
		result := matchProtocols(tt.Local, tt.Remote, nil)
		if len(result) != len(tt.Match) {
			t.Errorf("test %d: negotiation mismatch: have %v, want %v", i, len(result), len(tt.Match))
			continue
		}
		// Make sure all negotiated protocols are needed and correct
		for name, proto := range result {
			match, ok := tt.Match[name]
			if !ok {
				t.Errorf("test %d, proto '%s': negotiated but shouldn't have", i, name)
				continue
			}
			if proto.proto.Name != match.proto.Name {
				t.Errorf("test %d, proto '%s': name mismatch: have %v, want %v", i, name, proto.proto.Name, match.proto.Name)
			}
			if proto.proto.Version != match.proto.Version {
				t.Errorf("test %d, proto '%s': version mismatch: have %v, want %v", i, name, proto.proto.Version, match.proto.Version)
			}
			if proto.offset-baseProtocolLength != match.offset {
				t.Errorf("test %d, proto '%s': offset mismatch: have %v, want %v", i, name, proto.offset-baseProtocolLength, match.offset)
			}
		}
		// Make sure no protocols missed negotiation
		for name := range tt.Match {
			if _, ok := result[name]; !ok {
				t.Errorf("test %d, proto '%s': not negotiated, should have", i, name)
				continue
			}
		}
	}
}

func TestRlpxTransport_Handshake(t *testing.T) {
	//TODO: BOR

	/*
		var (
			prv0, _ = crypto.GenerateKey()
			pub0    = crypto.FromECDSAPub(&prv0.PublicKey)[1:]
			hs0     = &protoHandshake{Version: 3, ID: pub0, Caps: []Cap{{"a", 0}, {"b", 2}}}

			prv1, _ = crypto.GenerateKey()
			pub1    = crypto.FromECDSAPub(&prv1.PublicKey)[1:]
			hs1     = &protoHandshake{Version: 3, ID: pub1, Caps: []Cap{{"c", 1}, {"d", 3}}}

			wg sync.WaitGroup
		)

		fd0, fd1, err := pipes.TCPPipe()
		if err != nil {
			t.Fatal(err)
		}

		wg.Add(2)
		go func() {
			defer wg.Done()
			defer fd0.Close()
			frame := newRLPX(fd0, &prv1.PublicKey)
			rpubkey, err := frame.doEncHandshake(prv0)
			if err != nil {
				t.Errorf("dial side enc handshake failed: %v", err)
				return
			}
			if !reflect.DeepEqual(rpubkey, &prv1.PublicKey) {
				t.Errorf("dial side remote pubkey mismatch: got %v, want %v", rpubkey, &prv1.PublicKey)
				return
			}

			phs, err := frame.doProtoHandshake(hs0)
			if err != nil {
				t.Errorf("dial side proto handshake error: %v", err)
				return
			}
			phs.Rest = nil
			if !reflect.DeepEqual(phs, hs1) {
				t.Errorf("dial side proto handshake mismatch:\ngot: %s\nwant: %s\n", spew.Sdump(phs), spew.Sdump(hs1))
				return
			}
			frame.close(DiscQuitting)
		}()
		go func() {
			defer wg.Done()
			defer fd1.Close()
			rlpx := newRLPX(fd1, nil)
			rpubkey, err := rlpx.doEncHandshake(prv1)
			if err != nil {
				t.Errorf("listen side enc handshake failed: %v", err)
				return
			}
			if !reflect.DeepEqual(rpubkey, &prv0.PublicKey) {
				t.Errorf("listen side remote pubkey mismatch: got %v, want %v", rpubkey, &prv0.PublicKey)
				return
			}

			phs, err := rlpx.doProtoHandshake(hs1)
			if err != nil {
				t.Errorf("listen side proto handshake error: %v", err)
				return
			}
			phs.Rest = nil
			if !reflect.DeepEqual(phs, hs0) {
				t.Errorf("listen side proto handshake mismatch:\ngot: %s\nwant: %s\n", spew.Sdump(phs), spew.Sdump(hs0))
				return
			}

			if err := ExpectMsg(rlpx, discMsg, []DiscReason{DiscQuitting}); err != nil {
				t.Errorf("error receiving disconnect: %v", err)
			}
		}()
		wg.Wait()
	*/
}

func TestRlpxTransport_Handshake_Errors(t *testing.T) {
	tests := []struct {
		code uint64
		msg  interface{}
		err  error
	}{
		{
			code: discMsg,
			msg:  []DiscReason{DiscQuitting},
			err:  DiscQuitting,
		},
		{
			code: 0x989898,
			msg:  []byte{1},
			err:  errors.New("expected handshake, got 989898"),
		},
		{
			code: handshakeMsg,
			msg:  make([]byte, baseProtocolMaxMsgSize+2),
			err:  errors.New("message too big"),
		},
		{
			code: handshakeMsg,
			msg:  []byte{1, 2, 3},
			err:  newPeerError(errInvalidMsg, "(code 0) (size 4) rlp: expected input list for p2p.protoHandshake"),
		},
		{
			code: handshakeMsg,
			msg:  &protoHandshake{Version: 3},
			err:  DiscInvalidIdentity,
		},
	}

	for i, test := range tests {
		p1, p2 := MsgPipe()
		go Send(p1, test.code, test.msg)
		_, err := readProtocolHandshake(p2)
		if !reflect.DeepEqual(err, test.err) {
			t.Errorf("test %d: error mismatch: got %q, want %q", i, err, test.err)
		}
	}
}
