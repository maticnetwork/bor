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

package eth

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestTriggerTxGossipToPeers(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWithBlocks(1)
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	tx, err := api.syntheticTransaction()
	if err != nil {
		t.Fatalf("failed to build synthetic tx: %v", err)
	}
	peerA := &fakeTxGossipPeer{id: "a"}
	peerB := &fakeTxGossipPeer{id: "b"}

	if err := triggerTxGossipToPeers([]txGossipPeer{peerA, peerB}, tx); err != nil {
		t.Fatalf("failed to trigger tx gossip: %v", err)
	}
	for _, peer := range []*fakeTxGossipPeer{peerA, peerB} {
		if len(peer.txs) != 1 {
			t.Fatalf("peer %s received %d tx batches, want 1", peer.id, len(peer.txs))
		}
		if len(peer.txs[0]) != 1 {
			t.Fatalf("peer %s received %d txs, want 1", peer.id, len(peer.txs[0]))
		}
		if peer.txs[0][0].Hash() != tx.Hash() {
			t.Fatalf("peer %s received tx %s, want %s", peer.id, peer.txs[0][0].Hash(), tx.Hash())
		}
	}
}

func TestTriggerTxGossipToPeersPropagatesError(t *testing.T) {
	t.Parallel()

	handler := newTestHandlerWithBlocks(1)
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	tx, err := api.syntheticTransaction()
	if err != nil {
		t.Fatalf("failed to build synthetic tx: %v", err)
	}
	wantErr := errors.New("boom")
	if err := triggerTxGossipToPeers([]txGossipPeer{&fakeTxGossipPeer{id: "broken", err: wantErr}}, tx); !errors.Is(err, wantErr) {
		t.Fatalf("unexpected error: got %v want %v", err, wantErr)
	}
}

func TestTriggerBlockAnnouncementToPeers(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0x1234")
	number := uint64(99)
	peerA := &fakeBlockAnnouncementPeer{id: "a"}
	peerB := &fakeBlockAnnouncementPeer{id: "b"}

	if err := triggerBlockAnnouncementToPeers([]blockAnnouncementPeer{peerA, peerB}, hash, number); err != nil {
		t.Fatalf("failed to trigger block announcement: %v", err)
	}
	for _, peer := range []*fakeBlockAnnouncementPeer{peerA, peerB} {
		if len(peer.hashes) != 1 {
			t.Fatalf("peer %s received %d announcement batches, want 1", peer.id, len(peer.hashes))
		}
		if len(peer.hashes[0]) != 1 {
			t.Fatalf("peer %s received %d hashes, want 1", peer.id, len(peer.hashes[0]))
		}
		if peer.hashes[0][0] != hash {
			t.Fatalf("peer %s received hash %s, want %s", peer.id, peer.hashes[0][0], hash)
		}
		if len(peer.numbers) != 1 || len(peer.numbers[0]) != 1 || peer.numbers[0][0] != number {
			t.Fatalf("peer %s received unexpected block number payload", peer.id)
		}
	}
}

func TestAdminAPITargetEthPeersRequiresConnectedPeer(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	defer handler.close()

	api := NewAdminAPI(&Ethereum{blockchain: handler.chain, handler: handler.handler})
	if _, err := api.TriggerTxGossip(nil); err == nil {
		t.Fatal("expected missing peer error")
	}
}

type fakeTxGossipPeer struct {
	id  string
	txs []types.Transactions
	err error
}

func (p *fakeTxGossipPeer) ID() string { return p.id }

func (p *fakeTxGossipPeer) SendTransactions(txs types.Transactions) error {
	if p.err != nil {
		return p.err
	}
	p.txs = append(p.txs, txs)
	return nil
}

type fakeBlockAnnouncementPeer struct {
	id      string
	hashes  [][]common.Hash
	numbers [][]uint64
	err     error
}

func (p *fakeBlockAnnouncementPeer) ID() string { return p.id }

func (p *fakeBlockAnnouncementPeer) SendNewBlockHashes(hashes []common.Hash, numbers []uint64) error {
	if p.err != nil {
		return p.err
	}
	p.hashes = append(p.hashes, append([]common.Hash(nil), hashes...))
	p.numbers = append(p.numbers, append([]uint64(nil), numbers...))
	return nil
}
