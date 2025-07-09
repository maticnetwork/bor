package span

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"

	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"
)

func ConvertHeimdallValSetToBorValSet(heimdallValSet stakeTypes.ValidatorSet) valset.ValidatorSet {
	validators := make([]*valset.Validator, len(heimdallValSet.Validators))
	for i, v := range heimdallValSet.Validators {
		validators[i] = &valset.Validator{
			ID:               v.ValId,
			Address:          common.HexToAddress(v.Signer),
			VotingPower:      v.VotingPower,
			ProposerPriority: v.ProposerPriority,
		}
	}

	resValSet := valset.ValidatorSet{
		Validators: validators,
	}

	if heimdallValSet.Proposer != nil {
		resValSet.Proposer = &valset.Validator{
			ID:               heimdallValSet.Proposer.ValId,
			Address:          common.HexToAddress(heimdallValSet.Proposer.Signer),
			VotingPower:      heimdallValSet.Proposer.VotingPower,
			ProposerPriority: heimdallValSet.Proposer.ProposerPriority,
		}
	}

	resValSet.UpdateTotalVotingPower()
	resValSet.UpdateValidatorMap()

	return resValSet
}

func ConvertBorValSetToHeimdallValSet(borValSet *valset.ValidatorSet) stakeTypes.ValidatorSet {
	validators := make([]*stakeTypes.Validator, len(borValSet.Validators))
	for i, v := range borValSet.Validators {
		validators[i] = &stakeTypes.Validator{
			ValId:            v.ID,
			Signer:           v.Address.Hex(),
			VotingPower:      v.VotingPower,
			ProposerPriority: v.ProposerPriority,
		}
	}

	proposer := &stakeTypes.Validator{
		ValId:            borValSet.Proposer.ID,
		Signer:           borValSet.Proposer.Address.Hex(),
		VotingPower:      borValSet.Proposer.VotingPower,
		ProposerPriority: borValSet.Proposer.ProposerPriority,
	}

	return stakeTypes.ValidatorSet{
		Validators: validators,
		Proposer:   proposer,
	}
}

func ConvertBorValidatorsToHeimdallValidators(borValidators []*valset.Validator) []stakeTypes.Validator {
	validators := make([]stakeTypes.Validator, len(borValidators))
	for i, v := range borValidators {
		validators[i] = stakeTypes.Validator{
			ValId:            v.ID,
			Signer:           v.Address.Hex(),
			VotingPower:      v.VotingPower,
			ProposerPriority: v.ProposerPriority,
		}
	}
	return validators
}

func ConvertHeimdallValidatorsToBorValidators(heimdallValidators []stakeTypes.Validator) []valset.Validator {
	validators := make([]valset.Validator, len(heimdallValidators))
	for i, v := range heimdallValidators {
		validators[i] = valset.Validator{
			ID:               v.ValId,
			Address:          common.HexToAddress(v.Signer),
			VotingPower:      v.VotingPower,
			ProposerPriority: v.ProposerPriority,
		}
	}
	return validators
}

func ConvertHeimdallValidatorsToBorValidatorsByRef(heimdallValidators []*stakeTypes.Validator) []*valset.Validator {
	validators := make([]*valset.Validator, len(heimdallValidators))
	for i, v := range heimdallValidators {
		validators[i] = &valset.Validator{
			ID:               v.ValId,
			Address:          common.HexToAddress(v.Signer),
			VotingPower:      v.VotingPower,
			ProposerPriority: v.ProposerPriority,
		}
	}
	return validators
}
