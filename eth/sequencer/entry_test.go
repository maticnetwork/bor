package sequencer

import (
	"testing"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

func testOpen(number uint64, parent common.Hash, prefix commitment.Head) *pb.Entry {
	return &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
		BlockNumber:      number,
		BlockTimestamp:   1700000000 + number,
		ParentHash:       parent.Bytes(),
		GasLimit:         30_000_000,
		BaseFee:          []byte{0x01},
		PrefixCommitment: prefix.Bytes(),
	}}}
}

func testRecord(tx []byte, prefix commitment.Head) *pb.Entry {
	return &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{tx},
		PrefixCommitment: prefix.Bytes(),
	}}}
}

func testSeal(t *testing.T, number uint64, prefix commitment.Head) *pb.Entry {
	t.Helper()

	raw, err := rlp.EncodeToBytes(testHeader(number, common.Hash{0x01}))
	if err != nil {
		t.Fatalf("rlp: %v", err)
	}

	return &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{
		Header:           raw,
		PrefixCommitment: prefix.Bytes(),
	}}}
}

func TestContentEqual(t *testing.T) {
	a, b := commitment.Head{0xaa}, commitment.Head{0xbb}

	cases := []struct {
		name string
		x, y *pb.Entry
		want bool
	}{
		{"open equal ignoring prefix", testOpen(1, common.Hash{0x01}, a), testOpen(1, common.Hash{0x01}, b), true},
		{"open different number", testOpen(1, common.Hash{0x01}, a), testOpen(2, common.Hash{0x01}, a), false},
		{"open different parent", testOpen(1, common.Hash{0x01}, a), testOpen(1, common.Hash{0x02}, a), false},
		{"record equal ignoring prefix", testRecord([]byte{0x01}, a), testRecord([]byte{0x01}, b), true},
		{"record different tx", testRecord([]byte{0x01}, a), testRecord([]byte{0x02}, a), false},
		{"record different length", testRecord([]byte{0x01}, a), &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{Transactions: [][]byte{{0x01}, {0x02}}}}}, false},
		{"kind mismatch", testOpen(1, common.Hash{0x01}, a), testRecord([]byte{0x01}, a), false},
		{"kind mismatch reversed", testSeal(t, 1, a), testOpen(1, common.Hash{0x01}, a), false},
		{"seal equal ignoring prefix", testSeal(t, 1, a), testSeal(t, 1, b), true},
		{"seal different header", testSeal(t, 1, a), testSeal(t, 2, a), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentEqual(tc.x, tc.y); got != tc.want {
				t.Fatalf("contentEqual = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFoldEntryRejectsMalformedWireShape(t *testing.T) {
	prefix := make([]byte, len(commitment.Head{}))
	for _, test := range []struct {
		name  string
		entry *pb.Entry
	}{
		{
			name: "short open prefix",
			entry: &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
				PrefixCommitment: prefix[:len(prefix)-1],
				ParentHash:       make([]byte, common.HashLength),
			}}},
		},
		{
			name: "short open parent",
			entry: &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
				PrefixCommitment: prefix,
				ParentHash:       make([]byte, common.HashLength-1),
			}}},
		},
		{
			name: "short record prefix",
			entry: &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
				PrefixCommitment: prefix[:len(prefix)-1],
			}}},
		},
		{
			name: "empty seal header",
			entry: &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{
				PrefixCommitment: prefix,
			}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := foldEntry(commitment.Head{}, test.entry); err == nil {
				t.Fatal("malformed entry was accepted")
			}
		})
	}
}

func TestEntryHeight(t *testing.T) {
	if h, ok := entryHeight(testOpen(7, common.Hash{0x01}, commitment.Head{})); !ok || h != 7 {
		t.Fatalf("open height = %d/%v", h, ok)
	}

	if h, ok := entryHeight(testSeal(t, 9, commitment.Head{})); !ok || h != 9 {
		t.Fatalf("seal height = %d/%v", h, ok)
	}

	if _, ok := entryHeight(testRecord([]byte{0x01}, commitment.Head{})); ok {
		t.Fatal("record must carry no height")
	}

	garbage := &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{Header: []byte{0xde}}}}
	if _, ok := entryHeight(garbage); ok {
		t.Fatal("undecodable seal must carry no height")
	}
}

// refoldEntry must reproduce the enqueue-side folds exactly, with only the
// prefix rewritten.
func TestRefoldEntryMatchesDirectFolds(t *testing.T) {
	base := commitment.Head{0x11}

	open := testOpen(3, common.Hash{0x02}, commitment.Head{0xff})
	items := []journalItem{
		{entry: open, kind: entryOpen, height: 3},
		{entry: testRecord([]byte{0xbe, 0xef}, commitment.Head{0xff}), kind: entryRecord, height: 3},
		{entry: testSeal(t, 3, commitment.Head{0xff}), kind: entrySeal, height: 3},
	}

	cur := base

	for _, item := range items {
		entry, next, err := refoldEntry(cur, item)
		if err != nil {
			t.Fatalf("refold: %v", err)
		}

		if got := commitment.Head(entryPrefix(entry)); got != cur {
			t.Fatalf("prefix %x, want %x", got, cur)
		}

		if next == cur {
			t.Fatal("fold did not advance")
		}

		cur = next
	}

	// The refolded open must fold identically to a direct FoldOpen.
	wantOpen, err := commitment.FoldOpen(base, commitment.OpenContext{
		Number:     3,
		Timestamp:  open.GetBlockOpen().GetBlockTimestamp(),
		ParentHash: common.Hash{0x02},
		GasLimit:   open.GetBlockOpen().GetGasLimit(),
		BaseFee:    testHeader(0, common.Hash{}).BaseFee.SetBytes(open.GetBlockOpen().GetBaseFee()),
	})
	if err != nil {
		t.Fatalf("fold open: %v", err)
	}

	if _, next, _ := refoldEntry(base, items[0]); next != wantOpen {
		t.Fatalf("refolded open %x, want %x", next, wantOpen)
	}
}

func TestDecodeSealHeaderRejectsGarbage(t *testing.T) {
	if _, err := decodeSealHeader([]byte{0xde, 0xad}); err == nil {
		t.Fatal("garbage header decoded")
	}
}
