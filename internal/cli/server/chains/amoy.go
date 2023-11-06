package chains

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
)

var amoyTestnet = &Chain{
	NetworkId: 80002,
	Genesis: &core.Genesis{
		Config: &params.ChainConfig{
			ChainID:             big.NewInt(80002),
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
			LondonBlock:         nil,
			ShanghaiBlock:       nil,
			Bor: &params.BorConfig{
				JaipurBlock:           nil,
				DelhiBlock:            nil,
				ParallelUniverseBlock: nil,
				IndoreBlock:           nil,
				StateSyncConfirmationDelay: map[string]uint64{
					"0": 128,
				},
				Period: map[string]uint64{
					"0": 2,
				},
				ProducerDelay: map[string]uint64{
					"0": 4,
				},
				Sprint: map[string]uint64{
					"0": 16,
				},
				BackupMultiplier: map[string]uint64{
					"0": 2,
				},
				ValidatorContract:     "0x0000000000000000000000000000000000001000",
				StateReceiverContract: "0x0000000000000000000000000000000000001001",
				BurntContract: map[string]string{
					"0": "0x000000000000000000000000000000000000dead",
				},
			},
		},
		Nonce:      0,
		Timestamp:  1558348305,
		GasLimit:   10000000,
		Difficulty: big.NewInt(1),
		Mixhash:    common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Coinbase:   common.HexToAddress("0x0000000000000000000000000000000000000000"),
		Alloc:      readPrealloc("allocs/amoy.json"),
	},
}
