package eth

import (
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
)

// witHandler implements the eth.Backend interface to handle the various network
// packets that are sent as replies or broadcasts.
type witHandler handler

func (h *witHandler) Chain() *core.BlockChain { return h.chain }
func (h *witHandler) TxPool() eth.TxPool      { return h.txpool }
