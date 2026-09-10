package sequencer

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// session is one consistent stretch of the stream: a running commitment
// head, the speculative execution state, and the speculative seal hashes for
// BLOCKHASH resolution. env == nil between blocks and while skipping.
type session struct {
	applyMu   sync.Mutex
	ctx       context.Context
	consumer  *Consumer
	worker    *preconfWorker
	head      commitment.Head
	seeded    bool
	env       *blockEnv      // block currently being applied
	parked    *state.StateDB // post-state of the last speculative seal
	tip       common.Hash    // last speculatively sealed hash
	tipNumber uint64
	sealed    map[uint64]common.Hash
	verified  map[uint64]*types.Header
	tipHeader *types.Header
	reanchor  bool
}

var errPreconfReanchor = errors.New("preconf application requires canonical re-anchor")

type preconfWorker struct {
	session    *session
	env        *blockEnv
	header     *types.Header
	generation uint64
}

func newSession(consumer *Consumer) *session {
	s := &session{ctx: context.Background(), consumer: consumer}
	consumer.worker.Store(nil)
	return s
}

func (s *session) preparationSnapshot() streamPreparationState {
	if s == nil {
		return streamPreparationState{}
	}
	state := streamPreparationState{head: s.head, seeded: s.seeded}
	worker := s.consumer.worker.Load()
	if worker == nil || worker.session != s || worker.header == nil || worker.header.Number == nil {
		return state
	}
	state.blockNumber = worker.header.Number.Uint64()
	state.hasBlock = true
	state.signer = types.MakeSigner(s.consumer.chain.Config(), new(big.Int).Set(worker.header.Number), worker.header.Time)
	return state
}

func (s *session) setEnv(env *blockEnv) {
	s.env = env
	s.worker = nil
}

func (s *session) activateEnv() {
	worker := &preconfWorker{
		session:    s,
		env:        s.env,
		header:     types.CopyHeader(s.env.header),
		generation: s.env.generation,
	}
	s.worker = worker
	s.consumer.worker.Store(worker)
}

func (s *session) clearEnv() {
	worker := s.worker
	s.env = nil
	s.worker = nil
	if worker != nil {
		s.consumer.worker.CompareAndSwap(worker, nil)
	}
}

func (c *Consumer) interruptPreconfWorker(worker *preconfWorker, generation uint64) bool {
	if worker == nil || worker.env == nil || worker.generation != generation {
		return false
	}
	s, env := worker.session, worker.env
	env.interrupt.Store(true)
	if s.applyMu.TryLock() {
		if s.worker == worker && s.env == env {
			s.clearEnv()
			s.parked = nil
		}
		s.applyMu.Unlock()
	}
	return true
}

// handle verifies one entry against the commitment chain, folds it, and
// applies it best-effort. An error means the stream position itself is
// invalid; application problems skip instead (void speculative work, wait
// for a re-anchoring open).
func (s *session) handle(entry *pb.Entry) error {
	return s.handlePrepared(preparedStreamFrame{entry: entry, fold: prepareFoldAt(s.head, s.seeded, entry)})
}

func (s *session) handlePrepared(prepared preparedStreamFrame) error {
	if prepared.fold.err != nil {
		return prepared.fold.err
	}

	if prepared.fold.cold {
		// Cold start: adopt the stream's prefix as the seed; integrity from
		// here comes from the chain and the self-authenticating seals.
		s.head = prepared.fold.prefix
		s.seeded = true
	}

	s.applyMu.Lock()
	s.applyPrepared(prepared.entry, prepared.transactions)
	s.applyMu.Unlock()
	if s.reanchor {
		return errPreconfReanchor
	}
	s.head = prepared.fold.next

	return nil
}

// fold checks an entry's wire shape, computes its post-fold head over the
// running head, and returns the entry's claimed prefix alongside it.
func (s *session) fold(entry *pb.Entry) (commitment.Head, commitment.Head, error) {
	prefix := entryPrefix(entry)
	next, err := foldEntry(s.head, entry)
	if err != nil {
		return commitment.Head{}, commitment.Head{}, err
	}

	return commitment.Head(prefix), next, nil
}

func (s *session) applyPrepared(entry *pb.Entry, transactions []preparedTransaction) {
	switch kind := entry.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		s.applyOpen(kind.BlockOpen)
	case *pb.Entry_Record:
		s.applyPreparedRecord(kind.Record, transactions)
	case *pb.Entry_BlockSeal:
		s.applySeal(kind.BlockSeal)
	}
}

// skip voids the speculative work from a height upward and waits for the
// next re-anchoring open.
func (s *session) skip(from uint64, reason string, args ...any) {
	log.Warn("Preconf application skipped: "+reason, args...)
	s.consumer.invalidatePendingFrom(from)
	s.clearEnv()
	s.parked = nil
	s.tip = common.Hash{}
	s.tipNumber = 0
	s.tipHeader = nil
	s.sealed = nil
	s.verified = nil
}

func (s *session) reanchorFromCanonical(from uint64, reason string, args ...any) {
	s.skip(from, reason, args...)
	s.reanchor = true
}

// applyOpen starts a speculative block. A canonical parent is preferred as
// the base even on the happy path — it bounds how long one speculative
// StateDB lives; the parked post-seal state covers parents the chain hasn't
// imported yet.
func (s *session) applyOpen(open *pb.BlockOpen) {
	parent := common.BytesToHash(open.GetParentHash())
	number := open.GetBlockNumber()
	if s.consumer.canonicalHandoffAt(number) {
		s.skip(number, "open overtaken by canonical import", "number", number)
		return
	}

	if s.env != nil {
		// A new open while a block is still open is a producer rebuild of
		// the in-progress height; its state is unusable.
		s.skip(s.env.header.Number.Uint64(), "producer rebuilt in-progress block", "number", number)
	}

	parentHeader, cacheable, ok := s.resolveOpenParent(parent, number)
	if !ok {
		return
	}
	if err := validateOpenExecutionContext(s.consumer.chain, parentHeader, open); err != nil {
		s.skip(number, "open execution context invalid", "parent", parent, "err", err)
		return
	}

	var statedb *state.StateDB
	if cacheable {
		var err error
		statedb, err = s.consumer.chain.StateAt(parentHeader.Root)
		if err != nil {
			s.skip(number, "parent state unavailable", "parent", parent, "err", err)
			return
		}
		s.tip = parent
		s.tipNumber = parentHeader.Number.Uint64()
		s.tipHeader = parentHeader
		s.parked = nil
	} else {
		statedb = s.parked
		s.parked = nil
	}

	if cacheable {
		s.sealed = nil
		s.verified = nil
	}
	s.setEnv(newBlockEnv(s.consumer.chain, statedb, open, s.sealed))
	s.env.cacheable = cacheable
	block, payload, ok := preparePending(s.env, s.env.header, common.Hash{}, nil)
	if !ok {
		s.clearEnv()
		return
	}
	s.publishOpen(block, payload, parent, number, cacheable)
}

func (s *session) resolveOpenParent(parent common.Hash, number uint64) (*types.Header, bool, bool) {
	header := s.reconcileCanonicalParent(parent)
	if header == nil && (parent != s.tip || s.parked == nil) {
		header = s.waitForCanonicalParent(parent, number, preconfCanonicalParentWait)
	}
	if header == nil {
		header = s.reconcileCanonicalParent(parent)
	}
	if header != nil {
		if number != header.Number.Uint64()+1 {
			s.skip(number, "open height is not parent height+1", "number", number, "parent", header.Number)
			return nil, false, false
		}
		return header, true, true
	}
	if parent != s.tip || s.parked == nil {
		s.skip(number, "open parent neither canonical nor speculative tip", "parent", parent)
		return nil, false, false
	}
	if canonical := s.consumer.chain.GetCanonicalHash(s.tipNumber); canonical != (common.Hash{}) && canonical != parent {
		s.skip(number, "speculative parent lost canonical import", "parent", parent, "canonical", canonical)
		return nil, false, false
	}
	if number != s.tipNumber+1 {
		s.skip(number, "open height is not speculative parent height+1", "number", number, "parent", s.tipNumber)
		return nil, false, false
	}
	if s.tipHeader == nil {
		s.skip(number, "open parent header unavailable", "parent", parent)
		return nil, false, false
	}
	return s.tipHeader, false, true
}

func (s *session) canonicalParent(hash common.Hash) *types.Header {
	header := s.consumer.chain.GetHeaderByHash(hash)
	if header == nil || s.consumer.chain.GetCanonicalHash(header.Number.Uint64()) != hash {
		return nil
	}
	return header
}

func (s *session) waitForCanonicalParent(parent common.Hash, number uint64, wait time.Duration) *types.Header {
	head := s.consumer.chain.CurrentBlock()
	if header := s.reconcileCanonicalParent(parent); header != nil {
		return header
	}
	if head == nil || number <= head.Number.Uint64()+1 {
		return nil
	}
	handoffObserved := s.consumer.canonicalHandoffMatches(parent, number)
	blocks := make(chan core.ChainEvent, 1)
	sub := s.consumer.chain.SubscribeChainEvent(blocks)
	defer sub.Unsubscribe()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	timeout := timer.C
	if handoffObserved {
		timeout = nil
	}
	poll := time.NewTicker(preconfCanonicalParentPoll)
	defer poll.Stop()
	waitCtx := s.ctx
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	for {
		header, observed, done := s.canonicalParentWaitState(parent, number, handoffObserved)
		if done {
			return header
		}
		handoffObserved = observed
		if handoffObserved {
			timeout = nil
		}
		select {
		case <-blocks:
		case <-poll.C:
		case <-sub.Err():
			return s.reconcileCanonicalParent(parent)
		case <-waitCtx.Done():
			return nil
		case <-timeout:
			return s.reconcileCanonicalParent(parent)
		}
	}
}

func (s *session) canonicalParentWaitState(parent common.Hash, number uint64, handoffObserved bool) (*types.Header, bool, bool) {
	if header := s.reconcileCanonicalParent(parent); header != nil {
		return header, handoffObserved, true
	}
	head := s.consumer.chain.CurrentBlock()
	if head == nil || head.Number.Uint64() >= number-1 {
		return nil, handoffObserved, true
	}
	handoffMatches := s.consumer.canonicalHandoffMatches(parent, number)
	if s.consumer.canonicalHandoffAt(number-1) && !handoffMatches {
		return nil, handoffObserved, true
	}
	if handoffObserved && !handoffMatches {
		return s.reconcileCanonicalParent(parent), handoffObserved, true
	}
	return nil, handoffObserved || handoffMatches, false
}

func (s *session) reconcileCanonicalParent(hash common.Hash) *types.Header {
	header := s.canonicalParent(hash)
	if header == nil {
		return nil
	}
	head := s.consumer.chain.CurrentBlock()
	if head == nil || head.Hash() != hash || !s.consumer.ensureCanonicalHeadReconciled() {
		return nil
	}
	head = s.consumer.chain.CurrentBlock()
	if head == nil || head.Hash() != hash {
		return nil
	}
	return header
}

func (s *session) publishOpen(block *types.Block, payload pendingPayload, parent common.Hash, number uint64, cacheable bool) {
	s.consumer.publishMu.Lock()
	store := s.consumer.pendingStore()
	if !cacheable && !s.pendingOpenParentMatchesLocked(store, parent, number) {
		s.consumer.publishMu.Unlock()
		if s.retryCanonicalOpen(block, payload, parent, number) {
			return
		}
		s.skip(number, "speculative parent was reconciled", "parent", parent)
		return
	}
	invalidations, advanced := s.prepareOpenPublicationLocked(store, parent, number, cacheable)
	if advanced != nil {
		s.consumer.publishMu.Unlock()
		s.skip(number, "canonical parent advanced before open", "parent", parent, "head", advanced.Hash())
		return
	}
	s.env.generation = store.begin(number, parent, cacheable)
	if s.env.generation == 0 {
		s.consumer.publishMu.Unlock()
		store.writeInvalidations(invalidations)
		s.skip(number, "pending cache is full", "limit", pendingEntryLimit)
		return
	}
	s.publishOpenPayloadLocked(block, payload, number)
	s.consumer.publishMu.Unlock()
	store.writeInvalidations(invalidations)
}

func (s *session) pendingOpenParentMatchesLocked(store *PendingStore, parent common.Hash, number uint64) bool {
	head := s.consumer.chain.CurrentBlock()
	pending := store.PendingBlock()
	canonicalParent := head.Number.Uint64()+1 == number && head.Hash() == parent
	speculativeParent := head.Number.Uint64()+1 < number && pending != nil &&
		pending.NumberU64()+1 == number && pending.Hash() == parent
	return canonicalParent || speculativeParent
}

func (s *session) prepareOpenPublicationLocked(store *PendingStore, parent common.Hash, number uint64, cacheable bool) ([]pendingInvalidation, *types.Header) {
	s.consumer.index.ClearFrom(number + 1)
	if !cacheable {
		return nil, nil
	}
	head := s.consumer.chain.CurrentBlock()
	if head.Hash() != parent || head.Number.Uint64()+1 != number {
		return nil, head
	}
	logs, invalidations := store.invalidateFromMemory(number, "reorged")
	s.consumer.enqueuePendingLogs(logs)
	return invalidations, nil
}

func (s *session) publishOpenPayloadLocked(block *types.Block, payload pendingPayload, number uint64) {
	if s.consumer.publishPending(block, payload, s.env.generation) {
		s.consumer.index.ClearFrom(number)
		s.activateEnv()
		return
	}
	if s.detachRPCFromCanonicalLocked() || s.consumer.canonicalTransitionMatches(s.env.header) {
		s.activateEnv()
		return
	}
	s.clearExecution()
}

func (s *session) retryCanonicalOpen(block *types.Block, payload pendingPayload, parent common.Hash, number uint64) bool {
	header := s.reconcileCanonicalParent(parent)
	if header == nil {
		header = s.waitForCanonicalParent(parent, number, preconfCanonicalParentWait)
	}
	if header == nil || header.Number.Uint64()+1 != number {
		return false
	}
	s.env.cacheable = true
	s.publishOpen(block, payload, parent, number, true)
	return true
}

func (s *session) applyRecord(record *pb.Record) {
	s.applyPreparedRecord(record, nil)
}

func (s *session) applyPreparedRecord(record *pb.Record, transactions []preparedTransaction) {
	if s.env == nil || len(record.GetTransactions()) == 0 {
		return
	}
	if !s.acceptRecord(record) {
		return
	}
	s.executePreparedRecord(record, transactions)
}

func (s *session) acceptRecord(record *pb.Record) bool {
	for _, raw := range record.GetTransactions() {
		if uint64(len(raw)) > pendingInputLimit-s.env.inputBytes {
			s.skip(s.env.header.Number.Uint64(), "ordered input exceeds limit")
			return false
		}
		s.env.inputBytes += uint64(len(raw))
	}
	return true
}

func (s *session) applySeal(seal *pb.BlockSeal) {
	if s.env == nil {
		return
	}

	sealed, err := decodeSealHeader(seal.GetHeader())
	if err != nil {
		s.skip(s.env.header.Number.Uint64(), "undecodable sealed header", "err", err)

		return
	}

	assembled, reusable, verified, ok := s.sealResult(sealed)
	if !ok {
		return
	}

	sealedHash := common.Hash(commitment.SealedHash(seal.GetHeader()))
	if canonical := s.env.detachedCanonical.Load(); canonical != nil {
		canonicalHash := canonical.Hash()
		if sealedHash != canonicalHash || !s.consumer.canonicalTargetActive(sealed) {
			s.skip(s.env.header.Number.Uint64(), "detached seal differs from canonical import", "sealed", sealedHash, "canonical", canonicalHash)
			return
		}
		s.parkSeal(sealed, sealedHash, true)
		return
	}
	if !verified {
		payload, ok := preparePendingPayload(s.env, assembled, common.Hash{}, nil)
		if !ok {
			s.clearEnv()
			s.parked = nil
			return
		}
		s.publishUnverifiedSeal(assembled, payload, sealed, sealedHash)
		return
	}
	payload, ok := preparePendingPayload(s.env, assembled, sealedHash, reusable)
	if !ok {
		s.clearEnv()
		s.parked = nil
		return
	}
	s.publishSeal(assembled, payload, sealed, sealedHash)
}

func (s *session) publishUnverifiedSeal(block *types.Block, payload pendingPayload, sealed *types.Header, sealedHash common.Hash) {
	number := s.env.header.Number.Uint64()
	s.consumer.publishMu.Lock()
	if !s.consumer.publishPending(block, payload, s.env.generation) {
		detached := s.detachRPCFromCanonicalLocked()
		canonical := detachedCanonicalHash(s.env)
		s.consumer.publishMu.Unlock()
		if detached && sealedHash == canonical && s.consumer.canonicalTargetActive(sealed) {
			s.parkSeal(sealed, sealedHash, true)
		} else if detached {
			s.skip(number, "detached seal differs from canonical import", "sealed", sealedHash, "canonical", canonical)
		} else {
			s.clearEnv()
			s.parked = nil
		}
		return
	}
	logs, receipts := s.indexExecutedTransactions()
	s.consumer.enqueuePendingLogs(logs)
	s.consumer.publishMu.Unlock()
	s.consumer.receiptFeed.Send(receipts)
	s.parkSeal(sealed, sealedHash, false)
	log.Warn("Preconf seal verification deferred; retaining only an unsealed pending view", "number", number)
}

func (s *session) publishSeal(block *types.Block, payload pendingPayload, sealed *types.Header, sealedHash common.Hash) {
	number := s.env.header.Number.Uint64()
	s.consumer.publishMu.Lock()
	if !s.consumer.publishPending(block, payload, s.env.generation) {
		detached := s.detachRPCFromCanonicalLocked()
		canonical := detachedCanonicalHash(s.env)
		headTransition := s.consumer.canonicalTransitionMatches(s.env.header)
		s.consumer.publishMu.Unlock()
		if (detached && sealedHash == canonical && s.consumer.canonicalTargetActive(sealed)) ||
			(!detached && headTransition && s.consumer.canonicalTargetActive(sealed)) {
			s.parkSeal(sealed, sealedHash, true)
		} else if detached {
			s.skip(number, "detached seal differs from canonical import", "sealed", sealedHash, "canonical", canonical)
		} else {
			s.clearEnv()
			s.parked = nil
		}
		return
	}
	logs, receipts := s.indexExecutedTransactions()
	s.consumer.enqueuePendingLogs(logs)
	s.consumer.index.Seal(number, sealedHash)

	s.consumer.publishMu.Unlock()
	s.consumer.receiptFeed.Send(receipts)
	s.parkSeal(sealed, sealedHash, true)
}

func (s *session) detachRPCFromCanonicalLocked() bool {
	if s.env == nil {
		return false
	}
	canonical := s.consumer.reconciled.Load()
	if canonical == nil || !sameExecutionContext(s.env.header, canonical) || !s.consumer.canonicalTargetActive(canonical) {
		return false
	}
	s.env.detachedCanonical.Store(types.CopyHeader(canonical))
	return true
}

func detachedCanonicalHash(env *blockEnv) common.Hash {
	if env == nil {
		return common.Hash{}
	}
	header := env.detachedCanonical.Load()
	if header == nil {
		return common.Hash{}
	}
	return header.Hash()
}

func (c *Consumer) canonicalTargetActive(header *types.Header) bool {
	if header == nil || header.Number == nil {
		return false
	}
	number, hash := header.Number.Uint64(), header.Hash()
	if c.chain.GetCanonicalHash(number) == hash {
		return true
	}
	handoff := c.handoff.Load()
	return handoff != nil && handoff.Number != nil && handoff.Number.Uint64() == number && handoff.Hash() == hash
}

func (c *Consumer) canonicalTransitionMatches(header *types.Header) bool {
	if header == nil || header.Number == nil {
		return false
	}
	handoff := c.handoff.Load()
	if handoff != nil && handoff.Number != nil && header.Number.Uint64() == handoff.Number.Uint64()+1 && header.ParentHash == handoff.Hash() {
		return true
	}
	if c.chain == nil {
		return false
	}
	head, reconciled := c.chain.CurrentBlock(), c.reconciled.Load()
	return head != nil && head.Number != nil && reconciled != nil && reconciled.Number != nil &&
		head.Number.Uint64() == reconciled.Number.Uint64()+1 && header.Number.Uint64() == head.Number.Uint64()+1 && header.ParentHash == head.Hash()
}

func (s *session) parkSeal(sealed *types.Header, sealedHash common.Hash, verified bool) {
	number := s.env.header.Number.Uint64()
	if s.sealed == nil {
		s.sealed = map[uint64]common.Hash{}
	}
	s.sealed[number] = sealedHash
	if verified {
		if s.verified == nil {
			s.verified = map[uint64]*types.Header{}
		}
		s.verified[number] = types.CopyHeader(sealed)
	}
	for height := range s.sealed {
		if height+256 < number {
			delete(s.sealed, height)
			delete(s.verified, height)
		}
	}
	s.tip = sealedHash
	s.tipNumber = number
	s.tipHeader = types.CopyHeader(sealed)
	s.parked = s.env.statedb
	s.clearEnv()
}
