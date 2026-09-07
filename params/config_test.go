// Copyright 2017 The go-ethereum Authors
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

package params

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"gotest.tools/assert"
)

func TestCheckCompatible(t *testing.T) {
	type test struct {
		stored, new   *ChainConfig
		headBlock     uint64
		headTimestamp uint64
		wantErr       *ConfigCompatError
	}

	tests := []test{
		{stored: AllEthashProtocolChanges, new: AllEthashProtocolChanges, headBlock: 0, headTimestamp: 0, wantErr: nil},
		{stored: AllEthashProtocolChanges, new: AllEthashProtocolChanges, headBlock: 0, headTimestamp: uint64(time.Now().Unix()), wantErr: nil},
		{stored: AllEthashProtocolChanges, new: AllEthashProtocolChanges, headBlock: 100, wantErr: nil},
		{
			stored:    &ChainConfig{EIP150Block: big.NewInt(10)},
			new:       &ChainConfig{EIP150Block: big.NewInt(20)},
			headBlock: 9,
			wantErr:   nil,
		},
		{
			stored:    AllEthashProtocolChanges,
			new:       &ChainConfig{HomesteadBlock: nil},
			headBlock: 3,
			wantErr: &ConfigCompatError{
				What:          "Homestead fork block",
				StoredBlock:   big.NewInt(0),
				NewBlock:      nil,
				RewindToBlock: 0,
			},
		},
		{
			stored:    AllEthashProtocolChanges,
			new:       &ChainConfig{HomesteadBlock: big.NewInt(1)},
			headBlock: 3,
			wantErr: &ConfigCompatError{
				What:          "Homestead fork block",
				StoredBlock:   big.NewInt(0),
				NewBlock:      big.NewInt(1),
				RewindToBlock: 0,
			},
		},
		{
			stored:    &ChainConfig{HomesteadBlock: big.NewInt(30), EIP150Block: big.NewInt(10)},
			new:       &ChainConfig{HomesteadBlock: big.NewInt(25), EIP150Block: big.NewInt(20)},
			headBlock: 25,
			wantErr: &ConfigCompatError{
				What:          "EIP150 fork block",
				StoredBlock:   big.NewInt(10),
				NewBlock:      big.NewInt(20),
				RewindToBlock: 9,
			},
		},
		{
			stored:    &ChainConfig{ConstantinopleBlock: big.NewInt(30)},
			new:       &ChainConfig{ConstantinopleBlock: big.NewInt(30), PetersburgBlock: big.NewInt(30)},
			headBlock: 40,
			wantErr:   nil,
		},
		{
			stored:    &ChainConfig{ConstantinopleBlock: big.NewInt(30)},
			new:       &ChainConfig{ConstantinopleBlock: big.NewInt(30), PetersburgBlock: big.NewInt(31)},
			headBlock: 40,
			wantErr: &ConfigCompatError{
				What:          "Petersburg fork block",
				StoredBlock:   nil,
				NewBlock:      big.NewInt(31),
				RewindToBlock: 30,
			},
		},
		{
			stored:        &ChainConfig{ShanghaiBlock: big.NewInt(30)},
			new:           &ChainConfig{ShanghaiBlock: big.NewInt(30)},
			headTimestamp: 9,
			wantErr:       nil,
		},
	}

	for _, test := range tests {
		err := test.stored.CheckCompatible(test.new, test.headBlock, test.headTimestamp)
		if !reflect.DeepEqual(err, test.wantErr) {
			t.Errorf("error mismatch:\nstored: %v\nnew: %v\nheadBlock: %v\nheadTimestamp: %v\nerr: %v\nwant: %v", test.stored, test.new, test.headBlock, test.headTimestamp, err, test.wantErr)
		}
	}
}

func TestConfigRules(t *testing.T) {
	t.Parallel()

	c := &ChainConfig{
		LondonBlock:   new(big.Int),
		ShanghaiBlock: big.NewInt(10),
		CancunBlock:   big.NewInt(20),
		PragueBlock:   big.NewInt(30),
		VerkleBlock:   big.NewInt(40),
	}

	block := new(big.Int)

	if r := c.Rules(block, true, 0); r.IsShanghai {
		t.Errorf("expected %v to not be shanghai", 0)
	}

	block.SetInt64(10)

	if r := c.Rules(block, true, 0); !r.IsShanghai {
		t.Errorf("expected %v to be shanghai", 10)
	}

	block.SetInt64(20)

	if r := c.Rules(block, true, 0); !r.IsCancun {
		t.Errorf("expected %v to be cancun", 20)
	}

	block.SetInt64(30)

	if r := c.Rules(block, true, 0); !r.IsPrague {
		t.Errorf("expected %v to be prague", 30)
	}

	block = block.SetInt64(math.MaxInt64)

	if r := c.Rules(block, true, 0); !r.IsShanghai {
		t.Errorf("expected %v to be shanghai", 0)
	}
}

func TestBorKeyValueConfigHelper(t *testing.T) {
	t.Parallel()

	backupMultiplier := map[string]uint64{
		"0":        2,
		"25275000": 5,
		"29638656": 2,
	}
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 0), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 1), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 25275000-1), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 25275000), uint64(5))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 25275000+1), uint64(5))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 29638656-1), uint64(5))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 29638656), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(backupMultiplier, 29638656+1), uint64(2))

	config := map[string]uint64{
		"0":         1,
		"90000000":  2,
		"100000000": 3,
	}
	assert.Equal(t, borKeyValueConfigHelper(config, 0), uint64(1))
	assert.Equal(t, borKeyValueConfigHelper(config, 1), uint64(1))
	assert.Equal(t, borKeyValueConfigHelper(config, 90000000-1), uint64(1))
	assert.Equal(t, borKeyValueConfigHelper(config, 90000000), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(config, 90000000+1), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(config, 100000000-1), uint64(2))
	assert.Equal(t, borKeyValueConfigHelper(config, 100000000), uint64(3))
	assert.Equal(t, borKeyValueConfigHelper(config, 100000000+1), uint64(3))

	burntContract := map[string]string{
		"22640000": "0x70bcA57F4579f58670aB2d18Ef16e02C17553C38",
		"41824608": "0x617b94CCCC2511808A3C9478ebb96f455CF167aA",
	}
	assert.Equal(t, borKeyValueConfigHelper(burntContract, 22640000), "0x70bcA57F4579f58670aB2d18Ef16e02C17553C38")
	assert.Equal(t, borKeyValueConfigHelper(burntContract, 22640000+1), "0x70bcA57F4579f58670aB2d18Ef16e02C17553C38")
	assert.Equal(t, borKeyValueConfigHelper(burntContract, 41824608-1), "0x70bcA57F4579f58670aB2d18Ef16e02C17553C38")
	assert.Equal(t, borKeyValueConfigHelper(burntContract, 41824608), "0x617b94CCCC2511808A3C9478ebb96f455CF167aA")
	assert.Equal(t, borKeyValueConfigHelper(burntContract, 41824608+1), "0x617b94CCCC2511808A3C9478ebb96f455CF167aA")
}

func TestDescriptionValenciaBanner(t *testing.T) {
	t.Parallel()

	scheduled := &ChainConfig{ChainID: big.NewInt(1), Bor: &BorConfig{ValenciaBlock: big.NewInt(100)}}
	if got := scheduled.Description(); !strings.Contains(got, "Valencia:") {
		t.Errorf("expected Valencia banner when ValenciaBlock is set, got:\n%s", got)
	}

	unset := &ChainConfig{ChainID: big.NewInt(1), Bor: &BorConfig{}}
	if got := unset.Description(); strings.Contains(got, "Valencia:") {
		t.Errorf("did not expect Valencia banner when ValenciaBlock is nil, got:\n%s", got)
	}
}

func TestIsValencia(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fork   *big.Int
		number int64
		want   bool
	}{
		{"unset fork never active", nil, 0, false},
		{"unset fork never active at height", nil, 1_000_000, false},
		{"genesis activation at 0", big.NewInt(0), 0, true},
		{"genesis activation above 0", big.NewInt(0), 1, true},
		{"scheduled fork before activation", big.NewInt(100), 99, false},
		{"scheduled fork at activation", big.NewInt(100), 100, true},
		{"scheduled fork after activation", big.NewInt(100), 101, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &BorConfig{ValenciaBlock: tt.fork}
			assert.Equal(t, c.IsValencia(big.NewInt(tt.number)), tt.want)
		})
	}
}

// TestIsAustin covers nil, genesis, and scheduled activation boundaries.
func TestIsAustin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fork   *big.Int
		number int64
		want   bool
	}{
		{"unset fork never active", nil, 0, false},
		{"unset fork never active at height", nil, 1_000_000, false},
		{"genesis activation at 0", big.NewInt(0), 0, true},
		{"genesis activation above 0", big.NewInt(0), 1, true},
		{"scheduled fork before activation", big.NewInt(100), 99, false},
		{"scheduled fork at activation", big.NewInt(100), 100, true},
		{"scheduled fork after activation", big.NewInt(100), 101, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &BorConfig{AustinBlock: tt.fork}
			assert.Equal(t, c.IsAustin(big.NewInt(tt.number)), tt.want)
		})
	}
}

func TestIsHampi(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fork   *big.Int
		number int64
		want   bool
	}{
		{"unset fork never active", nil, 0, false},
		{"unset fork never active at height", nil, 1_000_000, false},
		{"genesis activation at 0", big.NewInt(0), 0, true},
		{"genesis activation above 0", big.NewInt(0), 1, true},
		{"scheduled fork before activation", big.NewInt(100), 99, false},
		{"scheduled fork at activation", big.NewInt(100), 100, true},
		{"scheduled fork after activation", big.NewInt(100), 101, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &BorConfig{HampiBlock: tt.fork}
			assert.Equal(t, c.IsHampi(big.NewInt(tt.number)), tt.want)
		})
	}
}

func TestOverrideStateSyncRecordsInRange(t *testing.T) {
	t.Parallel()

	// Test cases for GetOverrideStateSyncRecord method
	tests := []struct {
		name          string
		config        *BorConfig
		blockNumber   uint64
		expectedValue int
		expectedFound bool
		description   string
	}{
		{
			name: "Empty configuration",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{},
			},
			blockNumber:   100,
			expectedValue: 0,
			expectedFound: false,
			description:   "Should return 0, false when no ranges are configured",
		},
		{
			name: "Single range - block within range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
				},
			},
			blockNumber:   150,
			expectedValue: 5,
			expectedFound: true,
			description:   "Should return configured value when block is within range",
		},
		{
			name: "Single range - block at start boundary",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
				},
			},
			blockNumber:   100,
			expectedValue: 5,
			expectedFound: true,
			description:   "Should return configured value when block is at start boundary",
		},
		{
			name: "Single range - block at end boundary",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
				},
			},
			blockNumber:   200,
			expectedValue: 5,
			expectedFound: true,
			description:   "Should return configured value when block is at end boundary",
		},
		{
			name: "Single range - block before range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
				},
			},
			blockNumber:   50,
			expectedValue: 0,
			expectedFound: false,
			description:   "Should return 0, false when block is before range",
		},
		{
			name: "Single range - block after range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
				},
			},
			blockNumber:   250,
			expectedValue: 0,
			expectedFound: false,
			description:   "Should return 0, false when block is after range",
		},
		{
			name: "Multiple ranges - block in first range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
					{StartBlock: 300, EndBlock: 400, Value: 10},
					{StartBlock: 500, EndBlock: 600, Value: 15},
				},
			},
			blockNumber:   150,
			expectedValue: 5,
			expectedFound: true,
			description:   "Should return value from first range when block is in first range",
		},
		{
			name: "Multiple ranges - block in second range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
					{StartBlock: 300, EndBlock: 400, Value: 10},
					{StartBlock: 500, EndBlock: 600, Value: 15},
				},
			},
			blockNumber:   350,
			expectedValue: 10,
			expectedFound: true,
			description:   "Should return value from second range when block is in second range",
		},
		{
			name: "Multiple ranges - block in third range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
					{StartBlock: 300, EndBlock: 400, Value: 10},
					{StartBlock: 500, EndBlock: 600, Value: 15},
				},
			},
			blockNumber:   550,
			expectedValue: 15,
			expectedFound: true,
			description:   "Should return value from third range when block is in third range",
		},
		{
			name: "Multiple ranges - block between ranges",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 5},
					{StartBlock: 300, EndBlock: 400, Value: 10},
					{StartBlock: 500, EndBlock: 600, Value: 15},
				},
			},
			blockNumber:   250,
			expectedValue: 0,
			expectedFound: false,
			description:   "Should return 0, false when block is between ranges",
		},
		{
			name: "Overlapping ranges - should return first match",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 300, Value: 5},
					{StartBlock: 200, EndBlock: 400, Value: 10},
				},
			},
			blockNumber:   250,
			expectedValue: 5,
			expectedFound: true,
			description:   "Should return value from first matching range when ranges overlap",
		},
		{
			name: "Zero value range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: 0},
				},
			},
			blockNumber:   150,
			expectedValue: 0,
			expectedFound: true,
			description:   "Should return 0, true when range value is 0",
		},
		{
			name: "Negative value range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 200, Value: -5},
				},
			},
			blockNumber:   150,
			expectedValue: -5,
			expectedFound: true,
			description:   "Should return negative value when range value is negative",
		},
		{
			name: "Single block range",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 100, EndBlock: 100, Value: 5},
				},
			},
			blockNumber:   100,
			expectedValue: 5,
			expectedFound: true,
			description:   "Should work correctly with single block range",
		},
		{
			name: "Large block numbers",
			config: &BorConfig{
				OverrideStateSyncRecordsInRange: []BlockRangeOverride{
					{StartBlock: 1000000, EndBlock: 2000000, Value: 25},
				},
			},
			blockNumber:   1500000,
			expectedValue: 25,
			expectedFound: true,
			description:   "Should work correctly with large block numbers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, found := tt.config.GetOverrideStateSyncRecord(tt.blockNumber)

			if value != tt.expectedValue {
				t.Errorf("GetOverrideStateSyncRecord(%d) returned value = %d, want %d",
					tt.blockNumber, value, tt.expectedValue)
			}

			if found != tt.expectedFound {
				t.Errorf("GetOverrideStateSyncRecord(%d) returned found = %v, want %v",
					tt.blockNumber, found, tt.expectedFound)
			}
		})
	}
}

func TestBlockRangeOverrideStruct(t *testing.T) {
	t.Parallel()

	// Test BlockRangeOverride struct creation and field access
	override := BlockRangeOverride{
		StartBlock: 100,
		EndBlock:   200,
		Value:      5,
	}

	if override.StartBlock != 100 {
		t.Errorf("Expected StartBlock to be 100, got %d", override.StartBlock)
	}

	if override.EndBlock != 200 {
		t.Errorf("Expected EndBlock to be 200, got %d", override.EndBlock)
	}

	if override.Value != 5 {
		t.Errorf("Expected Value to be 5, got %d", override.Value)
	}

	// Test edge case where StartBlock equals EndBlock
	singleBlockOverride := BlockRangeOverride{
		StartBlock: 100,
		EndBlock:   100,
		Value:      10,
	}

	if singleBlockOverride.StartBlock != singleBlockOverride.EndBlock {
		t.Errorf("Expected StartBlock and EndBlock to be equal for single block range")
	}
}

func TestOverrideStateSyncRecordsInRangeIntegration(t *testing.T) {
	t.Parallel()

	// Test integration with actual BorConfig usage
	borConfig := &BorConfig{
		OverrideStateSyncRecordsInRange: []BlockRangeOverride{
			{StartBlock: 1000, EndBlock: 2000, Value: 3},
			{StartBlock: 3000, EndBlock: 4000, Value: 7},
			{StartBlock: 5000, EndBlock: 6000, Value: 0},
		},
	}

	// Test various scenarios
	testCases := []struct {
		blockNumber   uint64
		expectedValue int
		expectedFound bool
	}{
		{500, 0, false},  // Before all ranges
		{1000, 3, true},  // At start of first range
		{1500, 3, true},  // Middle of first range
		{2000, 3, true},  // At end of first range
		{2500, 0, false}, // Between ranges
		{3000, 7, true},  // At start of second range
		{3500, 7, true},  // Middle of second range
		{4000, 7, true},  // At end of second range
		{4500, 0, false}, // Between ranges
		{5000, 0, true},  // At start of third range (value is 0)
		{5500, 0, true},  // Middle of third range (value is 0)
		{6000, 0, true},  // At end of third range (value is 0)
		{6500, 0, false}, // After all ranges
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("Block_%d", tc.blockNumber), func(t *testing.T) {
			value, found := borConfig.GetOverrideStateSyncRecord(tc.blockNumber)

			if value != tc.expectedValue {
				t.Errorf("Block %d: expected value %d, got %d",
					tc.blockNumber, tc.expectedValue, value)
			}

			if found != tc.expectedFound {
				t.Errorf("Block %d: expected found %v, got %v",
					tc.blockNumber, tc.expectedFound, found)
			}
		})
	}
}

func TestOverrideStateSyncRecordsInRangeExample(t *testing.T) {
	t.Parallel()

	// Example: Configure state sync record overrides for specific block ranges
	// This simulates a real-world scenario where you want to limit state sync records
	// for certain block ranges to control gas usage or processing time
	borConfig := &BorConfig{
		OverrideStateSyncRecordsInRange: []BlockRangeOverride{
			// Limit to 5 state sync records for blocks 1000-2000
			{StartBlock: 1000, EndBlock: 2000, Value: 5},
			// Disable state sync records for blocks 3000-4000 (maintenance period)
			{StartBlock: 3000, EndBlock: 4000, Value: 0},
			// Allow up to 10 state sync records for blocks 5000-6000
			{StartBlock: 5000, EndBlock: 6000, Value: 10},
			// Normal operation resumes after block 6000 (no override)
		},
	}

	// Test the configuration
	testCases := []struct {
		blockNumber   uint64
		expectedValue int
		expectedFound bool
		scenario      string
	}{
		{500, 0, false, "Before any overrides - normal operation"},
		{1000, 5, true, "Start of first override range - limit to 5 records"},
		{1500, 5, true, "Middle of first override range - limit to 5 records"},
		{2000, 5, true, "End of first override range - limit to 5 records"},
		{2500, 0, false, "Between ranges - normal operation"},
		{3000, 0, true, "Start of maintenance period - no state sync records"},
		{3500, 0, true, "Middle of maintenance period - no state sync records"},
		{4000, 0, true, "End of maintenance period - no state sync records"},
		{4500, 0, false, "Between ranges - normal operation"},
		{5000, 10, true, "Start of high-capacity range - allow up to 10 records"},
		{5500, 10, true, "Middle of high-capacity range - allow up to 10 records"},
		{6000, 10, true, "End of high-capacity range - allow up to 10 records"},
		{6500, 0, false, "After all overrides - normal operation"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("Block_%d_%s", tc.blockNumber, tc.scenario), func(t *testing.T) {
			value, found := borConfig.GetOverrideStateSyncRecord(tc.blockNumber)

			if value != tc.expectedValue {
				t.Errorf("Block %d (%s): expected value %d, got %d",
					tc.blockNumber, tc.scenario, tc.expectedValue, value)
			}

			if found != tc.expectedFound {
				t.Errorf("Block %d (%s): expected found %v, got %v",
					tc.blockNumber, tc.scenario, tc.expectedFound, found)
			}
		})
	}

	// Demonstrate how this would be used in practice
	t.Run("Practical Usage Example", func(t *testing.T) {
		// Simulate processing a block and checking for overrides
		blockNumber := uint64(1500)
		overrideValue, hasOverride := borConfig.GetOverrideStateSyncRecord(blockNumber)

		if !hasOverride {
			t.Fatal("Expected to find override for block 1500")
		}

		if overrideValue != 5 {
			t.Fatalf("Expected override value 5, got %d", overrideValue)
		}

		// In practice, this value would be used to limit the number of state sync records
		// processed for this block, e.g.:
		// eventRecords = eventRecords[:overrideValue]
		t.Logf("Block %d: Processing maximum %d state sync records due to override",
			blockNumber, overrideValue)
	})
}

func TestCalculateCoinbase(t *testing.T) {
	t.Parallel()

	// Test case 1: Nil coinbase configuration
	t.Run("Nil coinbase configuration", func(t *testing.T) {
		config := &BorConfig{}

		result := config.CalculateCoinbase(100)
		expected := "0x0000000000000000000000000000000000000000"

		if result != expected {
			t.Errorf("Expected %s, got %s", expected, result)
		}
	})

	// Test case 4: Multiple coinbase addresses with block transitions
	t.Run("Multiple coinbase addresses", func(t *testing.T) {
		config := &BorConfig{
			Coinbase: map[string]string{
				"0":     "0x1111111111111111111111111111111111111111",
				"1000":  "0x2222222222222222222222222222222222222222",
				"5000":  "0x3333333333333333333333333333333333333333",
				"10000": "0x4444444444444444444444444444444444444444",
			},
		}

		testCases := []struct {
			blockNumber uint64
			expected    string
			description string
		}{
			{0, "0x1111111111111111111111111111111111111111", "At genesis block"},
			{500, "0x1111111111111111111111111111111111111111", "Before first transition"},
			{999, "0x1111111111111111111111111111111111111111", "Just before first transition"},
			{1000, "0x2222222222222222222222222222222222222222", "At first transition"},
			{1001, "0x2222222222222222222222222222222222222222", "Just after first transition"},
			{3000, "0x2222222222222222222222222222222222222222", "Between first and second transition"},
			{4999, "0x2222222222222222222222222222222222222222", "Just before second transition"},
			{5000, "0x3333333333333333333333333333333333333333", "At second transition"},
			{7500, "0x3333333333333333333333333333333333333333", "Between second and third transition"},
			{9999, "0x3333333333333333333333333333333333333333", "Just before third transition"},
			{10000, "0x4444444444444444444444444444444444444444", "At third transition"},
			{15000, "0x4444444444444444444444444444444444444444", "After final transition"},
			{999999, "0x4444444444444444444444444444444444444444", "Far beyond final transition"},
		}

		for _, tc := range testCases {
			result := config.CalculateCoinbase(tc.blockNumber)
			if result != tc.expected {
				t.Errorf("Block %d (%s): expected %s, got %s",
					tc.blockNumber, tc.description, tc.expected, result)
			}
		}
	})
}

// TestBlockAlloc_NoPrecisionLoss makes sure that
// the current BlockAlloc balances (updated at Oct 22, 2025) fit in 256 bits
// and do not change numerically when moving from Uint64() to MustFromBig.
func TestBlockAlloc_NoPrecisionLoss(t *testing.T) {
	blockAllocs := currentBlockAllocs()
	maxU64 := new(big.Int).SetUint64(^uint64(0))

	for network, baBlock := range blockAllocs {
		for blockNum, alloc := range baBlock {
			for addr, account := range alloc {
				acct, ok := account.(map[string]interface{})
				if !ok {
					t.Fatalf("[%s @ %s] addr %s: unexpected account type %T", network, blockNum, addr, account)
				}
				rawBal, ok := acct["balance"]
				if !ok {
					t.Fatalf("[%s @ %s] addr %s: missing balance", network, blockNum, addr)
				}
				bal, err := parseBalance(rawBal)
				if err != nil {
					t.Fatalf("[%s @ %s] addr %s: parse balance error: %v", network, blockNum, addr, err)
				}
				// 256-bit fit check
				if bal.BitLen() > 256 {
					t.Fatalf("[%s @ %s] addr %s: balance bitlen=%d exceeds 256 bits",
						network, blockNum, addr, bal.BitLen())
				}
				// Compare NewInt(bal.Uint64()) and uint256.FromBig(bal)
				oldValue := uint256.NewInt(bal.Uint64())
				newValue, overflow := uint256.FromBig(bal)
				if overflow {
					t.Fatalf("[%s @ %s] addr %s: overflow converting to uint256", network, blockNum, addr)
				}
				if newValue.Cmp(oldValue) != 0 {
					// bock alloc balance truncated (bal > MaxUint64)
					t.Fatalf("[%s @ %s] addr %s: precision loss (balance %s > MaxUint64)",
						network, blockNum, addr, bal.String())
				}
				// same numeric conditions
				if bal.Cmp(maxU64) > 0 {
					t.Fatalf("[%s @ %s] addr %s: balance %s > MaxUint64 (would truncate)",
						network, blockNum, addr, bal.String())
				}
			}
		}
	}
}

// parseBalance converts a 0x-value from blockAlloc's balance into *big.Int.
func parseBalance(v interface{}) (*big.Int, error) {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, fmt.Errorf("empty balance string")
		}
		z := new(big.Int)
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			if _, ok := z.SetString(s[2:], 16); !ok {
				return nil, fmt.Errorf("invalid hex balance: %q", s)
			}
			return z, nil
		} else {
			return nil, fmt.Errorf("unsupported balance type %T (%v)", v, v)
		}
	default:
		return nil, fmt.Errorf("unsupported balance type %T (%v)", v, v)
	}
}

// currentBlockAllocs returns the allocs pasted from config.go.
func currentBlockAllocs() map[string]map[string]map[string]interface{} {
	return map[string]map[string]map[string]interface{}{
		// Mumbai
		"mumbai": {
			"22244000": {
				"0000000000000000000000000000000000001010": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
			"41874000": {
				"0x0000000000000000000000000000000000001001": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
		},
		// Amoy
		"amoy": {
			"11865856": {
				"0000000000000000000000000000000000001001": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
				"0000000000000000000000000000000000001010": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
				"360ad4f9a9A8EFe9A8DCB5f461c4Cc1047E1Dcf9": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
			"12121856": {
				"360ad4f9a9A8EFe9A8DCB5f461c4Cc1047E1Dcf9": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
		},
		// Mainnet
		"mainnet": {
			"22156660": {
				"0000000000000000000000000000000000001010": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
			"50523000": {
				"0x0000000000000000000000000000000000001001": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
			"62278656": {
				"0000000000000000000000000000000000001001": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
				"0000000000000000000000000000000000001010": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
				"0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270": map[string]interface{}{
					"balance": "0x0",
					"code":    "...",
				},
			},
		},
	}
}

func TestGetTargetGasPercentage(t *testing.T) {
	t.Parallel()

	t.Run("Pre-Dandeli block returns 0", func(t *testing.T) {
		config := &BorConfig{
			DandeliBlock: big.NewInt(1000),
		}

		// Test before Dandeli fork
		result := config.GetTargetGasPercentage(big.NewInt(500))
		if result != 0 {
			t.Errorf("Pre-Dandeli: expected 0, got %d", result)
		}

		result = config.GetTargetGasPercentage(big.NewInt(999))
		if result != 0 {
			t.Errorf("Pre-Dandeli (block 999): expected 0, got %d", result)
		}
	})

	t.Run("Post-Dandeli with nil TargetGasPercentage returns default", func(t *testing.T) {
		config := &BorConfig{
			DandeliBlock:        big.NewInt(1000),
			TargetGasPercentage: nil,
		}

		result := config.GetTargetGasPercentage(big.NewInt(1000))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("Post-Dandeli with nil: expected %d, got %d", TargetGasPercentagePostDandeli, result)
		}

		result = config.GetTargetGasPercentage(big.NewInt(2000))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("Post-Dandeli with nil (block 2000): expected %d, got %d", TargetGasPercentagePostDandeli, result)
		}
	})

	t.Run("Post-Lisovo with valid custom values", func(t *testing.T) {
		testCases := []struct {
			customValue uint64
			description string
		}{
			{1, "minimum valid value"},
			{50, "mid-range value"},
			{100, "maximum valid value"},
			{65, "default value"},
		}

		for _, tc := range testCases {
			t.Run(tc.description, func(t *testing.T) {
				val := tc.customValue
				config := &BorConfig{
					DandeliBlock:        big.NewInt(1000),
					LisovoBlock:         big.NewInt(1000), // Configurable params require Lisovo
					TargetGasPercentage: &val,
				}

				result := config.GetTargetGasPercentage(big.NewInt(1000))
				if result != tc.customValue {
					t.Errorf("Post-Lisovo with custom value %d: expected %d, got %d",
						tc.customValue, tc.customValue, result)
				}
			})
		}
	})

	t.Run("Post-Dandeli with invalid value 0 falls back to default", func(t *testing.T) {
		invalidVal := uint64(0)
		config := &BorConfig{
			DandeliBlock:        big.NewInt(1000),
			TargetGasPercentage: &invalidVal,
		}

		result := config.GetTargetGasPercentage(big.NewInt(1000))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("Post-Dandeli with invalid value 0: expected %d, got %d",
				TargetGasPercentagePostDandeli, result)
		}
	})

	t.Run("Post-Dandeli with invalid value >100 falls back to default", func(t *testing.T) {
		testCases := []uint64{101, 200, 1000}

		for _, invalidVal := range testCases {
			t.Run(fmt.Sprintf("value_%d", invalidVal), func(t *testing.T) {
				val := invalidVal
				config := &BorConfig{
					DandeliBlock:        big.NewInt(1000),
					TargetGasPercentage: &val,
				}

				result := config.GetTargetGasPercentage(big.NewInt(1000))
				if result != TargetGasPercentagePostDandeli {
					t.Errorf("Post-Dandeli with invalid value %d: expected %d, got %d",
						invalidVal, TargetGasPercentagePostDandeli, result)
				}
			})
		}
	})
}

func TestGetBaseFeeChangeDenominator(t *testing.T) {
	t.Parallel()

	t.Run("Pre-Delhi returns DefaultBaseFeeChangeDenominator", func(t *testing.T) {
		config := &BorConfig{
			DelhiBlock:  big.NewInt(1000),
			BhilaiBlock: nil,
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(500))
		if result != DefaultBaseFeeChangeDenominator {
			t.Errorf("Pre-Delhi: expected %d, got %d", DefaultBaseFeeChangeDenominator, result)
		}

		result = BaseFeeChangeDenominator(config, big.NewInt(999))
		if result != DefaultBaseFeeChangeDenominator {
			t.Errorf("Pre-Delhi (block 999): expected %d, got %d", DefaultBaseFeeChangeDenominator, result)
		}
	})

	t.Run("Post-Delhi Pre-Bhilai returns BaseFeeChangeDenominatorPostDelhi", func(t *testing.T) {
		config := &BorConfig{
			DelhiBlock:   big.NewInt(1000),
			BhilaiBlock:  big.NewInt(2000),
			DandeliBlock: nil,
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(1000))
		if result != BaseFeeChangeDenominatorPostDelhi {
			t.Errorf("Post-Delhi, Pre-Bhilai (block 1000): expected %d, got %d",
				BaseFeeChangeDenominatorPostDelhi, result)
		}

		result = BaseFeeChangeDenominator(config, big.NewInt(1500))
		if result != BaseFeeChangeDenominatorPostDelhi {
			t.Errorf("Post-Delhi, Pre-Bhilai (block 1500): expected %d, got %d",
				BaseFeeChangeDenominatorPostDelhi, result)
		}

		result = BaseFeeChangeDenominator(config, big.NewInt(1999))
		if result != BaseFeeChangeDenominatorPostDelhi {
			t.Errorf("Post-Delhi, Pre-Bhilai (block 1999): expected %d, got %d",
				BaseFeeChangeDenominatorPostDelhi, result)
		}
	})

	t.Run("Post-Bhilai Pre-Dandeli returns BaseFeeChangeDenominatorPostBhilai", func(t *testing.T) {
		config := &BorConfig{
			DelhiBlock:   big.NewInt(1000),
			BhilaiBlock:  big.NewInt(2000),
			DandeliBlock: big.NewInt(3000),
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(2000))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Post-Bhilai, Pre-Dandeli (block 2000): expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}

		result = BaseFeeChangeDenominator(config, big.NewInt(2500))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Post-Bhilai, Pre-Dandeli (block 2500): expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}

		result = BaseFeeChangeDenominator(config, big.NewInt(2999))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Post-Bhilai, Pre-Dandeli (block 2999): expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}
	})

	t.Run("Post-Lisovo with nil custom value falls back to Bhilai default", func(t *testing.T) {
		config := &BorConfig{
			DelhiBlock:               big.NewInt(1000),
			BhilaiBlock:              big.NewInt(2000),
			DandeliBlock:             big.NewInt(3000),
			LisovoBlock:              big.NewInt(3000), // Configurable params require Lisovo
			BaseFeeChangeDenominator: nil,
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(3000))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Post-Lisovo with nil custom value: expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}

		result = BaseFeeChangeDenominator(config, big.NewInt(4000))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Post-Lisovo with nil custom value (block 4000): expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}
	})

	t.Run("Post-Lisovo with valid custom value", func(t *testing.T) {
		testCases := []uint64{1, 8, 16, 32, 64, 128}

		for _, customVal := range testCases {
			t.Run(fmt.Sprintf("value_%d", customVal), func(t *testing.T) {
				val := customVal
				config := &BorConfig{
					DelhiBlock:               big.NewInt(1000),
					BhilaiBlock:              big.NewInt(2000),
					DandeliBlock:             big.NewInt(3000),
					LisovoBlock:              big.NewInt(3000), // Configurable params require Lisovo
					BaseFeeChangeDenominator: &val,
				}

				result := BaseFeeChangeDenominator(config, big.NewInt(3000))
				if result != customVal {
					t.Errorf("Post-Lisovo with custom value %d: expected %d, got %d",
						customVal, customVal, result)
				}
			})
		}
	})

	t.Run("Post-Lisovo with invalid value 0 falls back to Bhilai default", func(t *testing.T) {
		invalidVal := uint64(0)
		config := &BorConfig{
			DelhiBlock:               big.NewInt(1000),
			BhilaiBlock:              big.NewInt(2000),
			DandeliBlock:             big.NewInt(3000),
			LisovoBlock:              big.NewInt(3000), // Configurable params require Lisovo
			BaseFeeChangeDenominator: &invalidVal,
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(3000))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Post-Lisovo with invalid value 0: expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}
	})

	t.Run("Pre-Lisovo with custom value ignores it", func(t *testing.T) {
		customVal := uint64(999)
		config := &BorConfig{
			DelhiBlock:               big.NewInt(1000),
			BhilaiBlock:              big.NewInt(2000),
			DandeliBlock:             big.NewInt(3000),
			LisovoBlock:              big.NewInt(4000), // Lisovo after Dandeli
			BaseFeeChangeDenominator: &customVal,
		}

		// Before Lisovo, custom value should be ignored
		result := BaseFeeChangeDenominator(config, big.NewInt(3500))
		if result != BaseFeeChangeDenominatorPostBhilai {
			t.Errorf("Pre-Lisovo with custom value (block 3500): expected %d, got %d",
				BaseFeeChangeDenominatorPostBhilai, result)
		}
	})

	t.Run("Post-Dandeli but Pre-Bhilai falls back to Delhi default", func(t *testing.T) {
		config := &BorConfig{
			DelhiBlock:   big.NewInt(1000),
			BhilaiBlock:  nil,
			DandeliBlock: big.NewInt(2000),
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(2000))
		if result != BaseFeeChangeDenominatorPostDelhi {
			t.Errorf("Post-Dandeli, Pre-Bhilai: expected %d, got %d",
				BaseFeeChangeDenominatorPostDelhi, result)
		}
	})

	t.Run("Post-Dandeli but Pre-Delhi falls back to default", func(t *testing.T) {
		config := &BorConfig{
			DelhiBlock:   nil,
			BhilaiBlock:  nil,
			DandeliBlock: big.NewInt(1000),
		}

		result := BaseFeeChangeDenominator(config, big.NewInt(1000))
		if result != DefaultBaseFeeChangeDenominator {
			t.Errorf("Post-Dandeli, Pre-Delhi: expected %d, got %d",
				DefaultBaseFeeChangeDenominator, result)
		}
	})
}

func TestGetDynamicTargetGasPercentage(t *testing.T) {
	t.Parallel()

	const (
		desiredBaseFee   = uint64(30_000_000_000) // 30 gwei
		buffer           = uint64(5_000_000_000)  // 5 gwei → upper=35g, lower=25g
		targetGasMin     = uint64(50)             // 50%
		targetGasMax     = uint64(80)             // 80%
		staticPercentage = TargetGasPercentagePostDandeli
		lisovoBlockNum   = int64(100)
		dandeliBlockNum  = int64(50)
	)

	// Helper to build a config with dynamic target gas enabled
	newConfig := func(enabled bool) *BorConfig {
		en := enabled
		min := targetGasMin
		max := targetGasMax
		dbf := desiredBaseFee
		buf := buffer
		return &BorConfig{
			DandeliBlock:           big.NewInt(dandeliBlockNum),
			LisovoBlock:            big.NewInt(lisovoBlockNum),
			EnableDynamicTargetGas: &en,
			TargetGasMinPercentage: &min,
			TargetGasMaxPercentage: &max,
			TargetBaseFee:          &dbf,
			BaseFeeBuffer:          &buf,
		}
	}

	tests := []struct {
		name           string
		config         *BorConfig
		number         *big.Int
		parentBaseFee  *big.Int
		expectedResult uint64
	}{
		{
			name:           "disabled_returns_static",
			config:         newConfig(false),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(50_000_000_000),
			expectedResult: staticPercentage,
		},
		{
			name:           "pre_lisovo_returns_static",
			config:         newConfig(true),
			number:         big.NewInt(99),
			parentBaseFee:  big.NewInt(50_000_000_000),
			expectedResult: staticPercentage,
		},
		{
			name:           "nil_base_fee_returns_static",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  nil,
			expectedResult: staticPercentage,
		},
		{
			name:           "high_base_fee_returns_max",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(40_000_000_000), // 40 gwei > upper(35 gwei)
			expectedResult: targetGasMax,
		},
		{
			name:           "low_base_fee_returns_min",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(20_000_000_000), // 20 gwei < lower(25 gwei)
			expectedResult: targetGasMin,
		},
		{
			name:           "within_buffer_at_target_returns_static",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(30_000_000_000), // 30 gwei == desired
			expectedResult: staticPercentage,
		},
		{
			name:           "at_upper_bound_returns_static",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(35_000_000_000), // 35 gwei == desired+buffer
			expectedResult: staticPercentage,
		},
		{
			name:           "at_lower_bound_returns_static",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(25_000_000_000), // 25 gwei == desired-buffer
			expectedResult: staticPercentage,
		},
		{
			name:           "just_above_upper_returns_max",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(35_000_000_001), // just above upper bound
			expectedResult: targetGasMax,
		},
		{
			name:           "just_below_lower_returns_min",
			config:         newConfig(true),
			number:         big.NewInt(101),
			parentBaseFee:  big.NewInt(24_999_999_999), // just below lower bound
			expectedResult: targetGasMin,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := tc.config.GetDynamicTargetGasPercentage(tc.parentBaseFee, tc.number)
			if result != tc.expectedResult {
				t.Errorf("GetDynamicTargetGasPercentage() = %d, want %d", result, tc.expectedResult)
			}
		})
	}
}

func TestGetDynamicTargetGasPercentage_BufferUnderflow(t *testing.T) {
	t.Parallel()

	// buffer > desiredBaseFee → lowerBound clamps to 0, so no base fee triggers the "low" branch
	en := true
	min := uint64(50)
	max := uint64(80)
	desiredBaseFee := uint64(10_000_000_000) // 10 gwei
	buffer := uint64(50_000_000_000)         // 50 gwei — larger than desired

	config := &BorConfig{
		DandeliBlock:           big.NewInt(50),
		LisovoBlock:            big.NewInt(100),
		EnableDynamicTargetGas: &en,
		TargetGasMinPercentage: &min,
		TargetGasMaxPercentage: &max,
		TargetBaseFee:          &desiredBaseFee,
		BaseFeeBuffer:          &buffer,
	}

	// 5 gwei: upperBound = 10+50 = 60 gwei, lowerBound = 0 (underflow guard)
	// baseFee(5g) is NOT < lowerBound(0) and NOT > upperBound(60g) → within buffer → static
	result := config.GetDynamicTargetGasPercentage(big.NewInt(5_000_000_000), big.NewInt(101))
	if result != TargetGasPercentagePostDandeli {
		t.Errorf("buffer underflow, 5 gwei: expected %d (static), got %d", TargetGasPercentagePostDandeli, result)
	}

	// 0 wei: still not < 0 → within buffer → static
	result = config.GetDynamicTargetGasPercentage(big.NewInt(0), big.NewInt(101))
	if result != TargetGasPercentagePostDandeli {
		t.Errorf("buffer underflow, 0 wei: expected %d (static), got %d", TargetGasPercentagePostDandeli, result)
	}

	// 65 gwei: exceeds upperBound(60 gwei) → returns max
	result = config.GetDynamicTargetGasPercentage(big.NewInt(65_000_000_000), big.NewInt(101))
	if result != max {
		t.Errorf("buffer underflow, 65 gwei (above upper): expected %d (max), got %d", max, result)
	}
}

func TestGetDynamicTargetGasPercentage_NilDesiredBaseFee(t *testing.T) {
	t.Parallel()

	en := true
	min := uint64(50)
	max := uint64(80)

	config := &BorConfig{
		DandeliBlock:           big.NewInt(50),
		LisovoBlock:            big.NewInt(100),
		EnableDynamicTargetGas: &en,
		TargetGasMinPercentage: &min,
		TargetGasMaxPercentage: &max,
		TargetBaseFee:          nil, // not set — misconfiguration
	}

	// Should fall back to static with log.Error
	result := config.GetDynamicTargetGasPercentage(big.NewInt(40_000_000_000), big.NewInt(101))
	if result != TargetGasPercentagePostDandeli {
		t.Errorf("nil TargetBaseFee: expected static %d, got %d", TargetGasPercentagePostDandeli, result)
	}
}

func TestGetDynamicTargetGasPercentage_InvalidMinMax(t *testing.T) {
	t.Parallel()

	en := true
	desiredBaseFee := uint64(30_000_000_000)

	t.Run("nil_TargetGasMaxPercentage_falls_back_to_static", func(t *testing.T) {
		t.Parallel()

		min := uint64(50)
		config := &BorConfig{
			DandeliBlock:           big.NewInt(50),
			LisovoBlock:            big.NewInt(100),
			EnableDynamicTargetGas: &en,
			TargetGasMinPercentage: &min,
			TargetGasMaxPercentage: nil, // nil
			TargetBaseFee:          &desiredBaseFee,
		}

		// base fee above upper → should return TargetGasMaxPercentage, but it's nil → static fallback
		result := config.GetDynamicTargetGasPercentage(big.NewInt(40_000_000_000), big.NewInt(101))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("nil TargetGasMaxPercentage: expected static %d, got %d", TargetGasPercentagePostDandeli, result)
		}
	})

	t.Run("nil_TargetGasMinPercentage_falls_back_to_static", func(t *testing.T) {
		t.Parallel()

		max := uint64(80)
		config := &BorConfig{
			DandeliBlock:           big.NewInt(50),
			LisovoBlock:            big.NewInt(100),
			EnableDynamicTargetGas: &en,
			TargetGasMinPercentage: nil, // nil
			TargetGasMaxPercentage: &max,
			TargetBaseFee:          &desiredBaseFee,
		}

		// base fee below lower → should return TargetGasMinPercentage, but it's nil → static fallback
		result := config.GetDynamicTargetGasPercentage(big.NewInt(10_000_000_000), big.NewInt(101))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("nil TargetGasMinPercentage: expected static %d, got %d", TargetGasPercentagePostDandeli, result)
		}
	})

	t.Run("TargetGasMaxPercentage_zero_falls_back_to_static", func(t *testing.T) {
		t.Parallel()

		min := uint64(50)
		zero := uint64(0)
		config := &BorConfig{
			DandeliBlock:           big.NewInt(50),
			LisovoBlock:            big.NewInt(100),
			EnableDynamicTargetGas: &en,
			TargetGasMinPercentage: &min,
			TargetGasMaxPercentage: &zero,
			TargetBaseFee:          &desiredBaseFee,
		}

		result := config.GetDynamicTargetGasPercentage(big.NewInt(40_000_000_000), big.NewInt(101))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("TargetGasMaxPercentage=0: expected static %d, got %d", TargetGasPercentagePostDandeli, result)
		}
	})

	t.Run("TargetGasMaxPercentage_over_100_falls_back_to_static", func(t *testing.T) {
		t.Parallel()

		min := uint64(50)
		over := uint64(101)
		config := &BorConfig{
			DandeliBlock:           big.NewInt(50),
			LisovoBlock:            big.NewInt(100),
			EnableDynamicTargetGas: &en,
			TargetGasMinPercentage: &min,
			TargetGasMaxPercentage: &over,
			TargetBaseFee:          &desiredBaseFee,
		}

		result := config.GetDynamicTargetGasPercentage(big.NewInt(40_000_000_000), big.NewInt(101))
		if result != TargetGasPercentagePostDandeli {
			t.Errorf("TargetGasMaxPercentage=101: expected static %d, got %d", TargetGasPercentagePostDandeli, result)
		}
	})
}
