package bor

import (
	"context"
	"encoding/hex"
	"math"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

// Backend is the reference to the smart contract state that holds the data
type Backend interface {
	GetCurrentValidators(headerHash common.Hash, blockNumber uint64) ([]*Validator, error)
	GetCurrentSpan(headerHash common.Hash) (*Span, error)
	CommitSpan(state *state.StateDB, heimdallSpan *HeimdallSpan, header *types.Header) error
	CommitState(event *EventRecordWithTime, state *state.StateDB, header *types.Header) error
	LastStateId(snapshotNumber uint64) (*big.Int, error)
}

type GenesisContractsClient struct {
	ValidatorContract     common.Address
	StateReceiverContract common.Address
	ethAPI                *ethapi.PublicBlockChainAPI
	ctx                   *chainContext
}

const validatorsetABIStr = `[{"constant":true,"inputs":[],"name":"SPRINT","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"SYSTEM_ADDRESS","outputs":[{"internalType":"address","name":"","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"CHAIN","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"FIRST_END_BLOCK","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"","type":"uint256"},{"internalType":"uint256","name":"","type":"uint256"}],"name":"producers","outputs":[{"internalType":"uint256","name":"id","type":"uint256"},{"internalType":"uint256","name":"power","type":"uint256"},{"internalType":"address","name":"signer","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"ROUND_TYPE","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"BOR_ID","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"","type":"uint256"}],"name":"spanNumbers","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"VOTE_TYPE","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"","type":"uint256"},{"internalType":"uint256","name":"","type":"uint256"}],"name":"validators","outputs":[{"internalType":"uint256","name":"id","type":"uint256"},{"internalType":"uint256","name":"power","type":"uint256"},{"internalType":"address","name":"signer","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"","type":"uint256"}],"name":"spans","outputs":[{"internalType":"uint256","name":"number","type":"uint256"},{"internalType":"uint256","name":"startBlock","type":"uint256"},{"internalType":"uint256","name":"endBlock","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"inputs":[],"payable":false,"stateMutability":"nonpayable","type":"constructor"},{"anonymous":false,"inputs":[{"indexed":true,"internalType":"uint256","name":"id","type":"uint256"},{"indexed":true,"internalType":"uint256","name":"startBlock","type":"uint256"},{"indexed":true,"internalType":"uint256","name":"endBlock","type":"uint256"}],"name":"NewSpan","type":"event"},{"constant":true,"inputs":[],"name":"currentSprint","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"}],"name":"getSpan","outputs":[{"internalType":"uint256","name":"number","type":"uint256"},{"internalType":"uint256","name":"startBlock","type":"uint256"},{"internalType":"uint256","name":"endBlock","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"getCurrentSpan","outputs":[{"internalType":"uint256","name":"number","type":"uint256"},{"internalType":"uint256","name":"startBlock","type":"uint256"},{"internalType":"uint256","name":"endBlock","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"getNextSpan","outputs":[{"internalType":"uint256","name":"number","type":"uint256"},{"internalType":"uint256","name":"startBlock","type":"uint256"},{"internalType":"uint256","name":"endBlock","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"number","type":"uint256"}],"name":"getSpanByBlock","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"currentSpanNumber","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"}],"name":"getValidatorsTotalStakeBySpan","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"}],"name":"getProducersTotalStakeBySpan","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"},{"internalType":"address","name":"signer","type":"address"}],"name":"getValidatorBySigner","outputs":[{"components":[{"internalType":"uint256","name":"id","type":"uint256"},{"internalType":"uint256","name":"power","type":"uint256"},{"internalType":"address","name":"signer","type":"address"}],"internalType":"struct BorValidatorSet.Validator","name":"result","type":"tuple"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"},{"internalType":"address","name":"signer","type":"address"}],"name":"isValidator","outputs":[{"internalType":"bool","name":"","type":"bool"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"},{"internalType":"address","name":"signer","type":"address"}],"name":"isProducer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"address","name":"signer","type":"address"}],"name":"isCurrentValidator","outputs":[{"internalType":"bool","name":"","type":"bool"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"address","name":"signer","type":"address"}],"name":"isCurrentProducer","outputs":[{"internalType":"bool","name":"","type":"bool"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"number","type":"uint256"}],"name":"getBorValidators","outputs":[{"internalType":"address[]","name":"","type":"address[]"},{"internalType":"uint256[]","name":"","type":"uint256[]"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"getInitialValidators","outputs":[{"internalType":"address[]","name":"","type":"address[]"},{"internalType":"uint256[]","name":"","type":"uint256[]"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"getValidators","outputs":[{"internalType":"address[]","name":"","type":"address[]"},{"internalType":"uint256[]","name":"","type":"uint256[]"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":false,"inputs":[{"internalType":"uint256","name":"newSpan","type":"uint256"},{"internalType":"uint256","name":"startBlock","type":"uint256"},{"internalType":"uint256","name":"endBlock","type":"uint256"},{"internalType":"bytes","name":"validatorBytes","type":"bytes"},{"internalType":"bytes","name":"producerBytes","type":"bytes"}],"name":"commitSpan","outputs":[],"payable":false,"stateMutability":"nonpayable","type":"function"},{"constant":true,"inputs":[{"internalType":"uint256","name":"span","type":"uint256"},{"internalType":"bytes32","name":"dataHash","type":"bytes32"},{"internalType":"bytes","name":"sigs","type":"bytes"}],"name":"getStakePowerBySigs","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[{"internalType":"bytes32","name":"rootHash","type":"bytes32"},{"internalType":"bytes32","name":"leaf","type":"bytes32"},{"internalType":"bytes","name":"proof","type":"bytes"}],"name":"checkMembership","outputs":[{"internalType":"bool","name":"","type":"bool"}],"payable":false,"stateMutability":"pure","type":"function"},{"constant":true,"inputs":[{"internalType":"bytes32","name":"d","type":"bytes32"}],"name":"leafNode","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"payable":false,"stateMutability":"pure","type":"function"},{"constant":true,"inputs":[{"internalType":"bytes32","name":"left","type":"bytes32"},{"internalType":"bytes32","name":"right","type":"bytes32"}],"name":"innerNode","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"payable":false,"stateMutability":"pure","type":"function"}]`
const stateReceiverABIStr = `[{"constant":true,"inputs":[],"name":"SYSTEM_ADDRESS","outputs":[{"internalType":"address","name":"","type":"address"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":true,"inputs":[],"name":"lastStateId","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"payable":false,"stateMutability":"view","type":"function"},{"constant":false,"inputs":[{"internalType":"uint256","name":"syncTime","type":"uint256"},{"internalType":"bytes","name":"recordBytes","type":"bytes"}],"name":"commitState","outputs":[{"internalType":"bool","name":"success","type":"bool"}],"payable":false,"stateMutability":"nonpayable","type":"function"}]`

var validatorSetABI, stateReceiverABI abi.ABI

func init() {
	var err error

	validatorSetABI, err = abi.JSON(strings.NewReader(validatorsetABIStr))
	if err != nil {
		panic("BUG: Incorrect abi for validator set")
	}
	stateReceiverABI, err = abi.JSON(strings.NewReader(stateReceiverABIStr))
	if err != nil {
		panic("BUG: Incorrect abi for validator set")
	}
}

func NewGenesisContractsClient(
	ctx *chainContext,
	validatorContract,
	stateReceiverContract string,
	ethAPI *ethapi.PublicBlockChainAPI,
) *GenesisContractsClient {
	c := &GenesisContractsClient{
		ctx:                   ctx,
		ValidatorContract:     common.HexToAddress(validatorContract),
		StateReceiverContract: common.HexToAddress(stateReceiverContract),
		ethAPI:                ethAPI,
	}
	return c
}

func (gc *GenesisContractsClient) CommitState(event *EventRecordWithTime, state *state.StateDB, header *types.Header) error {
	eventRecord := event.BuildEventRecord()
	recordBytes, err := rlp.EncodeToBytes(eventRecord)
	if err != nil {
		return err
	}
	method := "commitState"
	t := event.Time.Unix()
	data, err := stateReceiverABI.Pack(method, big.NewInt(0).SetInt64(t), recordBytes)
	if err != nil {
		log.Error("Unable to pack tx for commitState", "error", err)
		return err
	}
	log.Info("→ committing new state", "eventRecord", event.String())
	msg := getSystemMessage(gc.StateReceiverContract, data)

	if err := gc.applyMessage(msg, state, header); err != nil {
		return err
	}
	return nil
}

func (gc *GenesisContractsClient) LastStateId(snapshotNumber uint64) (*big.Int, error) {
	blockNr := rpc.BlockNumber(snapshotNumber)
	method := "lastStateId"
	data, err := stateReceiverABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for LastStateId", "error", err)
		return nil, err
	}

	msgData := (hexutil.Bytes)(data)
	toAddress := gc.StateReceiverContract
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	result, err := gc.ethAPI.Call(context.Background(), ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, rpc.BlockNumberOrHash{BlockNumber: &blockNr}, nil)
	if err != nil {
		return nil, err
	}

	var ret = new(*big.Int)
	if err := stateReceiverABI.UnpackIntoInterface(ret, method, result); err != nil {
		return nil, err
	}
	return *ret, nil
}

// GetCurrentSpan get current span from contract
func (gc *GenesisContractsClient) GetCurrentSpan(headerHash common.Hash) (*Span, error) {
	// block
	blockNr := rpc.BlockNumberOrHashWithHash(headerHash, false)

	// method
	method := "getCurrentSpan"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	data, err := validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getCurrentSpan", "error", err)
		return nil, err
	}

	msgData := (hexutil.Bytes)(data)
	toAddress := gc.ValidatorContract
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	result, err := gc.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, blockNr, nil)
	if err != nil {
		return nil, err
	}

	// span result
	ret := new(struct {
		Number     *big.Int
		StartBlock *big.Int
		EndBlock   *big.Int
	})
	if err := validatorSetABI.UnpackIntoInterface(ret, method, result); err != nil {
		return nil, err
	}

	// create new span
	span := Span{
		ID:         ret.Number.Uint64(),
		StartBlock: ret.StartBlock.Uint64(),
		EndBlock:   ret.EndBlock.Uint64(),
	}

	return &span, nil
}

// GetCurrentValidators get current validators
func (gc *GenesisContractsClient) GetCurrentValidators(headerHash common.Hash, blockNumber uint64) ([]*Validator, error) {
	// block
	blockNr := rpc.BlockNumberOrHashWithHash(headerHash, false)

	// method
	method := "getBorValidators"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	data, err := validatorSetABI.Pack(method, big.NewInt(0).SetUint64(blockNumber))
	if err != nil {
		log.Error("Unable to pack tx for getValidator", "error", err)
		return nil, err
	}

	// call
	msgData := (hexutil.Bytes)(data)
	toAddress := gc.ValidatorContract
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	result, err := gc.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, blockNr, nil)
	if err != nil {
		panic(err)
		// return nil, err
	}

	var (
		ret0 = new([]common.Address)
		ret1 = new([]*big.Int)
	)
	out := &[]interface{}{
		ret0,
		ret1,
	}

	if err := validatorSetABI.UnpackIntoInterface(out, method, result); err != nil {
		return nil, err
	}

	valz := make([]*Validator, len(*ret0))
	for i, a := range *ret0 {
		valz[i] = &Validator{
			Address:     a,
			VotingPower: (*ret1)[i].Int64(),
		}
	}

	return valz, nil
}

func (gc *GenesisContractsClient) CommitSpan(state *state.StateDB, heimdallSpan *HeimdallSpan, header *types.Header) error {
	// get validators bytes
	var validators []MinimalVal
	for _, val := range heimdallSpan.ValidatorSet.Validators {
		validators = append(validators, val.MinimalVal())
	}
	validatorBytes, err := rlp.EncodeToBytes(validators)
	if err != nil {
		return err
	}

	// get producers bytes
	var producers []MinimalVal
	for _, val := range heimdallSpan.SelectedProducers {
		producers = append(producers, val.MinimalVal())
	}
	producerBytes, err := rlp.EncodeToBytes(producers)
	if err != nil {
		return err
	}

	// method
	method := "commitSpan"
	log.Info("✅ Committing new span",
		"id", heimdallSpan.ID,
		"startBlock", heimdallSpan.StartBlock,
		"endBlock", heimdallSpan.EndBlock,
		"validatorBytes", hex.EncodeToString(validatorBytes),
		"producerBytes", hex.EncodeToString(producerBytes),
	)

	// get packed data
	data, err := validatorSetABI.Pack(method,
		big.NewInt(0).SetUint64(heimdallSpan.ID),
		big.NewInt(0).SetUint64(heimdallSpan.StartBlock),
		big.NewInt(0).SetUint64(heimdallSpan.EndBlock),
		validatorBytes,
		producerBytes,
	)
	if err != nil {
		log.Error("Unable to pack tx for commitSpan", "error", err)
		return err
	}

	// get system message
	msg := getSystemMessage(gc.ValidatorContract, data)

	// apply message
	return gc.applyMessage(msg, state, header)
}

// chain context
type chainContext struct {
	Chain consensus.ChainHeaderReader
	Bor   consensus.Engine
}

func (c chainContext) Engine() consensus.Engine {
	return c.Bor
}

func (c chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	return c.Chain.GetHeader(hash, number)
}

// callmsg implements core.Message to allow passing it as a transaction simulator.
type callmsg struct {
	ethereum.CallMsg
}

func (m callmsg) From() common.Address { return m.CallMsg.From }
func (m callmsg) Nonce() uint64        { return 0 }
func (m callmsg) CheckNonce() bool     { return false }
func (m callmsg) To() *common.Address  { return m.CallMsg.To }
func (m callmsg) GasPrice() *big.Int   { return m.CallMsg.GasPrice }
func (m callmsg) Gas() uint64          { return m.CallMsg.Gas }
func (m callmsg) Value() *big.Int      { return m.CallMsg.Value }
func (m callmsg) Data() []byte         { return m.CallMsg.Data }

// get system message
func getSystemMessage(toAddress common.Address, data []byte) callmsg {
	return callmsg{
		ethereum.CallMsg{
			From:     systemAddress,
			Gas:      math.MaxUint64 / 2,
			GasPrice: big.NewInt(0),
			Value:    big.NewInt(0),
			To:       &toAddress,
			Data:     data,
		},
	}
}

// apply message
func (gc *GenesisContractsClient) applyMessage(msg callmsg, state *state.StateDB, header *types.Header) error {
	chainConfig := gc.ctx.Chain.Config()
	chainContext := gc.ctx

	// Create a new context to be used in the EVM environment
	blockContext := core.NewEVMBlockContext(header, chainContext, &header.Coinbase)
	// Create a new environment which holds all relevant information
	// about the transaction and calling mechanisms.
	vmenv := vm.NewEVM(blockContext, vm.TxContext{}, state, chainConfig, vm.Config{})
	// Apply the transaction to the current state (included in the env)
	_, _, err := vmenv.Call(
		vm.AccountRef(msg.From()),
		*msg.To(),
		msg.Data(),
		msg.Gas(),
		msg.Value(),
	)
	// Update the state with pending changes
	if err != nil {
		state.Finalise(true)
	}

	return nil
}
