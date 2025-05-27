package span

import (
	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
)

// Span Bor represents a current bor span
type Span struct {
	Id         uint64 `json:"span_id" yaml:"span_id"`
	StartBlock uint64 `json:"start_block" yaml:"start_block"`
	EndBlock   uint64 `json:"end_block" yaml:"end_block"`
}

// HeimdallSpan represents span from heimdall APIs
type HeimdallSpan struct {
	Span
	ValidatorSet      valset.ValidatorSet `json:"validator_set" yaml:"validator_set"`
	SelectedProducers []valset.Validator  `json:"selected_producers" yaml:"selected_producers"`
	ChainID           string              `json:"bor_chain_id" yaml:"bor_chain_id"`
}

func ConvertV2SpanToV1Span(v2Span *borTypes.Span) *HeimdallSpan {
	v2ToV1Val := func(v2Val stakeTypes.Validator) valset.Validator {
		return valset.Validator{
			ID:               v2Val.ValId,
			Address:          common.HexToAddress(v2Val.Signer),
			VotingPower:      v2Val.VotingPower,
			ProposerPriority: v2Val.ProposerPriority,
		}
	}

	validatorsV1 := make([]*valset.Validator, len(v2Span.ValidatorSet.Validators))
	for i, v := range v2Span.ValidatorSet.Validators {
		v1Val := v2ToV1Val(*v)
		validatorsV1[i] = &v1Val
	}
	proposerV1 := v2ToV1Val(*v2Span.ValidatorSet.Proposer)
	producersV1 := make([]valset.Validator, 0, len(v2Span.SelectedProducers))
	for _, v := range v2Span.SelectedProducers {
		producersV1 = append(producersV1, v2ToV1Val(v))
	}

	return &HeimdallSpan{
		Span: Span{
			Id:         v2Span.Id,
			StartBlock: v2Span.StartBlock,
			EndBlock:   v2Span.EndBlock,
		},
		ValidatorSet: valset.ValidatorSet{
			Validators: validatorsV1,
			Proposer:   &proposerV1,
		},
		SelectedProducers: producersV1,
		ChainID:           v2Span.BorChainId,
	}
}
