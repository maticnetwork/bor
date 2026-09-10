// Copyright 2021 The go-ethereum Authors
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

package eip1559

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"
)

// copyConfig does a _shallow_ copy of a given config. Safe to set new values, but
// do not use e.g. SetInt() on the numbers. For testing only
func copyConfig(original *params.ChainConfig) *params.ChainConfig {
	config := &params.ChainConfig{
		ChainID:                 original.ChainID,
		HomesteadBlock:          original.HomesteadBlock,
		DAOForkBlock:            original.DAOForkBlock,
		DAOForkSupport:          original.DAOForkSupport,
		EIP150Block:             original.EIP150Block,
		EIP155Block:             original.EIP155Block,
		EIP158Block:             original.EIP158Block,
		ByzantiumBlock:          original.ByzantiumBlock,
		ConstantinopleBlock:     original.ConstantinopleBlock,
		PetersburgBlock:         original.PetersburgBlock,
		IstanbulBlock:           original.IstanbulBlock,
		MuirGlacierBlock:        original.MuirGlacierBlock,
		BerlinBlock:             original.BerlinBlock,
		LondonBlock:             original.LondonBlock,
		TerminalTotalDifficulty: original.TerminalTotalDifficulty,
		Ethash:                  original.Ethash,
		Clique:                  original.Clique,
	}
	if original.Bor != nil {
		config.Bor = &params.BorConfig{
			Period:                          original.Bor.Period,
			ProducerDelay:                   original.Bor.ProducerDelay,
			Sprint:                          original.Bor.Sprint,
			BackupMultiplier:                original.Bor.BackupMultiplier,
			ValidatorContract:               original.Bor.ValidatorContract,
			StateReceiverContract:           original.Bor.StateReceiverContract,
			ReservedRegistryContract:        original.Bor.ReservedRegistryContract,
			OverrideStateSyncRecords:        original.Bor.OverrideStateSyncRecords,
			OverrideStateSyncRecordsInRange: original.Bor.OverrideStateSyncRecordsInRange,
			BlockAlloc:                      original.Bor.BlockAlloc,
			BurntContract:                   original.Bor.BurntContract,
			Coinbase:                        original.Bor.Coinbase,
			SkipValidatorByteCheck:          original.Bor.SkipValidatorByteCheck,
			JaipurBlock:                     original.Bor.JaipurBlock,
			DelhiBlock:                      original.Bor.DelhiBlock,
			IndoreBlock:                     original.Bor.IndoreBlock,
			StateSyncConfirmationDelay:      original.Bor.StateSyncConfirmationDelay,
			AhmedabadBlock:                  original.Bor.AhmedabadBlock,
			BhilaiBlock:                     original.Bor.BhilaiBlock,
			RioBlock:                        original.Bor.RioBlock,
			MadhugiriBlock:                  original.Bor.MadhugiriBlock,
			MadhugiriProBlock:               original.Bor.MadhugiriProBlock,
			DandeliBlock:                    original.Bor.DandeliBlock,
			LisovoBlock:                     original.Bor.LisovoBlock,
		}
	}
	return config
}

func config() *params.ChainConfig {
	config := copyConfig(params.TestChainConfig)
	config.LondonBlock = big.NewInt(5)
	config.Bor.DelhiBlock = big.NewInt(8)

	return config
}

// TestBlockGasLimits tests the gasLimit checks for blocks both across
// the EIP-1559 boundary and post-1559 blocks
func TestBlockGasLimits(t *testing.T) {
	initial := new(big.Int).SetUint64(params.InitialBaseFee)

	for i, tc := range []struct {
		pGasLimit uint64
		pNum      int64
		gasLimit  uint64
		ok        bool
	}{
		// Transitions from non-london to london
		{10000000, 4, 20000000, true},  // No change
		{10000000, 4, 20019530, true},  // Upper limit
		{10000000, 4, 20019531, false}, // Upper +1
		{10000000, 4, 19980470, true},  // Lower limit
		{10000000, 4, 19980469, false}, // Lower limit -1
		// London to London
		{20000000, 5, 20000000, true},
		{20000000, 5, 20019530, true},  // Upper limit
		{20000000, 5, 20019531, false}, // Upper limit +1
		{20000000, 5, 19980470, true},  // Lower limit
		{20000000, 5, 19980469, false}, // Lower limit -1
		{40000000, 5, 40039061, true},  // Upper limit
		{40000000, 5, 40039062, false}, // Upper limit +1
		{40000000, 5, 39960939, true},  // lower limit
		{40000000, 5, 39960938, false}, // Lower limit -1
	} {
		parent := &types.Header{
			GasUsed:  tc.pGasLimit / 2,
			GasLimit: tc.pGasLimit,
			BaseFee:  initial,
			Number:   big.NewInt(tc.pNum),
		}
		header := &types.Header{
			GasUsed:  tc.gasLimit / 2,
			GasLimit: tc.gasLimit,
			BaseFee:  initial,
			Number:   big.NewInt(tc.pNum + 1),
		}
		err := VerifyEIP1559Header(config(), parent, header)
		if tc.ok && err != nil {
			t.Errorf("test %d: Expected valid header: %s", i, err)
		}

		if !tc.ok && err == nil {
			t.Errorf("test %d: Expected invalid header", i)
		}
	}
}

// TestCalcBaseFee assumes all blocks are 1559-blocks
func TestCalcBaseFee(t *testing.T) {
	t.Parallel()

	tests := []struct {
		parentBaseFee   int64
		parentGasLimit  uint64
		parentGasUsed   uint64
		expectedBaseFee int64
	}{
		{params.InitialBaseFee, 20000000, 10000000, params.InitialBaseFee}, // usage == target
		{params.InitialBaseFee, 20000000, 9000000, 987500000},              // usage below target
		{params.InitialBaseFee, 20000000, 11000000, 1012500000},            // usage above target
		{params.InitialBaseFee, 20000000, 20000000, 1125000000},            // usage full
		{params.InitialBaseFee, 20000000, 0, 875000000},                    // usage 0
	}
	for i, test := range tests {
		parent := &types.Header{
			Number:   big.NewInt(6),
			GasLimit: test.parentGasLimit,
			GasUsed:  test.parentGasUsed,
			BaseFee:  big.NewInt(test.parentBaseFee),
		}
		if have, want := CalcBaseFee(config(), parent), big.NewInt(test.expectedBaseFee); have.Cmp(want) != 0 {
			t.Errorf("test %d: have %d  want %d, ", i, have, want)
		}
	}
}

// TestCalcBaseFeeDelhi assumes all blocks are 1559-blocks and uses
// parameters post Delhi Hard Fork
func TestCalcBaseFeeDelhi(t *testing.T) {
	t.Parallel()

	// Delhi HF kicks in at block 8
	testConfig := copyConfig(config())

	tests := []struct {
		parentBaseFee   int64
		parentGasLimit  uint64
		parentGasUsed   uint64
		expectedBaseFee int64
	}{
		{params.InitialBaseFee, 20000000, 10000000, params.InitialBaseFee}, // usage == target
		{params.InitialBaseFee, 20000000, 9000000, 993750000},              // usage below target
		{params.InitialBaseFee, 20000000, 11000000, 1006250000},            // usage above target
		{params.InitialBaseFee, 20000000, 20000000, 1062500000},            // usage full
		{params.InitialBaseFee, 20000000, 0, 937500000},                    // usage 0

	}
	for i, test := range tests {
		parent := &types.Header{
			Number:   big.NewInt(8),
			GasLimit: test.parentGasLimit,
			GasUsed:  test.parentGasUsed,
			BaseFee:  big.NewInt(test.parentBaseFee),
		}
		if have, want := CalcBaseFee(testConfig, parent), big.NewInt(test.expectedBaseFee); have.Cmp(want) != 0 {
			t.Errorf("test %d: have %d  want %d, ", i, have, want)
		}
	}
}

// TestCalcBaseFeeBhilai assumes all blocks are 1559-blocks and uses
// parameters post Bhilai Hard Fork
func TestCalcBaseFeeBhilai(t *testing.T) {
	t.Parallel()

	// Bhilai HF kicks in at block 8
	testConfig := copyConfig(config())
	testConfig.Bor.BhilaiBlock = big.NewInt(8)

	tests := []struct {
		parentBaseFee   int64
		parentGasLimit  uint64
		parentGasUsed   uint64
		expectedBaseFee int64
	}{
		{params.InitialBaseFee, 20000000, 10000000, params.InitialBaseFee}, // usage == target
		{params.InitialBaseFee, 20000000, 9000000, 998437500},              // usage below target
		{params.InitialBaseFee, 20000000, 11000000, 1001562500},            // usage above target
		{params.InitialBaseFee, 20000000, 20000000, 1015625000},            // usage full
		{params.InitialBaseFee, 20000000, 0, 984375000},                    // usage 0

	}
	for i, test := range tests {
		parent := &types.Header{
			Number:   big.NewInt(8),
			GasLimit: test.parentGasLimit,
			GasUsed:  test.parentGasUsed,
			BaseFee:  big.NewInt(test.parentBaseFee),
		}
		if have, want := CalcBaseFee(testConfig, parent), big.NewInt(test.expectedBaseFee); have.Cmp(want) != 0 {
			t.Errorf("test %d: have %d  want %d, ", i, have, want)
		}
	}
}

// TestCalcBaseFeeNilParent tests that CalcBaseFee doesn't panic when
// the parent's BaseFee is nil.
func TestCalcBaseFeeNilParent(t *testing.T) {
	t.Parallel()

	testConfig := config()

	t.Run("nil baseFee for post-London parent returns InitialBaseFee", func(t *testing.T) {
		// Create a post-London parent header with nil BaseFee
		parent := &types.Header{
			Number:   big.NewInt(6), // Post-London because LondonBlock is 5 in test config
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  nil,
		}

		// CalcBaseFee should not panic but return InitialBaseFee
		result := CalcBaseFee(testConfig, parent)
		expected := big.NewInt(params.InitialBaseFee)

		require.NotNil(t, result, "CalcBaseFee should not return nil")
		require.Equal(t, expected, result,
			"CalcBaseFee should return InitialBaseFee when the parent's BaseFee is nil for post-London block")
	})

	t.Run("pre-London parent with nil baseFee returns InitialBaseFee", func(t *testing.T) {
		// Pre-London blocks should have nil BaseFee anyway
		parent := &types.Header{
			Number:   big.NewInt(4), // Pre-London because LondonBlock is 5 in test config
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  nil,
		}

		result := CalcBaseFee(testConfig, parent)
		expected := big.NewInt(params.InitialBaseFee)

		require.NotNil(t, result, "CalcBaseFee should not return nil")
		require.Equal(t, expected, result,
			"CalcBaseFee should return InitialBaseFee for first EIP-1559 block")
	})
}

// TestVerifyEIP1559HeaderNilParentBaseFee tests that VerifyEIP1559Header rejects post-London parents with nil BaseFee.
func TestVerifyEIP1559HeaderNilParentBaseFee(t *testing.T) {
	t.Parallel()

	testConfig := config()

	t.Run("post-London parent with nil BaseFee is rejected", func(t *testing.T) {
		// Malicious parent: post-London block with nil BaseFee
		parent := &types.Header{
			Number:   big.NewInt(6), // Post-London (LondonBlock is 5)
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  nil,
		}

		// Child header with valid BaseFee
		child := &types.Header{
			Number:   big.NewInt(7),
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}

		// VerifyEIP1559Header must reject due to nil parent's BaseFee
		err := VerifyEIP1559Header(testConfig, parent, child)
		require.Error(t, err, "VerifyEIP1559Header must reject nil parent's BaseFee")
		require.Contains(t, err.Error(), "parent header is missing baseFee",
			"Error message should indicate parent BaseFee is missing")
	})

	t.Run("pre-London parent with nil BaseFee is accepted", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(4), // Pre-London (LondonBlock is 5)
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  nil, // Expected for pre-London blocks
		}

		child := &types.Header{
			Number:   big.NewInt(5), // LondonBlock
			GasLimit: 40000000,      // parent.GasLimit * elasticityMultiplier = 20M * 2
			GasUsed:  20000000,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}

		err := VerifyEIP1559Header(testConfig, parent, child)
		require.NoError(t, err, "First London block with InitialBaseFee should be accepted")
	})

	t.Run("post-London parent with valid BaseFee is accepted", func(t *testing.T) {
		// Valid parent
		parent := &types.Header{
			Number:   big.NewInt(6), // Post-London (LondonBlock is 5)
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}

		// Valid child
		expectedBaseFee := CalcBaseFee(testConfig, parent)
		child := &types.Header{
			Number:   big.NewInt(7),
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  expectedBaseFee,
		}

		err := VerifyEIP1559Header(testConfig, parent, child)
		require.NoError(t, err, "Valid parent and child should be accepted")
	})
}

// TestBatchVerification tests that if a peer sends header batch [A, B] where A has nil BaseFee and future
// timestamp, and B is a child of A, it should not panic but return an error.
func TestBatchVerification(t *testing.T) {
	t.Parallel()

	testConfig := config()

	t.Run("batch A->B does not panic", func(t *testing.T) {
		// Header A: post-London, nil BaseFee, forwarded to child verification in batch
		headerA := &types.Header{
			Number:   big.NewInt(6), // Post-London (LondonBlock is 5)
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  nil,
		}

		// Header B: child of A, trying to exploit the nil BaseFee
		headerB := &types.Header{
			Number:   big.NewInt(7),
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}

		// verify B with A as parent doesn't panic but returns the expected error
		var err error
		require.NotPanics(t, func() {
			err = VerifyEIP1559Header(testConfig, headerA, headerB)
		}, "VerifyEIP1559Header must not panic when parent.BaseFee is nil")

		require.Error(t, err, "VerifyEIP1559Header must reject child when parent.BaseFee is nil")
		require.Contains(t, err.Error(), "parent header is missing baseFee",
			"Error must indicate parent BaseFee issue")
	})

	t.Run("CalcBaseFee called directly does not panic", func(t *testing.T) {
		// Header with nil BaseFee
		header := &types.Header{
			Number:   big.NewInt(6), // Post-London (LondonBlock is 5)
			GasLimit: 20000000,
			GasUsed:  10000000,
			BaseFee:  nil,
		}

		// CalcBaseFee doesn't panic
		var result *big.Int
		require.NotPanics(t, func() {
			result = CalcBaseFee(testConfig, header)
		}, "CalcBaseFee must not panic when parent.BaseFee is nil")

		require.NotNil(t, result, "CalcBaseFee should return non-nil result")
		require.Equal(t, big.NewInt(params.InitialBaseFee), result,
			"CalcBaseFee should return InitialBaseFee as fallback")
	})
}

func TestCalcParentGasTarget(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	defaultGasLimit := uint64(60_000_000)

	t.Run("gas target calculation pre dandeli HF", func(t *testing.T) {
		block := &types.Header{
			Number:   big.NewInt(9),
			GasLimit: defaultGasLimit,
			GasUsed:  defaultGasLimit / 2,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}
		gasTarget := calcParentGasTarget(testConfig, block)
		expected := block.GasLimit / 2 // because elasticity multiplier is set to 2 by default
		require.Equal(t, expected, gasTarget, "expected gas target = gaslimit/2")
	})

	t.Run("gas target calculation post dandeli HF", func(t *testing.T) {
		block := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: defaultGasLimit,
			GasUsed:  defaultGasLimit / 2,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}
		gasTarget := calcParentGasTarget(testConfig, block)
		expected := block.GasLimit * params.TargetGasPercentagePostDandeli / 100 // because gas target is derived by this protocol parameter
		require.Equal(t, expected, gasTarget, "case #1: expected gas target = 60 percent of gas limit")

		block = &types.Header{
			Number:   big.NewInt(21),
			GasLimit: defaultGasLimit,
			GasUsed:  defaultGasLimit / 2,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}
		gasTarget = calcParentGasTarget(testConfig, block)
		expected = block.GasLimit * params.TargetGasPercentagePostDandeli / 100 // because gas target is derived by this protocol parameter
		require.Equal(t, expected, gasTarget, "case #2: expected gas target = 60 percent of gas limit")
	})

	t.Run("nil bor config", func(t *testing.T) {
		testConfig.Bor = nil
		block := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: defaultGasLimit,
			GasUsed:  defaultGasLimit / 2,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}
		gasTarget := calcParentGasTarget(testConfig, block)
		expected := block.GasLimit / 2 // because elasticity multiplier is set to 2 by default
		require.Equal(t, expected, gasTarget, "expected gas target = gaslimit/2 when bor config is nil")
	})
}

// simpleBaseFeeCalculator contains an overly simplified logic of base fee calculations useful for generating
// expected values in test cases. It assumes all blocks are post-bhilai HF.
func simpleBaseFeeCalculator(initialBaseFee int64, gasLimit, gasUsed uint64, targetGasPercentage uint64) uint64 {
	initial := big.NewInt(initialBaseFee)
	val := big.NewInt(1)
	val.Mul(val, initial)

	// Assuming tests are running post bhilai
	bfd := int64(params.BaseFeeChangeDenominatorPostBhilai)

	// Define a target gas based on given percentage
	target := gasLimit * targetGasPercentage / 100
	if gasUsed == target {
		return initial.Uint64()
	}

	// follow the simple formula to get the new base fee:
	// base fee = initialBaseFee +/- (initialBaseFee * gasUsedDelta / gasTarget / baseFeeChangeDenominator)

	var delta int64
	if gasUsed > target {
		delta = int64(gasUsed - target)
	} else {
		delta = int64(target - gasUsed)
	}

	val.Mul(val, big.NewInt(delta))
	val.Div(val, big.NewInt(bfd))
	val.Div(val, big.NewInt(int64(target)))

	if gasUsed > target {
		return initial.Add(initial, val).Uint64()
	} else {
		return initial.Sub(initial, val).Uint64()
	}
}

func TestCalcBaseFeeDandeli(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.BhilaiBlock = big.NewInt(8)
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	// Case 1: Create pre-dandeli cases where HF is defined in future. Validate
	// base fee calculations before HF kicks in. Base fee should be calculated
	// based on default elasticity multiplier.
	tests := []struct {
		name            string
		parentBaseFee   int64
		parentGasLimit  uint64
		parentGasUsed   uint64
		expectedBaseFee uint64
	}{
		{"usage == target", params.InitialBaseFee, 60_000_000, 30_000_000, params.InitialBaseFee},
		{"usage below target #1", params.InitialBaseFee, 60_000_000, 20_000_000, 994791667},
		{"usage below target #2", params.InitialBaseFee, 60_000_000, 10_000_000, 989583334},
		{"usage above target #1", params.InitialBaseFee, 60_000_000, 40_000_000, 1005208333},
		{"usage above target #2", params.InitialBaseFee, 60_000_000, 50_000_000, 1010416666},
		{"usage full", params.InitialBaseFee, 60_000_000, 60_000_000, 1015625000},
		{"usage 0", params.InitialBaseFee, 60_000_000, 0, 984375000},
	}
	for _, test := range tests {
		block := &types.Header{
			Number:   big.NewInt(8),
			GasLimit: test.parentGasLimit,
			GasUsed:  test.parentGasUsed,
			BaseFee:  big.NewInt(test.parentBaseFee),
		}
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee := simpleBaseFeeCalculator(block.BaseFee.Int64(), block.GasLimit, block.GasUsed, params.DefaultTargetGasPercentage)
		require.Equal(
			t,
			expectedBaseFee,
			baseFee,
			fmt.Sprintf("pre-dandeli base fee mismatch with expected value, test: %s, got: %d, want: %d", test.name, baseFee, expectedBaseFee),
		)
		// Also check with manually calculated base fee
		require.Equal(
			t,
			test.expectedBaseFee,
			baseFee,
			fmt.Sprintf("pre-dandeli base fee mismatch with manually calculated value, test: %s, got: %d, want: %d", test.name, baseFee, expectedBaseFee),
		)
	}

	// Case 2: Create post-dandeli cases where HF has kicked in. Validate base fee changes
	// based on the newly introduced protocol param: TargetGasPrecentage. Target gas limit
	// should be calculated based on this percentage value out of total gas limit. Base
	// fee should be changed accordingly.
	tests = []struct {
		name            string
		parentBaseFee   int64
		parentGasLimit  uint64
		parentGasUsed   uint64
		expectedBaseFee uint64
	}{
		{"usage == target (65%)", params.InitialBaseFee, 60_000_000, 39_000_000, params.InitialBaseFee},
		{"usage below target #1", params.InitialBaseFee, 60_000_000, 30_000_000, 996394231},
		{"usage below target #2", params.InitialBaseFee, 60_000_000, 10_000_000, 988381411},
		{"usage above target #1", params.InitialBaseFee, 60_000_000, 40_000_000, 1000400641},
		{"usage above target #2", params.InitialBaseFee, 60_000_000, 50_000_000, 1004407051},
		{"usage full", params.InitialBaseFee, 60_000_000, 60_000_000, 1008413461},
		{"usage 0", params.InitialBaseFee, 60_000_000, 0, 984375000},
	}
	for _, test := range tests {
		// Post-dandeli block #1
		block := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: test.parentGasLimit,
			GasUsed:  test.parentGasUsed,
			BaseFee:  big.NewInt(test.parentBaseFee),
		}
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee := simpleBaseFeeCalculator(block.BaseFee.Int64(), block.GasLimit, block.GasUsed, params.TargetGasPercentagePostDandeli)
		require.Equal(
			t,
			expectedBaseFee,
			baseFee,
			fmt.Sprintf("post-dandeli #1: base fee mismatch with expected value, test: %s, got: %d, want: %d", test.name, baseFee, expectedBaseFee),
		)
		// Also check with manually calculated base fee
		require.Equal(
			t,
			test.expectedBaseFee,
			baseFee,
			fmt.Sprintf("post-dandeli #1: base fee mismatch with manually calculated value, test: %s, got: %d, want: %d", test.name, baseFee, expectedBaseFee),
		)

		// Post-dandeli block #2
		block = &types.Header{
			Number:   big.NewInt(21),
			GasLimit: test.parentGasLimit,
			GasUsed:  test.parentGasUsed,
			BaseFee:  big.NewInt(test.parentBaseFee),
		}
		baseFee = CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee = simpleBaseFeeCalculator(block.BaseFee.Int64(), block.GasLimit, block.GasUsed, params.TargetGasPercentagePostDandeli)
		require.Equal(
			t,
			expectedBaseFee,
			baseFee,
			fmt.Sprintf("post-dandeli #2: base fee mismatch with expected value, test: %s, got: %d, want: %d", test.name, baseFee, expectedBaseFee),
		)
		// Also check with manually calculated base fee
		require.Equal(
			t,
			test.expectedBaseFee,
			baseFee,
			fmt.Sprintf("post-dandeli #2: base fee mismatch with manually calculated value, test: %s, got: %d, want: %d", test.name, baseFee, expectedBaseFee),
		)
	}
}

// TestDynamicTargetGasPercentage verifies that the TargetGasPercentage parameter
// can be dynamically set after Lisovo HF and affects base fee calculations correctly
func TestDynamicTargetGasPercentage(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.BhilaiBlock = big.NewInt(8)
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	// Test with 70% target gas percentage
	targetGasPercentage70 := uint64(70)
	testConfig.Bor.TargetGasPercentage = &targetGasPercentage70

	gasLimit := uint64(60_000_000)
	initialBaseFee := int64(params.InitialBaseFee)

	t.Run("70% target gas percentage", func(t *testing.T) {
		// When gas used equals 70% of gas limit, base fee should stay the same
		block := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: gasLimit,
			GasUsed:  42_000_000, // 70% of 60M
			BaseFee:  big.NewInt(initialBaseFee),
		}
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "base fee should remain unchanged at target")

		// When gas used is below target (50%), base fee should decrease
		block.GasUsed = 30_000_000 // 50% of 60M
		baseFee = CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee := simpleBaseFeeCalculator(initialBaseFee, gasLimit, block.GasUsed, targetGasPercentage70)
		require.Equal(t, expectedBaseFee, baseFee, "base fee should decrease when below target")
		require.Less(t, baseFee, uint64(initialBaseFee), "base fee should be less than initial")

		// When gas used is above target (90%), base fee should increase
		block.GasUsed = 54_000_000 // 90% of 60M
		baseFee = CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee = simpleBaseFeeCalculator(initialBaseFee, gasLimit, block.GasUsed, targetGasPercentage70)
		require.Equal(t, expectedBaseFee, baseFee, "base fee should increase when above target")
		require.Greater(t, baseFee, uint64(initialBaseFee), "base fee should be greater than initial")
	})

	// Change target gas percentage to 50%
	targetGasPercentage50 := uint64(50)
	testConfig.Bor.TargetGasPercentage = &targetGasPercentage50

	t.Run("50% target gas percentage - same run different value", func(t *testing.T) {
		// When gas used equals 50% of gas limit, base fee should stay the same
		block := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: gasLimit,
			GasUsed:  30_000_000, // 50% of 60M
			BaseFee:  big.NewInt(initialBaseFee),
		}
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "base fee should remain unchanged at new target")

		// When gas used is below new target (40%), base fee should decrease
		block.GasUsed = 24_000_000 // 40% of 60M
		baseFee = CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee := simpleBaseFeeCalculator(initialBaseFee, gasLimit, block.GasUsed, targetGasPercentage50)
		require.Equal(t, expectedBaseFee, baseFee, "base fee should decrease when below new target")
		require.Less(t, baseFee, uint64(initialBaseFee), "base fee should be less than initial")

		// When gas used is above new target (70%), base fee should increase
		block.GasUsed = 42_000_000 // 70% of 60M
		baseFee = CalcBaseFee(testConfig, block).Uint64()
		expectedBaseFee = simpleBaseFeeCalculator(initialBaseFee, gasLimit, block.GasUsed, targetGasPercentage50)
		require.Equal(t, expectedBaseFee, baseFee, "base fee should increase when above new target")
		require.Greater(t, baseFee, uint64(initialBaseFee), "base fee should be greater than initial")
	})

	t.Run("nil target gas percentage falls back to default", func(t *testing.T) {
		testConfig.Bor.TargetGasPercentage = nil
		block := &types.Header{
			Number:   big.NewInt(22),
			GasLimit: gasLimit,
			GasUsed:  39_000_000, // 65% of 60M (default is 65%)
			BaseFee:  big.NewInt(initialBaseFee),
		}
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "base fee should remain unchanged at default target")
	})
}

// TestDynamicBaseFeeChangeDenominator verifies that the BaseFeeChangeDenominator parameter
// can be dynamically set after Lisovo HF and affects the rate of base fee change correctly
func TestDynamicBaseFeeChangeDenominator(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.BhilaiBlock = big.NewInt(8)
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	gasLimit := uint64(60_000_000)
	initialBaseFee := int64(params.InitialBaseFee)
	targetGasPercentage := uint64(params.TargetGasPercentagePostDandeli)

	// Test with denominator of 32 (slower changes)
	denominator32 := uint64(32)
	testConfig.Bor.BaseFeeChangeDenominator = &denominator32

	t.Run("denominator 32 - slower base fee changes", func(t *testing.T) {
		block := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: gasLimit,
			GasUsed:  50_000_000, // Above target
			BaseFee:  big.NewInt(initialBaseFee),
		}

		baseFee := CalcBaseFee(testConfig, block).Uint64()

		// Calculate expected with custom denominator
		target := gasLimit * targetGasPercentage / 100
		gasUsedDelta := block.GasUsed - target
		expectedIncrease := new(big.Int).Mul(big.NewInt(initialBaseFee), big.NewInt(int64(gasUsedDelta)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(target)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(denominator32)))
		expectedBaseFee := new(big.Int).Add(big.NewInt(initialBaseFee), expectedIncrease).Uint64()

		require.Equal(t, expectedBaseFee, baseFee, "base fee should change according to denominator 32")
	})

	// Change denominator to 16 (faster changes) in the same run
	denominator16 := uint64(16)
	testConfig.Bor.BaseFeeChangeDenominator = &denominator16

	t.Run("denominator 16 - faster base fee changes - same run different value", func(t *testing.T) {
		block := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: gasLimit,
			GasUsed:  50_000_000, // Same gas used as before
			BaseFee:  big.NewInt(initialBaseFee),
		}

		baseFee := CalcBaseFee(testConfig, block).Uint64()

		// Calculate expected with new denominator (should result in larger change)
		target := gasLimit * targetGasPercentage / 100
		gasUsedDelta := block.GasUsed - target
		expectedIncrease := new(big.Int).Mul(big.NewInt(initialBaseFee), big.NewInt(int64(gasUsedDelta)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(target)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(denominator16)))
		expectedBaseFee := new(big.Int).Add(big.NewInt(initialBaseFee), expectedIncrease).Uint64()

		require.Equal(t, expectedBaseFee, baseFee, "base fee should change more with denominator 16")
	})

	t.Run("nil denominator falls back to Bhilai default (64)", func(t *testing.T) {
		testConfig.Bor.BaseFeeChangeDenominator = nil
		block := &types.Header{
			Number:   big.NewInt(22),
			GasLimit: gasLimit,
			GasUsed:  50_000_000,
			BaseFee:  big.NewInt(initialBaseFee),
		}

		baseFee := CalcBaseFee(testConfig, block).Uint64()

		// Should use Bhilai denominator (64)
		target := gasLimit * targetGasPercentage / 100
		gasUsedDelta := block.GasUsed - target
		expectedIncrease := new(big.Int).Mul(big.NewInt(initialBaseFee), big.NewInt(int64(gasUsedDelta)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(target)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(params.BaseFeeChangeDenominatorPostBhilai)))
		expectedBaseFee := new(big.Int).Add(big.NewInt(initialBaseFee), expectedIncrease).Uint64()

		require.Equal(t, expectedBaseFee, baseFee, "base fee should use Bhilai default denominator")
	})
}

// TestVerifyEIP1559HeaderNoBaseFeeValidation tests post-Lisovo boundary validation
// instead of strict validation. Base fees must be within MaxBaseFeeChangePercent boundary.
func TestVerifyEIP1559HeaderNoBaseFeeValidation(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)
	testConfig.Bor.BhilaiBlock = big.NewInt(5)

	parent := &types.Header{
		Number:   big.NewInt(20),
		GasLimit: 30_000_000,
		GasUsed:  15_000_000,
		BaseFee:  big.NewInt(1_000_000_000),
	}

	t.Run("accepts base fee within boundary", func(t *testing.T) {
		// Header with a base fee that doesn't match calculated value but is within MaxBaseFeeChangePercent boundary
		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(1_040_000_000), // 4% increase - within boundary
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "should accept header with base fee within boundary")
	})

	t.Run("accepts base fee different from calculated but within boundary", func(t *testing.T) {
		calculatedBaseFee := CalcBaseFee(testConfig, parent)

		// Use a different base fee that's still within MaxBaseFeeChangePercent of parent
		differentBaseFee := new(big.Int).Mul(parent.BaseFee, big.NewInt(103))
		differentBaseFee.Div(differentBaseFee, big.NewInt(100)) // 3% increase

		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  differentBaseFee,
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "should accept header with base fee within boundary even if different from calculated: calculated=%s, header=%s", calculatedBaseFee, differentBaseFee)
	})

	t.Run("rejects base fee exceeding boundary", func(t *testing.T) {
		// Base fee that exceeds MaxBaseFeeChangePercent boundary
		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(1_100_000_000), // 10% increase - exceeds boundary
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.Error(t, err, "should reject header with base fee exceeding boundary")
		require.Contains(t, err.Error(), "baseFee change exceeds", "error should mention boundary exceeded")
	})

	t.Run("rejects nil base fee", func(t *testing.T) {
		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  nil, // Nil base fee should still be rejected
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.Error(t, err, "should reject header with nil base fee")
		require.Contains(t, err.Error(), "baseFee", "error should mention baseFee")
	})

	t.Run("accepts zero base fee when parent is also zero", func(t *testing.T) {
		parentZero := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: 30_000_000,
			GasUsed:  15_000_000,
			BaseFee:  big.NewInt(0),
		}

		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(0), // Zero is valid when parent is also zero
		}

		err := VerifyEIP1559Header(testConfig, parentZero, header)
		require.NoError(t, err, "should accept header with zero base fee when parent is zero")
	})
}

// TestInvalidTargetGasPercentage tests that invalid TargetGasPercentage values
// fall back to defaults and don't cause panics
func TestInvalidTargetGasPercentage(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.BhilaiBlock = big.NewInt(8)
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	gasLimit := uint64(60_000_000)
	initialBaseFee := int64(params.InitialBaseFee)

	t.Run("zero target gas percentage falls back to default", func(t *testing.T) {
		zeroValue := uint64(0)
		testConfig.Bor.TargetGasPercentage = &zeroValue

		block := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: gasLimit,
			GasUsed:  39_000_000, // 65% of 60M (default target)
			BaseFee:  big.NewInt(initialBaseFee),
		}

		// Should not panic and should use default (65%)
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "should use default target and base fee unchanged")
	})

	t.Run("target gas percentage > 100 falls back to default", func(t *testing.T) {
		invalidValue := uint64(150)
		testConfig.Bor.TargetGasPercentage = &invalidValue

		block := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: gasLimit,
			GasUsed:  39_000_000, // 65% of 60M (default target)
			BaseFee:  big.NewInt(initialBaseFee),
		}

		// Should not panic and should use default (65%)
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "should use default target and base fee unchanged")
	})

	t.Run("valid edge case: 1% target gas percentage", func(t *testing.T) {
		onePercent := uint64(1)
		testConfig.Bor.TargetGasPercentage = &onePercent

		block := &types.Header{
			Number:   big.NewInt(22),
			GasLimit: gasLimit,
			GasUsed:  600_000, // 1% of 60M
			BaseFee:  big.NewInt(initialBaseFee),
		}

		// Should work with 1% target
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "should accept 1% as valid target")
	})

	t.Run("valid edge case: 100% target gas percentage", func(t *testing.T) {
		hundredPercent := uint64(100)
		testConfig.Bor.TargetGasPercentage = &hundredPercent

		block := &types.Header{
			Number:   big.NewInt(23),
			GasLimit: gasLimit,
			GasUsed:  gasLimit, // 100% of gas limit
			BaseFee:  big.NewInt(initialBaseFee),
		}

		// Should work with 100% target
		baseFee := CalcBaseFee(testConfig, block).Uint64()
		require.Equal(t, uint64(initialBaseFee), baseFee, "should accept 100% as valid target")
	})
}

// TestInvalidBaseFeeChangeDenominator tests that invalid BaseFeeChangeDenominator values
// fall back to defaults and don't cause panics (especially division by zero)
func TestInvalidBaseFeeChangeDenominator(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.BhilaiBlock = big.NewInt(8)
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	gasLimit := uint64(60_000_000)
	initialBaseFee := int64(params.InitialBaseFee)

	t.Run("zero denominator falls back to default", func(t *testing.T) {
		zeroDenom := uint64(0)
		testConfig.Bor.BaseFeeChangeDenominator = &zeroDenom

		block := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: gasLimit,
			GasUsed:  50_000_000, // Above target
			BaseFee:  big.NewInt(initialBaseFee),
		}

		// Should not panic (no division by zero) and use Bhilai default (64)
		baseFee := CalcBaseFee(testConfig, block)
		require.NotNil(t, baseFee, "should calculate base fee without panic")

		// Verify it used default denominator (64) by checking the change is small
		target := gasLimit * params.TargetGasPercentagePostDandeli / 100
		gasUsedDelta := block.GasUsed - target
		expectedIncrease := new(big.Int).Mul(big.NewInt(initialBaseFee), big.NewInt(int64(gasUsedDelta)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(target)))
		expectedIncrease.Div(expectedIncrease, big.NewInt(int64(params.BaseFeeChangeDenominatorPostBhilai)))
		expectedBaseFee := new(big.Int).Add(big.NewInt(initialBaseFee), expectedIncrease)

		require.Equal(t, expectedBaseFee.Uint64(), baseFee.Uint64(), "should use Bhilai default denominator (64)")
	})

	t.Run("valid edge case: denominator = 1 (extreme volatility)", func(t *testing.T) {
		extremeDenom := uint64(1)
		testConfig.Bor.BaseFeeChangeDenominator = &extremeDenom

		block := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: gasLimit,
			GasUsed:  50_000_000,
			BaseFee:  big.NewInt(initialBaseFee),
		}

		// Should work but produce large changes
		baseFee := CalcBaseFee(testConfig, block)
		require.NotNil(t, baseFee, "should handle denominator of 1")
		require.Greater(t, baseFee.Uint64(), uint64(initialBaseFee), "base fee should increase significantly with denominator 1")
	})
}

// TestBaseFeeValidationPreDandeli tests that base fee validation still works before Dandeli HF
func TestBaseFeeValidationPreDandeli(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(20)
	testConfig.Bor.DandeliBlock = big.NewInt(20)

	parent := &types.Header{
		Number:   big.NewInt(10), // Pre-Dandeli
		GasLimit: 30_000_000,
		GasUsed:  15_000_000,
		BaseFee:  big.NewInt(1_000_000_000),
	}

	t.Run("pre-Dandeli: rejects incorrect base fee", func(t *testing.T) {
		calculatedBaseFee := CalcBaseFee(testConfig, parent)
		incorrectBaseFee := new(big.Int).Mul(calculatedBaseFee, big.NewInt(2))

		header := &types.Header{
			Number:   big.NewInt(11),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  incorrectBaseFee, // Wrong base fee
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.Error(t, err, "should reject incorrect base fee pre-Dandeli")
		require.Contains(t, err.Error(), "invalid baseFee", "error should mention invalid baseFee")
	})

	t.Run("pre-Dandeli: accepts correct base fee", func(t *testing.T) {
		calculatedBaseFee := CalcBaseFee(testConfig, parent)

		header := &types.Header{
			Number:   big.NewInt(11),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  calculatedBaseFee, // Correct base fee
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "should accept correct base fee pre-Dandeli")
	})

	t.Run("post-Dandeli: accepts base fee within boundary", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(25), // Post-Lisovo (boundary validation active)
			GasLimit: 30_000_000,
			GasUsed:  15_000_000,
			BaseFee:  big.NewInt(1_000_000_000),
		}

		calculatedBaseFee := CalcBaseFee(testConfig, parent)
		// Use a base fee within MaxBaseFeeChangePercent boundary (3% increase)
		baseFeeWithinBoundary := new(big.Int).Mul(parent.BaseFee, big.NewInt(103))
		baseFeeWithinBoundary.Div(baseFeeWithinBoundary, big.NewInt(100))

		header := &types.Header{
			Number:   big.NewInt(26),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  baseFeeWithinBoundary, // Within boundary
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "should accept base fee within boundary post-Dandeli: calculated=%s, header=%s", calculatedBaseFee, baseFeeWithinBoundary)
	})
}

// TestCalcBaseFeeVeryLowFeesCapping tests that base fee changes are capped
// even at very low base fees (1-20 wei) post-Lisovo
func TestCalcBaseFeeVeryLowFeesCapping(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(15)
	testConfig.Bor.DandeliBlock = big.NewInt(10)
	testConfig.Bor.BhilaiBlock = big.NewInt(5)

	// Use aggressive parameters (low denominator) to force large increases
	// that would exceed the 5% boundary at very low base fees
	aggressiveDenom := uint64(4)
	testConfig.Bor.BaseFeeChangeDenominator = &aggressiveDenom

	tests := []struct {
		name             string
		parentBaseFee    int64
		parentGasUsed    uint64
		maxAllowedChange int64 // Expected max change in wei
	}{
		{
			name:             "1 wei base fee - aggressive params",
			parentBaseFee:    1,
			parentGasUsed:    30_000_000, // 100% usage
			maxAllowedChange: 1,          // 5% rounds to 0, should cap at 1 wei
		},
		{
			name:             "10 wei base fee - aggressive params",
			parentBaseFee:    10,
			parentGasUsed:    30_000_000,
			maxAllowedChange: 1, // 5% = 0.5, rounds to 0, should cap at 1 wei
		},
		{
			name:             "20 wei base fee - aggressive params",
			parentBaseFee:    20,
			parentGasUsed:    30_000_000,
			maxAllowedChange: 1, // 5% = 1 wei exactly
		},
		{
			name:             "100 wei base fee - aggressive params",
			parentBaseFee:    100,
			parentGasUsed:    30_000_000,
			maxAllowedChange: 5, // 5% = 5 wei
		},
		{
			name:             "1 gwei base fee - aggressive params",
			parentBaseFee:    1_000_000_000,
			parentGasUsed:    30_000_000,
			maxAllowedChange: 50_000_000, // 5% of 1 gwei
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &types.Header{
				Number:   big.NewInt(20), // Post-Dandeli
				GasLimit: 30_000_000,
				GasUsed:  tt.parentGasUsed,
				BaseFee:  big.NewInt(tt.parentBaseFee),
			}

			calculatedBaseFee := CalcBaseFee(testConfig, parent)
			actualChange := new(big.Int).Sub(calculatedBaseFee, parent.BaseFee)

			// Verify change is capped
			require.LessOrEqual(t, actualChange.Int64(), tt.maxAllowedChange,
				"Base fee increase should be capped at %d wei, got %d wei (%.2f%%)",
				tt.maxAllowedChange, actualChange.Int64(),
				float64(actualChange.Int64())*100.0/float64(parent.BaseFee.Int64()))
		})
	}
}

// TestVerifyBaseFeeWithinBoundariesLowFees tests header validation at very low base fees
func TestVerifyBaseFeeWithinBoundariesLowFees(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(15)
	testConfig.Bor.DandeliBlock = big.NewInt(10)
	testConfig.Bor.BhilaiBlock = big.NewInt(5)

	tests := []struct {
		name          string
		parentBaseFee int64
		headerBaseFee int64
		expectError   bool
		description   string
	}{
		{
			name:          "1 wei → 2 wei (within 1 wei cap)",
			parentBaseFee: 1,
			headerBaseFee: 2,
			expectError:   false,
			description:   "1 wei increase should be accepted",
		},
		{
			name:          "1 wei → 3 wei (exceeds 1 wei cap)",
			parentBaseFee: 1,
			headerBaseFee: 3,
			expectError:   true,
			description:   "2 wei increase should be rejected",
		},
		{
			name:          "10 wei → 11 wei (within 1 wei cap)",
			parentBaseFee: 10,
			headerBaseFee: 11,
			expectError:   false,
			description:   "1 wei increase should be accepted",
		},
		{
			name:          "10 wei → 12 wei (exceeds 1 wei cap)",
			parentBaseFee: 10,
			headerBaseFee: 12,
			expectError:   true,
			description:   "2 wei increase should be rejected",
		},
		{
			name:          "20 wei → 21 wei (5% = 1 wei)",
			parentBaseFee: 20,
			headerBaseFee: 21,
			expectError:   false,
			description:   "1 wei increase (5%) should be accepted",
		},
		{
			name:          "20 wei → 22 wei (exceeds 5%)",
			parentBaseFee: 20,
			headerBaseFee: 22,
			expectError:   true,
			description:   "2 wei increase (10%) should be rejected",
		},
		{
			name:          "100 wei → 105 wei (5%)",
			parentBaseFee: 100,
			headerBaseFee: 105,
			expectError:   false,
			description:   "5% increase should be accepted",
		},
		{
			name:          "100 wei → 107 wei (exceeds 5%)",
			parentBaseFee: 100,
			headerBaseFee: 107,
			expectError:   true,
			description:   "7% increase should be rejected",
		},
		{
			name:          "1 gwei → 1.05 gwei (5%)",
			parentBaseFee: 1_000_000_000,
			headerBaseFee: 1_050_000_000,
			expectError:   false,
			description:   "5% increase should be accepted",
		},
		{
			name:          "1 gwei → 1.1 gwei (10%)",
			parentBaseFee: 1_000_000_000,
			headerBaseFee: 1_100_000_000,
			expectError:   true,
			description:   "10% increase should be rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &types.Header{
				Number:   big.NewInt(20), // Post-Dandeli
				GasLimit: 30_000_000,
				GasUsed:  15_000_000,
				BaseFee:  big.NewInt(tt.parentBaseFee),
			}

			header := &types.Header{
				Number:   big.NewInt(21),
				GasLimit: 30_000_000,
				GasUsed:  20_000_000,
				BaseFee:  big.NewInt(tt.headerBaseFee),
			}

			err := VerifyEIP1559Header(testConfig, parent, header)

			if tt.expectError {
				require.Error(t, err, tt.description)
				require.Contains(t, err.Error(), "baseFee change exceeds", "should mention boundary exceeded")
			} else {
				require.NoError(t, err, tt.description)
			}
		})
	}
}

// TestLowBaseFeeEdgeCases tests edge cases at very low base fees
func TestLowBaseFeeEdgeCases(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(15)
	testConfig.Bor.DandeliBlock = big.NewInt(10)
	testConfig.Bor.BhilaiBlock = big.NewInt(5)

	t.Run("exact 5% boundary at 20 wei", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: 30_000_000,
			GasUsed:  15_000_000,
			BaseFee:  big.NewInt(20), // 5% = 1 wei
		}

		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(21), // +1 wei = exactly 5%
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "exactly 5% increase should be accepted")
	})

	t.Run("just over 5% at 20 wei", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: 30_000_000,
			GasUsed:  15_000_000,
			BaseFee:  big.NewInt(20),
		}

		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(22), // +2 wei = 10%
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.Error(t, err, "10% increase should be rejected")
	})

	t.Run("decrease capping at very low fees", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: 30_000_000,
			GasUsed:  25_000_000, // High usage to get increase first
			BaseFee:  big.NewInt(2),
		}

		calculatedBaseFee := CalcBaseFee(testConfig, parent)
		require.GreaterOrEqual(t, calculatedBaseFee.Int64(), parent.BaseFee.Int64(),
			"high usage should not decrease base fee")

		// Now test decrease
		parent2 := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  10_000_000, // Low usage for decrease
			BaseFee:  big.NewInt(10),
		}

		header := &types.Header{
			Number:   big.NewInt(22),
			GasLimit: 30_000_000,
			GasUsed:  10_000_000,
			BaseFee:  big.NewInt(9), // -1 wei
		}

		err := VerifyEIP1559Header(testConfig, parent2, header)
		require.NoError(t, err, "1 wei decrease should be accepted")

		// Test exceeding decrease
		header.BaseFee = big.NewInt(8) // -2 wei
		err = VerifyEIP1559Header(testConfig, parent2, header)
		require.Error(t, err, "2 wei decrease should be rejected")
	})

	t.Run("multiple blocks of growth from 1 wei", func(t *testing.T) {
		baseFee := big.NewInt(1)

		// Simulate 5 blocks of full usage
		for i := 0; i < 5; i++ {
			parent := &types.Header{
				Number:   big.NewInt(int64(20 + i)),
				GasLimit: 30_000_000,
				GasUsed:  30_000_000, // Full usage
				BaseFee:  baseFee,
			}

			newBaseFee := CalcBaseFee(testConfig, parent)
			change := new(big.Int).Sub(newBaseFee, baseFee)

			// Each block should be capped at 1 wei increase
			require.LessOrEqual(t, change.Int64(), int64(1),
				"block %d: increase should be capped at 1 wei", i)

			baseFee = newBaseFee
		}

		// After 5 blocks of 1 wei increases, should be at most 6 wei
		require.LessOrEqual(t, baseFee.Int64(), int64(6),
			"after 5 blocks starting from 1 wei, should be at most 6 wei")
	})

	t.Run("zero base fee edge case", func(t *testing.T) {
		// Test starting from 0 wei (theoretical edge case)
		parentZero := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: 30_000_000,
			GasUsed:  30_000_000, // Full usage
			BaseFee:  big.NewInt(0),
		}

		// Calculate from 0 wei - should be able to increase
		calculatedBaseFee := CalcBaseFee(testConfig, parentZero)
		require.Greater(t, calculatedBaseFee.Int64(), int64(0),
			"base fee should be able to increase from 0")
		require.LessOrEqual(t, calculatedBaseFee.Int64(), int64(1),
			"increase from 0 wei should be capped at 1 wei")

		// Test validation: 0 → 0 should pass (no change)
		headerZero := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  30_000_000,
			BaseFee:  big.NewInt(0),
		}
		err := VerifyEIP1559Header(testConfig, parentZero, headerZero)
		require.NoError(t, err, "0 → 0 wei should be accepted")

		// Test validation: 0 → 1 should pass (within 1 wei cap)
		headerOne := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  30_000_000,
			BaseFee:  big.NewInt(1),
		}
		err = VerifyEIP1559Header(testConfig, parentZero, headerOne)
		require.NoError(t, err, "0 → 1 wei should be accepted (within 1 wei cap)")

		// Test validation: 0 → 2 should fail (exceeds 1 wei cap)
		headerTwo := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  30_000_000,
			BaseFee:  big.NewInt(2),
		}
		err = VerifyEIP1559Header(testConfig, parentZero, headerTwo)
		require.Error(t, err, "0 → 2 wei should be rejected (exceeds 1 wei cap)")
		require.Contains(t, err.Error(), "baseFee change exceeds",
			"error should mention boundary exceeded")
	})
}

// TestLowBaseFeesPreLisovo tests that pre-Lisovo strict validation still works at low fees
func TestLowBaseFeesPreLisovo(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.DandeliBlock = big.NewInt(100) // Far in future
	testConfig.Bor.BhilaiBlock = big.NewInt(5)

	t.Run("pre-Dandeli: low fees use strict validation", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(20), // Pre-Dandeli
			GasLimit: 30_000_000,
			GasUsed:  15_000_000,
			BaseFee:  big.NewInt(10), // Very low, but pre-Dandeli
		}

		calculatedBaseFee := CalcBaseFee(testConfig, parent)

		// Even 1 wei off should fail pre-Dandeli
		wrongBaseFee := new(big.Int).Add(calculatedBaseFee, big.NewInt(1))

		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  wrongBaseFee,
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.Error(t, err, "should reject even 1 wei difference pre-Dandeli")
		require.Contains(t, err.Error(), "invalid baseFee", "should use strict validation")
	})

	t.Run("pre-Dandeli: exact match accepted", func(t *testing.T) {
		parent := &types.Header{
			Number:   big.NewInt(20),
			GasLimit: 30_000_000,
			GasUsed:  15_000_000,
			BaseFee:  big.NewInt(10),
		}

		calculatedBaseFee := CalcBaseFee(testConfig, parent)

		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  calculatedBaseFee,
		}

		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "exact match should be accepted pre-Dandeli")
	})
}

// TestLowBaseFeeIntegrationWithExistingBoundary verifies new behavior doesn't break existing tests
func TestLowBaseFeeIntegrationWithExistingBoundary(t *testing.T) {
	t.Parallel()

	testConfig := copyConfig(config())
	testConfig.Bor.LisovoBlock = big.NewInt(15)
	testConfig.Bor.DandeliBlock = big.NewInt(10)
	testConfig.Bor.BhilaiBlock = big.NewInt(5)

	// Normal production base fees - should behave identically to before
	parent := &types.Header{
		Number:   big.NewInt(20),
		GasLimit: 30_000_000,
		GasUsed:  15_000_000,
		BaseFee:  big.NewInt(25_000_000_000), // 25 gwei (typical production)
	}

	t.Run("normal fees: 3% increase accepted", func(t *testing.T) {
		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(25_750_000_000), // 3% increase
		}
		err := VerifyEIP1559Header(testConfig, parent, header)
		require.NoError(t, err, "3% increase should be accepted")
	})

	t.Run("normal fees: 7% increase rejected", func(t *testing.T) {
		header := &types.Header{
			Number:   big.NewInt(21),
			GasLimit: 30_000_000,
			GasUsed:  20_000_000,
			BaseFee:  big.NewInt(26_750_000_000), // 7% increase
		}
		err := VerifyEIP1559Header(testConfig, parent, header)
		require.Error(t, err, "7% increase should be rejected")
		require.Contains(t, err.Error(), "baseFee change exceeds", "should mention boundary exceeded")
	})

	t.Run("normal fees: CalcBaseFee produces value within boundary", func(t *testing.T) {
		calculatedBaseFee := CalcBaseFee(testConfig, parent)

		// Calculate percentage change
		change := new(big.Int).Sub(calculatedBaseFee, parent.BaseFee)
		changePercent := new(big.Int).Mul(change, big.NewInt(100))
		changePercent.Div(changePercent, parent.BaseFee)

		require.LessOrEqual(t, changePercent.Int64(), int64(5),
			"CalcBaseFee should produce value within 5%% boundary")
	})
}

// TestCalcParentGasTargetDynamicForkTransition verifies that calcParentGasTarget uses
// static GetTargetGasPercentage before Lisovo and GetDynamicTargetGasPercentage after.
func TestCalcParentGasTargetDynamicForkTransition(t *testing.T) {
	t.Parallel()

	const (
		gasLimit     = uint64(30_000_000)
		desiredFee   = uint64(30_000_000_000) // 30 gwei
		buffer       = uint64(5_000_000_000)  // 5 gwei
		tGasMinPct   = uint64(50)
		tGasMaxPct   = uint64(80)
		dandeliBlock = int64(50)
		lisovoBlock  = int64(100)
	)

	enabled := true
	min := tGasMinPct
	max := tGasMaxPct
	dbf := desiredFee
	buf := buffer

	borCfg := &params.BorConfig{
		DandeliBlock:           big.NewInt(dandeliBlock),
		LisovoBlock:            big.NewInt(lisovoBlock),
		EnableDynamicTargetGas: &enabled,
		TargetGasMinPercentage: &min,
		TargetGasMaxPercentage: &max,
		TargetBaseFee:          &dbf,
		BaseFeeBuffer:          &buf,
	}

	chainConfig := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		LondonBlock: big.NewInt(0),
		Bor:         borCfg,
	}

	highBaseFee := big.NewInt(40_000_000_000) // 40 gwei → above upper bound

	// Pre-Lisovo block (99): dynamic feature inactive → static percentage (65%)
	preLisovoParent := &types.Header{
		Number:   big.NewInt(99),
		GasLimit: gasLimit,
		BaseFee:  highBaseFee,
	}
	preLisovoTarget := calcParentGasTarget(chainConfig, preLisovoParent)
	expectedStaticTarget := gasLimit * params.TargetGasPercentagePostDandeli / 100

	require.Equal(t, expectedStaticTarget, preLisovoTarget,
		"pre-Lisovo: target gas should use static percentage %d%%", params.TargetGasPercentagePostDandeli)

	// Post-Lisovo block (101) with high base fee: dynamic feature active → TargetGasMaxPercentage (80%)
	postLisovoParent := &types.Header{
		Number:   big.NewInt(101),
		GasLimit: gasLimit,
		BaseFee:  highBaseFee,
	}
	postLisovoTarget := calcParentGasTarget(chainConfig, postLisovoParent)
	expectedDynamicTarget := gasLimit * tGasMaxPct / 100

	require.Equal(t, expectedDynamicTarget, postLisovoTarget,
		"post-Lisovo with high base fee: target gas should use dynamic max percentage %d%%", tGasMaxPct)
}

// TestCalcParentGasTargetDynamicSequence simulates a series of blocks where the base fee
// oscillates above and below the buffer, verifying the percentage jumps correctly.
func TestCalcParentGasTargetDynamicSequence(t *testing.T) {
	t.Parallel()

	const (
		gasLimit   = uint64(30_000_000)
		desiredFee = uint64(30_000_000_000) // 30 gwei
		buffer     = uint64(5_000_000_000)  // 5 gwei → upper=35g, lower=25g
		tGasMinPct = uint64(50)
		tGasMaxPct = uint64(80)
	)

	enabled := true
	min := tGasMinPct
	max := tGasMaxPct
	dbf := desiredFee
	buf := buffer

	borCfg := &params.BorConfig{
		DandeliBlock:           big.NewInt(50),
		LisovoBlock:            big.NewInt(100),
		EnableDynamicTargetGas: &enabled,
		TargetGasMinPercentage: &min,
		TargetGasMaxPercentage: &max,
		TargetBaseFee:          &dbf,
		BaseFeeBuffer:          &buf,
	}

	chainConfig := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		LondonBlock: big.NewInt(0),
		Bor:         borCfg,
	}

	staticTarget := gasLimit * params.TargetGasPercentagePostDandeli / 100
	maxTarget := gasLimit * tGasMaxPct / 100
	minTarget := gasLimit * tGasMinPct / 100

	testCases := []struct {
		blockNum       int64
		baseFeeGwei    int64
		expectedTarget uint64
		description    string
	}{
		{101, 40_000_000_000, maxTarget, "above upper bound → max"},
		{102, 30_000_000_000, staticTarget, "at desired → static"},
		{103, 20_000_000_000, minTarget, "below lower bound → min"},
		{104, 35_000_000_000, staticTarget, "at upper bound (inclusive) → static"},
		{105, 25_000_000_000, staticTarget, "at lower bound (inclusive) → static"},
		{106, 35_000_000_001, maxTarget, "just above upper → max"},
		{107, 24_999_999_999, minTarget, "just below lower → min"},
		{108, 32_000_000_000, staticTarget, "within buffer → static"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("block_%d_%s", tc.blockNum, tc.description), func(t *testing.T) {
			t.Parallel()

			parent := &types.Header{
				Number:   big.NewInt(tc.blockNum),
				GasLimit: gasLimit,
				BaseFee:  big.NewInt(tc.baseFeeGwei),
			}
			target := calcParentGasTarget(chainConfig, parent)
			require.Equal(t, tc.expectedTarget, target,
				"block %d (baseFee=%d): %s", tc.blockNum, tc.baseFeeGwei, tc.description)
		})
	}
}
