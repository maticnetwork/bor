package registryreader

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var classifyChainID = big.NewInt(1337)

func classifySigner() types.Signer { return types.LatestSignerForChainID(classifyChainID) }

// signedTx builds a signed dynamic-fee tx with the given nonce and declared gas.
func signedTx(t *testing.T, key *ecdsa.PrivateKey, nonce, gas uint64) *types.Transaction {
	t.Helper()
	to := common.HexToAddress("0xdead")
	tx, err := types.SignNewTx(key, classifySigner(), &types.DynamicFeeTx{
		ChainID:   classifyChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(0),
		Gas:       gas,
		To:        &to,
	})
	require.NoError(t, err)
	return tx
}

// snapOf builds a snapshot mapping each address to a client with the given
// quota. capacity is the reported reserved capacity (registry invariant: the
// sum of active client quotas); classification never lets it bind.
func snapOf(capacity uint64, clients map[uint64][]common.Address, quota map[uint64]uint64) *Snapshot {
	m := make(map[common.Address]Client)
	for id, addrs := range clients {
		for _, a := range addrs {
			m[a] = Client{ID: id, GasQuota: quota[id]}
		}
	}
	return NewSnapshot(common.Hash{}, capacity, m)
}

func TestClassifyReserved(t *testing.T) {
	t.Parallel()

	keyA1, _ := crypto.GenerateKey()
	keyA2, _ := crypto.GenerateKey()
	keyB1, _ := crypto.GenerateKey()
	keyX, _ := crypto.GenerateKey()
	a1 := crypto.PubkeyToAddress(keyA1.PublicKey)
	a2 := crypto.PubkeyToAddress(keyA2.PublicKey)
	b1 := crypto.PubkeyToAddress(keyB1.PublicKey)

	signer := classifySigner()

	has := func(set map[ReservedKey]struct{}, addr common.Address, nonce uint64) bool {
		_, ok := set[ReservedKey{From: addr, Nonce: nonce}]
		return ok
	}

	t.Run("nil snapshot classifies nothing", func(t *testing.T) {
		t.Parallel()
		got, usage := ClassifyReserved([]*types.Transaction{signedTx(t, keyA1, 0, 21000)}, signer, nil)
		require.Nil(t, got)
		require.Nil(t, usage)
	})

	t.Run("empty snapshot classifies nothing", func(t *testing.T) {
		t.Parallel()
		empty := NewSnapshot(common.Hash{}, 0, map[common.Address]Client{})
		got, usage := ClassifyReserved([]*types.Transaction{signedTx(t, keyA1, 0, 21000)}, signer, empty)
		require.Nil(t, got)
		require.Nil(t, usage)
	})

	t.Run("empty txs still reports zero usage per client", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(1_500_000, map[uint64][]common.Address{1: {a1}, 2: {b1}}, map[uint64]uint64{1: 1_000_000, 2: 500_000})
		got, usage := ClassifyReserved(nil, signer, snap)
		require.Nil(t, got)
		require.Equal(t, map[uint64]ClientUsage{
			1: {Used: 0, Quota: 1_000_000},
			2: {Used: 0, Quota: 500_000},
		}, usage)
	})

	t.Run("no registered senders present", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(1_000_000, map[uint64][]common.Address{1: {a1}}, map[uint64]uint64{1: 1_000_000})
		got, _ := ClassifyReserved([]*types.Transaction{signedTx(t, keyX, 0, 21000)}, signer, snap)
		require.Empty(t, got)
	})

	t.Run("all under quota are reserved", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(1_000_000, map[uint64][]common.Address{1: {a1}}, map[uint64]uint64{1: 1_000_000})
		txs := []*types.Transaction{signedTx(t, keyA1, 0, 100), signedTx(t, keyA1, 1, 100)}
		got, _ := ClassifyReserved(txs, signer, snap)
		require.Len(t, got, 2)
		require.True(t, has(got, a1, 0))
		require.True(t, has(got, a1, 1))
	})

	t.Run("first quota breach blocks sender's later nonces", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(1000, map[uint64][]common.Address{1: {a1}}, map[uint64]uint64{1: 1000})
		txs := []*types.Transaction{
			signedTx(t, keyA1, 0, 600),
			signedTx(t, keyA1, 1, 600),
			signedTx(t, keyA1, 2, 600),
		}
		got, _ := ClassifyReserved(txs, signer, snap)
		require.Len(t, got, 1)
		require.True(t, has(got, a1, 0))
		require.False(t, has(got, a1, 1))
		require.False(t, has(got, a1, 2))
	})

	t.Run("zero quota overflows everything", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(0, map[uint64][]common.Address{1: {a1}}, map[uint64]uint64{1: 0})
		got, _ := ClassifyReserved([]*types.Transaction{signedTx(t, keyA1, 0, 1)}, signer, snap)
		require.Empty(t, got)
	})

	t.Run("non-registered sender txs stay normal", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(1_000_000, map[uint64][]common.Address{1: {a1}}, map[uint64]uint64{1: 1_000_000})
		txs := []*types.Transaction{signedTx(t, keyA1, 0, 100), signedTx(t, keyX, 0, 100)}
		got, _ := ClassifyReserved(txs, signer, snap)
		require.Len(t, got, 1)
		require.True(t, has(got, a1, 0))
	})

	t.Run("per-client quota is independent", func(t *testing.T) {
		t.Parallel()
		// Two clients, each quota 100, capacity = sum (200, the registry
		// invariant). Both single 100-gas txs are reserved - no global ceiling.
		snap := snapOf(200,
			map[uint64][]common.Address{1: {a1}, 2: {b1}},
			map[uint64]uint64{1: 100, 2: 100})
		txs := []*types.Transaction{signedTx(t, keyA1, 0, 100), signedTx(t, keyB1, 0, 100)}
		got, _ := ClassifyReserved(txs, signer, snap)
		require.Len(t, got, 2)
		require.True(t, has(got, a1, 0))
		require.True(t, has(got, b1, 0))
	})

	t.Run("classification is order-independent across clients", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(200,
			map[uint64][]common.Address{1: {a1}, 2: {b1}},
			map[uint64]uint64{1: 100, 2: 100})
		ab, _ := ClassifyReserved([]*types.Transaction{signedTx(t, keyA1, 0, 100), signedTx(t, keyB1, 0, 100)}, signer, snap)
		ba, _ := ClassifyReserved([]*types.Transaction{signedTx(t, keyB1, 0, 100), signedTx(t, keyA1, 0, 100)}, signer, snap)
		require.Len(t, ab, 2)
		require.Len(t, ba, 2)
		require.Equal(t, ab, ba)
	})

	t.Run("dropping a reserved tx frees quota for a later same-client tx (skip parity)", func(t *testing.T) {
		t.Parallel()
		// Classification is a pure function of the block's
		// contents. With quota 100 (one 100-gas tx), if a1's first tx is present
		// it wins the quota and a2 (same client) overflows; if a1's tx is absent
		// (producer skip), a2 wins the freed quota. The producer advances the
		// identical walk over committed txs, so it reaches the same conclusion.
		snap := snapOf(100, map[uint64][]common.Address{1: {a1, a2}}, map[uint64]uint64{1: 100})

		withBoth, _ := ClassifyReserved([]*types.Transaction{signedTx(t, keyA1, 0, 100), signedTx(t, keyA2, 0, 100)}, signer, snap)
		require.Len(t, withBoth, 1)
		require.True(t, has(withBoth, a1, 0), "first tx wins the shared client quota")
		require.False(t, has(withBoth, a2, 0))

		withoutFirst, _ := ClassifyReserved([]*types.Transaction{signedTx(t, keyA2, 0, 100)}, signer, snap)
		require.Len(t, withoutFirst, 1)
		require.True(t, has(withoutFirst, a2, 0), "freed quota reclassifies the survivor")
	})

	t.Run("usage attributes declared gas per client across multiple clients", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(300,
			map[uint64][]common.Address{1: {a1}, 2: {b1}},
			map[uint64]uint64{1: 100, 2: 200})
		txs := []*types.Transaction{signedTx(t, keyA1, 0, 60), signedTx(t, keyB1, 0, 150)}
		_, usage := ClassifyReserved(txs, signer, snap)
		require.Equal(t, ClientUsage{Used: 60, Quota: 100}, usage[1])
		require.Equal(t, ClientUsage{Used: 150, Quota: 200}, usage[2])
	})

	t.Run("an overflowed transaction's declared gas is not counted toward usage", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(100, map[uint64][]common.Address{1: {a1}}, map[uint64]uint64{1: 100})
		txs := []*types.Transaction{signedTx(t, keyA1, 0, 60), signedTx(t, keyA1, 1, 60)}
		got, usage := ClassifyReserved(txs, signer, snap)
		require.Len(t, got, 1, "only the first tx fits under quota")
		require.Equal(t, ClientUsage{Used: 60, Quota: 100}, usage[1], "the breaching second tx must not add to Used")
	})

	t.Run("an idle client with no transactions in the block reports zero Used", func(t *testing.T) {
		t.Parallel()
		snap := snapOf(150,
			map[uint64][]common.Address{1: {a1}, 2: {b1}},
			map[uint64]uint64{1: 100, 2: 50})
		txs := []*types.Transaction{signedTx(t, keyA1, 0, 50)}
		_, usage := ClassifyReserved(txs, signer, snap)
		require.Equal(t, ClientUsage{Used: 50, Quota: 100}, usage[1])
		require.Equal(t, ClientUsage{Used: 0, Quota: 50}, usage[2], "idle client still appears, with zero Used")
	})
}

// TestClassifyReservedMatrix is the exhaustive case matrix: 1..n clients, in
// region (within quota) vs out (non-registered), overflow (beyond quota),
// underflow (quota not filled), multiple senders per client, and mixed blocks.
// Each case lists the block's txs in order and the expected reserved (fee-free)
// subset; anything not listed is expected to pay normal fees.
func TestClassifyReservedMatrix(t *testing.T) {
	t.Parallel()

	// Deterministic keys per logical name.
	keys := map[string]*ecdsa.PrivateKey{}
	addrs := map[string]common.Address{}
	for _, name := range []string{"c1a", "c1b", "c2a", "c2b", "c3a", "stranger"} {
		k, _ := crypto.GenerateKey()
		keys[name] = k
		addrs[name] = crypto.PubkeyToAddress(k.PublicKey)
	}
	signer := classifySigner()

	// clientsOf maps clientID -> member names.
	type tx struct {
		who   string
		nonce uint64
		gas   uint64
	}
	cases := []struct {
		name    string
		clients map[uint64][]string
		quota   map[uint64]uint64
		txs     []tx
		want    []tx // expected reserved subset (who+nonce identify; gas ignored)
	}{
		{
			name:    "1 client underflow: quota not filled, all reserved",
			clients: map[uint64][]string{1: {"c1a"}},
			quota:   map[uint64]uint64{1: 1_000_000},
			txs:     []tx{{"c1a", 0, 100}, {"c1a", 1, 100}},
			want:    []tx{{"c1a", 0, 0}, {"c1a", 1, 0}},
		},
		{
			name:    "1 client exact fill: all reserved",
			clients: map[uint64][]string{1: {"c1a"}},
			quota:   map[uint64]uint64{1: 200},
			txs:     []tx{{"c1a", 0, 100}, {"c1a", 1, 100}},
			want:    []tx{{"c1a", 0, 0}, {"c1a", 1, 0}},
		},
		{
			name:    "1 client overflow: breach blocks later nonces",
			clients: map[uint64][]string{1: {"c1a"}},
			quota:   map[uint64]uint64{1: 150},
			txs:     []tx{{"c1a", 0, 100}, {"c1a", 1, 100}, {"c1a", 2, 40}},
			want:    []tx{{"c1a", 0, 0}}, // nonce1 breaches (100+100>150) → blocks nonce2 too
		},
		{
			name:    "1 client, 2 senders share quota; second sender overflows",
			clients: map[uint64][]string{1: {"c1a", "c1b"}},
			quota:   map[uint64]uint64{1: 100},
			txs:     []tx{{"c1a", 0, 100}, {"c1b", 0, 100}},
			want:    []tx{{"c1a", 0, 0}}, // c1a fills the shared quota; c1b overflows
		},
		{
			name:    "out of region: non-registered sender never reserved",
			clients: map[uint64][]string{1: {"c1a"}},
			quota:   map[uint64]uint64{1: 1_000_000},
			txs:     []tx{{"stranger", 0, 100}, {"c1a", 0, 100}},
			want:    []tx{{"c1a", 0, 0}},
		},
		{
			name:    "3 clients independent quotas",
			clients: map[uint64][]string{1: {"c1a"}, 2: {"c2a"}, 3: {"c3a"}},
			quota:   map[uint64]uint64{1: 100, 2: 100, 3: 100},
			txs:     []tx{{"c1a", 0, 100}, {"c2a", 0, 100}, {"c3a", 0, 100}, {"stranger", 0, 100}},
			want:    []tx{{"c1a", 0, 0}, {"c2a", 0, 0}, {"c3a", 0, 0}},
		},
		{
			name:    "multi-client mixed: each client has in-quota + overflow, interleaved with stranger",
			clients: map[uint64][]string{1: {"c1a"}, 2: {"c2a"}},
			quota:   map[uint64]uint64{1: 100, 2: 250},
			txs: []tx{
				{"c1a", 0, 100},      // reserved (fills client 1)
				{"c2a", 0, 100},      // reserved (client 2: 100/250)
				{"stranger", 0, 100}, // normal
				{"c1a", 1, 100},      // overflow (client 1 full)
				{"c2a", 1, 100},      // reserved (client 2: 200/250)
				{"c2a", 2, 100},      // overflow (client 2: 200+100>250) → blocks
			},
			want: []tx{{"c1a", 0, 0}, {"c2a", 0, 0}, {"c2a", 1, 0}},
		},
		{
			name:    "empty block",
			clients: map[uint64][]string{1: {"c1a"}},
			quota:   map[uint64]uint64{1: 100},
			txs:     nil,
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Build snapshot: capacity = sum of quotas (registry invariant).
			var capacity uint64
			for _, q := range tc.quota {
				capacity += q
			}
			named := make(map[uint64][]common.Address)
			for id, names := range tc.clients {
				for _, n := range names {
					named[id] = append(named[id], addrs[n])
				}
			}
			snap := snapOf(capacity, named, tc.quota)

			var block []*types.Transaction
			for _, x := range tc.txs {
				block = append(block, signedTx(t, keys[x.who], x.nonce, x.gas))
			}

			got, _ := ClassifyReserved(block, signer, snap)
			require.Len(t, got, len(tc.want), "reserved count")
			for _, w := range tc.want {
				_, ok := got[ReservedKey{From: addrs[w.who], Nonce: w.nonce}]
				require.Truef(t, ok, "expected %s nonce %d reserved", w.who, w.nonce)
			}
		})
	}
}

// TestReservedWalk covers the incremental scan the producer drives directly.
func TestReservedWalk(t *testing.T) {
	t.Parallel()
	a := common.HexToAddress("0x0a")
	b := common.HexToAddress("0x0b")
	x := common.HexToAddress("0xff")
	snap := snapOf(150, map[uint64][]common.Address{1: {a, b}}, map[uint64]uint64{1: 150})

	w := NewReservedWalk(snap)
	require.True(t, w.Reserved(a, 100), "a fits under 150")
	require.False(t, w.Reserved(a, 100), "a breaches; blocked for the rest")
	require.False(t, w.Reserved(b, 100), "client quota exhausted (100 used, 50 left, 100>50)")
	require.False(t, w.Reserved(x, 10), "x not registered")

	require.False(t, (*ReservedWalk)(nil).Reserved(a, 1), "nil walk classifies nothing")
	require.False(t, NewReservedWalk(nil).Reserved(a, 1), "nil snapshot classifies nothing")
}
