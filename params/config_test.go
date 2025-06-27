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
	"testing"
	"time"

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
