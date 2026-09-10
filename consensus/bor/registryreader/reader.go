// Package registryreader exposes the minimal read-only surface of the reserved
// blockspace registry that filtering modules (txpool, miner, block validator)
// need. It lives in a leaf package so core/, miner/, and core/txpool/ can
// import it without pulling in consensus/bor/contract → consensus/bor/statefull
// → core/, which would form an import cycle.
package registryreader

import (
	"fmt"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
)

// FeeModeFree is the only fee mode the protocol acts on: reserved transactions
// from a free-mode client pay zero in-protocol fee. The registry also defines
// feeMode 1 ("routed": fee paid, credited to the producer), reserved for a
// future external-block-producer world and not implemented - clients with any
// non-free fee mode are excluded from the effective set at snapshot build, so
// their senders pay standard fees like normal transactions.
const FeeModeFree uint8 = 0

// ClientLookup mirrors the slim "client for address" view returned by the
// registry contract. Defined here (not in consensus/bor/contract) so the
// interface is self-contained in this leaf package.
type ClientLookup struct {
	ClientID *big.Int
	GasQuota uint64
	Admin    common.Address
	Active   bool
	// FeeMode: see FeeModeFree. Only free-mode clients enter the effective set.
	FeeMode uint8
	// EffectiveFrom: block from which the client's reserved status applies.
	// Callers gate on Active && EffectiveFrom <= number.
	EffectiveFrom uint64
}

// Reader is the read-only view of the reserved blockspace registry consumed by
// transaction filtering paths. Callers must nil-check the interface before
// invoking — chain/txpool/miner expose a nil Reader when the chain has no
// registry configured (non-bor engines, devnets without the contract).
type Reader interface {
	HasReservedRegistry() bool
	IsReservedAddress(state *state.StateDB, number uint64, hash common.Hash, account common.Address) (bool, error)
	ReservedClientForAddress(state *state.StateDB, number uint64, hash common.Hash, account common.Address) (ClientLookup, error)
	// Root returns a value that changes whenever the reserved set or its limits
	// change, so a snapshot keyed on it can be reused until it moves.
	Root(state *state.StateDB, number uint64, hash common.Hash) (common.Hash, error)
	// WhitelistedAddresses returns every currently-active reserved address.
	WhitelistedAddresses(state *state.StateDB, number uint64, hash common.Hash) ([]common.Address, error)
	// TotalReservedGas returns the sum of active client quotas (reserved capacity).
	TotalReservedGas(state *state.StateDB, number uint64, hash common.Hash) (uint64, error)
}

// Client is the slim per-sender record a Snapshot stores: just what the hot
// classification and sequencing paths need. Activation state (active,
// effectiveFrom, feeMode) is resolved at snapshot build time, so it never
// appears here: every stored client is active, effective, and free-mode.
type Client struct {
	// ID is the registry contract's incremental clientId.
	ID uint64
	// GasQuota is the client's per-block reserved gas allowance, charged
	// against declared transaction gas limits.
	GasQuota uint64
}

// Snapshot is an immutable, pure-lookup view of the reserved set effective for
// one block, so the hot classification paths never do a per-transaction state
// read. Activation (active flag, effectiveFrom delay) is resolved once at
// build time: every stored entry is effective, which is why lookups take no
// block number. The txpool rebuilds it once per head; the execution path once
// per block (gated on the fork height). A nil *Snapshot classifies nothing
// (no registry / non-bor chain), so all methods are nil-safe.
type Snapshot struct {
	root     common.Hash
	capacity uint64
	// effectiveCapacity is Σ over quotas (the effective client set for this
	// block), distinct from capacity (the contract's raw totalReservedGas,
	// which createClient bumps immediately even for a client whose
	// effectiveFrom is still in the future). The base fee's reserved carve-out
	// is priced against effectiveCapacity: it equals exactly what this
	// snapshot's classification walk can admit, so the per-client-only quota
	// rule can never admit more reserved gas than the carve-out accounts for.
	effectiveCapacity uint64
	byAddress         map[common.Address]Client
	clientIDs         []uint64
	quotas            map[uint64]uint64
}

// BuildSnapshot reads the full active reserved set from the registry at the
// given block state and returns an immutable Snapshot classifying senders for
// block effectiveAt (clients with effectiveFrom > effectiveAt are excluded).
// Returns nil (no error) when no registry is configured.
func BuildSnapshot(r Reader, statedb *state.StateDB, number uint64, hash common.Hash, effectiveAt uint64) (*Snapshot, error) {
	if r == nil || !r.HasReservedRegistry() {
		return nil, nil
	}
	if statedb == nil {
		return readSnapshot(r, nil, number, hash, effectiveAt)
	}
	// Registry reads run through the EVM, which mutates the statedb it executes
	// against, so on the execution path they run isolated from the live block
	// state. The isolation must not drop them from a witness being produced: a
	// stateless verifier rebuilds this snapshot from the witness before
	// executing the block, and the block's own transactions don't necessarily
	// touch the registry (the first transaction-free block after a registry
	// change wouldn't).
	var snap *Snapshot
	err := statedb.ReadIsolated(func(tmp *state.StateDB) error {
		var err error
		snap, err = readSnapshot(r, tmp, number, hash, effectiveAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// readSnapshot performs the registry reads against statedb and assembles the
// snapshot.
func readSnapshot(r Reader, statedb *state.StateDB, number uint64, hash common.Hash, effectiveAt uint64) (*Snapshot, error) {
	root, err := r.Root(statedb, number, hash)
	if err != nil {
		return nil, err
	}
	addrs, err := r.WhitelistedAddresses(statedb, number, hash)
	if err != nil {
		return nil, err
	}
	capacity, err := r.TotalReservedGas(statedb, number, hash)
	if err != nil {
		return nil, err
	}
	clients, err := resolveClients(r, statedb, number, hash, addrs, effectiveAt)
	if err != nil {
		return nil, err
	}
	if err := assertCapacityInvariant(clients, capacity); err != nil {
		return nil, err
	}
	return NewSnapshot(root, capacity, clients), nil
}

// resolveClients reads each whitelisted address's client record and keeps only
// those effective for effectiveAt: active, past their effectiveFrom delay, and
// in free fee mode (see FeeModeFree for why non-free modes are excluded).
func resolveClients(r Reader, statedb *state.StateDB, number uint64, hash common.Hash, addrs []common.Address, effectiveAt uint64) (map[common.Address]Client, error) {
	clients := make(map[common.Address]Client, len(addrs))
	for _, a := range addrs {
		c, err := r.ReservedClientForAddress(statedb, number, hash, a)
		if err != nil {
			return nil, err
		}
		if !c.Active || c.EffectiveFrom > effectiveAt || c.FeeMode != FeeModeFree {
			continue
		}
		if c.ClientID == nil || !c.ClientID.IsUint64() {
			return nil, fmt.Errorf("reserved registry returned invalid client id %v for %s", c.ClientID, a)
		}
		clients[a] = Client{ID: c.ClientID.Uint64(), GasQuota: c.GasQuota}
	}
	return clients, nil
}

// assertCapacityInvariant fails hard when the sum of the effective clients'
// quotas exceeds capacity. capacity is the contract's totalReservedGas (Σ active
// quotas) and the effective set is a subset of active, so this holds for any
// well-formed registry. Classification depends on it: it enforces per-client
// quota only, with no cross-client ceiling — a violation would let the
// per-client-only rule over-admit reserved gas beyond what the base fee is
// priced against, and signals a broken/incompatible registry.
func assertCapacityInvariant(clients map[common.Address]Client, capacity uint64) error {
	quotaByClient := make(map[uint64]uint64, len(clients))
	for _, c := range clients {
		quotaByClient[c.ID] = c.GasQuota
	}
	var sumQuotas uint64
	for _, q := range quotaByClient {
		sumQuotas += q
	}
	if sumQuotas > capacity {
		return fmt.Errorf("reserved registry invariant violated: Σ effective client quotas %d > capacity %d", sumQuotas, capacity)
	}
	return nil
}

// NewSnapshot constructs a Snapshot from an explicit, already-effective client
// set. Used by tests and by callers that source the reserved set outside the
// registry contract.
func NewSnapshot(root common.Hash, capacity uint64, clients map[common.Address]Client) *Snapshot {
	quotas := make(map[uint64]uint64, len(clients))
	for _, c := range clients {
		quotas[c.ID] = c.GasQuota
	}
	ids := make([]uint64, 0, len(quotas))
	var effectiveCapacity uint64
	for id, q := range quotas {
		ids = append(ids, id)
		effectiveCapacity += q
	}
	slices.Sort(ids)
	return &Snapshot{
		root:              root,
		capacity:          capacity,
		effectiveCapacity: effectiveCapacity,
		byAddress:         clients,
		clientIDs:         ids,
		quotas:            quotas,
	}
}

// Root is the registry root this snapshot was built at; callers reuse the
// snapshot while the live root is unchanged.
func (s *Snapshot) Root() common.Hash {
	if s == nil {
		return common.Hash{}
	}
	return s.root
}

// IsReserved reports whether account is a reserved sender in this snapshot's
// effective set.
func (s *Snapshot) IsReserved(account common.Address) bool {
	if s == nil {
		return false
	}
	_, ok := s.byAddress[account]
	return ok
}

// Lookup returns the client ID that owns account, or (_, false) if account is
// not in the effective reserved set.
func (s *Snapshot) Lookup(account common.Address) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	c, ok := s.byAddress[account]
	return c.ID, ok
}

// Quota returns the per-block reserved gas allowance of clientID, or 0 for an
// unknown client.
func (s *Snapshot) Quota(clientID uint64) uint64 {
	if s == nil {
		return 0
	}
	return s.quotas[clientID]
}

// Clients returns the effective client IDs, sorted ascending.
func (s *Snapshot) Clients() []uint64 {
	if s == nil {
		return nil
	}
	return slices.Clone(s.clientIDs)
}

// Capacity returns the registry's raw totalReservedGas: the sum of active
// client quotas, including clients whose effectiveFrom hasn't been reached
// yet for this snapshot. Used solely for the build-time invariant check
// against effective quotas; base-fee pricing and header stamping use
// EffectiveCapacity instead.
func (s *Snapshot) Capacity() uint64 {
	if s == nil {
		return 0
	}
	return s.capacity
}

// EffectiveCapacity returns Σ over this snapshot's effective client quotas —
// exactly what the block's classification walk (ReservedWalk/ClassifyReserved)
// can admit. This is the value stamped into the header and priced against by
// the base fee; it excludes clients not yet effective for this block, unlike
// Capacity.
func (s *Snapshot) EffectiveCapacity() uint64 {
	if s == nil {
		return 0
	}
	return s.effectiveCapacity
}
