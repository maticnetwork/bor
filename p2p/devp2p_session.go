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
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"bytes"
	"crypto/ecdsa"
	"net"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/bitutil"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p/rlpx"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	ErrShuttingDown = errors.New("shutting down")
)

const (
	baseProtocolVersion    = 5
	baseProtocolLength     = uint64(16)
	baseProtocolMaxMsgSize = 2 * 1024

	snappyProtocolVersion = 5

	pingInterval = 15 * time.Second
)

const (
	// devp2p message codes
	handshakeMsg = 0x00
	discMsg      = 0x01
	pingMsg      = 0x02
	pongMsg      = 0x03
)

// protoHandshake is the RLP structure of the protocol handshake.
type protoHandshake struct {
	Version    uint64
	Name       string
	Caps       []Cap
	ListenPort uint64
	ID         []byte // secp256k1 public key

	// Ignore additional fields (for forward compatibility).
	Rest []rlp.RawValue `rlp:"tail"`
}

// devP2PSessionTransport is the transport interface required by the rlpx session
type devP2PSessionTransport interface {
	ReadMsg() (Msg, error)
	WriteMsg(msg Msg) error
	Close()
}

// devP2PSession represents an RLPX session opened with a remote peer
type devP2PSession struct {
	transport devP2PSessionTransport
	peer      *Peer

	streams map[string]*devP2PStream
	log     log.Logger
	created mclock.AbsTime

	wg       sync.WaitGroup
	protoErr chan error
	closed   chan struct{}
	disc     chan DiscReason

	// events receives message send / receive events if set
	events   *event.Feed
	testPipe *MsgPipeRW // for testing
}

// TODO: Much of this fields go away once we migrate to the new peer object

// Disconnect terminates the peer connection with the given reason.
// It returns immediately and does not wait until the connection is closed.
func (p *devP2PSession) Close(reason DiscReason) {
	if p.testPipe != nil {
		p.testPipe.Close()
	}

	select {
	case p.disc <- reason:
	case <-p.closed:
	}
}

func newRlpxSession(log log.Logger, peer *Peer, transport devP2PSessionTransport, protocols []Protocol) *devP2PSession {
	session := &devP2PSession{
		transport: transport,
		peer:      peer,
		created:   mclock.Now(),
		disc:      make(chan DiscReason),
		closed:    make(chan struct{}),
		streams:   make(map[string]*devP2PStream),
		log:       log.New("id", peer.ID(), "conn", peer.flags),
	}

	// for each of the match protocols create a stream
	matchedProtocols := matchProtocols(protocols, peer.caps)
	for _, m := range matchedProtocols {
		session.createStream(m)
	}

	// set the list of running protocols
	running := map[string]uint{}
	for _, m := range matchedProtocols {
		running[m.proto.Name] = m.proto.Version
	}
	peer.setRunning(running)

	return session
}

func (p *devP2PSession) createStream(m *matchedProtocol) {
	p.streams[m.proto.Name] = &devP2PStream{
		proto:  m.proto,
		offset: m.offset,
		in:     make(chan Msg),
		w:      p.transport,
	}
}

func (p *devP2PSession) run() (remoteRequested bool, err error) {
	// protocols + pingLoop
	p.protoErr = make(chan error, len(p.streams)+1)

	var (
		writeStart = make(chan struct{}, 1)
		writeErr   = make(chan error, 1)
		readErr    = make(chan error, 1)
		reason     DiscReason // sent to the peer
	)
	p.wg.Add(2)
	go p.readLoop(readErr)
	go p.pingLoop()

	// Start all protocol handlers.
	p.startProtocols(writeStart, writeErr)

	// The goal of this select is twofold:
	// 1. Catch an error from any of the concurrent tasks.
	// 2. Sync/Coordinate the writes from the subprotocols.
BACK:
	writeStart <- struct{}{}
	select {
	case err = <-writeErr:
		if err == nil {
			// (2) A write finished. Allow the next write to start if
			// there was no error.
			goto BACK
		}
		reason = DiscNetworkError

	case err = <-readErr:
		if r, ok := err.(DiscReason); ok {
			remoteRequested = true
			reason = r
		} else {
			reason = DiscNetworkError
		}

	case err = <-p.protoErr:
		reason = discReasonForError(err)

	case err = <-p.disc:
		reason = discReasonForError(err)
	}

	if reason != DiscNetworkError && !remoteRequested {
		// we started the disconnection, send a close message to the peer
		p.sendDiscMsg(reason)
	}

	close(p.closed)
	p.transport.Close()
	p.wg.Wait()
	return remoteRequested, err
}

func (p *devP2PSession) sendDiscMsg(reason DiscReason) {
	raw, _ := rlp.EncodeToBytes([]DiscReason{reason})

	msg := Msg{
		Code:    discMsg,
		Size:    uint32(len(raw)),
		Payload: bytes.NewReader(raw),
	}
	p.transport.WriteMsg(msg)
}

func (p *devP2PSession) pingLoop() {
	ping := time.NewTimer(pingInterval)
	defer p.wg.Done()
	defer ping.Stop()
	for {
		select {
		case <-ping.C:
			if err := SendItems(p.transport, pingMsg); err != nil {
				p.protoErr <- err
				return
			}
			ping.Reset(pingInterval)
		case <-p.closed:
			return
		}
	}
}

func (p *devP2PSession) readLoop(errc chan<- error) {
	defer p.wg.Done()
	for {
		msg, err := p.transport.ReadMsg()
		if err != nil {
			errc <- err
			return
		}
		msg.ReceivedAt = time.Now()
		if err = p.handle(msg); err != nil {
			errc <- err
			return
		}
	}
}

func (p *devP2PSession) handle(msg Msg) error {
	switch {
	case msg.Code == pingMsg:
		msg.Discard()
		go SendItems(p.transport, pongMsg)

	case msg.Code == discMsg:
		var reason [1]DiscReason
		// This is the last message. We don't need to discard or
		// check errors because, the connection will be closed after it.
		rlp.Decode(msg.Payload, &reason)
		return reason[0]

	case msg.Code < baseProtocolLength:
		// ignore other base protocol messages
		return msg.Discard()

	default:
		// it's a subprotocol message
		proto, err := p.getProto(msg.Code)

		if err != nil {
			return fmt.Errorf("msg code out of range: %v", msg.Code)
		}
		if metrics.Enabled {
			m := fmt.Sprintf("%s/%s/%d/%#02x", ingressMeterName, proto.proto.Name, proto.proto.Version, msg.Code-proto.offset)
			metrics.GetOrRegisterMeter(m, nil).Mark(int64(msg.meterSize))
			metrics.GetOrRegisterMeter(m+"/packets", nil).Mark(1)
		}
		select {
		case proto.in <- msg:
			return nil
		case <-p.closed:
			return io.EOF
		}
	}
	return nil
}

func countMatchingProtocols(protocols []Protocol, caps []Cap) int {
	n := 0
	for _, cap := range caps {
		for _, proto := range protocols {
			if proto.Name == cap.Name && proto.Version == cap.Version {
				n++
			}
		}
	}
	return n
}

type matchedProtocol struct {
	proto  Protocol
	offset uint64
}

// matchProtocols creates structures for matching named subprotocols.
func matchProtocols(protocols []Protocol, caps []Cap) map[string]*matchedProtocol {
	sort.Sort(capsByNameAndVersion(caps))
	offset := baseProtocolLength
	result := make(map[string]*matchedProtocol)

outer:
	for _, cap := range caps {
		for _, proto := range protocols {
			if proto.Name == cap.Name && proto.Version == cap.Version {
				// If an old protocol version matched, revert it
				if old := result[cap.Name]; old != nil {
					offset -= old.proto.Length
				}
				// Assign the new match
				result[cap.Name] = &matchedProtocol{
					proto:  proto,
					offset: offset,
				}
				// result[cap.Name] = &devP2PStream{proto: proto, offset: offset, in: make(chan Msg), w: rw}
				offset += proto.Length

				continue outer
			}
		}
	}
	return result
}

func (p *devP2PSession) startProtocols(writeStart <-chan struct{}, writeErr chan<- error) {
	p.wg.Add(len(p.streams))
	for _, proto := range p.streams {
		proto := proto
		proto.closed = p.closed
		proto.wstart = writeStart
		proto.werr = writeErr
		var rw MsgReadWriter = proto
		if p.events != nil {
			rw = newMsgEventer(rw, p.events, p.peer.ID(), proto.proto.Name, "TODO", "TODO") // TODO
		}
		p.log.Trace(fmt.Sprintf("Starting protocol %s/%d", proto.proto.Name, proto.proto.Version))
		go func() {
			defer p.wg.Done()

			err := proto.proto.Run(p.peer, rw)
			if err == nil {
				p.log.Trace(fmt.Sprintf("Protocol %s/%d returned", proto.proto.Name, proto.proto.Version))
				err = errProtocolReturned
			} else if err != io.EOF {
				p.log.Trace(fmt.Sprintf("Protocol %s/%d failed", proto.proto.Name, proto.proto.Version), "err", err)
			}
			p.protoErr <- err
		}()
	}
}

// getProto finds the protocol responsible for handling
// the given message code.
func (p *devP2PSession) getProto(code uint64) (*devP2PStream, error) {
	for _, proto := range p.streams {
		if code >= proto.offset && code < proto.offset+proto.proto.Length {
			return proto, nil
		}
	}
	return nil, newPeerError(errInvalidMsgCode, "%d", code)
}

type devP2PStream struct {
	proto  Protocol        // not the best name though
	in     chan Msg        // receives read messages
	closed <-chan struct{} // receives when peer is shutting down
	wstart <-chan struct{} // receives when write may start
	werr   chan<- error    // for write results
	offset uint64
	w      MsgWriter
}

func (rw *devP2PStream) WriteMsg(msg Msg) (err error) {
	if msg.Code >= rw.proto.Length {
		return newPeerError(errInvalidMsgCode, "not handled")
	}
	msg.meterCap = rw.proto.cap()
	msg.meterCode = msg.Code

	msg.Code += rw.offset

	select {
	case <-rw.wstart:
		err = rw.w.WriteMsg(msg)
		// Report write status back to Peer.run. It will initiate
		// shutdown if the error is non-nil and unblock the next write
		// otherwise. The calling protocol code should exit for errors
		// as well but we don't want to rely on that.
		rw.werr <- err
	case <-rw.closed:
		err = ErrShuttingDown
	}
	return err
}

func (rw *devP2PStream) ReadMsg() (Msg, error) {
	select {
	case msg := <-rw.in:
		msg.Code -= rw.offset
		return msg, nil
	case <-rw.closed:
		return Msg{}, io.EOF
	}
}

const (
	// total timeout for encryption handshake and protocol
	// handshake in both directions.
	handshakeTimeout = 5 * time.Second

	// This is the timeout for sending the disconnect reason.
	// This is shorter than the usual timeout because we don't want
	// to wait if the connection is known to be bad anyway.
	discWriteTimeout = 1 * time.Second
)

// rlpxTransport is the transport used by actual (non-test) connections.
// It wraps an RLPx connection with locks and read/write deadlines.
// It also does include functions to perform the raw write/read operations
// to perform the handshake, though the handshake per se is done in the
// devP2PSession struct.
type rlpxTransport struct {
	rmu  sync.Mutex
	wmu  sync.Mutex
	wbuf bytes.Buffer
	conn *rlpx.Conn
}

func newRLPX(conn net.Conn, dialDest *ecdsa.PublicKey) *rlpxTransport {
	return &rlpxTransport{conn: rlpx.NewConn(conn, dialDest)}
}

func (t *rlpxTransport) ReadMsg() (Msg, error) {
	t.rmu.Lock()
	defer t.rmu.Unlock()

	var msg Msg
	t.conn.SetReadDeadline(time.Now().Add(frameReadTimeout))
	code, data, wireSize, err := t.conn.Read()
	if err == nil {
		// Protocol messages are dispatched to subprotocol handlers asynchronously,
		// but package rlpx may reuse the returned 'data' buffer on the next call
		// to Read. Copy the message data to avoid this being an issue.
		data = common.CopyBytes(data)
		msg = Msg{
			ReceivedAt: time.Now(),
			Code:       code,
			Size:       uint32(len(data)),
			meterSize:  uint32(wireSize),
			Payload:    bytes.NewReader(data),
		}
	}
	return msg, err
}

func (t *rlpxTransport) WriteMsg(msg Msg) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()

	// Copy message data to write buffer.
	t.wbuf.Reset()
	if _, err := io.CopyN(&t.wbuf, msg.Payload, int64(msg.Size)); err != nil {
		return err
	}

	// Write the message.
	if msg.Code == discMsg {
		t.conn.SetWriteDeadline(time.Now().Add(discWriteTimeout))
	} else {
		t.conn.SetWriteDeadline(time.Now().Add(frameWriteTimeout))
	}

	size, err := t.conn.Write(msg.Code, t.wbuf.Bytes())
	if err != nil {
		return err
	}

	// Set metrics.
	msg.meterSize = size
	if metrics.Enabled && msg.meterCap.Name != "" { // don't meter non-subprotocol messages
		m := fmt.Sprintf("%s/%s/%d/%#02x", egressMeterName, msg.meterCap.Name, msg.meterCap.Version, msg.meterCode)
		metrics.GetOrRegisterMeter(m, nil).Mark(int64(msg.meterSize))
		metrics.GetOrRegisterMeter(m+"/packets", nil).Mark(1)
	}
	return nil
}

func (t *rlpxTransport) Close() {
	//t.wmu.Lock()
	//defer t.wmu.Unlock()

	// Note, it does not make sense to send the message here since that
	// is part of the session.

	/*
		// Tell the remote end why we're disconnecting if possible.
		// We only bother doing this if the underlying connection supports
		// setting a timeout tough.
		if t.conn != nil {
			if r, ok := err.(DiscReason); ok && r != DiscNetworkError {
				deadline := time.Now().Add(discWriteTimeout)
				if err := t.conn.SetWriteDeadline(deadline); err == nil {
					// Connection supports write deadline.
					t.wbuf.Reset()
					rlp.Encode(&t.wbuf, []DiscReason{r})
					t.conn.Write(discMsg, t.wbuf.Bytes())
				}
			}
		}
	*/

	t.conn.Close()
}

func (t *rlpxTransport) doEncHandshake(prv *ecdsa.PrivateKey) (*ecdsa.PublicKey, error) {
	t.conn.SetDeadline(time.Now().Add(handshakeTimeout))
	return t.conn.Handshake(prv)
}

func (t *rlpxTransport) doProtoHandshake(our *protoHandshake) (their *protoHandshake, err error) {
	// Writing our handshake happens concurrently, we prefer
	// returning the handshake read error. If the remote side
	// disconnects us early with a valid reason, we should return it
	// as the error so it can be tracked elsewhere.
	werr := make(chan error, 1)
	go func() { werr <- Send(t, handshakeMsg, our) }()
	if their, err = readProtocolHandshake(t); err != nil {
		<-werr // make sure the write terminates too
		return nil, err
	}
	if err := <-werr; err != nil {
		return nil, fmt.Errorf("write error: %v", err)
	}
	// If the protocol version supports Snappy encoding, upgrade immediately
	t.conn.SetSnappy(their.Version >= snappyProtocolVersion)

	return their, nil
}

func readProtocolHandshake(rw MsgReader) (*protoHandshake, error) {
	msg, err := rw.ReadMsg()
	if err != nil {
		return nil, err
	}
	if msg.Size > baseProtocolMaxMsgSize {
		return nil, fmt.Errorf("message too big")
	}
	if msg.Code == discMsg {
		// Disconnect before protocol handshake is valid according to the
		// spec and we send it ourself if the post-handshake checks fail.
		// We can't return the reason directly, though, because it is echoed
		// back otherwise. Wrap it in a string instead.
		var reason [1]DiscReason
		rlp.Decode(msg.Payload, &reason)
		return nil, reason[0]
	}
	if msg.Code != handshakeMsg {
		return nil, fmt.Errorf("expected handshake, got %x", msg.Code)
	}
	var hs protoHandshake
	if err := msg.Decode(&hs); err != nil {
		return nil, err
	}
	if len(hs.ID) != 64 || !bitutil.TestBytes(hs.ID) {
		return nil, DiscInvalidIdentity
	}
	return &hs, nil
}
