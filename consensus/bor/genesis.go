package bor

import (
	"math/big"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
)

//go:generate mockgen -destination=./genesis_contract_mock.go -package=bor . GenesisContract
type GenesisContract interface {
	CommitState(event *clerk.EventRecordWithTime) (uint64, error)
	LastStateId(snapshotNumber uint64) (*big.Int, error)
}
