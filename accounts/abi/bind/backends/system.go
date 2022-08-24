package backends

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

type SystemBackend struct {
	API *ethapi.PublicBlockChainAPI
}

func (b *SystemBackend) CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	gas := hexutil.Uint64(call.Gas)
	data := hexutil.Bytes(call.Data)

	opts := ethapi.TransactionArgs{
		To:   call.To,
		Gas:  &gas,
		Data: &data,
		//ChainID    *hexutil.Big      `json:"chainId,omitempty"`
	}

	fmt.Println("======================", blockNumber.String())

	/*
		result, err := c.ethAPI.Call(ctx, ethapi.TransactionArgs{
			Gas:  &gas,
			To:   &toAddress,
			Data: &msgData,
		}, blockNr, nil)
	*/

	return b.API.Call(ctx, opts, rpc.BlockNumberOrHashFromBigInt(blockNumber), nil)
}

func (b *SystemBackend) CodeAt(ctx context.Context, contract common.Address, blockNumber *big.Int) ([]byte, error) {
	return b.API.GetCode(ctx, contract, rpc.BlockNumberOrHashFromBigInt(blockNumber))
}

func (b *SystemBackend) Call(ctx context.Context, args ethapi.TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *ethapi.StateOverride) (hexutil.Bytes, error) {
	return b.API.Call(ctx, args, blockNrOrHash, overrides)
}

func (b *SystemBackend) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return b.API.Backend().HeaderByNumber(ctx, rpc.BlockNumberFromBigInt(number))
}

func (b *SystemBackend) PendingCodeAt(ctx context.Context, account common.Address) ([]byte, error) {
	return b.API.GetCode(ctx, account, rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber))
}

func (b *SystemBackend) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	panic("not implemented")
}

func (b *SystemBackend) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	panic("not implemented")
}

func (b *SystemBackend) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	panic("not implemented")
}

func (b *SystemBackend) EstimateGas(ctx context.Context, call ethereum.CallMsg) (gas uint64, err error) {
	panic("not implemented")
}

func (b *SystemBackend) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	panic("not implemented")
}

func (b *SystemBackend) FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	panic("not implemented")
}

func (b *SystemBackend) SubscribeFilterLogs(ctx context.Context, query ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error) {
	panic("not implemented")
}
