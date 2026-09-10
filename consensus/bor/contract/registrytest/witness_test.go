package registrytest

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// diskWithRegistryCode returns a fresh disk database holding only the registry
// bytecode, modeling a stateless node's local code store.
func diskWithRegistryCode(t *testing.T) ethdb.Database {
	t.Helper()
	disk := rawdb.NewMemoryDatabase()
	code := common.FromHex(params.ReservedBlockspaceRegistryCode)
	rawdb.WriteCode(disk, crypto.Keccak256Hash(code), code)
	return disk
}

// TestWitness_SelfSufficientForSnapshotRebuild pins the witness-completeness
// contract for reserved blockspace: a witness produced while building the
// registry snapshot must carry every trie node a stateless verifier needs to
// rebuild that same snapshot, even when the block's own transactions never
// touch the registry (BuildSnapshot's reads run against a throwaway state
// copy, so nothing else would put those nodes into the witness). The consumer
// side below is exactly ExecuteStateless's setup: a state backed only by the
// witness's node set, with an empty disk behind it.
func TestWitness_SelfSufficientForSnapshotRebuild(t *testing.T) {
	// Deploy the real registry bytecode into a committable trie database and
	// commit it, so the registry state lives in a real MPT a witness can prove.
	disk := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(disk, triedb.HashDefaults)
	sdb := state.NewDatabase(tdb, nil)
	st, err := state.New(types.EmptyRootHash, sdb)
	require.NoError(t, err)

	h := NewHarnessOn(t, st)

	root, err := st.Commit(0, true, false)
	require.NoError(t, err)
	require.NoError(t, tdb.Commit(root, false))

	parent := &types.Header{Number: big.NewInt(0), Root: root}
	context := &types.Header{Number: big.NewInt(1), ParentHash: parent.Hash()}
	headers := stateless.NewMockHeaderReader()
	headers.AddHeader(parent)

	// Producer: a witness-attached state at the committed root, mirroring how
	// block processing attaches the block witness before the snapshot build.
	prod, err := state.New(root, sdb)
	require.NoError(t, err)
	witness, err := stateless.NewWitness(context, headers)
	require.NoError(t, err)
	prod.StartPrefetcher("chain", witness, nil)
	defer prod.StopPrefetcher()

	snap, err := registryreader.BuildSnapshot(h.Reader, prod, 0, parent.Hash(), 1)
	require.NoError(t, err)
	require.True(t, snap.IsReserved(h.ReservedAddr))

	// Consumer: rebuild the snapshot from the witness alone. The disk behind
	// MakeHashDB carries only the registry bytecode - the Bor witness wire
	// format excludes code (stateless nodes source it from genesis alloc and
	// bytecode sync) - so every trie node must come from the witness. A missing
	// one fails the build, which is the chain-wedging failure mode on stateless
	// nodes.
	memdb := witness.MakeHashDB(diskWithRegistryCode(t))
	cons, err := state.New(root, state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
	require.NoError(t, err)

	rebuilt, err := registryreader.BuildSnapshot(h.Reader, cons, 0, parent.Hash(), 1)
	require.NoError(t, err, "witness must be self-sufficient for the reserved snapshot rebuild")
	require.Equal(t, snap.Root(), rebuilt.Root())
	require.Equal(t, snap.Capacity(), rebuilt.Capacity())
	require.Equal(t, snap.EffectiveCapacity(), rebuilt.EffectiveCapacity())
	require.True(t, rebuilt.IsReserved(h.ReservedAddr))
	require.False(t, rebuilt.IsReserved(h.UnreservedAddr))

	// Control: a witness the snapshot build never collected into cannot serve
	// the rebuild - pins that the positive case above is not vacuous.
	empty, err := stateless.NewWitness(context, headers)
	require.NoError(t, err)
	emptyDB := empty.MakeHashDB(diskWithRegistryCode(t))
	starved, err := state.New(root, state.NewDatabase(triedb.NewDatabase(emptyDB, triedb.HashDefaults), nil))
	if err == nil {
		_, err = registryreader.BuildSnapshot(h.Reader, starved, 0, parent.Hash(), 1)
	}
	require.Error(t, err, "an uncollected witness must not be able to serve the rebuild")
}
