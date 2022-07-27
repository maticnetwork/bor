package valset

import "github.com/ethereum/go-ethereum/common"

//go:generate mockgen destination=./tests/bor/mocks/IValidatorSet.go -package=mocks ./consensus/bor/valset ValidatorSet
type IValidatorSet interface {
	IsNilOrEmpty() bool
	CopyIncrementProposerPriority(times int) *ValidatorSet
	IncrementProposerPriority(times int)
	RescalePriorities(diffMax int64)
	Copy() *ValidatorSet
	HasAddress(address common.Address) bool
	GetByAddress(address common.Address) (index int, val *Validator)
	GetByIndex(index int) (address common.Address, val *Validator)
	Size() int
	UpdateTotalVotingPower() error
	TotalVotingPower() int64
	GetProposer() (proposer *Validator)
	Iterate(fn func(index int, val *Validator) bool)
	UpdateWithChangeSet(changes []*Validator) error
}
