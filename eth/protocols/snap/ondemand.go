// Copyright 2024 The go-ethereum Authors
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

package snap

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

// onDemandReqidBit marks a request id as belonging to an out-of-band, targeted
// bytecode fetch (FetchByteCodes) rather than the bulk sync loop. Loop reqids
// are uint64(rand.Int63()) and so always have the top bit clear; setting it
// guarantees the two id spaces never collide, so OnByteCodes can route a
// response to the right place unambiguously.
const onDemandReqidBit = uint64(1) << 63

// onDemandCodeFetchTimeout bounds how long a single peer is given to answer a
// targeted bytecode request before the next peer is tried.
const onDemandCodeFetchTimeout = 15 * time.Second

// onDemandCodeReq tracks one in-flight targeted bytecode request.
type onDemandCodeReq struct {
	deliver chan [][]byte
}

// FetchByteCodes retrieves the given contract bytecodes from a connected snap
// peer on demand — outside the bulk sync state machine — verifying each blob
// against its hash before returning it. It backs the stateless self-heal path:
// when witness-verified execution references a contract whose code is absent
// from local disk (WIT2 witnesses carry no code), the node fetches exactly that
// content-addressed blob and re-persists it, instead of stalling.
//
// It returns the subset of hashes it could fetch and verify, keyed by hash; a
// missing entry means no connected peer served it. Peers are tried in turn
// until every hash is found, the peer set is exhausted, or ctx is cancelled.
// Peers are registered by lifecycle (Register/Unregister) independently of a
// running Sync cycle, so this works while the syncer is otherwise idle.
func (s *Syncer) FetchByteCodes(ctx context.Context, hashes []common.Hash) (map[common.Hash][]byte, error) {
	out := make(map[common.Hash][]byte, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	tried := make(map[string]struct{})
	for {
		// Reduce to the hashes still outstanding.
		var pending []common.Hash
		for _, h := range hashes {
			if _, ok := out[h]; !ok {
				pending = append(pending, h)
			}
		}
		if len(pending) == 0 {
			return out, nil
		}
		if len(pending) > maxCodeRequestCount {
			pending = pending[:maxCodeRequestCount]
		}

		// Pick a peer we have not tried yet and that has not already declined to
		// serve state.
		s.lock.Lock()
		var peer SyncPeer
		for id, p := range s.peers {
			if _, done := tried[id]; done {
				continue
			}
			if _, bad := s.statelessPeers[id]; bad {
				continue
			}
			peer, tried[id] = p, struct{}{}
			break
		}
		if peer == nil {
			s.lock.Unlock()
			if len(out) > 0 {
				return out, nil
			}
			return out, fmt.Errorf("snap: no peer available to serve %d bytecode(s) on demand", len(pending))
		}
		reqid := uint64(rand.Int63()) | onDemandReqidBit
		req := &onDemandCodeReq{deliver: make(chan [][]byte, 1)}
		s.onDemandCodeReqs[reqid] = req
		s.lock.Unlock()

		if err := peer.RequestByteCodes(reqid, pending, maxRequestSize); err != nil {
			s.forgetOnDemandCodeReq(reqid)
			log.Debug("On-demand bytecode request failed to send", "peer", peer.ID(), "err", err)
			continue
		}

		select {
		case codes := <-req.deliver:
			s.forgetOnDemandCodeReq(reqid)
			verifyAndCollectByteCodes(pending, codes, out)
		case <-time.After(onDemandCodeFetchTimeout):
			s.forgetOnDemandCodeReq(reqid)
			log.Debug("On-demand bytecode request timed out", "peer", peer.ID(), "hashes", len(pending))
		case <-ctx.Done():
			s.forgetOnDemandCodeReq(reqid)
			if len(out) > 0 {
				return out, nil
			}
			return out, ctx.Err()
		}
	}
}

// onDemandByteCodes delivers a targeted bytecode response (top-bit reqid) to its
// waiting FetchByteCodes caller. Stale, duplicate or unrequested responses are
// dropped harmlessly.
func (s *Syncer) onDemandByteCodes(id uint64, bytecodes [][]byte) error {
	s.lock.Lock()
	req, ok := s.onDemandCodeReqs[id]
	if ok {
		delete(s.onDemandCodeReqs, id)
	}
	s.lock.Unlock()
	if !ok {
		return nil
	}
	select {
	case req.deliver <- bytecodes:
	default:
	}
	return nil
}

func (s *Syncer) forgetOnDemandCodeReq(reqid uint64) {
	s.lock.Lock()
	delete(s.onDemandCodeReqs, reqid)
	s.lock.Unlock()
}

// verifyAndCollectByteCodes hashes each delivered blob and, for any that matches
// a still-wanted hash, records it into out. Unmatched or corrupt blobs are
// ignored — a peer that returns garbage simply does not count as having served
// that hash, and the caller moves on to the next peer. Because entry is keyed by
// the verified keccak of the blob, a peer cannot inject code for the wrong hash.
func verifyAndCollectByteCodes(wanted []common.Hash, delivered [][]byte, out map[common.Hash][]byte) {
	hasher := crypto.NewKeccakState()
	var h common.Hash
	for _, blob := range delivered {
		if len(blob) == 0 {
			continue
		}
		hasher.Reset()
		hasher.Write(blob)
		hasher.Read(h[:])
		for _, w := range wanted {
			if h == w {
				if _, ok := out[w]; !ok {
					out[w] = blob
				}
				break
			}
		}
	}
}
