package wit

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestWitnessAnnouncementSigningHashStable pins the digest format. If this
// changes, every signed announcement on the network breaks at once — bump the
// protocol version explicitly. The test value is recomputed independently to
// catch silent reordering of the concatenation.
func TestWitnessAnnouncementSigningHashStable(t *testing.T) {
	blockHash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	blockNumber := uint64(0x0102030405060708)
	witnessHash := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")

	got := WitnessAnnouncementSigningHash(blockHash, blockNumber, witnessHash)

	// Manual recomposition: domain-tag || blockHash || blockNumber (big-endian u64) || witnessHash
	want := crypto.Keccak256Hash(
		witnessAnnounceDomainTag,
		blockHash.Bytes(),
		[]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		witnessHash.Bytes(),
	)
	if got != want {
		t.Fatalf("signing-hash format drift: got %s want %s", got.Hex(), want.Hex())
	}
}

// TestWitnessAnnouncementSigningHashDomainSeparated guards that the witness
// announce digest cannot collide with a raw 3-field concatenation lacking the
// domain tag. This is the structural check that a header-seal signature, or
// any other future SignBytes context, cannot be replayed as a wit2 announce.
func TestWitnessAnnouncementSigningHashDomainSeparated(t *testing.T) {
	blockHash := common.HexToHash("0xaa")
	blockNumber := uint64(7)
	witnessHash := common.HexToHash("0xbb")

	withTag := WitnessAnnouncementSigningHash(blockHash, blockNumber, witnessHash)
	withoutTag := crypto.Keccak256Hash(
		blockHash.Bytes(),
		[]byte{0, 0, 0, 0, 0, 0, 0, 7},
		witnessHash.Bytes(),
	)
	if withTag == withoutTag {
		t.Fatalf("domain tag absent: digests collide, replay across signing contexts is possible")
	}
}

// TestWitnessAnnouncementSigningHashSensitive ensures every input field is
// covered by the digest — flipping any byte in any input must change the hash.
// Catches a bug where a refactor silently drops a field from the digest.
func TestWitnessAnnouncementSigningHashSensitive(t *testing.T) {
	base := WitnessAnnouncementSigningHash(
		common.HexToHash("0xaa"),
		1,
		common.HexToHash("0xbb"),
	)
	cases := []struct {
		name     string
		blockH   common.Hash
		num      uint64
		witnessH common.Hash
	}{
		{"different blockHash", common.HexToHash("0xab"), 1, common.HexToHash("0xbb")},
		{"different blockNumber", common.HexToHash("0xaa"), 2, common.HexToHash("0xbb")},
		{"different witnessHash", common.HexToHash("0xaa"), 1, common.HexToHash("0xbc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WitnessAnnouncementSigningHash(tc.blockH, tc.num, tc.witnessH); got == base {
				t.Fatalf("digest unchanged when %s differed", tc.name)
			}
		})
	}
}

// TestProtocolVersionsContainsWIT2 guards the handshake advertising. WIT2 must
// be advertised first (preferred) for new connections. If WIT1 ever leaks
// ahead of WIT2, peers downgrade silently and the fast path stops working.
func TestProtocolVersionsContainsWIT2(t *testing.T) {
	if len(ProtocolVersions) == 0 || ProtocolVersions[0] != WIT2 {
		t.Fatalf("expected WIT2 first in ProtocolVersions, got %v", ProtocolVersions)
	}
	if protocolLengths[WIT2] != 7 {
		t.Fatalf("WIT2 protocolLengths must be 7 (one new message added), got %d", protocolLengths[WIT2])
	}
}
