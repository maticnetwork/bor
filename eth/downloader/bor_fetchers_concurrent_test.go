package downloader

import (
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

// mockPeer implements the Peer interface for testing peer version filtering
// in concurrentFetch.
type mockPeer struct {
	id               string
	protocol         uint
	receiptRequested atomic.Bool
}

func (m *mockPeer) Head() (common.Hash, *big.Int) { return common.Hash{}, new(big.Int) }
func (m *mockPeer) RequestHeadersByHash(common.Hash, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (m *mockPeer) RequestHeadersByNumber(uint64, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (m *mockPeer) RequestBodies([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (m *mockPeer) RequestReceipts([]common.Hash, []uint64, []uint64, chan *eth.Response) (*eth.Request, error) {
	m.receiptRequested.Store(true)
	// Return a valid request so concurrentFetch can track it.
	// peer field is nil so Close() is a no-op (test-safe per dispatcher.go).
	return &eth.Request{Peer: m.id, Sent: time.Now()}, nil
}
func (m *mockPeer) RequestWitnesses([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (m *mockPeer) SupportsWitness() bool { return false }

// newTestDownloader creates a minimal Downloader with only the fields needed
// by concurrentFetch, avoiding the full New() constructor.
func newTestDownloader() *Downloader {
	return &Downloader{
		queue:    newQueue(blockCacheMaxItems, blockCacheInitialItems, nil),
		peers:    newPeerSet(),
		cancelCh: make(chan struct{}),
		dropPeer: func(string) {},
	}
}

// scheduleReceiptTask sets up the downloader's queue in SnapSync mode and
// schedules a header with non-empty receipts so concurrentFetch enters the
// receipt peer selection path.
func scheduleReceiptTask(d *Downloader) {
	scheduleReceiptTasks(d, 1)
}

func scheduleReceiptTasks(d *Downloader, count int) {
	d.queue.Prepare(1, SnapSync)

	var (
		headers []*types.Header
		hashes  []common.Hash
	)
	for i := 1; i <= count; i++ {
		header := &types.Header{
			Number:      big.NewInt(int64(i)),
			ReceiptHash: common.Hash{byte(i)},
		}
		headers = append(headers, header)
		hashes = append(hashes, header.Hash())
	}
	d.queue.Schedule(headers, hashes, 1)
}

// TestConcurrentFetchReceipts_OnlyEth68Peers verifies that concurrentFetch
// returns ErrPeersUnavailable when all peers are below eth/69, since only
// eth/69 peers include bor receipts in responses.
func TestConcurrentFetchReceipts_OnlyEth68Peers(t *testing.T) {
	d := newTestDownloader()
	scheduleReceiptTask(d)

	var mockPeers = make([]*mockPeer, 2)
	mockPeers[0] = &mockPeer{id: "peer-a", protocol: eth.ETH68}
	mockPeers[1] = &mockPeer{id: "peer-b", protocol: eth.ETH68}

	for _, peer := range mockPeers {
		pc := newPeerConnection(peer.id, peer.protocol, peer, log.New("peer", peer.id))
		if err := d.peers.Register(pc); err != nil {
			t.Fatal(err)
		}
	}

	if d.queue.PendingReceipts() == 0 {
		t.Fatal("expected pending receipts in queue")
	}

	err := d.concurrentFetch((*receiptQueue)(d), false)
	if err != ErrPeersUnavailable {
		t.Fatalf("expected ErrPeersUnavailable, got %v", err)
	}

	for _, peer := range mockPeers {
		if peer.receiptRequested.Load() {
			t.Errorf("peer %s should NOT have received a receipt request", peer.id)
		}
	}
}

// TestConcurrentFetchReceipts_MixedPeers verifies that concurrentFetch
// dispatches receipt requests only to eth/69 peers, skipping eth/68 ones.
func TestConcurrentFetchReceipts_MixedPeers(t *testing.T) {
	d := newTestDownloader()
	scheduleReceiptTask(d)

	var mockPeers = make([]*mockPeer, 2)
	mockPeers[0] = &mockPeer{id: "peer-eth68", protocol: eth.ETH68}
	mockPeers[1] = &mockPeer{id: "peer-eth69", protocol: eth.ETH69}

	for _, peer := range mockPeers {
		pc := newPeerConnection(peer.id, peer.protocol, peer, log.New("peer", peer.id))
		if err := d.peers.Register(pc); err != nil {
			t.Fatal(err)
		}
	}

	// Cancel the downloader after a short delay to allow the receipt request
	// to be dispatched to the eth/69 peer.
	go func() {
		<-time.After(1 * time.Second)
		close(d.cancelCh)
	}()

	err := d.concurrentFetch((*receiptQueue)(d), false)
	if err != errCanceled {
		t.Fatalf("expected errCanceled, got %v", err)
	}

	if mockPeers[0].receiptRequested.Load() {
		t.Error("eth/68 peer should NOT have received a receipt request")
	}

	if !mockPeers[1].receiptRequested.Load() {
		t.Error("eth/69 peer should have received a receipt request")
	}
}

func TestConcurrentFetchReceipts_BackedOffPeer(t *testing.T) {
	d := newTestDownloader()
	scheduleReceiptTask(d)

	peer := &mockPeer{id: "peer-eth69", protocol: eth.ETH69}
	pc := newPeerConnection(peer.id, peer.protocol, peer, log.New("peer", peer.id))
	pc.backoffFor(time.Minute)
	if err := d.peers.Register(pc); err != nil {
		t.Fatal(err)
	}

	err := d.concurrentFetch((*receiptQueue)(d), false)
	if err != ErrPeerBackedOff {
		t.Fatalf("expected ErrPeerBackedOff, got %v", err)
	}
	if peer.receiptRequested.Load() {
		t.Fatal("backed-off peer should not receive a receipt request")
	}
}

func TestConcurrentFetchGradesDepartedStalePeer(t *testing.T) {
	d := newTestDownloader()
	d.peers.rates.OverrideTTLLimit = 50 * time.Millisecond
	scheduleReceiptTask(d)

	peer := &mockPeer{id: "peer-eth69", protocol: eth.ETH69}
	pc := newPeerConnection(peer.id, peer.protocol, peer, log.New("peer", peer.id))
	if err := d.peers.Register(pc); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- d.concurrentFetch((*receiptQueue)(d), false)
	}()

	waitFor := func(cond func() bool) bool {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return cond()
	}

	if !waitFor(func() bool { return peer.receiptRequested.Load() }) {
		close(d.cancelCh)
		<-done
		t.Fatal("receipt request was never dispatched")
	}

	time.Sleep(200 * time.Millisecond)
	if err := d.peers.Unregister(pc.id); err != nil {
		t.Fatalf("failed to unregister peer: %v", err)
	}

	if !waitFor(func() bool { return softStrikeTally(d.peers, peer.id) >= 1 }) {
		close(d.cancelCh)
		<-done
		t.Fatal("a peer that left with a stale request must be graded, not silently dropped")
	}

	close(d.cancelCh)
	<-done
}

func TestConcurrentFetchMasterTimeoutAborts(t *testing.T) {
	d := newTestDownloader()
	d.peers.rates.OverrideTTLLimit = 50 * time.Millisecond
	scheduleReceiptTask(d)

	peer := &mockPeer{id: "master-eth69", protocol: eth.ETH69}
	pc := newPeerConnection(peer.id, peer.protocol, peer, log.New("peer", peer.id))
	if err := d.peers.Register(pc); err != nil {
		t.Fatal(err)
	}
	d.cancelPeer = peer.id

	if err := d.concurrentFetch((*receiptQueue)(d), false); err != errTimeout {
		t.Fatalf("expected errTimeout when the master peer times out, got %v", err)
	}
	select {
	case <-d.cancelCh:
	default:
		t.Fatal("a master timeout must cancel the sync cycle")
	}
}
