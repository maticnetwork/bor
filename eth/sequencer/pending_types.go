package sequencer

import (
	"sync"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

const (
	pendingInputLimit                   = 32 * 1024 * 1024
	pendingOpenBaseFeeLimit             = 32
	pendingSealHeaderLimit              = 1 * 1024 * 1024
	pendingRPCPublicationLimit          = 4
	pendingEagerPublicationTxs          = 16
	pendingReceiptPublicationBatch      = 16
	pendingRPCMinPublishDelay           = 100 * time.Millisecond
	pendingRPCTimeFallbackLimit         = pendingRPCPublicationLimit / 2
	pendingRPCPublishFallbackDelay      = 200 * time.Millisecond
	preconfCanonicalParentWait          = 2 * time.Second
	preconfCanonicalParentPoll          = 5 * time.Millisecond
	pendingLogsQueueLimit               = 64
	pendingEntryLimit                   = 256
	maxPreconfPrefixCatchupTransactions = 512
)

type PendingPhase uint8

const (
	PendingEmpty PendingPhase = iota
	PendingBuilding
	PendingSealed
	PendingImporting
)

type PendingStateReader interface {
	GetBalance(common.Address) *uint256.Int
	GetNonce(common.Address) uint64
	GetNonceWithError(common.Address) (uint64, error)
	GetCode(common.Address) []byte
	GetStorage(common.Address, common.Hash) common.Hash
	NewStateDB() (*state.StateDB, error)
}

type pendingStateReader struct {
	mu    sync.RWMutex
	state *state.StateDB
}

func (r *pendingStateReader) GetBalance(address common.Address) *uint256.Int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return new(uint256.Int).Set(r.state.GetBalance(address))
}

func (r *pendingStateReader) GetNonce(address common.Address) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.GetNonce(address)
}

func (r *pendingStateReader) GetNonceWithError(address common.Address) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.GetNonce(address), r.state.Error()
}

func (r *pendingStateReader) GetCode(address common.Address) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.state.GetCode(address)...)
}

func (r *pendingStateReader) GetStorage(address common.Address, slot common.Hash) common.Hash {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.GetState(address, slot)
}

func (r *pendingStateReader) NewStateDB() (*state.StateDB, error) {
	// Published state is immutable. Copies may run concurrently; each copy owns
	// its journals, while accessor methods serialize cache-populating reads.
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.state.Error(); err != nil {
		return nil, err
	}
	return r.state.Copy(), nil
}

type PendingRPCView struct {
	Header           *types.Header
	Block            *types.Block
	State            PendingStateReader
	Receipts         map[common.Hash]*types.Receipt
	Logs             []*types.Log
	receiptBlockHash common.Hash
}

type ReusableExecution struct {
	HeaderHash common.Hash // Commits every encoded header field, including state root and gas used.
	TxRoot     common.Hash
	StateDB    *state.StateDB
	Result     *core.ProcessResult
}

type pendingKey struct {
	number uint64
	parent common.Hash
}

type pendingPayload struct {
	view      *PendingRPCView
	sealed    *ReusableExecution
	finalized bool
}

type pendingInvalidation struct {
	number uint64
	reason string
}

type pendingPrefix struct {
	Transactions types.Transactions
	State        PendingStateReader
	StateDB      *state.StateDB
	Result       *core.ProcessResult
	Generation   uint64
}

type pendingEntry struct {
	generation           uint64
	phase                PendingPhase
	canonicalBase        bool
	Number               uint64
	ParentHash           common.Hash
	RPCView              *PendingRPCView
	executedTransactions types.Transactions
	executedReceipts     types.Receipts
	Sealed               *ReusableExecution
	claimedHash          common.Hash
	partialClaim         bool
	rejected             bool
	deferredInvalidation string
}

type PendingStore struct {
	mu            sync.RWMutex
	stateCopyOnce sync.Once
	stateCopy     chan struct{}
	generation    uint64
	entries       map[pendingKey]*pendingEntry
	active        pendingKey
	hasActive     bool
	db            ethdb.Database
}
