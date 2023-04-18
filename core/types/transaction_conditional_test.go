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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
)

func TestKnownAccounts(t *testing.T) {
	t.Parallel()

	requestRaw := []byte(`{"0xadd1add1add1add1add1add1add1add1add1add1": "0x000000000000000000000000313aadca1750caadc7bcb26ff08175c95dcf8e38", "0xadd2add2add2add2add2add2add2add2add2add2": {"0x0000000000000000000000000000000000000000000000000000000000000aaa": "0x0000000000000000000000000000000000000000000000000000000000000bbb", "0x0000000000000000000000000000000000000000000000000000000000000ccc": "0x0000000000000000000000000000000000000000000000000000000000000ddd"}}`)

	accs := &KnownAccounts{}

	err := json.Unmarshal(requestRaw, accs)
	require.NoError(t, err)

	expected := &KnownAccounts{
		common.HexToAddress("0xadd1add1add1add1add1add1add1add1add1add1"): SingleFromHex("0x000000000000000000000000313aadca1750caadc7bcb26ff08175c95dcf8e38"),
		common.HexToAddress("0xadd2add2add2add2add2add2add2add2add2add2"): FromMap(map[string]string{
			"0x0000000000000000000000000000000000000000000000000000000000000aaa": "0x0000000000000000000000000000000000000000000000000000000000000bbb",
			"0x0000000000000000000000000000000000000000000000000000000000000ccc": "0x0000000000000000000000000000000000000000000000000000000000000ddd",
		}),
	}

	require.Equal(t, expected, accs)
}
