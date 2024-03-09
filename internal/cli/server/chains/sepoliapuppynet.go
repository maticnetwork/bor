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
		"enode://cda1b3e8bc2343c8c00b62a91b89dff63d40327f856a6d95602338f2774f85d78877eaa5c42782c5dfd3798cb0a6724ac8a7fbb9b639972c3b1e178ea4f05fa2@3.254.67.220:30303",
		"enode://2eeab8780edcd72f650a94fd9232fa7ca08d016724fb92cf3a44049532bde2afbb837712b8b63526173e788da89df6e9620818343f32e0c72e3a1ba986ca12bd@35.183.117.7:30303",
		"enode://f357de115c92d32e41a889636710aa4c25c9d1de5d7e9eb47608f20eeec0b7ab80c21a363c4592eb79f95f1c6595385fe14df3100b3a114ea88d91af03e97c01@3.227.32.22:30303",
		"enode://c700dcc8dc5f39e5f8a34c79327596f6124fe67b5a6cfcd14fa4d2f0e0161a831677711c0cdf418c2e236defe02948bce307ff12ea5004b737c1ab35a6523cd2@44.219.255.49:30303",
	},
}
