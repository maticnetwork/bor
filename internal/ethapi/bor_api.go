package ethapi

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// GetRootHash returns root hash for given start and end block
func (s *BlockChainAPI) GetRootHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64) (string, error) {
	root, err := s.b.GetRootHash(ctx, starBlockNr, endBlockNr)
	if err != nil {
		return "", err
	}

	return root, nil
}

func (s *BlockChainAPI) GetBorBlockReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	return s.b.GetBorBlockReceipt(ctx, hash)
}

//
// Bor transaction utils
//

func (s *BlockChainAPI) appendRPCMarshalBorTransaction(ctx context.Context, block *types.Block, fields map[string]interface{}, fullTx bool) map[string]interface{} {
	if block != nil {
		txHash := types.GetDerivedBorTxHash(types.BorReceiptKey(block.Number().Uint64(), block.Hash()))

		borTx, blockHash, blockNumber, txIndex, _ := s.b.GetBorBlockTransactionWithBlockHash(ctx, txHash, block.Hash())
		if borTx != nil {
			formattedTxs := fields["transactions"].([]interface{})

			if fullTx {
				marshalledTx := newRPCTransaction(borTx, blockHash, blockNumber, txIndex, block.BaseFee(), s.b.ChainConfig())
				// newRPCTransaction calculates hash based on RLP of the transaction data.
				// In case of bor block tx, we need simple derived tx hash (same as function argument) instead of RLP hash
				marshalledTx.Hash = txHash
				fields["transactions"] = append(formattedTxs, marshalledTx)
			} else {
				fields["transactions"] = append(formattedTxs, txHash)
			}
		}
	}

	return fields
}

// SendRawTransactionConditional will add the signed transaction to the transaction pool.
// The sender/bundler is responsible for signing the transaction
func (s *BlockChainAPI) SendRawTransactionConditional(ctx context.Context, input hexutil.Bytes, options types.OptionsAA4337) (common.Hash, error) {
	fmt.Println("PSP - SendRawTransactionConditional - 2")

	return s.b.SendRawTransactionConditional(ctx, input, options)
}
