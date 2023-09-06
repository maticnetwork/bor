// Copyright 2022 The go-ethereum Authors
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
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// EthereumAPI provides an API to access Ethereum related information.
type BorAPI struct {
	b Backend
}

// NewEthereumAPI creates a new Ethereum protocol API.
func NewBorAPI(b Backend) *BorAPI {
	return &BorAPI{b}
}

// SendRawTransactionConditional will add the signed transaction to the transaction pool.
// The sender/bundler is responsible for signing the transaction
func (api *BorAPI) SendRawTransactionConditional(ctx context.Context, input hexutil.Bytes, options types.OptionsAA4337) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return common.Hash{}, err
	}

	currentHeader := api.b.CurrentHeader()
	currentState, _, _ := api.b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(currentHeader.Number.Int64()))

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

	return SubmitTransaction(ctx, api.b, tx)
}
