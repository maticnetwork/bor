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

// Close overrides the embedded eth.Request's Close to also close the wit.Request
// and signal cancellation via the embedded request's cancel channel.
func (r *ethWitRequest) Close() error {
	// Signal cancellation on the embedded request's channel first.
	// Note: This assumes r.Request and r.Request.cancel are non-nil.
	// The channel is initialized in RequestWitnesses.
	close(r.Request.Cancel)

	// Then close the underlying witness request.
	// The eth.Request shim doesn't need explicit closing here,
	// as it's not registered with the eth dispatcher.
	return r.witReq.Close()
}

// RequestWitnesses implements downloader.Peer.
// It requests witnesses using the wit protocol for the given block hashes.
func (p *ethPeer) RequestWitnesses(hashes []common.Hash, dlResCh chan *eth.Response) (*eth.Request, error) {
	p.witPeer.Log().Trace("RequestWitnesses called", "peer", p.ID(), "hashes", len(hashes))
	// Create a channel for the underlying witness protocol responses.
	witResCh := make(chan *wit.Response)

	// Make the actual request via the witness protocol peer.
	witReq, err := p.witPeer.Peer.RequestWitness(hashes, witResCh)
	if err != nil {
		p.witPeer.Log().Error("RequestWitnesses failed to make wit request", "peer", p.ID(), "err", err)
		return nil, err
	}
	p.witPeer.Log().Trace("RequestWitnesses made wit request", "peer", p.ID(), "hashes", len(hashes))

	// Create the wrapper request. Embed a minimal eth.Request shim.
	// Its primary purpose is type compatibility for the return value.
	// The ethWitRequest's Close method handles actual cancellation via witReq.
	// *** Crucially, set the Peer field so the concurrent fetcher can find the peer ***
	ethReqShim := &eth.Request{
		Peer:   p.ID(),              // Set the Peer ID here
		Cancel: make(chan struct{}), // Initialize the cancel channel
	}
	wrapperReq := &ethWitRequest{
		Request: ethReqShim,
		witReq:  witReq,
	}

	// Start a goroutine to adapt responses from the wit channel to the eth channel.
	go func() {
		p.witPeer.Log().Trace("RequestWitnesses adapter goroutine started", "peer", p.ID())
		defer p.witPeer.Log().Trace("RequestWitnesses adapter goroutine finished", "peer", p.ID())

		for witRes := range witResCh {
			p.witPeer.Log().Trace("RequestWitnesses adapter received wit response", "peer", p.ID())
			// Adapt wit.Response to eth.Response.
			// We can only copy exported fields. The unexported fields (id, recv, code)
			// will have zero values in the ethRes sent to the caller.
			// Correlation still works via the Req field.
			ethRes := &eth.Response{
				Req:  wrapperReq.Request, // Point back to the embedded shim request.
				Res:  witRes.Res,
				Meta: witRes.Meta,
				Time: witRes.Time,
				Done: witRes.Done,
			}

			// Forward the adapted response to the downloader's channel,
			// or stop if the request has been cancelled.
			select {
			case dlResCh <- ethRes:
				p.witPeer.Log().Trace("RequestWitnesses adapter forwarded eth response", "peer", p.ID())
			case <-wrapperReq.Request.Cancel:
				p.witPeer.Log().Trace("RequestWitnesses adapter cancelled before forwarding response", "peer", p.ID())
				// If cancelled, exit the goroutine. The closing of witResCh
				// will also eventually terminate the loop, but returning
				// here ensures we don't block trying to send after cancellation.
				return
			}
		}
	}()

	// Return the embedded *eth.Request part of the wrapper.
	// This satisfies the function signature.
	p.witPeer.Log().Trace("RequestWitnesses returning ethReq shim", "peer", p.ID())
	return wrapperReq.Request, nil
}
