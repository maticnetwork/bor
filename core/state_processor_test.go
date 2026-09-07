// Copyright 2020 The go-ethereum Authors
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

package core

import (
	"crypto/ecdsa"
	"math"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/keccak"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

// TestStateProcessorErrors tests the output from the 'core' errors
// as defined in core/error.go. These errors are generated when the
// blockchain imports bad blocks, meaning blocks which have valid headers but
// contain invalid transactions
func TestStateProcessorErrors(t *testing.T) {
	var (
		config  = params.MergedTestChainConfig
		signer  = types.LatestSigner(config)
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("0202020202020202020202020202020202020202020202020202002020202020")
	)
	var makeTx = func(key *ecdsa.PrivateKey, nonce uint64, to common.Address, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *types.Transaction {
		tx, _ := types.SignTx(types.NewTransaction(nonce, to, amount, gasLimit, gasPrice, data), signer, key)
		return tx
	}
	var mkDynamicTx = func(nonce uint64, to common.Address, gasLimit uint64, gasTipCap, gasFeeCap *big.Int) *types.Transaction {
		tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			Nonce:     nonce,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			Gas:       gasLimit,
			To:        &to,
			Value:     big.NewInt(0),
		}), signer, key1)
		return tx
	}
	var mkDynamicCreationTx = func(nonce uint64, gasLimit uint64, gasTipCap, gasFeeCap *big.Int, data []byte) *types.Transaction {
		tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			Nonce:     nonce,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			Gas:       gasLimit,
			Value:     big.NewInt(0),
			Data:      data,
		}), signer, key1)
		return tx
	}
	var mkBlobTx = func(nonce uint64, to common.Address, gasLimit uint64, gasTipCap, gasFeeCap, blobGasFeeCap *big.Int, hashes []common.Hash) *types.Transaction {
		tx, err := types.SignTx(types.NewTx(&types.BlobTx{
			Nonce:      nonce,
			GasTipCap:  uint256.MustFromBig(gasTipCap),
			GasFeeCap:  uint256.MustFromBig(gasFeeCap),
			Gas:        gasLimit,
			To:         to,
			BlobHashes: hashes,
			BlobFeeCap: uint256.MustFromBig(blobGasFeeCap),
			Value:      new(uint256.Int),
		}), signer, key1)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	var mkSetCodeTx = func(nonce uint64, to common.Address, gasLimit uint64, gasTipCap, gasFeeCap *big.Int, authlist []types.SetCodeAuthorization) *types.Transaction {
		tx, err := types.SignTx(types.NewTx(&types.SetCodeTx{
			Nonce:     nonce,
			GasTipCap: uint256.MustFromBig(gasTipCap),
			GasFeeCap: uint256.MustFromBig(gasFeeCap),
			Gas:       gasLimit,
			To:        to,
			Value:     new(uint256.Int),
			AuthList:  authlist,
		}), signer, key1)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	{ // Tests against a 'recent' chain definition
		var (
			db    = rawdb.NewMemoryDatabase()
			gspec = &Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					common.HexToAddress("0x71562b71999873DB5b286dF957af199Ec94617F7"): types.Account{
						Balance: big.NewInt(1000000000000000000), // 1 ether
						Nonce:   0,
					},
					common.HexToAddress("0xfd0810DD14796680f72adf1a371963d0745BCc64"): types.Account{
						Balance: big.NewInt(1000000000000000000), // 1 ether
						Nonce:   math.MaxUint64,
					},
				},
			}
			blockchain, _  = NewBlockChain(db, gspec, beacon.New(ethash.NewFaker()), nil)
			tooBigInitCode = [params.MaxInitCodeSize + 1]byte{}
		)

		defer blockchain.Stop()
		bigNumber := new(big.Int).SetBytes(common.MaxHash.Bytes())
		tooBigNumber := new(big.Int).Set(bigNumber)
		tooBigNumber.Add(tooBigNumber, common.Big1)
		gasLimit := blockchain.CurrentHeader().GasLimit
		for i, tt := range []struct {
			txs  []*types.Transaction
			want string
		}{
			{ // ErrNonceTooLow
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(0), params.TxGas, big.NewInt(params.InitialBaseFee), nil),
					makeTx(key1, 0, common.Address{}, big.NewInt(0), params.TxGas, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 1 [0x596a31a467c607dda37b74b47cb9054c12a74e556b94920cda4fd056da7654eb]: nonce too low: address 0x71562b71999873DB5b286dF957af199Ec94617F7, tx: 0 state: 1",
			},
			{ // ErrNonceTooHigh
				txs: []*types.Transaction{
					makeTx(key1, 100, common.Address{}, big.NewInt(0), params.TxGas, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x1d27d485c0501642d4775c9fc4390ddb6b9c4caa941b6c75ed5a6dcd9f48680d]: nonce too high: address 0x71562b71999873DB5b286dF957af199Ec94617F7, tx: 100 state: 0",
			},
			{ // ErrNonceMax
				txs: []*types.Transaction{
					makeTx(key2, math.MaxUint64, common.Address{}, big.NewInt(0), params.TxGas, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0xc9fb065506701d9412510313c7375e62961206adb446e60e41ac168f86c51a49]: nonce has max value: address 0xfd0810DD14796680f72adf1a371963d0745BCc64, nonce: 18446744073709551615",
			},
			{ // ErrGasLimitReached
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(0), gasLimit+1, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x58ebc6e5ecfc7d220f0d137b97d519b0bee6d748ff637891e2f97eb7e8adc7a2]: gas limit reached",
			},
			{ // ErrInsufficientFundsForTransfer
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(1000000000000000000), params.TxGas, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x9ab13d4ea9f1067c0a66900f4e46f0f59366370a147d95458a32f9d847c395d4]: insufficient funds for gas * price + value: address 0x71562b71999873DB5b286dF957af199Ec94617F7 have 1000000000000000000 want 1000021000000000000",
			},
			{ // ErrInsufficientFunds
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(0), params.TxGas, big.NewInt(900000000000000000), nil),
				},
				want: "could not apply tx 0 [0x3002bf789236e7077312cbc5d7b025b3a8d94c9f1e439fdeea2c41c7919ecd87]: insufficient funds for gas * price + value: address 0x71562b71999873DB5b286dF957af199Ec94617F7 have 1000000000000000000 want 18900000000000000000000",
			},
			// ErrGasUintOverflow
			// One missing 'core' error is ErrGasUintOverflow: "gas uint64 overflow",
			// In order to trigger that one, we'd have to allocate a _huge_ chunk of data, such that the
			// multiplication len(data) +gas_per_byte overflows uint64. Not testable at the moment
			{ // ErrIntrinsicGas
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(0), params.TxGas-1000, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x3743f2b73a60ccf9ec6417c12a42c687e876fcc1159286b38f1e4bde393f441e]: intrinsic gas too low: have 20000, want 21000",
			},
			{ // ErrGasLimitReached
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(0), gasLimit+1, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x58ebc6e5ecfc7d220f0d137b97d519b0bee6d748ff637891e2f97eb7e8adc7a2]: gas limit reached",
			},
			{ // ErrFeeCapTooLow
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas, big.NewInt(0), big.NewInt(0)),
				},
				want: "could not apply tx 0 [0x80bfe833d79011a234eb3291501c0ee87ebb37c00674fb0152712c10ae6915b1]: max fee per gas less than block base fee: address 0x71562b71999873DB5b286dF957af199Ec94617F7, maxFeePerGas: 0, baseFee: 984375000",
			},
			{ // ErrTipVeryHigh
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas, tooBigNumber, big.NewInt(1)),
				},
				want: "could not apply tx 0 [0xc0dca0c5ffdc19a7a968aadddb33012c0d4b71dcb4bd4ebc11f656c77fe65e86]: max priority fee per gas higher than 2^256-1: address 0x71562b71999873DB5b286dF957af199Ec94617F7, maxPriorityFeePerGas bit length: 257",
			},
			{ // ErrFeeCapVeryHigh
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas, big.NewInt(1), tooBigNumber),
				},
				want: "could not apply tx 0 [0xf4344de3d587f1029a28c6a83ba853e113090689d2b34dff4008cb4830e0fc6c]: max fee per gas higher than 2^256-1: address 0x71562b71999873DB5b286dF957af199Ec94617F7, maxFeePerGas bit length: 257",
			},
			{ // ErrTipAboveFeeCap
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas, big.NewInt(2), big.NewInt(1)),
				},
				want: "could not apply tx 0 [0x9be80eb694c7621457746fa48b7277134855ef0f0a89c9494851a6ca7c50c91b]: max priority fee per gas higher than max fee per gas: address 0x71562b71999873DB5b286dF957af199Ec94617F7, maxPriorityFeePerGas: 2, maxFeePerGas: 1",
			},
			{ // ErrInsufficientFunds
				// Available balance:           1000000000000000000
				// Effective cost:                   18375000021000
				// FeeCap * gas:                1050000000000000000
				// This test is designed to have the effective cost be covered by the balance, but
				// the extended requirement on FeeCap*gas < balance to fail
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas, big.NewInt(1), big.NewInt(50000000000000)),
				},
				want: "could not apply tx 0 [0x7a88a2d5eb7ab14706682d6201ee87b27ca0886240b72ac84fed5ea9f124e761]: insufficient funds for gas * price + value: address 0x71562b71999873DB5b286dF957af199Ec94617F7 have 1000000000000000000 want 1050000000000000000",
			},
			{ // Another ErrInsufficientFunds, this one to ensure that feecap/tip of max u256 is allowed
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas, bigNumber, bigNumber),
				},
				want: "could not apply tx 0 [0xadb0614b034c7892c37cc108ae97c187a3f1b19ad9fd66debafd47c83bea9d7e]: insufficient funds for gas * price + value: address 0x71562b71999873DB5b286dF957af199Ec94617F7 required balance exceeds 256 bits",
			},
			{ // ErrMaxInitCodeSizeExceeded
				txs: []*types.Transaction{
					mkDynamicCreationTx(0, 520000, common.Big0, big.NewInt(params.InitialBaseFee), tooBigInitCode[:]),
				},
				want: "could not apply tx 0 [0xd04a638cd4148ca3810e13a0af8f6a759206c0fdaeb73bf1fbcf2e3974280697]: max initcode size exceeded: code size 49153 limit 49152",
			},
			{ // ErrIntrinsicGas: Not enough gas to cover init code
				txs: []*types.Transaction{
					mkDynamicCreationTx(0, 54299, common.Big0, big.NewInt(params.InitialBaseFee), make([]byte, 320)),
				},
				want: "could not apply tx 0 [0x94637e218dfd0e37be3e0b78a366fa717cefa62f728fa08fa2b24f0184bcb549]: intrinsic gas too low: have 54299, want 54300",
			},
			{ // ErrBlobFeeCapTooLow
				txs: []*types.Transaction{
					mkBlobTx(0, common.Address{}, params.TxGas, big.NewInt(1), big.NewInt(1), big.NewInt(0), []common.Hash{(common.Hash{1})}),
				},
				want: "could not apply tx 0 [0xa0b0d19c4a1d9eee524a2092d0e3b228cbcc90fb7258b63cfddd845c87cbe481]: max fee per gas less than block base fee: address 0x71562b71999873DB5b286dF957af199Ec94617F7, maxFeePerGas: 1, baseFee: 984375000",
			},
			{ // ErrEmptyAuthList
				txs: []*types.Transaction{
					mkSetCodeTx(0, common.Address{}, params.TxGas, big.NewInt(params.InitialBaseFee), big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x7aa3d133feeef7bb05a09c70d5bd1461c2861e16c71d4caed42c410527756b87]: EIP-7702 transaction with empty auth list (sender 0x71562b71999873DB5b286dF957af199Ec94617F7)",
			},
			// ErrSetCodeTxCreate cannot be tested here: it is impossible to create a SetCode-tx with nil `to`.
			// The EstimateGas API tests test this case.
			{ // ErrGasLimitTooHigh
				txs: []*types.Transaction{
					makeTx(key1, 0, common.Address{}, big.NewInt(0), params.MaxTxGas+1, big.NewInt(params.InitialBaseFee), nil),
				},
				want: "could not apply tx 0 [0x174949ec03b91f9b4ab21981c5d8f54fa847a57e2cf225856189bb795aa8ce9b]: transaction gas limit too high (cap: 33554432, tx: 33554433)",
			},
		} {
			block := GenerateBadBlock(gspec.ToBlock(), beacon.New(ethash.NewFaker()), tt.txs, gspec.Config, false)
			_, err := blockchain.InsertChain(types.Blocks{block}, false)
			if err == nil {
				t.Fatal("block imported without errors")
			}
			if have, want := err.Error(), tt.want; have != want {
				t.Errorf("test %d:\nhave \"%v\"\nwant \"%v\"\n", i, have, want)
			}
		}
	}

	// ErrTxTypeNotSupported, For this, we need an older chain
	{
		var (
			db    = rawdb.NewMemoryDatabase()
			gspec = &Genesis{
				Config: &params.ChainConfig{
					ChainID:             big.NewInt(1),
					HomesteadBlock:      big.NewInt(0),
					EIP150Block:         big.NewInt(0),
					EIP155Block:         big.NewInt(0),
					EIP158Block:         big.NewInt(0),
					ByzantiumBlock:      big.NewInt(0),
					ConstantinopleBlock: big.NewInt(0),
					PetersburgBlock:     big.NewInt(0),
					IstanbulBlock:       big.NewInt(0),
					MuirGlacierBlock:    big.NewInt(0),
				},
				Alloc: types.GenesisAlloc{
					common.HexToAddress("0x71562b71999873DB5b286dF957af199Ec94617F7"): types.Account{
						Balance: big.NewInt(1000000000000000000), // 1 ether
						Nonce:   0,
					},
				},
			}
			blockchain, _ = NewBlockChain(db, gspec, ethash.NewFaker(), nil)
		)
		defer blockchain.Stop()
		for i, tt := range []struct {
			txs  []*types.Transaction
			want string
		}{
			{ // ErrTxTypeNotSupported
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas-1000, big.NewInt(0), big.NewInt(0)),
				},
				want: "could not apply tx 0 [0xb3a8f07f415e7836554dc2291af495391af9bc6b9473e5fba880f69484e080a7]: transaction type not supported",
			},
		} {
			block := GenerateBadBlock(gspec.ToBlock(), ethash.NewFaker(), tt.txs, gspec.Config, true)
			_, err := blockchain.InsertChain(types.Blocks{block}, false)
			if err == nil {
				t.Fatal("block imported without errors")
			}
			if have, want := err.Error(), tt.want; have != want {
				t.Errorf("test %d:\nhave \"%v\"\nwant \"%v\"\n", i, have, want)
			}
		}
	}

	// ErrSenderNoEOA, for this we need the sender to have contract code
	{
		var (
			db    = rawdb.NewMemoryDatabase()
			gspec = &Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					common.HexToAddress("0x71562b71999873DB5b286dF957af199Ec94617F7"): types.Account{
						Balance: big.NewInt(1000000000000000000), // 1 ether
						Nonce:   0,
						Code:    common.FromHex("0xB0B0FACE"),
					},
				},
			}
			blockchain, _ = NewBlockChain(db, gspec, beacon.New(ethash.NewFaker()), nil)
		)
		defer blockchain.Stop()
		for i, tt := range []struct {
			txs  []*types.Transaction
			want string
		}{
			{ // ErrSenderNoEOA
				txs: []*types.Transaction{
					mkDynamicTx(0, common.Address{}, params.TxGas-1000, big.NewInt(0), big.NewInt(0)),
				},
				want: "could not apply tx 0 [0xb3a8f07f415e7836554dc2291af495391af9bc6b9473e5fba880f69484e080a7]: sender not an eoa: address 0x71562b71999873DB5b286dF957af199Ec94617F7, len(code): 4",
			},
		} {
			block := GenerateBadBlock(gspec.ToBlock(), beacon.New(ethash.NewFaker()), tt.txs, gspec.Config, false)
			_, err := blockchain.InsertChain(types.Blocks{block}, false)
			if err == nil {
				t.Fatal("block imported without errors")
			}
			if have, want := err.Error(), tt.want; have != want {
				t.Errorf("test %d:\nhave \"%v\"\nwant \"%v\"\n", i, have, want)
			}
		}
	}
}

// GenerateBadBlock constructs a "block" which contains the transactions. The transactions are not expected to be
// valid, and no proper post-state can be made. But from the perspective of the blockchain, the block is sufficiently
// valid to be considered for import:
// - valid pow (fake), ancestry, difficulty, gaslimit etc
func GenerateBadBlock(parent *types.Block, engine consensus.Engine, txs types.Transactions, config *params.ChainConfig, isPOW bool) *types.Block {
	difficulty := big.NewInt(0)
	if isPOW {
		fakeChainReader := newChainMaker(nil, config, engine)
		difficulty = engine.CalcDifficulty(fakeChainReader, parent.Time()+10, &types.Header{
			Number:     parent.Number(),
			Time:       parent.Time(),
			Difficulty: parent.Difficulty(),
			UncleHash:  parent.UncleHash(),
		})
	}

	header := &types.Header{
		ParentHash: parent.Hash(),
		Coinbase:   parent.Coinbase(),
		Difficulty: difficulty,
		GasLimit:   parent.GasLimit(),
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		Time:       parent.Time() + 10,
		UncleHash:  types.EmptyUncleHash,
	}

	if config.IsLondon(header.Number) {
		header.BaseFee = eip1559.CalcBaseFee(config, parent.Header())
	}
	if config.IsShanghai(header.Number) {
		header.WithdrawalsHash = &types.EmptyWithdrawalsHash
	}

	var receipts []*types.Receipt
	// The post-state result doesn't need to be correct (this is a bad block), but we do need something there
	// Preferably something unique. So let's use a combo of blocknum + txhash
	hasher := keccak.NewLegacyKeccak256()
	hasher.Write(header.Number.Bytes())

	var cumulativeGas uint64
	var nBlobs int
	for _, tx := range txs {
		txh := tx.Hash()
		hasher.Write(txh[:])

		receipt := types.NewReceipt(nil, false, cumulativeGas+tx.Gas())
		receipt.TxHash = tx.Hash()
		receipt.GasUsed = tx.Gas()
		receipts = append(receipts, receipt)
		cumulativeGas += tx.Gas()
		nBlobs += len(tx.BlobHashes())
	}

	header.Root = common.BytesToHash(hasher.Sum(nil))
	if config.IsCancun(header.Number) {
		excess := eip4844.CalcExcessBlobGas(config, parent.Header(), header.Time)
		used := uint64(nBlobs * params.BlobTxBlobGasPerBlob)
		header.ExcessBlobGas = &excess
		header.BlobGasUsed = &used

		beaconRoot := common.HexToHash("0xbeac00")
		header.ParentBeaconRoot = &beaconRoot
	}
	// Assemble and return the final block for sealing
	body := &types.Body{Transactions: txs}
	if config.IsShanghai(header.Number) {
		body.Withdrawals = []*types.Withdrawal{}
	}
	return types.NewBlock(header, body, receipts, trie.NewStackTrie(nil))
}
