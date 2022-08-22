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

// BorValidatorSetValidator is an auto generated low-level Go binding around an user-defined struct.
type BorValidatorSetValidator struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}

// BorValidatorSetMetaData contains all meta data concerning the BorValidatorSet contract.
var BorValidatorSetMetaData = &bind.MetaData{
	ABI: "[{\"constant\":true,\"inputs\":[],\"name\":\"SPRINT\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"SYSTEM_ADDRESS\",\"outputs\":[{\"internalType\":\"address\",\"name\":\"\",\"type\":\"address\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"CHAIN\",\"outputs\":[{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"FIRST_END_BLOCK\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"name\":\"producers\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"id\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"power\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"ROUND_TYPE\",\"outputs\":[{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"BOR_ID\",\"outputs\":[{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"name\":\"spanNumbers\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"VOTE_TYPE\",\"outputs\":[{\"internalType\":\"uint8\",\"name\":\"\",\"type\":\"uint8\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"name\":\"validators\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"id\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"power\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"name\":\"spans\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"number\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"startBlock\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"endBlock\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"inputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"constructor\"},{\"anonymous\":false,\"inputs\":[{\"indexed\":true,\"internalType\":\"uint256\",\"name\":\"id\",\"type\":\"uint256\"},{\"indexed\":true,\"internalType\":\"uint256\",\"name\":\"startBlock\",\"type\":\"uint256\"},{\"indexed\":true,\"internalType\":\"uint256\",\"name\":\"endBlock\",\"type\":\"uint256\"}],\"name\":\"NewSpan\",\"type\":\"event\"},{\"constant\":true,\"inputs\":[],\"name\":\"currentSprint\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"}],\"name\":\"getSpan\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"number\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"startBlock\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"endBlock\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"getCurrentSpan\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"number\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"startBlock\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"endBlock\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"getNextSpan\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"number\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"startBlock\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"endBlock\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"number\",\"type\":\"uint256\"}],\"name\":\"getSpanByBlock\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"currentSpanNumber\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"}],\"name\":\"getValidatorsTotalStakeBySpan\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"}],\"name\":\"getProducersTotalStakeBySpan\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"name\":\"getValidatorBySigner\",\"outputs\":[{\"components\":[{\"internalType\":\"uint256\",\"name\":\"id\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"power\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"internalType\":\"structBorValidatorSet.Validator\",\"name\":\"result\",\"type\":\"tuple\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"name\":\"isValidator\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"},{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"name\":\"isProducer\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"name\":\"isCurrentValidator\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"address\",\"name\":\"signer\",\"type\":\"address\"}],\"name\":\"isCurrentProducer\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"number\",\"type\":\"uint256\"}],\"name\":\"getBorValidators\",\"outputs\":[{\"internalType\":\"address[]\",\"name\":\"\",\"type\":\"address[]\"},{\"internalType\":\"uint256[]\",\"name\":\"\",\"type\":\"uint256[]\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"getInitialValidators\",\"outputs\":[{\"internalType\":\"address[]\",\"name\":\"\",\"type\":\"address[]\"},{\"internalType\":\"uint256[]\",\"name\":\"\",\"type\":\"uint256[]\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[],\"name\":\"getValidators\",\"outputs\":[{\"internalType\":\"address[]\",\"name\":\"\",\"type\":\"address[]\"},{\"internalType\":\"uint256[]\",\"name\":\"\",\"type\":\"uint256[]\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":false,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"newSpan\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"startBlock\",\"type\":\"uint256\"},{\"internalType\":\"uint256\",\"name\":\"endBlock\",\"type\":\"uint256\"},{\"internalType\":\"bytes\",\"name\":\"validatorBytes\",\"type\":\"bytes\"},{\"internalType\":\"bytes\",\"name\":\"producerBytes\",\"type\":\"bytes\"}],\"name\":\"commitSpan\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"uint256\",\"name\":\"span\",\"type\":\"uint256\"},{\"internalType\":\"bytes32\",\"name\":\"dataHash\",\"type\":\"bytes32\"},{\"internalType\":\"bytes\",\"name\":\"sigs\",\"type\":\"bytes\"}],\"name\":\"getStakePowerBySigs\",\"outputs\":[{\"internalType\":\"uint256\",\"name\":\"\",\"type\":\"uint256\"}],\"payable\":false,\"stateMutability\":\"view\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"rootHash\",\"type\":\"bytes32\"},{\"internalType\":\"bytes32\",\"name\":\"leaf\",\"type\":\"bytes32\"},{\"internalType\":\"bytes\",\"name\":\"proof\",\"type\":\"bytes\"}],\"name\":\"checkMembership\",\"outputs\":[{\"internalType\":\"bool\",\"name\":\"\",\"type\":\"bool\"}],\"payable\":false,\"stateMutability\":\"pure\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"d\",\"type\":\"bytes32\"}],\"name\":\"leafNode\",\"outputs\":[{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"}],\"payable\":false,\"stateMutability\":\"pure\",\"type\":\"function\"},{\"constant\":true,\"inputs\":[{\"internalType\":\"bytes32\",\"name\":\"left\",\"type\":\"bytes32\"},{\"internalType\":\"bytes32\",\"name\":\"right\",\"type\":\"bytes32\"}],\"name\":\"innerNode\",\"outputs\":[{\"internalType\":\"bytes32\",\"name\":\"\",\"type\":\"bytes32\"}],\"payable\":false,\"stateMutability\":\"pure\",\"type\":\"function\"}]",
}

// BorValidatorSetABI is the input ABI used to generate the binding from.
// Deprecated: Use BorValidatorSetMetaData.ABI instead.
var BorValidatorSetABI = BorValidatorSetMetaData.ABI

// BorValidatorSet is an auto generated Go binding around an Ethereum contract.
type BorValidatorSet struct {
	BorValidatorSetCaller     // Read-only binding to the contract
	BorValidatorSetTransactor // Write-only binding to the contract
	BorValidatorSetFilterer   // Log filterer for contract events
}

// BorValidatorSetCaller is an auto generated read-only Go binding around an Ethereum contract.
type BorValidatorSetCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// BorValidatorSetTransactor is an auto generated write-only Go binding around an Ethereum contract.
type BorValidatorSetTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// BorValidatorSetFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type BorValidatorSetFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// BorValidatorSetSession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type BorValidatorSetSession struct {
	Contract     *BorValidatorSet  // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// BorValidatorSetCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type BorValidatorSetCallerSession struct {
	Contract *BorValidatorSetCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts          // Call options to use throughout this session
}

// BorValidatorSetTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type BorValidatorSetTransactorSession struct {
	Contract     *BorValidatorSetTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts          // Transaction auth options to use throughout this session
}

// BorValidatorSetRaw is an auto generated low-level Go binding around an Ethereum contract.
type BorValidatorSetRaw struct {
	Contract *BorValidatorSet // Generic contract binding to access the raw methods on
}

// BorValidatorSetCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type BorValidatorSetCallerRaw struct {
	Contract *BorValidatorSetCaller // Generic read-only contract binding to access the raw methods on
}

// BorValidatorSetTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type BorValidatorSetTransactorRaw struct {
	Contract *BorValidatorSetTransactor // Generic write-only contract binding to access the raw methods on
}

// NewBorValidatorSet creates a new instance of BorValidatorSet, bound to a specific deployed contract.
func NewBorValidatorSet(address common.Address, backend bind.ContractBackend) (*BorValidatorSet, error) {
	contract, err := bindBorValidatorSet(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &BorValidatorSet{BorValidatorSetCaller: BorValidatorSetCaller{contract: contract}, BorValidatorSetTransactor: BorValidatorSetTransactor{contract: contract}, BorValidatorSetFilterer: BorValidatorSetFilterer{contract: contract}}, nil
}

// NewBorValidatorSetCaller creates a new read-only instance of BorValidatorSet, bound to a specific deployed contract.
func NewBorValidatorSetCaller(address common.Address, caller bind.ContractCaller) (*BorValidatorSetCaller, error) {
	contract, err := bindBorValidatorSet(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &BorValidatorSetCaller{contract: contract}, nil
}

// NewBorValidatorSetTransactor creates a new write-only instance of BorValidatorSet, bound to a specific deployed contract.
func NewBorValidatorSetTransactor(address common.Address, transactor bind.ContractTransactor) (*BorValidatorSetTransactor, error) {
	contract, err := bindBorValidatorSet(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &BorValidatorSetTransactor{contract: contract}, nil
}

// NewBorValidatorSetFilterer creates a new log filterer instance of BorValidatorSet, bound to a specific deployed contract.
func NewBorValidatorSetFilterer(address common.Address, filterer bind.ContractFilterer) (*BorValidatorSetFilterer, error) {
	contract, err := bindBorValidatorSet(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &BorValidatorSetFilterer{contract: contract}, nil
}

// bindBorValidatorSet binds a generic wrapper to an already deployed contract.
func bindBorValidatorSet(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := abi.JSON(strings.NewReader(BorValidatorSetABI))
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_BorValidatorSet *BorValidatorSetRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _BorValidatorSet.Contract.BorValidatorSetCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_BorValidatorSet *BorValidatorSetRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _BorValidatorSet.Contract.BorValidatorSetTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_BorValidatorSet *BorValidatorSetRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _BorValidatorSet.Contract.BorValidatorSetTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_BorValidatorSet *BorValidatorSetCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _BorValidatorSet.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_BorValidatorSet *BorValidatorSetTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _BorValidatorSet.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_BorValidatorSet *BorValidatorSetTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _BorValidatorSet.Contract.contract.Transact(opts, method, params...)
}

// BORID is a free data retrieval call binding the contract method 0xae756451.
//
// Solidity: function BOR_ID() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCaller) BORID(opts *bind.CallOpts) ([32]byte, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "BOR_ID")

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// BORID is a free data retrieval call binding the contract method 0xae756451.
//
// Solidity: function BOR_ID() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetSession) BORID() ([32]byte, error) {
	return _BorValidatorSet.Contract.BORID(&_BorValidatorSet.CallOpts)
}

// BORID is a free data retrieval call binding the contract method 0xae756451.
//
// Solidity: function BOR_ID() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCallerSession) BORID() ([32]byte, error) {
	return _BorValidatorSet.Contract.BORID(&_BorValidatorSet.CallOpts)
}

// CHAIN is a free data retrieval call binding the contract method 0x43ee8213.
//
// Solidity: function CHAIN() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCaller) CHAIN(opts *bind.CallOpts) ([32]byte, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "CHAIN")

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// CHAIN is a free data retrieval call binding the contract method 0x43ee8213.
//
// Solidity: function CHAIN() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetSession) CHAIN() ([32]byte, error) {
	return _BorValidatorSet.Contract.CHAIN(&_BorValidatorSet.CallOpts)
}

// CHAIN is a free data retrieval call binding the contract method 0x43ee8213.
//
// Solidity: function CHAIN() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCallerSession) CHAIN() ([32]byte, error) {
	return _BorValidatorSet.Contract.CHAIN(&_BorValidatorSet.CallOpts)
}

// FIRSTENDBLOCK is a free data retrieval call binding the contract method 0x66332354.
//
// Solidity: function FIRST_END_BLOCK() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) FIRSTENDBLOCK(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "FIRST_END_BLOCK")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// FIRSTENDBLOCK is a free data retrieval call binding the contract method 0x66332354.
//
// Solidity: function FIRST_END_BLOCK() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) FIRSTENDBLOCK() (*big.Int, error) {
	return _BorValidatorSet.Contract.FIRSTENDBLOCK(&_BorValidatorSet.CallOpts)
}

// FIRSTENDBLOCK is a free data retrieval call binding the contract method 0x66332354.
//
// Solidity: function FIRST_END_BLOCK() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) FIRSTENDBLOCK() (*big.Int, error) {
	return _BorValidatorSet.Contract.FIRSTENDBLOCK(&_BorValidatorSet.CallOpts)
}

// ROUNDTYPE is a free data retrieval call binding the contract method 0x98ab2b62.
//
// Solidity: function ROUND_TYPE() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCaller) ROUNDTYPE(opts *bind.CallOpts) ([32]byte, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "ROUND_TYPE")

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// ROUNDTYPE is a free data retrieval call binding the contract method 0x98ab2b62.
//
// Solidity: function ROUND_TYPE() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetSession) ROUNDTYPE() ([32]byte, error) {
	return _BorValidatorSet.Contract.ROUNDTYPE(&_BorValidatorSet.CallOpts)
}

// ROUNDTYPE is a free data retrieval call binding the contract method 0x98ab2b62.
//
// Solidity: function ROUND_TYPE() view returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCallerSession) ROUNDTYPE() ([32]byte, error) {
	return _BorValidatorSet.Contract.ROUNDTYPE(&_BorValidatorSet.CallOpts)
}

// SPRINT is a free data retrieval call binding the contract method 0x2bc06564.
//
// Solidity: function SPRINT() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) SPRINT(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "SPRINT")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// SPRINT is a free data retrieval call binding the contract method 0x2bc06564.
//
// Solidity: function SPRINT() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) SPRINT() (*big.Int, error) {
	return _BorValidatorSet.Contract.SPRINT(&_BorValidatorSet.CallOpts)
}

// SPRINT is a free data retrieval call binding the contract method 0x2bc06564.
//
// Solidity: function SPRINT() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) SPRINT() (*big.Int, error) {
	return _BorValidatorSet.Contract.SPRINT(&_BorValidatorSet.CallOpts)
}

// SYSTEMADDRESS is a free data retrieval call binding the contract method 0x3434735f.
//
// Solidity: function SYSTEM_ADDRESS() view returns(address)
func (_BorValidatorSet *BorValidatorSetCaller) SYSTEMADDRESS(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "SYSTEM_ADDRESS")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// SYSTEMADDRESS is a free data retrieval call binding the contract method 0x3434735f.
//
// Solidity: function SYSTEM_ADDRESS() view returns(address)
func (_BorValidatorSet *BorValidatorSetSession) SYSTEMADDRESS() (common.Address, error) {
	return _BorValidatorSet.Contract.SYSTEMADDRESS(&_BorValidatorSet.CallOpts)
}

// SYSTEMADDRESS is a free data retrieval call binding the contract method 0x3434735f.
//
// Solidity: function SYSTEM_ADDRESS() view returns(address)
func (_BorValidatorSet *BorValidatorSetCallerSession) SYSTEMADDRESS() (common.Address, error) {
	return _BorValidatorSet.Contract.SYSTEMADDRESS(&_BorValidatorSet.CallOpts)
}

// VOTETYPE is a free data retrieval call binding the contract method 0xd5b844eb.
//
// Solidity: function VOTE_TYPE() view returns(uint8)
func (_BorValidatorSet *BorValidatorSetCaller) VOTETYPE(opts *bind.CallOpts) (uint8, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "VOTE_TYPE")

	if err != nil {
		return *new(uint8), err
	}

	out0 := *abi.ConvertType(out[0], new(uint8)).(*uint8)

	return out0, err

}

// VOTETYPE is a free data retrieval call binding the contract method 0xd5b844eb.
//
// Solidity: function VOTE_TYPE() view returns(uint8)
func (_BorValidatorSet *BorValidatorSetSession) VOTETYPE() (uint8, error) {
	return _BorValidatorSet.Contract.VOTETYPE(&_BorValidatorSet.CallOpts)
}

// VOTETYPE is a free data retrieval call binding the contract method 0xd5b844eb.
//
// Solidity: function VOTE_TYPE() view returns(uint8)
func (_BorValidatorSet *BorValidatorSetCallerSession) VOTETYPE() (uint8, error) {
	return _BorValidatorSet.Contract.VOTETYPE(&_BorValidatorSet.CallOpts)
}

// CheckMembership is a free data retrieval call binding the contract method 0x35ddfeea.
//
// Solidity: function checkMembership(bytes32 rootHash, bytes32 leaf, bytes proof) pure returns(bool)
func (_BorValidatorSet *BorValidatorSetCaller) CheckMembership(opts *bind.CallOpts, rootHash [32]byte, leaf [32]byte, proof []byte) (bool, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "checkMembership", rootHash, leaf, proof)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// CheckMembership is a free data retrieval call binding the contract method 0x35ddfeea.
//
// Solidity: function checkMembership(bytes32 rootHash, bytes32 leaf, bytes proof) pure returns(bool)
func (_BorValidatorSet *BorValidatorSetSession) CheckMembership(rootHash [32]byte, leaf [32]byte, proof []byte) (bool, error) {
	return _BorValidatorSet.Contract.CheckMembership(&_BorValidatorSet.CallOpts, rootHash, leaf, proof)
}

// CheckMembership is a free data retrieval call binding the contract method 0x35ddfeea.
//
// Solidity: function checkMembership(bytes32 rootHash, bytes32 leaf, bytes proof) pure returns(bool)
func (_BorValidatorSet *BorValidatorSetCallerSession) CheckMembership(rootHash [32]byte, leaf [32]byte, proof []byte) (bool, error) {
	return _BorValidatorSet.Contract.CheckMembership(&_BorValidatorSet.CallOpts, rootHash, leaf, proof)
}

// CurrentSpanNumber is a free data retrieval call binding the contract method 0x4dbc959f.
//
// Solidity: function currentSpanNumber() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) CurrentSpanNumber(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "currentSpanNumber")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// CurrentSpanNumber is a free data retrieval call binding the contract method 0x4dbc959f.
//
// Solidity: function currentSpanNumber() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) CurrentSpanNumber() (*big.Int, error) {
	return _BorValidatorSet.Contract.CurrentSpanNumber(&_BorValidatorSet.CallOpts)
}

// CurrentSpanNumber is a free data retrieval call binding the contract method 0x4dbc959f.
//
// Solidity: function currentSpanNumber() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) CurrentSpanNumber() (*big.Int, error) {
	return _BorValidatorSet.Contract.CurrentSpanNumber(&_BorValidatorSet.CallOpts)
}

// CurrentSprint is a free data retrieval call binding the contract method 0xe3b7c924.
//
// Solidity: function currentSprint() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) CurrentSprint(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "currentSprint")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// CurrentSprint is a free data retrieval call binding the contract method 0xe3b7c924.
//
// Solidity: function currentSprint() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) CurrentSprint() (*big.Int, error) {
	return _BorValidatorSet.Contract.CurrentSprint(&_BorValidatorSet.CallOpts)
}

// CurrentSprint is a free data retrieval call binding the contract method 0xe3b7c924.
//
// Solidity: function currentSprint() view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) CurrentSprint() (*big.Int, error) {
	return _BorValidatorSet.Contract.CurrentSprint(&_BorValidatorSet.CallOpts)
}

// GetBorValidators is a free data retrieval call binding the contract method 0x0c35b1cb.
//
// Solidity: function getBorValidators(uint256 number) view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetCaller) GetBorValidators(opts *bind.CallOpts, number *big.Int) ([]common.Address, []*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getBorValidators", number)

	if err != nil {
		return *new([]common.Address), *new([]*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new([]common.Address)).(*[]common.Address)
	out1 := *abi.ConvertType(out[1], new([]*big.Int)).(*[]*big.Int)

	return out0, out1, err

}

// GetBorValidators is a free data retrieval call binding the contract method 0x0c35b1cb.
//
// Solidity: function getBorValidators(uint256 number) view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetSession) GetBorValidators(number *big.Int) ([]common.Address, []*big.Int, error) {
	return _BorValidatorSet.Contract.GetBorValidators(&_BorValidatorSet.CallOpts, number)
}

// GetBorValidators is a free data retrieval call binding the contract method 0x0c35b1cb.
//
// Solidity: function getBorValidators(uint256 number) view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetCallerSession) GetBorValidators(number *big.Int) ([]common.Address, []*big.Int, error) {
	return _BorValidatorSet.Contract.GetBorValidators(&_BorValidatorSet.CallOpts, number)
}

// GetCurrentSpan is a free data retrieval call binding the contract method 0xaf26aa96.
//
// Solidity: function getCurrentSpan() view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCaller) GetCurrentSpan(opts *bind.CallOpts) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getCurrentSpan")

	outstruct := new(struct {
		Number     *big.Int
		StartBlock *big.Int
		EndBlock   *big.Int
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Number = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.StartBlock = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.EndBlock = *abi.ConvertType(out[2], new(*big.Int)).(**big.Int)

	return *outstruct, err

}

// GetCurrentSpan is a free data retrieval call binding the contract method 0xaf26aa96.
//
// Solidity: function getCurrentSpan() view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetSession) GetCurrentSpan() (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.GetCurrentSpan(&_BorValidatorSet.CallOpts)
}

// GetCurrentSpan is a free data retrieval call binding the contract method 0xaf26aa96.
//
// Solidity: function getCurrentSpan() view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetCurrentSpan() (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.GetCurrentSpan(&_BorValidatorSet.CallOpts)
}

// GetInitialValidators is a free data retrieval call binding the contract method 0x65b3a1e2.
//
// Solidity: function getInitialValidators() view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetCaller) GetInitialValidators(opts *bind.CallOpts) ([]common.Address, []*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getInitialValidators")

	if err != nil {
		return *new([]common.Address), *new([]*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new([]common.Address)).(*[]common.Address)
	out1 := *abi.ConvertType(out[1], new([]*big.Int)).(*[]*big.Int)

	return out0, out1, err

}

// GetInitialValidators is a free data retrieval call binding the contract method 0x65b3a1e2.
//
// Solidity: function getInitialValidators() view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetSession) GetInitialValidators() ([]common.Address, []*big.Int, error) {
	return _BorValidatorSet.Contract.GetInitialValidators(&_BorValidatorSet.CallOpts)
}

// GetInitialValidators is a free data retrieval call binding the contract method 0x65b3a1e2.
//
// Solidity: function getInitialValidators() view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetCallerSession) GetInitialValidators() ([]common.Address, []*big.Int, error) {
	return _BorValidatorSet.Contract.GetInitialValidators(&_BorValidatorSet.CallOpts)
}

// GetNextSpan is a free data retrieval call binding the contract method 0x60c8614d.
//
// Solidity: function getNextSpan() view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCaller) GetNextSpan(opts *bind.CallOpts) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getNextSpan")

	outstruct := new(struct {
		Number     *big.Int
		StartBlock *big.Int
		EndBlock   *big.Int
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Number = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.StartBlock = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.EndBlock = *abi.ConvertType(out[2], new(*big.Int)).(**big.Int)

	return *outstruct, err

}

// GetNextSpan is a free data retrieval call binding the contract method 0x60c8614d.
//
// Solidity: function getNextSpan() view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetSession) GetNextSpan() (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.GetNextSpan(&_BorValidatorSet.CallOpts)
}

// GetNextSpan is a free data retrieval call binding the contract method 0x60c8614d.
//
// Solidity: function getNextSpan() view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetNextSpan() (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.GetNextSpan(&_BorValidatorSet.CallOpts)
}

// GetProducersTotalStakeBySpan is a free data retrieval call binding the contract method 0x9d11b807.
//
// Solidity: function getProducersTotalStakeBySpan(uint256 span) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) GetProducersTotalStakeBySpan(opts *bind.CallOpts, span *big.Int) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getProducersTotalStakeBySpan", span)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// GetProducersTotalStakeBySpan is a free data retrieval call binding the contract method 0x9d11b807.
//
// Solidity: function getProducersTotalStakeBySpan(uint256 span) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) GetProducersTotalStakeBySpan(span *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetProducersTotalStakeBySpan(&_BorValidatorSet.CallOpts, span)
}

// GetProducersTotalStakeBySpan is a free data retrieval call binding the contract method 0x9d11b807.
//
// Solidity: function getProducersTotalStakeBySpan(uint256 span) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetProducersTotalStakeBySpan(span *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetProducersTotalStakeBySpan(&_BorValidatorSet.CallOpts, span)
}

// GetSpan is a free data retrieval call binding the contract method 0x047a6c5b.
//
// Solidity: function getSpan(uint256 span) view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCaller) GetSpan(opts *bind.CallOpts, span *big.Int) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getSpan", span)

	outstruct := new(struct {
		Number     *big.Int
		StartBlock *big.Int
		EndBlock   *big.Int
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Number = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.StartBlock = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.EndBlock = *abi.ConvertType(out[2], new(*big.Int)).(**big.Int)

	return *outstruct, err

}

// GetSpan is a free data retrieval call binding the contract method 0x047a6c5b.
//
// Solidity: function getSpan(uint256 span) view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetSession) GetSpan(span *big.Int) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.GetSpan(&_BorValidatorSet.CallOpts, span)
}

// GetSpan is a free data retrieval call binding the contract method 0x047a6c5b.
//
// Solidity: function getSpan(uint256 span) view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetSpan(span *big.Int) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.GetSpan(&_BorValidatorSet.CallOpts, span)
}

// GetSpanByBlock is a free data retrieval call binding the contract method 0xb71d7a69.
//
// Solidity: function getSpanByBlock(uint256 number) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) GetSpanByBlock(opts *bind.CallOpts, number *big.Int) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getSpanByBlock", number)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// GetSpanByBlock is a free data retrieval call binding the contract method 0xb71d7a69.
//
// Solidity: function getSpanByBlock(uint256 number) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) GetSpanByBlock(number *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetSpanByBlock(&_BorValidatorSet.CallOpts, number)
}

// GetSpanByBlock is a free data retrieval call binding the contract method 0xb71d7a69.
//
// Solidity: function getSpanByBlock(uint256 number) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetSpanByBlock(number *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetSpanByBlock(&_BorValidatorSet.CallOpts, number)
}

// GetStakePowerBySigs is a free data retrieval call binding the contract method 0x44c15cb1.
//
// Solidity: function getStakePowerBySigs(uint256 span, bytes32 dataHash, bytes sigs) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) GetStakePowerBySigs(opts *bind.CallOpts, span *big.Int, dataHash [32]byte, sigs []byte) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getStakePowerBySigs", span, dataHash, sigs)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// GetStakePowerBySigs is a free data retrieval call binding the contract method 0x44c15cb1.
//
// Solidity: function getStakePowerBySigs(uint256 span, bytes32 dataHash, bytes sigs) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) GetStakePowerBySigs(span *big.Int, dataHash [32]byte, sigs []byte) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetStakePowerBySigs(&_BorValidatorSet.CallOpts, span, dataHash, sigs)
}

// GetStakePowerBySigs is a free data retrieval call binding the contract method 0x44c15cb1.
//
// Solidity: function getStakePowerBySigs(uint256 span, bytes32 dataHash, bytes sigs) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetStakePowerBySigs(span *big.Int, dataHash [32]byte, sigs []byte) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetStakePowerBySigs(&_BorValidatorSet.CallOpts, span, dataHash, sigs)
}

// GetValidatorBySigner is a free data retrieval call binding the contract method 0x44d6528f.
//
// Solidity: function getValidatorBySigner(uint256 span, address signer) view returns((uint256,uint256,address) result)
func (_BorValidatorSet *BorValidatorSetCaller) GetValidatorBySigner(opts *bind.CallOpts, span *big.Int, signer common.Address) (BorValidatorSetValidator, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getValidatorBySigner", span, signer)

	if err != nil {
		return *new(BorValidatorSetValidator), err
	}

	out0 := *abi.ConvertType(out[0], new(BorValidatorSetValidator)).(*BorValidatorSetValidator)

	return out0, err

}

// GetValidatorBySigner is a free data retrieval call binding the contract method 0x44d6528f.
//
// Solidity: function getValidatorBySigner(uint256 span, address signer) view returns((uint256,uint256,address) result)
func (_BorValidatorSet *BorValidatorSetSession) GetValidatorBySigner(span *big.Int, signer common.Address) (BorValidatorSetValidator, error) {
	return _BorValidatorSet.Contract.GetValidatorBySigner(&_BorValidatorSet.CallOpts, span, signer)
}

// GetValidatorBySigner is a free data retrieval call binding the contract method 0x44d6528f.
//
// Solidity: function getValidatorBySigner(uint256 span, address signer) view returns((uint256,uint256,address) result)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetValidatorBySigner(span *big.Int, signer common.Address) (BorValidatorSetValidator, error) {
	return _BorValidatorSet.Contract.GetValidatorBySigner(&_BorValidatorSet.CallOpts, span, signer)
}

// GetValidators is a free data retrieval call binding the contract method 0xb7ab4db5.
//
// Solidity: function getValidators() view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetCaller) GetValidators(opts *bind.CallOpts) ([]common.Address, []*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getValidators")

	if err != nil {
		return *new([]common.Address), *new([]*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new([]common.Address)).(*[]common.Address)
	out1 := *abi.ConvertType(out[1], new([]*big.Int)).(*[]*big.Int)

	return out0, out1, err

}

// GetValidators is a free data retrieval call binding the contract method 0xb7ab4db5.
//
// Solidity: function getValidators() view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetSession) GetValidators() ([]common.Address, []*big.Int, error) {
	return _BorValidatorSet.Contract.GetValidators(&_BorValidatorSet.CallOpts)
}

// GetValidators is a free data retrieval call binding the contract method 0xb7ab4db5.
//
// Solidity: function getValidators() view returns(address[], uint256[])
func (_BorValidatorSet *BorValidatorSetCallerSession) GetValidators() ([]common.Address, []*big.Int, error) {
	return _BorValidatorSet.Contract.GetValidators(&_BorValidatorSet.CallOpts)
}

// GetValidatorsTotalStakeBySpan is a free data retrieval call binding the contract method 0x2eddf352.
//
// Solidity: function getValidatorsTotalStakeBySpan(uint256 span) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) GetValidatorsTotalStakeBySpan(opts *bind.CallOpts, span *big.Int) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "getValidatorsTotalStakeBySpan", span)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// GetValidatorsTotalStakeBySpan is a free data retrieval call binding the contract method 0x2eddf352.
//
// Solidity: function getValidatorsTotalStakeBySpan(uint256 span) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) GetValidatorsTotalStakeBySpan(span *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetValidatorsTotalStakeBySpan(&_BorValidatorSet.CallOpts, span)
}

// GetValidatorsTotalStakeBySpan is a free data retrieval call binding the contract method 0x2eddf352.
//
// Solidity: function getValidatorsTotalStakeBySpan(uint256 span) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) GetValidatorsTotalStakeBySpan(span *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.GetValidatorsTotalStakeBySpan(&_BorValidatorSet.CallOpts, span)
}

// InnerNode is a free data retrieval call binding the contract method 0x2de3a180.
//
// Solidity: function innerNode(bytes32 left, bytes32 right) pure returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCaller) InnerNode(opts *bind.CallOpts, left [32]byte, right [32]byte) ([32]byte, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "innerNode", left, right)

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// InnerNode is a free data retrieval call binding the contract method 0x2de3a180.
//
// Solidity: function innerNode(bytes32 left, bytes32 right) pure returns(bytes32)
func (_BorValidatorSet *BorValidatorSetSession) InnerNode(left [32]byte, right [32]byte) ([32]byte, error) {
	return _BorValidatorSet.Contract.InnerNode(&_BorValidatorSet.CallOpts, left, right)
}

// InnerNode is a free data retrieval call binding the contract method 0x2de3a180.
//
// Solidity: function innerNode(bytes32 left, bytes32 right) pure returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCallerSession) InnerNode(left [32]byte, right [32]byte) ([32]byte, error) {
	return _BorValidatorSet.Contract.InnerNode(&_BorValidatorSet.CallOpts, left, right)
}

// IsCurrentProducer is a free data retrieval call binding the contract method 0x70ba5707.
//
// Solidity: function isCurrentProducer(address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCaller) IsCurrentProducer(opts *bind.CallOpts, signer common.Address) (bool, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "isCurrentProducer", signer)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsCurrentProducer is a free data retrieval call binding the contract method 0x70ba5707.
//
// Solidity: function isCurrentProducer(address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetSession) IsCurrentProducer(signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsCurrentProducer(&_BorValidatorSet.CallOpts, signer)
}

// IsCurrentProducer is a free data retrieval call binding the contract method 0x70ba5707.
//
// Solidity: function isCurrentProducer(address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCallerSession) IsCurrentProducer(signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsCurrentProducer(&_BorValidatorSet.CallOpts, signer)
}

// IsCurrentValidator is a free data retrieval call binding the contract method 0x55614fcc.
//
// Solidity: function isCurrentValidator(address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCaller) IsCurrentValidator(opts *bind.CallOpts, signer common.Address) (bool, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "isCurrentValidator", signer)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsCurrentValidator is a free data retrieval call binding the contract method 0x55614fcc.
//
// Solidity: function isCurrentValidator(address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetSession) IsCurrentValidator(signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsCurrentValidator(&_BorValidatorSet.CallOpts, signer)
}

// IsCurrentValidator is a free data retrieval call binding the contract method 0x55614fcc.
//
// Solidity: function isCurrentValidator(address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCallerSession) IsCurrentValidator(signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsCurrentValidator(&_BorValidatorSet.CallOpts, signer)
}

// IsProducer is a free data retrieval call binding the contract method 0x1270b574.
//
// Solidity: function isProducer(uint256 span, address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCaller) IsProducer(opts *bind.CallOpts, span *big.Int, signer common.Address) (bool, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "isProducer", span, signer)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsProducer is a free data retrieval call binding the contract method 0x1270b574.
//
// Solidity: function isProducer(uint256 span, address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetSession) IsProducer(span *big.Int, signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsProducer(&_BorValidatorSet.CallOpts, span, signer)
}

// IsProducer is a free data retrieval call binding the contract method 0x1270b574.
//
// Solidity: function isProducer(uint256 span, address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCallerSession) IsProducer(span *big.Int, signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsProducer(&_BorValidatorSet.CallOpts, span, signer)
}

// IsValidator is a free data retrieval call binding the contract method 0x23f2a73f.
//
// Solidity: function isValidator(uint256 span, address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCaller) IsValidator(opts *bind.CallOpts, span *big.Int, signer common.Address) (bool, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "isValidator", span, signer)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsValidator is a free data retrieval call binding the contract method 0x23f2a73f.
//
// Solidity: function isValidator(uint256 span, address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetSession) IsValidator(span *big.Int, signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsValidator(&_BorValidatorSet.CallOpts, span, signer)
}

// IsValidator is a free data retrieval call binding the contract method 0x23f2a73f.
//
// Solidity: function isValidator(uint256 span, address signer) view returns(bool)
func (_BorValidatorSet *BorValidatorSetCallerSession) IsValidator(span *big.Int, signer common.Address) (bool, error) {
	return _BorValidatorSet.Contract.IsValidator(&_BorValidatorSet.CallOpts, span, signer)
}

// LeafNode is a free data retrieval call binding the contract method 0x582a8d08.
//
// Solidity: function leafNode(bytes32 d) pure returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCaller) LeafNode(opts *bind.CallOpts, d [32]byte) ([32]byte, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "leafNode", d)

	if err != nil {
		return *new([32]byte), err
	}

	out0 := *abi.ConvertType(out[0], new([32]byte)).(*[32]byte)

	return out0, err

}

// LeafNode is a free data retrieval call binding the contract method 0x582a8d08.
//
// Solidity: function leafNode(bytes32 d) pure returns(bytes32)
func (_BorValidatorSet *BorValidatorSetSession) LeafNode(d [32]byte) ([32]byte, error) {
	return _BorValidatorSet.Contract.LeafNode(&_BorValidatorSet.CallOpts, d)
}

// LeafNode is a free data retrieval call binding the contract method 0x582a8d08.
//
// Solidity: function leafNode(bytes32 d) pure returns(bytes32)
func (_BorValidatorSet *BorValidatorSetCallerSession) LeafNode(d [32]byte) ([32]byte, error) {
	return _BorValidatorSet.Contract.LeafNode(&_BorValidatorSet.CallOpts, d)
}

// Producers is a free data retrieval call binding the contract method 0x687a9bd6.
//
// Solidity: function producers(uint256 , uint256 ) view returns(uint256 id, uint256 power, address signer)
func (_BorValidatorSet *BorValidatorSetCaller) Producers(opts *bind.CallOpts, arg0 *big.Int, arg1 *big.Int) (struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "producers", arg0, arg1)

	outstruct := new(struct {
		Id     *big.Int
		Power  *big.Int
		Signer common.Address
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Id = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.Power = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.Signer = *abi.ConvertType(out[2], new(common.Address)).(*common.Address)

	return *outstruct, err

}

// Producers is a free data retrieval call binding the contract method 0x687a9bd6.
//
// Solidity: function producers(uint256 , uint256 ) view returns(uint256 id, uint256 power, address signer)
func (_BorValidatorSet *BorValidatorSetSession) Producers(arg0 *big.Int, arg1 *big.Int) (struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}, error) {
	return _BorValidatorSet.Contract.Producers(&_BorValidatorSet.CallOpts, arg0, arg1)
}

// Producers is a free data retrieval call binding the contract method 0x687a9bd6.
//
// Solidity: function producers(uint256 , uint256 ) view returns(uint256 id, uint256 power, address signer)
func (_BorValidatorSet *BorValidatorSetCallerSession) Producers(arg0 *big.Int, arg1 *big.Int) (struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}, error) {
	return _BorValidatorSet.Contract.Producers(&_BorValidatorSet.CallOpts, arg0, arg1)
}

// SpanNumbers is a free data retrieval call binding the contract method 0xc1b3c919.
//
// Solidity: function spanNumbers(uint256 ) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCaller) SpanNumbers(opts *bind.CallOpts, arg0 *big.Int) (*big.Int, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "spanNumbers", arg0)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// SpanNumbers is a free data retrieval call binding the contract method 0xc1b3c919.
//
// Solidity: function spanNumbers(uint256 ) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetSession) SpanNumbers(arg0 *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.SpanNumbers(&_BorValidatorSet.CallOpts, arg0)
}

// SpanNumbers is a free data retrieval call binding the contract method 0xc1b3c919.
//
// Solidity: function spanNumbers(uint256 ) view returns(uint256)
func (_BorValidatorSet *BorValidatorSetCallerSession) SpanNumbers(arg0 *big.Int) (*big.Int, error) {
	return _BorValidatorSet.Contract.SpanNumbers(&_BorValidatorSet.CallOpts, arg0)
}

// Spans is a free data retrieval call binding the contract method 0xf59cf565.
//
// Solidity: function spans(uint256 ) view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCaller) Spans(opts *bind.CallOpts, arg0 *big.Int) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "spans", arg0)

	outstruct := new(struct {
		Number     *big.Int
		StartBlock *big.Int
		EndBlock   *big.Int
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Number = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.StartBlock = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.EndBlock = *abi.ConvertType(out[2], new(*big.Int)).(**big.Int)

	return *outstruct, err

}

// Spans is a free data retrieval call binding the contract method 0xf59cf565.
//
// Solidity: function spans(uint256 ) view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetSession) Spans(arg0 *big.Int) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.Spans(&_BorValidatorSet.CallOpts, arg0)
}

// Spans is a free data retrieval call binding the contract method 0xf59cf565.
//
// Solidity: function spans(uint256 ) view returns(uint256 number, uint256 startBlock, uint256 endBlock)
func (_BorValidatorSet *BorValidatorSetCallerSession) Spans(arg0 *big.Int) (struct {
	Number     *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
}, error) {
	return _BorValidatorSet.Contract.Spans(&_BorValidatorSet.CallOpts, arg0)
}

// Validators is a free data retrieval call binding the contract method 0xdcf2793a.
//
// Solidity: function validators(uint256 , uint256 ) view returns(uint256 id, uint256 power, address signer)
func (_BorValidatorSet *BorValidatorSetCaller) Validators(opts *bind.CallOpts, arg0 *big.Int, arg1 *big.Int) (struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}, error) {
	var out []interface{}
	err := _BorValidatorSet.contract.Call(opts, &out, "validators", arg0, arg1)

	outstruct := new(struct {
		Id     *big.Int
		Power  *big.Int
		Signer common.Address
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Id = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.Power = *abi.ConvertType(out[1], new(*big.Int)).(**big.Int)
	outstruct.Signer = *abi.ConvertType(out[2], new(common.Address)).(*common.Address)

	return *outstruct, err

}

// Validators is a free data retrieval call binding the contract method 0xdcf2793a.
//
// Solidity: function validators(uint256 , uint256 ) view returns(uint256 id, uint256 power, address signer)
func (_BorValidatorSet *BorValidatorSetSession) Validators(arg0 *big.Int, arg1 *big.Int) (struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}, error) {
	return _BorValidatorSet.Contract.Validators(&_BorValidatorSet.CallOpts, arg0, arg1)
}

// Validators is a free data retrieval call binding the contract method 0xdcf2793a.
//
// Solidity: function validators(uint256 , uint256 ) view returns(uint256 id, uint256 power, address signer)
func (_BorValidatorSet *BorValidatorSetCallerSession) Validators(arg0 *big.Int, arg1 *big.Int) (struct {
	Id     *big.Int
	Power  *big.Int
	Signer common.Address
}, error) {
	return _BorValidatorSet.Contract.Validators(&_BorValidatorSet.CallOpts, arg0, arg1)
}

// CommitSpan is a paid mutator transaction binding the contract method 0x23c2a2b4.
//
// Solidity: function commitSpan(uint256 newSpan, uint256 startBlock, uint256 endBlock, bytes validatorBytes, bytes producerBytes) returns()
func (_BorValidatorSet *BorValidatorSetTransactor) CommitSpan(opts *bind.TransactOpts, newSpan *big.Int, startBlock *big.Int, endBlock *big.Int, validatorBytes []byte, producerBytes []byte) (*types.Transaction, error) {
	return _BorValidatorSet.contract.Transact(opts, "commitSpan", newSpan, startBlock, endBlock, validatorBytes, producerBytes)
}

// CommitSpan is a paid mutator transaction binding the contract method 0x23c2a2b4.
//
// Solidity: function commitSpan(uint256 newSpan, uint256 startBlock, uint256 endBlock, bytes validatorBytes, bytes producerBytes) returns()
func (_BorValidatorSet *BorValidatorSetSession) CommitSpan(newSpan *big.Int, startBlock *big.Int, endBlock *big.Int, validatorBytes []byte, producerBytes []byte) (*types.Transaction, error) {
	return _BorValidatorSet.Contract.CommitSpan(&_BorValidatorSet.TransactOpts, newSpan, startBlock, endBlock, validatorBytes, producerBytes)
}

// CommitSpan is a paid mutator transaction binding the contract method 0x23c2a2b4.
//
// Solidity: function commitSpan(uint256 newSpan, uint256 startBlock, uint256 endBlock, bytes validatorBytes, bytes producerBytes) returns()
func (_BorValidatorSet *BorValidatorSetTransactorSession) CommitSpan(newSpan *big.Int, startBlock *big.Int, endBlock *big.Int, validatorBytes []byte, producerBytes []byte) (*types.Transaction, error) {
	return _BorValidatorSet.Contract.CommitSpan(&_BorValidatorSet.TransactOpts, newSpan, startBlock, endBlock, validatorBytes, producerBytes)
}

// BorValidatorSetNewSpanIterator is returned from FilterNewSpan and is used to iterate over the raw logs and unpacked data for NewSpan events raised by the BorValidatorSet contract.
type BorValidatorSetNewSpanIterator struct {
	Event *BorValidatorSetNewSpan // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *BorValidatorSetNewSpanIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(BorValidatorSetNewSpan)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(BorValidatorSetNewSpan)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *BorValidatorSetNewSpanIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *BorValidatorSetNewSpanIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// BorValidatorSetNewSpan represents a NewSpan event raised by the BorValidatorSet contract.
type BorValidatorSetNewSpan struct {
	Id         *big.Int
	StartBlock *big.Int
	EndBlock   *big.Int
	Raw        types.Log // Blockchain specific contextual infos
}

// FilterNewSpan is a free log retrieval operation binding the contract event 0xac9e8537ff98ccc9f53f34174714e4b56eee15cb11eafa9512d2914c60790c62.
//
// Solidity: event NewSpan(uint256 indexed id, uint256 indexed startBlock, uint256 indexed endBlock)
func (_BorValidatorSet *BorValidatorSetFilterer) FilterNewSpan(opts *bind.FilterOpts, id []*big.Int, startBlock []*big.Int, endBlock []*big.Int) (*BorValidatorSetNewSpanIterator, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}
	var startBlockRule []interface{}
	for _, startBlockItem := range startBlock {
		startBlockRule = append(startBlockRule, startBlockItem)
	}
	var endBlockRule []interface{}
	for _, endBlockItem := range endBlock {
		endBlockRule = append(endBlockRule, endBlockItem)
	}

	logs, sub, err := _BorValidatorSet.contract.FilterLogs(opts, "NewSpan", idRule, startBlockRule, endBlockRule)
	if err != nil {
		return nil, err
	}
	return &BorValidatorSetNewSpanIterator{contract: _BorValidatorSet.contract, event: "NewSpan", logs: logs, sub: sub}, nil
}

// WatchNewSpan is a free log subscription operation binding the contract event 0xac9e8537ff98ccc9f53f34174714e4b56eee15cb11eafa9512d2914c60790c62.
//
// Solidity: event NewSpan(uint256 indexed id, uint256 indexed startBlock, uint256 indexed endBlock)
func (_BorValidatorSet *BorValidatorSetFilterer) WatchNewSpan(opts *bind.WatchOpts, sink chan<- *BorValidatorSetNewSpan, id []*big.Int, startBlock []*big.Int, endBlock []*big.Int) (event.Subscription, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}
	var startBlockRule []interface{}
	for _, startBlockItem := range startBlock {
		startBlockRule = append(startBlockRule, startBlockItem)
	}
	var endBlockRule []interface{}
	for _, endBlockItem := range endBlock {
		endBlockRule = append(endBlockRule, endBlockItem)
	}

	logs, sub, err := _BorValidatorSet.contract.WatchLogs(opts, "NewSpan", idRule, startBlockRule, endBlockRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(BorValidatorSetNewSpan)
				if err := _BorValidatorSet.contract.UnpackLog(event, "NewSpan", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseNewSpan is a log parse operation binding the contract event 0xac9e8537ff98ccc9f53f34174714e4b56eee15cb11eafa9512d2914c60790c62.
//
// Solidity: event NewSpan(uint256 indexed id, uint256 indexed startBlock, uint256 indexed endBlock)
func (_BorValidatorSet *BorValidatorSetFilterer) ParseNewSpan(log types.Log) (*BorValidatorSetNewSpan, error) {
	event := new(BorValidatorSetNewSpan)
	if err := _BorValidatorSet.contract.UnpackLog(event, "NewSpan", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}
