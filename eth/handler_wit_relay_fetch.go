package eth

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// triggerRelayFetch asks some other peer for the body of a hash whose BP
// signature we already verified, purely so this node can serve/relay it —
// it does not need the bytes for its own import. Trust boundary matches
// acceptSignedBroadcast: we verify the fetched bytes against the signed
// witnessHash before caching or pushing anything. At most one fetch runs
// per hash at a time; excludePeer is skipped as a candidate since it's the
// one who just asked us (and therefore doesn't have it either).
func (h *handler) triggerRelayFetch(blockHash, wantHash common.Hash, excludePeer string) {
	if _, loaded := h.relayFetchInFlight.LoadOrStore(blockHash, struct{}{}); loaded {
		return
	}

	// Global concurrency cap, independent of the per-peer rate limiter above.
	// That limiter only bounds how often a SINGLE requesting peer can cause a
	// new trigger; it does nothing to stop N different peers from each
	// independently triggering their own burst at the same time. A relay
	// node with several downstream peers (the exact multi-hop topology this
	// fetch exists for) can otherwise rack up dozens of concurrent fetch
	// goroutines, each opening real network connections to candidate peers —
	// enough of a burst to transiently exhaust file descriptors on the node
	// (observed directly: peer connections and the heimdall HTTP client both
	// failed with "too many open files" during such a spike, and never
	// recovered even after the fd pressure passed). Acquiring non-blockingly
	// here means an over-cap trigger is simply dropped — the waiter stays
	// registered either way, so a later request for the same hash (from this
	// peer once its own rate limit refills, or from any other peer) gets a
	// fresh chance once capacity frees up.
	select {
	case h.relayFetchSem <- struct{}{}:
	default:
		h.relayFetchInFlight.Delete(blockHash)
		wit2RelayFetchConcurrencyDropMeter.Mark(1)
		log.Debug("wit2: relay fetch dropped, global concurrency cap reached", "hash", blockHash)
		return
	}

	wit2RelayFetchTriggeredMeter.Mark(1)
	go func() {
		defer func() {
			h.relayFetchInFlight.Delete(blockHash)
			<-h.relayFetchSem
		}()

		var candidates []namedWitnessPeer
		for _, src := range h.peers.peersWithWitnessCandidates(blockHash) {
			if src.witPeer == nil || src.ID() == excludePeer {
				continue
			}
			candidates = append(candidates, namedWitnessPeer{id: src.ID(), peer: src.witPeer.Peer})
		}

		data, witness, servedBy, ok := fetchAndVerifyWitness(candidates, blockHash, wantHash)
		if !ok {
			log.Info("wit2: relay fetch exhausted all candidates without success", "hash", blockHash)
			return
		}
		h.pendingWitnessBodies.put(blockHash, data, wantHash)
		h.pushWitnessToWaiters(blockHash, witness, len(data))
		log.Info("wit2: relay fetch succeeded, pushed to waiters", "hash", blockHash, "servedBy", servedBy, "bytes", len(data))
	}()
}

// namedWitnessPeer pairs a WitnessPeer with the log-friendly ID that only
// ethPeer (not the interface) exposes, so fetchAndVerifyWitness can log
// without depending on the concrete peerSet/ethPeer types — keeping it
// testable with plain fakes.
type namedWitnessPeer struct {
	id   string
	peer WitnessPeer
}

// fetchAndVerifyWitness tries each candidate in order, verifying fetched
// bytes against the already-verified signed hash before trusting them.
// Byte mismatch or decode failure on one candidate does not abort the
// whole attempt — a different candidate might have the real bytes; only a
// candidate serving bytes that don't match the signed commitment is
// distrusted, individually, exactly like acceptSignedBroadcast's model.
func fetchAndVerifyWitness(candidates []namedWitnessPeer, blockHash, wantHash common.Hash) ([]byte, *stateless.Witness, string, bool) {
	for _, c := range candidates {
		log.Info("wit2: relay fetch attempt", "hash", blockHash, "upstream", c.id)

		data, ok := fetchWitnessPages(c.peer, blockHash)
		if !ok {
			continue // this candidate came up empty/errored; try the next one
		}
		if got := stateless.WitnessCommitHash(data); got != wantHash {
			log.Warn("wit2: relay fetch byte mismatch against signed hash; dropping",
				"hash", blockHash, "peer", c.id, "expected", wantHash, "actual", got)
			continue // this peer served bad bytes; a different candidate might not
		}
		var witness stateless.Witness
		if err := rlp.DecodeBytes(data, &witness); err != nil {
			log.Debug("wit2: relay fetch failed to decode witness", "hash", blockHash, "err", err)
			continue
		}
		return data, &witness, c.id, true
	}
	return nil, nil, "", false
}

// maxRelayFetchPages bounds how many pages fetchWitnessPages will believe a
// candidate's page-0 response claiming, before any cross-page consistency
// check can happen. TotalPages is taken from the peer's own response and
// used directly to size an allocation below — left unbounded, a single
// malicious candidate reporting a huge TotalPages (or one whose value
// overflows int, going negative) turns one crafted response into either an
// out-of-memory allocation or an immediate "makeslice: cap out of range"
// panic, and the goroutine running this has no recover — crashing the whole
// node. Ties to the same overall per-witness memory budget the server side
// already enforces (MaximumCachedWitnessOnARequest / PageSize), rounded up
// by one so a witness landing exactly on the boundary isn't rejected.
var maxRelayFetchPages = uint64(MaximumCachedWitnessOnARequest/PageSize) + 1

// fetchWitnessPages pulls every page of hash's witness from peer and returns
// the concatenated bytes. Mirrors ethPeer.RequestWitnessPageCount's WIT1
// metadata request (falling back to the WIT0 page-0-probe for older peers),
// then walks pages 1..totalPages-1 the same way page 0 was fetched. A
// same-hash relay fetch this session may span more than one page — the
// PageSize cap is 15MB, and a witness can exceed that — so treating page 0
// as the whole body silently truncates anything bigger, which then fails the
// signed-hash check below and reads as a false "malicious peer" case.
func fetchWitnessPages(peer WitnessPeer, hash common.Hash) ([]byte, bool) {
	firstPage, totalPages, ok := requestWitnessPage(peer, hash, 0)
	if !ok || totalPages == 0 {
		return nil, false
	}
	if totalPages == 1 {
		return firstPage, true
	}
	if totalPages > maxRelayFetchPages {
		log.Warn("wit2: relay fetch candidate claimed an implausible page count; rejecting",
			"hash", hash, "totalPages", totalPages, "max", maxRelayFetchPages)
		return nil, false
	}
	all := make([]byte, 0, len(firstPage)*int(totalPages))
	all = append(all, firstPage...)
	for page := uint64(1); page < totalPages; page++ {
		data, pages, ok := requestWitnessPage(peer, hash, page)
		if !ok || pages != totalPages {
			return nil, false
		}
		all = append(all, data...)
	}
	return all, true
}

// relayFetchPageTimeout bounds how long requestWitnessPage waits for a
// single page's response before giving up on this candidate. A candidate
// that disconnects mid-fetch (after a request was dispatched but before the
// reply arrives) never signals failure on resCh — the dispatcher just stops
// — so this timeout, not an error return, is what lets fetchAndVerifyWitness
// move on to the next candidate. A package var (not an inline literal) so
// tests can shrink it instead of eating the full wait.
var relayFetchPageTimeout = 5 * time.Second

// requestWitnessPage fetches a single page and returns its data alongside
// the TotalPages the server reported for this hash (so the caller can detect
// a server that changes its story mid-fetch).
func requestWitnessPage(peer WitnessPeer, hash common.Hash, page uint64) ([]byte, uint64, bool) {
	resCh := make(chan *wit.Response, 1)
	req, err := peer.RequestWitness([]wit.WitnessPageRequest{{Hash: hash, Page: page}}, resCh)
	if err != nil {
		return nil, 0, false
	}
	defer req.Close()

	select {
	case res := <-resCh:
		if res == nil {
			return nil, 0, false
		}
		// The wit dispatcher (see dispatchResponse in wit/dispatcher.go)
		// blocks the peer's response-handling goroutine on res.Done until
		// the recipient signals receipt. That goroutine is the same one
		// draining the peer's per-protocol read channel — leaving it
		// blocked here head-of-line-blocks the peer's whole connection
		// (every subprotocol is demuxed off one shared TCP stream), not
		// just this witness fetch. Matches the existing correct pattern in
		// awaitWitnessResponse (eth/peer.go).
		if res.Done != nil {
			res.Done <- nil
		}
		packet, ok := res.Res.(*wit.WitnessPacketRLPPacket)
		if !ok || len(packet.WitnessPacketResponse) == 0 {
			return nil, 0, false
		}
		entry := packet.WitnessPacketResponse[0]
		if len(entry.Data) == 0 {
			return nil, 0, false // upstream doesn't have it either
		}
		return entry.Data, entry.TotalPages, true
	case <-time.After(relayFetchPageTimeout):
		return nil, 0, false
	}
}
