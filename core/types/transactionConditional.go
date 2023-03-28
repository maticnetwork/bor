// Copyright 2021 The go-ethereum Authors
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
	"errors"
	"fmt"
	"math/big"
	"reflect"

	"github.com/ethereum/go-ethereum/common"
)

type KnownAccounts map[common.Address]interface{}

type OptionsAA4337 struct {
	KnownAccounts  KnownAccounts `json:"knownAccounts"`
	BlockNumberMin *big.Int      `json:"blockNumberMin"`
	BlockNumberMax *big.Int      `json:"blockNumberMax"`
	TimestampMin   uint64        `json:"timestampMin"`
	TimestampMax   uint64        `json:"timestampMax"`
}

var ErrEmptyKnownAccounts = errors.New("knownAccounts cannot be nil")

func (ka KnownAccounts) ValidateLength() error {
	if ka == nil {
		return ErrEmptyKnownAccounts
	}

	length := 0

	for _, v := range ka {
		// check if the value is hex string or an object
		if _, ok := v.(string); ok {
			length += 1
		} else if object, ok := v.(map[string]interface{}); ok {
			length += len(object)
		} else {
			return fmt.Errorf("invalid type in knownAccounts %v", reflect.TypeOf(v))
		}
	}

	if length >= 1000 {
		fmt.Errorf("number of slots/accounts in KnownAccounts %v exceeds the limit of 1000", length)
	}

	return nil
}
