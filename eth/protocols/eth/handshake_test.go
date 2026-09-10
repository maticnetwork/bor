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
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// Tests that handshake failures are detected and reported correctly.
func TestHandshake70(t *testing.T) { testHandshake(t, ETH70) }
func TestHandshake69(t *testing.T) { testHandshake(t, ETH69) }
func TestHandshake68(t *testing.T) { testHandshake(t, ETH68) }

func testHandshake(t *testing.T, protocol uint) {
	t.Parallel()

	// Create a test backend only to have some valid genesis chain
	backend := newTestBackend(3)
	defer backend.close()

	genesis := backend.chain.Genesis()
	head := backend.chain.CurrentBlock()
	forkID := forkid.NewID(
		backend.chain.Config(),
		backend.chain.Genesis(),
		backend.chain.CurrentHeader().Number.Uint64(),
		backend.chain.CurrentHeader().Time,
	)

	// Helper to build the protocol-specific Status packet
	makeStatus := func(ver uint32, network uint64, td *big.Int, head, genesis common.Hash, fid forkid.ID) interface{} {
		switch protocol {
		case ETH68:
			return StatusPacket68{ver, network, td, head, genesis, fid}
		case ETH69, ETH70:
			// eth/70 leaves the status message untouched.
			return StatusPacket69{ver, network, td, genesis, fid, 1, 2, head}
		default:
			t.Fatalf("unsupported protocol version: %d", protocol)
			return nil
		}
	}

	tests := []struct {
		code uint64
		data interface{}
		want error
	}{
		{
			code: TransactionsMsg, data: []interface{}{},
			want: errNoStatusMsg,
		},
		{
			code: StatusMsg,
			data: makeStatus(10, 1, new(big.Int), head.Hash(), genesis.Hash(), forkID),
			want: errProtocolVersionMismatch,
		},
		{
			code: StatusMsg,
			data: makeStatus(uint32(protocol), 999, new(big.Int), head.Hash(), genesis.Hash(), forkID),
			want: errNetworkIDMismatch,
		},
		{
			code: StatusMsg,
			data: makeStatus(uint32(protocol), 1, new(big.Int), head.Hash(), common.Hash{3}, forkID),
			want: errGenesisMismatch,
		},
		{
			code: StatusMsg,
			data: makeStatus(uint32(protocol), 1, new(big.Int), head.Hash(), genesis.Hash(), forkid.ID{Hash: [4]byte{0x00, 0x01, 0x02, 0x03}}),
			want: errForkIDRejected,
		},
	}

	for i, test := range tests {
		// Create the two peers to shake with each other
		app, net := p2p.MsgPipe()
		defer app.Close()
		defer net.Close()

		peer := NewPeer(protocol, p2p.NewPeer(enode.ID{}, "peer", nil), net, nil, nil)
		defer peer.Close()

		// Send the junk test with one peer, check the handshake failure
		go p2p.Send(app, test.code, test.data)

		err := peer.Handshake(1, backend.chain, BlockRangeUpdatePacket{})
		if err == nil {
			t.Errorf("test %d: protocol returned nil error, want %q", i, test.want)
		} else if !errors.Is(err, test.want) {
			t.Errorf("test %d: wrong error: got %q, want %q", i, err, test.want)
		}
	}
}
