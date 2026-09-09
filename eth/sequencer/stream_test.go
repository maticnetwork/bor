package sequencer

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
)

func barePublisher() *Publisher {
	p := &Publisher{
		head:    commitment.Seed(testChainID),
		anchor:  commitment.Seed(testChainID),
		seed:    commitment.Seed(testChainID),
		journal: newJournal(),
		wake:    make(chan struct{}, 1),
	}
	p.read = newReader(nil, p.seed, p.markReachable)

	return p
}

func TestHandleAck(t *testing.T) {
	item := journalItem{seq: 7, post: commitment.Head{0x07}, kind: entrySeal}

	cases := []struct {
		name       string
		ack        ackResult
		inflight   []sent
		wantReason streamEnd
		wantDone   bool
		wantAcked  uint64
		wantFailed bool
	}{
		{
			name:       "ok retires",
			ack:        ackResult{status: pb.AckStatus_ACK_STATUS_OK},
			inflight:   []sent{{item: item, at: time.Now()}},
			wantDone:   false,
			wantAcked:  7,
			wantReason: endCtx, // unused when done=false
		},
		{
			name:       "stale reconciles even on a resend",
			ack:        ackResult{status: pb.AckStatus_ACK_STATUS_STALE_COMMITMENT},
			inflight:   []sent{{item: item}},
			wantDone:   true,
			wantReason: endStale,
		},
		{
			name:       "rate limited retries transport",
			ack:        ackResult{status: pb.AckStatus_ACK_STATUS_RATE_LIMITED},
			inflight:   []sent{{item: item}},
			wantDone:   true,
			wantReason: endTransport,
		},
		{
			name:       "malformed is terminal",
			ack:        ackResult{status: pb.AckStatus_ACK_STATUS_MALFORMED},
			inflight:   []sent{{item: item}},
			wantDone:   true,
			wantReason: endTerminal,
			wantFailed: true,
		},
		{
			name:       "transport error",
			ack:        ackResult{err: errors.New("recv")},
			inflight:   []sent{{item: item}},
			wantDone:   true,
			wantReason: endTransport,
		},
		{
			name:       "ack without pending is terminal",
			ack:        ackResult{status: pb.AckStatus_ACK_STATUS_OK},
			wantDone:   true,
			wantReason: endTerminal,
			wantFailed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := barePublisher()
			// retire only honors acks for entries still live in the journal;
			// seat the fixture item at its seq.
			p.journal.nextSeq = item.seq
			p.journal.items = append(p.journal.items, item)
			p.journal.nextSeq = item.seq + 1

			inflight := append([]sent(nil), tc.inflight...)

			res, done := p.handleAck(tc.ack, &inflight)

			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v", done, tc.wantDone)
			}

			if done && res.reason != tc.wantReason {
				t.Fatalf("reason = %v, want %v", res.reason, tc.wantReason)
			}

			if p.ackedSeq != tc.wantAcked {
				t.Fatalf("ackedSeq = %d, want %d", p.ackedSeq, tc.wantAcked)
			}

			if p.failed.Load() != tc.wantFailed {
				t.Fatalf("failed = %v, want %v", p.failed.Load(), tc.wantFailed)
			}
		})
	}
}

func TestHandleAckOKMarksProgress(t *testing.T) {
	p := barePublisher()
	live := journalItem{seq: 1, post: commitment.Head{0x01}}
	p.journal.items = append(p.journal.items, live)
	p.journal.nextSeq = 2

	inflight := []sent{{item: live, at: time.Now()}}

	// done=false is the progress signal: the session counts it as an
	// entry retired.
	if _, done := p.handleAck(ackResult{status: pb.AckStatus_ACK_STATUS_OK}, &inflight); done {
		t.Fatal("ok ack must not end the session")
	}

	if p.anchor != (commitment.Head{0x01}) {
		t.Fatalf("anchor = %x", p.anchor)
	}

	if !p.confirmed {
		t.Fatal("ok ack must confirm the anchor")
	}
}

func TestCoalescePublishedRecord(t *testing.T) {
	start := commitment.Head{0x01}
	items := make([]journalItem, 3)
	head := start
	for index := range items {
		raw := []byte{byte(index + 1)}
		next := commitment.FoldTx(head, raw)
		items[index] = journalItem{
			seq:    uint64(index + 1),
			entry:  recordEntry(raw, head),
			pre:    head,
			post:   next,
			kind:   entryRecord,
			height: 7,
		}
		head = next
	}

	entry, batch := coalescePublishedRecord(items, 0)
	if len(batch) != len(items) {
		t.Fatalf("batch length = %d, want %d", len(batch), len(items))
	}
	if got := entry.GetRecord().GetTransactions(); len(got) != 3 || got[0][0] != 1 || got[2][0] != 3 {
		t.Fatalf("transactions = %v", got)
	}
	if got := commitment.FoldTxs(start, entry.GetRecord().GetTransactions()); got != items[len(items)-1].post {
		t.Fatalf("batch commitment = %x, want %x", got, items[len(items)-1].post)
	}
}

func TestHandleAckRetiresPublishedRecordBatch(t *testing.T) {
	p := barePublisher()
	items := make([]journalItem, 3)
	for index := range items {
		items[index] = journalItem{seq: uint64(index + 1), post: commitment.Head{byte(index + 1)}}
		p.journal.items = append(p.journal.items, items[index])
	}
	p.journal.nextSeq = 4
	inflight := []sent{{item: items[0], batch: items, at: time.Now()}}

	if _, done := p.handleAck(ackResult{status: pb.AckStatus_ACK_STATUS_OK}, &inflight); done {
		t.Fatal("batched record ack ended the stream")
	}
	if p.ackedSeq != 3 || p.anchor != items[2].post {
		t.Fatalf("acked = %d, anchor = %x", p.ackedSeq, p.anchor)
	}
}

func TestCoalesceRecordByteCap(t *testing.T) {
	// Coalescing many large txs must stay under the store's max.message.bytes.
	const txSize = 32 * 1024

	items := make([]journalItem, 40)
	start := commitment.Head{0x01}
	head := start
	for i := range items {
		raw := make([]byte, txSize)
		raw[0] = byte(i + 1)
		next := commitment.FoldTx(head, raw)
		items[i] = journalItem{
			seq:    uint64(i + 1),
			entry:  recordEntry(raw, head),
			pre:    head,
			post:   next,
			kind:   entryRecord,
			height: 9,
		}
		head = next
	}

	entry, batch := coalescePublishedRecord(items, 0)

	if len(batch) == 0 || len(batch) >= len(items) {
		t.Fatalf("batch length = %d, want a byte-capped prefix of %d", len(batch), len(items))
	}

	got := 0
	for _, raw := range entry.GetRecord().GetTransactions() {
		got += len(raw)
	}
	if got > maxRecordBytes {
		t.Fatalf("coalesced tx bytes = %d, over the %d cap", got, maxRecordBytes)
	}
	if got+txSize <= maxRecordBytes {
		t.Fatalf("batch not maximal: %d plus one more tx still fits under %d", got, maxRecordBytes)
	}

	// proto.Size must stay under what the ingress accepts (max.message.bytes
	// less its recordFraming allowance).
	if size := proto.Size(entry); size > maxMessageBytes-1024 {
		t.Fatalf("record proto size = %d, the ingress rejects above %d", size, maxMessageBytes-1024)
	}

	if fold := commitment.FoldTxs(start, entry.GetRecord().GetTransactions()); fold != items[len(batch)-1].post {
		t.Fatalf("coalesced commitment = %x, want %x", fold, items[len(batch)-1].post)
	}
}

func TestCoalesceRecordCountCap(t *testing.T) {
	// Small records cap at maxTransactionsPerPublishedRecord, well below the
	// byte cap.
	items := make([]journalItem, maxTransactionsPerPublishedRecord+20)
	head := commitment.Head{0x01}
	for i := range items {
		raw := []byte{byte(i%251 + 1)}
		next := commitment.FoldTx(head, raw)
		items[i] = journalItem{
			seq:    uint64(i + 1),
			entry:  recordEntry(raw, head),
			pre:    head,
			post:   next,
			kind:   entryRecord,
			height: 9,
		}
		head = next
	}

	if _, batch := coalescePublishedRecord(items, 0); len(batch) != maxTransactionsPerPublishedRecord {
		t.Fatalf("batch length = %d, want the %d count cap", len(batch), maxTransactionsPerPublishedRecord)
	}
}

func TestCoalesceRecordByteCapBoundary(t *testing.T) {
	// Two records whose sizes sum to exactly maxRecordBytes coalesce together:
	// the cap boundary is inclusive (>, not >=).
	head := commitment.Head{0x02}
	sizes := []int{100_000, maxRecordBytes - 100_000}
	items := make([]journalItem, len(sizes))
	for i, sz := range sizes {
		raw := make([]byte, sz)
		raw[0] = byte(i + 1)
		next := commitment.FoldTx(head, raw)
		items[i] = journalItem{
			seq:    uint64(i + 1),
			entry:  recordEntry(raw, head),
			pre:    head,
			post:   next,
			kind:   entryRecord,
			height: 3,
		}
		head = next
	}

	if _, batch := coalescePublishedRecord(items, 0); len(batch) != 2 {
		t.Fatalf("exact-fill batch = %d, want 2 (the cap boundary is inclusive)", len(batch))
	}
}
