package wit

import (
	"encoding/binary"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/crypto"
)

// Constants to match up protocol versions and messages
const (
	WIT0 = 1
	WIT1 = 2
	// WIT2 adds BP-signed witness announcements, allowing peers to fast-validate
	// announces via signature recovery (microseconds) instead of full block
	// execution (~500ms). Signed announces are safe to relay transitively
	// because byte-correctness is verified at fetch time against the signed
	// witness hash; content-correctness blame attaches to the BP signer.
	WIT2 = 3
)

// ProtocolName is the official short name of the `wit` protocol used during
// devp2p capability negotiation.
const ProtocolName = "wit"

// ProtocolVersions are the supported versions of the `wit` protocol (first
// is primary).
var ProtocolVersions = []uint{WIT2, WIT1, WIT0}

// protocolLengths are the number of implemented message corresponding to
// different protocol versions.
var protocolLengths = map[uint]uint64{WIT2: 7, WIT1: 6, WIT0: 4}

// maxMessageSize is the maximum cap on the size of a protocol message.
const maxMessageSize = 16 * 1024 * 1024

const (
	MaxWitnessServe         = 16 // maximum pages a single GetWitness request may carry
	MaxWitnessMetadataServe = 64 // maximum hashes a single GetWitnessMetadata request may carry
)

const (
	NewWitnessMsg             = 0x00
	NewWitnessHashesMsg       = 0x01
	GetMsgWitness             = 0x02
	MsgWitness                = 0x03
	GetWitnessMetadataMsg     = 0x04
	WitnessMetadataMsg        = 0x05
	SignedNewWitnessHashesMsg = 0x06 // WIT2: signed witness announcement, safe to relay
)

// SignatureLength is the length of a BP signature over a witness announcement (r||s||v).
const SignatureLength = 65

// witnessAnnounceDomainTag is a unique prefix mixed into the signing digest so a
// signature produced for a WIT2 announcement cannot be replayed in any other
// context that signs 32-byte digests under the BP's signing key (block sealing,
// future signed messages, etc.). Cross-context replay is structurally
// impossible rather than only computationally hard, even if a future caller
// happens to share the same signFn mimetype.
var witnessAnnounceDomainTag = []byte("bor-wit2-announce\x00")

var (
	errMsgTooLarge    = errors.New("message too long")
	errDecode         = errors.New("invalid message")
	errInvalidMsgCode = errors.New("invalid message code")
)

// Packet represents a p2p message in the `wit` protocol.
type Packet interface {
	Name() string // Name returns a string corresponding to the message type.
	Kind() byte   // Kind returns the message type.
}

// GetWitnessRequest represents a list of witnesses query by witness pages.
type GetWitnessRequest struct {
	WitnessPages []WitnessPageRequest // Request by list of witness pages
}

type WitnessPageRequest struct {
	Hash common.Hash // BlockHash
	Page uint64      // Starts on 0
}

// GetWitnessPacket represents a witness query with request ID wrapping.
type GetWitnessPacket struct {
	RequestId uint64
	*GetWitnessRequest
}

// WitnessPacketRLPPacket represents a witness response with request ID wrapping.
type WitnessPacketRLPPacket struct {
	RequestId uint64
	WitnessPacketResponse
}

// WitnessPacketResponse represents a witness response, to use when we already
// have the witness rlp encoded.
type WitnessPacketResponse []WitnessPageResponse

type WitnessPageResponse struct {
	Data       []byte
	Hash       common.Hash
	Page       uint64 // Starts on 0; If Page >= TotalPages means the request was invalid and the response is an empty data array
	TotalPages uint64 // Length of pages
}

type NewWitnessPacket struct {
	Witness *stateless.Witness
}

type NewWitnessHashesPacket struct {
	Hashes  []common.Hash
	Numbers []uint64
}

// SignedWitnessAnnouncement is a BP-authenticated commitment to the existence
// of a specific witness for a specific block. The signer commits to:
//
//	keccak256(BlockHash || BlockNumber || WitnessHash)
//
// Receivers verify the signature with ecrecover and check that the recovered
// address is the validator scheduled for BlockNumber. Once verified, the
// announcement is safe to relay to other peers without local execution; any
// downstream receiver re-verifies independently. Bytes returned by a serving
// peer are checked against WitnessHash, so byte-correctness blame attaches to
// the server while content-correctness (state-root) blame attaches to the BP.
type SignedWitnessAnnouncement struct {
	BlockHash   common.Hash
	BlockNumber uint64
	WitnessHash common.Hash // WIT2 chunked-aggregate commitment over canonical witness RLP; see core/stateless.WitnessCommitHash
	Signature   []byte      // 65-byte secp256k1 signature
}

// SignedNewWitnessHashesPacket carries one or more signed witness announcements.
type SignedNewWitnessHashesPacket struct {
	Announcements []SignedWitnessAnnouncement
}

// GetWitnessMetadataRequest represents a request for witness metadata (just page count, no data)
type GetWitnessMetadataRequest struct {
	Hashes []common.Hash // Block hashes to get metadata for
}

// GetWitnessMetadataPacket represents a witness metadata query with request ID wrapping
type GetWitnessMetadataPacket struct {
	RequestId uint64
	*GetWitnessMetadataRequest
}

// WitnessMetadataResponse represents a single witness metadata response
type WitnessMetadataResponse struct {
	Hash        common.Hash
	TotalPages  uint64 // Total number of pages for this witness
	WitnessSize uint64 // Total witness size in bytes
	BlockNumber uint64 // Block number this witness belongs to
	Available   bool   // Whether witness exists in database
}

// WitnessMetadataPacket represents a witness metadata response with request ID wrapping
type WitnessMetadataPacket struct {
	RequestId uint64
	Metadata  []WitnessMetadataResponse
}

func (w *GetWitnessRequest) Name() string { return "GetWitness" }
func (w *GetWitnessRequest) Kind() byte   { return GetMsgWitness }

func (*WitnessPacketRLPPacket) Name() string { return "Witness" }
func (*WitnessPacketRLPPacket) Kind() byte   { return MsgWitness }

func (w *NewWitnessPacket) Name() string { return "NewWitness" }
func (w *NewWitnessPacket) Kind() byte   { return NewWitnessMsg }

func (w *NewWitnessHashesPacket) Name() string { return "NewWitnessHashes" }
func (w *NewWitnessHashesPacket) Kind() byte   { return NewWitnessHashesMsg }

func (w *SignedNewWitnessHashesPacket) Name() string { return "SignedNewWitnessHashes" }
func (w *SignedNewWitnessHashesPacket) Kind() byte   { return SignedNewWitnessHashesMsg }

// WitnessAnnouncementSigningPreImage returns the unhashed bytes a BP signs to
// authenticate a witness announcement. Production signing flows (clef,
// keystoreWallet.SignData) hash their input once before signing, so callers
// MUST pass this preimage, not WitnessAnnouncementSigningHash. The verifier
// independently computes WitnessAnnouncementSigningHash (= keccak256 of this
// preimage) and ecrecovers against it. Mismatching hash-vs-preimage between
// signer and verifier silently breaks every WIT2 signature, hence the split.
func WitnessAnnouncementSigningPreImage(blockHash common.Hash, blockNumber uint64, witnessHash common.Hash) []byte {
	const fixedLen = common.HashLength + 8 + common.HashLength
	buf := make([]byte, len(witnessAnnounceDomainTag)+fixedLen)
	n := copy(buf, witnessAnnounceDomainTag)
	copy(buf[n:], blockHash[:])
	binary.BigEndian.PutUint64(buf[n+common.HashLength:], blockNumber)
	copy(buf[n+common.HashLength+8:], witnessHash[:])
	return buf
}

// WitnessAnnouncementSigningHash returns the digest a BP signs to authenticate
// a witness announcement. Must be byte-identical on both signer and verifier.
// Used by the verifier; signers must instead feed the preimage into the wallet
// SignData path, which keccaks once internally.
func WitnessAnnouncementSigningHash(blockHash common.Hash, blockNumber uint64, witnessHash common.Hash) common.Hash {
	return crypto.Keccak256Hash(WitnessAnnouncementSigningPreImage(blockHash, blockNumber, witnessHash))
}

func (w *GetWitnessMetadataRequest) Name() string { return "GetWitnessMetadata" }
func (w *GetWitnessMetadataRequest) Kind() byte   { return GetWitnessMetadataMsg }

func (w *WitnessMetadataPacket) Name() string { return "WitnessMetadata" }
func (w *WitnessMetadataPacket) Kind() byte   { return WitnessMetadataMsg }
