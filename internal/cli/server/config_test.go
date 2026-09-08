package server

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/params"
)

func TestConfigDefault(t *testing.T) {
	// the default config should work out of the box
	config := DefaultConfig()
	assert.NoError(t, config.loadChain())

	_, err := config.buildNode()
	assert.NoError(t, err)

	ethConfig, err := config.buildEth(nil, nil)
	assert.NoError(t, err)
	assertBorDefaultGasPrice(t, ethConfig)
}

// assertBorDefaultGasPrice asserts the bor default gas price is set correctly.
func assertBorDefaultGasPrice(t *testing.T, ethConfig *ethconfig.Config) {
	assert.NotNil(t, ethConfig)
	assert.Equal(t, ethConfig.Miner.GasPrice, big.NewInt(params.BorDefaultMinerGasPrice))
}

func TestConfigMerge(t *testing.T) {
	c0 := &Config{
		Chain:    "0",
		Snapshot: true,
		RequiredBlocks: map[string]string{
			"a": "b",
		},
		TxPool: &TxPoolConfig{
			LifeTime: 5 * time.Second,
		},
		P2P: &P2PConfig{
			Discovery: &P2PDiscovery{
				StaticNodes: []string{
					"a",
				},
			},
		},
	}
	c1 := &Config{
		Chain: "1",
		RequiredBlocks: map[string]string{
			"b": "c",
		},
		P2P: &P2PConfig{
			MaxPeers: 10,
			Discovery: &P2PDiscovery{
				StaticNodes: []string{
					"b",
				},
			},
		},
	}

	expected := &Config{
		Chain:    "1",
		Snapshot: false,
		RequiredBlocks: map[string]string{
			"a": "b",
			"b": "c",
		},
		P2P: &P2PConfig{
			MaxPeers: 10,
			Discovery: &P2PDiscovery{
				StaticNodes: []string{
					"b",
				},
			},
		},
	}

	assert.NoError(t, c0.Merge(c1))
	assert.Equal(t, c0, expected)
}

func TestDefaultDatatypeOverride(t *testing.T) {
	t.Parallel()

	// This test is specific to `maxpeers` flag (for now) to check
	// if default datatype value (0 in case of uint64) is overridden.
	c0 := &Config{
		P2P: &P2PConfig{
			MaxPeers: 30,
		},
	}

	c1 := &Config{
		P2P: &P2PConfig{
			MaxPeers: 0,
		},
	}

	expected := &Config{
		P2P: &P2PConfig{
			MaxPeers: 0,
		},
	}

	assert.NoError(t, c0.Merge(c1))
	assert.Equal(t, c0, expected)
}

var dummyEnodeAddr = "enode://0cb82b395094ee4a2915e9714894627de9ed8498fb881cec6db7c65e8b9a5bd7f2f25cc84e71e89d0947e51c76e85d0847de848c7782b13c0255247a6758178c@44.232.55.71:30303"

func TestConfigBootnodesDefault(t *testing.T) {
	t.Run("EmptyBootnodes", func(t *testing.T) {
		// if no bootnodes are specific, we use the ones from the genesis chain
		config := DefaultConfig()
		assert.NoError(t, config.loadChain())

		cfg, err := config.buildNode()
		assert.NoError(t, err)
		assert.NotEmpty(t, cfg.P2P.BootstrapNodes)
	})
	t.Run("NotEmptyBootnodes", func(t *testing.T) {
		// if bootnodes specific, DO NOT load the genesis bootnodes
		config := DefaultConfig()
		config.P2P.Discovery.Bootnodes = []string{dummyEnodeAddr}

		cfg, err := config.buildNode()
		assert.NoError(t, err)
		assert.Len(t, cfg.P2P.BootstrapNodes, 1)
	})
}

func TestConfigBuildNodeBulkSidecar(t *testing.T) {
	config := DefaultConfig()
	config.P2P.EnableBulkSidecar = true
	config.P2P.BulkPort = 30304
	config.P2P.Bind = "0.0.0.0"

	cfg, err := config.buildNode()
	assert.NoError(t, err)
	assert.True(t, cfg.P2P.EnableBulkSidecar)
	assert.Equal(t, "0.0.0.0:30304", cfg.P2P.BulkListenAddr)
}

func TestMakePasswordListFromFile(t *testing.T) {
	t.Parallel()

	t.Run("ReadPasswordFile", func(t *testing.T) {
		t.Parallel()

		result, _ := MakePasswordListFromFile("./testdata/password.txt")
		assert.Equal(t, []string{"test1", "test2"}, result)
	})
}

func TestConfigStateScheme(t *testing.T) {
	config := DefaultConfig()
	config.StateScheme = "path"
	config.GcMode = "archive"

	assert.NoError(t, config.loadChain())

	_, err := config.buildNode()
	assert.NoError(t, err)

	_, err = config.buildEth(nil, nil)
	assert.NoError(t, err)
}

func TestSealerTargetGasPercentageConfig(t *testing.T) {
	t.Run("Valid custom value sets BorConfig", func(t *testing.T) {
		testCases := []uint64{1, 50, 65, 100}

		for _, targetVal := range testCases {
			config := DefaultConfig()
			config.Sealer.TargetGasPercentage = targetVal

			assert.NoError(t, config.loadChain())

			_, err := config.buildNode()
			assert.NoError(t, err)

			ethConfig, err := config.buildEth(nil, nil)
			assert.NoError(t, err)

			assert.NotNil(t, ethConfig.Genesis.Config.Bor.TargetGasPercentage)
			assert.Equal(t, targetVal, *ethConfig.Genesis.Config.Bor.TargetGasPercentage)
		}
	})

	t.Run("Invalid value >100 returns error", func(t *testing.T) {
		testCases := []uint64{101, 200, 1000}

		for _, invalidVal := range testCases {
			config := DefaultConfig()
			config.Sealer.TargetGasPercentage = invalidVal

			assert.NoError(t, config.loadChain())

			_, err := config.buildNode()
			assert.NoError(t, err)

			_, err = config.buildEth(nil, nil)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "miner.targetGasPercentage must be between 1-100")
		}
	})
}

func TestSealerBaseFeeChangeDenominatorConfig(t *testing.T) {
	t.Run("Valid custom value sets BorConfig", func(t *testing.T) {
		testCases := []uint64{1, 8, 16, 32, 64, 128}

		for _, denomVal := range testCases {
			config := DefaultConfig()
			config.Sealer.BaseFeeChangeDenominator = denomVal

			assert.NoError(t, config.loadChain())

			_, err := config.buildNode()
			assert.NoError(t, err)

			ethConfig, err := config.buildEth(nil, nil)
			assert.NoError(t, err)

			assert.NotNil(t, ethConfig.Genesis.Config.Bor.BaseFeeChangeDenominator)
			assert.Equal(t, denomVal, *ethConfig.Genesis.Config.Bor.BaseFeeChangeDenominator)
		}
	})
}

func TestSealerBothGasParametersConfig(t *testing.T) {
	t.Run("Both parameters set together", func(t *testing.T) {
		config := DefaultConfig()
		config.Sealer.TargetGasPercentage = 75
		config.Sealer.BaseFeeChangeDenominator = 128

		assert.NoError(t, config.loadChain())

		_, err := config.buildNode()
		assert.NoError(t, err)

		ethConfig, err := config.buildEth(nil, nil)
		assert.NoError(t, err)

		assert.NotNil(t, ethConfig.Genesis.Config.Bor.TargetGasPercentage)
		assert.Equal(t, uint64(75), *ethConfig.Genesis.Config.Bor.TargetGasPercentage)

		assert.NotNil(t, ethConfig.Genesis.Config.Bor.BaseFeeChangeDenominator)
		assert.Equal(t, uint64(128), *ethConfig.Genesis.Config.Bor.BaseFeeChangeDenominator)
	})

	t.Run("Only TargetGasPercentage set", func(t *testing.T) {
		config := DefaultConfig()
		config.Sealer.TargetGasPercentage = 80
		config.Sealer.BaseFeeChangeDenominator = 0

		assert.NoError(t, config.loadChain())

		_, err := config.buildNode()
		assert.NoError(t, err)

		ethConfig, err := config.buildEth(nil, nil)
		assert.NoError(t, err)

		assert.NotNil(t, ethConfig.Genesis.Config.Bor.TargetGasPercentage)
		assert.Equal(t, uint64(80), *ethConfig.Genesis.Config.Bor.TargetGasPercentage)
	})

	t.Run("Only BaseFeeChangeDenominator set", func(t *testing.T) {
		config := DefaultConfig()
		config.Sealer.TargetGasPercentage = 0
		config.Sealer.BaseFeeChangeDenominator = 256

		assert.NoError(t, config.loadChain())

		_, err := config.buildNode()
		assert.NoError(t, err)

		ethConfig, err := config.buildEth(nil, nil)
		assert.NoError(t, err)

		assert.NotNil(t, ethConfig.Genesis.Config.Bor.BaseFeeChangeDenominator)
		assert.Equal(t, uint64(256), *ethConfig.Genesis.Config.Bor.BaseFeeChangeDenominator)
	})

	t.Run("Invalid TargetGasPercentage with valid BaseFeeChangeDenominator", func(t *testing.T) {
		config := DefaultConfig()
		config.Sealer.TargetGasPercentage = 150
		config.Sealer.BaseFeeChangeDenominator = 64

		assert.NoError(t, config.loadChain())

		_, err := config.buildNode()
		assert.NoError(t, err)

		_, err = config.buildEth(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasPercentage must be between 1-100")
	})
}

// TestParseByteSize tests the parseByteSize helper function for parsing
// human-readable byte size strings (e.g., "500KB", "5MB", "1GB").
func TestParseByteSize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		// Valid inputs with different suffixes
		{"bytes", "1024B", 1024, false},
		{"kilobytes", "500KB", 500 * 1024, false},
		{"megabytes", "5MB", 5 * 1024 * 1024, false},
		{"gigabytes", "1GB", 1 * 1024 * 1024 * 1024, false},

		// Case insensitivity
		{"lowercase kb", "500kb", 500 * 1024, false},
		{"lowercase mb", "5mb", 5 * 1024 * 1024, false},
		{"mixed case", "5Mb", 5 * 1024 * 1024, false},

		// No suffix (assumes bytes)
		{"no suffix", "1024", 1024, false},

		// Zero and empty
		{"zero string", "0", 0, false},
		{"empty string", "", 0, false},

		// Whitespace handling
		{"leading space", "  500KB", 500 * 1024, false},
		{"trailing space", "500KB  ", 500 * 1024, false},
		{"space before suffix", "500 KB", 500 * 1024, false},

		// Invalid inputs
		{"invalid suffix", "500XB", 0, true},
		{"non-numeric", "abcMB", 0, true},
		{"float", "5.5MB", 0, true},

		// Note: negative values are technically parsed by ParseInt,
		// but produce negative results which are invalid for byte sizes.
		// The calling code should validate the result is non-negative.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseByteSize(tt.input)
			if tt.wantErr {
				assert.Error(t, err, "expected error for input: %s", tt.input)
			} else {
				assert.NoError(t, err, "unexpected error for input: %s", tt.input)
				assert.Equal(t, tt.expected, result, "unexpected result for input: %s", tt.input)
			}
		})
	}
}

// TestPreloadRateLimitConfig tests the preload rate limit configuration parsing.
func TestPreloadRateLimitConfig(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
	}{
		{"empty string defaults to 1MB/s", "", 1024 * 1024},
		{"explicit 1MB", "1MB", 1024 * 1024},
		{"unlimited (0)", "0", 0},
		{"invalid falls back to 1MB/s", "invalid", 1024 * 1024},
		{"500KB", "500KB", 500 * 1024},
		{"2MB", "2MB", 2 * 1024 * 1024},
		{"lowercase 100kb", "100kb", 100 * 1024},
		{"lowercase 10mb", "10mb", 10 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := DefaultConfig()
			config.Cache.PreloadRateLimit = tt.input

			assert.NoError(t, config.loadChain())

			ethConfig, err := config.buildEth(nil, nil)
			assert.NoError(t, err)

			assert.Equal(t, tt.expected, ethConfig.PreloadRateLimit, "input: %s", tt.input)
		})
	}
}

// TestSealerDynamicTargetGasConfig tests the EnableDynamicTargetGas feature validation
// and BorConfig wiring introduced in buildEth.
func TestSealerDynamicTargetGasConfig(t *testing.T) {
	// validDynamicTargetGasConfig returns a baseline config with all required fields
	// set to valid values for EnableDynamicTargetGas.
	validDynamicTargetGasConfig := func() *Config {
		config := DefaultConfig()
		config.Sealer.EnableDynamicTargetGas = true
		config.Sealer.TargetGasMinPercentage = 50    // 50% floor
		config.Sealer.TargetGasMaxPercentage = 80    // 80% ceiling
		config.Sealer.TargetBaseFee = 25_000_000_000 // 25 gwei
		config.Sealer.BaseFeeBuffer = 10_000_000_000 // 10 gwei buffer
		return config
	}

	buildConfig := func(t *testing.T, config *Config) (*ethconfig.Config, error) {
		t.Helper()
		assert.NoError(t, config.loadChain())
		_, err := config.buildNode()
		assert.NoError(t, err)
		return config.buildEth(nil, nil)
	}

	t.Run("MutualExclusivity_ReturnsError", func(t *testing.T) {
		config := DefaultConfig()
		config.Sealer.EnableDynamicGasLimit = true
		config.Sealer.EnableDynamicTargetGas = true

		assert.NoError(t, config.loadChain())
		_, err := config.buildNode()
		assert.NoError(t, err)
		_, err = config.buildEth(nil, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("TargetGasMinPercentage_Zero_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetGasMinPercentage = 0

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasMinPercentage (0) must be between 1-100")
	})

	t.Run("TargetGasMinPercentage_OutOfRange_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetGasMinPercentage = 101

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasMinPercentage (101) must be between 1-100")
	})

	t.Run("TargetGasMaxPercentage_Zero_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetGasMaxPercentage = 0

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasMaxPercentage (0) must be between 1-100")
	})

	t.Run("TargetGasMaxPercentage_OutOfRange_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetGasMaxPercentage = 101

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasMaxPercentage (101) must be between 1-100")
	})

	t.Run("TargetGasMinPercentage_EqualTo_TargetGasMaxPercentage_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetGasMinPercentage = 65
		config.Sealer.TargetGasMaxPercentage = 65

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasMinPercentage (65) must be less than miner.targetGasMaxPercentage (65)")
	})

	t.Run("TargetGasMinPercentage_GreaterThan_TargetGasMaxPercentage_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetGasMinPercentage = 80
		config.Sealer.TargetGasMaxPercentage = 50

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasMinPercentage (80) must be less than miner.targetGasMaxPercentage (50)")
	})

	t.Run("TargetGasPercentage_OutsideDynamicRange_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig() // min=50, max=80
		config.Sealer.TargetGasPercentage = 90  // outside [50, 80]

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.target-gas-percentage (90) must be between")
	})

	t.Run("TargetGasPercentage_WithinDynamicRange_NoError", func(t *testing.T) {
		config := validDynamicTargetGasConfig() // min=50, max=80
		config.Sealer.TargetGasPercentage = 65  // within [50, 80]

		_, err := buildConfig(t, config)
		assert.NoError(t, err)
	})

	t.Run("ImplicitDefault65_OutsideDynamicRange_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		// default 65% falls below min=70
		config.Sealer.TargetGasMinPercentage = 70
		config.Sealer.TargetGasMaxPercentage = 90
		// TargetGasPercentage left at 0 (not set) → implicit default 65 < 70

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "default target gas percentage (65) falls outside")
	})

	t.Run("TargetBaseFee_Zero_ReturnsError", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		config.Sealer.TargetBaseFee = 0
		config.Sealer.BaseFeeBuffer = 0

		_, err := buildConfig(t, config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetBaseFee must be greater than 0 when dynamic target gas is enabled")
	})

	t.Run("BaseFeeBuffer_GTE_TargetBaseFee_WarnsOnly", func(t *testing.T) {
		config := validDynamicTargetGasConfig()
		// Buffer equal to TargetBaseFee → lower bound clamps to 0, only a warning emitted
		config.Sealer.BaseFeeBuffer = config.Sealer.TargetBaseFee

		_, err := buildConfig(t, config)
		assert.NoError(t, err)
	})

	t.Run("ValidConfig_WiresBorConfig", func(t *testing.T) {
		config := validDynamicTargetGasConfig()

		ethConfig, err := buildConfig(t, config)
		assert.NoError(t, err)

		bor := ethConfig.Genesis.Config.Bor
		assert.NotNil(t, bor.EnableDynamicTargetGas)
		assert.True(t, *bor.EnableDynamicTargetGas)
		assert.NotNil(t, bor.TargetGasMinPercentage)
		assert.Equal(t, uint64(50), *bor.TargetGasMinPercentage)
		assert.NotNil(t, bor.TargetGasMaxPercentage)
		assert.Equal(t, uint64(80), *bor.TargetGasMaxPercentage)
		assert.NotNil(t, bor.TargetBaseFee)
		assert.Equal(t, uint64(25_000_000_000), *bor.TargetBaseFee)
		assert.NotNil(t, bor.BaseFeeBuffer)
		assert.Equal(t, uint64(10_000_000_000), *bor.BaseFeeBuffer)
	})

	t.Run("BothDisabled_NoError", func(t *testing.T) {
		config := DefaultConfig()
		// Both EnableDynamicGasLimit and EnableDynamicTargetGas default to false

		_, err := buildConfig(t, config)
		assert.NoError(t, err)
	})
}

// TestDeveloperModeGasParameters tests the developer mode specific code path
// for setting TargetGasPercentage and BaseFeeChangeDenominator (lines 1293-1304 in config.go).
// The default config uses mainnet which has Bor config, so these tests actually execute lines 1293-1304.
func TestDeveloperModeGasParameters(t *testing.T) {
	t.Run("Scenario 1: Both TargetGasPercentage > 0 AND BaseFeeChangeDenominator > 0", func(t *testing.T) {
		// Tests lines 1295-1300 AND 1301-1303 both execute
		config := DefaultConfig()
		config.Developer.Enabled = true
		config.Developer.Period = 0
		config.Sealer.TargetGasPercentage = 75       // > 0, so line 1295 true
		config.Sealer.BaseFeeChangeDenominator = 128 // > 0, so line 1301 true

		server, err := CreateMockServer(config)
		assert.NoError(t, err)
		defer CloseMockServer(server)

		// Mainnet config has Bor, so lines 1295-1303 execute and set both values
		chainConfig := server.backend.BlockChain().GetChainConfig()
		assert.NotNil(t, chainConfig.Bor)
		assert.NotNil(t, chainConfig.Bor.TargetGasPercentage)
		assert.Equal(t, uint64(75), *chainConfig.Bor.TargetGasPercentage)
		assert.NotNil(t, chainConfig.Bor.BaseFeeChangeDenominator)
		assert.Equal(t, uint64(128), *chainConfig.Bor.BaseFeeChangeDenominator)
	})

	t.Run("Scenario 2: Only TargetGasPercentage > 0, BaseFeeChangeDenominator == 0", func(t *testing.T) {
		// Tests line 1295 true (executes), line 1301 false (skips)
		config := DefaultConfig()
		config.Developer.Enabled = true
		config.Developer.Period = 0
		config.Sealer.TargetGasPercentage = 80     // > 0, so line 1295 true
		config.Sealer.BaseFeeChangeDenominator = 0 // == 0, so line 1301 false

		server, err := CreateMockServer(config)
		assert.NoError(t, err)
		defer CloseMockServer(server)

		chainConfig := server.backend.BlockChain().GetChainConfig()
		assert.NotNil(t, chainConfig.Bor)
		// Line 1299 sets TargetGasPercentage
		assert.NotNil(t, chainConfig.Bor.TargetGasPercentage)
		assert.Equal(t, uint64(80), *chainConfig.Bor.TargetGasPercentage)
		// Line 1301 condition is false, so BaseFeeChangeDenominator uses default from chain
		// (not nil, but not set by our config)
	})

	t.Run("Scenario 3: TargetGasPercentage == 0, only BaseFeeChangeDenominator > 0", func(t *testing.T) {
		// Tests line 1295 false (skips), line 1301 true (executes)
		config := DefaultConfig()
		config.Developer.Enabled = true
		config.Developer.Period = 0
		config.Sealer.TargetGasPercentage = 0       // == 0, so line 1295 false
		config.Sealer.BaseFeeChangeDenominator = 64 // > 0, so line 1301 true

		server, err := CreateMockServer(config)
		assert.NoError(t, err)
		defer CloseMockServer(server)

		chainConfig := server.backend.BlockChain().GetChainConfig()
		assert.NotNil(t, chainConfig.Bor)
		// Line 1295 condition is false, so TargetGasPercentage uses default from chain
		// Line 1302 sets BaseFeeChangeDenominator
		assert.NotNil(t, chainConfig.Bor.BaseFeeChangeDenominator)
		assert.Equal(t, uint64(64), *chainConfig.Bor.BaseFeeChangeDenominator)
	})

	t.Run("Validation: TargetGasPercentage > 100 fails", func(t *testing.T) {
		// Tests line 1296-1298 validation executes and returns error
		config := DefaultConfig()
		config.Developer.Enabled = true
		config.Developer.Period = 0
		config.Sealer.TargetGasPercentage = 150 // Invalid value

		// With mainnet Bor config, validation at line 1296-1298 executes and returns error
		_, err := CreateMockServer(config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "miner.targetGasPercentage must be between 1-100")
	})
}
