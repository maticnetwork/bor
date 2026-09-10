package sequencer

import (
	"context"
	"io"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

func wireTransaction(t *testing.T, nonce uint64) (*types.Transaction, []byte) {
	t.Helper()
	to := common.Address{0x01}
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(1),
		Gas:      21_000,
		To:       &to,
		Value:    big.NewInt(1),
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	return tx, raw
}

func TestPrepareTransactionUsesIndependentWireDecode(t *testing.T) {
	want, raw := wireTransaction(t, 1)
	prepared := (&Consumer{}).prepareTransaction(raw, nil)
	if prepared.err != nil {
		t.Fatalf("prepare transaction: %v", prepared.err)
	}
	if prepared.tx == want {
		t.Fatal("preparation reused a transaction object owned by another subsystem")
	}
	if prepared.tx.Hash() != want.Hash() {
		t.Fatalf("decoded hash %s, want %s", prepared.tx.Hash(), want.Hash())
	}

	malformed := (&Consumer{}).prepareTransaction([]byte{0xff}, nil)
	if malformed.err == nil || malformed.tx != nil {
		t.Fatal("malformed wire transaction must retain its decode error")
	}
}

func TestPrepareTransactionsPreservesOrder(t *testing.T) {
	const blockNumber = 7
	raw := make([][]byte, 3)
	want := make([]*types.Transaction, 3)
	for index := range want {
		want[index], raw[index] = wireTransaction(t, uint64(index))
	}
	consumer := &Consumer{}
	header := &types.Header{Number: big.NewInt(blockNumber)}
	consumer.worker.Store(&preconfWorker{env: &blockEnv{header: header}, header: types.CopyHeader(header)})

	prepared, ok := consumer.prepareTransactions(context.Background(), raw, nil, blockNumber)
	if !ok {
		t.Fatal("transaction preparation was canceled")
	}
	for index := range want {
		if prepared[index].err != nil || prepared[index].tx == want[index] || prepared[index].tx.Hash() != want[index].Hash() {
			t.Fatalf("prepared transaction %d did not preserve wire order", index)
		}
	}
}

func TestPrepareTransactionsStopsWhenCanceled(t *testing.T) {
	const blockNumber = 7
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, raw := wireTransaction(t, 1)
	consumer := &Consumer{}
	header := &types.Header{Number: big.NewInt(blockNumber)}
	consumer.worker.Store(&preconfWorker{env: &blockEnv{header: header}, header: types.CopyHeader(header)})

	if _, ok := consumer.prepareTransactions(ctx, [][]byte{raw, raw}, nil, blockNumber); ok {
		t.Fatal("transaction preparation continued after cancellation")
	}
}

func TestPrepareTransactionsStopsForInterruptedWorker(t *testing.T) {
	const blockNumber = 7
	_, raw := wireTransaction(t, 1)
	env := &blockEnv{header: &types.Header{Number: big.NewInt(blockNumber)}}
	env.interrupt.Store(true)
	consumer := &Consumer{}
	consumer.worker.Store(&preconfWorker{env: env, header: types.CopyHeader(env.header)})

	prepared, ok := consumer.prepareTransactions(context.Background(), [][]byte{raw, raw}, nil, blockNumber)
	if !ok || prepared != nil {
		t.Fatalf("interrupted preparation = %+v, %v", prepared, ok)
	}
}

type countingSigner struct {
	types.Signer
	calls atomic.Int32
}

func (s *countingSigner) Sender(tx *types.Transaction) (common.Address, error) {
	s.calls.Add(1)
	return s.Signer.Sender(tx)
}

func (s *countingSigner) Equal(other types.Signer) bool {
	if counted, ok := other.(*countingSigner); ok {
		return s.Signer.Equal(counted.Signer)
	}
	return s.Signer.Equal(other)
}

type staticTransactionLookup struct {
	tx        *types.Transaction
	requested common.Hash
}

func (l *staticTransactionLookup) Get(hash common.Hash) *types.Transaction {
	l.requested = hash
	return l.tx
}

func TestPrepareTransactionPrewarmsSender(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	config := &params.ChainConfig{
		ChainID:        big.NewInt(1),
		HomesteadBlock: big.NewInt(0),
		EIP155Block:    big.NewInt(0),
	}
	blockNumber := big.NewInt(10)
	base := types.MakeSigner(config, blockNumber, 100)
	tx, err := types.SignNewTx(key, base, &types.LegacyTx{GasPrice: big.NewInt(1), Gas: 21_000})
	if err != nil {
		t.Fatalf("sign transaction: %v", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	preparedSigner := &countingSigner{Signer: types.MakeSigner(config, new(big.Int).Set(blockNumber), 100)}
	prepared := (&Consumer{}).prepareTransaction(raw, preparedSigner)
	if prepared.err != nil {
		t.Fatalf("prepare transaction: %v", prepared.err)
	}
	if preparedSigner.calls.Load() != 1 {
		t.Fatalf("sender recovery calls %d, want 1", preparedSigner.calls.Load())
	}
	applySigner := &countingSigner{Signer: types.MakeSigner(config, new(big.Int).Set(blockNumber), 100)}
	if _, err := types.Sender(applySigner, prepared.tx); err != nil {
		t.Fatalf("cached sender: %v", err)
	}
	if applySigner.calls.Load() != 0 {
		t.Fatal("equivalent apply-time signer recovered the sender instead of using the prepared cache")
	}
}

func TestPrepareTransactionReusesPoolSenderForIndependentDecode(t *testing.T) {
	h := startExecHarness(t)
	sess := h.session()
	handleOK(t, sess, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x41}))

	pooled := h.transfer(t, 0)
	raw, err := pooled.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	signer := types.MakeSigner(h.config, sess.env.header.Number, sess.env.header.Time)
	seedSigner := &countingSigner{Signer: signer}
	wantSender, err := types.Sender(seedSigner, pooled)
	if err != nil {
		t.Fatalf("seed pooled sender: %v", err)
	}
	if seedSigner.calls.Load() != 1 {
		t.Fatalf("initial sender recovery calls %d, want 1", seedSigner.calls.Load())
	}

	lookup := &staticTransactionLookup{tx: pooled}
	sess.consumer.txLookup = lookup
	prepareSigner := &countingSigner{Signer: signer}
	prepared := sess.consumer.prepareTransaction(raw, prepareSigner)
	if prepared.err != nil {
		t.Fatalf("prepare transaction: %v", prepared.err)
	}
	if lookup.requested != pooled.Hash() {
		t.Fatalf("lookup hash %s, want %s", lookup.requested, pooled.Hash())
	}
	if prepared.tx == pooled {
		t.Fatal("stream preparation reused the pool-owned transaction")
	}
	if !prepared.senderVerified || prepared.sender != wantSender {
		t.Fatalf("prepared sender = %s, verified=%t, want %s", prepared.sender, prepared.senderVerified, wantSender)
	}
	if prepareSigner.calls.Load() != 0 {
		t.Fatalf("pool cache reuse recovered sender %d times", prepareSigner.calls.Load())
	}

	record := recordEntry(raw, sess.head)
	frame := preparedStreamFrame{
		entry:        record,
		transactions: []preparedTransaction{prepared},
		fold:         prepareFoldAt(sess.head, sess.seeded, record),
	}
	if err := sess.handlePrepared(frame); err != nil {
		t.Fatalf("apply prepared record: %v", err)
	}
	if len(sess.env.txs) != 1 || sess.env.txs[0] != prepared.tx {
		t.Fatal("execution did not retain the independently decoded stream transaction")
	}
	receipt, _, ok := sess.consumer.index.Lookup(pooled.Hash())
	if !ok || receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("prepared transaction receipt = %+v, ok=%t", receipt, ok)
	}
}

func TestPrepareTransactionIgnoresMismatchedPoolResult(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	config := &params.ChainConfig{ChainID: big.NewInt(1), HomesteadBlock: big.NewInt(0), EIP155Block: big.NewInt(0)}
	signer := types.MakeSigner(config, big.NewInt(10), 100)
	wanted, err := types.SignNewTx(key, signer, &types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21_000})
	if err != nil {
		t.Fatalf("sign wanted transaction: %v", err)
	}
	other, err := types.SignNewTx(key, signer, &types.LegacyTx{Nonce: 2, GasPrice: big.NewInt(1), Gas: 21_000})
	if err != nil {
		t.Fatalf("sign other transaction: %v", err)
	}
	raw, err := wanted.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal wanted transaction: %v", err)
	}
	lookup := &staticTransactionLookup{tx: other}
	counted := &countingSigner{Signer: signer}
	prepared := (&Consumer{txLookup: lookup}).prepareTransaction(raw, counted)
	if prepared.err != nil {
		t.Fatalf("prepare transaction: %v", prepared.err)
	}
	wantSender := crypto.PubkeyToAddress(key.PublicKey)
	if !prepared.senderVerified || prepared.sender != wantSender {
		t.Fatalf("fallback sender = %s, verified=%t, want %s", prepared.sender, prepared.senderVerified, wantSender)
	}
	if counted.calls.Load() != 1 {
		t.Fatalf("stream sender recovery calls %d, want 1", counted.calls.Load())
	}
}

func TestPrepareTransactionRejectsInvalidSender(t *testing.T) {
	_, raw := wireTransaction(t, 1)
	signer := types.NewEIP155Signer(big.NewInt(1))
	prepared := (&Consumer{}).prepareTransaction(raw, signer)
	if prepared.err == nil || prepared.tx == nil || prepared.senderVerified {
		t.Fatalf("invalid sender preparation = %+v", prepared)
	}
}

type sliceStreamReceiver struct {
	frames []*pb.StreamResponse
	calls  chan int
	index  int
}

func (s *sliceStreamReceiver) Recv() (*pb.StreamResponse, error) {
	s.index++
	if s.calls != nil {
		s.calls <- s.index
	}
	if s.index > len(s.frames) {
		return nil, io.EOF
	}
	return s.frames[s.index-1], nil
}

func TestPrepareStreamKeepsOneFrameOfLookahead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &sliceStreamReceiver{
		frames: []*pb.StreamResponse{{}, {}, {}},
		calls:  make(chan int, 3),
	}
	out := make(chan preparedStreamFrame)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Consumer{}).prepareStream(ctx, stream, streamPreparationState{}, out)
	}()

	if call := <-stream.calls; call != 1 {
		t.Fatalf("first receive call %d", call)
	}
	<-out
	if call := <-stream.calls; call != 2 {
		t.Fatalf("lookahead receive call %d", call)
	}
	select {
	case call := <-stream.calls:
		t.Fatalf("received frame %d while one prepared frame was still in flight", call)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream preparer did not stop")
	}
}

func TestPrepareStreamWaitsForOpenBeforePreparingFirstRecord(t *testing.T) {
	h := startExecHarness(t)
	sess := h.session()
	open := openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x32})
	tx := h.transfer(t, 0)
	openFold := prepareFoldAt(commitment.Head{}, false, open)
	if openFold.err != nil {
		t.Fatalf("fold open: %v", openFold.err)
	}
	record := recordEntry(marshalTransaction(t, tx), openFold.next)
	stream := &sliceStreamReceiver{frames: []*pb.StreamResponse{
		{Frame: &pb.StreamResponse_Entry{Entry: open}},
		{Frame: &pb.StreamResponse_Entry{Entry: record}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan preparedStreamFrame)
	go sess.consumer.prepareStream(ctx, stream, sess.preparationSnapshot(), out)

	openFrame := <-out
	if openFrame.openApplied == nil {
		t.Fatal("block open did not request an apply acknowledgement")
	}
	if err := sess.handlePrepared(openFrame); err != nil {
		t.Fatalf("apply open: %v", err)
	}
	close(openFrame.openApplied)
	recordFrame := <-out
	if len(recordFrame.transactions) != 1 || recordFrame.transactions[0].err != nil || recordFrame.transactions[0].tx == nil {
		t.Fatalf("first record was not prepared after open: %+v", recordFrame.transactions)
	}
}

func TestInactiveStreamPreparationFallsBackToWireDecode(t *testing.T) {
	h := startExecHarness(t)
	sess := h.session()
	open := openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x31})
	handleOK(t, sess, open)
	number := sess.env.header.Number.Uint64()
	tx := h.transfer(t, 0)
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	record := recordEntry(raw, sess.head)
	sess.skip(number, "test re-anchor")
	prepared, ok := sess.consumer.prepareTransactions(context.Background(), [][]byte{raw}, nil, number)
	if !ok || prepared != nil {
		t.Fatalf("inactive preparation = %+v, %v", prepared, ok)
	}
	sess.applyOpen(open.GetBlockOpen())
	if sess.env == nil || !sess.consumer.activeWorkerAt(number) {
		t.Fatal("stream block did not become executable after re-anchor")
	}
	frame := preparedStreamFrame{
		entry:        record,
		transactions: prepared,
		fold:         prepareFoldAt(sess.head, sess.seeded, record),
	}
	if err := sess.handlePrepared(frame); err != nil {
		t.Fatalf("apply unprepared record: %v", err)
	}
	if len(sess.env.txs) != 1 || sess.env.txs[0].Hash() != tx.Hash() {
		t.Fatal("ordered apply did not fall back to the wire transaction")
	}
}

func TestConsumePreservesPreparedTransactionOrderAndRecvError(t *testing.T) {
	seed := [32]byte{0x01}
	raw := make([][]byte, 3)
	for index := range raw {
		_, encoded := wireTransaction(t, uint64(index))
		raw[index] = encoded
	}
	consumer := &Consumer{index: NewIndex()}
	sess := newSession(consumer)
	sess.head = seed
	sess.seeded = true
	entry := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     raw,
		PrefixCommitment: seed[:],
	}}}
	stream := &sliceStreamReceiver{frames: []*pb.StreamResponse{{Frame: &pb.StreamResponse_Entry{Entry: entry}}}}
	ctx, cancel := context.WithCancel(context.Background())

	got, err := consumer.consume(ctx, cancel, stream, sess)
	if err == nil || err.Error() != "stream recv: EOF" {
		t.Fatalf("consume error %v, want wrapped EOF", err)
	}
	if got != sess {
		t.Fatal("receive error must preserve the warm session")
	}
	wantHead := commitment.FoldTxs(seed, raw)
	if sess.head != wantHead {
		t.Fatalf("session head %x, want %x", sess.head[:8], wantHead[:8])
	}
}

type blockingStreamReceiver struct {
	ctx      context.Context
	started  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (s *blockingStreamReceiver) Recv() (*pb.StreamResponse, error) {
	s.once.Do(func() { close(s.started) })
	<-s.ctx.Done()
	close(s.returned)
	return nil, s.ctx.Err()
}

func TestConsumeCancellationStopsStreamPreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &blockingStreamReceiver{
		ctx:      ctx,
		started:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	consumer := &Consumer{index: NewIndex()}
	sess := newSession(consumer)
	done := make(chan error, 1)
	go func() {
		_, err := consumer.consume(ctx, cancel, stream, sess)
		done <- err
	}()

	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("stream preparation did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled consume returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("consume did not stop after cancellation")
	}
	select {
	case <-stream.returned:
	case <-time.After(time.Second):
		t.Fatal("stream receive goroutine did not stop")
	}
}

func TestPreparationSnapshotAndImportInterruptAreRaceFree(t *testing.T) {
	ex := startExecHarness(t)
	consumer := &Consumer{chain: ex.chain, index: NewIndex()}
	number := new(big.Int).Add(ex.chain.CurrentBlock().Number, big.NewInt(1))

	for iteration := 0; iteration < 100; iteration++ {
		sess := newSession(consumer)
		env := &blockEnv{
			generation: uint64(iteration + 1),
			header:     &types.Header{Number: new(big.Int).Set(number), Time: uint64(iteration + 1)},
		}
		sess.applyMu.Lock()
		sess.setEnv(env)
		sess.activateEnv()
		worker := sess.worker
		sess.applyMu.Unlock()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			sess.preparationSnapshot()
		}()
		go func() {
			defer wg.Done()
			<-start
			consumer.interruptPreconfWorker(worker, env.generation)
		}()
		close(start)
		wg.Wait()
	}
}
