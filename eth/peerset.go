// Copyright 2020 The go-ethereum Authors
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

package eth

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/p2p"
)

var (
	// errPeerSetClosed is returned if a peer is attempted to be added or removed
	// from the peer set after it has been terminated.
	errPeerSetClosed = errors.New("peerset closed")

	// errPeerAlreadyRegistered is returned if a peer is attempted to be added
	// to the peer set, but one with the same id already exists.
	errPeerAlreadyRegistered = errors.New("peer already registered")

	// errPeerNotRegistered is returned if a peer is attempted to be removed from
	// a peer set, but no peer with the given id exists.
	errPeerNotRegistered = errors.New("peer not registered")

	// errSnapWithoutEth is returned if a peer attempts to connect only on the
	// snap protocol without advertising the eth main protocol.
	errSnapWithoutEth = errors.New("peer connected on snap without compatible eth support")

	// errWitWithoutEth is returned if a peer attempts to connect only on the
	// wit protocol without advertising the eth main protocol.
	errWitWithoutEth = errors.New("peer connected on wit without compatible eth support")
)

// peerSet represents the collection of active peers currently participating in
// the `eth` protocol, with or without the `snap` extension.
type peerSet struct {
	peers     map[string]*ethPeer // Peers connected on the `eth` protocol
	snapPeers int                 // Number of `snap` compatible peers for connection prioritization
	witPeers  int                 // Number of `wit` compatible peers for connection prioritization

	snapWait map[string]chan *snap.Peer // Peers connected on `eth` waiting for their snap extension
	snapPend map[string]*snap.Peer      // Peers connected on the `snap` protocol, but not yet on `eth`

	witWait map[string]chan *wit.Peer // Peers connected on `eth` waiting for their wit extension
	witPend map[string]*wit.Peer      // Peers connected on the `wit` protocol, but not yet on `eth`

	lock   sync.RWMutex
	closed bool
	quitCh chan struct{} // Quit channel to signal termination
}

// newPeerSet creates a new peer set to track the active participants.
func newPeerSet() *peerSet {
	return &peerSet{
		peers:    make(map[string]*ethPeer),
		snapWait: make(map[string]chan *snap.Peer),
		snapPend: make(map[string]*snap.Peer),
		witWait:  make(map[string]chan *wit.Peer),
		witPend:  make(map[string]*wit.Peer),
		quitCh:   make(chan struct{}),
	}
}

// registerSnapExtension unblocks an already connected `eth` peer waiting for its
// `snap` extension, or if no such peer exists, tracks the extension for the time
// being until the `eth` main protocol starts looking for it.
func (ps *peerSet) registerSnapExtension(peer *snap.Peer) error {
	// Reject the peer if it advertises `snap` without `eth` as `snap` is only a
	// satellite protocol meaningful with the chain selection of `eth`
	if !peer.RunningCap(eth.ProtocolName, eth.ProtocolVersions) {
		return fmt.Errorf("%w: have %v", errSnapWithoutEth, peer.Caps())
	}
	// Ensure nobody can double connect
	ps.lock.Lock()
	defer ps.lock.Unlock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}

	if _, ok := ps.snapPend[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// Inject the peer into an `eth` counterpart is available, otherwise save for later
	if wait, ok := ps.snapWait[id]; ok {
		delete(ps.snapWait, id)
		wait <- peer

		return nil
	}

	ps.snapPend[id] = peer

	return nil
}

// registerWitExtension unblocks an already connected `eth` peer waiting for its
// `wit` extension, or if no such peer exists, tracks the extension for the time
// being until the `eth` main protocol starts looking for it.
func (ps *peerSet) registerWitExtension(peer *wit.Peer) error {
	// Reject the peer if it advertises `wit` without `eth` as `wit` is only a
	// satellite protocol meaningful with the chain selection of `eth`
	if !peer.RunningCap(eth.ProtocolName, eth.ProtocolVersions) {
		return fmt.Errorf("%w: have %v", errWitWithoutEth, peer.Caps())
	}
	// Ensure nobody can double connect
	ps.lock.Lock()
	defer ps.lock.Unlock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}

	if _, ok := ps.witPend[id]; ok {
		return errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// Inject the peer into an `eth` counterpart is available, otherwise save for later
	if wait, ok := ps.witWait[id]; ok {
		delete(ps.witWait, id)
		wait <- peer

		return nil
	}

	ps.witPend[id] = peer

	return nil
}

// waitSnapExtension blocks until all satellite protocols are connected and tracked
// by the peerset.
func (ps *peerSet) waitSnapExtension(peer *eth.Peer) (*snap.Peer, error) {
	// If the peer does not support a compatible `snap`, don't wait
	if !peer.RunningCap(snap.ProtocolName, snap.ProtocolVersions) {
		return nil, nil
	}
	// Ensure nobody can double connect
	ps.lock.Lock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}

	if _, ok := ps.snapWait[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as pending ones
	}
	// If `snap` already connected, retrieve the peer from the pending set
	if snap, ok := ps.snapPend[id]; ok {
		delete(ps.snapPend, id)

		ps.lock.Unlock()

		return snap, nil
	}
	// Otherwise wait for `snap` to connect concurrently
	wait := make(chan *snap.Peer)
	ps.snapWait[id] = wait
	ps.lock.Unlock()

	select {
	case p := <-wait:
		return p, nil
	case <-ps.quitCh:
		ps.lock.Lock()
		delete(ps.snapWait, id)
		ps.lock.Unlock()
		return nil, errPeerSetClosed
	}
}

// waitWitExtension blocks until all satellite protocols are connected and tracked
// by the peerset.
func (ps *peerSet) waitWitExtension(peer *eth.Peer) (*wit.Peer, error) {
	// If the peer does not support a compatible `wit`, don't wait
	if !peer.RunningCap(wit.ProtocolName, wit.ProtocolVersions) {
		return nil, nil
	}

	// Ensure nobody can double connect
	ps.lock.Lock()

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		ps.lock.Unlock()
		return nil, errPeerAlreadyRegistered // avoid connections with the same id as existing ones
	}

	// If `wit` already connected, retrieve the peer from the pending set
	if wit, ok := ps.witPend[id]; ok {
		delete(ps.witPend, id)

		ps.lock.Unlock()

		return wit, nil
	}

	// Otherwise wait for `wit` to connect concurrently
	wait := make(chan *wit.Peer)
	ps.witWait[id] = wait
	ps.lock.Unlock()

	select {
	case p := <-wait:
		return p, nil
	case <-ps.quitCh:
		ps.lock.Lock()
		delete(ps.witWait, id)
		ps.lock.Unlock()
		return nil, errPeerSetClosed
	}
}

// registerPeer injects a new `eth` peer into the working set, or returns an error
// if the peer is already known.
func (ps *peerSet) registerPeer(peer *eth.Peer, extSnap *snap.Peer, extWit *wit.Peer) error {
	// Start tracking the new peer
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errPeerSetClosed
	}

	id := peer.ID()
	if _, ok := ps.peers[id]; ok {
		return errPeerAlreadyRegistered
	}

	eth := &ethPeer{
		Peer: peer,
	}
	if extSnap != nil {
		eth.snapExt = &snapPeer{extSnap}
		ps.snapPeers++
	}
	if extWit != nil {
		eth.witPeer = &witPeer{extWit}
		ps.witPeers++
	}

	ps.peers[id] = eth

	return nil
}

// unregisterPeer removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *peerSet) unregisterPeer(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	peer, ok := ps.peers[id]
	if !ok {
		return errPeerNotRegistered
	}

	delete(ps.peers, id)

	if peer.snapExt != nil {
		ps.snapPeers--
	}

	return nil
}

// peer retrieves the registered peer with the given id.
func (ps *peerSet) peer(id string) *ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// peersWithWitnessCandidates returns every candidate body source for `hash`,
// body-known peers first, then announce-only peers — same ordering rationale
// as getOnePeerWithWitness, but as a full list so a caller that needs to
// exclude one specific peer (e.g. the requester that just asked us, who by
// construction doesn't have it) still has a real fallback instead of giving
// up when that peer happens to be the single best candidate.
func (ps *peerSet) peersWithWitnessCandidates(hash common.Hash) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var withBody, announceOnly []*ethPeer
	for _, p := range ps.peers {
		if p.witPeer == nil {
			continue
		}
		if p.witPeer.Peer.KnownWitnessContainsHash(hash) {
			withBody = append(withBody, p)
		} else if p.witPeer.Peer.KnownAnnounceContainsHash(hash) {
			announceOnly = append(announceOnly, p)
		}
	}
	return append(withBody, announceOnly...)
}

// getOnePeerWithWitness returns a candidate body source for `hash`. Body-known
// peers (those that broadcast or served the body) are preferred; if none is
// available we fall back to peers that have only relayed a WIT2 signed
// announcement. The fast-path latency win depends on this fallback: at hop>=2
// the signed announce arrives long before the body broadcast, and the only
// peer that could serve us bytes is the one that forwarded the announce.
//
// Asking an announce-only peer is safe because byte-blame in
// witnessManager.verifyAgainstSignedHash only drops on a confirmed hash
// mismatch — empty/unavailable responses surface as soft failures, not drops.
func (ps *peerSet) getOnePeerWithWitness(hash common.Hash) *ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var announceFallback *ethPeer
	for _, p := range ps.peers {
		if p.witPeer == nil {
			continue
		}
		if p.witPeer.Peer.KnownWitnessContainsHash(hash) {
			return p
		}
		if announceFallback == nil && p.witPeer.Peer.KnownAnnounceContainsHash(hash) {
			announceFallback = p
		}
	}
	return announceFallback
}

// peersWithoutWitness retrives a list of peers that do nor have a given witness
// in their set of known hashes so it might be propagated to them.
// This is used to avoid sending the same witness to the same peer multiple times.
func (ps *peerSet) peersWithoutWitness(hash common.Hash) []*witPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*witPeer, 0, len(ps.peers))

	for _, p := range ps.peers {
		if p.witPeer != nil && !p.witPeer.Peer.KnownWitnessContainsHash(hash) {
			list = append(list, p.witPeer)
		}
	}

	return list
}

// peersWithoutSignedAnnounce returns peers that have neither received the body
// for `hash` nor seen a signed announcement for it. Used by WIT2 relay to skip
// peers that already know about the announcement, preventing announce storms,
// without ever assuming a peer that only saw an announcement holds the body.
func (ps *peerSet) peersWithoutSignedAnnounce(hash common.Hash) []*witPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*witPeer, 0, len(ps.peers))

	for _, p := range ps.peers {
		if p.witPeer == nil {
			continue
		}
		if p.witPeer.Peer.KnownWitnessContainsHash(hash) {
			continue
		}
		if p.witPeer.Peer.KnownAnnounceContainsHash(hash) {
			continue
		}
		list = append(list, p.witPeer)
	}

	return list
}

// peersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes so it might be propagated to them.
func (ps *peerSet) peersWithoutBlock(hash common.Hash) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*ethPeer, 0, len(ps.peers))

	for _, p := range ps.peers {
		if !p.KnownBlock(hash) {
			list = append(list, p)
		}
	}

	return list
}

// peersWithoutTransaction retrieves a list of peers that do not have a given
// transaction in their set of known hashes.
//
//nolint:unused
func (ps *peerSet) peersWithoutTransaction(hash common.Hash) []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*ethPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.KnownTransaction(hash) {
			list = append(list, p)
		}
	}
	return list
}

// all returns all current peers.
func (ps *peerSet) all() []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return slices.Collect(maps.Values(ps.peers))
}

// len returns if the current number of `eth` peers in the set. Since the `snap`
// peers are tied to the existence of an `eth` connection, that will always be a
// subset of `eth`.
func (ps *peerSet) len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// snapLen returns if the current number of `snap` peers in the set.
func (ps *peerSet) snapLen() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.snapPeers
}

// getAllPeers returns all connected peers
func (ps *peerSet) getAllPeers() []*ethPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	peers := make([]*ethPeer, 0, len(ps.peers))
	for _, peer := range ps.peers {
		peers = append(peers, peer)
	}
	return peers
}

// peerWithHighestTD retrieves the known peer with the currently highest total
// difficulty.
func (ps *peerSet) peerWithHighestTD(backoff func(string) time.Duration) (*eth.Peer, time.Duration) {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var (
		bestPeer        *eth.Peer
		bestTd          *big.Int
		shortestBackoff time.Duration
	)

	for _, p := range ps.peers {
		if backoff != nil {
			if remaining := backoff(p.ID()); remaining > 0 {
				if shortestBackoff == 0 || remaining < shortestBackoff {
					shortestBackoff = remaining
				}
				continue
			}
		}
		if _, td := p.Head(); bestPeer == nil || td.Cmp(bestTd) > 0 {
			bestPeer, bestTd = p.Peer, td
		}
	}

	return bestPeer, shortestBackoff
}

// close disconnects all peers.
func (ps *peerSet) close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	for _, p := range ps.peers {
		//nolint:typecheck
		p.Disconnect(p2p.DiscQuitting)
	}
	if !ps.closed {
		close(ps.quitCh)
	}
	ps.closed = true
}

// ForgetTransactions removes the given transaction hashes from all peers'
// known transaction sets, allowing them to be re-broadcast.
func (ps *peerSet) ForgetTransactions(hashes []common.Hash) {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	for _, p := range ps.peers {
		p.Peer.ForgetTransactions(hashes)
	}
}
