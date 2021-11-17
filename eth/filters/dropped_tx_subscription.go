package filters

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

type dropNotification struct {
	// TxHash common.Hash `json:"txhash"`
	Tx          *ethapi.RPCTransaction `json:"tx"`
	Reason      string                 `json:"reason"`
	Replacement *ethapi.RPCTransaction `json:"replacedby,omitempty"`
	Peer        interface{}            `json:"peer,omitempty"`
	Time        int64                  `json:"ts"`
}

type rejectNotification struct {
	Tx     *types.Transaction
	Reason string `json:"reason"`
}

// newRPCTransaction returns a transaction that will serialize to the RPC
// representation, with the given location metadata set (if available).
func newRPCPendingTransaction(tx *types.Transaction) *ethapi.RPCTransaction {
	if tx == nil {
		return nil
	}
	var signer types.Signer
	if tx.Protected() {
		signer = types.LatestSignerForChainID(tx.ChainId())
	} else {
		signer = types.HomesteadSigner{}
	}
	from, _ := types.Sender(signer, tx)
	v, r, s := tx.RawSignatureValues()
	result := &ethapi.RPCTransaction{
		Type:     hexutil.Uint64(tx.Type()),
		From:     from,
		Gas:      hexutil.Uint64(tx.Gas()),
		GasPrice: (*hexutil.Big)(tx.GasPrice()),
		Hash:     tx.Hash(),
		Input:    hexutil.Bytes(tx.Data()),
		Nonce:    hexutil.Uint64(tx.Nonce()),
		To:       tx.To(),
		Value:    (*hexutil.Big)(tx.Value()),
		V:        (*hexutil.Big)(v),
		R:        (*hexutil.Big)(r),
		S:        (*hexutil.Big)(s),
	}
	switch tx.Type() {
	case types.AccessListTxType:
		al := tx.AccessList()
		result.Accesses = &al
		result.ChainID = (*hexutil.Big)(tx.ChainId())
	case types.DynamicFeeTxType:
		al := tx.AccessList()
		result.Accesses = &al
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		// if the transaction has been mined, compute the effective gas price
		result.GasPrice = nil
	}
	return result
}

// DroppedTransactions send a notification each time a transaction is dropped from the mempool
func (api *PublicFilterAPI) DroppedTransactions(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	go func() {
		dropped := make(chan core.DropTxsEvent)
		droppedSub := api.backend.SubscribeDropTxsEvent(dropped)

		for {
			select {
			case d := <-dropped:
				for _, tx := range d.Txs {
					notification := &dropNotification{
						Tx:          newRPCPendingTransaction(tx),
						Reason:      d.Reason,
						Replacement: newRPCPendingTransaction(d.Replacement),
						Time:        time.Now().UnixNano(),
					}
					notifier.Notify(rpcSub.ID, notification)
				}
			case <-rpcSub.Err():
				droppedSub.Unsubscribe()
				return
			case <-notifier.Closed():
				droppedSub.Unsubscribe()
				return
			}
		}
	}()

	return rpcSub, nil
}
