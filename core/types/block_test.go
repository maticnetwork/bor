// Copyright 2014 The go-ethereum Authors
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

package types

import (
	"bytes"
	gomath "math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/blocktest"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// from bcValidBlockTest.json, "SimpleTx"
func TestBlockEncoding(t *testing.T) {
	blockEnc := common.FromHex("f90260f901f9a083cafc574e1f51ba9dc0568fc617a08ea2429fb384059c972f13b19fa1c8dd55a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347948888f1f195afa192cfee860698584c030f4c9db1a0ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017a05fe50b260da6308036625b850b5d6ced6d0a9f814c0688bc91ffb7b7a3a54b67a0bc37d79753ad738a6dac4921e57392f145d8887476de3f783dfa7edae9283e52b90100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008302000001832fefd8825208845506eb0780a0bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff49888a13a5a8c8f2bb1c4f861f85f800a82c35094095e7baea6a6c7c4c2dfeb977efac326af552d870a801ba09bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094fa08a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b1c0")

	var block Block

	if err := rlp.DecodeBytes(blockEnc, &block); err != nil {
		t.Fatal("decode error: ", err)
	}

	check := func(f string, got, want interface{}) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s mismatch: got %v, want %v", f, got, want)
		}
	}
	check("Difficulty", block.Difficulty(), big.NewInt(131072))
	check("GasLimit", block.GasLimit(), uint64(3141592))
	check("GasUsed", block.GasUsed(), uint64(21000))
	check("Coinbase", block.Coinbase(), common.HexToAddress("8888f1f195afa192cfee860698584c030f4c9db1"))
	check("MixDigest", block.MixDigest(), common.HexToHash("bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff498"))
	check("Root", block.Root(), common.HexToHash("ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017"))
	check("Hash", block.Hash(), common.HexToHash("0a5843ac1cb04865017cb35a57b50b07084e5fcee39b5acadade33149f4fff9e"))
	check("Nonce", block.Nonce(), uint64(0xa13a5a8c8f2bb1c4))
	check("Time", block.Time(), uint64(1426516743))
	check("Size", block.Size(), uint64(len(blockEnc)))

	tx1 := NewTransaction(0, common.HexToAddress("095e7baea6a6c7c4c2dfeb977efac326af552d87"), big.NewInt(10), 50000, big.NewInt(10), nil)
	tx1, _ = tx1.WithSignature(HomesteadSigner{}, common.Hex2Bytes("9bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094f8a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b100"))

	check("len(Transactions)", len(block.Transactions()), 1)
	check("Transactions[0].Hash", block.Transactions()[0].Hash(), tx1.Hash())

	ourBlockEnc, err := rlp.EncodeToBytes(&block)
	if err != nil {
		t.Fatal("encode error: ", err)
	}

	if !bytes.Equal(ourBlockEnc, blockEnc) {
		t.Errorf("encoded block mismatch:\ngot:  %x\nwant: %x", ourBlockEnc, blockEnc)
	}
}

// This is a replica of `(h *Header) GetValidatorBytes` function
func GetValidatorBytesTest(h *Header) []byte {
	if len(h.Extra) < ExtraVanityLength+ExtraSealLength {
		log.Error("length of extra less is than vanity and seal")
		return nil
	}

	var blockExtraData BlockExtraData
	if err := rlp.DecodeBytes(h.Extra[ExtraVanityLength:len(h.Extra)-ExtraSealLength], &blockExtraData); err != nil {
		log.Debug("error while decoding block extra data", "err", err)
		return nil
	}

	return blockExtraData.ValidatorBytes
}

func TestValidatorBytesBlockDecoding(t *testing.T) {
	t.Parallel()

	blockEnc := common.FromHex("f9037df90270a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000948888f1f195afa192cfee860698584c030f4c9db1a0ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000b90100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008302000080832fefd8825208845506eb07b8710000000000000000000000000000000000000000000000000000000000000000cf8776616c20736574c6c20201c201800000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000a0bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff49888a13a5a8c8f2bb1c4843b9aca00f90106f85f800a82c35094095e7baea6a6c7c4c2dfeb977efac326af552d870a801ba09bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094fa08a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b1b8a302f8a0018080843b9aca008301e24194095e7baea6a6c7c4c2dfeb977efac326af552d878080f838f7940000000000000000000000000000000000000001e1a0000000000000000000000000000000000000000000000000000000000000000080a0fe38ca4e44a30002ac54af7cf922a6ac2ba11b7d22f548e8ecb3f51f41cb31b0a06de6a5cbae13c0c856e33acf021b51819636cfc009d39eafb9f606d546e305a8c0")

	var block Block

	if err := rlp.DecodeBytes(blockEnc, &block); err != nil {
		t.Fatal("decode error: ", err)
	}

	check := func(f string, got, want interface{}) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s mismatch: got %v, want %v", f, got, want)
		}
	}

	check("Difficulty", block.Difficulty(), big.NewInt(131072))
	check("GasLimit", block.GasLimit(), uint64(3141592))
	check("GasUsed", block.GasUsed(), uint64(21000))
	check("Coinbase", block.Coinbase(), common.HexToAddress("8888f1f195afa192cfee860698584c030f4c9db1"))
	check("MixDigest", block.MixDigest(), common.HexToHash("bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff498"))
	check("Root", block.Root(), common.HexToHash("ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017"))
	check("Hash", block.Hash(), common.HexToHash("0xd41b39190a90b9c047ed091c4c77ba03289fef1ae6e87600ab60f7242d63e4a5"))
	check("Nonce", block.Nonce(), uint64(0xa13a5a8c8f2bb1c4))
	check("Time", block.Time(), uint64(1426516743))
	check("Size", block.Size(), uint64(len(blockEnc)))

	tx1 := NewTransaction(0, common.HexToAddress("095e7baea6a6c7c4c2dfeb977efac326af552d87"), big.NewInt(10), 50000, big.NewInt(10), nil)
	tx1, _ = tx1.WithSignature(HomesteadSigner{}, common.Hex2Bytes("9bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094f8a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b100"))

	validatorBytes := GetValidatorBytesTest(block.header)

	check("validatorBytes", validatorBytes, []byte("val set"))

	addr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	accesses := AccessList{AccessTuple{
		Address: addr,
		StorageKeys: []common.Hash{
			{0},
		},
	}}
	to := common.HexToAddress("095e7baea6a6c7c4c2dfeb977efac326af552d87")
	txdata := &DynamicFeeTx{
		ChainID:    big.NewInt(1),
		Nonce:      0,
		To:         &to,
		Gas:        123457,
		GasFeeCap:  new(big.Int).Set(block.BaseFee()),
		GasTipCap:  big.NewInt(0),
		AccessList: accesses,
		Data:       []byte{},
	}
	tx2 := NewTx(txdata)

	tx2, err := tx2.WithSignature(LatestSignerForChainID(big.NewInt(1)), common.Hex2Bytes("fe38ca4e44a30002ac54af7cf922a6ac2ba11b7d22f548e8ecb3f51f41cb31b06de6a5cbae13c0c856e33acf021b51819636cfc009d39eafb9f606d546e305a800"))
	if err != nil {
		t.Fatal("invalid signature error: ", err)
	}

	check("len(Transactions)", len(block.Transactions()), 2)
	check("Transactions[0].Hash", block.Transactions()[0].Hash(), tx1.Hash())
	check("Transactions[1].Hash", block.Transactions()[1].Hash(), tx2.Hash())
	check("Transactions[1].Type", block.Transactions()[1].Type(), tx2.Type())

	ourBlockEnc, err := rlp.EncodeToBytes(&block)

	if err != nil {
		t.Fatal("encode error: ", err)
	}

	if !bytes.Equal(ourBlockEnc, blockEnc) {
		t.Errorf("encoded block mismatch:\ngot:  %x\nwant: %x", ourBlockEnc, blockEnc)
	}
}

func TestEIP1559BlockEncoding(t *testing.T) {
	blockEnc := common.FromHex("f9030bf901fea083cafc574e1f51ba9dc0568fc617a08ea2429fb384059c972f13b19fa1c8dd55a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347948888f1f195afa192cfee860698584c030f4c9db1a0ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017a05fe50b260da6308036625b850b5d6ced6d0a9f814c0688bc91ffb7b7a3a54b67a0bc37d79753ad738a6dac4921e57392f145d8887476de3f783dfa7edae9283e52b90100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000008302000001832fefd8825208845506eb0780a0bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff49888a13a5a8c8f2bb1c4843b9aca00f90106f85f800a82c35094095e7baea6a6c7c4c2dfeb977efac326af552d870a801ba09bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094fa08a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b1b8a302f8a0018080843b9aca008301e24194095e7baea6a6c7c4c2dfeb977efac326af552d878080f838f7940000000000000000000000000000000000000001e1a0000000000000000000000000000000000000000000000000000000000000000080a0fe38ca4e44a30002ac54af7cf922a6ac2ba11b7d22f548e8ecb3f51f41cb31b0a06de6a5cbae13c0c856e33acf021b51819636cfc009d39eafb9f606d546e305a8c0")

	var block Block

	if err := rlp.DecodeBytes(blockEnc, &block); err != nil {
		t.Fatal("decode error: ", err)
	}

	check := func(f string, got, want interface{}) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s mismatch: got %v, want %v", f, got, want)
		}
	}

	check("Difficulty", block.Difficulty(), big.NewInt(131072))
	check("GasLimit", block.GasLimit(), uint64(3141592))
	check("GasUsed", block.GasUsed(), uint64(21000))
	check("Coinbase", block.Coinbase(), common.HexToAddress("8888f1f195afa192cfee860698584c030f4c9db1"))
	check("MixDigest", block.MixDigest(), common.HexToHash("bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff498"))
	check("Root", block.Root(), common.HexToHash("ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017"))
	check("Hash", block.Hash(), common.HexToHash("c7252048cd273fe0dac09650027d07f0e3da4ee0675ebbb26627cea92729c372"))
	check("Nonce", block.Nonce(), uint64(0xa13a5a8c8f2bb1c4))
	check("Time", block.Time(), uint64(1426516743))
	check("Size", block.Size(), uint64(len(blockEnc)))
	check("BaseFee", block.BaseFee(), new(big.Int).SetUint64(params.InitialBaseFee))

	tx1 := NewTransaction(0, common.HexToAddress("095e7baea6a6c7c4c2dfeb977efac326af552d87"), big.NewInt(10), 50000, big.NewInt(10), nil)
	tx1, _ = tx1.WithSignature(HomesteadSigner{}, common.Hex2Bytes("9bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094f8a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b100"))

	addr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	accesses := AccessList{AccessTuple{
		Address: addr,
		StorageKeys: []common.Hash{
			{0},
		},
	}}
	to := common.HexToAddress("095e7baea6a6c7c4c2dfeb977efac326af552d87")
	txdata := &DynamicFeeTx{
		ChainID:    big.NewInt(1),
		Nonce:      0,
		To:         &to,
		Gas:        123457,
		GasFeeCap:  new(big.Int).Set(block.BaseFee()),
		GasTipCap:  big.NewInt(0),
		AccessList: accesses,
		Data:       []byte{},
	}
	tx2 := NewTx(txdata)

	tx2, err := tx2.WithSignature(LatestSignerForChainID(big.NewInt(1)), common.Hex2Bytes("fe38ca4e44a30002ac54af7cf922a6ac2ba11b7d22f548e8ecb3f51f41cb31b06de6a5cbae13c0c856e33acf021b51819636cfc009d39eafb9f606d546e305a800"))
	if err != nil {
		t.Fatal("invalid signature error: ", err)
	}

	check("len(Transactions)", len(block.Transactions()), 2)
	check("Transactions[0].Hash", block.Transactions()[0].Hash(), tx1.Hash())
	check("Transactions[1].Hash", block.Transactions()[1].Hash(), tx2.Hash())
	check("Transactions[1].Type", block.Transactions()[1].Type(), tx2.Type())

	ourBlockEnc, err := rlp.EncodeToBytes(&block)
	if err != nil {
		t.Fatal("encode error: ", err)
	}

	if !bytes.Equal(ourBlockEnc, blockEnc) {
		t.Errorf("encoded block mismatch:\ngot:  %x\nwant: %x", ourBlockEnc, blockEnc)
	}
}

func TestEIP2718BlockEncoding(t *testing.T) {
	blockEnc := common.FromHex("f90319f90211a00000000000000000000000000000000000000000000000000000000000000000a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347948888f1f195afa192cfee860698584c030f4c9db1a0ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017a0e6e49996c7ec59f7a23d22b83239a60151512c65613bf84a0d7da336399ebc4aa0cafe75574d59780665a97fbfd11365c7545aa8f1abf4e5e12e8243334ef7286bb901000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000083020000820200832fefd882a410845506eb0796636f6f6c65737420626c6f636b206f6e20636861696ea0bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff49888a13a5a8c8f2bb1c4f90101f85f800a82c35094095e7baea6a6c7c4c2dfeb977efac326af552d870a801ba09bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094fa08a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b1b89e01f89b01800a8301e24194095e7baea6a6c7c4c2dfeb977efac326af552d878080f838f7940000000000000000000000000000000000000001e1a0000000000000000000000000000000000000000000000000000000000000000001a03dbacc8d0259f2508625e97fdfc57cd85fdd16e5821bc2c10bdd1a52649e8335a0476e10695b183a87b0aa292a7f4b78ef0c3fbe62aa2c42c84e1d9c3da159ef14c0")

	var block Block

	if err := rlp.DecodeBytes(blockEnc, &block); err != nil {
		t.Fatal("decode error: ", err)
	}

	check := func(f string, got, want interface{}) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s mismatch: got %v, want %v", f, got, want)
		}
	}
	check("Difficulty", block.Difficulty(), big.NewInt(131072))
	check("GasLimit", block.GasLimit(), uint64(3141592))
	check("GasUsed", block.GasUsed(), uint64(42000))
	check("Coinbase", block.Coinbase(), common.HexToAddress("8888f1f195afa192cfee860698584c030f4c9db1"))
	check("MixDigest", block.MixDigest(), common.HexToHash("bd4472abb6659ebe3ee06ee4d7b72a00a9f4d001caca51342001075469aff498"))
	check("Root", block.Root(), common.HexToHash("ef1552a40b7165c3cd773806b9e0c165b75356e0314bf0706f279c729f51e017"))
	check("Nonce", block.Nonce(), uint64(0xa13a5a8c8f2bb1c4))
	check("Time", block.Time(), uint64(1426516743))
	check("Size", block.Size(), uint64(len(blockEnc)))

	// Create legacy tx.
	to := common.HexToAddress("095e7baea6a6c7c4c2dfeb977efac326af552d87")
	tx1 := NewTx(&LegacyTx{
		Nonce:    0,
		To:       &to,
		Value:    big.NewInt(10),
		Gas:      50000,
		GasPrice: big.NewInt(10),
	})
	sig := common.Hex2Bytes("9bea4c4daac7c7c52e093e6a4c35dbbcf8856f1af7b059ba20253e70848d094f8a8fae537ce25ed8cb5af9adac3f141af69bd515bd2ba031522df09b97dd72b100")
	tx1, _ = tx1.WithSignature(HomesteadSigner{}, sig)

	// Create ACL tx.
	addr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	tx2 := NewTx(&AccessListTx{
		ChainID:    big.NewInt(1),
		Nonce:      0,
		To:         &to,
		Gas:        123457,
		GasPrice:   big.NewInt(10),
		AccessList: AccessList{{Address: addr, StorageKeys: []common.Hash{{0}}}},
	})
	sig2 := common.Hex2Bytes("3dbacc8d0259f2508625e97fdfc57cd85fdd16e5821bc2c10bdd1a52649e8335476e10695b183a87b0aa292a7f4b78ef0c3fbe62aa2c42c84e1d9c3da159ef1401")
	tx2, _ = tx2.WithSignature(NewEIP2930Signer(big.NewInt(1)), sig2)

	check("len(Transactions)", len(block.Transactions()), 2)
	check("Transactions[0].Hash", block.Transactions()[0].Hash(), tx1.Hash())
	check("Transactions[1].Hash", block.Transactions()[1].Hash(), tx2.Hash())
	check("Transactions[1].Type()", block.Transactions()[1].Type(), uint8(AccessListTxType))

	ourBlockEnc, err := rlp.EncodeToBytes(&block)
	if err != nil {
		t.Fatal("encode error: ", err)
	}
	if !bytes.Equal(ourBlockEnc, blockEnc) {
		t.Errorf("encoded block mismatch:\ngot:  %x\nwant: %x", ourBlockEnc, blockEnc)
	}
}

func TestEIP4844BlockEncoding(t *testing.T) {
	// https://github.com/ethereum/tests/blob/develop/BlockchainTests/ValidBlocks/bcEIP4844-blobtransactions/blockWithAllTransactionTypes.json
	blockEnc := common.FromHex("0xf90417f90244a05eb7f6da0f3e237c62bcae48b7fb5f4506d392616b62890429c8b76b4a1d4104a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d4934794ba5e000000000000000000000000000000000000a011639dcca0b44f2acb5b630a82c8a69cb82742b3711383ec4e111a554d27aea5a05cb644f722e31f9792a8ef6e2a762334e1a862e8b40c1612e1e9507fd7121ef9a00c82719448356ba6807d6edfcd8e5aea575a5e97f36038ffb3e395749b26d41cb9010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000800188016345785d8a00008301482082079e42a00000000000000000000000000000000000000000000000000000000000020000880000000000000000820314a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b4218302000080a00000000000000000000000000000000000000000000000000000000000000000f901cbf864808203e885e8d4a5100094100000000000000000000000000000000000000a01801ca09de4adda6288582a6700dbcd8eb70c0a4a7fc9487d965f7bf22424e0bd121095a01cdb078764cc3770d5db847e99e10333aa7c356247baaf09b03eae04d64e7926b86901f86601018203e885e8d4a5100094100000000000000000000000000000000000000a0380c080a025090740da12684493e4fb466a3979e365b194e8cf462edf3c2c3be2f130bb2ea034fa18fb4c1bff4d957d72e28535d27f1352517a942aeaca0ed944085f0cd8bbb86a02f8670102018203e885e8d4a5100094100000000000000000000000000000000000000a0580c080a0352a7be5002ce111bc5167f3addf97a75e2e0b810d826af71d2caae18aed284ea065d38f8a5c8948ce706842e8861fb21020b93a4d5e489162a0e6d419a457b735b88c03f8890103018203e885e8d4a5100094100000000000000000000000000000000000000a0780c00ae1a001a915e4d060149eb4365960e6a7a45f334393093061116b197e3240065ff2d8809f638144c46d5de7a9e630c0e7c5c63ae829ecfd8cc94715d9c29fe17c464de0a06c5fc54c3aa868ba35ef31a4e12431611631ab7bcdceb4214dd273d83f73b5e1c0c0")
	var block Block
	if err := rlp.DecodeBytes(blockEnc, &block); err != nil {
		t.Fatal("decode error: ", err)
	}

	check := func(f string, got, want interface{}) {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s mismatch: got %v, want %v", f, got, want)
		}
	}
	check("Difficulty", block.Difficulty(), big.NewInt(0))
	check("GasLimit", block.GasLimit(), hexutil.MustDecodeUint64("0x16345785d8a0000"))
	check("GasUsed", block.GasUsed(), hexutil.MustDecodeUint64("0x14820"))
	check("Coinbase", block.Coinbase(), common.HexToAddress("0xba5e000000000000000000000000000000000000"))
	check("MixDigest", block.MixDigest(), common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000020000"))
	check("Root", block.Root(), common.HexToHash("0x11639dcca0b44f2acb5b630a82c8a69cb82742b3711383ec4e111a554d27aea5"))
	check("WithdrawalRoot", *block.Header().WithdrawalsHash, common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"))
	check("Nonce", block.Nonce(), uint64(0))
	check("Time", block.Time(), hexutil.MustDecodeUint64("0x79e"))
	check("Size", block.Size(), uint64(len(blockEnc)))

	// Create blob tx.
	tx := NewTx(&BlobTx{
		ChainID:    uint256.NewInt(1),
		Nonce:      3,
		To:         common.HexToAddress("0x100000000000000000000000000000000000000a"),
		Gas:        hexutil.MustDecodeUint64("0xe8d4a51000"),
		GasTipCap:  uint256.MustFromHex("0x1"),
		GasFeeCap:  uint256.MustFromHex("0x3e8"),
		BlobFeeCap: uint256.MustFromHex("0xa"),
		BlobHashes: []common.Hash{
			common.BytesToHash(hexutil.MustDecode("0x01a915e4d060149eb4365960e6a7a45f334393093061116b197e3240065ff2d8")),
		},
		Value: uint256.MustFromHex("0x7"),
	})
	sig := common.Hex2Bytes("00638144c46d5de7a9e630c0e7c5c63ae829ecfd8cc94715d9c29fe17c464de06c5fc54c3aa868ba35ef31a4e12431611631ab7bcdceb4214dd273d83f73b5e100")
	tx, _ = tx.WithSignature(LatestSignerForChainID(big.NewInt(1)), sig)

	check("len(Transactions)", len(block.Transactions()), 4)
	check("Transactions[3].Hash", block.Transactions()[3].Hash(), tx.Hash())
	check("Transactions[3].Type()", block.Transactions()[3].Type(), uint8(BlobTxType))

	ourBlockEnc, err := rlp.EncodeToBytes(&block)
	if err != nil {
		t.Fatal("encode error: ", err)
	}

	if !bytes.Equal(ourBlockEnc, blockEnc) {
		t.Errorf("encoded block mismatch:\ngot:  %x\nwant: %x", ourBlockEnc, blockEnc)
	}
}

func TestUncleHash(t *testing.T) {
	uncles := make([]*Header, 0)
	h := CalcUncleHash(uncles)
	exp := EmptyUncleHash
	if h != exp {
		t.Fatalf("empty uncle hash is wrong, got %x != %x", h, exp)
	}
}

var benchBuffer = bytes.NewBuffer(make([]byte, 0, 32000))

func BenchmarkEncodeBlock(b *testing.B) {
	block := makeBenchBlock()

	for b.Loop() {
		benchBuffer.Reset()

		if err := rlp.Encode(benchBuffer, block); err != nil {
			b.Fatal(err)
		}
	}
}

func makeBenchBlock() *Block {
	var (
		key, _   = crypto.GenerateKey()
		txs      = make([]*Transaction, 70)
		receipts = make([]*Receipt, len(txs))
		signer   = LatestSigner(params.TestChainConfig)
		uncles   = make([]*Header, 3)
	)

	header := &Header{
		Difficulty: math.BigPow(11, 11),
		Number:     math.BigPow(2, 9),
		GasLimit:   12345678,
		GasUsed:    1476322,
		Time:       9876543,
		Extra:      []byte("coolest block on chain"),
	}

	for i := range txs {
		amount := math.BigPow(2, int64(i))
		price := big.NewInt(300000)
		data := make([]byte, 100)
		tx := NewTransaction(uint64(i), common.Address{}, amount, 123457, price, data)

		signedTx, err := SignTx(tx, signer, key)
		if err != nil {
			panic(err)
		}

		txs[i] = signedTx
		receipts[i] = NewReceipt(make([]byte, 32), false, tx.Gas())
	}

	for i := range uncles {
		uncles[i] = &Header{
			Difficulty: math.BigPow(11, 11),
			Number:     math.BigPow(2, 9),
			GasLimit:   12345678,
			GasUsed:    1476322,
			Time:       9876543,
			Extra:      []byte("benchmark uncle"),
		}
	}
	return NewBlock(header, &Body{Transactions: txs, Uncles: uncles}, receipts, blocktest.NewHasher())
}

func TestRlpDecodeParentHash(t *testing.T) {
	// A minimum one
	want := common.HexToHash("0x112233445566778899001122334455667788990011223344556677889900aabb")
	if rlpData, err := rlp.EncodeToBytes(&Header{ParentHash: want}); err != nil {
		t.Fatal(err)
	} else {
		if have := HeaderParentHashFromRLP(rlpData); have != want {
			t.Fatalf("have %x, want %x", have, want)
		}
	}
	// And a maximum one
	// | Difficulty  | dynamic| *big.Int       | 0x5ad3c2c71bbff854908 (current mainnet TD: 76 bits) |
	// | Number      | dynamic| *big.Int       | 64 bits               |
	// | Extra       | dynamic| []byte         | 65+32 byte (clique)   |
	// | BaseFee     | dynamic| *big.Int       | 64 bits               |
	mainnetTd := new(big.Int)
	mainnetTd.SetString("5ad3c2c71bbff854908", 16)

	if rlpData, err := rlp.EncodeToBytes(&Header{
		ParentHash: want,
		Difficulty: mainnetTd,
		Number:     new(big.Int).SetUint64(gomath.MaxUint64),
		Extra:      make([]byte, 65+32),
		BaseFee:    new(big.Int).SetUint64(gomath.MaxUint64),
	}); err != nil {
		t.Fatal(err)
	} else {
		if have := HeaderParentHashFromRLP(rlpData); have != want {
			t.Fatalf("have %x, want %x", have, want)
		}
	}
	// Also test a very very large header.
	{
		// The rlp-encoding of the header belowCauses _total_ length of 65540,
		// which is the first to blow the fast-path.
		h := &Header{
			ParentHash: want,
			Extra:      make([]byte, 65041),
		}
		if rlpData, err := rlp.EncodeToBytes(h); err != nil {
			t.Fatal(err)
		} else {
			if have := HeaderParentHashFromRLP(rlpData); have != want {
				t.Fatalf("have %x, want %x", have, want)
			}
		}
	}
	{
		// Test some invalid erroneous stuff
		for i, rlpData := range [][]byte{
			nil,
			common.FromHex("0x"),
			common.FromHex("0x01"),
			common.FromHex("0x3031323334"),
		} {
			if have, want := HeaderParentHashFromRLP(rlpData), (common.Hash{}); have != want {
				t.Fatalf("invalid %d: have %x, want %x", i, have, want)
			}
		}
	}
}

func TestValidateBlockNumberOptionsPIP15(t *testing.T) {
	t.Parallel()

	testsPass := []struct {
		number         string
		header         Header
		minBlockNumber *big.Int
		maxBlockNumber *big.Int
	}{
		{
			"1",
			Header{Number: big.NewInt(10)},
			big.NewInt(0),
			big.NewInt(20),
		},
		{
			"2",
			Header{Number: big.NewInt(10)},
			big.NewInt(10),
			big.NewInt(10),
		},
		{
			"3",
			Header{Number: big.NewInt(10)},
			big.NewInt(10),
			big.NewInt(11),
		},
		{
			"4",
			Header{Number: big.NewInt(10)},
			big.NewInt(0),
			big.NewInt(10),
		},
	}

	testsFail := []struct {
		number         string
		header         Header
		minBlockNumber *big.Int
		maxBlockNumber *big.Int
	}{
		{
			"5",
			Header{Number: big.NewInt(10)},
			big.NewInt(0),
			big.NewInt(0),
		},
		{
			"6",
			Header{Number: big.NewInt(10)},
			big.NewInt(0),
			big.NewInt(9),
		},
		{
			"7",
			Header{Number: big.NewInt(10)},
			big.NewInt(11),
			big.NewInt(9),
		},
		{
			"8",
			Header{Number: big.NewInt(10)},
			big.NewInt(11),
			big.NewInt(20),
		},
	}

	for _, test := range testsPass {
		if err := test.header.ValidateBlockNumberOptionsPIP15(test.minBlockNumber, test.maxBlockNumber); err != nil {
			t.Fatalf("test number %v should not have failed. err: %v", test.number, err)
		}
	}

	for _, test := range testsFail {
		if err := test.header.ValidateBlockNumberOptionsPIP15(test.minBlockNumber, test.maxBlockNumber); err == nil {
			t.Fatalf("test number %v should have failed. err is nil", test.number)
		}
	}
}

func TestValidateTimestampOptionsPIP15(t *testing.T) {
	t.Parallel()

	u64Ptr := func(n uint64) *uint64 {
		return &n
	}

	testsPass := []struct {
		number       string
		header       Header
		minTimestamp *uint64
		maxTimestamp *uint64
	}{
		{
			"1",
			Header{Time: 1600000000},
			u64Ptr(1500000000),
			u64Ptr(1700000000),
		},
		{
			"2",
			Header{Time: 1600000000},
			u64Ptr(1600000000),
			u64Ptr(1600000000),
		},
		{
			"3",
			Header{Time: 1600000000},
			u64Ptr(1600000000),
			u64Ptr(1700000000),
		},
		{
			"4",
			Header{Time: 1600000000},
			u64Ptr(1500000000),
			u64Ptr(1600000000),
		},
	}

	testsFail := []struct {
		number       string
		header       Header
		minTimestamp *uint64
		maxTimestamp *uint64
	}{
		{
			"5",
			Header{Time: 1600000000},
			u64Ptr(1500000000),
			u64Ptr(1500000000),
		},
		{
			"6",
			Header{Time: 1600000000},
			u64Ptr(1400000000),
			u64Ptr(1500000000),
		},
		{
			"7",
			Header{Time: 1600000000},
			u64Ptr(1700000000),
			u64Ptr(1500000000),
		},
		{
			"8",
			Header{Time: 1600000000},
			u64Ptr(1700000000),
			u64Ptr(1800000000),
		},
	}

	for _, test := range testsPass {
		if err := test.header.ValidateTimestampOptionsPIP15(test.minTimestamp, test.maxTimestamp); err != nil {
			t.Fatalf("test number %v should not have failed. err: %v", test.number, err)
		}
	}

	for _, test := range testsFail {
		if err := test.header.ValidateTimestampOptionsPIP15(test.minTimestamp, test.maxTimestamp); err == nil {
			t.Fatalf("test number %v should have failed. err is nil", test.number)
		}
	}
}

func TestHeaderGetActualTime(t *testing.T) {
	t.Parallel()

	t.Run("ActualTime is set - returns ActualTime", func(t *testing.T) {
		actualTime := time.Unix(1234567890, 0)
		header := &Header{
			Time:       1111111111,
			ActualTime: actualTime,
		}

		result := header.GetActualTime()
		if !result.Equal(actualTime) {
			t.Errorf("expected ActualTime %v, got %v", actualTime, result)
		}
	})

	t.Run("ActualTime is zero - returns time.Unix(Time, 0)", func(t *testing.T) {
		headerTime := uint64(1600000000)
		header := &Header{
			Time: headerTime,
			// ActualTime is not set (zero value)
		}

		result := header.GetActualTime()
		expected := time.Unix(int64(headerTime), 0)
		if !result.Equal(expected) {
			t.Errorf("expected time.Unix(%d, 0) = %v, got %v", headerTime, expected, result)
		}
	})

	t.Run("both Time and ActualTime zero - returns Unix epoch", func(t *testing.T) {
		header := &Header{
			Time: 0,
			// ActualTime is not set (zero value)
		}

		result := header.GetActualTime()
		expected := time.Unix(0, 0)
		if !result.Equal(expected) {
			t.Errorf("expected Unix epoch %v, got %v", expected, result)
		}
	})

	t.Run("far future time - ActualTime set", func(t *testing.T) {
		farFuture := time.Unix(9999999999, 0)
		header := &Header{
			Time:       5555555555,
			ActualTime: farFuture,
		}

		result := header.GetActualTime()
		if !result.Equal(farFuture) {
			t.Errorf("expected far future time %v, got %v", farFuture, result)
		}
	})

	t.Run("far future time - ActualTime not set", func(t *testing.T) {
		farFutureTimestamp := uint64(9999999999)
		header := &Header{
			Time: farFutureTimestamp,
		}

		result := header.GetActualTime()
		expected := time.Unix(int64(farFutureTimestamp), 0)
		if !result.Equal(expected) {
			t.Errorf("expected time %v, got %v", expected, result)
		}
	})
}

func TestHeaderSanityRejectsBitlenOver64(t *testing.T) {
	h := &Header{
		Difficulty: new(big.Int).Lsh(big.NewInt(1), 64), // bitlen=65
	}
	if err := h.SanityCheck(); err == nil {
		t.Fatalf("expected sanity check to reject difficulty bitlen > 64")
	}
}

func TestBlockExtraDataRLPBackwardCompatibility(t *testing.T) {
	t.Parallel()

	// Pre-Giugliano: 2-field struct without optional fields
	preGiugliano := &BlockExtraData{
		ValidatorBytes: []byte{0x01, 0x02},
		TxDependency:   [][]uint64{{1, 2}, {3}},
	}
	encoded, err := rlp.EncodeToBytes(preGiugliano)
	if err != nil {
		t.Fatalf("failed to encode pre-Giugliano BlockExtraData: %v", err)
	}

	var decoded BlockExtraData
	if err := rlp.DecodeBytes(encoded, &decoded); err != nil {
		t.Fatalf("failed to decode pre-Giugliano BlockExtraData: %v", err)
	}

	if !bytes.Equal(decoded.ValidatorBytes, preGiugliano.ValidatorBytes) {
		t.Errorf("ValidatorBytes mismatch: got %x, want %x", decoded.ValidatorBytes, preGiugliano.ValidatorBytes)
	}
	if decoded.GasTarget != nil {
		t.Errorf("GasTarget should be nil for pre-Giugliano, got %d", *decoded.GasTarget)
	}
	if decoded.BaseFeeChangeDenominator != nil {
		t.Errorf("BaseFeeChangeDenominator should be nil for pre-Giugliano, got %d", *decoded.BaseFeeChangeDenominator)
	}

	// Post-Giugliano: 4-field struct with optional fields populated
	gasTarget := uint64(15000000)
	bfcd := uint64(64)
	postGiugliano := &BlockExtraData{
		ValidatorBytes:           []byte{0x03, 0x04},
		TxDependency:             [][]uint64{{5}},
		GasTarget:                &gasTarget,
		BaseFeeChangeDenominator: &bfcd,
	}
	encoded, err = rlp.EncodeToBytes(postGiugliano)
	if err != nil {
		t.Fatalf("failed to encode post-Giugliano BlockExtraData: %v", err)
	}

	var decoded2 BlockExtraData
	if err := rlp.DecodeBytes(encoded, &decoded2); err != nil {
		t.Fatalf("failed to decode post-Giugliano BlockExtraData: %v", err)
	}

	if decoded2.GasTarget == nil || *decoded2.GasTarget != gasTarget {
		t.Errorf("GasTarget mismatch: got %v, want %d", decoded2.GasTarget, gasTarget)
	}
	if decoded2.BaseFeeChangeDenominator == nil || *decoded2.BaseFeeChangeDenominator != bfcd {
		t.Errorf("BaseFeeChangeDenominator mismatch: got %v, want %d", decoded2.BaseFeeChangeDenominator, bfcd)
	}
}

func TestGetBaseFeeParams(t *testing.T) {
	t.Parallel()

	cancunBlock := big.NewInt(100)
	chainConfig := &params.ChainConfig{
		ChainID:     big.NewInt(137),
		CancunBlock: cancunBlock,
	}

	// Helper to build extra data with BlockExtraData
	buildExtra := func(bed *BlockExtraData) []byte {
		vanity := make([]byte, ExtraVanityLength)
		seal := make([]byte, ExtraSealLength)
		encoded, _ := rlp.EncodeToBytes(bed)
		extra := append(vanity, encoded...)
		extra = append(extra, seal...)
		return extra
	}

	// Post-Cancun header with Giugliano fields
	gasTarget := uint64(15000000)
	bfcd := uint64(64)
	header := &Header{
		Number: big.NewInt(200),
		Extra: buildExtra(&BlockExtraData{
			ValidatorBytes:           nil,
			TxDependency:             nil,
			GasTarget:                &gasTarget,
			BaseFeeChangeDenominator: &bfcd,
		}),
	}

	gt, d := header.GetBaseFeeParams(chainConfig)
	if gt == nil || *gt != gasTarget {
		t.Errorf("expected gasTarget %d, got %v", gasTarget, gt)
	}
	if d == nil || *d != bfcd {
		t.Errorf("expected baseFeeChangeDenominator %d, got %v", bfcd, d)
	}

	// Pre-Cancun header should return nil, nil
	preCancunHeader := &Header{
		Number: big.NewInt(50),
		Extra:  header.Extra,
	}
	gt, d = preCancunHeader.GetBaseFeeParams(chainConfig)
	if gt != nil || d != nil {
		t.Errorf("expected nil for pre-Cancun, got gasTarget=%v, bfcd=%v", gt, d)
	}

	// Pre-Giugliano data (no optional fields) should return nil, nil
	preGiuglianoHeader := &Header{
		Number: big.NewInt(200),
		Extra: buildExtra(&BlockExtraData{
			ValidatorBytes: nil,
			TxDependency:   nil,
		}),
	}
	gt, d = preGiuglianoHeader.GetBaseFeeParams(chainConfig)
	if gt != nil || d != nil {
		t.Errorf("expected nil for pre-Giugliano extra data, got gasTarget=%v, bfcd=%v", gt, d)
	}

	// Short extra data should return nil, nil
	shortHeader := &Header{
		Number: big.NewInt(200),
		Extra:  []byte{0x01, 0x02},
	}
	gt, d = shortHeader.GetBaseFeeParams(chainConfig)
	if gt != nil || d != nil {
		t.Errorf("expected nil for short extra data, got gasTarget=%v, bfcd=%v", gt, d)
	}
}

func TestDecodeBlockExtraData(t *testing.T) {
	t.Parallel()

	cancunBlock := big.NewInt(100)
	chainConfig := &params.ChainConfig{
		ChainID:     big.NewInt(137),
		CancunBlock: cancunBlock,
	}

	buildExtra := func(bed *BlockExtraData) []byte {
		vanity := make([]byte, ExtraVanityLength)
		seal := make([]byte, ExtraSealLength)
		encoded, _ := rlp.EncodeToBytes(bed)
		extra := append(vanity, encoded...)
		extra = append(extra, seal...)
		return extra
	}

	// Pre-Cancun block → nil
	preCancun := &Header{Number: big.NewInt(50), Extra: buildExtra(&BlockExtraData{})}
	if preCancun.DecodeBlockExtraData(chainConfig) != nil {
		t.Error("expected nil for pre-Cancun block")
	}

	// Short Extra → nil
	shortExtra := &Header{Number: big.NewInt(200), Extra: []byte{0x01, 0x02}}
	if shortExtra.DecodeBlockExtraData(chainConfig) != nil {
		t.Error("expected nil for short Extra")
	}

	// Invalid RLP between vanity and seal → nil
	badRLP := make([]byte, ExtraVanityLength+ExtraSealLength+3)
	badRLP[ExtraVanityLength] = 0xff // invalid RLP prefix
	badRLP[ExtraVanityLength+1] = 0xff
	badRLP[ExtraVanityLength+2] = 0xff
	invalidRLP := &Header{Number: big.NewInt(200), Extra: badRLP}
	if invalidRLP.DecodeBlockExtraData(chainConfig) != nil {
		t.Error("expected nil for invalid RLP")
	}

	// Valid decode with all fields
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)
	txDep := [][]uint64{{0}, {1, 2}}
	bed := &BlockExtraData{
		ValidatorBytes:           []byte{0xaa, 0xbb},
		TxDependency:             txDep,
		GasTarget:                &gasTarget,
		BaseFeeChangeDenominator: &bfcd,
	}
	valid := &Header{Number: big.NewInt(200), Extra: buildExtra(bed)}
	result := valid.DecodeBlockExtraData(chainConfig)
	if result == nil {
		t.Fatal("expected non-nil result for valid extra data")
	}
	if !bytes.Equal(result.ValidatorBytes, bed.ValidatorBytes) {
		t.Errorf("ValidatorBytes mismatch: got %x, want %x", result.ValidatorBytes, bed.ValidatorBytes)
	}
	if result.GasTarget == nil || *result.GasTarget != gasTarget {
		t.Errorf("GasTarget mismatch: got %v, want %d", result.GasTarget, gasTarget)
	}
	if result.BaseFeeChangeDenominator == nil || *result.BaseFeeChangeDenominator != bfcd {
		t.Errorf("BaseFeeChangeDenominator mismatch: got %v, want %d", result.BaseFeeChangeDenominator, bfcd)
	}
	if len(result.TxDependency) != 2 || len(result.TxDependency[0]) != 1 || result.TxDependency[0][0] != 0 {
		t.Errorf("TxDependency mismatch: got %v, want %v", result.TxDependency, txDep)
	}
}

// TestGetValidatorBytesShortExtra is a regression test for the pre-Cancun
// branch of (*Header).GetValidatorBytes panicking with
// `runtime error: slice bounds out of range` when len(Extra) < ExtraVanityLength+ExtraSealLength.
//
// Prior to the fix the pre-Cancun branch performed
//
//	return h.Extra[ExtraVanityLength : len(h.Extra)-ExtraSealLength]
//
// without any length guard. The post-Cancun branch already guarded this
// path; this test exercises both branches across a range of short Extra
// lengths plus a happy-path case.
func TestGetValidatorBytesShortExtra(t *testing.T) {
	t.Parallel()

	preCancunCfg := &params.ChainConfig{
		ChainID: big.NewInt(137),
	}
	postCancunCfg := &params.ChainConfig{
		ChainID:     big.NewInt(137),
		CancunBlock: big.NewInt(100),
	}

	cases := []struct {
		name        string
		cfg         *params.ChainConfig
		blockNumber *big.Int
	}{
		{"pre-cancun", preCancunCfg, big.NewInt(50)},
		{"post-cancun", postCancunCfg, big.NewInt(200)},
	}

	shortLens := []int{0, 1, 32, 64, 65, 80, 96}

	for _, c := range cases {
		for _, n := range shortLens {
			h := &Header{
				Number: c.blockNumber,
				Extra:  make([]byte, n),
			}
			var got []byte
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s len(Extra)=%d: unexpected panic: %v", c.name, n, r)
					}
				}()
				got = h.GetValidatorBytes(c.cfg)
			}()
			if got != nil {
				t.Errorf("%s len(Extra)=%d: expected nil, got %x (len %d)",
					c.name, n, got, len(got))
			}
		}
	}

	// Happy path (pre-Cancun): vanity + 40-byte validator entry + seal.
	// Confirms the existing slice operation is preserved for valid input.
	extra := make([]byte, ExtraVanityLength+40+ExtraSealLength)
	for i := 0; i < 40; i++ {
		extra[ExtraVanityLength+i] = byte(i + 1)
	}
	h := &Header{Number: big.NewInt(50), Extra: extra}
	got := h.GetValidatorBytes(preCancunCfg)
	if len(got) != 40 {
		t.Fatalf("happy path: expected 40 bytes, got %d", len(got))
	}
	if !bytes.Equal(got, extra[ExtraVanityLength:ExtraVanityLength+40]) {
		t.Errorf("happy path: validator bytes mismatch: got %x, want %x",
			got, extra[ExtraVanityLength:ExtraVanityLength+40])
	}
}
