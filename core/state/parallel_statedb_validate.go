package state

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
)

// valuesEqual compares MVStore reads/writes. New value types MUST add
// a case — the default panics rather than falling back to interface{}
// equality, which panics for non-comparable types (slices, maps) and
// silently uses pointer-identity for pointer types.
func valuesEqual(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch av := a.(type) {
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	case common.Hash:
		bv, ok := b.(common.Hash)
		return ok && av == bv
	case uint64:
		bv, ok := b.(uint64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	default:
		panic(fmt.Sprintf("valuesEqual: unsupported MVStore value type %T", a))
	}
}

// ValidateResult holds categorized validation failure info.
type ValidateResult struct {
	Valid   bool
	FailKey string // "nonce", "storage", "code", "create", "unknown"
}

// Validate checks if this tx's reads are still consistent with MVStore/MVBalanceStore.
func (s *ParallelStateDB) Validate() bool {
	return s.ValidateDetailed().Valid
}

func (s *ParallelStateDB) ValidateCategory() string {
	r := s.ValidateDetailed()
	if r.Valid {
		return ""
	}
	return r.FailKey
}

// ValidateDetailed returns validation result with failure categorization.
// ValidateDetailed validates all reads including balance deltas.
// Nonce validation is skipped for txs with pre-computed sender nonces.
func (s *ParallelStateDB) ValidateDetailed() ValidateResult {
	if s.Panicked {
		return ValidateResult{Valid: false, FailKey: "panic"}
	}
	for i := range s.StoreReads {
		if cat := s.validateStoreRead(&s.StoreReads[i]); cat != "" {
			return ValidateResult{Valid: false, FailKey: cat}
		}
	}
	for i := range s.BalReads {
		if !s.validateBalanceRead(&s.BalReads[i]) {
			return ValidateResult{Valid: false, FailKey: "balance"}
		}
	}
	return ValidateResult{Valid: true}
}

// validateStoreRead validates a single store read against current MVStore
// state. Returns "" on success, or a failure category on mismatch.
func (s *ParallelStateDB) validateStoreRead(rd *StoreReadDesc) string {
	if s.skipNonceForSenderChain(rd.Key) {
		return ""
	}
	curVal, writer, inc, found, isEst := s.readForValidate(rd.Key)
	if storeReadMatches(rd, curVal, writer, inc, found, isEst) {
		return ""
	}
	return storeReadFailCategory(rd.Key)
}

// skipNonceForSenderChain reports whether a nonce read can be skipped
// because the sender's nonce was pre-computed for the same-sender chain.
func (s *ParallelStateDB) skipNonceForSenderChain(key blockstm.Key) bool {
	if !key.IsSubpath() || key.GetSubpath() != NoncePath {
		return false
	}
	_, ok := s.SenderNonces[key.GetAddress()]
	return ok
}

// readForValidate reads MVStore for validation, waiting once for any
// ESTIMATE writer to finish and re-reading atomically.
func (s *ParallelStateDB) readForValidate(key blockstm.Key) (val any, writer, inc int, found, isEst bool) {
	val, writer, inc, found, isEst = s.store.ReadVersionFull(key, s.TxIndex)
	if writer >= 0 && isEst {
		if s.WaitForTx != nil {
			s.WaitForTx(writer)
		}
		val, writer, inc, found, isEst = s.store.ReadVersionFull(key, s.TxIndex)
	}
	return
}

// storeReadMatches reports whether the recorded read still matches the
// current MVStore entry. ESTIMATE entries can never match — their values
// are stale and re-execution will produce a new incarnation.
func storeReadMatches(rd *StoreReadDesc, curVal any, writer, inc int, found, isEst bool) bool {
	if isEst {
		return !found && rd.StoreVal == nil
	}
	if writer == rd.WriterIdx && inc == rd.WriterInc {
		return true
	}
	// Value equality is unsound when the winning writer's position is
	// load-bearing (rd.ExactWriter): a different writer with an equal value can
	// still change the result, e.g. a reordered metamorphic CREATE2/SELFDESTRUCT
	// that moves the code/storage/marker writer across the destruction boundary.
	if !rd.ExactWriter && found && rd.StoreVal != nil && valuesEqual(curVal, rd.StoreVal) {
		return true
	}
	if !found && rd.StoreVal == nil {
		return true
	}
	return false
}

// validateBalanceRead validates a single balance delta read. Returns true
// on success. Fees are applied to the real StateDB during settlement, not
// through MVBalanceStore, so coinbase reads go through the same delta
// validation as any other address.
func (s *ParallelStateDB) validateBalanceRead(rd *BalReadDesc) bool {
	add, sub := s.bals.ReadDeltaAfter(rd.Addr, rd.AfterIdx, s.TxIndex)
	return add.Cmp(&rd.BalAdd) == 0 && sub.Cmp(&rd.BalSub) == 0
}

// storeReadFailCategory maps an MVStore Key to its failure category
// ("nonce", "code", "create", "storage", or "unknown").
func storeReadFailCategory(key blockstm.Key) string {
	if key.IsSubpath() {
		switch key.GetSubpath() {
		case NoncePath:
			return "nonce"
		case CodePath:
			return "code"
		case CreatePath:
			return "create"
		}
		return "unknown"
	}
	if key.IsState() {
		return "storage"
	}
	return "unknown"
}

// ValidationDiag holds diagnostic info about a validation failure.
type ValidationDiag struct {
	TxIdx      int
	Category   string         // "storage", "nonce", "balance", "code", "create"
	Addr       common.Address // address involved
	Slot       common.Hash    // storage slot (for storage failures)
	ReadWriter int            // writerIdx at read time
	CurWriter  int            // writerIdx at validation time
	ReadInc    int            // incarnation at read time
	CurInc     int            // incarnation at validation time
	FromBase   bool           // true if read was from base state
}

// DiagnoseValidation returns ALL validation failures (not just the first).
// For diagnostic use only — slower than Validate().
func (s *ParallelStateDB) DiagnoseValidation() []ValidationDiag {
	var diags []ValidationDiag
	for i := range s.StoreReads {
		if d, ok := s.diagnoseStoreRead(&s.StoreReads[i]); ok {
			diags = append(diags, d)
		}
	}
	for i := range s.BalReads {
		if d, ok := s.diagnoseBalanceRead(&s.BalReads[i]); ok {
			diags = append(diags, d)
		}
	}
	return diags
}

// diagnoseStoreRead returns a diagnostic for rd if it currently fails
// validation; ok=false means the read is still consistent.
func (s *ParallelStateDB) diagnoseStoreRead(rd *StoreReadDesc) (ValidationDiag, bool) {
	if s.skipNonceForSenderChain(rd.Key) {
		return ValidationDiag{}, false
	}
	curVal, writer, inc, found, _ := s.store.ReadVersionFull(rd.Key, s.TxIndex)
	// Diagnostic uses the historical (non-ESTIMATE-aware) match check —
	// it reports observed mismatches against the last committed entry.
	if storeReadMatches(rd, curVal, writer, inc, found, false) {
		return ValidationDiag{}, false
	}
	return ValidationDiag{
		TxIdx:      s.TxIndex,
		Category:   storeReadFailCategory(rd.Key),
		Addr:       rd.Key.GetAddress(),
		Slot:       rd.Key.GetStateKey(),
		ReadWriter: rd.WriterIdx,
		CurWriter:  writer,
		ReadInc:    rd.WriterInc,
		CurInc:     inc,
		FromBase:   rd.WriterIdx < 0,
	}, true
}

// diagnoseBalanceRead returns a diagnostic for rd if its cumulative delta
// drifted from the recorded value; ok=false means the read is consistent.
//
// No coinbase skip here: validateBalanceRead applies delta validation to
// every address including the coinbase (fees are settled on the real
// StateDB, not via MVBalanceStore), so the diagnostic must mirror that
// or it under-reports balance vfails on coinbase reads.
func (s *ParallelStateDB) diagnoseBalanceRead(rd *BalReadDesc) (ValidationDiag, bool) {
	add, sub := s.bals.ReadDeltaAfter(rd.Addr, rd.AfterIdx, s.TxIndex)
	if add.Cmp(&rd.BalAdd) == 0 && sub.Cmp(&rd.BalSub) == 0 {
		return ValidationDiag{}, false
	}
	return ValidationDiag{
		TxIdx:    s.TxIndex,
		Category: "balance",
		Addr:     rd.Addr,
		FromBase: false,
	}, true
}
