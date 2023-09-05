package eth

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

// GetRootHash returns root hash for given start and end block
func (b *EthAPIBackend) GetRootHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64) (string, error) {
	var api *bor.API

	for _, _api := range b.eth.Engine().APIs(b.eth.BlockChain()) {
		if _api.Namespace == "bor" {
			api = _api.Service.(*bor.API)
		}
	}

	if api == nil {
		return "", errors.New("Only available in Bor engine")
	}

	root, err := api.GetRootHash(starBlockNr, endBlockNr)
	if err != nil {
		return "", err
	}

	return root, nil
}

// GetBorBlockReceipt returns bor block receipt
func (b *EthAPIBackend) GetBorBlockReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	receipt := b.eth.blockchain.GetBorReceiptByHash(hash)
	if receipt == nil {
		return nil, ethereum.NotFound
	}

	return receipt, nil
}

// GetBorBlockLogs returns bor block logs
func (b *EthAPIBackend) GetBorBlockLogs(ctx context.Context, hash common.Hash) ([]*types.Log, error) {
	receipt := b.eth.blockchain.GetBorReceiptByHash(hash)
	if receipt == nil {
		return nil, nil
	}

	return receipt.Logs, nil
}

// GetBorBlockTransaction returns bor block tx
func (b *EthAPIBackend) GetBorBlockTransaction(ctx context.Context, hash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadBorTransaction(b.eth.ChainDb(), hash)
	return tx, blockHash, blockNumber, index, nil
}

func (b *EthAPIBackend) GetBorBlockTransactionWithBlockHash(ctx context.Context, txHash common.Hash, blockHash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadBorTransactionWithBlockHash(b.eth.ChainDb(), txHash, blockHash)
	return tx, blockHash, blockNumber, index, nil
}

// SubscribeStateSyncEvent subscribes to state sync event
func (b *EthAPIBackend) SubscribeStateSyncEvent(ch chan<- core.StateSyncEvent) event.Subscription {
	return b.eth.BlockChain().SubscribeStateSyncEvent(ch)
}

// SubscribeChain2HeadEvent subscribes to reorg/head/fork event
func (b *EthAPIBackend) SubscribeChain2HeadEvent(ch chan<- core.Chain2HeadEvent) event.Subscription {
	return b.eth.BlockChain().SubscribeChain2HeadEvent(ch)
}

func (b *EthAPIBackend) SendRawTransactionConditional(ctx context.Context, input hexutil.Bytes, options types.OptionsAA4337) (common.Hash, error) {
	fmt.Println("PSP - SendRawTransactionConditional - 3")

	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return common.Hash{}, err
	}

	currentHeader := b.CurrentHeader()
	currentState, _, _ := b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(currentHeader.Number.Int64()))

	// check block number range
	if err := currentHeader.ValidateBlockNumberOptions4337(options.BlockNumberMin, options.BlockNumberMax); err != nil {
		return common.Hash{}, &rpc.OptionsValidateError{Message: "out of block range. err: " + err.Error()}
	}

	// check timestamp range
	if err := currentHeader.ValidateTimestampOptions4337(options.TimestampMin, options.TimestampMax); err != nil {
		return common.Hash{}, &rpc.OptionsValidateError{Message: "out of time range. err: " + err.Error()}
	}

	// check knownAccounts length (number of slots/accounts) should be less than 1000
	if err := options.KnownAccounts.ValidateLength(); err != nil {
		return common.Hash{}, &rpc.KnownAccountsLimitExceededError{Message: "limit exceeded. err: " + err.Error()}
	}

	// check knownAccounts
	if err := currentState.ValidateKnownAccounts(options.KnownAccounts); err != nil {
		return common.Hash{}, &rpc.OptionsValidateError{Message: "storage error. err: " + err.Error()}
	}

	// put options data in Tx, to use it later while block building
	tx.PutOptions(&options)

	return ethapi.SubmitTransaction(ctx, b, tx)
}
