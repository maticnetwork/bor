package span

import (
	"context"
	"encoding/hex"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests/bor/contracts"
)

type ChainSpanner struct {
	validatorSet             *contracts.BorValidatorSet
	chainConfig              *params.ChainConfig
	validatorContractAddress common.Address
}

func NewChainSpanner(chainConfig *params.ChainConfig, validatorContractAddress common.Address) (*ChainSpanner, error) {
	backend, err := ethclient.Dial("127.0.0.1")
	if err != nil {
		return nil, err
	}
	validatorSet, err := contracts.NewBorValidatorSet(validatorContractAddress, backend)
	if err != nil {
		return nil, err
	}
	return &ChainSpanner{
		validatorSet:             validatorSet,
		chainConfig:              chainConfig,
		validatorContractAddress: validatorContractAddress,
	}, nil
}

// GetCurrentSpan get current span from contract
func (c *ChainSpanner) GetCurrentSpan(ctx context.Context, headerHash common.Hash) (*Span, error) {

	blockNr := rpc.BlockNumberOrHashWithHash(headerHash, false)
	opts := &bind.CallOpts{
		BlockNumber: big.NewInt(blockNr.BlockHash.Big().Int64()),
		Context:     ctx,
	}
	ret, err := c.validatorSet.GetCurrentSpan(opts)
	if err != nil {
		return nil, err
	}

	span := Span{
		ID:         ret.Number.Uint64(),
		StartBlock: ret.StartBlock.Uint64(),
		EndBlock:   ret.EndBlock.Uint64(),
	}

	return &span, nil
}

// GetCurrentValidators get current validators
func (c *ChainSpanner) GetCurrentValidators(ctx context.Context, headerHash common.Hash, blockNumber uint64) ([]*valset.Validator, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	blockNr := rpc.BlockNumberOrHashWithHash(headerHash, false)

	opts := &bind.CallOpts{
		BlockNumber: big.NewInt(blockNr.BlockNumber.Int64()),
		Context:     ctx,
	}

	ret0, ret1, err := c.validatorSet.GetBorValidators(opts, big.NewInt(int64(blockNumber)))
	if err != nil {
		return nil, err
	}

	vals := make([]*valset.Validator, len(ret0))

	for i, v := range ret0 {
		vals[i] = &valset.Validator{Address: v, VotingPower: ret1[i].Int64()}
	}

	return vals, nil
}

func (c *ChainSpanner) CommitSpan(ctx context.Context, heimdallSpan HeimdallSpan, state *state.StateDB, header *types.Header, chainContext core.ChainContext) error {
	// get validators bytes
	validators := make([]valset.MinimalVal, 0, len(heimdallSpan.ValidatorSet.Validators))
	for _, val := range heimdallSpan.ValidatorSet.Validators {
		validators = append(validators, val.MinimalVal())
	}

	validatorBytes, err := rlp.EncodeToBytes(validators)
	if err != nil {
		return err
	}

	// get producers bytes
	producers := make([]valset.MinimalVal, 0, len(heimdallSpan.SelectedProducers))
	for _, val := range heimdallSpan.SelectedProducers {
		producers = append(producers, val.MinimalVal())
	}

	producerBytes, err := rlp.EncodeToBytes(producers)
	if err != nil {
		return err
	}

	log.Info("✅ Committing new span",
		"id", heimdallSpan.ID,
		"startBlock", heimdallSpan.StartBlock,
		"endBlock", heimdallSpan.EndBlock,
		"validatorBytes", hex.EncodeToString(validatorBytes),
		"producerBytes", hex.EncodeToString(producerBytes),
	)

	opts := &bind.TransactOpts{
		From:     types.SystemAddress,
		GasPrice: big.NewInt(0),
		Value:    big.NewInt(0),
		Context:  ctx,
	}
	_, err = c.validatorSet.CommitSpan(opts, big.NewInt(int64(heimdallSpan.ID)), big.NewInt(int64(heimdallSpan.StartBlock)), big.NewInt(int64(heimdallSpan.EndBlock)), validatorBytes, producerBytes)
	if err != nil {
		return err
	}

	return err
}
