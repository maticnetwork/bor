package fetcher

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
)

// witnessSourceServer stands in for the network side of a witness pull. Each
// call hands back a witness from the first peer that the fetcher has not
// already blamed for this block, mirroring what ethHandler.resolveWitnessFetchPeer
// does against the real peer set.
type witnessSourceServer struct {
	fetcher *BlockFetcher

	lock     sync.Mutex
	peers    []string                      // Candidate sources, in preference order
	witness  map[string]*stateless.Witness // What each peer serves
	servedBy []string                      // Peers actually asked, in order
}

func newWitnessSourceServer(fetcher *BlockFetcher) *witnessSourceServer {
	return &witnessSourceServer{fetcher: fetcher, witness: make(map[string]*stateless.Witness)}
}

// add registers a peer that will serve the given witness when asked.
func (s *witnessSourceServer) add(peer string, witness *stateless.Witness) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.peers = append(s.peers, peer)
	s.witness[peer] = witness
}

// served returns the peers the fetcher pulled from, oldest first.
func (s *witnessSourceServer) served() []string {
	s.lock.Lock()
	defer s.lock.Unlock()

	return append([]string(nil), s.servedBy...)
}

// requester returns a witnessRequesterFn wired to this server.
func (s *witnessSourceServer) requester() witnessRequesterFn {
	return func(hash common.Hash, sink chan *eth.Response) (*eth.Request, error) {
		excluded := s.fetcher.ExcludedWitnessSources(hash)

		s.lock.Lock()
		var (
			peer    string
			witness *stateless.Witness
		)

		for _, candidate := range s.peers {
			if _, skip := excluded[candidate]; !skip {
				peer, witness = candidate, s.witness[candidate]
				break
			}
		}

		if peer == "" {
			s.lock.Unlock()
			// Same error string the real requester uses when no candidate is
			// left; the manager treats it as a soft failure with nobody to blame.
			return nil, errors.New("no peer with witness for hash is available")
		}

		s.servedBy = append(s.servedBy, peer)
		s.lock.Unlock()

		req := &eth.Request{Peer: peer, Cancel: make(chan struct{})}
		go func() {
			sink <- &eth.Response{
				Req:  req,
				Res:  []*stateless.Witness{witness},
				Done: make(chan error, 1),
			}
		}()

		return req, nil
	}
}

// newWitnessRetryTester builds a tester whose chain import rejects any witness
// in bad, reporting it the way core does for a witness-attributable failure.
func newWitnessRetryTester(t *testing.T, bad map[*stateless.Witness]bool) *fetcherTester {
	t.Helper()

	tester := newTester(false)
	tester.fetcher.SetWitnessServerStriker(tester.strikeWitnessServer)
	tester.insertHook = func(_ types.Blocks, witnesses []*stateless.Witness) (int, error) {
		if len(witnesses) > 0 && bad[witnesses[0]] {
			return 0, core.WitnessError(core.ErrStatelessStateRootMismatch)
		}
		return 0, nil
	}

	return tester
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// TestWitnessImportFailureRefetchesFromAnotherPeer is the core regression: a
// witness that passes every pre-import check and then fails stateless
// validation must not sink the block with it. Before the retry path existed the
// block was simply forgotten, which on a stateless node (no local state to fall
// back on) parks the chain at that height forever.
func TestWitnessImportFailureRefetchesFromAnotherPeer(t *testing.T) {
	hashes, blocks := makeChain(1, 0, genesis)
	target := blocks[hashes[0]]

	badWitness, err := stateless.NewWitness(target.Header(), nil)
	if err != nil {
		t.Fatalf("failed to build witness: %v", err)
	}

	goodWitness, err := stateless.NewWitness(target.Header(), nil)
	if err != nil {
		t.Fatalf("failed to build witness: %v", err)
	}

	tester := newWitnessRetryTester(t, map[*stateless.Witness]bool{badWitness: true})
	defer tester.fetcher.Stop()

	server := newWitnessSourceServer(tester.fetcher)
	server.add("bad-peer", badWitness)
	server.add("good-peer", goodWitness)

	if err := tester.fetcher.InjectBlockWithWitnessRequirement("origin-peer", target, server.requester()); err != nil {
		t.Fatalf("failed to inject block: %v", err)
	}

	waitFor(t, "block to import after retrying a different witness source", func() bool {
		return tester.getBlock(target.Hash()) != nil
	})

	if served := server.served(); len(served) != 2 || served[0] != "bad-peer" || served[1] != "good-peer" {
		t.Fatalf("witness sources asked = %v, want [bad-peer good-peer]", served)
	}

	// The peer that served the unusable witness is blamed; the one that served
	// a working witness is not.
	if got := tester.strikeCount("bad-peer"); got != 1 {
		t.Fatalf("bad-peer strikes = %d, want 1", got)
	}

	if got := tester.strikeCount("good-peer"); got != 0 {
		t.Fatalf("good-peer strikes = %d, want 0", got)
	}

	// Blaming the witness source must not touch the peer that sent the block:
	// the two are routinely different, and a bad witness says nothing about
	// whoever broadcast the block it belongs to.
	tester.lock.RLock()
	dropped := len(tester.drops)
	tester.lock.RUnlock()

	if dropped != 0 {
		t.Fatalf("dropped %d peers, want 0", dropped)
	}
}

// TestWitnessImportFailureBoundsRetries checks the other side of the trade: a
// block that fails against every witness it is offered (a producer that
// generated it wrong, or a block that is simply invalid) must not walk the
// whole peer set. It gets maxWitnessSourceRetries extra sources and no more.
func TestWitnessImportFailureBoundsRetries(t *testing.T) {
	hashes, blocks := makeChain(1, 0, genesis)
	target := blocks[hashes[0]]

	var (
		bad     = make(map[*stateless.Witness]bool)
		sources = []string{"peer-a", "peer-b", "peer-c", "peer-d"}
	)

	tester := newWitnessRetryTester(t, bad)
	defer tester.fetcher.Stop()

	server := newWitnessSourceServer(tester.fetcher)

	for _, peer := range sources {
		witness, err := stateless.NewWitness(target.Header(), nil)
		if err != nil {
			t.Fatalf("failed to build witness: %v", err)
		}

		bad[witness] = true

		server.add(peer, witness)
	}

	if err := tester.fetcher.InjectBlockWithWitnessRequirement("origin-peer", target, server.requester()); err != nil {
		t.Fatalf("failed to inject block: %v", err)
	}

	wantSources := maxWitnessSourceRetries + 1
	waitFor(t, "the retry budget to be spent", func() bool {
		return len(server.served()) >= wantSources
	})

	// Give the fetcher room to keep going if the bound were not enforced.
	time.Sleep(200 * time.Millisecond)

	if served := server.served(); len(served) != wantSources {
		t.Fatalf("witness sources asked = %v (%d), want %d", served, len(served), wantSources)
	}

	if tester.getBlock(target.Hash()) != nil {
		t.Fatal("block imported despite every witness failing validation")
	}

	// Nobody is blamed. Every source served a witness that failed, and no
	// source served one that worked, so there is no evidence separating "this
	// peer served bad bytes" from "the producer built a bad witness and every
	// peer relayed it faithfully". Striking here would punish honest relays for
	// a producer's fault — and with a whole sprint of such blocks, would cross
	// the disconnect threshold on a node with few witness-capable peers.
	for _, peer := range sources {
		if got := tester.strikeCount(peer); got != 0 {
			t.Fatalf("%s strikes = %d, want 0 (no peer is provably at fault)", peer, got)
		}
	}
}

// TestWitnessSourcesBlamedOnlyOnProvenRecovery isolates the blame rule from the
// retry mechanics: identical failing witnesses, the only difference being
// whether some peer eventually serves one that imports. A producer-side fault
// (nobody can serve a good witness) must cost honest relays nothing, while a
// peer-side fault (someone else's witness works) must be paid for.
func TestWitnessSourcesBlamedOnlyOnProvenRecovery(t *testing.T) {
	for _, tt := range []struct {
		name        string
		lastIsGood  bool
		wantStrikes int
	}{
		{name: "producer at fault, every witness bad", lastIsGood: false, wantStrikes: 0},
		{name: "peer at fault, a later witness works", lastIsGood: true, wantStrikes: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hashes, blocks := makeChain(1, 0, genesis)
			target := blocks[hashes[0]]

			bad := make(map[*stateless.Witness]bool)
			tester := newWitnessRetryTester(t, bad)
			defer tester.fetcher.Stop()

			server := newWitnessSourceServer(tester.fetcher)

			// One failing source, then a second whose witness is good or bad
			// depending on the case under test.
			for i, peer := range []string{"first-source", "second-source"} {
				witness, err := stateless.NewWitness(target.Header(), nil)
				if err != nil {
					t.Fatalf("failed to build witness: %v", err)
				}

				if i == 0 || !tt.lastIsGood {
					bad[witness] = true
				}

				server.add(peer, witness)
			}

			if err := tester.fetcher.InjectBlockWithWitnessRequirement("origin-peer", target, server.requester()); err != nil {
				t.Fatalf("failed to inject block: %v", err)
			}

			waitFor(t, "both sources to be tried", func() bool { return len(server.served()) >= 2 })
			time.Sleep(200 * time.Millisecond)

			if got := tester.strikeCount("first-source"); got != tt.wantStrikes {
				t.Fatalf("first-source strikes = %d, want %d", got, tt.wantStrikes)
			}

			// The peer that served a working witness is never blamed.
			if got := tester.strikeCount("second-source"); got != 0 {
				t.Fatalf("second-source strikes = %d, want 0", got)
			}
		})
	}
}

// TestNonWitnessImportFailureIsNotRetried guards the classification: an import
// that fails for a reason unrelated to the witness — an unavailable parent
// state, a stopped chain — must neither blame the peer that served the witness
// nor spend the block's retry budget.
func TestNonWitnessImportFailureIsNotRetried(t *testing.T) {
	hashes, blocks := makeChain(1, 0, genesis)
	target := blocks[hashes[0]]

	witness, err := stateless.NewWitness(target.Header(), nil)
	if err != nil {
		t.Fatalf("failed to build witness: %v", err)
	}

	tester := newTester(false)
	defer tester.fetcher.Stop()

	tester.fetcher.SetWitnessServerStriker(tester.strikeWitnessServer)
	tester.insertHook = func(types.Blocks, []*stateless.Witness) (int, error) {
		return 0, errors.New("missing trie node (local state unavailable)")
	}

	server := newWitnessSourceServer(tester.fetcher)
	server.add("only-peer", witness)

	if err := tester.fetcher.InjectBlockWithWitnessRequirement("origin-peer", target, server.requester()); err != nil {
		t.Fatalf("failed to inject block: %v", err)
	}

	waitFor(t, "the import attempt", func() bool { return len(server.served()) > 0 })
	time.Sleep(200 * time.Millisecond)

	if served := server.served(); len(served) != 1 {
		t.Fatalf("witness sources asked = %v, want exactly one (no retry)", served)
	}

	if got := tester.strikeCount("only-peer"); got != 0 {
		t.Fatalf("only-peer strikes = %d, want 0", got)
	}
}

// TestWitnessSourceExclusionLifecycle covers the bookkeeping the retry path
// depends on: exclusions are per-block, survive until the block resolves, and
// are reclaimed by the TTL sweep for blocks that never resolve at all.
func TestWitnessSourceExclusionLifecycle(t *testing.T) {
	m := newWitnessManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, 0)

	var (
		first  = common.Hash{0x01}
		second = common.Hash{0x02}
	)

	if got := m.excludedWitnessSources(first); got != nil {
		t.Fatalf("fresh manager reported exclusions %v, want none", got)
	}

	m.excludeWitnessSource(first, "peer-a")
	m.excludeWitnessSource(first, "peer-b")
	m.excludeWitnessSource(second, "peer-a")
	m.excludeWitnessSource(first, "") // Ignored: no peer to exclude.

	if got := m.excludedWitnessSources(first); len(got) != 2 {
		t.Fatalf("exclusions for first = %v, want 2 entries", got)
	}

	if !m.isWitnessSourceExcluded(first, "peer-a") {
		t.Fatal("peer-a should be excluded for the first block")
	}

	// Exclusions are scoped to the block that failed, not to the peer.
	if m.isWitnessSourceExcluded(second, "peer-b") {
		t.Fatal("peer-b should not be excluded for an unrelated block")
	}

	m.clearWitnessSourceExclusions(first)

	if m.isWitnessSourceExcluded(first, "peer-a") {
		t.Fatal("exclusions for the first block should be cleared")
	}

	if !m.isWitnessSourceExcluded(second, "peer-a") {
		t.Fatal("clearing one block must not clear another")
	}

	// A block that never reaches a terminal import outcome is reclaimed by TTL
	// rather than leaking for the process lifetime.
	m.witnessSourceMu.Lock()
	m.witnessSourceExpiry[second] = time.Now().Add(-time.Second)
	m.witnessSourceMu.Unlock()

	m.cleanupWitnessSourceExclusions()

	if m.isWitnessSourceExcluded(second, "peer-a") {
		t.Fatal("expired exclusions should be swept")
	}
}
