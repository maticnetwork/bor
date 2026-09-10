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

package downloader

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
)

type unfillableSkeletonPeer struct {
	*downloadTesterPeer
	head *types.Header
	td   *big.Int
}

func deliverSkeletonHeaders(id string, headers []*types.Header, sink chan *eth.Response) (*eth.Request, error) {
	hashes := make([]common.Hash, len(headers))
	for i, header := range headers {
		hashes[i] = header.Hash()
	}
	req := &eth.Request{Peer: id}
	res := &eth.Response{
		Req:  req,
		Res:  (*eth.BlockHeadersRequest)(&headers),
		Meta: hashes,
		Time: 1,
		Done: make(chan error, 1),
	}
	go func() { sink <- res }()
	return req, nil
}

func (p *unfillableSkeletonPeer) Head() (common.Hash, *big.Int) {
	return p.head.Hash(), new(big.Int).Set(p.td)
}

func (p *unfillableSkeletonPeer) RequestHeadersByHash(origin common.Hash, amount int, skip int, reverse bool, sink chan *eth.Response) (*eth.Request, error) {
	if origin == p.head.Hash() && amount == 1 {
		return deliverSkeletonHeaders(p.id, []*types.Header{p.head}, sink)
	}
	return p.downloadTesterPeer.RequestHeadersByHash(origin, amount, skip, reverse, sink)
}

func (p *unfillableSkeletonPeer) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool, sink chan *eth.Response) (*eth.Request, error) {
	if origin == p.head.Number.Uint64() && amount == MaxSkeletonSize && skip == MaxHeaderFetch-1 && !reverse {
		return deliverSkeletonHeaders(p.id, []*types.Header{p.head}, sink)
	}
	if amount == MaxHeaderFetch && skip == 0 && !reverse {
		return deliverSkeletonHeaders(p.id, nil, sink)
	}
	return p.downloadTesterPeer.RequestHeadersByNumber(origin, amount, skip, reverse, sink)
}

func TestUnfillableSkeletonIsAttributedToOrigin(t *testing.T) {
	tester := newTester(t)
	defer tester.terminate()

	base := tester.newPeer("origin", eth.ETH69, testChainBase.blocks[1:])
	head := &types.Header{
		ParentHash: common.HexToHash("0xdead"),
		Number:     new(big.Int).SetUint64(uint64(MaxHeaderFetch)),
		Difficulty: big.NewInt(1),
		GasLimit:   1,
		Time:       1,
		Extra:      []byte{1, 2, 3},
	}
	origin := &unfillableSkeletonPeer{
		downloadTesterPeer: base,
		head:               head,
		td:                 new(big.Int).Lsh(big.NewInt(1), 99),
	}
	if err := tester.downloader.UnregisterPeer(base.id); err != nil {
		t.Fatal(err)
	}
	if err := tester.downloader.RegisterPeer(base.id, eth.ETH69, origin); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		tester.newPeer(fmt.Sprintf("honest-%d", i), eth.ETH69, testChainBase.blocks[1:])
	}

	err := tester.downloader.LegacySync(base.id, head.Hash(), origin.td, nil, FullSync)
	if !errors.Is(err, errInvalidChain) {
		t.Fatalf("have %v, want wrapping %v", err, errInvalidChain)
	}
	if errors.Is(err, ErrPeersUnavailable) {
		t.Fatalf("skeleton origin failure still wraps %v", ErrPeersUnavailable)
	}
	if tester.downloader.peers.Peer(base.id) != nil {
		t.Fatal("skeleton origin was retained")
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("honest-%d", i)
		if tester.downloader.peers.Peer(id) == nil {
			t.Fatalf("honest peer %q was removed", id)
		}
	}
}
