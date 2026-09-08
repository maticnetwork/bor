package relay

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

func TestInit(t *testing.T) {
	t.Parallel()

	t.Run("all flags disabled creates neither component", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		require.Nil(t, rs.txRelay, "expected nil txRelay when all flags disabled")
		require.Nil(t, rs.privateTxStore, "expected nil privateTxStore when all flags disabled")
	})

	t.Run("enablePreconf only creates txRelay", func(t *testing.T) {
		rs := Init(true, false, false, false, nil)
		defer rs.Close()

		require.NotNil(t, rs.txRelay, "expected txRelay when enablePreconf is true")
		require.Nil(t, rs.privateTxStore, "expected nil privateTxStore when only enablePreconf is true")
	})

	t.Run("enablePrivateTx creates both components", func(t *testing.T) {
		rs := Init(false, true, false, false, nil)
		defer rs.Close()

		require.NotNil(t, rs.txRelay, "expected txRelay when enablePrivateTx is true")
		require.NotNil(t, rs.privateTxStore, "expected privateTxStore when enablePrivateTx is true")
	})

	t.Run("acceptPreconfTx only creates neither component", func(t *testing.T) {
		rs := Init(false, false, true, false, nil)
		defer rs.Close()

		require.Nil(t, rs.txRelay, "expected nil txRelay when only acceptPreconfTx is true")
		require.Nil(t, rs.privateTxStore, "expected nil privateTxStore when only acceptPreconfTx is true")
	})

	t.Run("acceptPrivateTx only creates store", func(t *testing.T) {
		rs := Init(false, false, false, true, nil)
		defer rs.Close()

		require.Nil(t, rs.txRelay, "expected nil txRelay when only acceptPrivateTx is true")
		require.NotNil(t, rs.privateTxStore, "expected privateTxStore when acceptPrivateTx is true")
	})

	t.Run("all flags enabled creates both components", func(t *testing.T) {
		server := newMockRpcServer()
		defer server.close()

		rs := Init(true, true, true, true, []string{server.server.URL})
		defer rs.Close()

		require.NotNil(t, rs.txRelay, "expected txRelay when all flags enabled")
		require.NotNil(t, rs.privateTxStore, "expected privateTxStore when all flags enabled")
	})

	t.Run("empty URLs creates txRelay with nil multiclient", func(t *testing.T) {
		rs := Init(true, false, false, false, []string{})
		defer rs.Close()

		require.NotNil(t, rs.txRelay, "expected txRelay to be created")
		require.Nil(t, rs.txRelay.multiclient, "expected nil multiclient with empty URLs")
	})

	t.Run("valid URLs creates txRelay with working multiclient", func(t *testing.T) {
		server := newMockRpcServer()
		defer server.close()

		rs := Init(true, false, false, false, []string{server.server.URL})
		defer rs.Close()

		require.NotNil(t, rs.txRelay, "expected txRelay to be created")
		require.NotNil(t, rs.txRelay.multiclient, "expected non-nil multiclient with valid URLs")
	})
}

func TestConfigAccessors(t *testing.T) {
	t.Parallel()

	rs := Init(true, false, true, false, nil)
	defer rs.Close()

	require.True(t, rs.PreconfEnabled(), "expected PreconfEnabled to be true")
	require.False(t, rs.PreconfRelayAvailable(), "expected preconf relay to be unavailable")
	require.False(t, rs.PrivateTxEnabled(), "expected PrivateTxEnabled to be false")
	require.True(t, rs.AcceptPreconfTxs(), "expected AcceptPreconfTxs to be true")
	require.False(t, rs.AcceptPrivateTxs(), "expected AcceptPrivateTxs to be false")

	server := newMockRpcServer()
	defer server.close()
	withProducer := Init(true, false, false, false, []string{server.server.URL})
	defer withProducer.Close()
	require.True(t, withProducer.PreconfRelayAvailable(), "expected preconf relay to be available")
}

func TestRelaySubmitPreconfTransaction(t *testing.T) {
	t.Parallel()

	t.Run("returns wrapped error when txRelay is nil", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := rs.SubmitPreconfTransaction(tx)
		require.Error(t, err)
		require.ErrorIs(t, err, errRelayNotConfigured)
		require.Contains(t, err.Error(), "request dropped")
	})

	t.Run("wraps underlying service error", func(t *testing.T) {
		// Empty URLs → nil multiclient → SubmitTransactionForPreconf returns errRpcClientUnavailable
		rs := Init(true, false, false, false, []string{})
		defer rs.Close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := rs.SubmitPreconfTransaction(tx)
		require.Error(t, err)
		require.ErrorIs(t, err, errRpcClientUnavailable)
		require.Contains(t, err.Error(), "request dropped")
	})

	t.Run("succeeds with working mock servers", func(t *testing.T) {
		server := newMockRpcServer()
		defer server.close()

		rs := Init(true, false, false, false, []string{server.server.URL})
		defer rs.Close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := rs.SubmitPreconfTransaction(tx)
		require.NoError(t, err)
	})
}

func TestRelaySubmitPrivateTransaction(t *testing.T) {
	t.Parallel()

	t.Run("returns wrapped error when txRelay is nil", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := rs.SubmitPrivateTransaction(tx)
		require.Error(t, err)
		require.ErrorIs(t, err, errRelayNotConfigured)
		require.Contains(t, err.Error(), "request dropped")
	})

	t.Run("returns unwrapped error from underlying service", func(t *testing.T) {
		// Empty URLs → nil multiclient → SubmitPrivateTx returns errRpcClientUnavailable
		// SubmitPrivateTransaction returns it as-is (no "request dropped:" wrapping)
		rs := Init(false, true, false, false, []string{})
		defer rs.Close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := rs.SubmitPrivateTransaction(tx)
		require.Error(t, err)
		require.ErrorIs(t, err, errRpcClientUnavailable)
		// Verify no extra wrapping was added
		require.NotContains(t, err.Error(), "request dropped")
	})

	t.Run("succeeds with working mock servers", func(t *testing.T) {
		server := newMockRpcServer()
		defer server.close()

		rs := Init(false, true, false, false, []string{server.server.URL})
		defer rs.Close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := rs.SubmitPrivateTransaction(tx)
		require.NoError(t, err)
	})
}

func TestRelayCheckPreconfStatus(t *testing.T) {
	t.Parallel()

	t.Run("returns wrapped error when txRelay is nil", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		hash := common.HexToHash("0x1")
		preconf, err := rs.CheckPreconfStatus(hash)
		require.Error(t, err)
		require.False(t, preconf)
		require.ErrorIs(t, err, errRelayNotConfigured)
		require.Contains(t, err.Error(), "request dropped")
	})

	t.Run("wraps underlying service error", func(t *testing.T) {
		// Empty URLs → nil multiclient → CheckTxPreconfStatus returns errRpcClientUnavailable
		rs := Init(true, false, false, false, []string{})
		defer rs.Close()

		hash := common.HexToHash("0x1")
		preconf, err := rs.CheckPreconfStatus(hash)
		require.Error(t, err)
		require.False(t, preconf)
		require.ErrorIs(t, err, errRpcClientUnavailable)
		require.Contains(t, err.Error(), "unable to offer preconf")
	})

	t.Run("succeeds with working mock servers", func(t *testing.T) {
		// Default mock server returns TxStatusPending for txpool_txStatus
		server := newMockRpcServer()
		defer server.close()

		rs := Init(true, false, false, false, []string{server.server.URL})
		defer rs.Close()

		hash := common.HexToHash("0x1")
		preconf, err := rs.CheckPreconfStatus(hash)
		require.NoError(t, err)
		require.True(t, preconf, "expected preconfirmed when mock server returns TxStatusPending")
	})
}

func TestRelayClose(t *testing.T) {
	t.Parallel()

	t.Run("no panic when both components are nil", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		require.Nil(t, rs.txRelay)
		require.Nil(t, rs.privateTxStore)

		require.NotPanics(t, func() {
			rs.Close()
		})
	})

	t.Run("no panic when both components exist", func(t *testing.T) {
		server := newMockRpcServer()
		defer server.close()

		rs := Init(true, true, true, true, []string{server.server.URL})
		require.NotNil(t, rs.txRelay)
		require.NotNil(t, rs.privateTxStore)

		require.NotPanics(t, func() {
			rs.Close()
		})
	})

	t.Run("no panic when only privateTxStore exists", func(t *testing.T) {
		rs := Init(false, false, false, true, nil)
		require.Nil(t, rs.txRelay)
		require.NotNil(t, rs.privateTxStore)

		require.NotPanics(t, func() {
			rs.Close()
		})
	})
}

func TestGetPrivateTxGetter(t *testing.T) {
	t.Parallel()

	t.Run("returns nil interface when store is nil", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		getter := rs.GetPrivateTxGetter()
		require.Nil(t, getter, "expected true nil interface when privateTxStore is nil")
	})

	t.Run("returns working getter when store exists", func(t *testing.T) {
		rs := Init(false, false, false, true, nil)
		defer rs.Close()

		getter := rs.GetPrivateTxGetter()
		require.NotNil(t, getter, "expected non-nil getter when privateTxStore exists")

		// Verify the getter works: add a hash, check it, purge it, check again
		hash := common.HexToHash("0xabc")
		require.False(t, getter.IsTxPrivate(hash), "expected unknown hash to not be private")

		rs.RecordPrivateTx(hash)
		require.True(t, getter.IsTxPrivate(hash), "expected recorded hash to be private")
	})
}

func TestRelayNilSafety(t *testing.T) {
	t.Parallel()

	t.Run("RecordPrivateTx with nil store", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		require.Nil(t, rs.privateTxStore)
		require.NotPanics(t, func() {
			rs.RecordPrivateTx(common.HexToHash("0x1"))
		})
	})

	t.Run("PurgePrivateTx with nil store", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		require.Nil(t, rs.privateTxStore)
		require.NotPanics(t, func() {
			rs.PurgePrivateTx(common.HexToHash("0x1"))
		})
	})

	t.Run("SetTxGetter with nil txRelay", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		require.Nil(t, rs.txRelay)
		require.NotPanics(t, func() {
			rs.SetTxGetter(func(hash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
				return false, nil, common.Hash{}, 0, 0
			})
		})
	})

	t.Run("SetchainEventSubFn with nil store", func(t *testing.T) {
		rs := Init(false, false, false, false, nil)
		defer rs.Close()

		require.Nil(t, rs.privateTxStore)
		require.NotPanics(t, func() {
			rs.SetchainEventSubFn(nil)
		})
	})
}

func TestRecordAndPurgePrivateTx(t *testing.T) {
	t.Parallel()

	rs := Init(false, false, false, true, nil)
	defer rs.Close()

	getter := rs.GetPrivateTxGetter()
	require.NotNil(t, getter)

	hash := common.HexToHash("0xdef")

	// Initially not present
	require.False(t, getter.IsTxPrivate(hash), "expected hash to not be private initially")

	// Record and verify
	rs.RecordPrivateTx(hash)
	require.True(t, getter.IsTxPrivate(hash), "expected hash to be private after recording")

	// Purge and verify
	rs.PurgePrivateTx(hash)
	require.False(t, getter.IsTxPrivate(hash), "expected hash to not be private after purging")
}
