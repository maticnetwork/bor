// Copyright 2022 The go-ethereum Authors
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
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

// hookedBackfiller is a tester backfiller with all interface methods mocked and
// hooked so tests can implement only the things they need.
type hookedBackfiller struct {
	// suspendHook is an optional hook to be called when the filler is requested
	// to be suspended.
	suspendHook func() *types.Header

	// resumeHook is an optional hook to be called when the filler is requested
	// to be resumed.
	resumeHook func()
}

// newHookedBackfiller creates a hooked backfiller with all callbacks disabled,
// essentially acting as a noop.
func newHookedBackfiller() backfiller {
	return new(hookedBackfiller)
}

// suspend requests the backfiller to abort any running full or snap sync
// based on the skeleton chain as it might be invalid. The backfiller should
// gracefully handle multiple consecutive suspends without a resume, even
// on initial startup.
func (hf *hookedBackfiller) suspend() *types.Header {
	if hf.suspendHook != nil {
		return hf.suspendHook()
	}

	return nil // we don't really care about header cleanups for now
}

// resume requests the backfiller to start running fill or snap sync based on
// the skeleton chain as it has successfully been linked. Appending new heads
// to the end of the chain will not result in suspend/resume cycles.
func (hf *hookedBackfiller) resume() {
	if hf.resumeHook != nil {
		hf.resumeHook()
	}
}

// skeletonTestPeer is a mock peer that can only serve header requests from a
// pre-perated header chain (which may be arbitrarily wrong for testing).
//
// Requesting anything else from these peers will hard panic. Note, do *not*
// implement any other methods. We actually want to make sure that the skeleton
// syncer only depends on - and will only ever do so - on header requests.
type skeletonTestPeer struct {
	id      string          // Unique identifier of the mock peer
	headers []*types.Header // Headers to serve when requested

	serve func(origin uint64) []*types.Header // Hook to allow custom responses

	served  atomic.Uint64 // Number of headers served by this peer
	dropped atomic.Uint64 // Flag whether the peer was dropped (stop responding)
	hang    atomic.Bool
}

// newSkeletonTestPeer creates a new mock peer to test the skeleton sync with.
func newSkeletonTestPeer(id string, headers []*types.Header) *skeletonTestPeer {
	return &skeletonTestPeer{
		id:      id,
		headers: headers,
	}
}

// newSkeletonTestPeerWithHook creates a new mock peer to test the skeleton sync with,
// and sets an optional serve hook that can return headers for delivery instead
// of the predefined chain. Useful for emulating malicious behavior that would
// otherwise require dedicated peer types.
func newSkeletonTestPeerWithHook(id string, headers []*types.Header, serve func(origin uint64) []*types.Header) *skeletonTestPeer {
	return &skeletonTestPeer{
		id:      id,
		headers: headers,
		serve:   serve,
	}
}

// RequestHeadersByNumber constructs a GetBlockHeaders function based on a numbered
// origin; associated with a particular peer in the download tester. The returned
// function can be used to retrieve batches of headers from the particular peer.
func (p *skeletonTestPeer) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool, sink chan *eth.Response) (*eth.Request, error) {
	// Since skeleton test peer are in-memory mocks, dropping the does not make
	// them inaccessible. As such, check a local `dropped` field to see if the
	// peer has been dropped and should not respond any more.
	if p.dropped.Load() != 0 {
		return nil, errors.New("peer already dropped")
	}
	if p.hang.Load() {
		return &eth.Request{Peer: p.id}, nil
	}
	// Skeleton sync retrieves batches of headers going backward without gaps.
	// This ensures we can follow a clean parent progression without any reorg
	// hiccups. There is no need for any other type of header retrieval, so do
	// panic if there's such a request.
	if !reverse || skip != 0 {
		// Note, if other clients want to do these kinds of requests, it's their
		// problem, it will still work. We just don't want *us* making complicated
		// requests without a very strong reason to.
		panic(fmt.Sprintf("invalid header retrieval: reverse %v, want true; skip %d, want 0", reverse, skip))
	}
	// If the skeleton syncer requests the genesis block, panic. Whilst it could
	// be considered a valid request, our code specifically should not request it
	// ever since we want to link up headers to an existing local chain, which at
	// worse will be the genesis.
	if int64(origin)-int64(amount) < 0 {
		panic(fmt.Sprintf("headers requested before (or at) genesis: origin %d, amount %d", origin, amount))
	}
	// To make concurrency easier, the skeleton syncer always requests fixed size
	// batches of headers. Panic if the peer is requested an amount other than the
	// configured batch size (apart from the request leading to the genesis).
	if amount > requestHeaders || (amount < requestHeaders && origin > uint64(amount)) {
		panic(fmt.Sprintf("non-chunk size header batch requested: requested %d, want %d, origin %d", amount, requestHeaders, origin))
	}
	// Simple reverse header retrieval. Fill from the peer's chain and return.
	// If the tester has a serve hook set, try to use that before falling back
	// to the default behavior.
	var headers []*types.Header
	if p.serve != nil {
		headers = p.serve(origin)
	}

	if headers == nil {
		headers = make([]*types.Header, 0, amount)
		if len(p.headers) > int(origin) { // Don't serve headers if we're missing the origin
			for i := 0; i < amount; i++ {
				// Consider nil headers as a form of attack and withhold them. Nil
				// cannot be decoded from RLP, so it's not possible to produce an
				// attack by sending/receiving those over eth.
				header := p.headers[int(origin)-i]
				if header == nil {
					continue
				}

				headers = append(headers, header)
			}
		}
	}

	p.served.Add(uint64(len(headers)))

	hashes := make([]common.Hash, len(headers))
	for i, header := range headers {
		hashes[i] = header.Hash()
	}
	// Deliver the headers to the downloader
	req := &eth.Request{
		Peer: p.id,
	}
	res := &eth.Response{
		Req:  req,
		Res:  (*eth.BlockHeadersRequest)(&headers),
		Meta: hashes,
		Time: 1,
		Done: make(chan error),
	}

	go func() {
		sink <- res

		if err := <-res.Done; err != nil {
			log.Warn("Skeleton test peer response rejected", "err", err)
			p.dropped.Add(1)
		}
	}()

	return req, nil
}

func (p *skeletonTestPeer) Head() (common.Hash, *big.Int) {
	panic("skeleton sync must not request the remote head")
}

func (p *skeletonTestPeer) RequestHeadersByHash(common.Hash, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	panic("skeleton sync must not request headers by hash")
}

func (p *skeletonTestPeer) RequestBodies([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	panic("skeleton sync must not request block bodies")
}

func (p *skeletonTestPeer) RequestReceipts([]common.Hash, []uint64, []uint64, chan *eth.Response) (*eth.Request, error) {
	panic("skeleton sync must not request receipts")
}

func (p *skeletonTestPeer) RequestWitnesses([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	panic("skeleton sync must not request witnesses")
}

func (p *skeletonTestPeer) SupportsWitness() bool {
	return false
}

// Tests various sync initializations based on previous leftovers in the database
// and announced heads.
func TestSkeletonSyncInit(t *testing.T) {
	// Create a few key headers
	var (
		genesis  = &types.Header{Number: big.NewInt(0)}
		block49  = &types.Header{Number: big.NewInt(49)}
		block49B = &types.Header{Number: big.NewInt(49), Extra: []byte("B")}
		block50  = &types.Header{Number: big.NewInt(50), ParentHash: block49.Hash()}
	)

	tests := []struct {
		headers  []*types.Header // Database content (beside the genesis)
		oldstate []*subchain     // Old sync state with various interrupted subchains
		head     *types.Header   // New head header to announce to reorg to
		newstate []*subchain     // Expected sync state after the reorg
	}{
		// Completely empty database with only the genesis set. The sync is expected
		// to create a single subchain with the requested head.
		{
			head:     block50,
			newstate: []*subchain{{Head: 50, Tail: 50}},
		},
		// Empty database with only the genesis set with a leftover empty sync
		// progress. This is a synthetic case, just for the sake of covering things.
		{
			oldstate: []*subchain{},
			head:     block50,
			newstate: []*subchain{{Head: 50, Tail: 50}},
		},
		// A single leftover subchain is present, older than the new head. The
		// old subchain should be left as is and a new one appended to the sync
		// status.
		{
			oldstate: []*subchain{{Head: 10, Tail: 5}},
			head:     block50,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
				{Head: 10, Tail: 5},
			},
		},
		// Multiple leftover subchains are present, older than the new head. The
		// old subchains should be left as is and a new one appended to the sync
		// status.
		{
			oldstate: []*subchain{
				{Head: 20, Tail: 15},
				{Head: 10, Tail: 5},
			},
			head: block50,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
				{Head: 20, Tail: 15},
				{Head: 10, Tail: 5},
			},
		},
		// A single leftover subchain is present, newer than the new head. The
		// newer subchain should be deleted and a fresh one created for the head.
		{
			oldstate: []*subchain{{Head: 65, Tail: 60}},
			head:     block50,
			newstate: []*subchain{{Head: 50, Tail: 50}},
		},
		// Multiple leftover subchain is present, newer than the new head. The
		// newer subchains should be deleted and a fresh one created for the head.
		{
			oldstate: []*subchain{
				{Head: 75, Tail: 70},
				{Head: 65, Tail: 60},
			},
			head:     block50,
			newstate: []*subchain{{Head: 50, Tail: 50}},
		},

		// Two leftover subchains are present, one fully older and one fully
		// newer than the announced head. The head should delete the newer one,
		// keeping the older one.
		{
			oldstate: []*subchain{
				{Head: 65, Tail: 60},
				{Head: 10, Tail: 5},
			},
			head: block50,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
				{Head: 10, Tail: 5},
			},
		},
		// Multiple leftover subchains are present, some fully older and some
		// fully newer than the announced head. The head should delete the newer
		// ones, keeping the older ones.
		{
			oldstate: []*subchain{
				{Head: 75, Tail: 70},
				{Head: 65, Tail: 60},
				{Head: 20, Tail: 15},
				{Head: 10, Tail: 5},
			},
			head: block50,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
				{Head: 20, Tail: 15},
				{Head: 10, Tail: 5},
			},
		},
		// A single leftover subchain is present and the new head is extending
		// it with one more header. We expect the subchain head to be pushed
		// forward.
		{
			headers:  []*types.Header{block49},
			oldstate: []*subchain{{Head: 49, Tail: 5}},
			head:     block50,
			newstate: []*subchain{{Head: 50, Tail: 5}},
		},
		// A single leftover subchain is present and although the new head does
		// extend it number wise, the hash chain does not link up. We expect a
		// new subchain to be created for the dangling head.
		{
			headers:  []*types.Header{block49B},
			oldstate: []*subchain{{Head: 49, Tail: 5}},
			head:     block50,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
				{Head: 49, Tail: 5},
			},
		},
		// A single leftover subchain is present. A new head is announced that
		// links into the middle of it, correctly anchoring into an existing
		// header. We expect the old subchain to be truncated and extended with
		// the new head.
		{
			headers:  []*types.Header{block49},
			oldstate: []*subchain{{Head: 100, Tail: 5}},
			head:     block50,
			newstate: []*subchain{{Head: 50, Tail: 5}},
		},
		// A single leftover subchain is present. A new head is announced that
		// links into the middle of it, but does not anchor into an existing
		// header. We expect the old subchain to be truncated and a new chain
		// be created for the dangling head.
		{
			headers:  []*types.Header{block49B},
			oldstate: []*subchain{{Head: 100, Tail: 5}},
			head:     block50,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
				{Head: 49, Tail: 5},
			},
		},
	}
	for i, tt := range tests {
		// Create a fresh database and initialize it with the starting state
		db := rawdb.NewMemoryDatabase()

		rawdb.WriteHeader(db, genesis)

		for _, header := range tt.headers {
			rawdb.WriteSkeletonHeader(db, header)
		}

		if tt.oldstate != nil {
			blob, _ := json.Marshal(&skeletonProgress{Subchains: tt.oldstate})
			rawdb.WriteSkeletonSyncStatus(db, blob)
		}
		// Create a skeleton sync and run a cycle
		wait := make(chan struct{})

		skeleton := newSkeleton(db, newPeerSet(), nil, newHookedBackfiller())
		skeleton.syncStarting = func() { close(wait) }
		_ = skeleton.Sync(tt.head, nil, true)

		<-wait

		_ = skeleton.Terminate()

		// Ensure the correct resulting sync status
		var progress skeletonProgress

		_ = json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &progress)

		if len(progress.Subchains) != len(tt.newstate) {
			t.Errorf("test %d: subchain count mismatch: have %d, want %d", i, len(progress.Subchains), len(tt.newstate))
			continue
		}

		for j := 0; j < len(progress.Subchains); j++ {
			if progress.Subchains[j].Head != tt.newstate[j].Head {
				t.Errorf("test %d: subchain %d head mismatch: have %d, want %d", i, j, progress.Subchains[j].Head, tt.newstate[j].Head)
			}

			if progress.Subchains[j].Tail != tt.newstate[j].Tail {
				t.Errorf("test %d: subchain %d tail mismatch: have %d, want %d", i, j, progress.Subchains[j].Tail, tt.newstate[j].Tail)
			}
		}
	}
}

// Tests that a running skeleton sync can be extended with properly linked up
// headers but not with side chains.
func TestSkeletonSyncExtend(t *testing.T) {
	// Create a few key headers
	var (
		genesis  = &types.Header{Number: big.NewInt(0)}
		block49  = &types.Header{Number: big.NewInt(49)}
		block49B = &types.Header{Number: big.NewInt(49), Extra: []byte("B")}
		block50  = &types.Header{Number: big.NewInt(50), ParentHash: block49.Hash()}
		block51  = &types.Header{Number: big.NewInt(51), ParentHash: block50.Hash()}
	)

	tests := []struct {
		head     *types.Header // New head header to announce to reorg to
		extend   *types.Header // New head header to announce to extend with
		newstate []*subchain   // Expected sync state after the reorg
		err      error         // Whether extension succeeds or not
	}{
		// Initialize a sync and try to extend it with a subsequent block.
		{
			head:   block49,
			extend: block50,
			newstate: []*subchain{
				{Head: 50, Tail: 49},
			},
		},
		// Initialize a sync and try to extend it with the existing head block.
		{
			head:   block49,
			extend: block49,
			newstate: []*subchain{
				{Head: 49, Tail: 49},
			},
		},
		// Initialize a sync and try to extend it with a sibling block.
		{
			head:   block49,
			extend: block49B,
			newstate: []*subchain{
				{Head: 49, Tail: 49},
			},
			err: errChainReorged,
		},
		// Initialize a sync and try to extend it with a number-wise sequential
		// header, but a hash wise non-linking one.
		{
			head:   block49B,
			extend: block50,
			newstate: []*subchain{
				{Head: 49, Tail: 49},
			},
			err: errChainForked,
		},
		// Initialize a sync and try to extend it with a non-linking future block.
		{
			head:   block49,
			extend: block51,
			newstate: []*subchain{
				{Head: 49, Tail: 49},
			},
			err: errChainGapped,
		},
		// Initialize a sync and try to extend it with a past canonical block.
		{
			head:   block50,
			extend: block49,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
			},
			err: errChainReorged,
		},
		// Initialize a sync and try to extend it with a past sidechain block.
		{
			head:   block50,
			extend: block49B,
			newstate: []*subchain{
				{Head: 50, Tail: 50},
			},
			err: errChainReorged,
		},
	}
	for i, tt := range tests {
		// Create a fresh database and initialize it with the starting state
		db := rawdb.NewMemoryDatabase()
		rawdb.WriteHeader(db, genesis)

		// Create a skeleton sync and run a cycle
		wait := make(chan struct{})

		skeleton := newSkeleton(db, newPeerSet(), nil, newHookedBackfiller())
		skeleton.syncStarting = func() { close(wait) }
		_ = skeleton.Sync(tt.head, nil, true)

		<-wait
		if err := skeleton.Sync(tt.extend, nil, false); !errors.Is(err, tt.err) {
			t.Errorf("test %d: extension failure mismatch: have %v, want %v", i, err, tt.err)
		}

		skeleton.Terminate()

		// Ensure the correct resulting sync status
		var progress skeletonProgress

		json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &progress)

		if len(progress.Subchains) != len(tt.newstate) {
			t.Errorf("test %d: subchain count mismatch: have %d, want %d", i, len(progress.Subchains), len(tt.newstate))
			continue
		}

		for j := 0; j < len(progress.Subchains); j++ {
			if progress.Subchains[j].Head != tt.newstate[j].Head {
				t.Errorf("test %d: subchain %d head mismatch: have %d, want %d", i, j, progress.Subchains[j].Head, tt.newstate[j].Head)
			}

			if progress.Subchains[j].Tail != tt.newstate[j].Tail {
				t.Errorf("test %d: subchain %d tail mismatch: have %d, want %d", i, j, progress.Subchains[j].Tail, tt.newstate[j].Tail)
			}
		}
	}
}

func TestEarlierBackoff(t *testing.T) {
	t.Parallel()

	early := time.Unix(1000, 0)
	late := time.Unix(2000, 0)

	tests := []struct {
		name      string
		current   time.Time
		candidate time.Time
		want      time.Time
	}{
		{name: "zero candidate keeps current", current: late, candidate: time.Time{}, want: late},
		{name: "zero candidate with zero current stays zero", current: time.Time{}, candidate: time.Time{}, want: time.Time{}},
		{name: "zero current takes candidate", current: time.Time{}, candidate: late, want: late},
		{name: "earlier candidate wins", current: late, candidate: early, want: early},
		{name: "later candidate keeps current", current: early, candidate: late, want: early},
		{name: "equal keeps current", current: early, candidate: early, want: early},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := earlierBackoff(tt.current, tt.candidate); !got.Equal(tt.want) {
				t.Fatalf("earlierBackoff(%v, %v) = %v, want %v", tt.current, tt.candidate, got, tt.want)
			}
		})
	}
}

func TestSkeletonAssignTasksReportsBackoff(t *testing.T) {
	chain := []*types.Header{{Number: big.NewInt(0)}}

	peerset := newPeerSet()
	soft := newPeerConnection("soft", eth.ETH69, newSkeletonTestPeer("soft", chain), log.New("id", "soft"))
	strong := newPeerConnection("strong", eth.ETH69, newSkeletonTestPeer("strong", chain), log.New("id", "strong"))
	if err := peerset.Register(soft); err != nil {
		t.Fatalf("failed to register soft peer: %v", err)
	}
	if err := peerset.Register(strong); err != nil {
		t.Fatalf("failed to register strong peer: %v", err)
	}

	strong.backoffFor(2 * time.Minute)
	soft.backoffFor(30 * time.Second)

	skeleton := &skeleton{
		peers:         peerset,
		idles:         map[string]*peerConnection{soft.id: soft, strong.id: strong},
		scratchSpace:  make([]*types.Header, scratchHeaders),
		scratchOwners: make([]string, scratchHeaders/requestHeaders),
		requests:      make(map[uint64]*headerRequest),
	}

	success := make(chan *headerResponse, 1)
	fail := make(chan *headerRequest, 1)
	cancel := make(chan struct{})

	wake := skeleton.assignTasks(success, fail, cancel)
	if wake.IsZero() {
		t.Fatal("expected a non-zero backoff wakeup when all peers are backed off")
	}
	if until := time.Until(wake); until <= 0 || until > 30*time.Second {
		t.Fatalf("backoff wakeup mismatch: have %v remaining, want (0, %v]", until, 30*time.Second)
	}
	for _, owner := range skeleton.scratchOwners {
		if owner != "" {
			t.Fatalf("no task should be assigned to backed-off peers, got owner %q", owner)
		}
	}
}

func TestSkeletonSyncWakesAfterBackoff(t *testing.T) {
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < 2*requestHeaders+2; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	peerset := newPeerSet()
	testPeer := newSkeletonTestPeer("backed-off", chain)
	peer := newPeerConnection("backed-off", eth.ETH69, testPeer, log.New("id", "backed-off"))
	if err := peerset.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	peer.backoffFor(100 * time.Millisecond)

	skeleton := newSkeleton(db, peerset, func(string) {}, newHookedBackfiller())
	if err := skeleton.Sync(chain[len(chain)-1], nil, true); err != nil {
		t.Fatalf("failed to announce sync head: %v", err)
	}
	defer skeleton.Terminate()

	var progress skeletonProgress
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &progress)
		if len(progress.Subchains) == 1 && progress.Subchains[0].Tail == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(progress.Subchains) != 1 || progress.Subchains[0].Tail != 1 {
		t.Fatalf("skeleton did not link after backoff expiry: %+v", progress.Subchains)
	}
	if testPeer.served.Load() == 0 {
		t.Fatal("backed-off peer never served headers after backoff expiry")
	}
}

func TestSkeletonSyncBacksOffOnTimeout(t *testing.T) {
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < 2*requestHeaders+2; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	peerset := newPeerSet()
	peerset.rates.OverrideTTLLimit = 100 * time.Millisecond

	testPeer := newSkeletonTestPeer("stuck", chain)
	testPeer.hang.Store(true)
	peer := newPeerConnection("stuck", eth.ETH69, testPeer, log.New("id", "stuck"))
	if err := peerset.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	var dropped atomic.Bool
	requeued := make(chan struct{}, 1)
	skeleton := newSkeleton(db, peerset, func(string) { dropped.Store(true) }, newHookedBackfiller())
	skeleton.requestFailed = func(string) {
		select {
		case requeued <- struct{}{}:
		default:
		}
	}
	if err := skeleton.Sync(chain[len(chain)-1], nil, true); err != nil {
		t.Fatalf("failed to announce sync head: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !peer.backedOff() {
		time.Sleep(20 * time.Millisecond)
	}
	if !peer.backedOff() {
		skeleton.Terminate()
		t.Fatal("timed-out skeleton peer never backed off")
	}

	select {
	case <-requeued:
	case <-time.After(5 * time.Second):
		skeleton.Terminate()
		t.Fatal("timed-out skeleton peer was never requeued")
	}

	skeleton.Terminate()

	if !peer.backedOff() {
		t.Fatal("timed-out skeleton peer should be backed off")
	}
	if dropped.Load() {
		t.Fatal("timed-out skeleton peer should not be hard-dropped")
	}
	if peerset.persistentBackoff("stuck") <= 0 {
		t.Fatal("a timed-out skeleton peer must persist a jail across reconnects")
	}
	if _, ok := skeleton.idles["stuck"]; !ok {
		t.Fatal("a timed-out skeleton peer must be requeued into the idle set, not stranded")
	}
}

func TestSkeletonDropForInvalidHeadersBenches(t *testing.T) {
	peerset := newPeerSet()
	testPeer := newSkeletonTestPeer("junk", []*types.Header{{Number: big.NewInt(0)}})
	peer := newPeerConnection("junk", eth.ETH69, testPeer, log.New("id", "junk"))
	if err := peerset.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	var dropped atomic.Bool
	skeleton := newSkeleton(rawdb.NewMemoryDatabase(), peerset, func(string) { dropped.Store(true) }, newHookedBackfiller())
	skeleton.dropForInvalidHeaders("junk")

	if !peer.backedOff() {
		t.Fatal("a peer dropped for invalid skeleton headers must be benched")
	}
	if !dropped.Load() {
		t.Fatal("a peer dropped for invalid skeleton headers must be disconnected")
	}
	if got := peerset.persistentBackoff("junk"); got <= peerJailBackoff {
		t.Fatalf("invalid-headers drop must persist the long drop bench across reconnects: got %v, want > %v", got, peerJailBackoff)
	}
}

func TestSkeletonDropForInvalidHeadersWithoutDropper(t *testing.T) {
	peerset := newPeerSet()
	testPeer := newSkeletonTestPeer("junk", []*types.Header{{Number: big.NewInt(0)}})
	peer := newPeerConnection("junk", eth.ETH69, testPeer, log.New("id", "junk"))
	if err := peerset.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	skeleton := newSkeleton(rawdb.NewMemoryDatabase(), peerset, nil, newHookedBackfiller())
	skeleton.dropForInvalidHeaders("junk")

	if !peer.backedOff() {
		t.Fatal("invalid-headers drop must still bench the peer when no dropper is configured")
	}
}

func TestSkeletonDropForInvalidHeadersBenchesDepartedPeer(t *testing.T) {
	peerset := newPeerSet()
	skeleton := newSkeleton(rawdb.NewMemoryDatabase(), peerset, func(string) {}, newHookedBackfiller())
	skeleton.dropForInvalidHeaders("gone")

	if got := peerset.persistentBackoff("gone"); got <= peerJailBackoff {
		t.Fatalf("a departed peer that delivered invalid headers must still be benched: got %v, want > %v", got, peerJailBackoff)
	}
}

func TestSkeletonHeaderTimeoutBacksOffPeer(t *testing.T) {
	peerset := newPeerSet()
	testPeer := newSkeletonTestPeer("slow", []*types.Header{{Number: big.NewInt(0)}})
	peer := newPeerConnection("slow", eth.ETH69, testPeer, log.New("id", "slow"))
	if err := peerset.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	skeleton := newSkeleton(rawdb.NewMemoryDatabase(), peerset, func(string) {}, newHookedBackfiller())

	req := &headerRequest{
		peer:   "slow",
		stale:  make(chan struct{}),
		cancel: make(chan struct{}),
		revert: make(chan *headerRequest, 1),
	}
	skeleton.handleHeaderTimeout(peer, req, time.Second)

	if !peer.backedOff() {
		t.Fatal("a skeleton header timeout must bench the peer")
	}
}

func TestSkeletonHeaderTimeoutSkipsDepartedPeer(t *testing.T) {
	peerset := newPeerSet()
	testPeer := newSkeletonTestPeer("departed", []*types.Header{{Number: big.NewInt(0)}})
	peer := newPeerConnection("departed", eth.ETH69, testPeer, log.New("id", "departed"))
	if err := peerset.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	skeleton := newSkeleton(rawdb.NewMemoryDatabase(), peerset, func(string) {}, newHookedBackfiller())

	req := &headerRequest{
		peer:   "departed",
		stale:  make(chan struct{}),
		cancel: make(chan struct{}),
		revert: make(chan *headerRequest, 1),
	}
	close(req.stale)
	skeleton.handleHeaderTimeout(peer, req, time.Second)

	if peer.backedOff() {
		t.Fatal("a header timeout for an already-reverted request must not bench the departed peer")
	}
	if got := peerset.persistentBackoff("departed"); got != 0 {
		t.Fatalf("a header timeout for an already-reverted request must not jail the peer: got %v", got)
	}
}

func TestSkeletonHandleRequestFailRequeuesBenchedPeer(t *testing.T) {
	peerset := newPeerSet()
	benched := newPeerConnection("benched", eth.ETH69, newSkeletonTestPeer("benched", []*types.Header{{Number: big.NewInt(0)}}), log.New("id", "benched"))
	fresh := newPeerConnection("fresh", eth.ETH69, newSkeletonTestPeer("fresh", []*types.Header{{Number: big.NewInt(0)}}), log.New("id", "fresh"))
	if err := peerset.Register(benched); err != nil {
		t.Fatalf("failed to register benched peer: %v", err)
	}
	if err := peerset.Register(fresh); err != nil {
		t.Fatalf("failed to register fresh peer: %v", err)
	}
	skeleton := newSkeleton(rawdb.NewMemoryDatabase(), peerset, func(string) {}, newHookedBackfiller())
	skeleton.idles = make(map[string]*peerConnection)
	skeleton.scratchHead = requestHeaders
	skeleton.scratchOwners = make([]string, scratchHeaders/requestHeaders)

	benched.backoffFor(time.Minute)
	skeleton.handleRequestFail(&headerRequest{peer: "benched", head: requestHeaders, stale: make(chan struct{})})
	if _, ok := skeleton.idles["benched"]; !ok {
		t.Fatal("a benched peer must be requeued so the scheduler arms its wake-up timer")
	}

	skeleton.handleRequestFail(&headerRequest{peer: "fresh", head: requestHeaders, stale: make(chan struct{})})
	if _, ok := skeleton.idles["fresh"]; ok {
		t.Fatal("a non-benched failed peer must not be requeued, to avoid re-admitting a peer that delivered an unusable batch")
	}
}

func TestSkeletonInvalidHeadersBenchesPeerEndToEnd(t *testing.T) {
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < 2*requestHeaders+2; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}

	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
	rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

	bad := append([]*types.Header{}, chain...)
	corrupt := *chain[len(chain)-2]
	corrupt.Extra = []byte("corrupt skeleton header")
	bad[len(bad)-2] = &corrupt
	testPeer := newSkeletonTestPeer("duper", bad)
	peerset := newPeerSet()
	if err := peerset.Register(newPeerConnection("duper", eth.ETH69, testPeer, log.New("id", "duper"))); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	var dropped atomic.Bool
	drop := func(id string) {
		if p := peerset.Peer(id); p != nil {
			p.peer.(*skeletonTestPeer).dropped.Add(1)
		}
		dropped.Store(true)
	}
	skeleton := newSkeleton(db, peerset, drop, newHookedBackfiller())
	if err := skeleton.Sync(chain[len(chain)-1], nil, true); err != nil {
		t.Fatalf("failed to announce sync head: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !dropped.Load() {
		time.Sleep(20 * time.Millisecond)
	}

	skeleton.Terminate()

	if !dropped.Load() {
		t.Fatal("a peer delivering invalid skeleton headers must be dropped")
	}
	if got := peerset.persistentBackoff("duper"); got <= peerJailBackoff {
		t.Fatalf("the invalid-headers drop call site must persist the long drop bench: got %v, want > %v", got, peerJailBackoff)
	}
}

// Tests that the skeleton sync correctly retrieves headers from one or more
// peers without duplicates or other strange side effects.
func TestSkeletonSyncRetrievals(t *testing.T) {
	//log.Root().SetHandler(log.LvlFilterHandler(log.LvlTrace, log.StreamHandler(os.Stderr, log.TerminalFormat(true))))
	// Since skeleton headers don't need to be meaningful, beyond a parent hash
	// progression, create a long fake chain to test with.
	chain := []*types.Header{{Number: big.NewInt(0)}}
	for i := 1; i < 10000; i++ {
		chain = append(chain, &types.Header{
			ParentHash: chain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
		})
	}
	// Some tests require a forking side chain to trigger cornercases.
	sidechain := make([]*types.Header, 0, len(chain))

	for i := 0; i < len(chain)/2; i++ { // Fork at block #5000
		sidechain = append(sidechain, chain[i])
	}

	for i := len(chain) / 2; i < len(chain); i++ {
		sidechain = append(sidechain, &types.Header{
			ParentHash: sidechain[i-1].Hash(),
			Number:     big.NewInt(int64(i)),
			Extra:      []byte("B"), // force a different hash
		})
	}

	tests := []struct {
		fill          bool // Whether to run a real backfiller in this test case
		unpredictable bool // Whether to ignore drops/serves due to uncertain packet assignments

		head     *types.Header       // New head header to announce to reorg to
		peers    []*skeletonTestPeer // Initial peer set to start the sync with
		midstate []*subchain         // Expected sync state after initial cycle
		midserve uint64              // Expected number of header retrievals after initial cycle
		middrop  uint64              // Expected number of peers dropped after initial cycle

		newHead  *types.Header     // New header to anoint on top of the old one
		newPeer  *skeletonTestPeer // New peer to join the skeleton syncer
		endstate []*subchain       // Expected sync state after the post-init event
		endserve uint64            // Expected number of header retrievals after the post-init event
		enddrop  uint64            // Expected number of peers dropped after the post-init event
	}{
		// Completely empty database with only the genesis set. The sync is expected
		// to create a single subchain with the requested head. No peers however, so
		// the sync should be stuck without any progression.
		//
		// When a new peer is added, it should detect the join and fill the headers
		// to the genesis block.
		{
			head:     chain[len(chain)-1],
			midstate: []*subchain{{Head: uint64(len(chain) - 1), Tail: uint64(len(chain) - 1)}},

			newPeer:  newSkeletonTestPeer("test-peer", chain),
			endstate: []*subchain{{Head: uint64(len(chain) - 1), Tail: 1}},
			endserve: uint64(len(chain) - 2), // len - head - genesis
		},
		// Completely empty database with only the genesis set. The sync is expected
		// to create a single subchain with the requested head. With one valid peer,
		// the sync is expected to complete already in the initial round.
		//
		// Adding a second peer should not have any effect.
		{
			head:     chain[len(chain)-1],
			peers:    []*skeletonTestPeer{newSkeletonTestPeer("test-peer-1", chain)},
			midstate: []*subchain{{Head: uint64(len(chain) - 1), Tail: 1}},
			midserve: uint64(len(chain) - 2), // len - head - genesis

			newPeer:  newSkeletonTestPeer("test-peer-2", chain),
			endstate: []*subchain{{Head: uint64(len(chain) - 1), Tail: 1}},
			endserve: uint64(len(chain) - 2), // len - head - genesis
		},
		// Completely empty database with only the genesis set. The sync is expected
		// to create a single subchain with the requested head. With many valid peers,
		// the sync is expected to complete already in the initial round.
		//
		// Adding a new peer should not have any effect.
		{
			head: chain[len(chain)-1],
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("test-peer-1", chain),
				newSkeletonTestPeer("test-peer-2", chain),
				newSkeletonTestPeer("test-peer-3", chain),
			},
			midstate: []*subchain{{Head: uint64(len(chain) - 1), Tail: 1}},
			midserve: uint64(len(chain) - 2), // len - head - genesis

			newPeer:  newSkeletonTestPeer("test-peer-4", chain),
			endstate: []*subchain{{Head: uint64(len(chain) - 1), Tail: 1}},
			endserve: uint64(len(chain) - 2), // len - head - genesis
		},
		// This test checks if a peer tries to withhold a header - *on* the sync
		// boundary - instead of sending the requested amount. The malicious short
		// package should not be accepted.
		//
		// Joining with a new peer should however unblock the sync.
		{
			head: chain[requestHeaders+100],
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("header-skipper", append(append(append([]*types.Header{}, chain[:99]...), nil), chain[100:]...)),
			},
			midstate: []*subchain{{Head: requestHeaders + 100, Tail: 100}},
			midserve: requestHeaders + 101 - 3, // len - head - genesis - missing
			middrop:  1,                        // penalize shortened header deliveries

			newPeer:  newSkeletonTestPeer("good-peer", chain),
			endstate: []*subchain{{Head: requestHeaders + 100, Tail: 1}},
			endserve: (requestHeaders + 101 - 3) + (100 - 1), // midserve + lenrest - genesis
			enddrop:  1,                                      // no new drops
		},
		// This test checks if a peer tries to withhold a header - *off* the sync
		// boundary - instead of sending the requested amount. The malicious short
		// package should not be accepted.
		//
		// Joining with a new peer should however unblock the sync.
		{
			head: chain[requestHeaders+100],
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("header-skipper", append(append(append([]*types.Header{}, chain[:50]...), nil), chain[51:]...)),
			},
			midstate: []*subchain{{Head: requestHeaders + 100, Tail: 100}},
			midserve: requestHeaders + 101 - 3, // len - head - genesis - missing
			middrop:  1,                        // penalize shortened header deliveries

			newPeer:  newSkeletonTestPeer("good-peer", chain),
			endstate: []*subchain{{Head: requestHeaders + 100, Tail: 1}},
			endserve: (requestHeaders + 101 - 3) + (100 - 1), // midserve + lenrest - genesis
			enddrop:  1,                                      // no new drops
		},
		// This test checks if a peer tries to duplicate a header - *on* the sync
		// boundary - instead of sending the correct sequence. The malicious duped
		// package should not be accepted.
		//
		// Joining with a new peer should however unblock the sync.
		{
			head: chain[requestHeaders+100], // We want to force the 100th header to be a request boundary
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("header-duper", append(append(append([]*types.Header{}, chain[:99]...), chain[98]), chain[100:]...)),
			},
			midstate: []*subchain{{Head: requestHeaders + 100, Tail: 100}},
			midserve: requestHeaders + 101 - 2, // len - head - genesis
			middrop:  1,                        // penalize invalid header sequences

			newPeer:  newSkeletonTestPeer("good-peer", chain),
			endstate: []*subchain{{Head: requestHeaders + 100, Tail: 1}},
			endserve: (requestHeaders + 101 - 2) + (100 - 1), // midserve + lenrest - genesis
			enddrop:  1,                                      // no new drops
		},
		// This test checks if a peer tries to duplicate a header - *off* the sync
		// boundary - instead of sending the correct sequence. The malicious duped
		// package should not be accepted.
		//
		// Joining with a new peer should however unblock the sync.
		{
			head: chain[requestHeaders+100], // We want to force the 100th header to be a request boundary
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("header-duper", append(append(append([]*types.Header{}, chain[:50]...), chain[49]), chain[51:]...)),
			},
			midstate: []*subchain{{Head: requestHeaders + 100, Tail: 100}},
			midserve: requestHeaders + 101 - 2, // len - head - genesis
			middrop:  1,                        // penalize invalid header sequences

			newPeer:  newSkeletonTestPeer("good-peer", chain),
			endstate: []*subchain{{Head: requestHeaders + 100, Tail: 1}},
			endserve: (requestHeaders + 101 - 2) + (100 - 1), // midserve + lenrest - genesis
			enddrop:  1,                                      // no new drops
		},
		// This test checks if a peer tries to inject a different header - *on*
		// the sync boundary - instead of sending the correct sequence. The bad
		// package should not be accepted.
		//
		// Joining with a new peer should however unblock the sync.
		{
			head: chain[requestHeaders+100], // We want to force the 100th header to be a request boundary
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("header-changer",
					append(
						append(
							append([]*types.Header{}, chain[:99]...),
							&types.Header{
								ParentHash: chain[98].Hash(),
								Number:     big.NewInt(int64(99)),
								GasLimit:   1,
							},
						), chain[100:]...,
					),
				),
			},
			midstate: []*subchain{{Head: requestHeaders + 100, Tail: 100}},
			midserve: requestHeaders + 101 - 2, // len - head - genesis
			middrop:  1,                        // different set of headers, drop // TODO(karalabe): maybe just diff sync?

			newPeer:  newSkeletonTestPeer("good-peer", chain),
			endstate: []*subchain{{Head: requestHeaders + 100, Tail: 1}},
			endserve: (requestHeaders + 101 - 2) + (100 - 1), // midserve + lenrest - genesis
			enddrop:  1,                                      // no new drops
		},
		// This test checks if a peer tries to inject a different header - *off*
		// the sync boundary - instead of sending the correct sequence. The bad
		// package should not be accepted.
		//
		// Joining with a new peer should however unblock the sync.
		{
			head: chain[requestHeaders+100], // We want to force the 100th header to be a request boundary
			peers: []*skeletonTestPeer{
				newSkeletonTestPeer("header-changer",
					append(
						append(
							append([]*types.Header{}, chain[:50]...),
							&types.Header{
								ParentHash: chain[49].Hash(),
								Number:     big.NewInt(int64(50)),
								GasLimit:   1,
							},
						), chain[51:]...,
					),
				),
			},
			midstate: []*subchain{{Head: requestHeaders + 100, Tail: 100}},
			midserve: requestHeaders + 101 - 2, // len - head - genesis
			middrop:  1,                        // different set of headers, drop

			newPeer:  newSkeletonTestPeer("good-peer", chain),
			endstate: []*subchain{{Head: requestHeaders + 100, Tail: 1}},
			endserve: (requestHeaders + 101 - 2) + (100 - 1), // midserve + lenrest - genesis
			enddrop:  1,                                      // no new drops
		},
		// This test reproduces a bug caught during review (kudos to @holiman)
		// where a subchain is merged with a previously interrupted one, causing
		// pending data in the scratch space to become "invalid" (since we jump
		// ahead during subchain merge). In that case it is expected to ignore
		// the queued up data instead of trying to process on top of a shifted
		// task set.
		//
		// The test is a bit convoluted since it needs to trigger a concurrency
		// issue. First we sync up an initial chain of 2x512 items. Then announce
		// 2x512+2 as head and delay delivering the head batch to fill the scratch
		// space first. The delivery head should merge with the previous download
		// and the scratch space must not be consumed further.
		{
			head: chain[2*requestHeaders],
			peers: []*skeletonTestPeer{
				newSkeletonTestPeerWithHook("peer-1", chain, func(origin uint64) []*types.Header {
					if origin == chain[2*requestHeaders+1].Number.Uint64() {
						time.Sleep(100 * time.Millisecond)
					}
					return nil // Fallback to default behavior, just delayed
				}),
				newSkeletonTestPeerWithHook("peer-2", chain, func(origin uint64) []*types.Header {
					if origin == chain[2*requestHeaders+1].Number.Uint64() {
						time.Sleep(100 * time.Millisecond)
					}
					return nil // Fallback to default behavior, just delayed
				}),
			},
			midstate: []*subchain{{Head: 2 * requestHeaders, Tail: 1}},
			midserve: 2*requestHeaders - 1, // len - head - genesis

			newHead:  chain[2*requestHeaders+2],
			endstate: []*subchain{{Head: 2*requestHeaders + 2, Tail: 1}},
			endserve: 4 * requestHeaders,
		},
		// This test reproduces a bug caught by (@rjl493456442) where a skeleton
		// header goes missing, causing the sync to get stuck and/or panic.
		//
		// The setup requires a previously successfully synced chain up to a block
		// height N. That results is a single skeleton header (block N) and a single
		// subchain (head N, Tail N) being stored on disk.
		//
		// The following step requires a new sync cycle to a new side chain of a
		// height higher than N, and an ancestor lower than N (e.g. N-2, N+2).
		// In this scenario, when processing a batch of headers, a link point of
		// N-2 will be found, meaning that N-1 and N have been overwritten.
		//
		// The link event triggers an early exit, noticing that the previous sub-
		// chain is a leftover and deletes it (with it's skeleton header N). But
		// since skeleton header N has been overwritten to the new side chain, we
		// end up losing it and creating a gap.
		{
			fill:          true,
			unpredictable: true, // We have good and bad peer too, bad may be dropped, test too short for certainty

			head:     chain[len(chain)/2+1], // Sync up until the sidechain common ancestor + 2
			peers:    []*skeletonTestPeer{newSkeletonTestPeer("test-peer-oldchain", chain)},
			midstate: []*subchain{{Head: uint64(len(chain)/2 + 1), Tail: 1}},

			newHead:  sidechain[len(sidechain)/2+3], // Sync up until the sidechain common ancestor + 4
			newPeer:  newSkeletonTestPeer("test-peer-newchain", sidechain),
			endstate: []*subchain{{Head: uint64(len(sidechain)/2 + 3), Tail: uint64(len(chain) / 2)}},
		},
	}
	for i, tt := range tests {
		// Create a fresh database and initialize it with the starting state
		db := rawdb.NewMemoryDatabase()

		rawdb.WriteBlock(db, types.NewBlockWithHeader(chain[0]))
		rawdb.WriteReceipts(db, chain[0].Hash(), chain[0].Number.Uint64(), types.Receipts{})

		// Create a peer set to feed headers through
		peerset := newPeerSet()
		for _, peer := range tt.peers {
			peerset.Register(newPeerConnection(peer.id, eth.ETH69, peer, log.New("id", peer.id)))
		}
		// Create a peer dropper to track malicious peers
		dropped := make(map[string]int)
		drop := func(peer string) {
			if p := peerset.Peer(peer); p != nil {
				p.peer.(*skeletonTestPeer).dropped.Add(1)
			}

			peerset.Unregister(peer)

			dropped[peer]++
		}
		// Create a backfiller if we need to run more advanced tests
		filler := newHookedBackfiller()

		if tt.fill {
			var filled *types.Header

			filler = &hookedBackfiller{
				resumeHook: func() {
					var progress skeletonProgress
					_ = json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &progress)

					for progress.Subchains[0].Tail < progress.Subchains[0].Head {
						header := rawdb.ReadSkeletonHeader(db, progress.Subchains[0].Tail)

						rawdb.WriteBlock(db, types.NewBlockWithHeader(header))
						rawdb.WriteReceipts(db, header.Hash(), header.Number.Uint64(), types.Receipts{})

						rawdb.DeleteSkeletonHeader(db, header.Number.Uint64())

						progress.Subchains[0].Tail++
						progress.Subchains[0].Next = header.Hash()
					}
					filled = rawdb.ReadSkeletonHeader(db, progress.Subchains[0].Tail)

					rawdb.WriteBlock(db, types.NewBlockWithHeader(filled))
					rawdb.WriteReceipts(db, filled.Hash(), filled.Number.Uint64(), types.Receipts{})
				},

				suspendHook: func() *types.Header {
					prev := filled
					filled = nil

					return prev
				},
			}
		}
		// Create a skeleton sync and run a cycle
		skeleton := newSkeleton(db, peerset, drop, filler)
		_ = skeleton.Sync(tt.head, nil, true)

		var progress skeletonProgress
		// Wait a bit (bleah) for the initial sync loop to go to idle. This might
		// be either a finish or a never-start hence why there's no event to hook.
		check := func() error {
			if len(progress.Subchains) != len(tt.midstate) {
				return fmt.Errorf("test %d, mid state: subchain count mismatch: have %d, want %d", i, len(progress.Subchains), len(tt.midstate))
			}

			for j := 0; j < len(progress.Subchains); j++ {
				if progress.Subchains[j].Head != tt.midstate[j].Head {
					return fmt.Errorf("test %d, mid state: subchain %d head mismatch: have %d, want %d", i, j, progress.Subchains[j].Head, tt.midstate[j].Head)
				}

				if progress.Subchains[j].Tail != tt.midstate[j].Tail {
					return fmt.Errorf("test %d, mid state: subchain %d tail mismatch: have %d, want %d", i, j, progress.Subchains[j].Tail, tt.midstate[j].Tail)
				}
			}

			return nil
		}

		// Wait for the sync loop to settle: both subchain state AND the served
		// header counter must converge. Under -race the background serving
		// goroutines trail the subchain update, so reading served the instant
		// subchain matches can observe a partial count (seen as a 1/20 flake
		// on CI). Polling until served reaches the expected total removes the
		// window. 10s budget tolerates -race overhead.
		midserved := func() uint64 {
			var s uint64
			for _, peer := range tt.peers {
				s += peer.served.Load()
			}
			return s
		}
		waitStart := time.Now()
		for waitTime := 20 * time.Millisecond; time.Since(waitStart) < 30*time.Second; waitTime = waitTime * 2 {
			time.Sleep(min(waitTime, 500*time.Millisecond))
			json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &progress)
			if err := check(); err != nil {
				continue
			}
			if tt.unpredictable || midserved() >= tt.midserve {
				break
			}
		}

		if err := check(); err != nil {
			t.Error(err)
			continue
		}

		if !tt.unpredictable {
			if served := midserved(); served != tt.midserve {
				t.Errorf("test %d, mid state: served headers mismatch: have %d, want %d", i, served, tt.midserve)
			}

			// Wait for expected peer drops (may happen asynchronously after subchain state updates)
			var drops uint64
			waitStart = time.Now()
			for waitTime := 20 * time.Millisecond; time.Since(waitStart) < 30*time.Second; waitTime = waitTime * 2 {
				drops = 0
				for _, peer := range tt.peers {
					drops += peer.dropped.Load()
				}
				if drops >= tt.middrop {
					break
				}
				time.Sleep(min(waitTime, 500*time.Millisecond))
			}

			if drops != tt.middrop {
				t.Errorf("test %d, mid state: dropped peers mismatch: have %d, want %d", i, drops, tt.middrop)
			}
		}
		// Apply the post-init events if there's any
		if tt.newHead != nil {
			_ = skeleton.Sync(tt.newHead, nil, true)
		}

		if tt.newPeer != nil {
			if err := peerset.Register(newPeerConnection(tt.newPeer.id, eth.ETH69, tt.newPeer, log.New("id", tt.newPeer.id))); err != nil {
				t.Errorf("test %d: failed to register new peer: %v", i, err)
			}
		}
		// Wait a bit (bleah) for the second sync loop to go to idle. This might
		// be either a finish or a never-start hence why there's no event to hook.
		check = func() error {
			if len(progress.Subchains) != len(tt.endstate) {
				return fmt.Errorf("test %d, end state: subchain count mismatch: have %d, want %d", i, len(progress.Subchains), len(tt.endstate))
			}

			for j := 0; j < len(progress.Subchains); j++ {
				if progress.Subchains[j].Head != tt.endstate[j].Head {
					return fmt.Errorf("test %d, end state: subchain %d head mismatch: have %d, want %d", i, j, progress.Subchains[j].Head, tt.endstate[j].Head)
				}

				if progress.Subchains[j].Tail != tt.endstate[j].Tail {
					return fmt.Errorf("test %d, end state: subchain %d tail mismatch: have %d, want %d", i, j, progress.Subchains[j].Tail, tt.endstate[j].Tail)
				}
			}

			return nil
		}
		// Same polling shape as the mid-state check above: wait for both
		// subchain state and total served count to settle before asserting.
		endserved := func() uint64 {
			var s uint64
			for _, peer := range tt.peers {
				s += peer.served.Load()
			}
			if tt.newPeer != nil {
				s += tt.newPeer.served.Load()
			}
			return s
		}
		waitStart = time.Now()

		for waitTime := 20 * time.Millisecond; time.Since(waitStart) < 30*time.Second; waitTime = waitTime * 2 {
			time.Sleep(min(waitTime, 500*time.Millisecond))
			json.Unmarshal(rawdb.ReadSkeletonSyncStatus(db), &progress)
			if err := check(); err != nil {
				continue
			}
			if tt.unpredictable || endserved() >= tt.endserve {
				break
			}
		}

		if err := check(); err != nil {
			t.Error(err)
			continue
		}
		// Check that the peers served no more headers than we actually needed
		if !tt.unpredictable {
			if served := endserved(); served != tt.endserve {
				t.Errorf("test %d, end state: served headers mismatch: have %d, want %d", i, served, tt.endserve)
			}

			// Wait for expected peer drops (may happen asynchronously after subchain state updates)
			var drops uint64
			waitStart = time.Now()
			for waitTime := 20 * time.Millisecond; time.Since(waitStart) < 30*time.Second; waitTime = waitTime * 2 {
				drops = 0
				for _, peer := range tt.peers {
					drops += peer.dropped.Load()
				}
				if tt.newPeer != nil {
					drops += tt.newPeer.dropped.Load()
				}
				if drops >= tt.enddrop {
					break
				}
				time.Sleep(min(waitTime, 500*time.Millisecond))
			}

			if drops != tt.enddrop {
				t.Errorf("test %d, end state: dropped peers mismatch: have %d, want %d", i, drops, tt.enddrop)
			}
		}
		// Clean up any leftover skeleton sync resources
		skeleton.Terminate()
	}
}
