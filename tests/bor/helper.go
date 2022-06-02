package bor

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
)

var (
	// The genesis for tests was generated with following parameters
	extraSeal = 65 // Fixed number of extra-data suffix bytes reserved for signer seal

	// Only this account is a validator for the 0th span
	privKey = "b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291"
	key, _  = crypto.HexToECDSA(privKey)
	addr    = crypto.PubkeyToAddress(key.PublicKey) // 0x71562b71999873DB5b286dF957af199Ec94617F7

	// This account is one the validators for 1st span (0-indexed)
	privKey2 = "9b28f36fbd67381120752d6172ecdcf10e06ab2d9a1367aac00cdcd6ac7855d3"
	key2, _  = crypto.HexToECDSA(privKey2)
	addr2    = crypto.PubkeyToAddress(key2.PublicKey) // 0x9fB29AAc15b9A4B7F17c3385939b007540f4d791

	validatorHeaderBytesLength        = common.AddressLength + 20 // address + power
	sprintSize                 uint64 = 4
	spanSize                   uint64 = 8
)

type BlockGen struct {
	i       int
	parent  *types.Block
	chain   []*types.Block
	header  *types.Header
	statedb *state.StateDB

	gasPool  *core.GasPool
	txs      []*types.Transaction
	receipts []*types.Receipt
	uncles   []*types.Header

	config *params.ChainConfig
	engine consensus.Engine
}
type fakeChainReader struct {
	config *params.ChainConfig
}
type initializeData struct {
	genesis  *core.Genesis
	ethereum *eth.Ethereum
}

func buildEthereumInstance(t *testing.T, db ethdb.Database) *initializeData {
	genesisData, err := ioutil.ReadFile("./testdata/genesis.json")
	if err != nil {
		t.Fatalf("%s", err)
	}
	gen := &core.Genesis{}
	if err := json.Unmarshal(genesisData, gen); err != nil {
		t.Fatalf("%s", err)
	}
	ethConf := &eth.Config{
		Genesis: gen,
	}
	ethConf.Genesis.MustCommit(db)

	ethereum := utils.CreateBorEthereum(ethConf)
	if err != nil {
		t.Fatalf("failed to register Ethereum protocol: %v", err)
	}

	ethConf.Genesis.MustCommit(ethereum.ChainDb())
	return &initializeData{
		genesis:  gen,
		ethereum: ethereum,
	}
}

func insertNewBlock(t *testing.T, chain *core.BlockChain, block *types.Block) {
	if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
		t.Fatalf("%s", err)
	}
}

func buildNextBlock(t *testing.T, _bor *bor.Bor, chain *core.BlockChain, block *types.Block, signer []byte, borConfig *params.BorConfig, gen func(int, *BlockGen)) *types.Block {

	header := block.Header()
	header.Number.Add(header.Number, big.NewInt(1))
	number := header.Number.Uint64()
	header.ParentHash = block.Hash()
	header.Time += bor.CalcProducerDelay(header.Number.Uint64(), 0, borConfig)
	header.Extra = make([]byte, 32+65) // vanity + extraSeal

	currentValidators := []*bor.Validator{bor.NewValidator(addr, 10)}

	isSpanEnd := (number+1)%spanSize == 0
	isSpanStart := number%spanSize == 0
	isSprintEnd := (header.Number.Uint64()+1)%sprintSize == 0
	if isSpanEnd {
		_, heimdallSpan := loadSpanFromFile(t)
		// this is to stash the validator bytes in the header
		currentValidators = heimdallSpan.ValidatorSet.Validators
	} else if isSpanStart {
		header.Difficulty = new(big.Int).SetInt64(3)
	}
	if isSprintEnd {
		sort.Sort(bor.ValidatorsByAddress(currentValidators))
		validatorBytes := make([]byte, len(currentValidators)*validatorHeaderBytesLength)
		header.Extra = make([]byte, 32+len(validatorBytes)+65) // vanity + validatorBytes + extraSeal
		for i, val := range currentValidators {
			copy(validatorBytes[i*validatorHeaderBytesLength:], val.HeaderBytes())
		}
		copy(header.Extra[32:], validatorBytes)
	}

	if chain.Config().IsLondon(header.Number) {
		header.BaseFee = misc.CalcBaseFee(chain.Config(), block.Header())
		if !chain.Config().IsLondon(block.Number()) {
			parentGasLimit := block.GasLimit() * params.ElasticityMultiplier
			header.GasLimit = core.CalcGasLimit(parentGasLimit, parentGasLimit)
		}
	}

	config := chain.Config()
	if signer == nil {
		signer = getSignerKey(header.Number.Uint64())
	}
	blocks := make([]*types.Block, 1)
	genblock := func(i int, parent *types.Block, statedb *state.StateDB) *types.Block {

		b := &BlockGen{i: i, chain: blocks, parent: parent, statedb: statedb, config: config, engine: _bor}
		b.header = header

		// Set the difficulty for clique block. The chain maker doesn't have access
		// to a chain, so the difficulty will be left unset (nil). Set it here to the
		// correct value.
		if b.header.Difficulty == nil {
			if config.TerminalTotalDifficulty == nil {
				// Clique chain
				b.header.Difficulty = big.NewInt(2)
			} else {
				// Post-merge chain
				b.header.Difficulty = big.NewInt(0)
			}
		}
		// Mutate the state and block according to any hard-fork specs
		if daoBlock := config.DAOForkBlock; daoBlock != nil {
			limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
			if b.header.Number.Cmp(daoBlock) >= 0 && b.header.Number.Cmp(limit) < 0 {
				if config.DAOForkSupport {
					b.header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
				}
			}
		}
		if config.DAOForkSupport && config.DAOForkBlock != nil && config.DAOForkBlock.Cmp(b.header.Number) == 0 {
			misc.ApplyDAOHardFork(statedb)
		}
		// Execute any user modifications to the block
		if gen != nil {
			gen(i, b)
		}
		if b.engine != nil {
			blockR, err := _bor.FinalizeAndAssemble(chain, b.header, statedb, b.txs, b.uncles, b.receipts)
			if err != nil {
				t.Fatalf("%s", err)
			}

			header2 := blockR.Header()

			// Write state changes to db
			root, err := statedb.Commit(config.IsEIP158(b.header.Number))
			if err != nil {
				panic(fmt.Sprintf("state write error: %v", err))
			}
			if err := statedb.Database().TrieDB().Commit(root, false, nil); err != nil {
				panic(fmt.Sprintf("trie write error: %v", err))
			}

			sign(t, header2, signer, borConfig)

			return types.NewBlockWithHeaderAndBlock(header2, blockR)

		}
		return nil
	}

	// statedb, err := state.New(block.Root(), state.NewDatabase(*chain.DB()), nil)
	// if err != nil {
	// 	panic(err)
	// }

	statedb, err := state.New(block.Root(), chain.StateCache(), chain.Snapshots())
	if err != nil {
		panic(err)
	}

	// state, err := chain.State()
	// if err != nil {
	// 	t.Fatalf("%s", err)
	// }
	blockR := genblock(0, block, statedb)

	return blockR

}

func sign(t *testing.T, header *types.Header, signer []byte, c *params.BorConfig) {
	sig, err := secp256k1.Sign(crypto.Keccak256(bor.BorRLP(header, c)), signer)
	if err != nil {
		t.Fatalf("%s", err)
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
}

func stateSyncEventsPayload(t *testing.T) *bor.ResponseWithHeight {
	stateData, err := ioutil.ReadFile("./testdata/states.json")
	if err != nil {
		t.Fatalf("%s", err)
	}
	res := &bor.ResponseWithHeight{}
	if err := json.Unmarshal(stateData, res); err != nil {
		t.Fatalf("%s", err)
	}
	return res
}

func loadSpanFromFile(t *testing.T) (*bor.ResponseWithHeight, *bor.HeimdallSpan) {
	spanData, err := ioutil.ReadFile("./testdata/span.json")
	if err != nil {
		t.Fatalf("%s", err)
	}
	res := &bor.ResponseWithHeight{}
	if err := json.Unmarshal(spanData, res); err != nil {
		t.Fatalf("%s", err)
	}

	heimdallSpan := &bor.HeimdallSpan{}
	if err := json.Unmarshal(res.Result, heimdallSpan); err != nil {
		t.Fatalf("%s", err)
	}
	return res, heimdallSpan
}

func getSignerKey(number uint64) []byte {
	signerKey := privKey
	isSpanStart := number%spanSize == 0
	if isSpanStart {
		// validator set in the new span has changed
		signerKey = privKey2
	}
	_key, _ := hex.DecodeString(signerKey)
	return _key
}
