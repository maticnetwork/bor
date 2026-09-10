// Copyright 2025 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package txpool

import (
	"crypto/ecdsa"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

func TestValidateTransactionEIP2681(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	head := &types.Header{
		Number:     big.NewInt(1),
		GasLimit:   5000000,
		Time:       1,
		Difficulty: big.NewInt(1),
	}

	signer := types.LatestSigner(params.TestChainConfig)

	// Create validation options
	opts := &ValidationOptions{
		Config:       params.TestChainConfig,
		Accept:       0xFF, // Accept all transaction types
		MaxSize:      32 * 1024,
		MaxBlobCount: 6,
		MinTip:       big.NewInt(0),
	}

	tests := []struct {
		name    string
		nonce   uint64
		wantErr error
	}{
		{
			name:    "normal nonce",
			nonce:   42,
			wantErr: nil,
		},
		{
			name:    "max allowed nonce (2^64-2)",
			nonce:   math.MaxUint64 - 1,
			wantErr: nil,
		},
		{
			name:    "EIP-2681 nonce overflow (2^64-1)",
			nonce:   math.MaxUint64,
			wantErr: core.ErrNonceMax,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := createTestTransaction(key, tt.nonce)
			err := ValidateTransaction(tx, head, signer, opts)

			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("ValidateTransaction() error = %v, wantErr nil", err)
				}
			} else {
				if err == nil {
					t.Errorf("ValidateTransaction() error = nil, wantErr %v", tt.wantErr)
				} else if !errors.Is(err, tt.wantErr) {
					t.Errorf("ValidateTransaction() error = %v, wantErr %v", err, tt.wantErr)
				}
			}
		})
	}
}

// createTestTransaction creates a basic transaction for testing
func createTestTransaction(key *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
	to := common.HexToAddress("0x0000000000000000000000000000000000000001")

	txdata := &types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    big.NewInt(1000),
		Gas:      21000,
		GasPrice: big.NewInt(1),
		Data:     nil,
	}

	tx := types.NewTx(txdata)
	signedTx, _ := types.SignTx(tx, types.HomesteadSigner{}, key)
	return signedTx
}

// fallbackFeeTestTx builds a dynamic-fee transaction whose full cost (value +
// gas*feeCap) vastly exceeds any balance used in the tests below, while its
// value alone is affordable. It models a reserved-blockspace sender that
// holds value but not gas headroom (Primer §6.3).
func fallbackFeeTestTx(key *ecdsa.PrivateKey, nonce uint64, value *big.Int) *types.Transaction {
	to := common.HexToAddress("0x0000000000000000000000000000000000000001")
	tx, _ := types.SignNewTx(key, types.LatestSigner(params.TestChainConfig), &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(1_000_000_000),
		Gas:       100_000, // gas*feeCap = 1e14, far above every balance below
		To:        &to,
		Value:     value,
	})
	return tx
}

// stateWithBalance returns an in-memory StateDB with addr funded at balance.
func stateWithBalance(t *testing.T, addr common.Address, balance int64) *state.StateDB {
	t.Helper()
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	sdb.AddBalance(addr, uint256.NewInt(uint64(balance)), tracing.BalanceChangeUnspecified)
	return sdb
}

// noPriorTx is a ValidationOptionsWithState.ExistingCost that always reports
// no pooled transaction at the given nonce, i.e. every admission below is a
// fresh nonce rather than a replacement.
func noPriorTx(common.Address, uint64) *big.Int { return nil }

// TestValidateTransactionWithStateEffectiveCost pins EffectiveCost's balance-
// check override: a fallback-fee transaction whose balance covers only its
// value (not its full cost) is admitted when EffectiveCost prices it at
// value, and rejected with the ordinary tx.Cost() pricing when it doesn't.
func TestValidateTransactionWithStateEffectiveCost(t *testing.T) {
	t.Parallel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	signer := types.LatestSigner(params.TestChainConfig)

	value := big.NewInt(1_000)
	tx := fallbackFeeTestTx(key, 0, value)
	if tx.Cost().Cmp(big.NewInt(2_000)) <= 0 {
		t.Fatalf("test fixture must have a full cost far above the value: cost=%s", tx.Cost())
	}

	tests := []struct {
		name          string
		effectiveCost func(common.Address, *types.Transaction) *big.Int
		wantErr       error
	}{
		{
			name:          "reserved-aware EffectiveCost admits a value-only balance",
			effectiveCost: func(_ common.Address, tx *types.Transaction) *big.Int { return tx.Value() },
			wantErr:       nil,
		},
		{
			name:          "nil EffectiveCost falls back to tx.Cost() and rejects",
			effectiveCost: nil,
			wantErr:       core.ErrInsufficientFunds,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Balance covers the value with room to spare, but nowhere near
			// the full cost (gas*feeCap alone is 1e14).
			sdb := stateWithBalance(t, addr, 2_000)
			opts := &ValidationOptionsWithState{
				State:               sdb,
				EffectiveCost:       tt.effectiveCost,
				ExistingExpenditure: func(common.Address) *big.Int { return new(big.Int) },
				ExistingCost:        noPriorTx,
			}
			err := ValidateTransactionWithState(tx, signer, opts)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateTransactionWithState() error = %v, want nil", err)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateTransactionWithState() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateTransactionWithStateEffectiveCostOverdraft pins overdraft
// accounting across multiple pooled fallback-fee transactions: each one's own
// value fits the balance, and the pool-side ExistingExpenditure implementation
// sums prior *effective* costs (as legacypool's does for a reserved sender),
// so admission must fail once the sum of values, not the sum of full costs,
// first exceeds the balance.
func TestValidateTransactionWithStateEffectiveCostOverdraft(t *testing.T) {
	t.Parallel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	signer := types.LatestSigner(params.TestChainConfig)

	const balance = 2_500
	sdb := stateWithBalance(t, addr, balance)

	var pooledValue big.Int // running sum of admitted txs' effective (value) cost
	opts := &ValidationOptionsWithState{
		State:               sdb,
		EffectiveCost:       func(_ common.Address, tx *types.Transaction) *big.Int { return tx.Value() },
		ExistingExpenditure: func(common.Address) *big.Int { return new(big.Int).Set(&pooledValue) },
		ExistingCost:        noPriorTx,
	}

	// Two 1000-value txs fit (1000, then 2000 <= 2500); the third pushes the
	// running sum to 3000, over the 2500 balance, even though its own value
	// (1000) is individually affordable and its full cost is never reached.
	wantErrs := []error{nil, nil, core.ErrInsufficientFunds}
	for i, wantErr := range wantErrs {
		tx := fallbackFeeTestTx(key, uint64(i), big.NewInt(1_000))
		err := ValidateTransactionWithState(tx, signer, opts)
		if wantErr == nil {
			if err != nil {
				t.Fatalf("tx %d: error = %v, want nil", i, err)
			}
			pooledValue.Add(&pooledValue, tx.Value())
		} else if !errors.Is(err, wantErr) {
			t.Fatalf("tx %d: error = %v, want %v", i, err, wantErr)
		}
	}
}

// TestValidateTransactionWithStateEffectiveCostReplacement pins that the
// replacement overdraft math (the cost-delta "bump" between a new tx and the
// pooled one it targets) is computed on the effective-cost basis end to end:
// both ExistingCost's prior value and EffectiveCost's new value, not
// tx.Cost(). This is distinct from list.Add's fee-field replacement threshold
// (which stays fee-priced and is covered in legacypool's list tests) — this
// bump is the pool's balance-overdraft accounting for the nonce being
// replaced.
func TestValidateTransactionWithStateEffectiveCostReplacement(t *testing.T) {
	t.Parallel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	signer := types.LatestSigner(params.TestChainConfig)

	// A single pooled tx at nonce 0 whose effective (value) cost was 1000.
	prevCost := big.NewInt(1_000)
	baseOpts := func(balance int64) *ValidationOptionsWithState {
		return &ValidationOptionsWithState{
			State:               stateWithBalance(t, addr, balance),
			EffectiveCost:       func(_ common.Address, tx *types.Transaction) *big.Int { return tx.Value() },
			ExistingExpenditure: func(common.Address) *big.Int { return new(big.Int).Set(prevCost) },
			ExistingCost: func(common.Address, uint64) *big.Int {
				return new(big.Int).Set(prevCost)
			},
		}
	}

	tests := []struct {
		name    string
		balance int64
		wantErr error
	}{
		// need = spent(1000) + bump(1500-1000=500) = 1500 <= 2000.
		{name: "bump fits the balance", balance: 2_000, wantErr: nil},
		// need = 1500 > 1400.
		{name: "bump overdraws the balance", balance: 1_400, wantErr: core.ErrInsufficientFunds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replacement := fallbackFeeTestTx(key, 0, big.NewInt(1_500))
			err := ValidateTransactionWithState(replacement, signer, baseOpts(tt.balance))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
