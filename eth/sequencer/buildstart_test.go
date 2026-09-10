package sequencer

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
)

// classifierPublisher reads the harness store but runs no transport loop,
// so the state a classification sets stays put for assertions.
func classifierPublisher(t *testing.T, h *harness, chain chainReader) *Publisher {
	t.Helper()

	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	p := barePublisher()
	p.chain = chain
	p.read.setClient(pb.NewConsumerServiceClient(conn))

	return p
}

func modeKind(p *Publisher) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.mode.kind
}

func holdKind(p *Publisher) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.hold.kind
}

// Each build-start store shape classifies to its designed action: hold when
// blind or behind, mute over sealed ground, recover a dead sealer's height.
func TestBuildStartStoreShapes(t *testing.T) {
	t.Run("unreadable store holds the build", func(t *testing.T) {
		h := startHarness(t)
		p := newTestPublisher(t, h, nil)
		sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
		waitHead(t, h, p, 5*time.Second)

		h.stop()

		if w := p.AdoptWindow(2, sealed.Hash()); w != nil {
			t.Fatal("a blind build start must not adopt")
		}

		if holdKind(p) != holdBuild {
			t.Fatal("a blind build start must hold and buffer")
		}
	})

	t.Run("empty store is a clean boundary", func(t *testing.T) {
		h := startHarness(t)

		fresh := classifierPublisher(t, h, nil)
		if w := fresh.AdoptWindow(1, common.Hash{0x01}); w != nil {
			t.Fatal("an empty store must not adopt")
		}

		if holdKind(fresh) != holdNone {
			t.Fatal("a clean boundary must not hold: the open publishes")
		}
	})

	t.Run("live window above a young height is behind", func(t *testing.T) {
		h := startHarness(t)
		appendForeignOpen(t, h, 10, common.Hash{0x0a})

		fresh := classifierPublisher(t, h, nil)
		if w := fresh.AdoptWindow(5, common.Hash{0x05}); w != nil {
			t.Fatal("a store without our history must not adopt")
		}

		if holdKind(fresh) != holdBuild {
			t.Fatal("a behind store must hold and buffer")
		}
	})

	t.Run("seal edge below the parent is behind", func(t *testing.T) {
		h := startHarness(t)
		p := newTestPublisher(t, h, nil)
		first := publishBlock(t, p, 1, common.Hash{0xef}, 1)
		publishBlock(t, p, 2, first.Hash(), 1)
		waitHead(t, h, p, 5*time.Second)

		fresh := classifierPublisher(t, h, &fakeChain{})
		if w := fresh.AdoptWindow(5, common.Hash{0x05}); w != nil {
			t.Fatal("an owed gap must not adopt")
		}

		if holdKind(fresh) != holdBuild {
			t.Fatal("an owed gap must hold and buffer")
		}

		fresh.mu.Lock()
		from, to := fresh.pendingFrom, fresh.pendingTo
		fresh.mu.Unlock()

		if from != 2 || to != 4 {
			t.Fatalf("the gap [seal edge, parent] must be primed as debt: %d..%d", from, to)
		}

		// A later prime merges its gap into the standing debt rather than
		// clobbering it.
		fresh.mu.Lock()
		fresh.primeBackfillLocked(tailInfo{haveSeal: true, lastSealHeight: 3}, 7)
		from, to = fresh.pendingFrom, fresh.pendingTo
		fresh.mu.Unlock()

		if from != 2 || to != 6 {
			t.Fatalf("debt must merge across build starts: %d..%d", from, to)
		}
	})

	t.Run("probes find the seal edge", func(t *testing.T) {
		h := startHarness(t)
		p := newTestPublisher(t, h, nil)
		parent := common.Hash{0xef}

		for n := uint64(1); n <= 3; n++ {
			parent = publishBlock(t, p, n, parent, 1).Hash()
		}

		waitHead(t, h, p, 5*time.Second)

		r := classifierPublisher(t, h, nil).read
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if hgt, found, err := r.probeDown(ctx, 9); err != nil || !found || hgt != 3 {
			t.Fatalf("probeDown(9) = %d %v %v, want the edge 3", hgt, found, err)
		}

		if hgt, found, err := r.probeDown(ctx, 3); err != nil || !found || hgt != 3 {
			t.Fatalf("probeDown(3) = %d %v %v, want the direct hit", hgt, found, err)
		}

		if edge, err := r.probeUp(ctx, 1); err != nil || edge != 3 {
			t.Fatalf("probeUp(1) = %d %v, want the edge 3", edge, err)
		}

		if edge, err := r.probeUp(ctx, 3); err != nil || edge != 3 {
			t.Fatalf("probeUp(3) = %d %v, want itself", edge, err)
		}
	})

	t.Run("sealed height the chain has is muted over", func(t *testing.T) {
		h := startHarness(t)
		p := newTestPublisher(t, h, nil)
		sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
		waitHead(t, h, p, 5*time.Second)

		fc := &fakeChain{canonical: map[uint64]common.Hash{1: sealed.Hash()}}

		fresh := newTestPublisher(t, h, fc)
		if w := fresh.AdoptWindow(1, common.Hash{0xef}); w != nil {
			t.Fatal("a store-sealed height must mute, not adopt")
		}

		if modeKind(fresh) != modeOverSealed {
			t.Fatal("sealing over a closed height must run gate-tolerated")
		}
	})

	t.Run("freshly sealed height waits out the grace", func(t *testing.T) {
		h := startHarness(t)
		parent := common.Hash{0xef}

		header := testHeader(1, parent)
		header.Time = uint64(time.Now().Unix())
		appendForeignOpenAt(t, h, 1, parent, header.Time)
		appendForeignSeal(t, h, header)

		fc := &fakeChain{canonical: map[uint64]common.Hash{}}

		fresh := newTestPublisher(t, h, fc)
		if w := fresh.AdoptWindow(1, parent); w != nil {
			t.Fatal("a seal inside its grace must not be rebuilt yet")
		}

		if modeKind(fresh) != modeSealedWait {
			t.Fatal("a seal inside its grace must refuse divergent broadcasts")
		}
	})

	t.Run("dead sealer's height is recovered exactly", func(t *testing.T) {
		h := startHarness(t)
		p := newTestPublisher(t, h, nil)
		// testHeader timestamps are ancient, so the grace has long elapsed.
		sealed := publishBlock(t, p, 1, common.Hash{0xef}, 2)
		waitHead(t, h, p, 5*time.Second)

		fc := &fakeChain{canonical: map[uint64]common.Hash{}}

		fresh := newTestPublisher(t, h, fc)

		w := fresh.AdoptWindow(1, common.Hash{0xef})
		if w == nil {
			t.Fatal("an elapsed sealed height must offer its rebuild")
		}

		if w.Number != 1 || w.ParentHash != (common.Hash{0xef}) || len(w.Txs) != 2 {
			t.Fatalf("recovered window must mirror the sealed generation: %+v", w)
		}

		if w.Timestamp != sealed.Time {
			t.Fatalf("recovered timestamp %d, want the sealed block's %d", w.Timestamp, sealed.Time)
		}

		if modeKind(fresh) != modeRecover {
			t.Fatal("a recovery build must publish nothing")
		}
	})
}
