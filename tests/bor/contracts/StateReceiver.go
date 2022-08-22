// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package contracts

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
)

// StateReceiverMetaData contains all meta data concerning the StateReceiver contract.
var StateReceiverMetaData = &bind.MetaData{
	ABI: "[{\"constant\":true,\"inputs\":[],\"name\":\"SYSTEM_ADDRESS\",\"outputs\":[{\"internalType\":\"address\",\"name\":\"\",\"type\":\"address\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"lastStateId\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"syncTime\",\"type\":\"uint256\"},{\"internalType\":\"bytes\",\"name\":\"recordBytes\",\"type\":\"bytes\"}],\"name\":\"commitState\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"success\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]",
}

// StateReceiverABI is the input ABI used to generate the binding from.
// Deprecated: Use StateReceiverMetaData.ABI instead.
var StateReceiverABI = StateReceiverMetaData.ABI

// StateReceiver is an auto generated Go binding around an Ethereum contract.
type StateReceiver struct {
	StateReceiverCaller     // Read-only binding to the contract
	StateReceiverTransactor // Write-only binding to the contract
	StateReceiverFilterer   // Log filterer for contract events
}

// StateReceiverCaller is an auto generated read-only Go binding around an Ethereum contract.
type StateReceiverCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// StateReceiverTransactor is an auto generated write-only Go binding around an Ethereum contract.
type StateReceiverTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// StateReceiverFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type StateReceiverFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// StateReceiverSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type StateReceiverSession struct {
	Contract     *StateReceiver    // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// StateReceiverCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type StateReceiverCallerSession struct {
	Contract *StateReceiverCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts        // Call options to use throughout this session
}

// StateReceiverTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type StateReceiverTransactorSession struct {
	Contract     *StateReceiverTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts        // Transaction auth options to use throughout this session
}

// StateReceiverRaw is an auto generated low-level Go binding around an Ethereum contract.
type StateReceiverRaw struct {
	Contract *StateReceiver // Generic contract binding to access the raw methods on
}

// StateReceiverCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type StateReceiverCallerRaw struct {
	Contract *StateReceiverCaller // Generic read-only contract binding to access the raw methods on
}

// StateReceiverTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type StateReceiverTransactorRaw struct {
	Contract *StateReceiverTransactor // Generic write-only contract binding to access the raw methods on
}

// NewStateReceiver creates a new instance of StateReceiver, bound to a specific deployed contract.
func NewStateReceiver(address common.Address, backend bind.ContractBackend) (*StateReceiver, error) {
	contract, err := bindStateReceiver(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &StateReceiver{StateReceiverCaller: StateReceiverCaller{contract: contract}, StateReceiverTransactor: StateReceiverTransactor{contract: contract}, StateReceiverFilterer: StateReceiverFilterer{contract: contract}}, nil
}

// NewStateReceiverCaller creates a new read-only instance of StateReceiver, bound to a specific deployed contract.
func NewStateReceiverCaller(address common.Address, caller bind.ContractCaller) (*StateReceiverCaller, error) {
	contract, err := bindStateReceiver(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &StateReceiverCaller{contract: contract}, nil
}

// NewStateReceiverTransactor creates a new write-only instance of StateReceiver, bound to a specific deployed contract.
func NewStateReceiverTransactor(address common.Address, transactor bind.ContractTransactor) (*StateReceiverTransactor, error) {
	contract, err := bindStateReceiver(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &StateReceiverTransactor{contract: contract}, nil
}

// NewStateReceiverFilterer creates a new log filterer instance of StateReceiver, bound to a specific deployed contract.
func NewStateReceiverFilterer(address common.Address, filterer bind.ContractFilterer) (*StateReceiverFilterer, error) {
	contract, err := bindStateReceiver(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &StateReceiverFilterer{contract: contract}, nil
}

// bindStateReceiver binds a generic wrapper to an already deployed contract.
func bindStateReceiver(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := abi.JSON(strings.NewReader(StateReceiverABI))
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_StateReceiver *StateReceiverRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _StateReceiver.Contract.StateReceiverCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_StateReceiver *StateReceiverRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _StateReceiver.Contract.StateReceiverTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_StateReceiver *StateReceiverRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _StateReceiver.Contract.StateReceiverTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_StateReceiver *StateReceiverCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _StateReceiver.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_StateReceiver *StateReceiverTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _StateReceiver.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_StateReceiver *StateReceiverTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _StateReceiver.Contract.contract.Transact(opts, method, params...)
}

// SYSTEMADDRESS is a free data retrieval call binding the contract method 0x3434735f.
//
// Solidity: function SYSTEM_ADDRESS() view returns(address)
func (_StateReceiver *StateReceiverCaller) SYSTEMADDRESS(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _StateReceiver.contract.Call(opts, &out, "SYSTEM_ADDRESS")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// SYSTEMADDRESS is a free data retrieval call binding the contract method 0x3434735f.
//
// Solidity: function SYSTEM_ADDRESS() view returns(address)
func (_StateReceiver *StateReceiverSession) SYSTEMADDRESS() (common.Address, error) {
	return _StateReceiver.Contract.SYSTEMADDRESS(&_StateReceiver.CallOpts)
}

// SYSTEMADDRESS is a free data retrieval call binding the contract method 0x3434735f.
//
// Solidity: function SYSTEM_ADDRESS() view returns(address)
func (_StateReceiver *StateReceiverCallerSession) SYSTEMADDRESS() (common.Address, error) {
	return _StateReceiver.Contract.SYSTEMADDRESS(&_StateReceiver.CallOpts)
}

// LastStateId is a free data retrieval call binding the contract method 0x5407ca67.
//
// Solidity: function lastStateId() view returns(uint256)
func (_StateReceiver *StateReceiverCaller) LastStateId(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _StateReceiver.contract.Call(opts, &out, "lastStateId")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// LastStateId is a free data retrieval call binding the contract method 0x5407ca67.
//
// Solidity: function lastStateId() view returns(uint256)
func (_StateReceiver *StateReceiverSession) LastStateId() (*big.Int, error) {
	return _StateReceiver.Contract.LastStateId(&_StateReceiver.CallOpts)
}

// LastStateId is a free data retrieval call binding the contract method 0x5407ca67.
//
// Solidity: function lastStateId() view returns(uint256)
func (_StateReceiver *StateReceiverCallerSession) LastStateId() (*big.Int, error) {
	return _StateReceiver.Contract.LastStateId(&_StateReceiver.CallOpts)
}

// CommitState is a paid mutator transaction binding the contract method 0x19494a17.
//
// Solidity: function commitState(uint256 syncTime, bytes recordBytes) returns(bool success)
func (_StateReceiver *StateReceiverTransactor) CommitState(opts *bind.TransactOpts, syncTime *big.Int, recordBytes []byte) (*types.Transaction, error) {
	return _StateReceiver.contract.Transact(opts, "commitState", syncTime, recordBytes)
}

// CommitState is a paid mutator transaction binding the contract method 0x19494a17.
//
// Solidity: function commitState(uint256 syncTime, bytes recordBytes) returns(bool success)
func (_StateReceiver *StateReceiverSession) CommitState(syncTime *big.Int, recordBytes []byte) (*types.Transaction, error) {
	return _StateReceiver.Contract.CommitState(&_StateReceiver.TransactOpts, syncTime, recordBytes)
}

// CommitState is a paid mutator transaction binding the contract method 0x19494a17.
//
// Solidity: function commitState(uint256 syncTime, bytes recordBytes) returns(bool success)
func (_StateReceiver *StateReceiverTransactorSession) CommitState(syncTime *big.Int, recordBytes []byte) (*types.Transaction, error) {
	return _StateReceiver.Contract.CommitState(&_StateReceiver.TransactOpts, syncTime, recordBytes)
}
