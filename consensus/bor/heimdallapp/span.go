package heimdallapp

import (
	"context"

	hmTypes "github.com/maticnetwork/heimdall/types"

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

	return toSpan(res), nil
}

func toSpan(hdSpan *hmTypes.Span) *span.HeimdallSpan {
	return &span.HeimdallSpan{
		Span: span.Span{
			ID:         hdSpan.ID,
			StartBlock: hdSpan.StartBlock,
			EndBlock:   hdSpan.EndBlock,
		},
		ValidatorSet:      toValidatorSet(hdSpan.ValidatorSet),
		SelectedProducers: toValidators(hdSpan.SelectedProducers),
		ChainID:           hdSpan.ChainID,
	}
}

func toValidatorSet(vs hmTypes.ValidatorSet) valset.ValidatorSet {
	set := valset.ValidatorSet{
		Validators: toValidatorsRef(vs.Validators),
		Proposer:   toValidatorRef(vs.Proposer),
	}

	// set validator map
	validatorsMap := make(map[common.Address]int, len(set.Validators))

	for i, val := range set.Validators {
		validatorsMap[val.Address] = i
	}

	set.SetMap(validatorsMap)

	set.SetTotalVotingPower(vs.TotalVotingPower())

	return set
}

func toValidators(vs []hmTypes.Validator) []valset.Validator {
	newVS := make([]valset.Validator, len(vs))

	for i, v := range vs {
		newVS[i] = toValidator(v)
	}

	return newVS
}

func toValidatorsRef(vs []*hmTypes.Validator) []*valset.Validator {
	newVS := make([]*valset.Validator, len(vs))

	for i, v := range vs {
		if v == nil {
			continue
		}

		newVS[i] = toValidatorRef(v)
	}

	return newVS
}

func toValidator(v hmTypes.Validator) valset.Validator {
	return valset.Validator{
		ID:               uint64(v.ID),
		Address:          common.Address(v.Signer),
		VotingPower:      v.VotingPower,
		ProposerPriority: v.ProposerPriority,
	}
}

func toValidatorRef(v *hmTypes.Validator) *valset.Validator {
	return &valset.Validator{
		ID:               uint64(v.ID),
		Address:          common.Address(v.Signer),
		VotingPower:      v.VotingPower,
		ProposerPriority: v.ProposerPriority,
	}
}
