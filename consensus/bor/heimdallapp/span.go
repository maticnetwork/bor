package heimdallapp

import (
	"context"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/log"
)

func (h *HeimdallAppClient) GetSpan(ctx context.Context, spanID uint64) (*borTypes.Span, error) {
	log.Info("GetSpan not implemented!")
	return nil, nil
}

//nolint:unused
func toSpan(hdSpan *borTypes.Span) *span.HeimdallSpan {
	return &span.HeimdallSpan{
		Span: span.Span{
			ID:         hdSpan.Id,
			StartBlock: hdSpan.StartBlock,
			EndBlock:   hdSpan.EndBlock,
		},
		ValidatorSet:      toValidatorSet(hdSpan.ValidatorSet),
		SelectedProducers: toValidators(hdSpan.SelectedProducers),
		ChainID:           hdSpan.BorChainId,
	}
}

//nolint:unused
func toValidatorSet(vs stakeTypes.ValidatorSet) valset.ValidatorSet {
	return valset.ValidatorSet{
		Validators: toValidatorsRef(vs.Validators),
		Proposer:   toValidatorRef(vs.Proposer),
	}
}

//nolint:unused
func toValidators(vs []stakeTypes.Validator) []valset.Validator {
	newVS := make([]valset.Validator, len(vs))

	for i, v := range vs {
		newVS[i] = toValidator(v)
	}

	return newVS
}

//nolint:unused
func toValidatorsRef(vs []*stakeTypes.Validator) []*valset.Validator {
	newVS := make([]*valset.Validator, len(vs))

	for i, v := range vs {
		if v == nil {
			continue
		}

		newVS[i] = toValidatorRef(v)
	}

	return newVS
}

//nolint:unused
func toValidatorRef(v *stakeTypes.Validator) *valset.Validator {
	return &valset.Validator{
		ID:               v.ValId,
		Address:          common.HexToAddress(v.Signer),
		VotingPower:      v.VotingPower,
		ProposerPriority: v.ProposerPriority,
	}
}

//nolint:unused
func toValidator(v stakeTypes.Validator) valset.Validator {
	return valset.Validator{
		ID:               v.ValId,
		Address:          common.HexToAddress(v.Signer),
		VotingPower:      v.VotingPower,
		ProposerPriority: v.ProposerPriority,
	}
}
