package p2p

import (
	"slices"
	"sync"

	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

type BulkSidecarChannelCounters struct {
	Opened        uint64 `json:"opened"`
	Replaced      uint64 `json:"replaced"`
	ReadMessages  uint64 `json:"readMessages"`
	WriteMessages uint64 `json:"writeMessages"`
	ReadTimeouts  uint64 `json:"readTimeouts"`
	ReadErrors    uint64 `json:"readErrors"`
	WriteFallback uint64 `json:"writeFallback"`
}

type BulkSidecarCounters struct {
	SessionsEstablished uint64                                `json:"sessionsEstablished"`
	Channels            map[string]BulkSidecarChannelCounters `json:"channels,omitempty"`
}

type BulkSidecarSocketBuffers struct {
	ConfiguredReadBuffer  int `json:"configuredReadBuffer,omitempty"`
	ActualReadBuffer      int `json:"actualReadBuffer,omitempty"`
	ConfiguredWriteBuffer int `json:"configuredWriteBuffer,omitempty"`
	ActualWriteBuffer     int `json:"actualWriteBuffer,omitempty"`
}

type BulkSidecarWireCounters struct {
	PacketsSent     uint64 `json:"packetsSent"`
	PacketsReceived uint64 `json:"packetsReceived"`
	PacketsDropped  uint64 `json:"packetsDropped"`
	BytesSent       uint64 `json:"bytesSent"`
	BytesReceived   uint64 `json:"bytesReceived"`
	BytesDropped    uint64 `json:"bytesDropped"`
}

type BulkSidecarPeerStatus struct {
	PeerID    string   `json:"peerId"`
	Connected bool     `json:"connected"`
	Dialing   bool     `json:"dialing"`
	Channels  []string `json:"channels,omitempty"`
}

type BulkSidecarStatus struct {
	Enabled        bool                     `json:"enabled"`
	ListenAddr     string                   `json:"listenAddr,omitempty"`
	ActiveSessions int                      `json:"activeSessions"`
	ActiveChannels int                      `json:"activeChannels"`
	Peers          []BulkSidecarPeerStatus  `json:"peers,omitempty"`
	SocketBuffers  BulkSidecarSocketBuffers `json:"socketBuffers,omitempty"`
	Counters       BulkSidecarCounters      `json:"counters"`
	Wire           BulkSidecarWireCounters  `json:"wire"`
}

type bulkSidecarStatsBook struct {
	lock                sync.Mutex
	sessionsEstablished uint64
	channels            map[string]*BulkSidecarChannelCounters
	socketBuffers       BulkSidecarSocketBuffers
	wire                BulkSidecarWireCounters
}

var bulkSidecarStats = &bulkSidecarStatsBook{
	channels: make(map[string]*BulkSidecarChannelCounters),
}

func (b *BulkSidecar) Status() BulkSidecarStatus {
	if b == nil {
		return BulkSidecarStatus{}
	}
	status := BulkSidecarStatus{
		Enabled:    true,
		ListenAddr: b.Addr().String(),
	}
	status.Counters, status.SocketBuffers, status.Wire = bulkSidecarStats.snapshot()
	b.lock.Lock()
	defer b.lock.Unlock()

	status.Peers = make([]BulkSidecarPeerStatus, 0, len(b.sessions))
	for _, session := range b.sessions {
		peer := session.status()
		if peer.Connected {
			status.ActiveSessions++
		}
		status.ActiveChannels += len(peer.Channels)
		status.Peers = append(status.Peers, peer)
	}
	slices.SortFunc(status.Peers, func(a, b BulkSidecarPeerStatus) int {
		switch {
		case a.PeerID < b.PeerID:
			return -1
		case a.PeerID > b.PeerID:
			return 1
		default:
			return 0
		}
	})
	return status
}

func (s *bulkSession) status() BulkSidecarPeerStatus {
	s.lock.Lock()
	defer s.lock.Unlock()

	channels := make([]string, 0, len(s.channels))
	for name := range s.channels {
		channels = append(channels, name)
	}
	slices.Sort(channels)

	return BulkSidecarPeerStatus{
		PeerID:    s.remoteID.String(),
		Connected: s.conn != nil,
		Dialing:   s.dialing,
		Channels:  channels,
	}
}

func (s *bulkSidecarStatsBook) markSessionEstablished() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.sessionsEstablished++
}

func (s *bulkSidecarStatsBook) markChannelOpened(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).Opened++
}

func (s *bulkSidecarStatsBook) markChannelReplaced(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).Replaced++
}

func (s *bulkSidecarStatsBook) markChannelRead(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).ReadMessages++
}

func (s *bulkSidecarStatsBook) markChannelWrite(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).WriteMessages++
}

func (s *bulkSidecarStatsBook) markChannelReadTimeout(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).ReadTimeouts++
}

func (s *bulkSidecarStatsBook) markChannelReadError(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).ReadErrors++
}

func (s *bulkSidecarStatsBook) markChannelWriteFallback(channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.channel(channel).WriteFallback++
}

func (s *bulkSidecarStatsBook) setSocketBuffers(status BulkSidecarSocketBuffers) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.socketBuffers = status
}

func (s *bulkSidecarStatsBook) markPacketSent(length int) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.wire.PacketsSent++
	s.wire.BytesSent += uint64(length)
}

func (s *bulkSidecarStatsBook) markPacketReceived(length int) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.wire.PacketsReceived++
	s.wire.BytesReceived += uint64(length)
}

func (s *bulkSidecarStatsBook) markPacketDropped(length int) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.wire.PacketsDropped++
	s.wire.BytesDropped += uint64(length)
}

func (s *bulkSidecarStatsBook) snapshot() (BulkSidecarCounters, BulkSidecarSocketBuffers, BulkSidecarWireCounters) {
	s.lock.Lock()
	defer s.lock.Unlock()

	out := BulkSidecarCounters{
		SessionsEstablished: s.sessionsEstablished,
		Channels:            make(map[string]BulkSidecarChannelCounters, len(s.channels)),
	}
	for name, counters := range s.channels {
		out.Channels[name] = *counters
	}
	return out, s.socketBuffers, s.wire
}

func (s *bulkSidecarStatsBook) channel(name string) *BulkSidecarChannelCounters {
	counters := s.channels[name]
	if counters == nil {
		counters = new(BulkSidecarChannelCounters)
		s.channels[name] = counters
	}
	return counters
}

func (s *bulkSidecarStatsBook) newTransportRecorder() qlogwriter.Recorder {
	return &bulkSidecarQlogRecorder{stats: s}
}

func (s *bulkSidecarStatsBook) newConnectionTrace() qlogwriter.Trace {
	return &bulkSidecarQlogTrace{stats: s}
}

type bulkSidecarQlogTrace struct {
	stats *bulkSidecarStatsBook
}

func (t *bulkSidecarQlogTrace) SupportsSchemas(string) bool { return true }

func (t *bulkSidecarQlogTrace) AddProducer() qlogwriter.Recorder {
	return &bulkSidecarQlogRecorder{stats: t.stats}
}

type bulkSidecarQlogRecorder struct {
	stats *bulkSidecarStatsBook
}

func (r *bulkSidecarQlogRecorder) RecordEvent(ev qlogwriter.Event) {
	switch event := ev.(type) {
	case qlog.PacketSent:
		r.stats.markPacketSent(event.Raw.Length)
	case qlog.PacketReceived:
		r.stats.markPacketReceived(event.Raw.Length)
	case qlog.PacketDropped:
		r.stats.markPacketDropped(event.Raw.Length)
	}
}

func (r *bulkSidecarQlogRecorder) Close() error { return nil }
