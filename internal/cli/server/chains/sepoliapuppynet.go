package chains

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
)

var sepoliapuppynetTestnet = &Chain{
	Hash:      common.HexToHash("0x5be2e21ae365a45dc256f34acb11953a5dca432730d325f0d084775be8dfcd0f"),
	NetworkId: 157,
	Genesis: &core.Genesis{
		Config: &params.ChainConfig{
			ChainID:             big.NewInt(157),
			HomesteadBlock:      big.NewInt(0),
			DAOForkBlock:        nil,
			DAOForkSupport:      true,
			EIP150Block:         big.NewInt(0),
			EIP155Block:         big.NewInt(0),
			EIP158Block:         big.NewInt(0),
			ByzantiumBlock:      big.NewInt(0),
			ConstantinopleBlock: big.NewInt(0),
			PetersburgBlock:     big.NewInt(0),
			IstanbulBlock:       big.NewInt(0),
			MuirGlacierBlock:    big.NewInt(0),
			BerlinBlock:         big.NewInt(0),
			LondonBlock:         big.NewInt(0),
			Bor: &params.BorConfig{
				JaipurBlock: big.NewInt(0),
				Period: map[string]uint64{
					"0": 5,
				},
				ProducerDelay: map[string]uint64{
					"0": 6,
				},
				Sprint: map[string]uint64{
					"0": 64,
				},
				BackupMultiplier: map[string]uint64{
					"0": 5,
				},
				ValidatorContract:     "0x0000000000000000000000000000000000001000",
				StateReceiverContract: "0x0000000000000000000000000000000000001001",
				BurntContract: map[string]string{
					"0":      "0x8B066912dCDA9c9001A84b28F1aBD06e9E19114B",
					"586000": "0x0335c046f8317C0FAa8f76C77D5d1Da98a976181",
				},
				BlockAlloc: map[string]interface{}{},
			},
		},
		Nonce:      0,
		Timestamp:  1558348305,
		GasLimit:   10000000,
		Difficulty: big.NewInt(1),
		Mixhash:    common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Coinbase:   common.HexToAddress("0x0000000000000000000000000000000000000000"),
		Alloc:      readPrealloc("allocs/sepoliapuppynet.json"),
	},
	Bootnodes: []string{
		// "enode://320553cda00dfc003f499a3ce9598029f364fbb3ed1222fdc20a94d97dcc4d8ba0cd0bfa996579dcc6d17a534741fb0a5da303a90579431259150de66b597251@54.147.31.250:30303",
		// "enode://f0f48a8781629f95ff02606081e6e43e4aebd503f3d07fc931fad7dd5ca1ba52bd849a6f6c3be0e375cf13c9ae04d859c4a9ae3546dc8ed4f10aa5dbb47d4998@34.226.134.117:30303",
	},
}
