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
	"fmt"
	"sync"
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

// ethWitRequest wraps an eth.Request and holds the underlying wit.Request.
// This allows the downloader to track the request lifecycle via the eth.Request
// while allowing cancellation to be passed to the wit.Request.
type ethWitRequest struct {
	*eth.Request              // Embedded eth.Request (must be non-nil)
	witReq       *wit.Request // The actual witness protocol request
}

// Close overrides the embedded eth.Request's Close to also close the wit.Request.
func (r *ethWitRequest) Close() error {
	// First, close the underlying witness request.
	err := r.witReq.Close()
	// Then, call the embedded eth.Request's Close (if it exists and does anything).
	// If eth.Request.Close() is a no-op or doesn't exist, this could be removed.
	if r.Request != nil {
		// Assuming eth.Request has a Close method. If not, remove this line.
		// r.Request.Close() // Example: eth.Request might not have Close()
	}
	return err
}

// RequestWitnesses implements downloader.Peer.
// It requests witnesses using the wit protocol for the given block hashes.
// NOTE: Response adaptation lacks robust cancellation checks.
func (p *ethPeer) RequestWitnesses(hashes []common.Hash, dlResCh chan *eth.Response) (*eth.Request, error) {
	// Check if peer supports the witness protocol
	if p.witPeer == nil {
		return nil, errors.New("peer does not support witness protocol (wit)")
	}

	if len(hashes) == 0 {
		return nil, errors.New("RequestWitnesses called with empty hash list")
	}

	p.Log().Debug("Requesting witnesses via hash list", "count", len(hashes), "first_hash", hashes[0])

	// --- Call Underlying wit.Peer ---
	witSink := make(chan *wit.Response, 100)
	// Call the updated wit peer's request method with the full hashes slice
	witReq, err := p.witPeer.Peer.RequestWitness(hashes, witSink)
	if err != nil {
		return nil, fmt.Errorf("witPeer.RequestWitness failed: %w", err)
	}

	// --- Create the eth.Request wrapper ---
	ethReqBase := &eth.Request{
		Peer: p.Peer.ID(),
		Sent: time.Now(),
	}
	wrapper := &ethWitRequest{
		Request: ethReqBase,
		witReq:  witReq,
	}

	// --- Adapt Response Channel (Goroutine) ---
	adaptWg := sync.WaitGroup{}
	adaptWg.Add(1)
	go func() {
		defer adaptWg.Done()
		p.Log().Trace("Witness response adapter goroutine started", "peer", p.ID())
		select {
		case witRes := <-witSink:
			var reqID uint64
			if witRes != nil {
				switch resp := witRes.Res.(type) {
				case *wit.WitnessPacketRLPPacket:
					reqID = resp.RequestId
				}
			}
			p.Log().Trace("Witness response received from witSink", "peer", p.ID(), "reqID", reqID, "is_nil", witRes == nil)
			if witRes == nil {
				p.Log().Warn("Nil response received from wit sink", "peer", p.ID(), "reqID", reqID)
				return
			}

			// --- Response Adaptation ---
			p.Log().Trace("Adapting witness response", "peer", p.ID(), "reqID", reqID)
			witnessRespPacket, ok := witRes.Res.(*wit.WitnessPacketRLPPacket)
			if !ok {
				p.Log().Error("Invalid witness response type/field from wit sink", "peer", p.ID(), "reqID", reqID, "type", fmt.Sprintf("%T", witRes.Res))
				return
			}
			reqID = witnessRespPacket.RequestId

			// Construct eth.Response, using the embedded Request from the wrapper
			dlRes := &eth.Response{
				Req: wrapper.Request,
				Res: *witnessRespPacket,
			}

			dlRes.Done = make(chan error, 1)

			// Send adapted response (Incomplete: lacks cancellation checks)
			p.Log().Trace("Sending adapted witness response to dlResCh", "peer", p.ID(), "reqID", reqID)
			select {
			case dlResCh <- dlRes:
				p.Log().Trace("Forwarded witness response to downloader", "peer", p.ID(), "reqID", reqID)
			}
		}
		p.Log().Trace("Witness response adapter goroutine finished", "peer", p.ID())
	}()

	// --- Return Value ---
	return wrapper.Request, nil
}
