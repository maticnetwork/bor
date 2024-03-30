package chains

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
)

var shibarium = &Chain{
	Hash:      common.HexToHash("0x80c9851317f242906f0d5d42038029fbdbe7df64e700bdc8b8eaabcc349969f4"),
	NetworkId: 109,
	Genesis: &core.Genesis{
		Config: &params.ChainConfig{
			ChainID:             big.NewInt(109),
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
				OverrideStateSyncRecords: map[string]int{
					"189888": 0,
					"189952": 0,
					"190016": 0,
					"190080": 0,
					"190144": 0,
					"190208": 0,
				},
				BurntContract: map[string]string{
					"0":       "0x000000000000000000000000000000000000dead",
					"1962000": "0xc7D0445ac2947760b3dD388B8586Adf079972Bf3",
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
		Alloc:      readPrealloc("allocs/shibarium.json"),
	},
	Bootnodes: []string{
		"enode://f5fdf04f2f7dfdd8522fcd6418b61cd8307b3734348b8af531d0d3fbe3d047ecc00e071e8fc48e4668a345b1f0c4cf7132f0967b592a3d87a96ddc104b8744f7@52.6.145.99:30303",
		"enode://e000b17168e0b586340da21d2a96129db9e4ecfa0aac12e8ec48a90fb049c165a190e4fce543b2ad43f2968312f3ce2b6820b1fac45b17273d77bc6de671b27e@52.6.79.52:30303",
		"enode://23b746e230b70a0ec749adb5947d3993dcf2264f7ecfa408f439dadc6a5d1811e69e2733f0f4b7c8fb47adf348906575c9c45c3c244058bc3d3906bcddb6584e@54.237.88.44:30303",
		"enode://ce63f1ed651a31b6acd8dfbd4488229f16e0d787cdc1fe18e2d5dcf9323276912453afe33880b59e8673895537b06efe53f6cac8008efbe5e6d00a16148ffd0b@75.101.231.169:30303",
		"enode://10778d63b6bc8f2b9d0f228b8047ef81cc56dd19f414d334b10cb7c19bca526ed0d2c1f20ac650f13aea491149889a74fc0ef4768cee18b6abfb97b5afe00c7e@50.16.175.79:30303",
	},
}
