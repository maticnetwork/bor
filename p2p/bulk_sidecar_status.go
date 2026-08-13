package p2p

import (
	"slices"
	"sync"
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

type BulkSidecarPeerStatus struct {
	PeerID    string   `json:"peerId"`
	Connected bool     `json:"connected"`
	Dialing   bool     `json:"dialing"`
	Channels  []string `json:"channels,omitempty"`
}

type BulkSidecarStatus struct {
	Enabled        bool                   `json:"enabled"`
	ListenAddr     string                 `json:"listenAddr,omitempty"`
	ActiveSessions int                    `json:"activeSessions"`
	ActiveChannels int                    `json:"activeChannels"`
	Peers          []BulkSidecarPeerStatus `json:"peers,omitempty"`
	Counters       BulkSidecarCounters    `json:"counters"`
}

type bulkSidecarStatsBook struct {
	lock                sync.Mutex
	sessionsEstablished uint64
	channels            map[string]*BulkSidecarChannelCounters
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
		Counters:   bulkSidecarStats.snapshot(),
	}
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

func (s *bulkSidecarStatsBook) snapshot() BulkSidecarCounters {
	s.lock.Lock()
	defer s.lock.Unlock()

	out := BulkSidecarCounters{
		SessionsEstablished: s.sessionsEstablished,
		Channels:            make(map[string]BulkSidecarChannelCounters, len(s.channels)),
	}
	for name, counters := range s.channels {
		out.Channels[name] = *counters
	}
	return out
}

func (s *bulkSidecarStatsBook) channel(name string) *BulkSidecarChannelCounters {
	counters := s.channels[name]
	if counters == nil {
		counters = new(BulkSidecarChannelCounters)
		s.channels[name] = counters
	}
	return counters
}
