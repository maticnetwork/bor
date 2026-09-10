package contract

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	borabi "github.com/ethereum/go-ethereum/consensus/bor/abi"
	"github.com/ethereum/go-ethereum/consensus/bor/api"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/statefull"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

type GenesisContractsClient struct {
	validatorSetABI          abi.ABI
	stateReceiverABI         abi.ABI
	reservedRegistryABI      abi.ABI
	ValidatorContract        string
	StateReceiverContract    string
	ReservedRegistryContract string
	chainConfig              *params.ChainConfig
	ethAPI                   api.Caller
}

func NewGenesisContractsClient(
	chainConfig *params.ChainConfig,
	validatorContract,
	stateReceiverContract string,
	ethAPI api.Caller,
) *GenesisContractsClient {
	return &GenesisContractsClient{
		validatorSetABI:          borabi.ValidatorSet(),
		stateReceiverABI:         borabi.StateReceiver(),
		reservedRegistryABI:      borabi.ReservedBlockspaceRegistry(),
		ValidatorContract:        validatorContract,
		StateReceiverContract:    stateReceiverContract,
		ReservedRegistryContract: chainConfig.Bor.ReservedRegistryContract,
		chainConfig:              chainConfig,
		ethAPI:                   ethAPI,
	}
}

func (gc *GenesisContractsClient) CommitState(
	event *clerk.EventRecordWithTime,
	state vm.StateDB,
	header *types.Header,
	chCtx statefull.ChainContext,
	vmCfg vm.Config,
) (uint64, error) {
	eventRecord := event.BuildEventRecord()

	recordBytes, err := rlp.EncodeToBytes(eventRecord)
	if err != nil {
		return 0, err
	}

	const method = "commitState"

	t := event.Time.Unix()

	data, err := gc.stateReceiverABI.Pack(method, big.NewInt(0).SetInt64(t), recordBytes)
	if err != nil {
		log.Error("Unable to pack tx for commitState", "error", err)
		return 0, err
	}

	msg := statefull.GetSystemMessage(common.HexToAddress(gc.StateReceiverContract), data)

	log.Info("→ committing new state", "eventRecord", event.ID)

	gasUsed, err := statefull.ApplyMessage(context.Background(), msg, state, header, gc.chainConfig, chCtx, vmCfg)

	// Logging event log with time and individual gasUsed
	log.Info("→ committed new state", "eventRecord", event.String(gasUsed))

	if err != nil {
		return 0, err
	}

	return gasUsed, nil
}

func (gc *GenesisContractsClient) LastStateId(state *state.StateDB, number uint64, hash common.Hash) (*big.Int, error) {
	blockNr := rpc.BlockNumber(number)

	const method = "lastStateId"

	data, err := gc.stateReceiverABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for LastStateId", "error", err)

		return nil, err
	}

	msgData := (hexutil.Bytes)(data)
	toAddress := common.HexToAddress(gc.StateReceiverContract)

	// BOR: Do a 'CallWithState' so that we can fetch the last state ID from a given (incoming)
	// state instead of local(canonical) chain's state.
	result, err := gc.ethAPI.CallWithState(ethapi.WithBorInternalCall(context.Background()), ethapi.TransactionArgs{
		Gas:  &borabi.SystemTxGas,
		To:   &toAddress,
		Data: &msgData,
	}, &rpc.BlockNumberOrHash{BlockNumber: &blockNr, BlockHash: &hash}, state, nil, nil)
	if err != nil {
		return nil, err
	}

	ret := new(*big.Int)
	if err := gc.stateReceiverABI.UnpackIntoInterface(ret, method, result); err != nil {
		return nil, err
	}

	return *ret, nil
}
