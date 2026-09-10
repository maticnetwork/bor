package sequencer

import (
	"fmt"
	"math/big"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

type speculativeHeaderChain struct {
	consensus.ChainHeaderReader
	headers map[uint64]*types.Header
}

func (c *speculativeHeaderChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if header := c.headers[number]; header != nil && header.Hash() == hash {
		return header
	}
	return c.ChainHeaderReader.GetHeader(hash, number)
}

func (c *speculativeHeaderChain) GetHeaderByNumber(number uint64) *types.Header {
	if header := c.headers[number]; header != nil {
		return header
	}
	return c.ChainHeaderReader.GetHeaderByNumber(number)
}

func (c *speculativeHeaderChain) GetHeaderByHash(hash common.Hash) *types.Header {
	for _, header := range c.headers {
		if header.Hash() == hash {
			return header
		}
	}
	return c.ChainHeaderReader.GetHeaderByHash(hash)
}

func pendingHeader(open *pb.BlockOpen) *types.Header {
	return &types.Header{
		ParentHash: common.BytesToHash(open.GetParentHash()),
		Number:     new(big.Int).SetUint64(open.GetBlockNumber()),
		GasLimit:   open.GetGasLimit(),
		Time:       open.GetBlockTimestamp(),
		BaseFee:    new(big.Int).SetBytes(open.GetBaseFee()),
		Difficulty: big.NewInt(1),
	}
}

func validateOpenExecutionContext(chain consensus.ChainHeaderReader, parent *types.Header, open *pb.BlockOpen) error {
	header := pendingHeader(open)
	if header.GasLimit > params.MaxGasLimit {
		return fmt.Errorf("gas limit %d exceeds maximum %d", header.GasLimit, params.MaxGasLimit)
	}
	if chain.Config().IsLondon(header.Number) {
		return eip1559.VerifyEIP1559Header(chain.Config(), parent, header)
	}
	return misc.VerifyGaslimit(parent.GasLimit, header.GasLimit)
}
