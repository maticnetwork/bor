// Copyright 2015 The go-ethereum Authors
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
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
)

// ethPeerInfo represents a short summary of the `eth` sub-protocol metadata known
// about a connected peer.
type ethPeerInfo struct {
	Version uint `json:"version"` // Ethereum protocol version negotiated
}

// ethPeer is a wrapper around eth.Peer to maintain a few extra metadata.
type ethPeer struct {
	*eth.Peer
	snapExt *snapPeer // Satellite `snap` connection
	witPeer *witPeer
}

// info gathers and returns some `eth` protocol metadata known about a peer.
// nolint:typecheck
func (p *ethPeer) info() *ethPeerInfo {
	return &ethPeerInfo{
		Version: p.Version(),
	}
}

// snapPeerInfo represents a short summary of the `snap` sub-protocol metadata known
// about a connected peer.
type snapPeerInfo struct {
	Version uint `json:"version"` // Snapshot protocol version negotiated
}

// snapPeer is a wrapper around snap.Peer to maintain a few extra metadata.
type snapPeer struct {
	*snap.Peer
}

// info gathers and returns some `snap` protocol metadata known about a peer.
func (p *snapPeer) info() *snapPeerInfo {
	return &snapPeerInfo{
		Version: p.Version(),
	}
}

type witPeerInfo struct {
	Version uint `json:"version"` // Witness protocol version negotiated
}

// witPeer is wrapper around wit.Peer to maintain a few extra metadata.
type witPeer struct {
	*wit.Peer
}

// info gathers and returns some `wit` protocol metadata known about a peer.
func (p *witPeer) info() *witPeerInfo {
	return &witPeerInfo{
		Version: p.Version(),
	}
}

// witRequestAdapter implements the minimal interface needed by the downloader
// while keeping witness requests completely separate from eth protocol tracking.
type witRequestAdapter struct {
	id     uint64        // Unique ID with high bit set
	peerID string        // Peer identifier
	cancel chan struct{} // Cancellation channel
	sent   time.Time     // Timestamp when sent
	witReq *wit.Request  // The actual witness request
}

// Peer returns the peer ID (for downloader compatibility)
func (r *witRequestAdapter) Peer() string {
	return r.peerID
}

// Cancel returns the cancellation channel (for downloader compatibility)
func (r *witRequestAdapter) Cancel() <-chan struct{} {
	return r.cancel
}

// Sent returns when the request was sent (for timeout tracking)
func (r *witRequestAdapter) Sent() time.Time {
	return r.sent
}

// Close cancels the request
func (r *witRequestAdapter) Close() error {
	// Close our cancel channel
	if r.cancel != nil {
		select {
		case <-r.cancel:
			// Already closed
		default:
			close(r.cancel)
		}
	}
	// Close the underlying witness request
	if r.witReq != nil {
		return r.witReq.Close()
	}
	return nil
}

// RequestWitnesses implements downloader.Peer.
// It requests witnesses using the wit protocol for the given block hashes.
// Unlike eth protocol requests, witness requests use their own tracking mechanism
// to avoid ID collisions.
func (p *ethPeer) RequestWitnesses(hashes []common.Hash, dlResCh chan *eth.Response) (*eth.Request, error) {
	if p.witPeer == nil {
		return nil, errors.New("witness peer not found")
	}

	// Generate a unique ID for this witness request
	adapterID := rand.Uint64()

	p.witPeer.Log().Trace("RequestWitnesses called", "peer", p.ID(), "hashes", len(hashes), "adapterID", adapterID)

	// Create a channel for the underlying witness protocol responses
	witResCh := make(chan *wit.Response)

	// Make the actual request via the witness protocol peer
	witReq, err := p.witPeer.Peer.RequestWitness(hashes, witResCh)
	if err != nil {
		p.witPeer.Log().Error("RequestWitnesses failed to make wit request", "peer", p.ID(), "err", err)
		return nil, err
	}

	p.witPeer.Log().Trace("RequestWitnesses made wit request", "peer", p.ID(), "witReqID", witReq.ID())

	// Create an adapter that satisfies the downloader interface without
	// interfering with eth protocol tracking
	adapter := &witRequestAdapter{
		id:     adapterID,
		peerID: p.ID(),
		cancel: make(chan struct{}),
		sent:   time.Now(),
		witReq: witReq,
	}

	// Create a minimal eth.Request that wraps our adapter
	// This is needed because the downloader expects *eth.Request specifically
	ethReq := &eth.Request{
		Peer:   adapter.peerID,
		Cancel: adapter.cancel,
		Sent:   adapter.sent,
		// The id field remains unset (0) - it won't be used for tracking
	}

	// Start a goroutine to adapt responses from wit to eth format
	go func() {
		p.witPeer.Log().Trace("RequestWitnesses adapter goroutine started", "peer", p.ID())
		defer p.witPeer.Log().Trace("RequestWitnesses adapter goroutine finished", "peer", p.ID())

		for witRes := range witResCh {
			p.witPeer.Log().Trace("RequestWitnesses adapter received wit response", "peer", p.ID())

			// Create eth.Response that references our eth.Request
			ethRes := &eth.Response{
				Req:  ethReq,
				Res:  witRes.Res,
				Meta: witRes.Meta,
				Time: witRes.Time,
				Done: witRes.Done,
			}

			// Forward the response or exit if cancelled
			select {
			case dlResCh <- ethRes:
				p.witPeer.Log().Trace("RequestWitnesses adapter forwarded eth response", "peer", p.ID())
			case <-adapter.cancel:
				p.witPeer.Log().Trace("RequestWitnesses adapter cancelled", "peer", p.ID())
				return
			}
		}
	}()

	// Return the eth.Request to satisfy the interface
	p.witPeer.Log().Trace("RequestWitnesses returning eth request", "peer", p.ID(), "adapterID", adapterID)
	return ethReq, nil
}
