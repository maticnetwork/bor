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

func (h *HeimdallAppClient) Span(ctx context.Context, spanID uint64) (*span.HeimdallSpan, error) {
	log.Info("Fetching span", "spanID", spanID)

	res, err := h.hApp.BorKeeper.GetSpan(h.NewContext(), spanID)
	if err != nil {
		return nil, err
	}

	log.Info("Fetched span", "spanID", spanID)

	return toSpan(&res), nil
}

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

func toValidatorSet(vs stakeTypes.ValidatorSet) valset.ValidatorSet {
	return valset.ValidatorSet{
		Validators: toValidatorsRef(vs.Validators),
		Proposer:   toValidatorRef(vs.Proposer),
	}
}

func toValidators(vs []stakeTypes.Validator) []valset.Validator {
	newVS := make([]valset.Validator, len(vs))

	for i, v := range vs {
		newVS[i] = toValidator(v)
	}

	return newVS
}

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

func toValidatorRef(v *stakeTypes.Validator) *valset.Validator {
	return &valset.Validator{
		ID:               v.ValId,
		Address:          common.HexToAddress(v.Signer),
		VotingPower:      v.VotingPower,
		ProposerPriority: v.ProposerPriority,
	}
}

func toValidator(v stakeTypes.Validator) valset.Validator {
	return valset.Validator{
		ID:               v.ValId,
		Address:          common.HexToAddress(v.Signer),
		VotingPower:      v.VotingPower,
		ProposerPriority: v.ProposerPriority,
	}
}
