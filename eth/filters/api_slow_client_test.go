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

package filters

import (
	"context"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// TestSlowClientDoesNotStarveOtherSubscribers reproduces a production failure:
// a WebSocket client that subscribes to newPendingTransactions and then stops
// reading its connection must not affect delivery to other subscribers. The
// EventSystem fans events out to all subscriptions from one shared goroutine
// with blocking sends, so if client delivery back-pressures into the
// subscription channel, one stalled client freezes every subscription on the
// node until the write deadline fires.
func TestSlowClientDoesNotStarveOtherSubscribers(t *testing.T) {
	t.Parallel()

	var (
		db           = rawdb.NewMemoryDatabase()
		backend, sys = newTestFilterSystem(db, Config{})
		api          = NewFilterAPI(sys, false)
	)

	server := rpc.NewServer("", 0, 0)
	defer server.Stop()

	if err := server.RegisterName("eth", api); err != nil {
		t.Fatal(err)
	}

	// Stalled client: subscribe over a raw pipe, read the subscription reply,
	// then never read again. Notification writes to this connection block
	// until the server's write deadline.
	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	defer cliConn.Close()

	go server.ServeCodec(rpc.NewCodec(srvConn), 0)

	if err := cliConn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := cliConn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions"]}` + "\n")); err != nil {
		t.Fatal(err)
	}

	reply := make([]byte, 512)
	if _, err := cliConn.Read(reply); err != nil {
		t.Fatalf("stalled client never got subscription reply: %v", err)
	}

	// Healthy client on its own connection.
	client := rpc.DialInProc(server)
	defer client.Close()

	const events = 200

	healthy := make(chan common.Hash, events)

	sub, err := client.EthSubscribe(context.Background(), healthy, "newPendingTransactions")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	// EthSubscribe returns once the server has assigned a subscription ID, which
	// happens before the handler goroutine installs the subscription in the
	// EventSystem. Events sent in that window are dropped by the feed and would
	// make the exact-count assertion below fail for the wrong reason, so send
	// warm-up events until one is delivered, then discard what they produced.
	awaitInstalled(t, backend, healthy)

	for i := 0; i < events; i++ {
		tx := types.NewTransaction(uint64(i), common.HexToAddress("0xb794f5ea0ba39494ce83a213fffba74279579268"), new(big.Int), 0, new(big.Int), nil)
		backend.txFeed.Send(core.NewTxsEvent{Txs: []*types.Transaction{tx}})
	}

	// The healthy subscriber must receive every event promptly even though the
	// stalled client stopped reading. Keep the deadline well under the RPC
	// write timeout so recovery-by-disconnect cannot mask starvation.
	received := 0
	timeout := time.After(5 * time.Second)

	for received < events {
		select {
		case <-healthy:
			received++
		case err := <-sub.Err():
			t.Fatalf("healthy subscription failed after %d events: %v", received, err)
		case <-timeout:
			t.Fatalf("healthy subscriber starved by stalled client: got %d of %d events", received, events)
		}
	}
}

// awaitInstalled blocks until the subscription feeding delivered is installed in
// the EventSystem, then drains the events the probing produced.
func awaitInstalled(t *testing.T, backend *testBackend, delivered <-chan common.Hash) {
	t.Helper()

	tx := types.NewTransaction(0, common.HexToAddress("0xb794f5ea0ba39494ce83a213fffba74279579268"), new(big.Int), 0, new(big.Int), nil)
	deadline := time.After(5 * time.Second)

	for {
		backend.txFeed.Send(core.NewTxsEvent{Txs: []*types.Transaction{tx}})

		select {
		case <-delivered:
			// Drain anything the earlier probes delivered so the caller starts
			// from an empty channel.
			for {
				select {
				case <-delivered:
				default:
					return
				}
			}
		case <-deadline:
			t.Fatal("subscription was never installed in the EventSystem")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
