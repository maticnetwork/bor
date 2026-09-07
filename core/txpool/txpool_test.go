package txpool

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

type speculativeTestSubPool struct {
	SubPool
	head  *types.Header
	state *state.StateDB
}

func (p *speculativeTestSubPool) SetSpeculativeState(head *types.Header, statedb *state.StateDB) {
	p.head = head
	p.state = statedb
}

type plainTestSubPool struct {
	SubPool
}

// TestSubscribeRebroadcastTransactionsNilPool tests that calling
// SubscribeRebroadcastTransactions on a nil TxPool returns a valid no-op
// subscription.
func TestSubscribeRebroadcastTransactionsNilPool(t *testing.T) {
	var pool *TxPool // nil pool

	ch := make(chan core.StuckTxsEvent, 1)
	sub := pool.SubscribeRebroadcastTransactions(ch)

	// Verify the subscription is valid even for nil pool
	if sub == nil {
		t.Fatal("expected non-nil subscription")
	}

	// Unsubscribe should work without issues
	sub.Unsubscribe()

	// Channel should be empty (no events should be sent)
	select {
	case event := <-ch:
		t.Fatalf("unexpected event: %v", event)
	default:
		// Expected - no events
	}
}

func TestSetSpeculativeState(t *testing.T) {
	setter := new(speculativeTestSubPool)
	plain := new(plainTestSubPool)
	pool := &TxPool{subpools: []SubPool{setter, plain}}
	header := &types.Header{Number: big.NewInt(42)}
	statedb := new(state.StateDB)

	pool.SetSpeculativeState(header, statedb)

	pool.stateLock.RLock()
	aggregatedState := pool.state
	pool.stateLock.RUnlock()
	if aggregatedState != statedb {
		t.Fatal("aggregator did not retain speculative state")
	}
	if setter.head != header || setter.state != statedb {
		t.Fatal("speculative state was not forwarded to supporting subpool")
	}
}
