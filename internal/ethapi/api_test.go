// Copyright 2023 The go-ethereum Authors
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

package ethapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/filtermaps"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/blocktest"
	"github.com/ethereum/go-ethereum/internal/ethapi/override"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

func testTransactionMarshal(t *testing.T, tests []txData, config *params.ChainConfig) {
	var (
		signer = types.LatestSigner(config)
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	)

	for i, tt := range tests {
		var tx2 types.Transaction
		tx, err := types.SignNewTx(key, signer, tt.Tx)
		if err != nil {
			t.Fatalf("test %d: signing failed: %v", i, err)
		}
		// Regular transaction
		if data, err := json.Marshal(tx); err != nil {
			t.Fatalf("test %d: marshalling failed; %v", i, err)
		} else if err = tx2.UnmarshalJSON(data); err != nil {
			t.Fatalf("test %d: unmarshal failed: %v", i, err)
		} else if want, have := tx.Hash(), tx2.Hash(); want != have {
			t.Fatalf("test %d: stx changed, want %x have %x", i, want, have)
		}

		// rpcTransaction
		rpcTx := newRPCTransaction(tx, common.Hash{}, 0, 0, 0, nil, config)
		if data, err := json.Marshal(rpcTx); err != nil {
			t.Fatalf("test %d: marshalling failed; %v", i, err)
		} else if err = tx2.UnmarshalJSON(data); err != nil {
			t.Fatalf("test %d: unmarshal failed: %v", i, err)
		} else if want, have := tx.Hash(), tx2.Hash(); want != have {
			t.Fatalf("test %d: tx changed, want %x have %x", i, want, have)
		} else {
			want, have := tt.Want, string(data)
			require.JSONEqf(t, want, have, "test %d: rpc json not match, want %s have %s", i, want, have)
		}
	}
}

func TestTransaction_RoundTripRpcJSON(t *testing.T) {
	t.Parallel()

	var (
		config = params.AllEthashProtocolChanges
		tests  = allTransactionTypes(common.Address{0xde, 0xad}, config)
	)
	testTransactionMarshal(t, tests, config)
}

func TestTransactionBlobTx(t *testing.T) {
	t.Parallel()

	config := *params.TestChainConfig
	config.ShanghaiBlock = big.NewInt(0)
	config.CancunBlock = big.NewInt(0)
	tests := allBlobTxs(common.Address{0xde, 0xad}, &config)

	testTransactionMarshal(t, tests, &config)
}

type txData struct {
	Tx   types.TxData
	Want string
}

func allTransactionTypes(addr common.Address, config *params.ChainConfig) []txData {
	return []txData{
		{
			Tx: &types.LegacyTx{
				Nonce:    5,
				GasPrice: big.NewInt(6),
				Gas:      7,
				To:       &addr,
				Value:    big.NewInt(8),
				Data:     []byte{0, 1, 2, 3, 4},
				V:        big.NewInt(9),
				R:        big.NewInt(10),
				S:        big.NewInt(11),
			},
			Want: `{
				"blockHash": null,
				"blockNumber": null,
				"blockTimestamp": null,
				"from": "0x71562b71999873db5b286df957af199ec94617f7",
				"gas": "0x7",
				"gasPrice": "0x6",
				"hash": "0x5f3240454cd09a5d8b1c5d651eefae7a339262875bcd2d0e6676f3d989967008",
				"input": "0x0001020304",
				"nonce": "0x5",
				"to": "0xdead000000000000000000000000000000000000",
				"transactionIndex": null,
				"value": "0x8",
				"type": "0x0",
				"chainId": "0x539",
				"v": "0xa96",
				"r": "0xbc85e96592b95f7160825d837abb407f009df9ebe8f1b9158a4b8dd093377f75",
				"s": "0x1b55ea3af5574c536967b039ba6999ef6c89cf22fc04bcb296e0e8b0b9b576f5"
			}`,
		}, {
			Tx: &types.LegacyTx{
				Nonce:    5,
				GasPrice: big.NewInt(6),
				Gas:      7,
				To:       nil,
				Value:    big.NewInt(8),
				Data:     []byte{0, 1, 2, 3, 4},
				V:        big.NewInt(32),
				R:        big.NewInt(10),
				S:        big.NewInt(11),
			},
			Want: `{
				"blockHash": null,
				"blockNumber": null,
				"blockTimestamp": null,
				"from": "0x71562b71999873db5b286df957af199ec94617f7",
				"gas": "0x7",
				"gasPrice": "0x6",
				"hash": "0x806e97f9d712b6cb7e781122001380a2837531b0fc1e5f5d78174ad4cb699873",
				"input": "0x0001020304",
				"nonce": "0x5",
				"to": null,
				"transactionIndex": null,
				"value": "0x8",
				"type": "0x0",
				"chainId": "0x539",
				"v": "0xa96",
				"r": "0x9dc28b267b6ad4e4af6fe9289668f9305c2eb7a3241567860699e478af06835a",
				"s": "0xa0b51a071aa9bed2cd70aedea859779dff039e3630ea38497d95202e9b1fec7"
			}`,
		},
		{
			Tx: &types.AccessListTx{
				ChainID:  config.ChainID,
				Nonce:    5,
				GasPrice: big.NewInt(6),
				Gas:      7,
				To:       &addr,
				Value:    big.NewInt(8),
				Data:     []byte{0, 1, 2, 3, 4},
				AccessList: types.AccessList{
					types.AccessTuple{
						Address:     common.Address{0x2},
						StorageKeys: []common.Hash{types.EmptyRootHash},
					},
				},
				V: big.NewInt(32),
				R: big.NewInt(10),
				S: big.NewInt(11),
			},
			Want: `{
				"blockHash": null,
				"blockNumber": null,
				"blockTimestamp": null,
				"from": "0x71562b71999873db5b286df957af199ec94617f7",
				"gas": "0x7",
				"gasPrice": "0x6",
				"hash": "0x121347468ee5fe0a29f02b49b4ffd1c8342bc4255146bb686cd07117f79e7129",
				"input": "0x0001020304",
				"nonce": "0x5",
				"to": "0xdead000000000000000000000000000000000000",
				"transactionIndex": null,
				"value": "0x8",
				"type": "0x1",
				"accessList": [
					{
						"address": "0x0200000000000000000000000000000000000000",
						"storageKeys": [
							"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
						]
					}
				],
				"chainId": "0x539",
				"v": "0x0",
				"r": "0xf372ad499239ae11d91d34c559ffc5dab4daffc0069e03afcabdcdf231a0c16b",
				"s": "0x28573161d1f9472fa0fd4752533609e72f06414f7ab5588699a7141f65d2abf",
				"yParity": "0x0"
			}`,
		}, {
			Tx: &types.AccessListTx{
				ChainID:  config.ChainID,
				Nonce:    5,
				GasPrice: big.NewInt(6),
				Gas:      7,
				To:       nil,
				Value:    big.NewInt(8),
				Data:     []byte{0, 1, 2, 3, 4},
				AccessList: types.AccessList{
					types.AccessTuple{
						Address:     common.Address{0x2},
						StorageKeys: []common.Hash{types.EmptyRootHash},
					},
				},
				V: big.NewInt(32),
				R: big.NewInt(10),
				S: big.NewInt(11),
			},
			Want: `{
				"blockHash": null,
				"blockNumber": null,
				"blockTimestamp": null,
				"from": "0x71562b71999873db5b286df957af199ec94617f7",
				"gas": "0x7",
				"gasPrice": "0x6",
				"hash": "0x067c3baebede8027b0f828a9d933be545f7caaec623b00684ac0659726e2055b",
				"input": "0x0001020304",
				"nonce": "0x5",
				"to": null,
				"transactionIndex": null,
				"value": "0x8",
				"type": "0x1",
				"accessList": [
					{
						"address": "0x0200000000000000000000000000000000000000",
						"storageKeys": [
							"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
						]
					}
				],
				"chainId": "0x539",
				"v": "0x1",
				"r": "0x542981b5130d4613897fbab144796cb36d3cb3d7807d47d9c7f89ca7745b085c",
				"s": "0x7425b9dd6c5deaa42e4ede35d0c4570c4624f68c28d812c10d806ffdf86ce63",
				"yParity": "0x1"
			}`,
		}, {
			Tx: &types.DynamicFeeTx{
				ChainID:   config.ChainID,
				Nonce:     5,
				GasTipCap: big.NewInt(6),
				GasFeeCap: big.NewInt(9),
				Gas:       7,
				To:        &addr,
				Value:     big.NewInt(8),
				Data:      []byte{0, 1, 2, 3, 4},
				AccessList: types.AccessList{
					types.AccessTuple{
						Address:     common.Address{0x2},
						StorageKeys: []common.Hash{types.EmptyRootHash},
					},
				},
				V: big.NewInt(32),
				R: big.NewInt(10),
				S: big.NewInt(11),
			},
			Want: `{
				"blockHash": null,
				"blockNumber": null,
				"blockTimestamp": null,
				"from": "0x71562b71999873db5b286df957af199ec94617f7",
				"gas": "0x7",
				"gasPrice": "0x9",
				"maxFeePerGas": "0x9",
				"maxPriorityFeePerGas": "0x6",
				"hash": "0xb63e0b146b34c3e9cb7fbabb5b3c081254a7ded6f1b65324b5898cc0545d79ff",
				"input": "0x0001020304",
				"nonce": "0x5",
				"to": "0xdead000000000000000000000000000000000000",
				"transactionIndex": null,
				"value": "0x8",
				"type": "0x2",
				"accessList": [
					{
						"address": "0x0200000000000000000000000000000000000000",
						"storageKeys": [
							"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
						]
					}
				],
				"chainId": "0x539",
				"v": "0x1",
				"r": "0x3b167e05418a8932cd53d7578711fe1a76b9b96c48642402bb94978b7a107e80",
				"s": "0x22f98a332d15ea2cc80386c1ebaa31b0afebfa79ebc7d039a1e0074418301fef",
				"yParity": "0x1"
			}`,
		}, {
			Tx: &types.DynamicFeeTx{
				ChainID:    config.ChainID,
				Nonce:      5,
				GasTipCap:  big.NewInt(6),
				GasFeeCap:  big.NewInt(9),
				Gas:        7,
				To:         nil,
				Value:      big.NewInt(8),
				Data:       []byte{0, 1, 2, 3, 4},
				AccessList: types.AccessList{},
				V:          big.NewInt(32),
				R:          big.NewInt(10),
				S:          big.NewInt(11),
			},
			Want: `{
				"blockHash": null,
				"blockNumber": null,
				"blockTimestamp": null,
				"from": "0x71562b71999873db5b286df957af199ec94617f7",
				"gas": "0x7",
				"gasPrice": "0x9",
				"maxFeePerGas": "0x9",
				"maxPriorityFeePerGas": "0x6",
				"hash": "0xcbab17ee031a9d5b5a09dff909f0a28aedb9b295ac0635d8710d11c7b806ec68",
				"input": "0x0001020304",
				"nonce": "0x5",
				"to": null,
				"transactionIndex": null,
				"value": "0x8",
				"type": "0x2",
				"accessList": [],
				"chainId": "0x539",
				"v": "0x0",
				"r": "0x6446b8a682db7e619fc6b4f6d1f708f6a17351a41c7fbd63665f469bc78b41b9",
				"s": "0x7626abc15834f391a117c63450047309dbf84c5ce3e8e609b607062641e2de43",
				"yParity": "0x0"
			}`,
		},
	}
}

func allBlobTxs(addr common.Address, config *params.ChainConfig) []txData {
	return []txData{
		{
			Tx: &types.BlobTx{
				Nonce:      6,
				GasTipCap:  uint256.NewInt(1),
				GasFeeCap:  uint256.NewInt(5),
				Gas:        6,
				To:         addr,
				BlobFeeCap: uint256.NewInt(1),
				BlobHashes: []common.Hash{{1}},
				Value:      new(uint256.Int),
				V:          uint256.NewInt(32),
				R:          uint256.NewInt(10),
				S:          uint256.NewInt(11),
			},
			Want: `{
                "blockHash": null,
                "blockNumber": null,
				"blockTimestamp": null,
                "from": "0x71562b71999873db5b286df957af199ec94617f7",
                "gas": "0x6",
                "gasPrice": "0x5",
                "maxFeePerGas": "0x5",
                "maxPriorityFeePerGas": "0x1",
                "maxFeePerBlobGas": "0x1",
                "hash": "0x1f2b59a20e61efc615ad0cbe936379d6bbea6f938aafaf35eb1da05d8e7f46a3",
                "input": "0x",
                "nonce": "0x6",
                "to": "0xdead000000000000000000000000000000000000",
                "transactionIndex": null,
                "value": "0x0",
                "type": "0x3",
                "accessList": [],
                "chainId": "0x1",
                "blobVersionedHashes": [
                    "0x0100000000000000000000000000000000000000000000000000000000000000"
                ],
                "v": "0x0",
                "r": "0x618be8908e0e5320f8f3b48042a079fe5a335ebd4ed1422a7d2207cd45d872bc",
                "s": "0x27b2bc6c80e849a8e8b764d4549d8c2efac3441e73cf37054eb0a9b9f8e89b27",
                "yParity": "0x0"
            }`,
		},
	}
}

func newTestAccountManager(t *testing.T) (*accounts.Manager, accounts.Account) {
	var (
		dir        = t.TempDir()
		am         = accounts.NewManager(nil)
		b          = keystore.NewKeyStore(dir, 2, 1)
		testKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	)
	acc, err := b.ImportECDSA(testKey, "")
	if err != nil {
		t.Fatalf("failed to create test account: %v", err)
	}
	if err := b.Unlock(acc, ""); err != nil {
		t.Fatalf("failed to unlock account: %v\n", err)
	}
	am.AddBackend(b)
	return am, acc
}

type testBackend struct {
	db     ethdb.Database
	chain  *core.BlockChain
	accman *accounts.Manager
	acc    accounts.Account

	pending         *types.Block
	pendingReceipts types.Receipts

	chainFeed *event.Feed
	autoMine  bool

	sentTx     *types.Transaction
	sentTxHash common.Hash

	syncDefaultTimeout time.Duration
	syncMaxTimeout     time.Duration

	// Relay / preconf / private tx mock controls
	preconfEnabled   bool
	privateTxEnabled bool
	acceptPreconfTxs bool
	acceptPrivateTxs bool

	// Callback overrides (nil = use default)
	txStatusFn           func(common.Hash) txpool.TxStatus
	submitTxForPreconfFn func(*types.Transaction) error
	checkPreconfStatusFn func(common.Hash) (bool, error)
	submitPrivateTxFn    func(*types.Transaction) error
	recordPrivateTxFn    func(common.Hash)
	purgePrivateTxFn     func(common.Hash)

	// Error to inject from SendTx (nil = existing logic)
	sendTxErr error
}

func fakeBlockHash(txh common.Hash) common.Hash {
	return crypto.Keccak256Hash([]byte("testblock"), txh.Bytes())
}

func newTestBackend(t *testing.T, n int, gspec *core.Genesis, engine consensus.Engine, generator func(i int, b *core.BlockGen)) *testBackend {
	options := core.DefaultConfig().WithArchive(true)
	options.TxLookupLimit = 0 // index all txs

	accman, acc := newTestAccountManager(t)
	gspec.Alloc[acc.Address] = types.Account{Balance: big.NewInt(params.Ether)}

	// Generate blocks for testing
	db, blocks, receipts := core.GenerateChainWithGenesis(gspec, engine, n+1, generator)

	chain, err := core.NewBlockChain(db, gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks[:n], false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
	backend := &testBackend{
		db:              db,
		chain:           chain,
		accman:          accman,
		acc:             acc,
		pending:         blocks[n],
		pendingReceipts: receipts[n],
		chainFeed:       new(event.Feed),
	}
	return backend
}

func (b testBackend) PreconfEnabled() bool   { return b.preconfEnabled }
func (b testBackend) PrivateTxEnabled() bool { return b.privateTxEnabled }
func (b testBackend) AcceptPreconfTxs() bool { return b.acceptPreconfTxs }
func (b testBackend) AcceptPrivateTxs() bool { return b.acceptPrivateTxs }

func (b testBackend) SubmitTxForPreconf(tx *types.Transaction) error {
	if b.submitTxForPreconfFn != nil {
		return b.submitTxForPreconfFn(tx)
	}
	return nil
}
func (b testBackend) CheckPreconfStatus(hash common.Hash) (bool, error) {
	if b.checkPreconfStatusFn != nil {
		return b.checkPreconfStatusFn(hash)
	}
	return false, nil
}
func (b testBackend) SubmitPrivateTx(tx *types.Transaction) error {
	if b.submitPrivateTxFn != nil {
		return b.submitPrivateTxFn(tx)
	}
	return nil
}
func (b testBackend) RecordPrivateTx(hash common.Hash) {
	if b.recordPrivateTxFn != nil {
		b.recordPrivateTxFn(hash)
	}
}
func (b testBackend) PurgePrivateTx(hash common.Hash) {
	if b.purgePrivateTxFn != nil {
		b.purgePrivateTxFn(hash)
	}
}

func (b testBackend) SyncProgress(ctx context.Context) ethereum.SyncProgress {
	return ethereum.SyncProgress{}
}
func (b testBackend) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return big.NewInt(0), nil
}
func (b testBackend) FeeHistory(ctx context.Context, blockCount uint64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, []*big.Int, []float64, error) {
	return nil, nil, nil, nil, nil, nil, nil
}
func (b testBackend) BlobBaseFee(ctx context.Context) *big.Int { return new(big.Int) }
func (b testBackend) BaseFee(ctx context.Context) *big.Int     { return new(big.Int) }
func (b testBackend) ChainDb() ethdb.Database                  { return b.db }
func (b testBackend) AccountManager() *accounts.Manager        { return b.accman }
func (b testBackend) ExtRPCEnabled() bool                      { return false }
func (b testBackend) RPCGasCap() uint64                        { return 10000000 }
func (b testBackend) RPCEVMTimeout() time.Duration             { return time.Second }
func (b testBackend) RPCTxFeeCap() float64                     { return 0 }
func (b testBackend) UnprotectedAllowed() bool                 { return false }
func (b testBackend) SetHead(number uint64)                    {}
func (b testBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if number == rpc.LatestBlockNumber {
		return b.chain.CurrentBlock(), nil
	}
	if number == rpc.PendingBlockNumber && b.pending != nil {
		return b.pending.Header(), nil
	}
	return b.chain.GetHeaderByNumber(uint64(number)), nil
}
func (b testBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return b.chain.GetHeaderByHash(hash), nil
}
func (b testBackend) HeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.HeaderByNumber(ctx, blockNr)
	}
	if blockHash, ok := blockNrOrHash.Hash(); ok {
		return b.HeaderByHash(ctx, blockHash)
	}
	panic("unknown type rpc.BlockNumberOrHash")
}

func (b testBackend) CurrentHeader() *types.Header    { return b.chain.CurrentHeader() }
func (b testBackend) CurrentBlock() *types.Header     { return b.chain.CurrentBlock() }
func (b testBackend) CurrentSafeBlock() *types.Header { return b.chain.CurrentSafeBlock() }
func (b testBackend) GetFinalizedBlockNumber(_ context.Context) (uint64, error) {
	// For tests, return a fixed finalized block number
	current := b.chain.CurrentBlock().Number.Uint64()
	if current < 10 {
		return 0, nil
	}
	return current - 10, nil
}
func (b testBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number == rpc.LatestBlockNumber {
		head := b.chain.CurrentBlock()
		return b.chain.GetBlock(head.Hash(), head.Number.Uint64()), nil
	}
	if number == rpc.PendingBlockNumber {
		return b.pending, nil
	}
	if number == rpc.EarliestBlockNumber {
		number = 0
	}
	return b.chain.GetBlockByNumber(uint64(number)), nil
}

func (b testBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return b.chain.GetBlockByHash(hash), nil
}
func (b testBackend) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.BlockByNumber(ctx, blockNr)
	}
	if blockHash, ok := blockNrOrHash.Hash(); ok {
		return b.BlockByHash(ctx, blockHash)
	}
	panic("unknown type rpc.BlockNumberOrHash")
}
func (b testBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	return b.chain.GetBlock(hash, uint64(number)).Body(), nil
}
func (b testBackend) StateAndHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*state.StateDB, *types.Header, error) {
	if number == rpc.PendingBlockNumber {
		panic("pending state not implemented")
	}
	header, err := b.HeaderByNumber(ctx, number)
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		return nil, nil, errors.New("header not found")
	}
	stateDb, err := b.chain.StateAt(header.Root)
	return stateDb, header, err
}
func (b testBackend) StateAndHeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return b.StateAndHeaderByNumber(ctx, blockNr)
	}
	panic("only implemented for number")
}
func (b testBackend) WaitForStateCommit(ctx context.Context, root common.Hash) error {
	return nil
}
func (b testBackend) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	block := b.pending
	if block == nil {
		return nil, nil, nil
	}
	return block, b.pendingReceipts, nil
}
func (b testBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	header, err := b.HeaderByHash(ctx, hash)
	if header == nil || err != nil {
		return nil, err
	}
	receipts := rawdb.ReadReceipts(b.db, hash, header.Number.Uint64(), header.Time, b.chain.Config())
	return receipts, nil
}
func (b testBackend) GetTd(ctx context.Context, hash common.Hash) *big.Int {
	if b.pending != nil && hash == b.pending.Hash() {
		return nil
	}
	return big.NewInt(1)
}
func (b testBackend) GetTdByNumber(ctx context.Context, blockNr rpc.BlockNumber) *big.Int {
	panic("not implemented")
}

func (b testBackend) GetEVM(ctx context.Context, state *state.StateDB, header *types.Header, vmConfig *vm.Config, blockContext *vm.BlockContext) *vm.EVM {
	if vmConfig == nil {
		vmConfig = b.chain.GetVMConfig()
	}
	context := core.NewEVMBlockContext(header, b.chain, nil)
	if blockContext != nil {
		context = *blockContext
	}
	return vm.NewEVM(context, state, b.chain.Config(), *vmConfig)
}
func (b testBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return b.chainFeed.Subscribe(ch)
}
func (b testBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	panic("implement me")
}
func (b *testBackend) SendTx(ctx context.Context, tx *types.Transaction) error {
	if b.sendTxErr != nil {
		return b.sendTxErr
	}
	b.sentTx = tx
	b.sentTxHash = tx.Hash()

	if b.autoMine {
		// Synthesize a "mined" receipt at head+1
		num := b.chain.CurrentHeader().Number.Uint64() + 1
		receipt := &types.Receipt{
			TxHash:            tx.Hash(),
			Status:            types.ReceiptStatusSuccessful,
			BlockHash:         fakeBlockHash(tx.Hash()),
			BlockNumber:       new(big.Int).SetUint64(num),
			TransactionIndex:  0,
			CumulativeGasUsed: 21000,
			GasUsed:           21000,
		}
		// Broadcast a ChainEvent that includes the receipts and txs
		b.chainFeed.Send(core.ChainEvent{
			Header: &types.Header{
				Number: new(big.Int).SetUint64(num),
			},
			Receipts:     types.Receipts{receipt},
			Transactions: types.Transactions{tx},
		})
	}
	return nil
}
func (b *testBackend) GetCanonicalTransaction(txHash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
	// Treat the auto-mined tx as canonically placed at head+1.
	if b.autoMine && txHash == b.sentTxHash {
		num := b.chain.CurrentHeader().Number.Uint64() + 1
		return true, b.sentTx, fakeBlockHash(txHash), num, 0
	}
	tx, blockHash, blockNumber, index := rawdb.ReadCanonicalTransaction(b.db, txHash)
	return tx != nil, tx, blockHash, blockNumber, index
}
func (b *testBackend) GetCanonicalReceipt(tx *types.Transaction, blockHash common.Hash, blockNumber, blockIndex uint64) (*types.Receipt, error) {
	if b.autoMine && tx != nil && tx.Hash() == b.sentTxHash &&
		blockHash == fakeBlockHash(tx.Hash()) &&
		blockIndex == 0 &&
		blockNumber == b.chain.CurrentHeader().Number.Uint64()+1 {
		return &types.Receipt{
			Type:              tx.Type(),
			Status:            types.ReceiptStatusSuccessful,
			CumulativeGasUsed: 21000,
			GasUsed:           21000,
			EffectiveGasPrice: big.NewInt(1),
			BlockHash:         blockHash,
			BlockNumber:       new(big.Int).SetUint64(blockNumber),
			TransactionIndex:  0,
			TxHash:            tx.Hash(),
		}, nil
	}
	return b.chain.GetCanonicalReceipt(tx, blockHash, blockNumber, blockIndex)
}
func (b testBackend) TxIndexDone() bool {
	return true
}
func (b testBackend) GetPoolTransactions() (types.Transactions, error) { return nil, nil }
func (b testBackend) GetPoolTransaction(txHash common.Hash) *types.Transaction {
	return nil
}
func (b testBackend) GetPoolNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return 0, nil
}
func (b testBackend) Stats() (pending int, queued int) { panic("implement me") }
func (b testBackend) TxPoolContent() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	panic("implement me")
}
func (b testBackend) TxPoolContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	panic("implement me")
}
func (b testBackend) TxStatus(hash common.Hash) txpool.TxStatus {
	if b.txStatusFn != nil {
		return b.txStatusFn(hash)
	}
	return txpool.TxStatusUnknown
}
func (b testBackend) SubscribeNewTxsEvent(events chan<- core.NewTxsEvent) event.Subscription {
	panic("implement me")
}
func (b testBackend) ChainConfig() *params.ChainConfig { return b.chain.Config() }
func (b testBackend) Engine() consensus.Engine         { return b.chain.Engine() }
func (b testBackend) GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error) {
	panic("implement me")
}
func (b testBackend) SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription {
	panic("implement me")
}
func (b testBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	panic("implement me")
}

// GetBorBlockTransaction returns bor block tx
func (b testBackend) GetBorBlockTransaction(ctx context.Context, hash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadBorTransaction(b.ChainDb(), hash)
	return tx, blockHash, blockNumber, index, nil
}

func (b testBackend) GetBorBlockTransactionWithBlockHash(ctx context.Context, txHash common.Hash, blockHash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadBorTransactionWithBlockHash(b.ChainDb(), txHash, blockHash)
	return tx, blockHash, blockNumber, index, nil
}

func (b testBackend) GetRootHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64) (string, error) {
	panic("implement me")
}

func (b testBackend) GetVoteOnHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64, hash string, milestoneId string) (bool, error) {
	panic("implement me")
}

func (b testBackend) GetWhitelistedCheckpoint() (bool, uint64, common.Hash) {
	panic("implement me")
}

func (b testBackend) GetWhitelistedMilestone() (bool, uint64, common.Hash) {
	panic("implement me")
}

func (b testBackend) PurgeWhitelistedMilestone() {
	panic("implement me")
}

func (b testBackend) PurgeWhitelistedCheckpoint() {
	panic("implement me")
}

func (b testBackend) RPCRpcReturnDataLimit() uint64 {
	return 0
}

func (b testBackend) SubscribeChain2HeadEvent(ch chan<- core.Chain2HeadEvent) event.Subscription {
	panic("implement me")
}

func (b testBackend) SubscribeStateSyncEvent(ch chan<- core.StateSyncEvent) event.Subscription {
	panic("implement me")
}

func (b testBackend) PeerStats() interface{} {
	panic("implement me")
}

func (b testBackend) GetBorBlockLogs(ctx context.Context, hash common.Hash) ([]*types.Log, error) {
	receipt, err := b.GetBorBlockReceipt(ctx, hash)
	if err != nil || receipt == nil {
		return nil, err
	}

	return receipt.Logs, nil
}

func (b testBackend) GetWitnesses(ctx context.Context, startBlock uint64, endBlock uint64) ([]*stateless.Witness, error) {
	return nil, nil
}

func (b testBackend) StoreWitness(ctx context.Context, hash common.Hash, witness *stateless.Witness) error {
	return nil
}

func (b testBackend) WitnessByNumber(ctx context.Context, number rpc.BlockNumber) (*stateless.Witness, error) {
	blockHeader, err := b.HeaderByNumber(ctx, number)
	if err != nil {
		return nil, err
	}
	if blockHeader == nil {
		return nil, nil
	}

	rlpEncodedWitness := rawdb.ReadWitness(b.ChainDb(), blockHeader.Hash())
	if len(rlpEncodedWitness) == 0 {
		return nil, nil
	}

	witness, err := stateless.GetWitnessFromRlp(rlpEncodedWitness)
	if err != nil {
		return nil, err
	}

	return witness, nil
}

func (b testBackend) WitnessByHash(ctx context.Context, hash common.Hash) (*stateless.Witness, error) {
	blockHeader, err := b.HeaderByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if blockHeader == nil {
		return nil, nil
	}

	rlpEncodedWitness := rawdb.ReadWitness(b.ChainDb(), hash)
	if len(rlpEncodedWitness) == 0 {
		return nil, nil
	}

	witness, err := stateless.GetWitnessFromRlp(rlpEncodedWitness)
	if err != nil {
		return nil, err
	}

	return witness, nil
}

func (b testBackend) WitnessByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*stateless.Witness, error) {
	if blockNumber, ok := blockNrOrHash.Number(); ok {
		return b.WitnessByNumber(ctx, blockNumber)
	}
	if blockHash, ok := blockNrOrHash.Hash(); ok {
		return b.WitnessByHash(ctx, blockHash)
	}

	return nil, errors.New("invalid block number or hash")
}

func (b testBackend) GetBorBlockReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	receipt := b.chain.GetBorReceiptByHash(hash)
	if receipt == nil {
		return nil, ethereum.NotFound
	}

	return receipt, nil
}

func (b testBackend) CurrentView() *filtermaps.ChainView {
	panic("implement me")
}
func (b testBackend) NewMatcherBackend() filtermaps.MatcherBackend {
	panic("implement me")
}

func (b testBackend) SubscribePendingLogsEvent(ch chan<- []*types.Log) event.Subscription {
	panic("implement me")
}

func (b testBackend) HistoryPruningCutoff() uint64 {
	bn, _ := b.chain.HistoryPruningCutoff()
	return bn
}

func (b testBackend) IsParallelImportActive() bool {
	return false
}

func TestEstimateGas(t *testing.T) {
	t.Parallel()
	// Initialize test accounts
	var (
		accounts = newAccounts(4)
		genesis  = &core.Genesis{
			Config: params.MergedTestChainConfig,
			Alloc: types.GenesisAlloc{
				accounts[0].addr: {Balance: big.NewInt(params.Ether)},
				accounts[1].addr: {Balance: big.NewInt(params.Ether)},
				accounts[2].addr: {Balance: big.NewInt(params.Ether), Code: append(types.DelegationPrefix, accounts[3].addr.Bytes()...)},
			},
		}
		genBlocks      = 10
		signer         = types.HomesteadSigner{}
		randomAccounts = newAccounts(2)
	)
	packRevert := func(revertMessage string) []byte {
		var revertSelector = crypto.Keccak256([]byte("Error(string)"))[:4]
		stringType, _ := abi.NewType("string", "", nil)
		args := abi.Arguments{
			{Type: stringType},
		}
		encodedMessage, _ := args.Pack(revertMessage)

		return append(revertSelector, encodedMessage...)
	}

	api := NewBlockChainAPI(newTestBackend(t, genBlocks, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(i), To: &accounts[1].addr, Value: big.NewInt(1000), Gas: params.TxGas, GasPrice: b.BaseFee(), Data: nil}), signer, accounts[0].key)
		b.AddTx(tx)
		b.SetPoS()
	}))

	setCodeAuthorization, _ := types.SignSetCode(accounts[0].key, types.SetCodeAuthorization{
		Address: accounts[0].addr,
		Nonce:   uint64(genBlocks + 1),
	})

	var testSuite = []struct {
		blockNumber    rpc.BlockNumber
		call           TransactionArgs
		overrides      override.StateOverride
		blockOverrides override.BlockOverrides
		expectErr      error
		want           uint64
	}{
		//simple transfer on latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: nil,
			want:      21000,
		},
		// simple transfer with insufficient funds on latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: core.ErrInsufficientFunds,
			want:      21000,
		},
		// empty create
		{
			blockNumber: rpc.LatestBlockNumber,
			call:        TransactionArgs{},
			expectErr:   nil,
			want:        53000,
		},
		{
			blockNumber: rpc.LatestBlockNumber,
			call:        TransactionArgs{},
			overrides: override.StateOverride{
				randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether)))},
			},
			expectErr: nil,
			want:      53000,
		},
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			overrides: override.StateOverride{
				randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(big.NewInt(0))},
			},
			expectErr: core.ErrInsufficientFunds,
		},
		// Test for a bug where the gas price was set to zero but the basefee non-zero
		//
		// contract BasefeeChecker {
		//    constructor() {
		//        require(tx.gasprice >= block.basefee);
		//        if (tx.gasprice > 0) {
		//            require(block.basefee > 0);
		//        }
		//    }
		//}
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:     &accounts[0].addr,
				Input:    hex2Bytes("6080604052348015600f57600080fd5b50483a1015601c57600080fd5b60003a111560315760004811603057600080fd5b5b603f80603e6000396000f3fe6080604052600080fdfea264697066735822122060729c2cee02b10748fae5200f1c9da4661963354973d9154c13a8e9ce9dee1564736f6c63430008130033"),
				GasPrice: (*hexutil.Big)(big.NewInt(1_000_000_000)), // Legacy as pricing
			},
			expectErr: nil,
			want:      67617,
		},
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:         &accounts[0].addr,
				Input:        hex2Bytes("6080604052348015600f57600080fd5b50483a1015601c57600080fd5b60003a111560315760004811603057600080fd5b5b603f80603e6000396000f3fe6080604052600080fdfea264697066735822122060729c2cee02b10748fae5200f1c9da4661963354973d9154c13a8e9ce9dee1564736f6c63430008130033"),
				MaxFeePerGas: (*hexutil.Big)(big.NewInt(1_000_000_000)), // 1559 gas pricing
			},
			expectErr: nil,
			want:      67617,
		},
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:         &accounts[0].addr,
				Input:        hex2Bytes("6080604052348015600f57600080fd5b50483a1015601c57600080fd5b60003a111560315760004811603057600080fd5b5b603f80603e6000396000f3fe6080604052600080fdfea264697066735822122060729c2cee02b10748fae5200f1c9da4661963354973d9154c13a8e9ce9dee1564736f6c63430008130033"),
				GasPrice:     nil, // No legacy gas pricing
				MaxFeePerGas: nil, // No 1559 gas pricing
			},
			expectErr: nil,
			want:      67595,
		},
		// Blobs should have no effect on gas estimate
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:       &accounts[0].addr,
				To:         &accounts[1].addr,
				Value:      (*hexutil.Big)(big.NewInt(1)),
				BlobHashes: []common.Hash{{0x01, 0x22}},
				BlobFeeCap: (*hexutil.Big)(big.NewInt(1)),
			},
			want: 21000,
		},
		// // SPDX-License-Identifier: GPL-3.0
		//pragma solidity >=0.8.2 <0.9.0;
		//
		//contract BlockOverridesTest {
		//    function call() public view returns (uint256) {
		//        return block.number;
		//    }
		//
		//    function estimate() public view {
		//        revert(string.concat("block ", uint2str(block.number)));
		//    }
		//
		//    function uint2str(uint256 _i) internal pure returns (string memory str) {
		//        if (_i == 0) {
		//            return "0";
		//        }
		//        uint256 j = _i;
		//        uint256 length;
		//        while (j != 0) {
		//            length++;
		//            j /= 10;
		//        }
		//        bytes memory bstr = new bytes(length);
		//        uint256 k = length;
		//        j = _i;
		//        while (j != 0) {
		//            bstr[--k] = bytes1(uint8(48 + (j % 10)));
		//            j /= 10;
		//        }
		//        str = string(bstr);
		//    }
		//}
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From: &accounts[0].addr,
				To:   &accounts[1].addr,
				Data: hex2Bytes("0x3592d016"), //estimate
			},
			overrides: override.StateOverride{
				accounts[1].addr: override.OverrideAccount{
					Code: hex2Bytes("608060405234801561000f575f5ffd5b5060043610610034575f3560e01c806328b5e32b146100385780633592d0161461004b575b5f5ffd5b4360405190815260200160405180910390f35b610053610055565b005b61005e4361009d565b60405160200161006e91906101a5565b60408051601f198184030181529082905262461bcd60e51b8252610094916004016101cd565b60405180910390fd5b6060815f036100c35750506040805180820190915260018152600360fc1b602082015290565b815f5b81156100ec57806100d681610216565b91506100e59050600a83610242565b91506100c6565b5f8167ffffffffffffffff81111561010657610106610255565b6040519080825280601f01601f191660200182016040528015610130576020820181803683370190505b508593509050815b831561019c57610149600a85610269565b61015490603061027c565b60f81b8261016183610295565b92508281518110610174576101746102aa565b60200101906001600160f81b03191690815f1a905350610195600a85610242565b9350610138565b50949350505050565b650313637b1b5960d51b81525f82518060208501600685015e5f920160060191825250919050565b602081525f82518060208401528060208501604085015e5f604082850101526040601f19601f83011684010191505092915050565b634e487b7160e01b5f52601160045260245ffd5b5f6001820161022757610227610202565b5060010190565b634e487b7160e01b5f52601260045260245ffd5b5f826102505761025061022e565b500490565b634e487b7160e01b5f52604160045260245ffd5b5f826102775761027761022e565b500690565b8082018082111561028f5761028f610202565b92915050565b5f816102a3576102a3610202565b505f190190565b634e487b7160e01b5f52603260045260245ffdfea2646970667358221220a253cad1e2e3523b8c053c1d0cd1e39d7f3bafcedd73440a244872701f05dab264736f6c634300081c0033"),
				},
			},
			blockOverrides: override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(11))},
			expectErr:      newRevertError(packRevert("block 11")),
		},
		// Should be able to send to an EIP-7702 delegated account.
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[2].addr,
				Value: (*hexutil.Big)(big.NewInt(1)),
			},
			want: 21000,
		},
		// Should be able to send as EIP-7702 delegated account.
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &accounts[2].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1)),
			},
			want: 21000,
		},
		// Should be able to estimate SetCodeTx.
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:              &accounts[0].addr,
				To:                &accounts[1].addr,
				Value:             (*hexutil.Big)(big.NewInt(0)),
				AuthorizationList: []types.SetCodeAuthorization{setCodeAuthorization},
			},
			want: 46000,
		},
		// Should retrieve the code of 0xef0001 || accounts[0].addr and return an invalid opcode error.
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:              &accounts[0].addr,
				To:                &accounts[0].addr,
				Value:             (*hexutil.Big)(big.NewInt(0)),
				AuthorizationList: []types.SetCodeAuthorization{setCodeAuthorization},
			},
			expectErr: errors.New("invalid opcode: opcode 0xef not defined"),
		},
		// SetCodeTx with empty authorization list should fail.
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:              &accounts[0].addr,
				To:                &common.Address{},
				Value:             (*hexutil.Big)(big.NewInt(0)),
				AuthorizationList: []types.SetCodeAuthorization{},
			},
			expectErr: core.ErrEmptyAuthList,
		},
		// SetCodeTx with nil `to` should fail.
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:              &accounts[0].addr,
				To:                nil,
				Value:             (*hexutil.Big)(big.NewInt(0)),
				AuthorizationList: []types.SetCodeAuthorization{setCodeAuthorization},
			},
			expectErr: core.ErrSetCodeTxCreate,
		},
	}
	for i, tc := range testSuite {
		result, err := api.EstimateGas(t.Context(), tc.call, &rpc.BlockNumberOrHash{BlockNumber: &tc.blockNumber}, &tc.overrides, &tc.blockOverrides)
		if tc.expectErr != nil {
			if err == nil {
				t.Errorf("test %d: want error %v, have nothing", i, tc.expectErr)
				continue
			}
			if !errors.Is(err, tc.expectErr) {
				if err.Error() != tc.expectErr.Error() {
					t.Errorf("test %d: error mismatch, want %v, have %v", i, tc.expectErr, err)
				}
			}
			continue
		}
		if err != nil {
			t.Errorf("test %d: want no error, have %v", i, err)
			continue
		}
		if float64(result) > float64(tc.want)*(1+estimateGasErrorRatio) {
			t.Errorf("test %d, result mismatch, have\n%v\n, want\n%v\n", i, uint64(result), tc.want)
		}
	}
}

func TestCall(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	var (
		accounts = newAccounts(3)
		dad      = common.HexToAddress("0x0000000000000000000000000000000000000dad")
		genesis  = &core.Genesis{
			Config: params.MergedTestChainConfig,
			Alloc: types.GenesisAlloc{
				accounts[0].addr: {Balance: big.NewInt(params.Ether)},
				accounts[1].addr: {Balance: big.NewInt(params.Ether)},
				accounts[2].addr: {Balance: big.NewInt(params.Ether)},
				dad: {
					Balance: big.NewInt(params.Ether),
					Nonce:   1,
					Storage: map[common.Hash]common.Hash{
						{}: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
					},
				},
			},
		}
		genBlocks = 10
		signer    = types.HomesteadSigner{}
	)
	api := NewBlockChainAPI(newTestBackend(t, genBlocks, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(i), To: &accounts[1].addr, Value: big.NewInt(1000), Gas: params.TxGas, GasPrice: b.BaseFee(), Data: nil}), signer, accounts[0].key)
		b.AddTx(tx)
		b.SetPoS()
	}))
	randomAccounts := newAccounts(3)
	var testSuite = []struct {
		name           string
		blockNumber    rpc.BlockNumber
		overrides      override.StateOverride
		call           TransactionArgs
		blockOverrides override.BlockOverrides
		expectErr      error
		want           string
	}{
		// transfer on genesis
		{
			name:        "transfer-on-genesis",
			blockNumber: rpc.BlockNumber(0),
			call: TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: nil,
			want:      "0x",
		},
		// transfer on the head
		{
			name:        "transfer-on-the-head",
			blockNumber: rpc.BlockNumber(genBlocks),
			call: TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: nil,
			want:      "0x",
		},
		// transfer on a non-existent block, error expects
		{
			name:        "transfer-non-existent-block",
			blockNumber: rpc.BlockNumber(genBlocks + 1),
			call: TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: errors.New("header not found"),
		},
		// transfer on the latest block
		{
			name:        "transfer-latest-block",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: nil,
			want:      "0x",
		},
		// Call which can only succeed if state is state overridden
		{
			name:        "state-override-success",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			overrides: override.StateOverride{
				randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether)))},
			},
			want: "0x",
		},
		// Invalid call without state overriding
		{
			name:        "insufficient-funds-simple",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			expectErr: core.ErrInsufficientFunds,
		},
		// Successful simple contract call
		//
		// // SPDX-License-Identifier: GPL-3.0
		//
		//  pragma solidity >=0.7.0 <0.8.0;
		//
		//  /**
		//   * @title Storage
		//   * @dev Store & retrieve value in a variable
		//   */
		//  contract Storage {
		//      uint256 public number;
		//      constructor() {
		//          number = block.number;
		//      }
		//  }
		{
			name:        "simple-contract-call",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &randomAccounts[2].addr,
				Data: hex2Bytes("8381f58a"), // call number()
			},
			overrides: override.StateOverride{
				randomAccounts[2].addr: override.OverrideAccount{
					Code:      hex2Bytes("6080604052348015600f57600080fd5b506004361060285760003560e01c80638381f58a14602d575b600080fd5b60336049565b6040518082815260200191505060405180910390f35b6000548156fea2646970667358221220eab35ffa6ab2adfe380772a48b8ba78e82a1b820a18fcb6f59aa4efb20a5f60064736f6c63430007040033"),
					StateDiff: map[common.Hash]common.Hash{{}: common.BigToHash(big.NewInt(123))},
				},
			},
			want: "0x000000000000000000000000000000000000000000000000000000000000007b",
		},
		// // SPDX-License-Identifier: GPL-3.0
		//pragma solidity >=0.8.2 <0.9.0;
		//
		//contract BlockOverridesTest {
		//    function call() public view returns (uint256) {
		//        return block.number;
		//    }
		//
		//    function estimate() public view {
		//        revert(string.concat("block ", uint2str(block.number)));
		//    }
		//
		//    function uint2str(uint256 _i) internal pure returns (string memory str) {
		//        if (_i == 0) {
		//            return "0";
		//        }
		//        uint256 j = _i;
		//        uint256 length;
		//        while (j != 0) {
		//            length++;
		//            j /= 10;
		//        }
		//        bytes memory bstr = new bytes(length);
		//        uint256 k = length;
		//        j = _i;
		//        while (j != 0) {
		//            bstr[--k] = bytes1(uint8(48 + (j % 10)));
		//            j /= 10;
		//        }
		//        str = string(bstr);
		//    }
		//}
		{
			name:        "block-override-with-state-override",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From: &accounts[1].addr,
				To:   &accounts[2].addr,
				Data: hex2Bytes("0x28b5e32b"), //call
			},
			overrides: override.StateOverride{
				accounts[2].addr: override.OverrideAccount{
					Code: hex2Bytes("608060405234801561000f575f5ffd5b5060043610610034575f3560e01c806328b5e32b146100385780633592d0161461004b575b5f5ffd5b4360405190815260200160405180910390f35b610053610055565b005b61005e4361009d565b60405160200161006e91906101a5565b60408051601f198184030181529082905262461bcd60e51b8252610094916004016101cd565b60405180910390fd5b6060815f036100c35750506040805180820190915260018152600360fc1b602082015290565b815f5b81156100ec57806100d681610216565b91506100e59050600a83610242565b91506100c6565b5f8167ffffffffffffffff81111561010657610106610255565b6040519080825280601f01601f191660200182016040528015610130576020820181803683370190505b508593509050815b831561019c57610149600a85610269565b61015490603061027c565b60f81b8261016183610295565b92508281518110610174576101746102aa565b60200101906001600160f81b03191690815f1a905350610195600a85610242565b9350610138565b50949350505050565b650313637b1b5960d51b81525f82518060208501600685015e5f920160060191825250919050565b602081525f82518060208401528060208501604085015e5f604082850101526040601f19601f83011684010191505092915050565b634e487b7160e01b5f52601160045260245ffd5b5f6001820161022757610227610202565b5060010190565b634e487b7160e01b5f52601260045260245ffd5b5f826102505761025061022e565b500490565b634e487b7160e01b5f52604160045260245ffd5b5f826102775761027761022e565b500690565b8082018082111561028f5761028f610202565b92915050565b5f816102a3576102a3610202565b505f190190565b634e487b7160e01b5f52603260045260245ffdfea2646970667358221220a253cad1e2e3523b8c053c1d0cd1e39d7f3bafcedd73440a244872701f05dab264736f6c634300081c0033"),
				},
			},
			blockOverrides: override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(11))},
			want:           "0x000000000000000000000000000000000000000000000000000000000000000b",
		},
		// Clear storage trie
		{
			name:        "clear-storage-trie",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From: &accounts[1].addr,
				// Yul:
				// object "Test" {
				//    code {
				//        let dad := 0x0000000000000000000000000000000000000dad
				//        if eq(balance(dad), 0) {
				//            revert(0, 0)
				//        }
				//        let slot := sload(0)
				//        mstore(0, slot)
				//        return(0, 32)
				//    }
				// }
				Input: hex2Bytes("610dad6000813103600f57600080fd5b6000548060005260206000f3"),
			},
			overrides: override.StateOverride{
				dad: override.OverrideAccount{
					State: map[common.Hash]common.Hash{},
				},
			},
			want: "0x0000000000000000000000000000000000000000000000000000000000000000",
		},
		// Invalid blob tx
		{
			name:        "invalid-blob-tx",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From:       &accounts[1].addr,
				Input:      &hexutil.Bytes{0x00},
				BlobHashes: []common.Hash{},
			},
			expectErr: core.ErrBlobTxCreate,
		},
		// BOR Doesn't support blob tx
		// BLOBHASH opcode
		// {
		// 	blockNumber: rpc.LatestBlockNumber,
		// 	call: TransactionArgs{
		// 		From:       &accounts[1].addr,
		// 		To:         &randomAccounts[2].addr,
		// 		BlobHashes: []common.Hash{{0x01, 0x22}},
		// 		BlobFeeCap: (*hexutil.Big)(big.NewInt(1)),
		// 	},
		// 	overrides: StateOverride{
		// 		randomAccounts[2].addr: {
		// 			Code: hex2Bytes("60004960005260206000f3"),
		// 		},
		// 	},
		// 	want: "0x0122000000000000000000000000000000000000000000000000000000000000",
		// },
		// Clear the entire storage set
		{
			name:        "blobhash-opcode",
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From: &accounts[1].addr,
				// Yul:
				// object "Test" {
				//    code {
				//        let dad := 0x0000000000000000000000000000000000000dad
				//        if eq(balance(dad), 0) {
				//            revert(0, 0)
				//        }
				//        let slot := sload(0)
				//        mstore(0, slot)
				//        return(0, 32)
				//    }
				// }
				Input: hex2Bytes("610dad6000813103600f57600080fd5b6000548060005260206000f3"),
			},
			overrides: override.StateOverride{
				dad: override.OverrideAccount{
					State: map[common.Hash]common.Hash{},
				},
			},
			want: "0x0000000000000000000000000000000000000000000000000000000000000000",
		},
		// Clear the entire storage set
		{
			blockNumber: rpc.LatestBlockNumber,
			call: TransactionArgs{
				From: &accounts[1].addr,
				// Yul:
				// object "Test" {
				//    code {
				//        let dad := 0x0000000000000000000000000000000000000dad
				//        if eq(balance(dad), 0) {
				//            revert(0, 0)
				//        }
				//        let slot := sload(0)
				//        mstore(0, slot)
				//        return(0, 32)
				//    }
				// }
				Input: hex2Bytes("610dad6000813103600f57600080fd5b6000548060005260206000f3"),
			},
			overrides: override.StateOverride{
				dad: override.OverrideAccount{
					State: map[common.Hash]common.Hash{},
				},
			},
			want: "0x0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:        "unsupported block override beaconRoot",
			blockNumber: rpc.LatestBlockNumber,
			call:        TransactionArgs{},
			blockOverrides: override.BlockOverrides{
				BeaconRoot: &common.Hash{0, 1, 2},
			},
			expectErr: errors.New(`block override "beaconRoot" is not supported for this RPC method`),
		},
		{
			name:        "unsupported block override withdrawals",
			blockNumber: rpc.LatestBlockNumber,
			call:        TransactionArgs{},
			blockOverrides: override.BlockOverrides{
				Withdrawals: &types.Withdrawals{},
			},
			expectErr: errors.New(`block override "withdrawals" is not supported for this RPC method`),
		},
	}
	for _, tc := range testSuite {
		result, err := api.Call(t.Context(), tc.call, &rpc.BlockNumberOrHash{BlockNumber: &tc.blockNumber}, &tc.overrides, &tc.blockOverrides)
		if tc.expectErr != nil {
			if err == nil {
				t.Errorf("test %s: want error %v, have nothing", tc.name, tc.expectErr)
				continue
			}
			if !errors.Is(err, tc.expectErr) {
				// Second try
				if !reflect.DeepEqual(err, tc.expectErr) {
					t.Errorf("test %s: error mismatch, want %v, have %v", tc.name, tc.expectErr, err)
				}
			}
			continue
		}
		if err != nil {
			t.Errorf("test %s: want no error, have %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(result.String(), tc.want) {
			t.Errorf("test %s, result mismatch, have\n%v\n, want\n%v\n", tc.name, result.String(), tc.want)
		}
	}
}

func TestSimulateV1(t *testing.T) {
	t.Parallel()
	// Initialize test accounts
	var (
		accounts     = newAccounts(3)
		fixedAccount = newTestAccount()
		genBlocks    = 10
		signer       = types.HomesteadSigner{}
		cac          = common.HexToAddress("0x0000000000000000000000000000000000000cac")
		bab          = common.HexToAddress("0x0000000000000000000000000000000000000bab")
		coinbase     = "0x000000000000000000000000000000000000ffff"
		genesis      = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accounts[0].addr: {Balance: big.NewInt(params.Ether)},
				accounts[1].addr: {Balance: big.NewInt(params.Ether)},
				accounts[2].addr: {Balance: big.NewInt(params.Ether)},
				// Yul:
				// object "Test" {
				//     code {
				//         let dad := 0x0000000000000000000000000000000000000dad
				//         selfdestruct(dad)
				//     }
				// }
				cac: {Balance: big.NewInt(params.Ether), Code: common.Hex2Bytes("610dad80ff")},
				bab: {
					Balance: big.NewInt(1),
					// object "Test" {
					//    code {
					//        let value1 := sload(1)
					//        let value2 := sload(2)
					//
					//        // Shift value1 by 128 bits to the left by multiplying it with 2^128
					//        value1 := mul(value1, 0x100000000000000000000000000000000)
					//
					//        // Concatenate value1 and value2
					//        let concatenatedValue := add(value1, value2)
					//
					//        // Store the result in memory and return it
					//        mstore(0, concatenatedValue)
					//        return(0, 0x20)
					//    }
					// }
					Code: common.FromHex("0x600154600254700100000000000000000000000000000000820291508082018060005260206000f3"),
					Storage: map[common.Hash]common.Hash{
						common.BigToHash(big.NewInt(1)): common.BigToHash(big.NewInt(10)),
						common.BigToHash(big.NewInt(2)): common.BigToHash(big.NewInt(12)),
					},
				},
			},
		}
		sha256Address = common.BytesToAddress([]byte{0x02})
	)
	api := NewBlockChainAPI(newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.HexToAddress(coinbase))
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), signer, accounts[0].key)
		b.AddTx(tx)
	}))
	var (
		randomAccounts   = newAccounts(4)
		latest           = rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
		includeTransfers = true
		validation       = true
	)
	type log struct {
		Address        common.Address `json:"address"`
		Topics         []common.Hash  `json:"topics"`
		Data           hexutil.Bytes  `json:"data"`
		BlockNumber    hexutil.Uint64 `json:"blockNumber"`
		BlockTimestamp hexutil.Uint64 `json:"blockTimestamp"`
		// Skip txHash
		//TxHash common.Hash `json:"transactionHash" gencodec:"required"`
		TxIndex hexutil.Uint `json:"transactionIndex"`
		//BlockHash common.Hash  `json:"blockHash"`
		Index hexutil.Uint `json:"logIndex"`
	}
	type callErr struct {
		Message string
		Code    int
	}
	type callRes struct {
		ReturnValue string `json:"returnData"`
		Error       callErr
		Logs        []log
		GasUsed     string
		Status      string
	}
	type blockRes struct {
		Number string
		//Hash   string
		// Ignore timestamp
		GasLimit      string
		GasUsed       string
		Miner         string
		BaseFeePerGas string
		Calls         []callRes
	}
	var testSuite = []struct {
		name             string
		blocks           []simBlock
		tag              rpc.BlockNumberOrHash
		includeTransfers *bool
		validation       *bool
		expectErr        error
		want             []blockRes
	}{
		// State build-up over calls:
		// First value transfer OK after state override.
		// Second one should succeed because of first transfer.
		{
			name: "simple",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(big.NewInt(1000))},
				},
				Calls: []TransactionArgs{{
					From:  &randomAccounts[0].addr,
					To:    &randomAccounts[1].addr,
					Value: (*hexutil.Big)(big.NewInt(1000)),
				}, {
					From:  &randomAccounts[1].addr,
					To:    &randomAccounts[2].addr,
					Value: (*hexutil.Big)(big.NewInt(1000)),
				}, {
					To: &randomAccounts[3].addr,
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xf618",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[0].addr.Bytes()),
								common.BytesToHash(randomAccounts[1].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8},
							BlockNumber:    11,
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        0,
							Index:          0,
						},
					},
					Status: "0x1",
				}, {
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[1].addr.Bytes()),
								common.BytesToHash(randomAccounts[2].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8},
							BlockNumber:    11,
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        1,
							Index:          1,
						},
					},
					Status: "0x1",
				}, {
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		}, {
			// State build-up over blocks.
			name: "simple-multi-block",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(big.NewInt(2000))},
				},
				Calls: []TransactionArgs{
					{
						From:  &randomAccounts[0].addr,
						To:    &randomAccounts[1].addr,
						Value: (*hexutil.Big)(big.NewInt(1000)),
					}, {
						From:  &randomAccounts[0].addr,
						To:    &randomAccounts[3].addr,
						Value: (*hexutil.Big)(big.NewInt(1000)),
					},
				},
			}, {
				StateOverrides: &override.StateOverride{
					randomAccounts[3].addr: override.OverrideAccount{Balance: newRPCBalance(big.NewInt(0))},
				},
				Calls: []TransactionArgs{
					{
						From:  &randomAccounts[1].addr,
						To:    &randomAccounts[2].addr,
						Value: (*hexutil.Big)(big.NewInt(1000)),
					},
				},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xa410",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[0].addr.Bytes()),
								common.BytesToHash(randomAccounts[1].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, 0xd0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8},
							BlockNumber:    11,
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        0,
							Index:          0,
						},
					},
					Status: "0x1",
				}, {
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[0].addr.Bytes()),
								common.BytesToHash(randomAccounts[3].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8},
							BlockNumber:    11,
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        1,
							Index:          1,
						},
					},
					Status: "0x1",
				}},
			}, {
				Number:        "0xc",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x5208",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[1].addr.Bytes()),
								common.BytesToHash(randomAccounts[2].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8},
							BlockNumber:    12,
							BlockTimestamp: hexutil.Uint64(0x7c),
							TxIndex:        0,
							Index:          0,
						},
					},
					Status: "0x1",
				}},
			}},
		}, {
			// insufficient funds
			name: "insufficient-funds",
			tag:  latest,
			blocks: []simBlock{{
				Calls: []TransactionArgs{{
					From:  &randomAccounts[0].addr,
					To:    &randomAccounts[1].addr,
					Value: (*hexutil.Big)(big.NewInt(1000)),
				}},
			}},
			want:      nil,
			expectErr: &invalidTxError{Message: fmt.Sprintf("err: insufficient funds for gas * price + value: address %s have 0 want 1000 (supplied gas 4712388)", randomAccounts[0].addr.String()), Code: errCodeInsufficientFunds},
		}, {
			// EVM error
			name: "evm-error",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{Code: hex2Bytes("f3")},
				},
				Calls: []TransactionArgs{{
					From: &randomAccounts[0].addr,
					To:   &randomAccounts[2].addr,
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x47e7c4",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					Error:       callErr{Message: "stack underflow (0 <=> 2)", Code: errCodeVMError},
					GasUsed:     "0x47e7c4",
					Logs:        []log{},
					Status:      "0x0",
				}},
			}},
		}, {
			// Block overrides should work, each call is simulated on a different block number
			name: "block-overrides",
			tag:  latest,
			blocks: []simBlock{{
				BlockOverrides: &override.BlockOverrides{
					Number:       (*hexutil.Big)(big.NewInt(11)),
					FeeRecipient: &cac,
				},
				Calls: []TransactionArgs{
					{
						From: &accounts[0].addr,
						Input: &hexutil.Bytes{
							0x43,             // NUMBER
							0x60, 0x00, 0x52, // MSTORE offset 0
							0x60, 0x20, 0x60, 0x00, 0xf3, // RETURN
						},
					},
				},
			}, {
				BlockOverrides: &override.BlockOverrides{
					Number: (*hexutil.Big)(big.NewInt(12)),
				},
				Calls: []TransactionArgs{{
					From: &accounts[1].addr,
					Input: &hexutil.Bytes{
						0x43,             // NUMBER
						0x60, 0x00, 0x52, // MSTORE offset 0
						0x60, 0x20, 0x60, 0x00, 0xf3,
					},
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xe891",
				Miner:         strings.ToLower(cac.String()),
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x000000000000000000000000000000000000000000000000000000000000000b",
					GasUsed:     "0xe891",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}, {
				Number:        "0xc",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xe891",
				Miner:         strings.ToLower(cac.String()),
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x000000000000000000000000000000000000000000000000000000000000000c",
					GasUsed:     "0xe891",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		// Block numbers must be in order.
		{
			name: "block-number-order",
			tag:  latest,
			blocks: []simBlock{{
				BlockOverrides: &override.BlockOverrides{
					Number: (*hexutil.Big)(big.NewInt(12)),
				},
				Calls: []TransactionArgs{{
					From: &accounts[1].addr,
					Input: &hexutil.Bytes{
						0x43,             // NUMBER
						0x60, 0x00, 0x52, // MSTORE offset 0
						0x60, 0x20, 0x60, 0x00, 0xf3, // RETURN
					},
				}},
			}, {
				BlockOverrides: &override.BlockOverrides{
					Number: (*hexutil.Big)(big.NewInt(11)),
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					Input: &hexutil.Bytes{
						0x43,             // NUMBER
						0x60, 0x00, 0x52, // MSTORE offset 0
						0x60, 0x20, 0x60, 0x00, 0xf3, // RETURN
					},
				}},
			}},
			want:      []blockRes{},
			expectErr: &invalidBlockNumberError{message: "block numbers must be in order: 11 <= 12"},
		},
		// Test on solidity storage example. Set value in one call, read in next.
		{
			name: "storage-contract",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						Code: hex2Bytes("608060405234801561001057600080fd5b50600436106100365760003560e01c80632e64cec11461003b5780636057361d14610059575b600080fd5b610043610075565b60405161005091906100d9565b60405180910390f35b610073600480360381019061006e919061009d565b61007e565b005b60008054905090565b8060008190555050565b60008135905061009781610103565b92915050565b6000602082840312156100b3576100b26100fe565b5b60006100c184828501610088565b91505092915050565b6100d3816100f4565b82525050565b60006020820190506100ee60008301846100ca565b92915050565b6000819050919050565b600080fd5b61010c816100f4565b811461011757600080fd5b5056fea2646970667358221220404e37f487a89a932dca5e77faaf6ca2de3b991f93d230604b1b8daaef64766264736f6c63430008070033"),
					},
				},
				Calls: []TransactionArgs{{
					// Set value to 5
					From:  &randomAccounts[0].addr,
					To:    &randomAccounts[2].addr,
					Input: hex2Bytes("6057361d0000000000000000000000000000000000000000000000000000000000000005"),
				}, {
					// Read value
					From:  &randomAccounts[0].addr,
					To:    &randomAccounts[2].addr,
					Input: hex2Bytes("2e64cec1"),
				},
				},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x10683",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0xaacc",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x0000000000000000000000000000000000000000000000000000000000000005",
					GasUsed:     "0x5bb7",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		// Test logs output.
		{
			name: "logs",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						// Yul code:
						// object "Test" {
						//    code {
						//        let hash:u256 := 0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
						//        log1(0, 0, hash)
						//        return (0, 0)
						//    }
						// }
						Code: hex2Bytes("7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff80600080a1600080f3"),
					},
				},
				Calls: []TransactionArgs{{
					From: &randomAccounts[0].addr,
					To:   &randomAccounts[2].addr,
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x5508",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					Logs: []log{{
						Address:        randomAccounts[2].addr,
						Topics:         []common.Hash{common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")},
						BlockNumber:    hexutil.Uint64(11),
						BlockTimestamp: hexutil.Uint64(0x70),
						Data:           hexutil.Bytes{},
					}},
					GasUsed: "0x5508",
					Status:  "0x1",
				}},
			}},
		},
		// Test ecrecover override
		{
			name: "ecrecover-override",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						// Yul code that returns ecrecover(0, 0, 0, 0).
						// object "Test" {
						//    code {
						//        // Free memory pointer
						//        let free_ptr := mload(0x40)
						//
						//        // Initialize inputs with zeros
						//        mstore(free_ptr, 0)  // Hash
						//        mstore(add(free_ptr, 0x20), 0)  // v
						//        mstore(add(free_ptr, 0x40), 0)  // r
						//        mstore(add(free_ptr, 0x60), 0)  // s
						//
						//        // Call ecrecover precompile (at address 1) with all 0 inputs
						//        let success := staticcall(gas(), 1, free_ptr, 0x80, free_ptr, 0x20)
						//
						//        // Check if the call was successful
						//        if eq(success, 0) {
						//            revert(0, 0)
						//        }
						//
						//        // Return the recovered address
						//        return(free_ptr, 0x14)
						//    }
						// }
						Code: hex2Bytes("6040516000815260006020820152600060408201526000606082015260208160808360015afa60008103603157600080fd5b601482f3"),
					},
					common.BytesToAddress([]byte{0x01}): override.OverrideAccount{
						// Yul code that returns the address of the caller.
						// object "Test" {
						//    code {
						//        let c := caller()
						//        mstore(0, c)
						//        return(0xc, 0x14)
						//    }
						// }
						Code: hex2Bytes("33806000526014600cf3"),
					},
				},
				Calls: []TransactionArgs{{
					From: &randomAccounts[0].addr,
					To:   &randomAccounts[2].addr,
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x52f6",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					// Caller is in this case the contract that invokes ecrecover.
					ReturnValue: strings.ToLower(randomAccounts[2].addr.String()),
					GasUsed:     "0x52f6",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		// Test moving the sha256 precompile.
		{
			name: "precompile-move",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					sha256Address: override.OverrideAccount{
						// Yul code that returns the calldata.
						// object "Test" {
						//    code {
						//        let size := calldatasize() // Get the size of the calldata
						//
						//        // Allocate memory to store the calldata
						//        let memPtr := msize()
						//
						//        // Copy calldata to memory
						//        calldatacopy(memPtr, 0, size)
						//
						//        // Return the calldata from memory
						//        return(memPtr, size)
						//    }
						// }
						Code:             hex2Bytes("365981600082378181f3"),
						MovePrecompileTo: &randomAccounts[2].addr,
					},
				},
				Calls: []TransactionArgs{{
					From:  &randomAccounts[0].addr,
					To:    &randomAccounts[2].addr,
					Input: hex2Bytes("0000000000000000000000000000000000000000000000000000000000000001"),
				}, {
					From:  &randomAccounts[0].addr,
					To:    &sha256Address,
					Input: hex2Bytes("0000000000000000000000000000000000000000000000000000000000000001"),
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xa58c",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0xec4916dd28fc4c10d78e287ca5d9cc51ee1ae73cbfde08c6b37324cbfaac8bc5",
					GasUsed:     "0x52dc",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x0000000000000000000000000000000000000000000000000000000000000001",
					GasUsed:     "0x52b0",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		// Test ether transfers.
		{
			name: "transfer-logs",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{
						Balance: newRPCBalance(big.NewInt(100)),
						// Yul code that transfers 100 wei to address passed in calldata:
						// object "Test" {
						//    code {
						//        let recipient := shr(96, calldataload(0))
						//        let value := 100
						//        let success := call(gas(), recipient, value, 0, 0, 0, 0)
						//        if eq(success, 0) {
						//            revert(0, 0)
						//        }
						//    }
						// }
						Code: hex2Bytes("60003560601c606460008060008084865af160008103601d57600080fd5b505050"),
					},
				},
				Calls: []TransactionArgs{{
					From:  &accounts[0].addr,
					To:    &randomAccounts[0].addr,
					Value: (*hexutil.Big)(big.NewInt(50)),
					Input: hex2Bytes(strings.TrimPrefix(fixedAccount.addr.String(), "0x")),
				}},
			}},
			includeTransfers: &includeTransfers,
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x77dc",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x77dc",
					Logs: []log{
						{
							Address: transferAddress,
							Topics: []common.Hash{
								transferTopic,
								addressToHash(accounts[0].addr),
								addressToHash(randomAccounts[0].addr),
							},
							Data:           hexutil.Bytes(common.BigToHash(big.NewInt(50)).Bytes()),
							BlockNumber:    hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
						},
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(accounts[0].addr.Bytes()),
								common.BytesToHash(randomAccounts[0].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x77, 0x0d, 0x88, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x77, 0x0d, 0x56, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x96},
							BlockNumber:    hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
							Index:          hexutil.Uint(1),
						},
						{
							Address: transferAddress,
							Topics: []common.Hash{
								transferTopic,
								addressToHash(randomAccounts[0].addr),
								addressToHash(fixedAccount.addr),
							},
							Data:           hexutil.Bytes(common.BigToHash(big.NewInt(100)).Bytes()),
							BlockNumber:    hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
							Index:          hexutil.Uint(2),
						},
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[0].addr.Bytes()),
								common.BytesToHash(fixedAccount.addr.Bytes()),
							},
							Data: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x96, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0xb6, 0xb3, 0xa7, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0xb6, 0xb3, 0xa7, 0x64, 0x00, 0x64}, BlockNumber: hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        0,
							Index:          3,
						},
					},
					Status: "0x1",
				}},
			}},
		},
		// Tests selfdestructed contract.
		{
			name: "selfdestruct",
			tag:  latest,
			blocks: []simBlock{{
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &cac,
				}, {
					From: &accounts[0].addr,
					// Check that cac is selfdestructed and balance transferred to dad.
					// object "Test" {
					//    code {
					//        let cac := 0x0000000000000000000000000000000000000cac
					//        let dad := 0x0000000000000000000000000000000000000dad
					//        if gt(balance(cac), 0) {
					//            revert(0, 0)
					//        }
					//        if gt(extcodesize(cac), 0) {
					//            revert(0, 0)
					//        }
					//        if eq(balance(dad), 0) {
					//            revert(0, 0)
					//        }
					//    }
					// }
					Input: hex2Bytes("610cac610dad600082311115601357600080fd5b6000823b1115602157600080fd5b6000813103602e57600080fd5b5050"),
				}},
			}, {
				Calls: []TransactionArgs{{
					From:  &accounts[0].addr,
					Input: hex2Bytes("610cac610dad600082311115601357600080fd5b6000823b1115602157600080fd5b6000813103602e57600080fd5b5050"),
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x1b83f",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0xd166",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x",
					GasUsed:     "0xe6d9",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}, {
				Number:        "0xc",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xe6d9",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0xe6d9",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		// Enable validation checks.
		{
			name: "validation-checks",
			tag:  latest,
			blocks: []simBlock{{
				Calls: []TransactionArgs{{
					From:  &accounts[2].addr,
					To:    &cac,
					Nonce: newUint64(2),
				}},
			}},
			validation: &validation,
			want:       nil,
			expectErr:  &invalidTxError{Message: fmt.Sprintf("err: nonce too high: address %s, tx: 2 state: 0 (supplied gas 4712388)", accounts[2].addr), Code: errCodeNonceTooHigh},
		},
		// Contract sends tx in validation mode.
		{
			name: "validation-checks-from-contract",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						Balance: newRPCBalance(big.NewInt(2098640803896784)),
						Code:    hex2Bytes("00"),
						Nonce:   newUint64(1),
					},
				},
				Calls: []TransactionArgs{{
					From:                 &randomAccounts[2].addr,
					To:                   &cac,
					Nonce:                newUint64(1),
					MaxFeePerGas:         newInt(233138868),
					MaxPriorityFeePerGas: newInt(1),
				}},
			}},
			validation: &validation,
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xd166",
				Miner:         coinbase,
				BaseFeePerGas: "0xde56ab3",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0xd166",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{0x4d, 0xfe, 0x1b, 0xbb, 0xcf, 0x07, 0x7d, 0xdc, 0x3e, 0x01, 0x29, 0x1e, 0xea, 0x2d, 0x5c, 0x70, 0xc2, 0xb4, 0x22, 0xb4, 0x15, 0xd9, 0x56, 0x45, 0xb9, 0xad, 0xcf, 0xd6, 0x78, 0xcb, 0x1d, 0x63}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[2].addr.Bytes()),
								common.BytesToHash(common.HexToAddress("0x000000000000000000000000000000000000ffff").Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xd1, 0x66, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, 0x74, 0xb3, 0xe3, 0xa0, 0x9d, 0xd0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x15, 0x8e, 0x46, 0x09, 0x13, 0xd0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, 0x74, 0xb3, 0xe3, 0x9f, 0xcc, 0x6a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x15, 0x8e, 0x46, 0x09, 0x13, 0xd0, 0xd1, 0x66},
							BlockNumber:    hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        0,
							Index:          0,
						},
					},
					Status: "0x1",
				}},
			}},
		},
		// Successful validation
		{
			name: "validation-checks-success",
			tag:  latest,
			blocks: []simBlock{{
				BlockOverrides: &override.BlockOverrides{
					BaseFeePerGas: (*hexutil.Big)(big.NewInt(1)),
				},
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(big.NewInt(10000000))},
				},
				Calls: []TransactionArgs{{
					From:         &randomAccounts[0].addr,
					To:           &randomAccounts[1].addr,
					Value:        (*hexutil.Big)(big.NewInt(1000)),
					MaxFeePerGas: (*hexutil.Big)(big.NewInt(2)),
				}},
			}},
			validation: &validation,
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x5208",
				Miner:         coinbase,
				BaseFeePerGas: "0x1",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x5208",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{
									230, 73, 126, 62, 229, 72, 163, 55, 33, 54, 175, 47, 203, 6, 150, 219,
									49, 252, 108, 242, 2, 96, 112, 118, 69, 6, 139, 211, 254, 151, 243, 196,
								}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(randomAccounts[0].addr.Bytes()),
								common.BytesToHash(randomAccounts[1].addr.Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x50, 0xae, 0xbc, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x50, 0xaa, 0xd4, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xe8},
							BlockNumber:    hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        0,
							Index:          0,
						},
					},
					Status: "0x1",
				}},
			}},
		},
		// Clear storage.
		{
			name: "clear-storage",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: {
						Code: newBytes(genesis.Alloc[bab].Code),
						StateDiff: map[common.Hash]common.Hash{
							common.BigToHash(big.NewInt(1)): common.BigToHash(big.NewInt(2)),
							common.BigToHash(big.NewInt(2)): common.BigToHash(big.NewInt(3)),
						},
					},
					bab: {
						State: map[common.Hash]common.Hash{
							common.BigToHash(big.NewInt(1)): common.BigToHash(big.NewInt(1)),
						},
					},
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
				}, {
					From: &accounts[0].addr,
					To:   &bab,
				}},
			}, {
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: {
						State: map[common.Hash]common.Hash{
							common.BigToHash(big.NewInt(1)): common.BigToHash(big.NewInt(5)),
						},
					},
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xc542",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x0000000000000000000000000000000200000000000000000000000000000003",
					GasUsed:     "0x62a1",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x0000000000000000000000000000000100000000000000000000000000000000",
					GasUsed:     "0x62a1",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}, {
				Number:        "0xc",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x62a1",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x0000000000000000000000000000000500000000000000000000000000000000",
					GasUsed:     "0x62a1",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		{
			name: "blockhash-opcode",
			tag:  latest,
			blocks: []simBlock{{
				BlockOverrides: &override.BlockOverrides{
					Number: (*hexutil.Big)(big.NewInt(12)),
				},
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: {
						Code: hex2Bytes("600035804060008103601057600080fd5b5050"),
					},
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// Phantom block after base.
					Input: uint256ToBytes(uint256.NewInt(11)),
				}, {
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// Canonical block.
					Input: uint256ToBytes(uint256.NewInt(8)),
				}, {
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// base block.
					Input: uint256ToBytes(uint256.NewInt(10)),
				}},
			}, {
				BlockOverrides: &override.BlockOverrides{
					Number: (*hexutil.Big)(big.NewInt(16)),
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// blocks[0]
					Input: uint256ToBytes(uint256.NewInt(12)),
				}, {
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// Phantom after blocks[0]
					Input: uint256ToBytes(uint256.NewInt(13)),
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x0",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls:         []callRes{},
			}, {
				Number:        "0xc",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xf864",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x52cc",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x",
					GasUsed:     "0x52cc",
					Logs:        []log{},
					Status:      "0x1",
				}, {

					ReturnValue: "0x",
					GasUsed:     "0x52cc",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}, {
				Number:        "0xd",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x0",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls:         []callRes{},
			}, {
				Number:        "0xe",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x0",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls:         []callRes{},
			}, {
				Number:        "0xf",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x0",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls:         []callRes{},
			}, {
				Number:        "0x10",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xa598",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x",
					GasUsed:     "0x52cc",
					Logs:        []log{},
					Status:      "0x1",
				}, {

					ReturnValue: "0x",
					GasUsed:     "0x52cc",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		{
			name: "basefee-non-validation",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: {
						// Yul code:
						// object "Test" {
						//    code {
						//        // Get the gas price from the transaction
						//        let gasPrice := gasprice()
						//
						//        // Get the base fee from the block
						//        let baseFee := basefee()
						//
						//        // Store gasPrice and baseFee in memory
						//        mstore(0x0, gasPrice)
						//        mstore(0x20, baseFee)
						//
						//        // Return the data
						//        return(0x0, 0x40)
						//    }
						// }
						Code: hex2Bytes("3a489060005260205260406000f3"),
					},
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// 0 gas price
				}, {
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// non-zero gas price
					MaxPriorityFeePerGas: newInt(1),
					MaxFeePerGas:         newInt(2),
				},
				},
			}, {
				BlockOverrides: &override.BlockOverrides{
					BaseFeePerGas: (*hexutil.Big)(big.NewInt(1)),
				},
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// 0 gas price
				}, {
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
					// non-zero gas price
					MaxPriorityFeePerGas: newInt(1),
					MaxFeePerGas:         newInt(2),
				},
				},
			}, {
				// Base fee should be 0 to zero even if it was set in previous block.
				Calls: []TransactionArgs{{
					From: &accounts[0].addr,
					To:   &randomAccounts[2].addr,
				}},
			}},
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xa44e",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
					GasUsed:     "0x5227",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x00000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000",
					GasUsed:     "0x5227",
					Logs: []log{{
						Address: core.GetFeeAddress(),
						Topics: []common.Hash{
							common.Hash([32]byte{0x4d, 0xfe, 0x1b, 0xbb, 0xcf, 0x07, 0x7d, 0xdc, 0x3e, 0x01, 0x29, 0x1e, 0xea, 0x2d, 0x5c, 0x70, 0xc2, 0xb4, 0x22, 0xb4, 0x15, 0xd9, 0x56, 0x45, 0xb9, 0xad, 0xcf, 0xd6, 0x78, 0xcb, 0x1d, 0x63}),
							common.BytesToHash(core.GetFeeAddress().Bytes()),
							common.BytesToHash(accounts[0].addr.Bytes()),
							common.BytesToHash(common.HexToAddress("0x000000000000000000000000000000000000ffff").Bytes()),
						},
						Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x52, 0x27, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x77, 0x0d, 0x88, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x15, 0x8e, 0x46, 0x09, 0x13, 0xd0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x76, 0xbb, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x15, 0x8e, 0x46, 0x09, 0x13, 0xd0, 0x52, 0x27},
						BlockNumber:    hexutil.Uint64(11),
						BlockTimestamp: hexutil.Uint64(0x70),
						TxIndex:        1,
						Index:          0,
					},
					},
					Status: "0x1",
				}},
			}, {
				Number:        "0xc",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0xa44e",
				Miner:         coinbase,
				BaseFeePerGas: "0x1",
				Calls: []callRes{{
					ReturnValue: "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001",
					GasUsed:     "0x5227",
					Logs:        []log{},
					Status:      "0x1",
				}, {
					ReturnValue: "0x00000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000001",
					GasUsed:     "0x5227",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{0x4d, 0xfe, 0x1b, 0xbb, 0xcf, 0x07, 0x7d, 0xdc, 0x3e, 0x01, 0x29, 0x1e, 0xea, 0x2d, 0x5c, 0x70, 0xc2, 0xb4, 0x22, 0xb4, 0x15, 0xd9, 0x56, 0x45, 0xb9, 0xad, 0xcf, 0xd6, 0x78, 0xcb, 0x1d, 0x63}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(accounts[0].addr.Bytes()),
								common.BytesToHash(common.HexToAddress("0x000000000000000000000000000000000000ffff").Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x52, 0x27, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x76, 0xbb, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x31, 0x4f, 0xb3, 0x70, 0x62, 0x98, 0xa4, 0x4e, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x76, 0x69, 0x3a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x31, 0x4f, 0xb3, 0x70, 0x62, 0x98, 0xf6, 0x75},
							BlockNumber:    hexutil.Uint64(12),
							BlockTimestamp: hexutil.Uint64(0x7c),
							TxIndex:        1,
							Index:          0,
						},
					},
					Status: "0x1",
				}},
			}, {
				Number:        "0xd",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x5227",
				Miner:         coinbase,
				BaseFeePerGas: "0x0",
				Calls: []callRes{{
					ReturnValue: "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
					GasUsed:     "0x5227",
					Logs:        []log{},
					Status:      "0x1",
				}},
			}},
		},
		{
			name: "basefee-validation-mode",
			tag:  latest,
			blocks: []simBlock{{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: {
						// Yul code:
						// object "Test" {
						//    code {
						//        // Get the gas price from the transaction
						//        let gasPrice := gasprice()
						//
						//        // Get the base fee from the block
						//        let baseFee := basefee()
						//
						//        // Store gasPrice and baseFee in memory
						//        mstore(0x0, gasPrice)
						//        mstore(0x20, baseFee)
						//
						//        // Return the data
						//        return(0x0, 0x40)
						//    }
						// }
						Code: hex2Bytes("3a489060005260205260406000f3"),
					},
				},
				Calls: []TransactionArgs{{
					From:                 &accounts[0].addr,
					To:                   &randomAccounts[2].addr,
					MaxFeePerGas:         newInt(233138868),
					MaxPriorityFeePerGas: newInt(1),
				}},
			}},
			validation: &validation,
			want: []blockRes{{
				Number:        "0xb",
				GasLimit:      "0x47e7c4",
				GasUsed:       "0x5227",
				Miner:         coinbase,
				BaseFeePerGas: "0xde56ab3",
				Calls: []callRes{{
					ReturnValue: "0x000000000000000000000000000000000000000000000000000000000de56ab4000000000000000000000000000000000000000000000000000000000de56ab3",
					GasUsed:     "0x5227",
					Logs: []log{
						{
							Address: core.GetFeeAddress(),
							Topics: []common.Hash{
								common.Hash([32]byte{0x4d, 0xfe, 0x1b, 0xbb, 0xcf, 0x07, 0x7d, 0xdc, 0x3e, 0x01, 0x29, 0x1e, 0xea, 0x2d, 0x5c, 0x70, 0xc2, 0xb4, 0x22, 0xb4, 0x15, 0xd9, 0x56, 0x45, 0xb9, 0xad, 0xcf, 0xd6, 0x78, 0xcb, 0x1d, 0x63}),
								common.BytesToHash(core.GetFeeAddress().Bytes()),
								common.BytesToHash(accounts[0].addr.Bytes()),
								common.BytesToHash(common.HexToAddress("0x000000000000000000000000000000000000ffff").Bytes()),
							},
							Data:           []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x52, 0x27, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x77, 0x0d, 0x88, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x15, 0x8e, 0x46, 0x09, 0x13, 0xd0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0d, 0xe0, 0x53, 0xbf, 0x1f, 0x76, 0xbb, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x15, 0x8e, 0x46, 0x09, 0x13, 0xd0, 0x52, 0x27},
							BlockNumber:    hexutil.Uint64(11),
							BlockTimestamp: hexutil.Uint64(0x70),
							TxIndex:        0,
							Index:          0,
						},
					},
					Status: "0x1",
				}},
			}},
		},
	}

	for _, tc := range testSuite {
		t.Run(tc.name, func(t *testing.T) {
			opts := simOpts{BlockStateCalls: tc.blocks}
			if tc.includeTransfers != nil && *tc.includeTransfers {
				opts.TraceTransfers = true
			}
			if tc.validation != nil && *tc.validation {
				opts.Validation = true
			}
			result, err := api.SimulateV1(t.Context(), opts, &tc.tag)
			if tc.expectErr != nil {
				if err == nil {
					t.Fatalf("test %s: want error %v, have nothing", tc.name, tc.expectErr)
				}
				if !errors.Is(err, tc.expectErr) {
					// Second try
					if !reflect.DeepEqual(err, tc.expectErr) {
						t.Errorf("test %s: error mismatch, want %v, have %v", tc.name, tc.expectErr, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("test %s: want no error, have %v", tc.name, err)
			}
			// Turn result into res-struct
			var have []blockRes
			resBytes, _ := json.Marshal(result)
			if err := json.Unmarshal(resBytes, &have); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}
			if !reflect.DeepEqual(have, tc.want) {
				t.Log(string(resBytes))
				t.Errorf("test %s, result mismatch, have\n%v\n, want\n%v\n", tc.name, have, tc.want)
			}
		})
	}
}

func TestSimulateV1ChainLinkage(t *testing.T) {
	t.Skip("bor: skipping because we use total difficulty and beacon expects 0")
	var (
		acc          = newTestAccount()
		sender       = acc.addr
		contractAddr = common.Address{0xaa, 0xaa}
		recipient    = common.Address{0xbb, 0xbb}
		gspec        = &core.Genesis{
			Config: params.MergedTestChainConfig,
			Alloc: types.GenesisAlloc{
				sender:       {Balance: big.NewInt(params.Ether)},
				contractAddr: {Code: common.Hex2Bytes("5f35405f8114600f575f5260205ff35b5f80fd")},
			},
		}
		signer = types.LatestSigner(params.MergedTestChainConfig)
	)
	backend := newTestBackend(t, 1, gspec, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		tx := types.MustSignNewTx(acc.key, signer, &types.LegacyTx{
			Nonce:    uint64(i),
			GasPrice: b.BaseFee(),
			Gas:      params.TxGas,
			To:       &recipient,
			Value:    big.NewInt(500),
		})
		b.AddTx(tx)
	})

	ctx := t.Context()
	stateDB, baseHeader, err := backend.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
	if err != nil {
		t.Fatalf("failed to get state and header: %v", err)
	}

	sim := &simulator{
		b:              backend,
		state:          stateDB,
		base:           baseHeader,
		chainConfig:    backend.ChainConfig(),
		budget:         newGasBudget(0),
		traceTransfers: false,
		validate:       false,
		fullTx:         false,
	}

	var (
		call1 = TransactionArgs{
			From:  &sender,
			To:    &recipient,
			Value: (*hexutil.Big)(big.NewInt(1000)),
		}
		call2 = TransactionArgs{
			From:  &sender,
			To:    &recipient,
			Value: (*hexutil.Big)(big.NewInt(2000)),
		}
		call3a = TransactionArgs{
			From:  &sender,
			To:    &contractAddr,
			Input: uint256ToBytes(uint256.NewInt(baseHeader.Number.Uint64() + 1)),
			Gas:   newUint64(1000000),
		}
		call3b = TransactionArgs{
			From:  &sender,
			To:    &contractAddr,
			Input: uint256ToBytes(uint256.NewInt(baseHeader.Number.Uint64() + 2)),
			Gas:   newUint64(1000000),
		}
		blocks = []simBlock{
			{Calls: []TransactionArgs{call1}},
			{Calls: []TransactionArgs{call2}},
			{Calls: []TransactionArgs{call3a, call3b}},
		}
	)

	results, err := sim.execute(ctx, blocks)
	if err != nil {
		t.Fatalf("simulation execution failed: %v", err)
	}
	require.Equal(t, 3, len(results), "expected 3 simulated blocks")

	// Check linkages of simulated blocks:
	// Verify that block2's parent hash equals block1's hash.
	block1 := results[0].Block
	block2 := results[1].Block
	block3 := results[2].Block
	require.Equal(t, block1.ParentHash(), baseHeader.Hash(), "parent hash of block1 should equal hash of base block")
	require.Equal(t, block1.Hash(), block2.Header().ParentHash, "parent hash of block2 should equal hash of block1")
	require.Equal(t, block2.Hash(), block3.Header().ParentHash, "parent hash of block3 should equal hash of block2")

	// In block3, two calls were executed to our contract.
	// The first call in block3 should return the blockhash for block1 (i.e. block1.Hash()),
	// whereas the second call should return the blockhash for block2 (i.e. block2.Hash()).
	require.Equal(t, block1.Hash().Bytes(), []byte(results[2].Calls[0].ReturnValue), "returned blockhash for block1 does not match")
	require.Equal(t, block2.Hash().Bytes(), []byte(results[2].Calls[1].ReturnValue), "returned blockhash for block2 does not match")
}

func TestSimulateV1TxSender(t *testing.T) {
	var (
		sender    = common.Address{0xaa, 0xaa}
		sender2   = common.Address{0xaa, 0xab}
		sender3   = common.Address{0xaa, 0xac}
		recipient = common.Address{0xbb, 0xbb}
		gspec     = &core.Genesis{
			Config: params.MergedTestChainConfig,
			Alloc: types.GenesisAlloc{
				sender:  {Balance: big.NewInt(params.Ether)},
				sender2: {Balance: big.NewInt(params.Ether)},
				sender3: {Balance: big.NewInt(params.Ether)},
			},
		}
		ctx = t.Context()
	)
	backend := newTestBackend(t, 0, gspec, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {})
	stateDB, baseHeader, err := backend.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
	if err != nil {
		t.Fatalf("failed to get state and header: %v", err)
	}

	sim := &simulator{
		b:              backend,
		state:          stateDB,
		base:           baseHeader,
		chainConfig:    backend.ChainConfig(),
		budget:         newGasBudget(0),
		traceTransfers: false,
		validate:       false,
		fullTx:         true,
	}

	results, err := sim.execute(ctx, []simBlock{
		{Calls: []TransactionArgs{
			{From: &sender, To: &recipient, Value: (*hexutil.Big)(big.NewInt(1000))},
			{From: &sender2, To: &recipient, Value: (*hexutil.Big)(big.NewInt(2000))},
			{From: &sender3, To: &recipient, Value: (*hexutil.Big)(big.NewInt(3000))},
		}},
		{Calls: []TransactionArgs{
			{From: &sender2, To: &recipient, Value: (*hexutil.Big)(big.NewInt(4000))},
		}},
	})
	if err != nil {
		t.Fatalf("simulation execution failed: %v", err)
	}
	require.Len(t, results, 2, "expected 2 simulated blocks")
	require.Len(t, results[0].Block.Transactions(), 3, "expected 3 transaction in simulated block")
	require.Len(t, results[1].Block.Transactions(), 1, "expected 1 transaction in 2nd simulated block")
	enc, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("failed to marshal results: %v", err)
	}
	type resultType struct {
		Transactions []struct {
			From common.Address `json:"from"`
		}
	}
	var summary []resultType
	if err := json.Unmarshal(enc, &summary); err != nil {
		t.Fatalf("failed to unmarshal results: %v", err)
	}
	require.Len(t, summary, 2, "expected 2 simulated blocks")
	require.Len(t, summary[0].Transactions, 3, "expected 3 transaction in simulated block")
	require.Equal(t, sender, summary[0].Transactions[0].From, "sender address mismatch")
	require.Equal(t, sender2, summary[0].Transactions[1].From, "sender address mismatch")
	require.Equal(t, sender3, summary[0].Transactions[2].From, "sender address mismatch")
	require.Len(t, summary[1].Transactions, 1, "expected 1 transaction in simulated block")
	require.Equal(t, sender2, summary[1].Transactions[0].From, "sender address mismatch")
}

func TestSignTransaction(t *testing.T) {
	t.Parallel()
	// Initialize test accounts
	var (
		key, _  = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		to      = crypto.PubkeyToAddress(key.PublicKey)
		genesis = &core.Genesis{
			Config: params.MergedTestChainConfig,
			Alloc:  types.GenesisAlloc{},
		}
	)
	_, acc := newTestAccountManager(t)
	genesis.Alloc[acc.Address] = types.Account{Balance: big.NewInt(params.Ether)}
	b := newTestBackend(t, 1, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		b.SetPoS()
	})
	api := NewTransactionAPI(b, nil)
	res, err := api.FillTransaction(t.Context(), TransactionArgs{
		From:  &b.acc.Address,
		To:    &to,
		Value: (*hexutil.Big)(big.NewInt(1)),
	})
	if err != nil {
		t.Fatalf("failed to fill tx defaults: %v\n", err)
	}

	res, err = api.SignTransaction(t.Context(), argsFromTransaction(res.Tx, b.acc.Address))
	if err != nil {
		t.Fatalf("failed to sign tx: %v\n", err)
	}
	tx, err := json.Marshal(res.Tx)
	if err != nil {
		t.Fatal(err)
	}
	expect := `{"type":"0x2","chainId":"0x539","nonce":"0x0","to":"0x703c4b2bd70c169f5717101caee543299fc946c7","gas":"0x5208","gasPrice":null,"maxPriorityFeePerGas":"0x0","maxFeePerGas":"0x7558bdb0","value":"0x1","input":"0x","accessList":[],"v":"0x0","r":"0xb0bbf33a3acf058714ac1a21edf4f3fb5634ee948b04399556fd3c8f28fb580f","s":"0x3d47400704ef822792cb2181b745ca962772a4c9e5dcd31024029a6889552c9f","yParity":"0x0","hash":"0xd748a0bf422d48f98e82633391438302f68f8567571ed1d258d1d6b5a3a626d1"}`
	if !bytes.Equal(tx, []byte(expect)) {
		t.Errorf("result mismatch. Have:\n%s\nWant:\n%s\n", tx, expect)
	}
}

// func TestSignBlobTransaction(t *testing.T) {
// 	t.Parallel()
// 	// Initialize test accounts
// 	var (
// 		key, _  = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
// 		to      = crypto.PubkeyToAddress(key.PublicKey)
// 		genesis = &core.Genesis{
// 			Config: params.MergedTestChainConfig,
// 			Alloc:  types.GenesisAlloc{},
// 		}
// 	)
// 	b := newTestBackend(t, 1, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
// 		b.SetPoS()
// 	})
// 	api := NewTransactionAPI(b, nil)
// 	res, err := api.FillTransaction(t.Context(), TransactionArgs{
// 		From:       &b.acc.Address,
// 		To:         &to,
// 		Value:      (*hexutil.Big)(big.NewInt(1)),
// 		BlobHashes: []common.Hash{{0x01, 0x22}},
// 	})
// 	if err != nil {
// 		t.Fatalf("failed to fill tx defaults: %v\n", err)
// 	}

// 	_, err = api.SignTransaction(t.Context(), argsFromTransaction(res.Tx, b.acc.Address))
// 	if err != nil {
// 		t.Fatalf("should not fail on blob transaction")
// 	}
// }

// func TestSendBlobTransaction(t *testing.T) {
// 	t.Parallel()
// 	// Initialize test accounts
// 	var (
// 		key, _  = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
// 		to      = crypto.PubkeyToAddress(key.PublicKey)
// 		genesis = &core.Genesis{
// 			Config: params.MergedTestChainConfig,
// 			Alloc:  types.GenesisAlloc{},
// 		}
// 	)
// 	b := newTestBackend(t, 1, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
// 		b.SetPoS()
// 	})
// 	api := NewTransactionAPI(b, nil)
// 	res, err := api.FillTransaction(t.Context(), TransactionArgs{
// 		From:       &b.acc.Address,
// 		To:         &to,
// 		Value:      (*hexutil.Big)(big.NewInt(1)),
// 		BlobHashes: []common.Hash{{0x01, 0x22}},
// 	})
// 	if err != nil {
// 		t.Fatalf("failed to fill tx defaults: %v\n", err)
// 	}

// 	_, err = api.SendTransaction(t.Context(), argsFromTransaction(res.Tx, b.acc.Address))
// 	if err == nil {
// 		t.Errorf("sending tx should have failed")
// 	} else if !errors.Is(err, errBlobTxNotSupported) {
// 		t.Errorf("unexpected error. Have %v, want %v\n", err, errBlobTxNotSupported)
// 	}
// }

// func TestFillBlobTransaction(t *testing.T) {
// 	t.Parallel()
// 	// Initialize test accounts
// 	var (
// 		key, _  = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
// 		to      = crypto.PubkeyToAddress(key.PublicKey)
// 		genesis = &core.Genesis{
// 			Config: params.MergedTestChainConfig,
// 			Alloc:  types.GenesisAlloc{},
// 		}
// 		emptyBlob                      = new(kzg4844.Blob)
// 		emptyBlobs                     = []kzg4844.Blob{*emptyBlob}
// 		emptyBlobCommit, _             = kzg4844.BlobToCommitment(emptyBlob)
// 		emptyBlobProof, _              = kzg4844.ComputeBlobProof(emptyBlob, emptyBlobCommit)
// 		emptyBlobHash      common.Hash = kzg4844.CalcBlobHashV1(sha256.New(), &emptyBlobCommit)
// 	)
// 	b := newTestBackend(t, 1, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
// 		b.SetPoS()
// 	})
// 	api := NewTransactionAPI(b, nil)
// 	type result struct {
// 		Hashes  []common.Hash
// 		Sidecar *types.BlobTxSidecar
// 	}
// 	suite := []struct {
// 		name string
// 		args TransactionArgs
// 		err  string
// 		want *result
// 	}{
// 		{
// 			name: "TestInvalidParamsCombination1",
// 			args: TransactionArgs{
// 				From:   &b.acc.Address,
// 				To:     &to,
// 				Value:  (*hexutil.Big)(big.NewInt(1)),
// 				Blobs:  []kzg4844.Blob{{}},
// 				Proofs: []kzg4844.Proof{{}},
// 			},
// 			err: `blob proofs provided while commitments were not`,
// 		},
// 		{
// 			name: "TestInvalidParamsCombination2",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				Blobs:       []kzg4844.Blob{{}},
// 				Commitments: []kzg4844.Commitment{{}},
// 			},
// 			err: `blob commitments provided while proofs were not`,
// 		},
// 		{
// 			name: "TestInvalidParamsCount1",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				Blobs:       []kzg4844.Blob{{}},
// 				Commitments: []kzg4844.Commitment{{}, {}},
// 				Proofs:      []kzg4844.Proof{{}, {}},
// 			},
// 			err: `number of blobs and commitments mismatch (have=2, want=1)`,
// 		},
// 		{
// 			name: "TestInvalidParamsCount2",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				Blobs:       []kzg4844.Blob{{}, {}},
// 				Commitments: []kzg4844.Commitment{{}, {}},
// 				Proofs:      []kzg4844.Proof{{}},
// 			},
// 			err: `number of blobs and proofs mismatch (have=1, want=2)`,
// 		},
// 		{
// 			name: "TestInvalidProofVerification",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				Blobs:       []kzg4844.Blob{{}, {}},
// 				Commitments: []kzg4844.Commitment{{}, {}},
// 				Proofs:      []kzg4844.Proof{{}, {}},
// 			},
// 			err: `failed to verify blob proof: short buffer`,
// 		},
// 		{
// 			name: "TestGenerateBlobHashes",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				Blobs:       emptyBlobs,
// 				Commitments: []kzg4844.Commitment{emptyBlobCommit},
// 				Proofs:      []kzg4844.Proof{emptyBlobProof},
// 			},
// 			want: &result{
// 				Hashes: []common.Hash{emptyBlobHash},
// 				Sidecar: &types.BlobTxSidecar{
// 					Blobs:       emptyBlobs,
// 					Commitments: []kzg4844.Commitment{emptyBlobCommit},
// 					Proofs:      []kzg4844.Proof{emptyBlobProof},
// 				},
// 			},
// 		},
// 		{
// 			name: "TestValidBlobHashes",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				BlobHashes:  []common.Hash{emptyBlobHash},
// 				Blobs:       emptyBlobs,
// 				Commitments: []kzg4844.Commitment{emptyBlobCommit},
// 				Proofs:      []kzg4844.Proof{emptyBlobProof},
// 			},
// 			want: &result{
// 				Hashes: []common.Hash{emptyBlobHash},
// 				Sidecar: &types.BlobTxSidecar{
// 					Blobs:       emptyBlobs,
// 					Commitments: []kzg4844.Commitment{emptyBlobCommit},
// 					Proofs:      []kzg4844.Proof{emptyBlobProof},
// 				},
// 			},
// 		},
// 		{
// 			name: "TestInvalidBlobHashes",
// 			args: TransactionArgs{
// 				From:        &b.acc.Address,
// 				To:          &to,
// 				Value:       (*hexutil.Big)(big.NewInt(1)),
// 				BlobHashes:  []common.Hash{{0x01, 0x22}},
// 				Blobs:       emptyBlobs,
// 				Commitments: []kzg4844.Commitment{emptyBlobCommit},
// 				Proofs:      []kzg4844.Proof{emptyBlobProof},
// 			},
// 			err: fmt.Sprintf("blob hash verification failed (have=%s, want=%s)", common.Hash{0x01, 0x22}, emptyBlobHash),
// 		},
// 		{
// 			name: "TestGenerateBlobProofs",
// 			args: TransactionArgs{
// 				From:  &b.acc.Address,
// 				To:    &to,
// 				Value: (*hexutil.Big)(big.NewInt(1)),
// 				Blobs: emptyBlobs,
// 			},
// 			want: &result{
// 				Hashes: []common.Hash{emptyBlobHash},
// 				Sidecar: &types.BlobTxSidecar{
// 					Blobs:       emptyBlobs,
// 					Commitments: []kzg4844.Commitment{emptyBlobCommit},
// 					Proofs:      []kzg4844.Proof{emptyBlobProof},
// 				},
// 			},
// 		},
// 	}
// 	for _, tc := range suite {
// 		t.Run(tc.name, func(t *testing.T) {
// 			res, err := api.FillTransaction(t.Context(), tc.args)
// 			if len(tc.err) > 0 {
// 				if err == nil {
// 					t.Fatalf("missing error. want: %s", tc.err)
// 				} else if err.Error() != tc.err {
// 					t.Fatalf("error mismatch. want: %s, have: %s", tc.err, err.Error())
// 				}
// 				return
// 			}
// 			if err != nil && len(tc.err) == 0 {
// 				t.Fatalf("expected no error. have: %s", err)
// 			}
// 			if res == nil {
// 				t.Fatal("result missing")
// 			}
// 			want, err := json.Marshal(tc.want)
// 			if err != nil {
// 				t.Fatalf("failed to encode expected: %v", err)
// 			}
// 			have, err := json.Marshal(result{Hashes: res.Tx.BlobHashes(), Sidecar: res.Tx.BlobTxSidecar()})
// 			if err != nil {
// 				t.Fatalf("failed to encode computed sidecar: %v", err)
// 			}
// 			if !bytes.Equal(have, want) {
// 				t.Errorf("blob sidecar mismatch. Have: %s, want: %s", have, want)
// 			}
// 		})
// 	}
// }

func argsFromTransaction(tx *types.Transaction, from common.Address) TransactionArgs {
	var (
		gas        = tx.Gas()
		nonce      = tx.Nonce()
		input      = tx.Data()
		accessList *types.AccessList
	)
	if acl := tx.AccessList(); acl != nil {
		accessList = &acl
	}
	return TransactionArgs{
		From:                 &from,
		To:                   tx.To(),
		Gas:                  (*hexutil.Uint64)(&gas),
		MaxFeePerGas:         (*hexutil.Big)(tx.GasFeeCap()),
		MaxPriorityFeePerGas: (*hexutil.Big)(tx.GasTipCap()),
		Value:                (*hexutil.Big)(tx.Value()),
		Nonce:                (*hexutil.Uint64)(&nonce),
		Input:                (*hexutil.Bytes)(&input),
		ChainID:              (*hexutil.Big)(tx.ChainId()),
		AccessList:           accessList,
		BlobFeeCap:           (*hexutil.Big)(tx.BlobGasFeeCap()),
		BlobHashes:           tx.BlobHashes(),
	}
}

type account struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

func newAccounts(n int) (accounts []account) {
	for i := 0; i < n; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		accounts = append(accounts, account{key: key, addr: addr})
	}
	slices.SortFunc(accounts, func(a, b account) int { return a.addr.Cmp(b.addr) })
	return accounts
}

func newTestAccount() account {
	// testKey is a private key to use for funding a tester account.
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	// testAddr is the Ethereum address of the tester account.
	addr := crypto.PubkeyToAddress(key.PublicKey)
	return account{key: key, addr: addr}
}

func newRPCBalance(balance *big.Int) *hexutil.Big {
	rpcBalance := (*hexutil.Big)(balance)
	return rpcBalance
}

func hex2Bytes(str string) *hexutil.Bytes {
	rpcBytes := hexutil.Bytes(common.FromHex(str))
	return &rpcBytes
}

func newUint64(v uint64) *hexutil.Uint64 {
	rpcUint64 := hexutil.Uint64(v)
	return &rpcUint64
}

func newBytes(b []byte) *hexutil.Bytes {
	rpcBytes := hexutil.Bytes(b)
	return &rpcBytes
}

func uint256ToBytes(v *uint256.Int) *hexutil.Bytes {
	b := v.Bytes32()
	r := hexutil.Bytes(b[:])
	return &r
}

func TestRPCMarshalBlock(t *testing.T) {
	t.Parallel()
	var (
		txs []*types.Transaction
		to  = common.BytesToAddress([]byte{0x11})
	)
	for i := uint64(1); i <= 4; i++ {
		var tx *types.Transaction
		if i%2 == 0 {
			tx = types.NewTx(&types.LegacyTx{
				Nonce:    i,
				GasPrice: big.NewInt(11111),
				Gas:      1111,
				To:       &to,
				Value:    big.NewInt(111),
				Data:     []byte{0x11, 0x11, 0x11},
			})
		} else {
			tx = types.NewTx(&types.AccessListTx{
				ChainID:  big.NewInt(1337),
				Nonce:    i,
				GasPrice: big.NewInt(11111),
				Gas:      1111,
				To:       &to,
				Value:    big.NewInt(111),
				Data:     []byte{0x11, 0x11, 0x11},
			})
		}
		txs = append(txs, tx)
	}
	block := types.NewBlock(&types.Header{Number: big.NewInt(100)}, &types.Body{Transactions: txs}, nil, blocktest.NewHasher())

	var testSuite = []struct {
		inclTx bool
		fullTx bool
		want   string
	}{
		// without txs
		{
			inclTx: false,
			fullTx: false,
			want: `{
				"difficulty": "0x0",
				"extraData": "0x",
				"gasLimit": "0x0",
				"gasUsed": "0x0",
				"hash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
				"miner": "0x0000000000000000000000000000000000000000",
				"mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"nonce": "0x0000000000000000",
				"number": "0x64",
				"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"receiptsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
				"size": "0x296",
				"stateRoot": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"timestamp": "0x0",
				"transactionsRoot": "0x661a9febcfa8f1890af549b874faf9fa274aede26ef489d9db0b25daa569450e",
				"uncles": []
			}`,
		},
		// only tx hashes
		{
			inclTx: true,
			fullTx: false,
			want: `{
				"difficulty": "0x0",
				"extraData": "0x",
				"gasLimit": "0x0",
				"gasUsed": "0x0",
				"hash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
				"miner": "0x0000000000000000000000000000000000000000",
				"mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"nonce": "0x0000000000000000",
				"number": "0x64",
				"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"receiptsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
				"size": "0x296",
				"stateRoot": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"timestamp": "0x0",
				"transactions": [
					"0x7d39df979e34172322c64983a9ad48302c2b889e55bda35324afecf043a77605",
					"0x9bba4c34e57c875ff57ac8d172805a26ae912006985395dc1bdf8f44140a7bf4",
					"0x98909ea1ff040da6be56bc4231d484de1414b3c1dac372d69293a4beb9032cb5",
					"0x12e1f81207b40c3bdcc13c0ee18f5f86af6d31754d57a0ea1b0d4cfef21abef1"
				],
				"transactionsRoot": "0x661a9febcfa8f1890af549b874faf9fa274aede26ef489d9db0b25daa569450e",
				"uncles": []
			}`,
		},
		// full tx details
		{
			inclTx: true,
			fullTx: true,
			want: `{
				"difficulty": "0x0",
				"extraData": "0x",
				"gasLimit": "0x0",
				"gasUsed": "0x0",
				"hash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
				"miner": "0x0000000000000000000000000000000000000000",
				"mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"nonce": "0x0000000000000000",
				"number": "0x64",
				"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"receiptsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
				"size": "0x296",
				"stateRoot": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"timestamp": "0x0",
				"transactions": [
					{
						"blockHash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
						"blockNumber": "0x64",
						"blockTimestamp": "0x0",
						"from": "0x0000000000000000000000000000000000000000",
						"gas": "0x457",
						"gasPrice": "0x2b67",
						"hash": "0x7d39df979e34172322c64983a9ad48302c2b889e55bda35324afecf043a77605",
						"input": "0x111111",
						"nonce": "0x1",
						"to": "0x0000000000000000000000000000000000000011",
						"transactionIndex": "0x0",
						"value": "0x6f",
						"type": "0x1",
						"accessList": [],
						"chainId": "0x539",
						"v": "0x0",
						"r": "0x0",
						"s": "0x0",
						"yParity": "0x0"
					},
					{
						"blockHash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
						"blockNumber": "0x64",
						"blockTimestamp": "0x0",
						"from": "0x0000000000000000000000000000000000000000",
						"gas": "0x457",
						"gasPrice": "0x2b67",
						"hash": "0x9bba4c34e57c875ff57ac8d172805a26ae912006985395dc1bdf8f44140a7bf4",
						"input": "0x111111",
						"nonce": "0x2",
						"to": "0x0000000000000000000000000000000000000011",
						"transactionIndex": "0x1",
						"value": "0x6f",
						"type": "0x0",
						"chainId": "0x7fffffffffffffee",
						"v": "0x0",
						"r": "0x0",
						"s": "0x0"
					},
					{
						"blockHash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
						"blockNumber": "0x64",
						"blockTimestamp": "0x0",
						"from": "0x0000000000000000000000000000000000000000",
						"gas": "0x457",
						"gasPrice": "0x2b67",
						"hash": "0x98909ea1ff040da6be56bc4231d484de1414b3c1dac372d69293a4beb9032cb5",
						"input": "0x111111",
						"nonce": "0x3",
						"to": "0x0000000000000000000000000000000000000011",
						"transactionIndex": "0x2",
						"value": "0x6f",
						"type": "0x1",
						"accessList": [],
						"chainId": "0x539",
						"v": "0x0",
						"r": "0x0",
						"s": "0x0",
						"yParity": "0x0"
					},
					{
						"blockHash": "0x9b73c83b25d0faf7eab854e3684c7e394336d6e135625aafa5c183f27baa8fee",
						"blockNumber": "0x64",
						"blockTimestamp": "0x0",
						"from": "0x0000000000000000000000000000000000000000",
						"gas": "0x457",
						"gasPrice": "0x2b67",
						"hash": "0x12e1f81207b40c3bdcc13c0ee18f5f86af6d31754d57a0ea1b0d4cfef21abef1",
						"input": "0x111111",
						"nonce": "0x4",
						"to": "0x0000000000000000000000000000000000000011",
						"transactionIndex": "0x3",
						"value": "0x6f",
						"type": "0x0",
						"chainId": "0x7fffffffffffffee",
						"v": "0x0",
						"r": "0x0",
						"s": "0x0"
					}
				],
				"transactionsRoot": "0x661a9febcfa8f1890af549b874faf9fa274aede26ef489d9db0b25daa569450e",
				"uncles": []
			}`,
		},
	}

	for i, tc := range testSuite {
		resp := RPCMarshalBlock(block, tc.inclTx, tc.fullTx, params.MainnetChainConfig, nil)
		out, err := json.Marshal(resp)
		if err != nil {
			t.Errorf("test %d: json marshal error: %v", i, err)
			continue
		}
		require.JSONEqf(t, tc.want, string(out), "test %d", i)
	}
}

func TestRPCGetBlockOrHeader(t *testing.T) {
	// Note: Upstream (geth) tests have a different genesis hash as it has a different
	// state root hash due to allocating balance separately in test backend. Because
	// that is commented out in bor, we use the old genesis hash in the test files.

	t.Parallel()

	// Initialize test accounts
	var (
		acc1Key, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		acc2Key, _ = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
		acc1Addr   = crypto.PubkeyToAddress(acc1Key.PublicKey)
		acc2Addr   = crypto.PubkeyToAddress(acc2Key.PublicKey)
		genesis    = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				acc1Addr: {Balance: big.NewInt(params.Ether)},
				acc2Addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 10
		signer    = types.HomesteadSigner{}
	)
	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(i), To: &acc2Addr, Value: big.NewInt(1000), Gas: params.TxGas, GasPrice: b.BaseFee(), Data: nil}), signer, acc1Key)
		b.AddTx(tx)
	})
	api := NewBlockChainAPI(backend)
	blockHashes := make([]common.Hash, genBlocks+1)
	ctx := t.Context()
	for i := 0; i <= genBlocks; i++ {
		header, err := backend.HeaderByNumber(ctx, rpc.BlockNumber(i))
		if err != nil {
			t.Errorf("failed to get block: %d err: %v", i, err)
		}
		blockHashes[i] = header.Hash()
	}
	pendingHash := backend.pending.Hash()

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		blockHash   *common.Hash
		fullTx      bool
		reqHeader   bool
		file        string
		expectErr   error
	}{
		// 0. latest header
		{
			blockNumber: rpc.LatestBlockNumber,
			reqHeader:   true,
			file:        "tag-latest",
		},
		// 1. genesis header
		{
			blockNumber: rpc.BlockNumber(0),
			reqHeader:   true,
			file:        "number-0",
		},
		// 2. #1 header
		{
			blockNumber: rpc.BlockNumber(1),
			reqHeader:   true,
			file:        "number-1",
		},
		// 3. latest-1 header
		{
			blockNumber: rpc.BlockNumber(9),
			reqHeader:   true,
			file:        "number-latest-1",
		},
		// 4. latest+1 header
		{
			blockNumber: rpc.BlockNumber(11),
			reqHeader:   true,
			file:        "number-latest+1",
		},
		// 5. pending header
		{
			blockNumber: rpc.PendingBlockNumber,
			reqHeader:   true,
			file:        "tag-pending",
		},
		// 6. latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			file:        "tag-latest",
		},
		// 7. genesis block
		{
			blockNumber: rpc.BlockNumber(0),
			file:        "number-0",
		},
		// 8. #1 block
		{
			blockNumber: rpc.BlockNumber(1),
			file:        "number-1",
		},
		// 9. latest-1 block
		{
			blockNumber: rpc.BlockNumber(9),
			fullTx:      true,
			file:        "number-latest-1",
		},
		// 10. latest+1 block
		{
			blockNumber: rpc.BlockNumber(11),
			fullTx:      true,
			file:        "number-latest+1",
		},
		// 11. pending block
		{
			blockNumber: rpc.PendingBlockNumber,
			file:        "tag-pending",
		},
		// 12. pending block + fullTx
		{
			blockNumber: rpc.PendingBlockNumber,
			fullTx:      true,
			file:        "tag-pending-fullTx",
		},
		// 13. latest header by hash
		{
			blockHash: &blockHashes[len(blockHashes)-1],
			reqHeader: true,
			file:      "hash-latest",
		},
		// 14. genesis header by hash
		{
			blockHash: &blockHashes[0],
			reqHeader: true,
			file:      "hash-0",
		},
		// 15. #1 header
		{
			blockHash: &blockHashes[1],
			reqHeader: true,
			file:      "hash-1",
		},
		// 16. latest-1 header
		{
			blockHash: &blockHashes[len(blockHashes)-2],
			reqHeader: true,
			file:      "hash-latest-1",
		},
		// 17. empty hash
		{
			blockHash: &common.Hash{},
			reqHeader: true,
			file:      "hash-empty",
		},
		// 18. pending hash
		{
			blockHash: &pendingHash,
			reqHeader: true,
			file:      `hash-pending`,
		},
		// 19. latest block
		{
			blockHash: &blockHashes[len(blockHashes)-1],
			file:      "hash-latest",
		},
		// 20. genesis block
		{
			blockHash: &blockHashes[0],
			file:      "hash-genesis",
		},
		// 21. #1 block
		{
			blockHash: &blockHashes[1],
			file:      "hash-1",
		},
		// 22. latest-1 block
		{
			blockHash: &blockHashes[len(blockHashes)-2],
			fullTx:    true,
			file:      "hash-latest-1-fullTx",
		},
		// 23. empty hash + body
		{
			blockHash: &common.Hash{},
			fullTx:    true,
			file:      "hash-empty-fullTx",
		},
		// 24. pending block
		{
			blockHash: &pendingHash,
			file:      `hash-pending`,
		},
		// 25. pending block + fullTx
		{
			blockHash: &pendingHash,
			fullTx:    true,
			file:      "hash-pending-fullTx",
		},
		// 26. safe block
		{
			blockNumber: rpc.SafeBlockNumber,
			file:        "tag-safe",
		},
	}

	for i, tt := range testSuite {
		var (
			result map[string]interface{}
			err    error
			rpc    string
		)
		if tt.blockHash != nil {
			if tt.reqHeader {
				result = api.GetHeaderByHash(t.Context(), *tt.blockHash)
				rpc = "eth_getHeaderByHash"
			} else {
				result, err = api.GetBlockByHash(t.Context(), *tt.blockHash, tt.fullTx, nil)
				rpc = "eth_getBlockByHash"
			}
		} else {
			if tt.reqHeader {
				result, err = api.GetHeaderByNumber(t.Context(), tt.blockNumber)
				rpc = "eth_getHeaderByNumber"
			} else {
				result, err = api.GetBlockByNumber(t.Context(), tt.blockNumber, tt.fullTx, nil)
				rpc = "eth_getBlockByNumber"
			}
		}
		if tt.expectErr != nil {
			if err == nil {
				t.Errorf("test %d: want error %v, have nothing", i, tt.expectErr)
				continue
			}
			if !errors.Is(err, tt.expectErr) {
				t.Errorf("test %d: error mismatch, want %v, have %v", i, tt.expectErr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("test %d: want no error, have %v", i, err)
			continue
		}

		testRPCResponseWithFile(t, i, result, rpc, tt.file)
	}
}

func setupTransactionsToApiTest(t *testing.T) (*TransactionAPI, []common.Hash, []struct {
	txHash common.Hash
	file   string
}) {
	config := *params.TestChainConfig
	genBlocks := 5
	config.ShanghaiBlock = big.NewInt(0)
	config.CancunBlock = big.NewInt(0)

	var (
		acc1Key, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		acc2Key, _ = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
		acc1Addr   = crypto.PubkeyToAddress(acc1Key.PublicKey)
		acc2Addr   = crypto.PubkeyToAddress(acc2Key.PublicKey)
		contract   = common.HexToAddress("0000000000000000000000000000000000031ec7")
		genesis    = &core.Genesis{
			Config:        &config,
			ExcessBlobGas: new(uint64),
			BlobGasUsed:   new(uint64),
			Alloc: types.GenesisAlloc{
				acc1Addr: {Balance: big.NewInt(params.Ether)},
				acc2Addr: {Balance: big.NewInt(params.Ether)},
				// // SPDX-License-Identifier: GPL-3.0
				// pragma solidity >=0.7.0 <0.9.0;
				//
				// contract Token {
				//     event Transfer(address indexed from, address indexed to, uint256 value);
				//     function transfer(address to, uint256 value) public returns (bool) {
				//         emit Transfer(msg.sender, to, value);
				//         return true;
				//     }
				// }
				contract: {Balance: big.NewInt(params.Ether), Code: common.FromHex("0x608060405234801561001057600080fd5b506004361061002b5760003560e01c8063a9059cbb14610030575b600080fd5b61004a6004803603810190610045919061016a565b610060565b60405161005791906101c5565b60405180910390f35b60008273ffffffffffffffffffffffffffffffffffffffff163373ffffffffffffffffffffffffffffffffffffffff167fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef846040516100bf91906101ef565b60405180910390a36001905092915050565b600080fd5b600073ffffffffffffffffffffffffffffffffffffffff82169050919050565b6000610101826100d6565b9050919050565b610111816100f6565b811461011c57600080fd5b50565b60008135905061012e81610108565b92915050565b6000819050919050565b61014781610134565b811461015257600080fd5b50565b6000813590506101648161013e565b92915050565b60008060408385031215610181576101806100d1565b5b600061018f8582860161011f565b92505060206101a085828601610155565b9150509250929050565b60008115159050919050565b6101bf816101aa565b82525050565b60006020820190506101da60008301846101b6565b92915050565b6101e981610134565b82525050565b600060208201905061020460008301846101e0565b9291505056fea2646970667358221220b469033f4b77b9565ee84e0a2f04d496b18160d26034d54f9487e57788fd36d564736f6c63430008120033")},
			},
		}
		signer   = types.LatestSignerForChainID(params.TestChainConfig.ChainID)
		txHashes = make([]common.Hash, 0, genBlocks+1)
	)

	// Set the terminal total difficulty in the config
	genesis.Config.TerminalTotalDifficulty = big.NewInt(0)

	backend := newTestBackend(t, genBlocks, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		var (
			tx  *types.Transaction
			err error
		)
		b.SetPoS()
		switch i {
		case 1:
			// create contract
			tx, err = types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(i), To: nil, Gas: 53100, GasPrice: b.BaseFee(), Data: common.FromHex("0x60806040")}), signer, acc1Key)
		case 2:
			// with logs
			// transfer(address to, uint256 value)
			data := fmt.Sprintf("0xa9059cbb%s%s", common.HexToHash(common.BigToAddress(big.NewInt(int64(i + 1))).Hex()).String()[2:], common.BytesToHash([]byte{byte(i + 11)}).String()[2:])
			tx, err = types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(i), To: &contract, Gas: 60000, GasPrice: b.BaseFee(), Data: common.FromHex(data)}), signer, acc1Key)
		case 3:
			// dynamic fee with logs
			// transfer(address to, uint256 value)
			data := fmt.Sprintf("0xa9059cbb%s%s", common.HexToHash(common.BigToAddress(big.NewInt(int64(i + 1))).Hex()).String()[2:], common.BytesToHash([]byte{byte(i + 11)}).String()[2:])
			fee := big.NewInt(500)
			fee.Add(fee, b.BaseFee())
			tx, err = types.SignTx(types.NewTx(&types.DynamicFeeTx{Nonce: uint64(i), To: &contract, Gas: 60000, Value: big.NewInt(1), GasTipCap: big.NewInt(500), GasFeeCap: fee, Data: common.FromHex(data)}), signer, acc1Key)
		case 4:
			// access list with contract create
			accessList := types.AccessList{{
				Address:     contract,
				StorageKeys: []common.Hash{{0}},
			}}
			tx, err = types.SignTx(types.NewTx(&types.AccessListTx{Nonce: uint64(i), To: nil, Gas: 58100, GasPrice: b.BaseFee(), Data: common.FromHex("0x60806040"), AccessList: accessList}), signer, acc1Key)
		case 5:
			// blob tx
			fee := big.NewInt(500)
			fee.Add(fee, b.BaseFee())
			tx, err = types.SignTx(types.NewTx(&types.BlobTx{
				Nonce:      uint64(i),
				GasTipCap:  uint256.NewInt(1),
				GasFeeCap:  uint256.MustFromBig(fee),
				Gas:        params.TxGas,
				To:         acc2Addr,
				BlobFeeCap: uint256.NewInt(1),
				BlobHashes: []common.Hash{{1}},
				Value:      new(uint256.Int),
			}), signer, acc1Key)
		default:
			// transfer 1000wei
			tx, err = types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(i), To: &acc2Addr, Value: big.NewInt(1000), Gas: params.TxGas, GasPrice: b.BaseFee(), Data: nil}), types.HomesteadSigner{}, acc1Key)
		}
		if err != nil {
			t.Errorf("failed to sign tx: %v", err)
		}
		if tx != nil {
			b.AddTx(tx)
			txHashes = append(txHashes, tx.Hash())
		}
	})

	txHashes[genBlocks] = mockStateSyncTxOnCurrentBlock(t, backend)

	var testSuite = []struct {
		txHash common.Hash
		file   string
	}{
		// 0. normal success
		{
			txHash: txHashes[0],
			file:   "normal-transfer-tx",
		},
		// 1. create contract
		{
			txHash: txHashes[1],
			file:   "create-contract-tx",
		},
		// 2. with logs success
		{
			txHash: txHashes[2],
			file:   "with-logs",
		},
		// 3. dynamic tx with logs success
		{
			txHash: txHashes[3],
			file:   `dynamic-tx-with-logs`,
		},
		// 4. access list tx with create contract
		{
			txHash: txHashes[4],
			file:   "create-contract-with-access-list",
		},
		// 5. txhash empty
		{
			txHash: common.Hash{},
			file:   "txhash-empty",
		},
		// 6. txhash not found
		{
			txHash: common.HexToHash("deadbeef"),
			file:   "txhash-notfound",
		},
		// 7. state sync tx found
		{
			txHash: txHashes[5],
			file:   "state-sync-tx",
		},
	}
	// map sprint 0 to block 6
	backend.ChainConfig().Bor.Sprint["0"] = uint64(genBlocks)

	api := NewTransactionAPI(backend, new(AddrLocker))

	return api, txHashes, testSuite
}

func mockStateSyncTxOnCurrentBlock(t *testing.T, backend *testBackend) common.Hash {
	// State Sync Tx Setup
	var stateSyncLogs []*types.Log
	block, err := backend.BlockByHash(t.Context(), backend.CurrentBlock().Hash())
	if err != nil {
		t.Errorf("failed to get current block: %v", err)
	}

	types.DeriveFieldsForBorLogs(stateSyncLogs, block.Hash(), block.NumberU64(), 0, 0)

	// Write bor receipt
	rawdb.WriteBorReceipt(backend.ChainDb(), block.Hash(), block.NumberU64(), &types.ReceiptForStorage{
		Status: types.ReceiptStatusSuccessful, // make receipt status successful
		Logs:   stateSyncLogs,
	})

	// Write bor tx reverse lookup
	rawdb.WriteBorTxLookupEntry(backend.ChainDb(), block.Hash(), block.NumberU64())
	return types.GetDerivedBorTxHash(types.BorReceiptKey(block.NumberU64(), block.Hash()))
}

func TestRPCGetTransactionReceipt(t *testing.T) {
	var (
		api, _, testSuite = setupTransactionsToApiTest(t)
	)

	for i, tt := range testSuite {
		var (
			result interface{}
			err    error
		)
		result, err = api.GetTransactionReceipt(t.Context(), tt.txHash)
		if err != nil {
			t.Errorf("test %d: want no error, have %v", i, err)
			continue
		}
		testRPCResponseWithFile(t, i, result, "eth_getTransactionReceipt", tt.file)
	}
}
func TestRPCGetTransactionByHash(t *testing.T) {
	var (
		api, _, testSuite = setupTransactionsToApiTest(t)
	)

	for i, tt := range testSuite {
		var (
			result interface{}
			err    error
		)
		result, err = api.GetTransactionByHash(t.Context(), tt.txHash)
		if err != nil {
			t.Errorf("test %d: want no error, have %v", i, err)
			continue
		}
		testRPCResponseWithFile(t, i, result, "eth_getTransactionByHash", tt.file)
	}
}

func TestRPCGetBlockTransactionCountByHash(t *testing.T) {
	var (
		api, _, _ = setupTransactionsToApiTest(t)
	)

	cnt, err := api.GetBlockTransactionCountByHash(t.Context(), api.b.CurrentBlock().Hash())
	if err != nil {
		t.Errorf("failed to get block transaction count by hash: %v", err)
	}

	// 2 txs: create-contract-with-access-list + state sync tx
	expected := hexutil.Uint(2)
	require.Equal(t, expected, *cnt)
}

func TestRPCGetTransactionByBlockHashAndIndex(t *testing.T) {
	var (
		api, _, _ = setupTransactionsToApiTest(t)
	)

	createContractWithAccessList, err := api.GetTransactionByBlockHashAndIndex(t.Context(), api.b.CurrentBlock().Hash(), 0)
	if err != nil {
		t.Errorf("failed to get transaction by block hash and index: %v", err)
	}

	stateSyncTx, err := api.GetTransactionByBlockHashAndIndex(t.Context(), api.b.CurrentBlock().Hash(), 1)
	if err != nil {
		t.Errorf("failed to get transaction by block hash and index: %v", err)
	}

	testRPCResponseWithFile(t, 0, createContractWithAccessList, "eth_getTransactionByBlockHashAndIndex", "create-contract-with-access-list")
	testRPCResponseWithFile(t, 1, stateSyncTx, "eth_getTransactionByBlockHashAndIndex", "state-sync-tx")
}

func testRPCResponseWithFile(t *testing.T, testid int, result interface{}, rpc string, file string) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Errorf("test %d: json marshal error", testid)
		return
	}
	outputFile := filepath.Join("testdata", fmt.Sprintf("%s-%s.json", rpc, file))
	if os.Getenv("WRITE_TEST_FILES") != "" {
		err = os.WriteFile(outputFile, data, 0644)
		require.NoError(t, err, "failed to write test output file: %s", outputFile)
	}
	want, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("error reading expected test file: %s output: %v", outputFile, err)
	}
	require.JSONEqf(t, string(want), string(data), "test %d: json not match, want: %s, have: %s", testid, string(want), string(data))
}

func TestRPCGetTransactionReceiptsByBlock(t *testing.T) {
	api, blockNrOrHash, testSuite := setupBlocksToApiTest(t)

	receipts, err := api.GetBlockReceipts(t.Context(), blockNrOrHash)
	if err != nil {
		t.Fatal("api error")
	}

	for i, tt := range testSuite {
		data, err := json.Marshal(receipts[i])
		if err != nil {
			t.Errorf("test %d: json marshal error", i)
			continue
		}
		want, have := tt.want, string(data)
		require.JSONEqf(t, want, have, "test %d: json not match, want: %s, have: %s", i, want, have)
	}
}

func TestRPCGetBlockReceipts(t *testing.T) {
	api, blockNrOrHash, testSuite := setupBlocksToApiTest(t)

	receipts, err := api.GetBlockReceipts(t.Context(), blockNrOrHash)
	if err != nil {
		t.Fatal("api error")
	}

	for i, tt := range testSuite {
		data, err := json.Marshal(receipts[i])
		if err != nil {
			t.Errorf("test %d: json marshal error", i)
			continue
		}
		want, have := tt.want, string(data)
		require.JSONEqf(t, want, have, "test %d: json not match, want: %s, have: %s", i, want, have)
	}
}

func TestAccessListWorksForAnyEmptyAddress(t *testing.T) {
	api, _, _ := setupBlocksToApiTest(t)

	from := common.BytesToAddress([]byte("deadbeef"))
	block := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	overrides := override.StateOverride{}
	res, err := api.CreateAccessList(t.Context(), TransactionArgs{
		From: &from,
		Data: hex2Bytes("0x608060806080608155"),
	}, &block, &overrides)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, res.Accesslist.StorageKeys())
}

func setupBlocksToApiTest(t *testing.T) (*BlockChainAPI, rpc.BlockNumberOrHash, []struct {
	txHash common.Hash
	want   string
}) {
	// Initialize test accounts
	var (
		acc1Key, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		acc2Key, _ = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
		acc1Addr   = crypto.PubkeyToAddress(acc1Key.PublicKey)
		acc2Addr   = crypto.PubkeyToAddress(acc2Key.PublicKey)
		contract   = common.HexToAddress("0000000000000000000000000000000000031ec7")
		genesis    = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				acc1Addr: {Balance: big.NewInt(params.Ether)},
				acc2Addr: {Balance: big.NewInt(params.Ether)},
				contract: {Balance: big.NewInt(params.Ether), Code: common.FromHex("0x608060405234801561001057600080fd5b506004361061002b5760003560e01c8063a9059cbb14610030575b600080fd5b61004a6004803603810190610045919061016a565b610060565b60405161005791906101c5565b60405180910390f35b60008273ffffffffffffffffffffffffffffffffffffffff163373ffffffffffffffffffffffffffffffffffffffff167fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef846040516100bf91906101ef565b60405180910390a36001905092915050565b600080fd5b600073ffffffffffffffffffffffffffffffffffffffff82169050919050565b6000610101826100d6565b9050919050565b610111816100f6565b811461011c57600080fd5b50565b60008135905061012e81610108565b92915050565b6000819050919050565b61014781610134565b811461015257600080fd5b50565b6000813590506101648161013e565b92915050565b60008060408385031215610181576101806100d1565b5b600061018f8582860161011f565b92505060206101a085828601610155565b9150509250929050565b60008115159050919050565b6101bf816101aa565b82525050565b60006020820190506101da60008301846101b6565b92915050565b6101e981610134565b82525050565b600060208201905061020460008301846101e0565b9291505056fea2646970667358221220b469033f4b77b9565ee84e0a2f04d496b18160d26034d54f9487e57788fd36d564736f6c63430008120033")},
			},
		}
		genTxs    = 6
		genBlocks = 1
		signer    = types.LatestSignerForChainID(params.TestChainConfig.ChainID)
		txHashes  = make([]common.Hash, 0, genTxs)
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		switch i {
		case 0:
			// transfer 1000wei
			tx1, _ := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(0), To: &acc2Addr, Value: big.NewInt(1000), Gas: params.TxGas, GasPrice: b.BaseFee(), Data: nil}), types.HomesteadSigner{}, acc1Key)
			b.AddTx(tx1)
			txHashes = append(txHashes, tx1.Hash())

			// create contract
			tx2, _ := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(1), To: nil, Gas: 53100, GasPrice: b.BaseFee(), Data: common.FromHex("0x60806040")}), signer, acc1Key)
			b.AddTx(tx2)
			txHashes = append(txHashes, tx2.Hash())

			// with logs
			// transfer(address to, uint256 value)
			data3 := fmt.Sprintf("0xa9059cbb%s%s", common.HexToHash(common.BigToAddress(big.NewInt(int64(i + 1))).Hex()).String()[2:], common.BytesToHash([]byte{byte(i + 11)}).String()[2:])
			tx3, _ := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: uint64(2), To: &contract, Gas: 60000, GasPrice: b.BaseFee(), Data: common.FromHex(data3)}), signer, acc1Key)
			b.AddTx(tx3)
			txHashes = append(txHashes, tx3.Hash())

			// dynamic fee with logs
			// transfer(address to, uint256 value)
			data4 := fmt.Sprintf("0xa9059cbb%s%s", common.HexToHash(common.BigToAddress(big.NewInt(int64(i + 1))).Hex()).String()[2:], common.BytesToHash([]byte{byte(i + 11)}).String()[2:])
			fee := big.NewInt(500)
			fee.Add(fee, b.BaseFee())
			tx4, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{Nonce: uint64(3), To: &contract, Gas: 60000, Value: big.NewInt(1), GasTipCap: big.NewInt(500), GasFeeCap: fee, Data: common.FromHex(data4)}), signer, acc1Key)
			b.AddTx(tx4)
			txHashes = append(txHashes, tx4.Hash())

			// access list with contract create
			accessList := types.AccessList{{
				Address:     contract,
				StorageKeys: []common.Hash{{0}},
			}}
			tx5, _ := types.SignTx(types.NewTx(&types.AccessListTx{Nonce: uint64(4), To: nil, Gas: 58100, GasPrice: b.BaseFee(), Data: common.FromHex("0x60806040"), AccessList: accessList}), signer, acc1Key)
			b.AddTx(tx5)
			txHashes = append(txHashes, tx5.Hash())
		}
	})

	txHashes = append(txHashes, mockStateSyncTxOnCurrentBlock(t, backend))

	// map sprint 0 to block 1
	backend.ChainConfig().Bor.Sprint["0"] = 1

	api := NewBlockChainAPI(backend)
	blockHashes := make([]common.Hash, genBlocks+1)
	ctx := t.Context()
	for i := 0; i <= genBlocks; i++ {
		header, err := backend.HeaderByNumber(ctx, rpc.BlockNumber(i))
		if err != nil {
			t.Errorf("failed to get block: %d err: %v", i, err)
		}
		blockHashes[i] = header.Hash()
	}
	blockNrOrHash := rpc.BlockNumberOrHashWithHash(blockHashes[1], true)

	var testSuite = []struct {
		txHash common.Hash
		want   string
	}{
		// 0. normal success
		{
			txHash: txHashes[0],
			want: `{
				"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
				"blockNumber": "0x1",
				"contractAddress": null,
				"cumulativeGasUsed": "0x5208",
				"effectiveGasPrice": "0x342770c0",
				"from": "0x703c4b2bd70c169f5717101caee543299fc946c7",
				"gasUsed": "0x5208",
				"logs": [
				  {
					"address": "0x0000000000000000000000000000000000001010",
					"topics": [
					  "0xe6497e3ee548a3372136af2fcb0696db31fc6cf20260707645068bd3fe97f3c4",
					  "0x0000000000000000000000000000000000000000000000000000000000001010",
					  "0x000000000000000000000000703c4b2bd70c169f5717101caee543299fc946c7",
					  "0x0000000000000000000000000d3ab14bbad3d99f4203bd7a11acb94882050e7e"
					],
					"data": "0x00000000000000000000000000000000000000000000000000000000000003e80000000000000000000000000000000000000000000000000de0a5fd640afa000000000000000000000000000000000000000000000000000de0b6b3a76400000000000000000000000000000000000000000000000000000de0a5fd640af6180000000000000000000000000000000000000000000000000de0b6b3a76403e8",
					"blockNumber": "0x1",
					"blockTimestamp": "0xa",
					"transactionHash": "0x644a31c354391520d00e95b9affbbb010fc79ac268144ab8e28207f4cf51097e",
					"transactionIndex": "0x0",
					"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
					"logIndex": "0x0",
					"removed": false
				  }
				],
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000100000000000808000000000000000000000000000000000000000000000000000000000800000000000000000000100000020000000000000000000000000000000000802000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000004000000000000000000000800000000000000000000000800000108000000000000000000000000000000000000000000000000020000000000000000000100000",
				"status": "0x1",
				"to": "0x0d3ab14bbad3d99f4203bd7a11acb94882050e7e",
				"transactionHash": "0x644a31c354391520d00e95b9affbbb010fc79ac268144ab8e28207f4cf51097e",
				"transactionIndex": "0x0",
				"type": "0x0"
			  }`,
		},
		// 1. create contract
		{
			txHash: txHashes[1],
			want: `{
				"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
				"blockNumber": "0x1",
				"contractAddress": "0xae9bea628c4ce503dcfd7e305cab4e29e7476592",
				"cumulativeGasUsed": "0x12156",
				"effectiveGasPrice": "0x342770c0",
				"from": "0x703c4b2bd70c169f5717101caee543299fc946c7",
				"gasUsed": "0xcf4e",
				"logs": [],
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
				"status": "0x1",
				"to": null,
				"transactionHash": "0x705a7fca1d214002ee90d4e1c651b53e3506e6d5e3a539e9a7f7bf05b49add91",
				"transactionIndex": "0x1",
				"type": "0x0"
			  }`,
		},
		// 2. with logs success
		{
			txHash: txHashes[2],
			want: `{
				"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
				"blockNumber": "0x1",
				"contractAddress": null,
				"cumulativeGasUsed": "0x17f7e",
				"effectiveGasPrice": "0x342770c0",
				"from": "0x703c4b2bd70c169f5717101caee543299fc946c7",
				"gasUsed": "0x5e28",
				"logs": [
				  {
					"address": "0x0000000000000000000000000000000000031ec7",
					"topics": [
					  "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
					  "0x000000000000000000000000703c4b2bd70c169f5717101caee543299fc946c7",
					  "0x0000000000000000000000000000000000000000000000000000000000000001"
					],
					"data": "0x000000000000000000000000000000000000000000000000000000000000000b",
					"blockNumber": "0x1",
					"blockTimestamp": "0xa",
					"transactionHash": "0xa228af0975b99799bd28331085a6966aba2fb5814a8d89aabc342462aa40429a",
					"transactionIndex": "0x2",
					"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
					"logIndex": "0x1",
					"removed": false
				  }
				],
				"logsBloom": "0x00000000000000000000008000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000040000000000800000000000000008000000000000000000040000000000000020000000080000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000002000000000000800000000000000000000000000000000000000040000000000000000000000000000000000000000020000000000000000000000000",
				"status": "0x1",
				"to": "0x0000000000000000000000000000000000031ec7",
				"transactionHash": "0xa228af0975b99799bd28331085a6966aba2fb5814a8d89aabc342462aa40429a",
				"transactionIndex": "0x2",
				"type": "0x0"
			  }`,
		},
		// 3. dynamic tx with logs success
		{
			txHash: txHashes[3],
			want: `{
				"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
				"blockNumber": "0x1",
				"contractAddress": null,
				"cumulativeGasUsed": "0x1d30b",
				"effectiveGasPrice": "0x342772b4",
				"from": "0x703c4b2bd70c169f5717101caee543299fc946c7",
				"gasUsed": "0x538d",
				"logs": [
				  {
					"address": "0x0000000000000000000000000000000000001010",
					"topics": [
					  "0x4dfe1bbbcf077ddc3e01291eea2d5c70c2b422b415d95645b9adcfd678cb1d63",
					  "0x0000000000000000000000000000000000000000000000000000000000001010",
					  "0x000000000000000000000000703c4b2bd70c169f5717101caee543299fc946c7",
					  "0x0000000000000000000000000000000000000000000000000000000000000000"
					],
					"data": "0x0000000000000000000000000000000000000000000000000000000000a32f640000000000000000000000000000000000000000000000000de06892fa4b3d9800000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000de06892f9a80e340000000000000000000000000000000000000000000000000000000000a32f64",
					"blockNumber": "0x1",
					"blockTimestamp": "0xa",
					"transactionHash": "0xc2cc458a65bc96f642d4a2063cce162b0da642613d801271bdbc4aa7e775f3ed",
					"transactionIndex": "0x3",
					"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
					"logIndex": "0x2",
					"removed": false
				  }
				],
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008000000000000000000000000000000000000000000000000000000000800000000000000000000100000020000000000000020000000000000000000800000000000000000080000000000000000000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000004000000000000000000001800000000000000000000000000000100000000020000000000000000000000000000000000000000020000000000000000000100000",
				"status": "0x0",
				"to": "0x0000000000000000000000000000000000031ec7",
				"transactionHash": "0xc2cc458a65bc96f642d4a2063cce162b0da642613d801271bdbc4aa7e775f3ed",
				"transactionIndex": "0x3",
				"type": "0x2"
			  }`,
		},
		// 4. access list tx with create contract
		{
			txHash: txHashes[4],
			want: `{
				"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
				"blockNumber": "0x1",
				"contractAddress": "0xfdaa97661a584d977b4d3abb5370766ff5b86a18",
				"cumulativeGasUsed": "0x2b325",
				"effectiveGasPrice": "0x342770c0",
				"from": "0x703c4b2bd70c169f5717101caee543299fc946c7",
				"gasUsed": "0xe01a",
				"logs": [],
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
				"status": "0x1",
				"to": null,
				"transactionHash": "0x1e1161cf3fd01a02fc9c5ee66fc45a4805b3828bf41edd54213c20d97fc12b1d",
				"transactionIndex": "0x4",
				"type": "0x1"
			  }`,
		},
		// 5. state sync tx
		{
			txHash: txHashes[5],
			want: `{
				"blockHash": "0xcdbefbd0a516759927751ca4c00084f967cef3817ce6e0fd819f0534b271cb4a",
				"blockNumber": "0x1",
				"contractAddress": null,
				"cumulativeGasUsed": "0x2b325",
				"effectiveGasPrice": "0x0",
				"from": "0x0000000000000000000000000000000000000000",
				"gasUsed": "0x0",
				"logs": [],
				"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
				"status": "0x1",
				"to": "0x0000000000000000000000000000000000000000",
				"transactionHash": "0xcd9b0d3d7c08df38f716a708b13e38cb286af42680feb37e6a34a1b4197231a3",
				"transactionIndex": "0x5",
				"type": "0x0"
			  }`,
		},
	}

	return api, blockNrOrHash, testSuite
}

func addressToHash(a common.Address) common.Hash {
	return common.BytesToHash(a.Bytes())
}

func TestCreateAccessListWithStateOverrides(t *testing.T) {
	// Initialize test backend
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0x71562b71999873db5b286df957af199ec94617f7"): {Balance: big.NewInt(1000000000000000000)},
		},
	}
	backend := newTestBackend(t, 1, genesis, ethash.NewFaker(), nil)

	// Create a new BlockChainAPI instance
	api := NewBlockChainAPI(backend)

	// Create test contract code - a simple storage contract
	//
	// SPDX-License-Identifier: MIT
	// pragma solidity ^0.8.0;
	//
	// contract SimpleStorage {
	//     uint256 private value;
	//
	//     function retrieve() public view returns (uint256) {
	//         return value;
	//     }
	// }
	var (
		contractCode = hexutil.Bytes(common.Hex2Bytes("6080604052348015600f57600080fd5b506004361060285760003560e01c80632e64cec114602d575b600080fd5b60336047565b604051603e91906067565b60405180910390f35b60008054905090565b6000819050919050565b6061816050565b82525050565b6000602082019050607a6000830184605a565b9291505056"))
		// Create state overrides with more complete state
		contractAddr = common.HexToAddress("0x1234567890123456789012345678901234567890")
		nonce        = hexutil.Uint64(1)
		overrides    = &override.StateOverride{
			contractAddr: override.OverrideAccount{
				Code:    &contractCode,
				Balance: (*hexutil.Big)(big.NewInt(1000000000000000000)),
				Nonce:   &nonce,
				State: map[common.Hash]common.Hash{
					{}: common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000002a"),
				},
			},
		}
	)

	// Create transaction arguments with gas and value
	var (
		from = common.HexToAddress("0x71562b71999873db5b286df957af199ec94617f7")
		data = hexutil.Bytes(common.Hex2Bytes("2e64cec1")) // retrieve()
		gas  = hexutil.Uint64(100000)
		args = TransactionArgs{
			From:  &from,
			To:    &contractAddr,
			Data:  &data,
			Gas:   &gas,
			Value: new(hexutil.Big),
		}
	)
	// Call CreateAccessList
	result, err := api.CreateAccessList(t.Context(), args, nil, overrides)
	if err != nil {
		t.Fatalf("Failed to create access list: %v", err)
	}
	if result == nil {
		t.Fatalf("Failed to create access list: result is nil")
	}
	require.NotNil(t, result.Accesslist)

	// Verify access list contains the contract address and storage slot
	expected := &types.AccessList{{
		Address:     contractAddr,
		StorageKeys: []common.Hash{{}},
	}}
	require.Equal(t, expected, result.Accesslist)
}

func TestSendRawTransactionForPreconf(t *testing.T) {
	t.Parallel()

	t.Run("rejected when AcceptPreconfTxs is false", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		api := NewTransactionAPI(b, new(AddrLocker))
		raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

		result, err := api.SendRawTransactionForPreconf(context.Background(), raw)
		require.Nil(t, result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "preconf transactions are not accepted")
	})

	t.Run("invalid raw tx bytes", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPreconfTxs = true

		api := NewTransactionAPI(b, new(AddrLocker))
		result, err := api.SendRawTransactionForPreconf(context.Background(), hexutil.Bytes{0xde, 0xad})
		require.Nil(t, result)
		require.Error(t, err)
	})

	t.Run("SubmitTransaction fails with non-ErrAlreadyKnown", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPreconfTxs = true
		b.sendTxErr = errors.New("nonce too low")

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

		result, err := api.SendRawTransactionForPreconf(context.Background(), raw)
		require.Nil(t, result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nonce too low")
	})

	t.Run("success with TxStatusPending returns preconfirmed true", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPreconfTxs = true
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus { return txpool.TxStatusPending }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		result, err := api.SendRawTransactionForPreconf(context.Background(), raw)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, tx.Hash(), result["hash"])
		require.Equal(t, true, result["preconfirmed"])
	})

	t.Run("success with TxStatusQueued returns preconfirmed false", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPreconfTxs = true
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus { return txpool.TxStatusQueued }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		result, err := api.SendRawTransactionForPreconf(context.Background(), raw)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, tx.Hash(), result["hash"])
		require.Equal(t, false, result["preconfirmed"])
	})

	t.Run("ErrAlreadyKnown with TxStatusPending returns preconfirmed true", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPreconfTxs = true
		b.sendTxErr = txpool.ErrAlreadyKnown
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus { return txpool.TxStatusPending }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		result, err := api.SendRawTransactionForPreconf(context.Background(), raw)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, tx.Hash(), result["hash"], "hash should be tx.Hash() not zero hash")
		require.Equal(t, true, result["preconfirmed"])
	})

	t.Run("ErrAlreadyKnown with TxStatusUnknown returns preconfirmed false", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPreconfTxs = true
		b.sendTxErr = txpool.ErrAlreadyKnown
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus { return txpool.TxStatusUnknown }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		result, err := api.SendRawTransactionForPreconf(context.Background(), raw)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, tx.Hash(), result["hash"])
		require.Equal(t, false, result["preconfirmed"])
	})
}

func TestSendRawTransactionPrivate(t *testing.T) {
	t.Parallel()

	t.Run("rejected when both AcceptPrivateTxs and PrivateTxEnabled are false", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		api := NewTransactionAPI(b, new(AddrLocker))
		raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.Equal(t, common.Hash{}, hash)
		require.Error(t, err)
		require.Contains(t, err.Error(), "private transactions are not accepted")
	})

	t.Run("invalid raw tx bytes", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPrivateTxs = true

		api := NewTransactionAPI(b, new(AddrLocker))
		hash, err := api.SendRawTransactionPrivate(context.Background(), hexutil.Bytes{0xde, 0xad})
		require.Equal(t, common.Hash{}, hash)
		require.Error(t, err)
	})

	t.Run("accepted via AcceptPrivateTxs, RecordPrivateTx called", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPrivateTxs = true

		var recordCount atomic.Int32
		var submitPrivateCount atomic.Int32
		b.recordPrivateTxFn = func(hash common.Hash) { recordCount.Add(1) }
		b.submitPrivateTxFn = func(tx *types.Transaction) error { submitPrivateCount.Add(1); return nil }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.NoError(t, err)
		require.Equal(t, tx.Hash(), hash)
		require.Equal(t, int32(1), recordCount.Load(), "RecordPrivateTx should be called once")
		require.Equal(t, int32(0), submitPrivateCount.Load(), "SubmitPrivateTx should NOT be called when PrivateTxEnabled is false")
	})

	t.Run("SendTx fails, PurgePrivateTx called", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPrivateTxs = true
		b.sendTxErr = errors.New("pool full")

		var recordCount atomic.Int32
		var purgeCount atomic.Int32
		b.recordPrivateTxFn = func(hash common.Hash) { recordCount.Add(1) }
		b.purgePrivateTxFn = func(hash common.Hash) { purgeCount.Add(1) }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

		_, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.Error(t, err)
		require.Contains(t, err.Error(), "pool full")
		require.Equal(t, int32(1), recordCount.Load(), "RecordPrivateTx should be called before SendTx")
		require.Equal(t, int32(1), purgeCount.Load(), "PurgePrivateTx should be called on SendTx failure")
	})

	t.Run("ErrAlreadyKnown does not purge", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPrivateTxs = true
		b.sendTxErr = txpool.ErrAlreadyKnown

		var purgeCount atomic.Int32
		b.purgePrivateTxFn = func(hash common.Hash) { purgeCount.Add(1) }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

		_, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.ErrorIs(t, err, txpool.ErrAlreadyKnown)
		require.Equal(t, int32(0), purgeCount.Load(), "PurgePrivateTx should NOT be called for ErrAlreadyKnown")
	})

	t.Run("PrivateTxEnabled, SubmitPrivateTx succeeds", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPrivateTxs = true
		b.privateTxEnabled = true

		var submitCount atomic.Int32
		b.submitPrivateTxFn = func(tx *types.Transaction) error { submitCount.Add(1); return nil }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.NoError(t, err)
		require.Equal(t, tx.Hash(), hash)
		require.Equal(t, int32(1), submitCount.Load(), "SubmitPrivateTx should be called once")
	})

	t.Run("PrivateTxEnabled, SubmitPrivateTx fails returns wrapped error", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.acceptPrivateTxs = true
		b.privateTxEnabled = true
		b.submitPrivateTxFn = func(tx *types.Transaction) error { return errors.New("relay down") }

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.Error(t, err)
		require.Contains(t, err.Error(), "private tx accepted locally, submission failed")
		require.Contains(t, err.Error(), "relay down")
		require.Equal(t, tx.Hash(), hash, "hash should be returned even on SubmitPrivateTx failure")
	})

	t.Run("accepted via PrivateTxEnabled only", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.privateTxEnabled = true // acceptPrivateTxs is false

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransactionPrivate(context.Background(), raw)
		require.NoError(t, err)
		require.Equal(t, tx.Hash(), hash, "should succeed via PrivateTxEnabled OR condition")
	})
}

func TestCheckPreconfStatus(t *testing.T) {
	t.Parallel()

	t.Run("rejected when PreconfEnabled is false", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		api := NewTransactionAPI(b, new(AddrLocker))

		result, err := api.CheckPreconfStatus(context.Background(), common.HexToHash("0x1"))
		require.False(t, result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "preconf transactions are not accepted")
	})

	t.Run("delegates to backend and returns true", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.preconfEnabled = true
		b.checkPreconfStatusFn = func(hash common.Hash) (bool, error) { return true, nil }

		api := NewTransactionAPI(b, new(AddrLocker))
		result, err := api.CheckPreconfStatus(context.Background(), common.HexToHash("0x1"))
		require.NoError(t, err)
		require.True(t, result)
	})

	t.Run("delegates to backend and returns false", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.preconfEnabled = true
		b.checkPreconfStatusFn = func(hash common.Hash) (bool, error) { return false, nil }

		api := NewTransactionAPI(b, new(AddrLocker))
		result, err := api.CheckPreconfStatus(context.Background(), common.HexToHash("0x1"))
		require.NoError(t, err)
		require.False(t, result)
	})

	t.Run("delegates to backend and returns error", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.preconfEnabled = true
		b.checkPreconfStatusFn = func(hash common.Hash) (bool, error) {
			return false, errors.New("relay unreachable")
		}

		api := NewTransactionAPI(b, new(AddrLocker))
		result, err := api.CheckPreconfStatus(context.Background(), common.HexToHash("0x1"))
		require.False(t, result)
		require.Error(t, err)
		require.Contains(t, err.Error(), "relay unreachable")
	})

	t.Run("passes correct hash to backend", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.preconfEnabled = true

		var capturedHash common.Hash
		b.checkPreconfStatusFn = func(hash common.Hash) (bool, error) {
			capturedHash = hash
			return true, nil
		}

		targetHash := common.HexToHash("0xabcdef")
		api := NewTransactionAPI(b, new(AddrLocker))
		_, _ = api.CheckPreconfStatus(context.Background(), targetHash)
		require.Equal(t, targetHash, capturedHash, "backend should receive the exact hash passed to API")
	})
}

func TestTxPoolAPI_TxStatus(t *testing.T) {
	t.Parallel()

	t.Run("returns TxStatusUnknown", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus { return txpool.TxStatusUnknown }

		api := NewTxPoolAPI(b)
		require.Equal(t, txpool.TxStatusUnknown, api.TxStatus(common.HexToHash("0x1")))
	})

	t.Run("returns TxStatusPending", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus { return txpool.TxStatusPending }

		api := NewTxPoolAPI(b)
		require.Equal(t, txpool.TxStatusPending, api.TxStatus(common.HexToHash("0x1")))
	})

	t.Run("passes correct hash to backend", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)

		var capturedHash common.Hash
		b.txStatusFn = func(hash common.Hash) txpool.TxStatus {
			capturedHash = hash
			return txpool.TxStatusPending
		}

		targetHash := common.HexToHash("0xabcdef")
		api := NewTxPoolAPI(b)
		api.TxStatus(targetHash)
		require.Equal(t, targetHash, capturedHash)
	})
}

func TestSendRawTransaction_PreconfPath(t *testing.T) {
	t.Parallel()

	t.Run("preconf disabled, SubmitTxForPreconf not called", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		// preconfEnabled defaults to false

		var preconfCount atomic.Int32
		b.submitTxForPreconfFn = func(tx *types.Transaction) error {
			preconfCount.Add(1)
			return nil
		}

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

		_, err := api.SendRawTransaction(context.Background(), raw)
		require.NoError(t, err)
		require.Equal(t, int32(0), preconfCount.Load(), "SubmitTxForPreconf should NOT be called when preconf is disabled")
	})

	t.Run("preconf enabled, SubmitTxForPreconf called", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.preconfEnabled = true

		var preconfCount atomic.Int32
		var capturedTxHash common.Hash
		b.submitTxForPreconfFn = func(tx *types.Transaction) error {
			preconfCount.Add(1)
			capturedTxHash = tx.Hash()
			return nil
		}

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransaction(context.Background(), raw)
		require.NoError(t, err)
		require.Equal(t, tx.Hash(), hash)
		require.Equal(t, int32(1), preconfCount.Load(), "SubmitTxForPreconf should be called once")
		require.Equal(t, tx.Hash(), capturedTxHash, "SubmitTxForPreconf should receive the correct tx")
	})

	t.Run("preconf enabled, SubmitTxForPreconf fails, error not propagated", func(t *testing.T) {
		t.Parallel()
		genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
		b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
		b.preconfEnabled = true
		b.submitTxForPreconfFn = func(tx *types.Transaction) error {
			return errors.New("relay down")
		}

		api := NewTransactionAPI(b, new(AddrLocker))
		raw, tx := makeSelfSignedRaw(t, api, b.acc.Address)

		hash, err := api.SendRawTransaction(context.Background(), raw)
		require.NoError(t, err, "SendRawTransaction should NOT propagate SubmitTxForPreconf errors")
		require.Equal(t, tx.Hash(), hash)
	})
}
func (b *testBackend) ProtocolVersion() uint {
	return 69 // ETH69
}

func (b *testBackend) GetWork() ([4]string, error) {
	// testBackend uses ethash (PoW) but doesn't implement mining work API
	return [4]string{}, errors.New("mining work API not implemented by backend")
}

func (b *testBackend) SubmitWork(_ types.BlockNonce, _, _ common.Hash) (bool, error) {
	// testBackend uses ethash (PoW) but doesn't implement mining work API
	return false, errors.New("mining work API not implemented by backend")
}

func (b *testBackend) SubmitHashrate(_ hexutil.Uint64, _ common.Hash) (bool, error) {
	// testBackend uses ethash (PoW) but doesn't implement mining work API
	return false, errors.New("mining work API not implemented by backend")
}

func (b *backendMock) Etherbase() (common.Address, error) {
	return common.Address{}, nil
}

func (b *backendMock) Hashrate() (uint64, error) {
	return 0, nil
}

func (b *backendMock) Mining() (bool, error) {
	return false, nil
}

func (b *backendMock) ProtocolVersion() uint {
	return 69 // ETH69
}

func (b *backendMock) GetWork() ([4]string, error) {
	return [4]string{}, errors.New("mining work API not implemented by backend")
}

func (b *backendMock) SubmitWork(_ types.BlockNonce, _, _ common.Hash) (bool, error) {
	return false, errors.New("mining work API not implemented by backend")
}

func (b *backendMock) SubmitHashrate(_ hexutil.Uint64, _ common.Hash) (bool, error) {
	return false, errors.New("mining work API not implemented by backend")
}

func makeSignedRaw(t *testing.T, api *TransactionAPI, from, to common.Address, value *big.Int) (hexutil.Bytes, *types.Transaction) {
	t.Helper()

	fillRes, err := api.FillTransaction(context.Background(), TransactionArgs{
		From:  &from,
		To:    &to,
		Value: (*hexutil.Big)(value),
	})
	if err != nil {
		t.Fatalf("FillTransaction failed: %v", err)
	}
	signRes, err := api.SignTransaction(context.Background(), argsFromTransaction(fillRes.Tx, from))
	if err != nil {
		t.Fatalf("SignTransaction failed: %v", err)
	}
	return signRes.Raw, signRes.Tx
}

// makeSelfSignedRaw is a convenience for a 0-ETH self-transfer.
func makeSelfSignedRaw(t *testing.T, api *TransactionAPI, addr common.Address) (hexutil.Bytes, *types.Transaction) {
	return makeSignedRaw(t, api, addr, addr, big.NewInt(0))
}

func TestSendRawTransactionSync_Success(t *testing.T) {
	t.Parallel()
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{},
	}
	b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
	b.autoMine = true // immediately “mines” the tx in-memory

	api := NewTransactionAPI(b, new(AddrLocker))

	raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

	receipt, err := api.SendRawTransactionSync(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receipt == nil {
		t.Fatalf("expected non-nil receipt")
	}
	if _, ok := receipt["blockNumber"]; !ok {
		t.Fatalf("expected blockNumber in receipt, got %#v", receipt)
	}
}

func TestSendRawTransactionSync_Timeout(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{},
	}
	b := newTestBackend(t, 0, genesis, ethash.NewFaker(), nil)
	b.autoMine = false // don't mine, should time out

	api := NewTransactionAPI(b, new(AddrLocker))

	raw, _ := makeSelfSignedRaw(t, api, b.acc.Address)

	timeout := uint64(200) // 200ms
	receipt, err := api.SendRawTransactionSync(context.Background(), raw, &timeout)

	if receipt != nil {
		t.Fatalf("expected nil receipt, got %#v", receipt)
	}
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	// assert error shape & data (hash)
	var de interface {
		ErrorCode() int
		ErrorData() interface{}
	}
	if !errors.As(err, &de) {
		t.Fatalf("expected data error with code/data, got %T %v", err, err)
	}
	if de.ErrorCode() != errCodeTxSyncTimeout {
		t.Fatalf("expected code %d, got %d", errCodeTxSyncTimeout, de.ErrorCode())
	}
	tx := new(types.Transaction)
	if e := tx.UnmarshalBinary(raw); e != nil {
		t.Fatal(e)
	}
	if got, want := de.ErrorData(), tx.Hash().Hex(); got != want {
		t.Fatalf("expected ErrorData=%s, got %v", want, got)
	}
}

func TestCoinbase(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks        = 5
		expectedCoinbase = common.HexToAddress("0x1234567890123456789012345678901234567890")
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})

	// Mock the Etherbase to return our expected address
	customBackend := &testBackendWithCoinbase{
		testBackend: backend,
		coinbase:    expectedCoinbase,
	}

	api := NewEthereumAPI(customBackend)
	coinbase, err := api.Coinbase()
	if err != nil {
		t.Fatalf("Coinbase() failed: %v", err)
	}

	if coinbase != expectedCoinbase {
		t.Errorf("Coinbase() = %v, want %v", coinbase, expectedCoinbase)
	}
}

func TestHashrate(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks               = 5
		expectedHashrate uint64 = 12345678
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})

	// Mock the Hashrate to return our expected value
	customBackend := &testBackendWithHashrate{
		testBackend: backend,
		hashrate:    expectedHashrate,
	}

	api := NewEthereumAPI(customBackend)
	hashrate, err := api.Hashrate()
	if err != nil {
		t.Fatalf("Hashrate() error = %v", err)
	}

	if uint64(hashrate) != expectedHashrate {
		t.Errorf("Hashrate() = %v, want %v", hashrate, expectedHashrate)
	}
}

func TestMining(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 5
	)

	// test "node not mining"
	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	customBackend := &testBackendWithMining{
		testBackend: backend,
		mining:      false,
	}
	api := NewEthereumAPI(customBackend)

	mining, err := api.Mining()
	if err != nil {
		t.Fatalf("Mining() error = %v", err)
	}
	if mining != false {
		t.Errorf("Mining() = %v, want false", mining)
	}

	// test "node mining"
	customBackend.mining = true
	mining, err = api.Mining()
	if err != nil {
		t.Fatalf("Mining() error = %v", err)
	}
	if mining != true {
		t.Errorf("Mining() = %v, want true", mining)
	}
}

func TestProtocolVersion(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 5
		// Expected protocol version (ETH69)
		expectedVersion uint = 69
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	customBackend := &testBackendWithProtocolVersion{
		testBackend:     backend,
		protocolVersion: expectedVersion,
	}

	api := NewEthereumAPI(customBackend)
	version := api.ProtocolVersion()

	if uint(version) != expectedVersion {
		t.Errorf("ProtocolVersion() = %v, want %v", version, expectedVersion)
	}
}

func TestGetWork(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 5
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	api := NewEthereumAPI(backend)

	// ethash.NewFaker() is a PoW engine, but Bor's mining backend doesn't implement GetWork
	work, err := api.GetWork()
	if err == nil {
		t.Errorf("GetWork() expected error, got nil")
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		if rpcErr.ErrorCode() != -32000 {
			t.Errorf("GetWork() error code = %d, want -32000 (server error)", rpcErr.ErrorCode())
		}
	}
	if work != [4]string{} {
		t.Errorf("GetWork() work = %v, want empty array", work)
	}
}

func TestSubmitWork(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 5
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	api := NewEthereumAPI(backend)

	// Test SubmitWork with PoW consensus (bor doesn't implement mining work)
	nonce := types.BlockNonce{}
	hash := common.Hash{}
	digest := common.Hash{}

	result, err := api.SubmitWork(nonce, hash, digest)
	if err == nil {
		t.Errorf("SubmitWork() expected error, got nil")
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		if rpcErr.ErrorCode() != -32000 {
			t.Errorf("SubmitWork() error code = %d, want -32000 (server error)", rpcErr.ErrorCode())
		}
	}
	if result != false {
		t.Errorf("SubmitWork() = %v, want false", result)
	}
}

func TestSubmitHashrate(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 5
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	api := NewEthereumAPI(backend)

	// Test SubmitHashrate with PoW consensus (bor doesn't implement mining work)
	rate := hexutil.Uint64(123456)
	id := common.Hash{}

	result, err := api.SubmitHashrate(rate, id)
	if err == nil {
		t.Errorf("SubmitHashrate() expected error, got nil")
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		if rpcErr.ErrorCode() != -32000 {
			t.Errorf("SubmitHashrate() error code = %d, want -32000 (server error)", rpcErr.ErrorCode())
		}
	}
	if result != false {
		t.Errorf("SubmitHashrate() = %v, want false", result)
	}
}

type testBackendCancelAccountAtReplay struct {
	*testBackend
	cancel   context.CancelFunc
	canceled bool
}

func (b *testBackendCancelAccountAtReplay) GetEVM(ctx context.Context, state *state.StateDB, header *types.Header, vmConfig *vm.Config, blockContext *vm.BlockContext) *vm.EVM {
	if !b.canceled {
		b.canceled = true
		b.cancel()
	}
	return b.testBackend.GetEVM(ctx, state, header, vmConfig, blockContext)
}

func TestAccountAt(t *testing.T) {
	t.Parallel()

	// Setup backend with some blocks
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr    = crypto.PubkeyToAddress(key.PublicKey)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		signer = types.LatestSigner(genesis.Config)
	)

	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		// Create a transaction that changes the account state
		toAddr := common.Address{0x01}
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    b.TxNonce(addr),
			To:       &toAddr,
			Value:    big.NewInt(1000),
			Gas:      21000,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), signer, key)
		b.AddTx(tx)
	})
	api := NewDebugAPI(backend)

	// Get the block 1 hash
	block, err := backend.BlockByNumber(context.Background(), rpc.BlockNumber(1))
	if err != nil {
		t.Fatalf("Failed to get block: %v", err)
	}
	blockHash := block.Hash()

	t.Run("valid block and transaction", func(t *testing.T) {
		// Query account state after the first transaction
		result, err := api.AccountAt(context.Background(), blockHash, 0, addr)
		if err != nil {
			t.Fatalf("AccountAt failed: %v", err)
		}
		if result == nil {
			t.Fatal("Expected non-nil result")
		}

		// Check that nonce increased
		if result.Nonce != 1 {
			t.Errorf("Expected nonce 1, got %d", result.Nonce)
		}

		// Check that the balance decreased
		expectedBalance := new(big.Int).Sub(big.NewInt(params.Ether), big.NewInt(1000))
		gasUsed := new(big.Int).Mul(big.NewInt(21000), block.BaseFee())
		expectedBalance.Sub(expectedBalance, gasUsed)

		if result.Balance.ToInt().Cmp(expectedBalance) != 0 {
			t.Logf("Expected balance %s, got %s", expectedBalance.String(), result.Balance.ToInt().String())
		}

		// Check that the code is empty (not a contract)
		if len(result.Code) != 0 {
			t.Errorf("Expected empty code, got %d bytes", len(result.Code))
		}
	})

	t.Run("non-existent block", func(t *testing.T) {
		nonExistentHash := common.HexToHash("0x1234567890123456789012345678901234567890123456789012345678901234")
		result, err := api.AccountAt(context.Background(), nonExistentHash, 0, addr)
		if err != nil {
			t.Fatalf("Expected no error for non-existent block, got: %v", err)
		}
		if result != nil {
			t.Error("Expected nil result for non-existent block")
		}
	})

	t.Run("invalid transaction index", func(t *testing.T) {
		// Query with an out-of-bounds index
		resultOutOfBounds, err := api.AccountAt(context.Background(), blockHash, 999, addr)
		if err != nil {
			t.Fatalf("AccountAt with out-of-bounds index failed: %v", err)
		}
		if resultOutOfBounds == nil {
			t.Fatal("Expected non-nil result even for out-of-range txIndex")
		}

		// Get the last transaction index (block has at least 1 tx)
		lastTxIdx := uint64(len(block.Transactions()) - 1)
		resultAtLast, err := api.AccountAt(context.Background(), blockHash, lastTxIdx, addr)
		if err != nil {
			t.Fatalf("AccountAt at last tx index failed: %v", err)
		}

		// Both queries should return the same state
		if resultOutOfBounds.Balance.ToInt().Cmp(resultAtLast.Balance.ToInt()) != 0 {
			t.Errorf("Out-of-bounds query balance mismatch: got %v, want %v",
				resultOutOfBounds.Balance.ToInt(), resultAtLast.Balance.ToInt())
		}
		if resultOutOfBounds.Nonce != resultAtLast.Nonce {
			t.Errorf("Out-of-bounds query nonce mismatch: got %v, want %v",
				resultOutOfBounds.Nonce, resultAtLast.Nonce)
		}
	})

	t.Run("non-existent account", func(t *testing.T) {
		// Query account that doesn't exist
		nonExistentAddr := common.HexToAddress("0x0000000000000000000000000000000000000099")
		result, err := api.AccountAt(context.Background(), blockHash, 0, nonExistentAddr)
		if err != nil {
			t.Fatalf("AccountAt failed: %v", err)
		}
		if result == nil {
			t.Fatal("Expected non-nil result even for non-existent account")
		}

		// Non-existent account should have zero balance and nonce
		if result.Balance.ToInt().Cmp(big.NewInt(0)) != 0 {
			t.Errorf("Expected zero balance for non-existent account, got %s", result.Balance.ToInt().String())
		}
		if result.Nonce != 0 {
			t.Errorf("Expected zero nonce for non-existent account, got %d", result.Nonce)
		}
	})

	t.Run("context cancellation during replay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		wrapped := &testBackendCancelAccountAtReplay{
			testBackend: backend,
			cancel:      cancel,
		}
		api := NewDebugAPI(wrapped)

		_, err := api.AccountAt(ctx, blockHash, 0, addr)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestCallWithStateMissingHeader(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(1)
	genesis := &core.Genesis{
		Config: params.MergedTestChainConfig,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	backend := newTestBackend(t, 1, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) { b.SetPoS() })
	api := NewBlockChainAPI(backend)

	statedb, _, err := backend.StateAndHeaderByNumberOrHash(context.Background(), rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
	require.NoError(t, err)

	// A block ref carrying both number and hash resolves by hash on the
	// consensus-internal path; an unknown hash must be a clear error, not a
	// nil result.
	blockNr := rpc.BlockNumber(1)
	phantom := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	_, err = api.CallWithState(context.Background(), TransactionArgs{From: &accounts[0].addr, To: &accounts[0].addr}, &rpc.BlockNumberOrHash{BlockNumber: &blockNr, BlockHash: &phantom}, statedb, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "header not found for hash")
}

// simFinalizingEngine wraps an engine with the simulation finalization hook so
// tests can exercise the simulationFinalizer dispatch in eth_simulateV1.
type simFinalizingEngine struct {
	consensus.Engine
	simFinalized bool
}

func (e *simFinalizingEngine) FinalizeAndAssembleForSimulation(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt) (*types.Block, []*types.Receipt, time.Duration, error) {
	e.simFinalized = true
	return e.Engine.FinalizeAndAssemble(chain, header, state, body, receipts)
}

func TestSimulateV1DispatchesSimulationFinalize(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	engine := &simFinalizingEngine{Engine: ethash.NewFaker()}
	api := NewBlockChainAPI(newTestBackend(t, 1, genesis, engine, func(i int, b *core.BlockGen) {}))

	latest := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	opts := simOpts{BlockStateCalls: []simBlock{{Calls: []TransactionArgs{{
		From:  &accounts[0].addr,
		To:    &accounts[1].addr,
		Value: (*hexutil.Big)(big.NewInt(1)),
	}}}}}
	res, err := api.SimulateV1(context.Background(), opts, &latest)
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.True(t, engine.simFinalized, "simulation finalization hook was not used")
}
