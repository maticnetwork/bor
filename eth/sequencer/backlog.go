package sequencer

import (
	"github.com/ethereum/go-ethereum/log"
)

// backlogOpenDepth is how far below the canonical head an open has to be
// before it counts as replayed history rather than a live rebuild. A producer
// rebuilding the tip after a rotation lands within a block or two of the head
// and must still be executed — its generation can yet win the height — while
// a restart's backlog is thousands of blocks back. Var for tests.
var backlogOpenDepth uint64 = 64

// behindCanonicalHead reports whether the chain has moved far enough past a
// height that re-executing it speculatively could serve nobody.
func (c *Consumer) behindCanonicalHead(number uint64) bool {
	head := c.chain.CurrentBlock()
	if head == nil || head.Number == nil {
		return false
	}

	return behindHead(number, head.Number.Uint64())
}

// behindHead is the depth predicate on its own, so the boundary is testable
// without a chain deep enough to reach it. The height comes off the wire, so
// the comparison subtracts rather than adds: number+depth would wrap for a
// height near the top of the range and read as backlog.
func behindHead(number, headNumber uint64) bool {
	return headNumber > backlogOpenDepth && number <= headNumber-backlogOpenDepth
}

// dropBacklogOpen voids any speculative work and ignores an open for a height
// the chain is already well past — the stream replaying history after a
// restart, while p2p import runs ahead of it. Unlike skip it leaves the
// pending store and the invalidation ledger alone: nothing was ever published
// for a height the chain already holds, so there is nothing to invalidate.
// Clearing env matters because records carry no height, so a dropped open must
// not leave the previous block open to absorb them.
func (s *session) dropBacklogOpen(number uint64) {
	s.clearEnv()
	s.parked = nil
	log.Debug("Preconf open behind canonical head", "number", number)
}
