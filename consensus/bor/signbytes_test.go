package bor

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
)

// TestSignBytesForwardsMimetype is the regression for the wit2 announce
// signing path's external-signer compatibility: bor.SignBytes must hand the
// caller-supplied mimetype to the configured signer untouched. Operators
// configuring Clef whitelist a specific string ("application/x-bor-wit2-
// announce"); if SignBytes ever rewrote, lower-cased, or stripped that, the
// signer would either reject the request or sign under a different domain.
//
// The test captures the (mimetype, payload) the wallet sees and asserts both
// match exactly what the caller passed.
func TestSignBytesForwardsMimetype(t *testing.T) {
	bor := &Bor{}
	addr := common.HexToAddress("0x1234")

	var (
		gotMimetype string
		gotPayload  []byte
	)
	bor.Authorize(addr, func(_ accounts.Account, mimetype string, data []byte) ([]byte, error) {
		gotMimetype = mimetype
		gotPayload = append([]byte(nil), data...)
		return make([]byte, 65), nil
	})

	preimage := []byte("wit2-announce-preimage")
	signer, sig, err := bor.SignBytes(accounts.MimetypeBorWitnessAnnounce, preimage)
	if err != nil {
		t.Fatalf("SignBytes: %v", err)
	}
	if signer != addr {
		t.Fatalf("signer addr mismatch: got %s want %s", signer, addr)
	}
	if len(sig) != 65 {
		t.Fatalf("expected 65-byte signature, got %d", len(sig))
	}
	if gotMimetype != accounts.MimetypeBorWitnessAnnounce {
		t.Fatalf("mimetype not forwarded literally: got %q want %q",
			gotMimetype, accounts.MimetypeBorWitnessAnnounce)
	}
	if !bytes.Equal(gotPayload, preimage) {
		t.Fatalf("payload not forwarded literally: got %x want %x", gotPayload, preimage)
	}
}

// TestSignBytesRejectsHeaderMimetype guards against accidental cross-context
// reuse: callers must never pass MimetypeBor (header sealing) into SignBytes,
// since that would let an announce signature replay as a block-seal.
func TestSignBytesRejectsHeaderMimetype(t *testing.T) {
	bor := &Bor{}
	bor.Authorize(common.HexToAddress("0x1234"), func(accounts.Account, string, []byte) ([]byte, error) {
		t.Fatal("signFn must not be reached for rejected mimetype")
		return nil, nil
	})

	if _, _, err := bor.SignBytes("", []byte{0x01}); err == nil {
		t.Fatal("empty mimetype must be rejected")
	}
	if _, _, err := bor.SignBytes(accounts.MimetypeBor, []byte{0x01}); err == nil {
		t.Fatal("MimetypeBor must be rejected to prevent header-seal replay")
	}
}

// TestSignBytesWithoutAuthorizedSigner covers the not-a-validator paths: a
// node that never called Authorize (or authorized the zero address) must
// refuse to sign rather than emit a signature under a zero identity.
func TestSignBytesWithoutAuthorizedSigner(t *testing.T) {
	bor := &Bor{}
	if _, _, err := bor.SignBytes(accounts.MimetypeBorWitnessAnnounce, []byte{0x01}); err == nil {
		t.Fatal("SignBytes must fail with no authorized signer")
	}

	bor.Authorize(common.Address{}, func(accounts.Account, string, []byte) ([]byte, error) {
		t.Fatal("signFn must not be reached for a zero-address signer")
		return nil, nil
	})
	if _, _, err := bor.SignBytes(accounts.MimetypeBorWitnessAnnounce, []byte{0x01}); err == nil {
		t.Fatal("SignBytes must fail for a zero-address signer")
	}
}

// TestSignBytesPropagatesSignFnError pins that wallet/clef failures surface to
// the caller instead of returning a bogus (signer, nil-sig) pair.
func TestSignBytesPropagatesSignFnError(t *testing.T) {
	bor := &Bor{}
	bor.Authorize(common.HexToAddress("0x1234"), func(accounts.Account, string, []byte) ([]byte, error) {
		return nil, errors.New("wallet locked")
	})

	_, _, err := bor.SignBytes(accounts.MimetypeBorWitnessAnnounce, []byte{0x01})
	if err == nil || !strings.Contains(err.Error(), "wallet locked") {
		t.Fatalf("expected wallet error to propagate, got %v", err)
	}
}

// TestCurrentSigner covers both states of the authorized-signer lookup used by
// the wit2 announce path to decide whether this node may sign announcements.
func TestCurrentSigner(t *testing.T) {
	bor := &Bor{}
	if got := bor.CurrentSigner(); got != (common.Address{}) {
		t.Fatalf("expected zero address before Authorize, got %s", got)
	}

	addr := common.HexToAddress("0x5678")
	bor.Authorize(addr, func(accounts.Account, string, []byte) ([]byte, error) {
		return make([]byte, 65), nil
	})
	if got := bor.CurrentSigner(); got != addr {
		t.Fatalf("CurrentSigner: got %s want %s", got, addr)
	}
}
