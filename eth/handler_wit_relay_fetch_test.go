package eth

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"go.uber.org/mock/gomock"
)

// fakeWitnessPeer implements WitnessPeer with a canned, page-addressable
// response, so fetchWitnessPages/requestWitnessPage can be exercised without
// a live devnet. Modeled after the wit.Request zero-value pattern
// ("Tests mock out the dispatcher, skip internal cancellation" in
// (*wit.Request).Close), which is exactly what a zero-value &wit.Request{}
// triggers.
type fakeWitnessPeer struct {
	pages      map[uint64][]byte
	totalPages uint64
	// requested records every page number asked for, in call order — lets a
	// test assert both the reconstructed bytes AND that paging actually
	// walked more than one page (a naive single-page fetch could otherwise
	// "pass" by luck if a test only checked the final bytes).
	requested []uint64
	// knownWitness/knownAnnounce back the Add/Contains pair for real, so a
	// test can register this fake as a body-holder or announce-relayer and
	// have peersWithWitnessCandidates actually find it — a no-op stub here
	// would silently make every candidate invisible to that lookup.
	knownWitness  map[common.Hash]bool
	knownAnnounce map[common.Hash]bool
}

func (f *fakeWitnessPeer) RequestWitness(reqs []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
	page := reqs[0].Page
	f.requested = append(f.requested, page)
	data := f.pages[page]
	sink <- &wit.Response{
		Res: &wit.WitnessPacketRLPPacket{
			WitnessPacketResponse: wit.WitnessPacketResponse{
				{Data: data, Hash: reqs[0].Hash, Page: page, TotalPages: f.totalPages},
			},
		},
	}
	return &wit.Request{}, nil
}

func (f *fakeWitnessPeer) RequestWitnessMetadata(hashes []common.Hash, sink chan *wit.Response) (*wit.Request, error) {
	return &wit.Request{}, nil
}
func (f *fakeWitnessPeer) AsyncSendNewWitness(witness *stateless.Witness)                   {}
func (f *fakeWitnessPeer) AsyncSendNewWitnessHash(hash common.Hash, number uint64)          {}
func (f *fakeWitnessPeer) AsyncSendSignedWitnessAnnouncement(wit.SignedWitnessAnnouncement) {}
func (f *fakeWitnessPeer) Close()                                                           {}
func (f *fakeWitnessPeer) ID() string                                                       { return "fake" }
func (f *fakeWitnessPeer) Version() uint                                                    { return wit.WIT2 }
func (f *fakeWitnessPeer) Log() log.Logger                                                  { return log.Root() }
func (f *fakeWitnessPeer) KnownWitnesses() *wit.KnownCache                                  { return nil }
func (f *fakeWitnessPeer) AddKnownWitness(hash common.Hash) {
	if f.knownWitness == nil {
		f.knownWitness = make(map[common.Hash]bool)
	}
	f.knownWitness[hash] = true
}
func (f *fakeWitnessPeer) AddKnownAnnounce(hash common.Hash) {
	if f.knownAnnounce == nil {
		f.knownAnnounce = make(map[common.Hash]bool)
	}
	f.knownAnnounce[hash] = true
}
func (f *fakeWitnessPeer) KnownWitnessesCount() int                         { return len(f.knownWitness) }
func (f *fakeWitnessPeer) KnownWitnessesContains(w *stateless.Witness) bool { return false }
func (f *fakeWitnessPeer) KnownWitnessContainsHash(hash common.Hash) bool {
	return f.knownWitness[hash]
}
func (f *fakeWitnessPeer) KnownAnnounceContainsHash(hash common.Hash) bool {
	return f.knownAnnounce[hash]
}
func (f *fakeWitnessPeer) ReplyWitness(requestID uint64, response *wit.WitnessPacketResponse) error {
	return nil
}

func TestFetchWitnessPages_SinglePage(t *testing.T) {
	hash := common.HexToHash("0x1")
	data := []byte("small witness fits in one page")
	peer := &fakeWitnessPeer{pages: map[uint64][]byte{0: data}, totalPages: 1}

	got, ok := fetchWitnessPages(peer, hash)
	if !ok {
		t.Fatal("expected success")
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	if len(peer.requested) != 1 || peer.requested[0] != 0 {
		t.Fatalf("expected exactly one request for page 0, got %v", peer.requested)
	}
}

// doneSignalingWitnessPeer wraps fakeWitnessPeer but reproduces the one
// property none of the other fakes in this file model: the real wit
// dispatcher (wit/dispatcher.go's dispatchResponse) sets a non-nil
// Response.Done channel and BLOCKS the peer's own response-handling
// goroutine on it until the recipient signals receipt — and that goroutine
// is the same one draining the peer's per-protocol read channel, so leaving
// Done unsignaled head-of-line-blocks the peer's entire connection (every
// subprotocol is demuxed off one shared TCP stream), not just this one
// fetch. The other fakes' nil Done silently no-ops through requestWitnessPage
// (see `if res.Done != nil`), so they could never have caught a missing
// signal — this one can.
type doneSignalingWitnessPeer struct {
	*fakeWitnessPeer
	// Buffered generously: one send per page fetched, never more than a
	// handful in these tests, so the watchdog goroutines below never block
	// on it regardless of whether the test drains every entry.
	doneSignaled chan struct{}
}

func (f *doneSignalingWitnessPeer) RequestWitness(reqs []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
	page := reqs[0].Page
	f.requested = append(f.requested, page)
	data := f.pages[page]
	done := make(chan error) // unbuffered, exactly like dispatchResponse's real one
	go func() {
		select {
		case <-done:
			f.doneSignaled <- struct{}{}
		case <-time.After(2 * time.Second):
			// No signal sent: the test's own assertion below will time out
			// and fail with a clear message instead of this goroutine
			// leaking silently.
		}
	}()
	sink <- &wit.Response{
		Res: &wit.WitnessPacketRLPPacket{
			WitnessPacketResponse: wit.WitnessPacketResponse{
				{Data: data, Hash: reqs[0].Hash, Page: page, TotalPages: f.totalPages},
			},
		},
		Done: done,
	}
	return &wit.Request{}, nil
}

// TestRequestWitnessPage_SignalsDoneOnRealResponse guards against the exact
// bug found on a real devnet run: requestWitnessPage received a real
// response and returned without ever signaling Response.Done. In
// production that permanently blocks the wit dispatcher's response-handling
// goroutine for that peer — which is the same goroutine draining the
// peer's demuxed read channel — silently freezing ALL further traffic from
// that peer (including unrelated ETH-protocol block announcements), not
// just this fetch. The peer looks connected throughout; nothing errors.
func TestRequestWitnessPage_SignalsDoneOnRealResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages map[uint64][]byte
	}{
		{"single page", map[uint64][]byte{0: []byte("witness bytes")}},
		{"multi page", map[uint64][]byte{0: make([]byte, PageSize), 1: []byte("tail")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			totalPages := uint64(1)
			if len(tc.pages) > 1 {
				totalPages = uint64(len(tc.pages))
			}
			peer := &doneSignalingWitnessPeer{
				fakeWitnessPeer: &fakeWitnessPeer{pages: tc.pages, totalPages: totalPages},
				doneSignaled:    make(chan struct{}, len(tc.pages)),
			}

			hash := common.HexToHash("0xd0d0")
			if _, ok := fetchWitnessPages(peer, hash); !ok {
				t.Fatal("expected fetchWitnessPages to succeed")
			}

			// Every page fetched must have signaled Done — not just the
			// last one — since each RequestWitness call gets its own real
			// dispatcher-shaped response that would otherwise permanently
			// block that call's (simulated) dispatcher goroutine.
			for i := 0; i < len(tc.pages); i++ {
				select {
				case <-peer.doneSignaled:
				case <-time.After(1 * time.Second):
					t.Fatalf("requestWitnessPage never signaled Response.Done for page %d — this reproduces the real "+
						"devnet deadlock: the wit dispatcher's response goroutine for this peer would hang forever, "+
						"head-of-line-blocking all further traffic (including block announcements) from it", i)
				}
			}
		})
	}
}

// malformedDoneSignalingWitnessPeer reproduces a misbehaving/buggy peer that
// replies with the wrong response type (or an already-empty page), still
// wrapped in a real dispatcher-shaped Done channel. requestWitnessPage must
// signal Done as soon as it receives *any* non-nil response — before it ever
// looks at what's inside — so a peer sending garbage must not be able to
// leave the dispatcher goroutine blocked any more than a well-formed one
// can. If a future change moved the res.Done signal below the type/content
// check ("only signal on a response we can actually use"), this is exactly
// the case that would silently reintroduce the head-of-line-blocking bug.
type malformedDoneSignalingWitnessPeer struct {
	*fakeWitnessPeer
	doneSignaled chan struct{}
}

func (f *malformedDoneSignalingWitnessPeer) RequestWitness(reqs []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
	page := reqs[0].Page
	f.requested = append(f.requested, page)
	done := make(chan error) // unbuffered, exactly like dispatchResponse's real one
	go func() {
		select {
		case <-done:
			f.doneSignaled <- struct{}{}
		case <-time.After(2 * time.Second):
			// No signal sent: the assertion below times out with a clear message.
		}
	}()
	sink <- &wit.Response{
		Res:  "not a *wit.WitnessPacketRLPPacket", // wrong type: fails the cast in requestWitnessPage
		Done: done,
	}
	return &wit.Request{}, nil
}

func TestRequestWitnessPage_SignalsDoneEvenOnMalformedResponse(t *testing.T) {
	peer := &malformedDoneSignalingWitnessPeer{
		fakeWitnessPeer: &fakeWitnessPeer{totalPages: 1},
		doneSignaled:    make(chan struct{}, 1),
	}

	hash := common.HexToHash("0xbadbad")
	if _, _, ok := requestWitnessPage(peer, hash, 0); ok {
		t.Fatal("expected requestWitnessPage to fail on a wrong-typed response")
	}

	select {
	case <-peer.doneSignaled:
	case <-time.After(1 * time.Second):
		t.Fatal("requestWitnessPage never signaled Response.Done for a malformed response — " +
			"a misbehaving peer would permanently block the wit dispatcher's response goroutine, " +
			"head-of-line-blocking all further traffic from it, exactly like the real devnet deadlock")
	}
}

func TestFetchWitnessPages_MultiPage(t *testing.T) {
	hash := common.HexToHash("0x2")
	page0 := make([]byte, PageSize)
	for i := range page0 {
		page0[i] = byte(i % 251)
	}
	page1 := []byte("tail page, shorter than PageSize")
	peer := &fakeWitnessPeer{
		pages:      map[uint64][]byte{0: page0, 1: page1},
		totalPages: 2,
	}

	got, ok := fetchWitnessPages(peer, hash)
	if !ok {
		t.Fatal("expected success")
	}
	want := append(append([]byte{}, page0...), page1...)
	if len(got) != len(want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte mismatch at offset %d: got %d want %d", i, got[i], want[i])
		}
	}
	if len(peer.requested) != 2 || peer.requested[0] != 0 || peer.requested[1] != 1 {
		t.Fatalf("expected requests for pages [0 1] in order, got %v", peer.requested)
	}
}

func TestFetchWitnessPages_EmptyPageMeansUpstreamDoesNotHaveIt(t *testing.T) {
	hash := common.HexToHash("0x3")
	peer := &fakeWitnessPeer{pages: map[uint64][]byte{0: nil}, totalPages: 1}

	if _, ok := fetchWitnessPages(peer, hash); ok {
		t.Fatal("expected failure on an empty page response")
	}
}

// TestFetchWitnessPages_ImplausibleTotalPagesIsRejectedNotCrashed guards the
// finding a code review caught: fetchWitnessPages used to size
// make([]byte, 0, len(firstPage)*int(totalPages)) directly from the peer's
// own page-0 response with no bound check. A single malicious candidate
// reporting a huge — or int-overflowing, i.e. negative once cast —
// TotalPages turned one crafted response into either an OOM allocation or
// an immediate "makeslice: cap out of range" panic, and the goroutine
// running triggerRelayFetch has no recover, so that panic crashes the whole
// node. Every case here must fail cleanly (ok=false), never panic.
func TestFetchWitnessPages_ImplausibleTotalPagesIsRejectedNotCrashed(t *testing.T) {
	hash := common.HexToHash("0xbadc0de")

	for _, tc := range []struct {
		name       string
		totalPages uint64
	}{
		{"far beyond any real witness size", maxRelayFetchPages * 1000},
		{"exactly one past the cap", maxRelayFetchPages + 1},
		{"overflows int64 to negative when cast", uint64(1) << 63},
		{"maximum possible uint64", ^uint64(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &fakeWitnessPeer{pages: map[uint64][]byte{0: []byte("small")}, totalPages: tc.totalPages}

			if _, ok := fetchWitnessPages(peer, hash); ok {
				t.Fatalf("expected rejection for an implausible TotalPages=%d, not success", tc.totalPages)
			}
			// Reaching this line at all (rather than the test process dying
			// to a panic) is itself part of what this test verifies.
		})
	}
}

// TestFetchWitnessPages_TotalPagesAtCapIsAccepted confirms the cap isn't
// simply rejecting every multi-page witness: a page count right at
// maxRelayFetchPages — a large but legitimate witness — must still succeed.
func TestFetchWitnessPages_TotalPagesAtCapIsAccepted(t *testing.T) {
	hash := common.HexToHash("0xf00d")
	pages := make(map[uint64][]byte, maxRelayFetchPages)
	for i := uint64(0); i < maxRelayFetchPages; i++ {
		pages[i] = []byte{byte(i)}
	}
	peer := &fakeWitnessPeer{pages: pages, totalPages: maxRelayFetchPages}

	got, ok := fetchWitnessPages(peer, hash)
	if !ok {
		t.Fatal("expected a page count exactly at the cap to succeed")
	}
	if len(got) != int(maxRelayFetchPages) {
		t.Fatalf("got %d bytes, want %d (one byte per page)", len(got), maxRelayFetchPages)
	}
}

func TestFetchWitnessPages_MidFetchTotalPagesChangeIsRejected(t *testing.T) {
	// A server that reports a different TotalPages on page 1 than it did on
	// page 0 is either buggy or lying; either way the partial concatenation
	// must not be trusted as if it were the complete witness.
	hash := common.HexToHash("0x4")
	peer := &fakeWitnessPeer{pages: map[uint64][]byte{0: []byte("a"), 1: []byte("b")}, totalPages: 2}
	// Override page 1's reported TotalPages by wrapping RequestWitness via a
	// second fake that answers page 0 normally but flips TotalPages on page 1.
	inconsistent := &inconsistentTotalPagesPeer{fakeWitnessPeer: peer, flipAtPage: 1, flippedTotal: 3}

	if _, ok := fetchWitnessPages(inconsistent, hash); ok {
		t.Fatal("expected failure when TotalPages is inconsistent across pages")
	}
}

type inconsistentTotalPagesPeer struct {
	*fakeWitnessPeer
	flipAtPage   uint64
	flippedTotal uint64
}

func (f *inconsistentTotalPagesPeer) RequestWitness(reqs []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
	page := reqs[0].Page
	f.requested = append(f.requested, page)
	total := f.totalPages
	if page == f.flipAtPage {
		total = f.flippedTotal
	}
	sink <- &wit.Response{
		Res: &wit.WitnessPacketRLPPacket{
			WitnessPacketResponse: wit.WitnessPacketResponse{
				{Data: f.pages[page], Hash: reqs[0].Hash, Page: page, TotalPages: total},
			},
		},
	}
	return &wit.Request{}, nil
}

// buildTestWitnessBytes returns canonical RLP bytes for a minimal witness and
// their commit hash, mirroring what a real BP-signed announcement would
// commit to.
func buildTestWitnessBytes(t *testing.T) ([]byte, common.Hash) {
	t.Helper()
	header := &types.Header{Number: big.NewInt(1)}
	witness, err := stateless.NewWitness(header, nil)
	if err != nil {
		t.Fatalf("stateless.NewWitness: %v", err)
	}
	var buf []byte
	{
		w := &rlpBufWriter{}
		if err := witness.EncodeRLP(w); err != nil {
			t.Fatalf("EncodeRLP: %v", err)
		}
		buf = w.data
	}
	return buf, stateless.WitnessCommitHash(buf)
}

// rlpBufWriter is a trivial io.Writer since bytes.Buffer isn't imported here.
type rlpBufWriter struct{ data []byte }

func (w *rlpBufWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

func TestFetchAndVerifyWitness_FallsBackOnByteMismatchToNextCandidate(t *testing.T) {
	hash := common.HexToHash("0x10")
	goodData, wantHash := buildTestWitnessBytes(t)

	bad := &fakeWitnessPeer{pages: map[uint64][]byte{0: []byte("not the real witness")}, totalPages: 1}
	good := &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1}

	candidates := []namedWitnessPeer{{id: "bad", peer: bad}, {id: "good", peer: good}}
	data, witness, servedBy, ok := fetchAndVerifyWitness(candidates, hash, wantHash)
	if !ok {
		t.Fatal("expected the second (good) candidate to succeed despite the first serving mismatched bytes")
	}
	if string(data) != string(goodData) {
		t.Fatalf("got %d bytes, want the good candidate's %d bytes", len(data), len(goodData))
	}
	if witness == nil {
		t.Fatal("expected a decoded witness")
	}
	// The bad candidate must never contaminate the result even though it was
	// tried first — no partial trust, no averaging, no first-come-wins.
	if len(bad.requested) == 0 {
		t.Fatal("expected the bad candidate to actually be tried (not skipped)")
	}
	if servedBy != "good" {
		t.Fatalf("expected servedBy to identify the good candidate, got %q", servedBy)
	}
}

func TestFetchAndVerifyWitness_AllCandidatesFail(t *testing.T) {
	hash := common.HexToHash("0x11")
	_, wantHash := buildTestWitnessBytes(t)

	empty := &fakeWitnessPeer{pages: map[uint64][]byte{0: nil}, totalPages: 1}
	wrong := &fakeWitnessPeer{pages: map[uint64][]byte{0: []byte("still not it")}, totalPages: 1}

	candidates := []namedWitnessPeer{{id: "empty", peer: empty}, {id: "wrong", peer: wrong}}
	if _, _, _, ok := fetchAndVerifyWitness(candidates, hash, wantHash); ok {
		t.Fatal("expected failure when every candidate is empty or wrong")
	}
}

// registerFakePeer inserts p into the peer set under ps.lock. newTestHandler
// starts a real background chainSyncer goroutine that reads ps.peers (e.g.
// peerSet.len() from nextSyncOp) concurrently with test setup, so a raw
// `ps.peers[id] = p` from the test goroutine data-races with it under -race.
// registerPeer itself can't be reused here: it takes a concrete *wit.Peer,
// not the WitnessPeer interface these tests fake.
func registerFakePeer(ps *peerSet, p *ethPeer) {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	ps.peers[p.ID()] = p
}

// unregisterFakePeer is the lock-safe counterpart to registerFakePeer, for
// tests that need to swap out a candidate mid-test.
func unregisterFakePeer(ps *peerSet, id string) {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	delete(ps.peers, id)
}

// newFakeEthPeerWithWitness builds a real *ethPeer (needed for triggerRelayFetch's
// h.peers.peersWithWitnessCandidates + src.ID() calls) wrapping a fake
// WitnessPeer, matching the &ethPeer{Peer: eth.NewPeer(...), witPeer: ...}
// pattern already used in peer_test.go.
func newFakeEthPeerWithWitness(id byte, fake WitnessPeer) *ethPeer {
	p2pPeer := p2p.NewPeer(enode.ID{id}, "test-peer", []p2p.Cap{})
	return &ethPeer{
		Peer:    eth.NewPeer(eth.ETH69, p2pPeer, nil, nil),
		witPeer: &witPeer{Peer: fake},
	}
}

func TestTriggerRelayFetch_EndToEndPushesToWaiterAndCleansUpDedup(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	goodData, wantHash := buildTestWitnessBytes(t)
	hash := common.HexToHash("0x20")

	// One candidate that has it, registered in the peer set the same way a
	// real connected peer would be.
	src := newFakeEthPeerWithWitness(1, &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1})
	src.witPeer.Peer.AddKnownWitness(hash) // mark it a body-holder so peersWithWitnessCandidates finds it
	registerFakePeer(h.peers, src)

	// One real downstream waiter, registered the same way recordWitnessWaiter
	// would via witnessWaiters.record on an actual GetWitness request.
	waiterPeer, cleanup := newTestWitPeerWithReader()
	defer cleanup()
	h.witnessWaiters.record(hash, waiterPeer)

	pushesBefore := wit2WaiterPushMeter.Snapshot().Count()

	h.triggerRelayFetch(hash, wantHash, "" /* no requester to exclude */)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := h.pendingWitnessBodies.get(hash); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	body, _, ok := h.pendingWitnessBodies.get(hash)
	if !ok {
		t.Fatal("expected the fetched witness to be cached for serving")
	}
	if string(body) != string(goodData) {
		t.Fatal("cached body does not match the fetched witness bytes")
	}
	if h.witnessWaiters.has(hash) {
		t.Fatal("expected the waiter to have been drained (pushed to) after the fetch succeeded")
	}
	if got := wit2WaiterPushMeter.Snapshot().Count() - pushesBefore; got != 1 {
		t.Fatalf("expected exactly 1 waiter push, got %d", got)
	}
	if _, inFlight := h.relayFetchInFlight.Load(hash); inFlight {
		t.Fatal("expected the in-flight dedup entry to be cleaned up after the fetch completed")
	}
}

func TestTriggerRelayFetch_DedupSkipsConcurrentDuplicateFetch(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	goodData, wantHash := buildTestWitnessBytes(t)
	hash := common.HexToHash("0x21")

	counting := &countingFakeWitnessPeer{fakeWitnessPeer: &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1}}
	src := newFakeEthPeerWithWitness(2, counting)
	src.witPeer.Peer.AddKnownWitness(hash)
	registerFakePeer(h.peers, src)

	h.triggerRelayFetch(hash, wantHash, "")
	h.triggerRelayFetch(hash, wantHash, "") // fired immediately after — must be deduped, not a second fetch

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := h.pendingWitnessBodies.get(hash); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, ok := h.pendingWitnessBodies.get(hash); !ok {
		t.Fatal("expected the fetch to eventually succeed")
	}
	if got := counting.fetchCount(); got != 1 {
		t.Fatalf("expected exactly 1 real fetch attempt despite 2 triggers, got %d", got)
	}
}

// countingFakeWitnessPeer counts distinct RequestWitness calls at page 0,
// which is a proxy for "how many times did we actually go fetch this hash"
// (as opposed to how many times triggerRelayFetch was called).
type countingFakeWitnessPeer struct {
	*fakeWitnessPeer
}

func (f *countingFakeWitnessPeer) fetchCount() int {
	n := 0
	for _, p := range f.requested {
		if p == 0 {
			n++
		}
	}
	return n
}

func TestRecordWitnessWaiter_DeferredOnlyNeverTriggersRelayFetch(t *testing.T) {
	th := newTestHandler()
	h := th.handler
	hash := common.HexToHash("0x22")

	// A deferred (not-yet-producer-verified) announcement exists, but there
	// is NO verified signed entry for this hash — recordWitnessWaiter's
	// "hasSigned" branch (the only one that calls triggerRelayFetch) must
	// not fire. This is the trust boundary: an unverified announcement must
	// never be able to make this node spend resources fetching on its say-so.
	if h.deferredAnnounces == nil {
		t.Fatal("test fixture missing deferredAnnounces cache")
	}
	h.deferredAnnounces.put(wit.SignedWitnessAnnouncement{BlockHash: hash}, "some-peer")

	peer, cleanup := newTestWitPeerWithReader()
	defer cleanup()

	witHandler := (*witHandler)(h)
	witHandler.recordWitnessWaiter(hash, peer)

	time.Sleep(50 * time.Millisecond) // give a wrongly-triggered goroutine a chance to start
	if _, inFlight := h.relayFetchInFlight.Load(hash); inFlight {
		t.Fatal("recordWitnessWaiter must not trigger a relay fetch from the deferred (unverified) branch")
	}
}

func TestTriggerRelayFetch_PushesToAllRegisteredWaitersNotJustOne(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	goodData, wantHash := buildTestWitnessBytes(t)
	hash := common.HexToHash("0x23")

	src := newFakeEthPeerWithWitness(3, &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1})
	src.witPeer.Peer.AddKnownWitness(hash)
	registerFakePeer(h.peers, src)

	// Two distinct downstream peers both asked us for this hash before we
	// had it — both must get pushed once we do, not just the first.
	waiterA, cleanupA := newTestWitPeerWithReader()
	defer cleanupA()
	waiterB, cleanupB := newTestWitPeerWithReader()
	defer cleanupB()
	h.witnessWaiters.record(hash, waiterA)
	h.witnessWaiters.record(hash, waiterB)

	pushesBefore := wit2WaiterPushMeter.Snapshot().Count()
	h.triggerRelayFetch(hash, wantHash, "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if wit2WaiterPushMeter.Snapshot().Count()-pushesBefore >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := wit2WaiterPushMeter.Snapshot().Count() - pushesBefore; got != 2 {
		t.Fatalf("expected both distinct waiters to be pushed (2 pushes), got %d", got)
	}
}

func TestTriggerRelayFetch_CleansUpAfterTotalFailureAndAllowsLaterRetry(t *testing.T) {
	th := newTestHandler()
	h := th.handler
	hash := common.HexToHash("0x24")

	// First attempt: the only candidate has nothing for this hash at all.
	empty := newFakeEthPeerWithWitness(4, &fakeWitnessPeer{pages: map[uint64][]byte{0: nil}, totalPages: 1})
	empty.witPeer.Peer.AddKnownWitness(hash)
	registerFakePeer(h.peers, empty)

	_, wantHash := buildTestWitnessBytes(t)
	h.triggerRelayFetch(hash, wantHash, "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, inFlight := h.relayFetchInFlight.Load(hash); !inFlight {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, inFlight := h.relayFetchInFlight.Load(hash); inFlight {
		t.Fatal("expected the dedup entry to clear after the fetch exhausted all candidates")
	}
	if _, _, ok := h.pendingWitnessBodies.get(hash); ok {
		t.Fatal("expected nothing cached after a total failure")
	}

	// Second attempt, later: a real candidate now exists. The earlier
	// failure must not have left anything (a stuck dedup entry, a poisoned
	// cache) that would block this from succeeding.
	goodData, wantHash2 := buildTestWitnessBytes(t)
	if wantHash2 != wantHash {
		t.Fatal("test setup: expected buildTestWitnessBytes to be deterministic")
	}
	good := newFakeEthPeerWithWitness(5, &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1})
	good.witPeer.Peer.AddKnownWitness(hash)
	registerFakePeer(h.peers, good)
	unregisterFakePeer(h.peers, empty.ID())

	h.triggerRelayFetch(hash, wantHash, "")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := h.pendingWitnessBodies.get(hash); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, ok := h.pendingWitnessBodies.get(hash); !ok {
		t.Fatal("expected the retry to succeed once a real candidate was available")
	}
}

// TestRecordWitnessWaiter_FetchTriggerRateLimitBlocksExcessTriggersFromSamePeer
// verifies the item-3 gap fix: a single peer repeatedly requesting many
// distinct not-yet-fetched (but genuinely signed) hashes cannot cause
// unbounded concurrent triggerRelayFetch goroutines. Beyond its per-peer
// burst budget, recordWitnessWaiter must still register the waiter (so an
// honest retry, or another peer, can satisfy it) but must NOT itself spawn a
// fetch.
func TestRecordWitnessWaiter_FetchTriggerRateLimitBlocksExcessTriggersFromSamePeer(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	peer, cleanup := newTestWitPeerWithReader()
	defer cleanup()

	dropsBefore := wit2FetchTriggerRateLimitDropMeter.Snapshot().Count()
	// wit2RelayFetchTriggeredMeter is marked synchronously inside
	// triggerRelayFetch, before it spawns the fetch goroutine — unlike
	// relayFetchInFlight, a fast no-candidate failure racing to clean up its
	// map entry can't make this count disappear before we read it below.
	triggeredBefore := wit2RelayFetchTriggeredMeter.Snapshot().Count()

	numHashes := int(wit2FetchTriggerBurstCap) + 5
	hashes := make([]common.Hash, numHashes)
	for i := 0; i < numHashes; i++ {
		hash := common.BigToHash(big.NewInt(int64(1000 + i)))
		hashes[i] = hash
		h.signedWitnesses.putIfNewer(wit.SignedWitnessAnnouncement{
			BlockHash:   hash,
			WitnessHash: common.BigToHash(big.NewInt(int64(2000 + i))),
			Signature:   make([]byte, wit.SignatureLength),
		})
		witHandler := (*witHandler)(h)
		witHandler.recordWitnessWaiter(hash, peer)
	}

	// Every request must still be registered as a waiter regardless of the
	// fetch-trigger budget — the rate limit only gates whether WE go fetch,
	// not whether the requester's own ask is remembered.
	for _, hash := range hashes {
		if !h.witnessWaiters.has(hash) {
			t.Fatalf("expected hash %s to be registered as a waiter even when rate-limited", hash)
		}
	}

	triggered := wit2RelayFetchTriggeredMeter.Snapshot().Count() - triggeredBefore
	if triggered > int64(wit2FetchTriggerBurstCap) {
		t.Fatalf("expected at most %d triggered fetches (the burst budget), got %d", wit2FetchTriggerBurstCap, triggered)
	}
	if triggered == 0 {
		t.Fatal("expected at least some fetches to have been allowed within budget")
	}
	if got := wit2FetchTriggerRateLimitDropMeter.Snapshot().Count() - dropsBefore; got == 0 {
		t.Fatal("expected the rate-limit drop meter to record at least one drop")
	}
}

// TestTriggerRelayFetch_GlobalConcurrencyCapBoundsSimultaneousFetches verifies
// the item found on a real devnet run: several DISTINCT requesting peers,
// each well within their own per-peer budget (wit2FetchTriggerBurstCap),
// must still be bounded by a single GLOBAL cap on how many relay-fetch
// goroutines run at once. Without this, N peers each bursting their own
// budget independently can stack into far more concurrent outbound fetches
// than any one hop needs — which is what produced real file-descriptor
// exhaustion (peer connections and the heimdall HTTP client both failing
// with "too many open files") on the devnet repro.
func TestTriggerRelayFetch_GlobalConcurrencyCapBoundsSimultaneousFetches(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	// A slow candidate: blocks until the test releases it, so in-flight
	// fetches stay "in flight" long enough to observe the cap reliably,
	// rather than racing against fetches that fail fast and free their
	// slot before the assertion runs.
	release := make(chan struct{})
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	slow := NewMockWitnessPeer(ctrl)
	slow.EXPECT().Log().Return(log.New()).AnyTimes()
	slow.EXPECT().KnownWitnessContainsHash(gomock.Any()).Return(true).AnyTimes()
	slow.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).DoAndReturn(
		func(pages []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
			go func() {
				<-release
				// A nil response (rather than relying on
				// relayFetchPageTimeout's own timeout branch) both frees
				// the semaphore slot and avoids any test dependency on
				// that shared package var — mutating it here would race
				// against any other in-flight goroutine still reading it.
				sink <- nil
			}()
			return &wit.Request{}, nil
		},
	).AnyTimes()

	slowPeer := newFakeEthPeerWithWitness(200, slow)
	registerFakePeer(h.peers, slowPeer)

	numTriggers := int(wit2RelayFetchGlobalConcurrencyCap) + 5
	hashes := make([]common.Hash, numTriggers)
	for i := 0; i < numTriggers; i++ {
		hash := common.BigToHash(big.NewInt(int64(9000 + i)))
		hashes[i] = hash
		h.signedWitnesses.putIfNewer(wit.SignedWitnessAnnouncement{
			BlockHash:   hash,
			WitnessHash: common.BigToHash(big.NewInt(int64(9500 + i))),
			Signature:   make([]byte, wit.SignatureLength),
		})
		// Each trigger comes from its own distinct peer so the per-peer
		// rate limiter (wit2FetchTriggerTracker) never itself becomes the
		// bottleneck being measured here — only the global cap should be.
		peer, cleanup := newTestWitPeerWithReader()
		defer cleanup()
		witHandler := (*witHandler)(h)
		witHandler.recordWitnessWaiter(hash, peer)
	}

	// Give the goroutines a moment to reach their blocking RequestWitness
	// call and occupy their semaphore slot.
	time.Sleep(100 * time.Millisecond)

	inFlight := 0
	for _, hash := range hashes {
		if _, ok := h.relayFetchInFlight.Load(hash); ok {
			inFlight++
		}
	}
	if inFlight > int(wit2RelayFetchGlobalConcurrencyCap) {
		t.Fatalf("expected at most %d concurrent in-flight fetches (the global cap), got %d", wit2RelayFetchGlobalConcurrencyCap, inFlight)
	}
	if inFlight == 0 {
		t.Fatal("expected at least some fetches to be in flight")
	}
	if got := wit2RelayFetchConcurrencyDropMeter.Snapshot().Count(); got == 0 {
		t.Fatal("expected the global concurrency cap to have dropped at least one excess trigger")
	}

	close(release)
	// Wait for every in-flight goroutine to actually drain its semaphore
	// slot (not just a fixed sleep) before returning: this test's deferred
	// restore of relayFetchPageTimeout, and ctrl.Finish() above, must not
	// run while a goroutine is still mid-flight reading that package var or
	// calling into the mock — either would race under -race.
	drainDeadline := time.Now().Add(2 * time.Second)
	for len(h.relayFetchSem) > 0 && time.Now().Before(drainDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(h.relayFetchSem); n > 0 {
		t.Fatalf("test cleanup: %d relay-fetch goroutines never drained", n)
	}
}

// TestTriggerRelayFetch_DroppedByCapCanBeRetriedOnceCapacityFrees closes a
// gap diffguard's mutation testing found: removing triggerRelayFetch's
// h.relayFetchInFlight.Delete(blockHash) call on the cap-drop path (the
// `default:` branch of the semaphore select) survived every existing test —
// nothing asserted on it. Left unfixed, that mutation would permanently wedge
// the dedup map: LoadOrStore would keep reporting "already in flight" for
// that hash forever, silently breaking the exact guarantee the code comment
// above promises — that a cap-dropped hash "gets a fresh chance once
// capacity frees up." This test proves that promise actually holds: a
// trigger dropped while the cap is full must be retriable, and succeed, once
// a slot opens.
func TestTriggerRelayFetch_DroppedByCapCanBeRetriedOnceCapacityFrees(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	// Saturate the cap with slow candidates that block until released,
	// exactly like TestTriggerRelayFetch_GlobalConcurrencyCapBoundsSimultaneousFetches.
	release := make(chan struct{})
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	slow := NewMockWitnessPeer(ctrl)
	slow.EXPECT().Log().Return(log.New()).AnyTimes()
	slow.EXPECT().KnownWitnessContainsHash(gomock.Any()).Return(true).AnyTimes()
	slow.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).DoAndReturn(
		func(pages []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
			go func() {
				<-release
				sink <- nil
			}()
			return &wit.Request{}, nil
		},
	).AnyTimes()
	slowPeer := newFakeEthPeerWithWitness(210, slow)
	registerFakePeer(h.peers, slowPeer)

	fillerHashes := make([]common.Hash, wit2RelayFetchGlobalConcurrencyCap)
	for i := range fillerHashes {
		fillerHashes[i] = common.BigToHash(big.NewInt(int64(30000 + i)))
		h.triggerRelayFetch(fillerHashes[i], common.Hash{}, "")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.relayFetchSem) < int(wit2RelayFetchGlobalConcurrencyCap) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(h.relayFetchSem); n != int(wit2RelayFetchGlobalConcurrencyCap) {
		t.Fatalf("test setup: expected the cap to be fully saturated (%d), got %d", wit2RelayFetchGlobalConcurrencyCap, n)
	}

	// This hash's first trigger, while the cap is full, must be dropped.
	goodData, wantHash := buildTestWitnessBytes(t)
	hash := common.HexToHash("0xd00d")
	healthy := &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1}
	healthyPeer := newFakeEthPeerWithWitness(211, healthy)
	registerFakePeer(h.peers, healthyPeer)
	healthyPeer.witPeer.Peer.AddKnownWitness(hash)

	dropsBefore := wit2RelayFetchConcurrencyDropMeter.Snapshot().Count()
	h.triggerRelayFetch(hash, wantHash, "")
	if got := wit2RelayFetchConcurrencyDropMeter.Snapshot().Count() - dropsBefore; got != 1 {
		t.Fatalf("test setup: expected the first trigger to be dropped by the full cap, got %d drops", got)
	}
	if _, inFlight := h.relayFetchInFlight.Load(hash); inFlight {
		t.Fatal("a cap-dropped trigger must clean up its in-flight entry immediately — " +
			"otherwise every future retry for this hash is permanently blocked, not just this one")
	}

	// Free every filler slot, then retry the same hash: this must now
	// succeed, proving the drop path really did leave the hash retriable.
	close(release)
	drainDeadline := time.Now().Add(2 * time.Second)
	for len(h.relayFetchSem) > 0 && time.Now().Before(drainDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(h.relayFetchSem); n > 0 {
		t.Fatalf("test cleanup: %d filler goroutines never drained", n)
	}

	h.triggerRelayFetch(hash, wantHash, "")
	retryDeadline := time.Now().Add(2 * time.Second)
	for {
		if body, _, ok := h.pendingWitnessBodies.get(hash); ok {
			if string(body) != string(goodData) {
				t.Fatal("retried fetch's cached body does not match the expected witness bytes")
			}
			return
		}
		if time.Now().After(retryDeadline) {
			t.Fatal("the retried trigger never succeeded — a cap-dropped hash was left permanently unretriable")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRecordWitnessWaiter_FetchTriggerRateLimitIsPerPeer verifies the budget
// is tracked per requesting peer, not globally: a second peer must still get
// its own fetch triggered even after a first peer has exhausted its budget.
func TestRecordWitnessWaiter_FetchTriggerRateLimitIsPerPeer(t *testing.T) {
	th := newTestHandler()
	h := th.handler

	exhaustedPeer, cleanup1 := newTestWitPeerWithReader()
	defer cleanup1()
	freshPeer, cleanup2 := newTestWitPeerWithReader()
	defer cleanup2()

	witHandler := (*witHandler)(h)

	// Exhaust the first peer's budget.
	for i := 0; i < int(wit2FetchTriggerBurstCap)+2; i++ {
		hash := common.BigToHash(big.NewInt(int64(3000 + i)))
		h.signedWitnesses.putIfNewer(wit.SignedWitnessAnnouncement{
			BlockHash:   hash,
			WitnessHash: common.BigToHash(big.NewInt(int64(4000 + i))),
			Signature:   make([]byte, wit.SignatureLength),
		})
		witHandler.recordWitnessWaiter(hash, exhaustedPeer)
	}

	// Drain the global concurrency semaphore before checking the fresh peer:
	// the exhausted peer's own burst (up to wit2FetchTriggerBurstCap, capped
	// again by wit2RelayFetchGlobalConcurrencyCap) briefly occupies every
	// global slot. Those goroutines have no real candidates registered, so
	// they fail fast and free their slot almost instantly — but "almost
	// instantly" is still a race against a synchronous check right after the
	// loop above, and this test is specifically about the PER-PEER budget,
	// not the separate global cap, so wait the global cap out first.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.relayFetchSem) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.relayFetchSem) > 0 {
		t.Fatal("test setup: global concurrency slots from the exhausted peer's burst never drained")
	}

	// A distinct hash requested by a different peer must still be allowed to
	// trigger a fetch — the per-peer budget must not be shared globally.
	// Checked via the trigger meter (marked synchronously before the fetch
	// goroutine is spawned) rather than relayFetchInFlight, since a fetch
	// that finds no real candidates (none are registered in this test)
	// completes and cleans up its map entry near-instantly — a direct map
	// check here would race against that cleanup.
	freshHash := common.HexToHash("0x5000")
	h.signedWitnesses.putIfNewer(wit.SignedWitnessAnnouncement{
		BlockHash:   freshHash,
		WitnessHash: common.HexToHash("0x6000"),
		Signature:   make([]byte, wit.SignatureLength),
	})
	triggeredBefore := wit2RelayFetchTriggeredMeter.Snapshot().Count()
	witHandler.recordWitnessWaiter(freshHash, freshPeer)

	if got := wit2RelayFetchTriggeredMeter.Snapshot().Count() - triggeredBefore; got != 1 {
		t.Fatalf("expected a fresh peer's request to trigger a fetch even though another peer's budget is exhausted, got %d triggers", got)
	}
}

// TestFetchWitnessPages_SourceDisconnectsMidFetchFallsBackToNextCandidate
// exercises the gap flagged after the paging fix landed: what happens if the
// candidate peer disconnects strictly BETWEEN pages — page 0 succeeds, then
// the peer goes away before replying to page 1. On the real wire (see
// wit/dispatcher.go's dispatcher loop) a disconnect mid-request never
// signals failure on the response channel; the dispatcher goroutine just
// returns on <-p.term. requestWitnessPage's timeout branch is therefore the
// ONLY thing that unblocks this candidate — there is no error path to catch.
// This confirms fetchAndVerifyWitness still falls back to a second, healthy
// candidate rather than hanging or giving up outright.
func TestFetchWitnessPages_SourceDisconnectsMidFetchFallsBackToNextCandidate(t *testing.T) {
	old := relayFetchPageTimeout
	relayFetchPageTimeout = 50 * time.Millisecond
	defer func() { relayFetchPageTimeout = old }()

	goodData, wantHash := buildTestWitnessBytes(t)
	// Split into 2 pages worth of data so the disconnecting candidate is
	// asked for a page 1 it never answers.
	page0 := goodData[:len(goodData)/2]
	page1 := goodData[len(goodData)/2:]

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	flaky := NewMockWitnessPeer(ctrl)
	flaky.EXPECT().Log().Return(log.New()).AnyTimes()
	// Page 0: answers normally, claiming 2 total pages.
	flaky.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).DoAndReturn(
		func(pages []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
			if pages[0].Page != 0 {
				// Page 1 (or later): simulate a mid-fetch disconnect by
				// never writing to sink. requestWitnessPage's own timeout is
				// what has to save this, exactly like a real dispatcher
				// stuck on <-p.term with no pending-request notification.
				return &wit.Request{}, nil
			}
			go func() {
				sink <- &wit.Response{Res: &wit.WitnessPacketRLPPacket{
					WitnessPacketResponse: wit.WitnessPacketResponse{
						{Hash: pages[0].Hash, Page: 0, TotalPages: 2, Data: page0},
					},
				}}
			}()
			return &wit.Request{}, nil
		},
	).AnyTimes()

	healthy := &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1}

	hash := common.HexToHash("0x30")
	candidates := []namedWitnessPeer{
		{id: "flaky-disconnects-after-page0", peer: flaky},
		{id: "healthy-fallback", peer: healthy},
	}

	start := time.Now()
	data, _, servedBy, ok := fetchAndVerifyWitness(candidates, hash, wantHash)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("expected fetchAndVerifyWitness to fall back to the healthy candidate")
	}
	if servedBy != "healthy-fallback" {
		t.Fatalf("expected the healthy candidate to serve the witness, got %q", servedBy)
	}
	if string(data) != string(goodData) {
		t.Fatal("fallback candidate's data does not match the expected witness bytes")
	}
	// Sanity: this must have actually gone through the timeout path (not a
	// fast error return) but must not have used the real 5s production
	// timeout — proves relayFetchPageTimeout is what's honored.
	if elapsed < relayFetchPageTimeout {
		t.Fatalf("expected the flaky candidate's page-1 stall to cost at least one timeout (%s), took %s", relayFetchPageTimeout, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s — relayFetchPageTimeout override was not honored (still waiting on the real 5s default?)", elapsed)
	}
	_ = page1 // only page0 is ever served by the flaky candidate in this scenario
}

// neverRespondingWitnessPeer simulates a candidate that disconnects before
// answering at all (as opposed to disconnecting strictly between pages, which
// TestFetchWitnessPages_SourceDisconnectsMidFetchFallsBackToNextCandidate
// already covers): RequestWitness returns successfully — matching a real
// dispatched request — but never writes to sink. relayFetchPageTimeout's
// timeout branch is the only thing that ever unblocks it.
type neverRespondingWitnessPeer struct{ *fakeWitnessPeer }

func (f *neverRespondingWitnessPeer) RequestWitness(reqs []wit.WitnessPageRequest, sink chan *wit.Response) (*wit.Request, error) {
	f.requested = append(f.requested, reqs[0].Page)
	return &wit.Request{}, nil
}

// TestTriggerRelayFetch_ConcurrentMultiPeerDisconnectStormDoesNotDeadlockOrLeak
// stresses the fallback logic under load rather than in isolation: many
// DIFFERENT relay fetches (different hashes) running concurrently, at exactly
// the global concurrency cap, where EACH ONE'S candidate list has multiple
// disconnecting peers ahead of the one healthy candidate — not just one
// fetch with one flaky peer. This is the scenario the single-peer disconnect
// test above can't exercise: several fetches all paying the
// relayFetchPageTimeout cost for multiple candidates at once, all competing
// for the same bounded semaphore, must still all complete, all fall back
// correctly, and leave no goroutine or semaphore slot behind.
//
// Deliberately stays AT the cap, not above it: triggerRelayFetch's
// concurrency-cap acquire (see the `select { case h.relayFetchSem <- ...:
// default: ... drop }` in triggerRelayFetch) is non-blocking and drop-on-full
// by design — a trigger arriving while the cap is saturated is simply
// abandoned, not queued, and only gets retried if some independent later
// event calls triggerRelayFetch again for that hash. Triggering above the
// cap here would make this test's own single-shot call the only chance that
// hash ever gets, which isn't a guarantee the real system makes; going above
// the cap is exactly what TestTriggerRelayFetch_GlobalConcurrencyCapBoundsSimultaneousFetches
// already covers, on the drop side.
func TestTriggerRelayFetch_ConcurrentMultiPeerDisconnectStormDoesNotDeadlockOrLeak(t *testing.T) {
	old := relayFetchPageTimeout
	relayFetchPageTimeout = 30 * time.Millisecond
	defer func() { relayFetchPageTimeout = old }()

	th := newTestHandler()
	h := th.handler

	goodData, wantHash := buildTestWitnessBytes(t)

	// Each hash gets its OWN trio of candidates — deliberately not shared
	// across hashes. fakeWitnessPeer's bookkeeping (requested, knownWitness)
	// has no lock, matching a real peer, which is never called concurrently
	// by multiple fetch goroutines the way a shared Go struct here would be;
	// sharing peer objects across concurrently-running goroutines would race
	// on that bookkeeping and fail under -race for reasons that have nothing
	// to do with triggerRelayFetch's own correctness.
	numHashes := int(wit2RelayFetchGlobalConcurrencyCap)
	hashes := make([]common.Hash, numHashes)
	for i := 0; i < numHashes; i++ {
		hash := common.BigToHash(big.NewInt(int64(20000 + i)))
		hashes[i] = hash

		disconnectA := &neverRespondingWitnessPeer{fakeWitnessPeer: &fakeWitnessPeer{}}
		disconnectB := &neverRespondingWitnessPeer{fakeWitnessPeer: &fakeWitnessPeer{}}
		healthy := &fakeWitnessPeer{pages: map[uint64][]byte{0: goodData}, totalPages: 1}

		// byte id: 3 peers per hash, capped well under 256 by
		// wit2RelayFetchGlobalConcurrencyCap's size (8 today).
		base := byte(i * 3)
		peerA := newFakeEthPeerWithWitness(base+1, disconnectA)
		peerB := newFakeEthPeerWithWitness(base+2, disconnectB)
		peerHealthy := newFakeEthPeerWithWitness(base+3, healthy)
		registerFakePeer(h.peers, peerA)
		registerFakePeer(h.peers, peerB)
		registerFakePeer(h.peers, peerHealthy)
		peerA.witPeer.Peer.AddKnownWitness(hash)
		peerB.witPeer.Peer.AddKnownWitness(hash)
		peerHealthy.witPeer.Peer.AddKnownWitness(hash)

		waiter, cleanup := newTestWitPeerWithReader()
		defer cleanup()
		h.witnessWaiters.record(hash, waiter)
	}

	// Fire every trigger only after ALL peers are fully registered and
	// known-witness-marked. peersWithWitnessCandidates scans every peer in
	// h.peers regardless of which hash is being looked up, so a fetch
	// goroutine spawned mid-setup would race the main goroutine's still-in-
	// -progress AddKnownWitness calls for a LATER hash's peers — a real
	// concurrent map read/write on those peers' knownWitness map, but purely
	// a test-setup ordering issue, not anything triggerRelayFetch itself
	// gets wrong.
	for _, hash := range hashes {
		h.triggerRelayFetch(hash, wantHash, "")
	}

	deadline := time.Now().Add(5 * time.Second)
	for _, hash := range hashes {
		for {
			if body, _, ok := h.pendingWitnessBodies.get(hash); ok {
				if string(body) != string(goodData) {
					t.Fatalf("hash %s: cached body does not match expected witness bytes", hash)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("hash %s: never got a cached body — a disconnect storm must not be able to permanently "+
					"starve or deadlock a fetch that has a healthy candidate available", hash)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Every goroutine must have actually finished (released its semaphore
	// slot and cleaned up its in-flight entry), not just reached the point of
	// caching the body — otherwise this test's own deferred restore of
	// relayFetchPageTimeout would race a goroutine still reading it.
	drainDeadline := time.Now().Add(2 * time.Second)
	for len(h.relayFetchSem) > 0 && time.Now().Before(drainDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(h.relayFetchSem); n > 0 {
		t.Fatalf("%d relay-fetch goroutines never drained their semaphore slot after the storm", n)
	}
	for _, hash := range hashes {
		if _, inFlight := h.relayFetchInFlight.Load(hash); inFlight {
			t.Fatalf("hash %s: in-flight dedup entry was never cleaned up", hash)
		}
	}
}
