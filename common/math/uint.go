package math

import (
	"github.com/holiman/uint256"
)

var (
	U1   = uint256.NewInt(1)
	U100 = uint256.NewInt(100)
)

func U256LTE(a, b *uint256.Int) bool {
	return a.Lt(b) || a.Eq(b)
}
