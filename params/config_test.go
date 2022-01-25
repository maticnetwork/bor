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
	"math/big"
	"reflect"
	"strconv"
	"testing"
)

func TestCheckCompatible(t *testing.T) {
	type test struct {
		stored, new *ChainConfig
		head        uint64
		wantErr     *ConfigCompatError
	}
	tests := []test{
		{stored: AllEthashProtocolChanges, new: AllEthashProtocolChanges, head: 0, wantErr: nil},
		{stored: AllEthashProtocolChanges, new: AllEthashProtocolChanges, head: 100, wantErr: nil},
		{
			stored:  &ChainConfig{EIP150Block: big.NewInt(10)},
			new:     &ChainConfig{EIP150Block: big.NewInt(20)},
			head:    9,
			wantErr: nil,
		},
		{
			stored: AllEthashProtocolChanges,
			new:    &ChainConfig{HomesteadBlock: nil},
			head:   3,
			wantErr: &ConfigCompatError{
				What:         "Homestead fork block",
				StoredConfig: big.NewInt(0),
				NewConfig:    nil,
				RewindTo:     0,
			},
		},
		{
			stored: AllEthashProtocolChanges,
			new:    &ChainConfig{HomesteadBlock: big.NewInt(1)},
			head:   3,
			wantErr: &ConfigCompatError{
				What:         "Homestead fork block",
				StoredConfig: big.NewInt(0),
				NewConfig:    big.NewInt(1),
				RewindTo:     0,
			},
		},
		{
			stored: &ChainConfig{HomesteadBlock: big.NewInt(30), EIP150Block: big.NewInt(10)},
			new:    &ChainConfig{HomesteadBlock: big.NewInt(25), EIP150Block: big.NewInt(20)},
			head:   25,
			wantErr: &ConfigCompatError{
				What:         "EIP150 fork block",
				StoredConfig: big.NewInt(10),
				NewConfig:    big.NewInt(20),
				RewindTo:     9,
			},
		},
		{
			stored:  &ChainConfig{ConstantinopleBlock: big.NewInt(30)},
			new:     &ChainConfig{ConstantinopleBlock: big.NewInt(30), PetersburgBlock: big.NewInt(30)},
			head:    40,
			wantErr: nil,
		},
		{
			stored: &ChainConfig{ConstantinopleBlock: big.NewInt(30)},
			new:    &ChainConfig{ConstantinopleBlock: big.NewInt(30), PetersburgBlock: big.NewInt(31)},
			head:   40,
			wantErr: &ConfigCompatError{
				What:         "Petersburg fork block",
				StoredConfig: nil,
				NewConfig:    big.NewInt(31),
				RewindTo:     30,
			},
		},
	}

	for _, test := range tests {
		err := test.stored.CheckCompatible(test.new, test.head)
		if !reflect.DeepEqual(err, test.wantErr) {
			t.Errorf("error mismatch:\nstored: %v\nnew: %v\nhead: %v\nerr: %v\nwant: %v", test.stored, test.new, test.head, err, test.wantErr)
		}
	}
}

func copyConfig(original *ChainConfig) *ChainConfig {
	return &ChainConfig{
		ChainID:                 original.ChainID,
		HomesteadBlock:          original.HomesteadBlock,
		DAOForkBlock:            original.DAOForkBlock,
		DAOForkSupport:          original.DAOForkSupport,
		EIP150Block:             original.EIP150Block,
		EIP150Hash:              original.EIP150Hash,
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
		Bor:                     original.Bor,
		FeeConfigMap:            original.FeeConfigMap,
	}
}

func config() *ChainConfig {
	config := copyConfig(TestChainConfig)
	config.LondonBlock = big.NewInt(5)
	return config
}

func TestChainConfig_GetFeeConfig(t *testing.T) {
	tests := []struct {
		blockNumber                      string
		feeConfig                        FeeConfig
		expectedBaseFeeChangeDenominator uint64
		expectedElasticityMultiplier     float64
	}{
		{
			blockNumber: "5",
			feeConfig: FeeConfig{
				BaseFeeChangeDenominator: 2,
				ElasticityMultiplier:     1.5,
			},
			expectedBaseFeeChangeDenominator: 2,
			expectedElasticityMultiplier:     1.5,
		},
		{
			blockNumber: "15",
			feeConfig: FeeConfig{
				BaseFeeChangeDenominator: 10,
				ElasticityMultiplier:     1.15,
			},
			expectedBaseFeeChangeDenominator: 10,
			expectedElasticityMultiplier:     1.15,
		},
		{
			blockNumber: "20",
			feeConfig: FeeConfig{
				BaseFeeChangeDenominator: 35,
				ElasticityMultiplier:     1.75,
			},
			expectedBaseFeeChangeDenominator: 35,
			expectedElasticityMultiplier:     1.75,
		},
	}
	config := config()

	// Check default values, Withou adding any fee config to chain config
	feeConfig := config.GetFeeConfig(123)
	if feeConfig.BaseFeeChangeDenominator != BaseFeeChangeDenominator {
		t.Errorf("expected base fee change denominator %v, got %v", BaseFeeChangeDenominator, feeConfig.BaseFeeChangeDenominator)
	}
	if feeConfig.ElasticityMultiplier != ElasticityMultiplier {
		t.Errorf("expected elasticity multiplier %v, got %v", ElasticityMultiplier, feeConfig.ElasticityMultiplier)
	}

	// Add all the fee configs
	config.FeeConfigMap = make(map[string]FeeConfig, len(tests))
	for _, test := range tests {
		config.FeeConfigMap[test.blockNumber] = test.feeConfig
	}

	for _, test := range tests {
		number, err := strconv.ParseUint(test.blockNumber, 10, 64)
		if err != nil {
			t.Errorf("error parsing block number: %v", err)
		}
		feeConfig := config.GetFeeConfig(number + 1)
		if feeConfig.BaseFeeChangeDenominator != test.expectedBaseFeeChangeDenominator {
			t.Errorf("expected base fee change denominator %v, got %v", test.expectedBaseFeeChangeDenominator, feeConfig.BaseFeeChangeDenominator)
		}
		if feeConfig.ElasticityMultiplier != test.expectedElasticityMultiplier {
			t.Errorf("expected elasticity multiplier %v, got %v", test.expectedElasticityMultiplier, feeConfig.ElasticityMultiplier)
		}
	}
}
