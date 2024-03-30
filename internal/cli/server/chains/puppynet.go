package chains

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
)

var puppynet = &Chain{
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
		Alloc:      readPrealloc("allocs/puppynet.json"),
	},
	Bootnodes: []string{
		"enode://30da2a73104d66f4f8a8af2bf73e1574c703df277de79fac909743cccfab8f93bcf0d4149bdedd26221400b232da52ff6a5cb22146241f7a62b70267048a8c52@3.84.194.166:30303",
		"enode://88667acbaa709d6f1146043ecd7df6c0a124849db56496e9587d3d80d87b3c14759c10e6c37f689be6a04cda7e323805d33b0b68d48f97faf7682691f8bd371a@18.143.185.174:30303",
		"enode://2eeab8780edcd72f650a94fd9232fa7ca08d016724fb92cf3a44049532bde2afbb837712b8b63526173e788da89df6e9620818343f32e0c72e3a1ba986ca12bd@35.183.117.7:30303",
		"enode://0091347d3850d795b8a8baa3cbe75ee7b2190f84e9ff3191af1b4ad019aa8e2c7d3fc131a6010e0e13d51ef85daf98b8589b530ce499a05c032abeb6a861faf0@3.254.67.220:30303",
	},
}
