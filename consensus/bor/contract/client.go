package contract

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/statefull"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests/bor/contracts"
)

type GenesisContractsClient struct {
	validatorSet      *contracts.BorValidatorSet
	stateReceiver     *contracts.StateReceiver
	ValidatorSetAddr  common.Address
	StateReceiverAddr common.Address
	chainConfig       *params.ChainConfig
}

func NewGenesisContractsClient(chainConfig *params.ChainConfig, validatorSetAddr, stateReceiverAddr common.Address) *GenesisContractsClient {
	backend, err := ethclient.Dial("127.0.0.1")
	if err != nil {
		log.Error("Failed to initialize ethereum client", "error", err)
	}

	validatorSet, err := contracts.NewBorValidatorSet(validatorSetAddr, backend)
	if err != nil {
		log.Error("Failed to initialize BorValidatorSet", "error", err)
	}

	stateReceiver, err := contracts.NewStateReceiver(stateReceiverAddr, backend)
	if err != nil {
		log.Error("Failed to initialize StateReceiver", "error", err)
	}
	return &GenesisContractsClient{
		validatorSet:      validatorSet,
		stateReceiver:     stateReceiver,
		ValidatorSetAddr:  validatorSetAddr,
		StateReceiverAddr: stateReceiverAddr,
		chainConfig:       chainConfig,
	}
}

func (gc *GenesisContractsClient) CommitState(
	event *clerk.EventRecordWithTime,
	state *state.StateDB,
	header *types.Header,
	chCtx statefull.ChainContext,
) (uint64, error) {
	eventRecord := event.BuildEventRecord()

	recordBytes, err := rlp.EncodeToBytes(eventRecord)
	if err != nil {
		return 0, err
	}

	t := event.Time.Unix()

	opts := &bind.TransactOpts{
		From:     types.SystemAddress,
		GasPrice: big.NewInt(0),
		Value:    big.NewInt(0),
	}

	tx, err := gc.stateReceiver.CommitState(opts, big.NewInt(t), recordBytes)

	// Logging event log with time and individual tx gas limit
	log.Info("→ committing new state", "eventRecord", event.String(tx.Gas()))

	if err != nil {
		return 0, err
	}

	return tx.Gas(), nil
}

func (gc *GenesisContractsClient) LastStateId(snapshotNumber uint64) (*big.Int, error) {
	blockNr := rpc.BlockNumber(snapshotNumber)

	opts := &bind.CallOpts{
		BlockNumber: big.NewInt(blockNr.Int64()),
	}

	res, err := gc.stateReceiver.LastStateId(opts)
	if err != nil {
		log.Error("Failed to fetch last state ID", "error", err)
	}

	return res, nil
}
