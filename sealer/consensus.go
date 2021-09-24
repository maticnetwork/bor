package sealer

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// mapping of the fields from bor to Sealer

func (s *Sealer) Author(header *types.Header) (common.Address, error) {
	return s.bor.Author(header)
}

func (s *Sealer) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	return s.bor.VerifyHeader(chain, header, seal)
}

func (s *Sealer) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	return s.bor.VerifyHeaders(chain, headers, seals)
}

func (s *Sealer) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	return s.bor.VerifyUncles(chain, block)
}

func (s *Sealer) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	return s.bor.Prepare(chain, header)
}

func (s *Sealer) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	s.bor.Finalize(chain, header, state, txs, uncles)
}

func (s *Sealer) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
	uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	return s.bor.FinalizeAndAssemble(chain, header, state, txs, uncles, receipts)
}

func (s *Sealer) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	return s.bor.Seal(chain, block, results, stop)
}

func (s *Sealer) SealHash(header *types.Header) common.Hash {
	return s.bor.SealHash(header)
}

func (s *Sealer) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return s.bor.CalcDifficulty(chain, time, parent)
}

func (s *Sealer) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return s.bor.APIs(chain)
}

func (s *Sealer) Close() error {
	return s.bor.Close()
}
