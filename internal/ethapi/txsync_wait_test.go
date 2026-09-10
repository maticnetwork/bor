package ethapi

import (
	"context"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

func newTxSyncBackend(t *testing.T) *testBackend {
	t.Helper()

	genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}

	return newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
}

func TestSendRawTransactionSyncWaiterCap(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	b.syncMaxConcurrent = 1

	api := NewTransactionAPI(b, new(AddrLocker))
	raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

	parked := make(chan struct{})

	go func() {
		defer close(parked)

		timeout := hexutil.Uint64(800)
		_, _ = api.SendRawTransactionSync(context.Background(), raw, &timeout)
	}()

	waitForTxSyncWaiters(t, api, 1)

	timeout := hexutil.Uint64(100)

	receipt, err := api.SendRawTransactionSync(context.Background(), raw, &timeout)
	if receipt != nil {
		t.Fatalf("receipt = %#v, want nil", receipt)
	}
	if !errors.Is(err, errTxSyncBusy) {
		t.Fatalf("err = %v, want %v", err, errTxSyncBusy)
	}

	<-parked

	// The seat is handed back, so the next caller waits rather than being refused.
	if _, err := api.SendRawTransactionSync(context.Background(), raw, &timeout); errors.Is(err, errTxSyncBusy) {
		t.Fatal("still refusing callers after the waiter left")
	}
}

func TestSendRawTransactionSyncUncapped(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	b.syncMaxConcurrent = 0

	api := NewTransactionAPI(b, new(AddrLocker))
	if api.txSyncWaiters != nil {
		t.Fatal("waiter admission built for an uncapped node")
	}

	if !api.enterTxSyncWait() {
		t.Fatal("uncapped node refused a waiter")
	}

	api.leaveTxSyncWait()
}

func TestPollReceipt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		preconf        bool
		canonical      bool
		checkCanonical bool
		wantReceipt    bool
		wantPreconf    bool
		wantLookup     bool
	}{
		{name: "nothing landed"},
		{name: "preconf only", preconf: true, wantReceipt: true, wantPreconf: true},
		{name: "preconf prefers canonical", preconf: true, canonical: true, wantReceipt: true, wantLookup: true},
		{name: "canonical is not read on a preconf tick", canonical: true},
		{name: "canonical is read on the backstop tick", canonical: true, checkCanonical: true, wantReceipt: true, wantLookup: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := newTxSyncBackend(t)
			b.canonicalLookups = new(atomic.Int64)
			api := NewTransactionAPI(b, new(AddrLocker))
			_, tx := makeSelfSignedRaw(t, api, b.acc.Address)

			if tt.canonical {
				b.autoMine = true
				b.sentTx, b.sentTxHash = tx, tx.Hash()
			}

			if tt.preconf {
				b.preconfEnabled = true
				blockNumber := new(big.Int).Add(b.CurrentBlock().Number, common.Big1)
				if tt.canonical {
					blockNumber.Set(b.CurrentBlock().Number)
				}
				b.preconf.tx = tx
				b.preconf.receipt = &types.Receipt{
					Type:              tx.Type(),
					Status:            types.ReceiptStatusSuccessful,
					CumulativeGasUsed: tx.Gas(),
					GasUsed:           tx.Gas(),
					EffectiveGasPrice: tx.GasPrice(),
					TxHash:            tx.Hash(),
					BlockNumber:       blockNumber,
				}
			}

			receipt := api.pollReceipt(context.Background(), tx.Hash(), tt.checkCanonical)

			if (receipt != nil) != tt.wantReceipt {
				t.Fatalf("receipt = %#v, want receipt: %v", receipt, tt.wantReceipt)
			}
			if receipt == nil {
				return
			}
			if got := receipt["preconfirmation"] == true; got != tt.wantPreconf {
				t.Fatalf("preconfirmation = %v, want %v (receipt %#v)", got, tt.wantPreconf, receipt)
			}
			if got := b.canonicalLookups.Load() > 0; got != tt.wantLookup {
				t.Fatalf("canonical lookup = %v, want %v", got, tt.wantLookup)
			}
		})
	}
}

func waitForTxSyncWaiters(t *testing.T, api *TransactionAPI, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(api.txSyncWaiters) == want {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("waiters = %d, want %d", len(api.txSyncWaiters), want)
}

func TestTxSyncTimeout(t *testing.T) {
	t.Parallel()

	ms := func(v uint64) *hexutil.Uint64 {
		h := hexutil.Uint64(v)

		return &h
	}

	tests := []struct {
		name    string
		request *hexutil.Uint64
		want    time.Duration
	}{
		{name: "unset falls back to default", request: nil, want: 2 * time.Second},
		{name: "zero falls back to default", request: ms(0), want: 2 * time.Second},
		{name: "under the ceiling is honoured", request: ms(1500), want: 1500 * time.Millisecond},
		{name: "at the ceiling", request: ms(300_000), want: 5 * time.Minute},
		{name: "over the ceiling is clamped", request: ms(600_000), want: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := NewTransactionAPI(newTxSyncBackend(t), new(AddrLocker))
			if got := api.txSyncTimeout(tt.request); got != tt.want {
				t.Fatalf("timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTxSyncWaitClosed(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0xabc")

	t.Run("deadline names the pooled transaction", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()

		<-ctx.Done()

		err := txSyncWaitClosed(ctx, hash, 3*time.Second)

		var timeoutErr *txSyncTimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("err = %T %v, want *txSyncTimeoutError", err, err)
		}
		if timeoutErr.ErrorCode() != errCodeTxSyncTimeout {
			t.Fatalf("code = %d, want %d", timeoutErr.ErrorCode(), errCodeTxSyncTimeout)
		}
		if timeoutErr.ErrorData() != hash.Hex() {
			t.Fatalf("data = %v, want %s", timeoutErr.ErrorData(), hash.Hex())
		}
		if !strings.Contains(timeoutErr.Error(), "3s") {
			t.Fatalf("message %q does not name the wait window", timeoutErr.Error())
		}
	})

	t.Run("caller cancellation is passed through", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := txSyncWaitClosed(ctx, hash, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
	})
}

func TestSubscriptionFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("feed broke")

	tests := []struct {
		name string
		err  error
		open bool
		want error
	}{
		{name: "closed channel", err: nil, open: false, want: errSubClosed},
		{name: "live error", err: sentinel, open: true, want: sentinel},
		{name: "live without error", err: nil, open: true, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := subscriptionFailure(tt.err, tt.open); !errors.Is(got, tt.want) {
				t.Fatalf("err = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTxSyncReceiptHubCanonical(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	api := NewTransactionAPI(b, new(AddrLocker))
	_, tx := makeSelfSignedRaw(t, api, b.acc.Address)
	blockHash := common.HexToHash("0xfeed")
	receipt := &types.Receipt{
		Type:              tx.Type(),
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 21000,
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(1),
		TxHash:            tx.Hash(),
		BlockHash:         blockHash,
		BlockNumber:       big.NewInt(7),
		TransactionIndex:  0,
	}
	first, unregisterFirst := api.txSyncReceipts.register(tx.Hash())
	defer unregisterFirst()
	second, unregisterSecond := api.txSyncReceipts.register(tx.Hash())
	defer unregisterSecond()
	b.chainFeed.Send(core.ChainEvent{
		Receipts:     types.Receipts{receipt},
		Transactions: types.Transactions{tx},
	})

	for _, updates := range []<-chan txSyncReceiptResult{first, second} {
		select {
		case result := <-updates:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if got := result.receipt["blockHash"]; got != blockHash {
				t.Fatalf("blockHash = %v, want %v", got, blockHash)
			}
			if got := result.receipt["transactionHash"]; got != tx.Hash() {
				t.Fatalf("transactionHash = %v, want %v", got, tx.Hash())
			}
		case <-time.After(time.Second):
			t.Fatal("canonical receipt was not dispatched")
		}
	}
}

func TestTxSyncReceiptHubSharesSubscription(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	api := NewTransactionAPI(b, new(AddrLocker))
	unregister := make([]func(), 100)
	for index := range unregister {
		_, unregister[index] = api.txSyncReceipts.register(common.BigToHash(big.NewInt(int64(index + 1))))
	}
	if got := b.chainSubscriptions.Load(); got != 1 {
		t.Fatalf("chain subscriptions = %d, want 1", got)
	}
	for _, stop := range unregister {
		stop()
	}
}

func TestTxSyncReceiptHubUsesPublishedReceiptSnapshot(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	api := NewTransactionAPI(b, new(AddrLocker))
	_, tx := makeSelfSignedRaw(t, api, b.acc.Address)
	receipt := &types.Receipt{
		Type:              tx.Type(),
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 21000,
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(1),
		TxHash:            tx.Hash(),
		BlockNumber:       big.NewInt(7),
	}
	updates, unregister := api.txSyncReceipts.register(tx.Hash())
	defer unregister()

	api.txSyncReceipts.preconfirmed(core.PreconfReceiptsEvent{
		BlockTime:    10,
		Receipts:     types.Receipts{receipt},
		Transactions: types.Transactions{tx},
	})

	select {
	case result := <-updates:
		if result.err != nil || result.receipt["preconfirmation"] != true || result.receipt["blockHash"] != nil {
			t.Fatalf("receipt result = %#v, err = %v", result.receipt, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("published receipt snapshot was not dispatched")
	}
}

func TestDecodeRawTransaction(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	api := NewTransactionAPI(b, new(AddrLocker))
	raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

	t.Run("round trips a signed transaction", func(t *testing.T) {
		t.Parallel()

		got, err := api.decodeRawTransaction(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Hash() != tx.Hash() {
			t.Fatalf("hash = %s, want %s", got.Hash(), tx.Hash())
		}
	})

	t.Run("rejects garbage", func(t *testing.T) {
		t.Parallel()

		if _, err := api.decodeRawTransaction(hexutil.Bytes{0x01, 0x02, 0x03}); err == nil {
			t.Fatal("expected a decode error")
		}
	})
}

func TestSendRawTransactionSyncBusyDoesNotSubmit(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	b.syncMaxConcurrent = 1

	api := NewTransactionAPI(b, new(AddrLocker))
	raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

	if !api.enterTxSyncWait() {
		t.Fatal("could not take the only seat")
	}

	if _, err := api.SendRawTransactionSync(context.Background(), raw, nil); !errors.Is(err, errTxSyncBusy) {
		t.Fatalf("err = %v, want %v", err, errTxSyncBusy)
	}
	if b.sentTx != nil {
		t.Fatalf("refused call still submitted %s", b.sentTx.Hash())
	}
}

// TestParkedSyncCallDoesNotBlockTheServer drives the real handler over HTTP
// against a single-slot execution pool. The parked sync call owns that slot
// when it enters the wait, so a second call can only get through if the wait
// hands the slot back.
func TestParkedSyncCallDoesNotBlockTheServer(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	b.autoMine = false // nothing ever mines, so the sync call waits out its window
	b.syncMaxConcurrent = 8

	api := NewTransactionAPI(b, new(AddrLocker))
	raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

	server := rpc.NewServer("test", 1, 0)
	defer server.Stop()

	if err := server.RegisterName("eth", api); err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	post := func(body string, timeout time.Duration) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, strings.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		out, err := io.ReadAll(resp.Body)

		return string(out), err
	}

	go func() {
		_, _ = post(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransactionSync","params":["`+raw.String()+`","0x1388"]}`, 30*time.Second)
	}()

	// A registered waiter means the call is past admission and holding the
	// pool's only slot.
	waitForTxSyncWaiters(t, api, 1)

	got, err := post(`{"jsonrpc":"2.0","id":2,"method":"eth_getTransactionCount","params":["`+b.acc.Address.Hex()+`","pending"]}`, 2*time.Second)
	if err != nil {
		t.Fatalf("second call blocked behind the parked sync call: %v", err)
	}
	if !strings.Contains(got, `"result"`) {
		t.Fatalf("unexpected response: %s", got)
	}
}

func TestSendRawTransactionRejectsGarbage(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	api := NewTransactionAPI(b, new(AddrLocker))
	garbage := hexutil.Bytes{0x01, 0x02, 0x03}

	t.Run("async", func(t *testing.T) {
		t.Parallel()

		if _, err := api.SendRawTransaction(context.Background(), garbage); err == nil {
			t.Fatal("expected a decode error")
		}
	})

	t.Run("sync", func(t *testing.T) {
		t.Parallel()

		if _, err := api.SendRawTransactionSync(context.Background(), garbage, nil); err == nil {
			t.Fatal("expected a decode error")
		}
	})

	if b.sentTx != nil {
		t.Fatalf("undecodable input reached the pool as %s", b.sentTx.Hash())
	}
}

func TestPendingReceiptLookupSkipsCanonicalLookup(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	b.canonicalLookups = new(atomic.Int64)

	api := NewTransactionAPI(b, new(AddrLocker))
	_, tx := makeSelfSignedRaw(t, api, b.acc.Address)
	b.preconf.tx = tx
	b.preconf.receipt = &types.Receipt{
		TxHash:      tx.Hash(),
		BlockNumber: new(big.Int).Add(b.CurrentBlock().Number, common.Big1),
	}

	if got := api.pollReceipt(context.Background(), tx.Hash(), false); got == nil || got["preconfirmation"] != true {
		t.Fatalf("receipt = %#v, want preconfirmation", got)
	}
	if got := b.canonicalLookups.Load(); got != 0 {
		t.Fatalf("preconf tick made %d canonical lookups, want 0", got)
	}

	b.preconf.tx = nil
	b.preconf.receipt = nil
	if got := api.pollReceipt(context.Background(), tx.Hash(), true); got != nil {
		t.Fatalf("receipt = %#v, want nil", got)
	}
	if got := b.canonicalLookups.Load(); got == 0 {
		t.Fatal("backstop tick made no canonical lookup")
	}
}

func TestPollReceiptSkipsCanonicalLookupForPooledTransaction(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		status txpool.TxStatus
	}{
		{name: "pending", status: txpool.TxStatusPending},
		{name: "queued", status: txpool.TxStatusQueued},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := newTxSyncBackend(t)
			b.canonicalLookups = new(atomic.Int64)
			b.txStatusFn = func(common.Hash) txpool.TxStatus { return tt.status }

			api := NewTransactionAPI(b, new(AddrLocker))
			_, tx := makeSelfSignedRaw(t, api, b.acc.Address)

			if got := api.pollReceipt(context.Background(), tx.Hash(), true); got != nil {
				t.Fatalf("receipt = %#v, want nil", got)
			}
			if got := b.canonicalLookups.Load(); got != 0 {
				t.Fatalf("pooled transaction made %d canonical lookups, want 0", got)
			}
		})
	}
}

func TestWaitLoopDoesNotPollPerRequest(t *testing.T) {
	t.Parallel()

	b := newTxSyncBackend(t)
	b.autoMine = false
	b.preconfEnabled = true
	b.canonicalLookups = new(atomic.Int64)

	api := NewTransactionAPI(b, new(AddrLocker))
	raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

	timeout := hexutil.Uint64(700)

	if _, err := api.SendRawTransactionSync(context.Background(), raw, &timeout); err == nil {
		t.Fatal("expected the wait window to elapse")
	}

	if got := b.canonicalLookups.Load(); got != 0 {
		t.Fatalf("%d canonical lookups over a 700ms wait, want 0", got)
	}
}
