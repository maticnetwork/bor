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

package p2p

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlogwriter"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	bulkSidecarNextProto      = "bor-bulk/1"
	bulkSidecarVersion        = uint64(1)
	bulkAuthControlMaxSize    = 1024
	bulkChannelControlMaxSize = 256
	bulkFrameHeaderSize       = 12
	bulkMaxMessageSize        = 10 * 1024 * 1024
	bulkDialTimeout           = 5 * time.Second
	bulkAuthTimeout           = 5 * time.Second
	bulkChannelOpenTimeout    = 5 * time.Second
	bulkMessageReadTimeout    = 30 * time.Second
	bulkMessageWriteTimeout   = 20 * time.Second
	bulkConnReceiveWindow     = 16 * bulkMaxMessageSize
	bulkSocketReadBufferSize  = 8 * 1024 * 1024
	bulkSocketWriteBufferSize = 8 * 1024 * 1024
	bulkSidecarCloseErrorCode = quic.ApplicationErrorCode(0x424f52)
	bulkSidecarProtocolError  = quic.ApplicationErrorCode(0x424f53)
)

var (
	errBulkSidecarNoPeer  = errors.New("bulk sidecar peer not connected")
	errBulkSidecarNoQUIC  = errors.New("peer has no bulk sidecar endpoint")
	errBulkChannelTimeout = errors.New("bulk channel open timed out")
)

type BulkSidecar struct {
	srv       *Server
	listener  *quic.Listener
	transport *quic.Transport
	tls       *tls.Config
	config    *quic.Config
	log       log.Logger

	localID enode.ID
	priv    *ecdsa.PrivateKey

	closeOnce sync.Once
	closeCh   chan struct{}

	lock     sync.Mutex
	sessions map[enode.ID]*bulkSession
}

type bulkSession struct {
	sidecar  *BulkSidecar
	remote   *enode.Node
	remoteID enode.ID

	lock       sync.Mutex
	conn       *quic.Conn
	connClosed <-chan struct{}
	dialing    bool
	dialWait   chan struct{}
	channels   map[string]MsgReadWriter
	waiters    map[string][]chan bulkChannelResult
}

type bulkChannelResult struct {
	rw  MsgReadWriter
	err error
}

type bulkAuthHello struct {
	Version uint64
	From    enode.ID
	To      enode.ID
	Nonce   [32]byte
}

type bulkAuthChallenge struct {
	Nonce     [32]byte
	Signature []byte
}

type bulkAuthResponse struct {
	Signature []byte
}

type bulkChannelHello struct {
	Version uint64
	Channel string
}

type bulkStreamMsgRW struct {
	stream  *quic.Stream
	channel string
	log     log.Logger
	write   sync.Mutex
}

func newBulkSidecar(srv *Server, listenAddr string) (*BulkSidecar, error) {
	cert, err := generateBulkSidecarCertificate()
	if err != nil {
		return nil, err
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{bulkSidecarNextProto},
		MinVersion:   tls.VersionTLS13,
	}
	quicConf := &quic.Config{
		HandshakeIdleTimeout:           bulkAuthTimeout,
		MaxIdleTimeout:                 60 * time.Second,
		KeepAlivePeriod:                15 * time.Second,
		MaxIncomingStreams:             32,
		InitialStreamReceiveWindow:     2 * bulkMaxMessageSize,
		MaxStreamReceiveWindow:         2 * bulkMaxMessageSize,
		InitialConnectionReceiveWindow: bulkConnReceiveWindow,
		MaxConnectionReceiveWindow:     bulkConnReceiveWindow,
		Tracer: func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
			return bulkSidecarStats.newConnectionTrace()
		},
	}
	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	socketBuffers := configureBulkSidecarSocketBuffers(udpConn)
	bulkSidecarStats.setSocketBuffers(socketBuffers)

	transport := &quic.Transport{
		Conn:   udpConn,
		Tracer: bulkSidecarStats.newTransportRecorder(),
	}
	listener, err := transport.Listen(tlsConf, quicConf)
	if err != nil {
		_ = udpConn.Close()
		return nil, err
	}
	return &BulkSidecar{
		srv:       srv,
		listener:  listener,
		transport: transport,
		tls:       tlsConf,
		config:    quicConf,
		log:       srv.log,
		localID:   srv.localnode.ID(),
		priv:      srv.PrivateKey,
		closeCh:   make(chan struct{}),
		sessions:  make(map[enode.ID]*bulkSession),
	}, nil
}

func (b *BulkSidecar) Addr() net.Addr {
	return b.listener.Addr()
}

func (b *BulkSidecar) Close() {
	b.closeOnce.Do(func() {
		close(b.closeCh)
		if b.transport != nil {
			_ = b.transport.Close()
		} else if b.listener != nil {
			_ = b.listener.Close()
		}
		b.lock.Lock()
		sessions := make([]*bulkSession, 0, len(b.sessions))
		for _, session := range b.sessions {
			sessions = append(sessions, session)
		}
		b.lock.Unlock()
		for _, session := range sessions {
			session.close()
		}
	})
}

func (b *BulkSidecar) run() {
	for {
		conn, err := b.listener.Accept(context.Background())
		if err != nil {
			select {
			case <-b.closeCh:
				return
			default:
				b.log.Debug("Bulk sidecar accept failed", "err", err)
				return
			}
		}
		go b.handleIncomingConn(conn)
	}
}

func (b *BulkSidecar) OpenChannel(peer *Peer, channel string) (MsgReadWriter, error) {
	if peer == nil || peer.Node() == nil {
		return nil, errBulkSidecarNoPeer
	}
	session := b.session(peer.Node())
	ctx, cancel := context.WithTimeout(context.Background(), bulkChannelOpenTimeout)
	defer cancel()
	return session.openChannel(ctx, channel)
}

func (b *BulkSidecar) DropPeer(id enode.ID) {
	b.lock.Lock()
	session := b.sessions[id]
	delete(b.sessions, id)
	b.lock.Unlock()
	if session != nil {
		session.close()
	}
}

func (b *BulkSidecar) session(remote *enode.Node) *bulkSession {
	remoteID := remote.ID()
	b.lock.Lock()
	defer b.lock.Unlock()
	if session, ok := b.sessions[remoteID]; ok {
		session.remote = remote
		return session
	}
	session := &bulkSession{
		sidecar:  b,
		remote:   remote,
		remoteID: remoteID,
		channels: make(map[string]MsgReadWriter),
		waiters:  make(map[string][]chan bulkChannelResult),
	}
	b.sessions[remoteID] = session
	return session
}

func (b *BulkSidecar) handleIncomingConn(conn *quic.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), bulkAuthTimeout)
	defer cancel()

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		_ = conn.CloseWithError(bulkSidecarProtocolError, "missing auth stream")
		return
	}
	remote, err := b.acceptAuth(stream)
	if err != nil {
		_ = conn.CloseWithError(bulkSidecarProtocolError, err.Error())
		return
	}
	if !b.adoptConn(remote, conn) {
		_ = conn.CloseWithError(bulkSidecarCloseErrorCode, "duplicate bulk connection")
		return
	}
	b.runConn(remote.ID(), conn)
}

func (b *BulkSidecar) acceptAuth(stream *quic.Stream) (*enode.Node, error) {
	var hello bulkAuthHello
	if err := readBulkControl(stream, bulkAuthControlMaxSize, &hello); err != nil {
		return nil, err
	}
	if hello.Version != bulkSidecarVersion {
		return nil, fmt.Errorf("unsupported bulk auth version %d", hello.Version)
	}
	if hello.To != b.localID {
		return nil, errors.New("bulk auth remote target mismatch")
	}
	peer := b.srv.Peer(hello.From)
	if peer == nil || peer.Node() == nil {
		return nil, errBulkSidecarNoPeer
	}
	remote := peer.Node()
	remoteKey := remote.Pubkey()
	if remoteKey == nil {
		return nil, errors.New("bulk auth peer missing pubkey")
	}
	var challenge bulkAuthChallenge
	if _, err := io.ReadFull(crand.Reader, challenge.Nonce[:]); err != nil {
		return nil, err
	}
	hash := bulkAuthTranscriptHash(hello.From, hello.To, hello.Nonce, challenge.Nonce)
	sig, err := crypto.Sign(hash, b.priv)
	if err != nil {
		return nil, err
	}
	challenge.Signature = slices.Clone(sig[:64])
	if err := writeBulkControl(stream, challenge); err != nil {
		return nil, err
	}
	var response bulkAuthResponse
	if err := readBulkControl(stream, bulkAuthControlMaxSize, &response); err != nil {
		return nil, err
	}
	if len(response.Signature) != 64 {
		return nil, errors.New("bulk auth response signature length invalid")
	}
	if !crypto.VerifySignature(crypto.CompressPubkey(remoteKey), hash, response.Signature) {
		return nil, errors.New("bulk auth response signature invalid")
	}
	return remote, nil
}

func (b *BulkSidecar) dialConn(ctx context.Context, remote *enode.Node) (*quic.Conn, error) {
	endpoint, ok := remote.QUICEndpoint()
	if !ok {
		b.log.Debug("Bulk sidecar peer missing QUIC endpoint", "peer", remote.ID(), "node", remote.String(), "ip", remote.IPAddr(), "tcp", remote.TCP(), "udp", remote.UDP())
		return nil, errBulkSidecarNoQUIC
	}
	dialCtx, cancel := context.WithTimeout(ctx, bulkDialTimeout)
	defer cancel()

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{bulkSidecarNextProto},
		MinVersion:         tls.VersionTLS13,
	}
	conn, err := quic.DialAddr(dialCtx, endpoint.String(), tlsConf, b.config)
	if err != nil {
		return nil, err
	}
	if err := b.initiateAuth(conn, remote); err != nil {
		_ = conn.CloseWithError(bulkSidecarProtocolError, err.Error())
		return nil, err
	}
	return conn, nil
}

func (b *BulkSidecar) initiateAuth(conn *quic.Conn, remote *enode.Node) error {
	ctx, cancel := context.WithTimeout(context.Background(), bulkAuthTimeout)
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	var hello bulkAuthHello
	hello.Version = bulkSidecarVersion
	hello.From = b.localID
	hello.To = remote.ID()
	if _, err := io.ReadFull(crand.Reader, hello.Nonce[:]); err != nil {
		return err
	}
	if err := writeBulkControl(stream, hello); err != nil {
		return err
	}
	var challenge bulkAuthChallenge
	if err := readBulkControl(stream, bulkAuthControlMaxSize, &challenge); err != nil {
		return err
	}
	if len(challenge.Signature) != 64 {
		return errors.New("bulk auth challenge signature length invalid")
	}
	remoteKey := remote.Pubkey()
	if remoteKey == nil {
		return errors.New("bulk auth remote pubkey missing")
	}
	hash := bulkAuthTranscriptHash(hello.From, hello.To, hello.Nonce, challenge.Nonce)
	if !crypto.VerifySignature(crypto.CompressPubkey(remoteKey), hash, challenge.Signature) {
		return errors.New("bulk auth challenge signature invalid")
	}
	sig, err := crypto.Sign(hash, b.priv)
	if err != nil {
		return err
	}
	return writeBulkControl(stream, bulkAuthResponse{Signature: slices.Clone(sig[:64])})
}

func (b *BulkSidecar) adoptConn(remote *enode.Node, conn *quic.Conn) bool {
	session := b.session(remote)
	session.lock.Lock()
	defer session.lock.Unlock()
	if session.conn != nil {
		select {
		case <-session.connClosed:
		default:
			return false
		}
	}
	session.conn = conn
	session.connClosed = conn.Context().Done()
	session.channels = make(map[string]MsgReadWriter)
	bulkSidecarStats.markSessionEstablished()
	b.log.Debug("Bulk sidecar session established", "peer", remote.ID(), "remote", conn.RemoteAddr())
	return true
}

func (b *BulkSidecar) runConn(remoteID enode.ID, conn *quic.Conn) {
	b.lock.Lock()
	session := b.sessions[remoteID]
	b.lock.Unlock()
	if session == nil {
		_ = conn.CloseWithError(bulkSidecarCloseErrorCode, "bulk session missing")
		return
	}
	defer b.clearConn(remoteID, conn)
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		if err := session.acceptChannel(stream); err != nil {
			_ = conn.CloseWithError(bulkSidecarProtocolError, err.Error())
			return
		}
	}
}

func (b *BulkSidecar) clearConn(remoteID enode.ID, conn *quic.Conn) {
	b.lock.Lock()
	session := b.sessions[remoteID]
	b.lock.Unlock()
	if session == nil {
		return
	}
	session.lock.Lock()
	defer session.lock.Unlock()
	if session.conn == conn {
		session.conn = nil
		session.connClosed = nil
		for name, waiters := range session.waiters {
			for _, waiter := range waiters {
				waiter <- bulkChannelResult{err: io.EOF}
				close(waiter)
			}
			delete(session.waiters, name)
		}
		session.channels = make(map[string]MsgReadWriter)
	}
}

func (s *bulkSession) close() {
	s.lock.Lock()
	conn := s.conn
	s.conn = nil
	s.connClosed = nil
	for name, waiters := range s.waiters {
		for _, waiter := range waiters {
			waiter <- bulkChannelResult{err: io.EOF}
			close(waiter)
		}
		delete(s.waiters, name)
	}
	s.lock.Unlock()
	if conn != nil {
		_ = conn.CloseWithError(bulkSidecarCloseErrorCode, "bulk peer dropped")
	}
}

func (s *bulkSession) ensureConn(ctx context.Context) (*quic.Conn, error) {
	s.lock.Lock()
	if s.conn != nil {
		select {
		case <-s.connClosed:
			s.conn = nil
			s.connClosed = nil
		default:
			conn := s.conn
			s.lock.Unlock()
			return conn, nil
		}
	}
	if bytes.Compare(s.sidecar.localID[:], s.remoteID[:]) > 0 {
		s.lock.Unlock()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
				s.lock.Lock()
				if s.conn != nil {
					select {
					case <-s.connClosed:
						s.conn = nil
						s.connClosed = nil
					default:
						conn := s.conn
						s.lock.Unlock()
						return conn, nil
					}
				}
				s.lock.Unlock()
			}
		}
	}
	if s.dialing {
		wait := s.dialWait
		s.lock.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.lock.Lock()
		defer s.lock.Unlock()
		if s.conn != nil {
			return s.conn, nil
		}
		return nil, errBulkSidecarNoPeer
	}
	s.dialing = true
	wait := make(chan struct{})
	s.dialWait = wait
	remote := s.remote
	s.lock.Unlock()

	conn, err := s.sidecar.dialConn(ctx, remote)

	s.lock.Lock()
	defer s.lock.Unlock()
	s.dialing = false
	close(wait)
	s.dialWait = nil
	if err != nil {
		return nil, err
	}
	if s.conn == nil {
		s.conn = conn
		s.connClosed = conn.Context().Done()
		s.channels = make(map[string]MsgReadWriter)
		bulkSidecarSessionMeter.Mark(1)
		bulkSidecarStats.markSessionEstablished()
		s.sidecar.log.Debug("Bulk sidecar session established", "peer", s.remoteID, "remote", conn.RemoteAddr())
		go s.sidecar.runConn(s.remoteID, conn)
		return conn, nil
	}
	_ = conn.CloseWithError(bulkSidecarCloseErrorCode, "bulk connection superseded")
	return s.conn, nil
}

func (s *bulkSession) openChannel(ctx context.Context, channel string) (MsgReadWriter, error) {
	if channel == "" || len(channel) > 64 {
		return nil, errors.New("invalid bulk channel")
	}
	if rw, ok := s.getChannel(channel); ok {
		return rw, nil
	}
	conn, err := s.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	if bytes.Compare(s.sidecar.localID[:], s.remoteID[:]) < 0 {
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return nil, err
		}
		if err := writeBulkControl(stream, bulkChannelHello{Version: bulkSidecarVersion, Channel: channel}); err != nil {
			return nil, err
		}
		rw := &bulkStreamMsgRW{
			stream:  stream,
			channel: channel,
			log:     log.New("peer", s.remoteID, "channel", channel),
		}
		s.storeChannel(channel, rw)
		return rw, nil
	}
	return s.waitChannel(ctx, channel)
}

func (s *bulkSession) acceptChannel(stream *quic.Stream) error {
	var hello bulkChannelHello
	if err := readBulkControl(stream, bulkChannelControlMaxSize, &hello); err != nil {
		return err
	}
	if hello.Version != bulkSidecarVersion {
		return fmt.Errorf("unsupported bulk channel version %d", hello.Version)
	}
	if hello.Channel == "" || len(hello.Channel) > 64 {
		return errors.New("invalid bulk channel name")
	}
	s.storeChannel(hello.Channel, &bulkStreamMsgRW{
		stream:  stream,
		channel: hello.Channel,
		log:     log.New("peer", s.remoteID, "channel", hello.Channel),
	})
	return nil
}

func (s *bulkSession) getChannel(channel string) (MsgReadWriter, bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	rw, ok := s.channels[channel]
	return rw, ok
}

func (s *bulkSession) waitChannel(ctx context.Context, channel string) (MsgReadWriter, error) {
	if rw, ok := s.getChannel(channel); ok {
		return rw, nil
	}
	waiter := make(chan bulkChannelResult, 1)
	s.lock.Lock()
	if rw, ok := s.channels[channel]; ok {
		s.lock.Unlock()
		return rw, nil
	}
	s.waiters[channel] = append(s.waiters[channel], waiter)
	s.lock.Unlock()

	select {
	case result := <-waiter:
		return result.rw, result.err
	case <-ctx.Done():
		return nil, errBulkChannelTimeout
	}
}

func (s *bulkSession) storeChannel(channel string, rw MsgReadWriter) {
	s.lock.Lock()
	_, exists := s.channels[channel]
	s.channels[channel] = rw
	waiters := s.waiters[channel]
	delete(s.waiters, channel)
	s.lock.Unlock()

	if exists {
		bulkSidecarChannelReplaceMeter.Mark(1)
		bulkSidecarStats.markChannelReplaced(channel)
	} else {
		bulkSidecarChannelOpenMeter.Mark(1)
		bulkSidecarStats.markChannelOpened(channel)
	}
	s.sidecar.log.Debug("Bulk sidecar channel opened", "peer", s.remoteID, "channel", channel)

	for _, waiter := range waiters {
		waiter <- bulkChannelResult{rw: rw}
		close(waiter)
	}
}

func (rw *bulkStreamMsgRW) ReadMsg() (Msg, error) {
	var header [bulkFrameHeaderSize]byte
	if _, err := io.ReadFull(rw.stream, header[:]); err != nil {
		return Msg{}, err
	}
	size := binary.BigEndian.Uint32(header[8:])
	if size > bulkMaxMessageSize {
		return Msg{}, fmt.Errorf("bulk message too large: %d", size)
	}
	if err := rw.stream.SetReadDeadline(time.Now().Add(bulkMessageReadTimeout)); err != nil {
		return Msg{}, err
	}
	msg := Msg{
		Code:    binary.BigEndian.Uint64(header[:8]),
		Size:    size,
		Payload: io.LimitReader(rw.stream, int64(size)),
	}
	bulkSidecarStats.markChannelRead(rw.channel)
	rw.log.Trace("Bulk sidecar read message", "code", msg.Code, "size", msg.Size)
	return msg, nil
}

func (rw *bulkStreamMsgRW) WriteMsg(msg Msg) error {
	rw.write.Lock()
	defer rw.write.Unlock()

	if msg.Size > bulkMaxMessageSize {
		return fmt.Errorf("bulk message too large: %d", msg.Size)
	}
	if err := rw.stream.SetWriteDeadline(time.Now().Add(bulkMessageWriteTimeout)); err != nil {
		return err
	}
	var header [bulkFrameHeaderSize]byte
	binary.BigEndian.PutUint64(header[:8], msg.Code)
	binary.BigEndian.PutUint32(header[8:], msg.Size)
	if _, err := rw.stream.Write(header[:]); err != nil {
		return err
	}
	n, err := io.CopyN(rw.stream, msg.Payload, int64(msg.Size))
	if err != nil {
		return err
	}
	if n != int64(msg.Size) {
		return io.ErrUnexpectedEOF
	}
	bulkSidecarStats.markChannelWrite(rw.channel)
	rw.log.Trace("Bulk sidecar wrote message", "code", msg.Code, "size", msg.Size)
	return nil
}

func writeBulkControl(stream *quic.Stream, msg interface{}) error {
	payload, err := rlp.EncodeToBytes(msg)
	if err != nil {
		return err
	}
	if len(payload) > bulkAuthControlMaxSize {
		return errors.New("bulk control payload too large")
	}
	if err := stream.SetWriteDeadline(time.Now().Add(bulkAuthTimeout)); err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
	if _, err := stream.Write(size[:]); err != nil {
		return err
	}
	_, err = stream.Write(payload)
	return err
}

func configureBulkSidecarSocketBuffers(conn *net.UDPConn) BulkSidecarSocketBuffers {
	status := BulkSidecarSocketBuffers{
		ConfiguredReadBuffer:  bulkSocketReadBufferSize,
		ConfiguredWriteBuffer: bulkSocketWriteBufferSize,
	}
	if err := conn.SetReadBuffer(status.ConfiguredReadBuffer); err != nil {
		status.ConfiguredReadBuffer = 0
	}
	if err := conn.SetWriteBuffer(status.ConfiguredWriteBuffer); err != nil {
		status.ConfiguredWriteBuffer = 0
	}
	status.ActualReadBuffer = socketBufferSize(conn, syscall.SO_RCVBUF)
	status.ActualWriteBuffer = socketBufferSize(conn, syscall.SO_SNDBUF)
	return status
}

func socketBufferSize(conn *net.UDPConn, opt int) int {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	value := 0
	controlErr := rawConn.Control(func(fd uintptr) {
		if size, sockErr := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, opt); sockErr == nil {
			value = size
		}
	})
	if controlErr != nil {
		return 0
	}
	return value
}

func readBulkControl(stream *quic.Stream, maxSize uint32, out interface{}) error {
	if err := stream.SetReadDeadline(time.Now().Add(bulkAuthTimeout)); err != nil {
		return err
	}
	var size [4]byte
	if _, err := io.ReadFull(stream, size[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > maxSize {
		return fmt.Errorf("bulk control payload invalid size %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(stream, payload); err != nil {
		return err
	}
	return rlp.DecodeBytes(payload, out)
}

func bulkAuthTranscriptHash(from, to enode.ID, nonceA, nonceB [32]byte) []byte {
	return crypto.Keccak256(
		[]byte("bor bulk sidecar auth"),
		from[:],
		to[:],
		nonceA[:],
		nonceB[:],
	)
}

func generateBulkSidecarCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := crand.Int(crand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
