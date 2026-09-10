package contract

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	borabi "github.com/ethereum/go-ethereum/consensus/bor/abi"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

var errReservedRegistryNotConfigured = errors.New("reserved blockspace registry contract is not configured")

// ReservedClientLookup is the slim "client for address" view returned by the
// registry contract. Aliased to registryreader.ClientLookup so the type lives
// in a leaf package that core/, miner/, and core/txpool/ can import without
// triggering an import cycle through consensus/bor/statefull.
type ReservedClientLookup = registryreader.ClientLookup

var _ registryreader.Reader = (*GenesisContractsClient)(nil)

type ReservedClient struct {
	ClientID  *big.Int
	Admin     common.Address
	GasQuota  uint64
	Active    bool
	Metadata  string
	Addresses []common.Address
}

func (gc *GenesisContractsClient) HasReservedRegistry() bool {
	if gc == nil || gc.ReservedRegistryContract == "" {
		return false
	}
	return common.HexToAddress(gc.ReservedRegistryContract) != (common.Address{})
}

func (gc *GenesisContractsClient) IsReservedAddress(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
	account common.Address,
) (bool, error) {
	if !gc.HasReservedRegistry() {
		return false, nil
	}

	values, err := gc.callReservedRegistry(state, number, hash, "isReservedAddress", account)
	if err != nil {
		return false, err
	}
	if len(values) != 1 {
		return false, fmt.Errorf("reserved registry isReservedAddress returned %d values", len(values))
	}

	reserved, ok := values[0].(bool)
	if !ok {
		return false, fmt.Errorf("reserved registry isReservedAddress returned %T", values[0])
	}

	return reserved, nil
}

func (gc *GenesisContractsClient) ReservedClientForAddress(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
	account common.Address,
) (ReservedClientLookup, error) {
	if !gc.HasReservedRegistry() {
		return ReservedClientLookup{ClientID: new(big.Int)}, nil
	}

	values, err := gc.callReservedRegistry(state, number, hash, "getClientForAddress", account)
	if err != nil {
		return ReservedClientLookup{}, err
	}
	if len(values) != 6 {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry getClientForAddress returned %d values", len(values))
	}

	clientID, ok := values[0].(*big.Int)
	if !ok {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry client id has type %T", values[0])
	}
	gasQuota, ok := values[1].(uint64)
	if !ok {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry gas quota has type %T", values[1])
	}
	admin, ok := values[2].(common.Address)
	if !ok {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry admin has type %T", values[2])
	}
	active, ok := values[3].(bool)
	if !ok {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry active flag has type %T", values[3])
	}
	feeMode, ok := values[4].(uint8)
	if !ok {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry fee mode has type %T", values[4])
	}
	effectiveFrom, ok := values[5].(uint64)
	if !ok {
		return ReservedClientLookup{}, fmt.Errorf("reserved registry effective-from has type %T", values[5])
	}

	return ReservedClientLookup{
		ClientID:      new(big.Int).Set(clientID),
		GasQuota:      gasQuota,
		Admin:         admin,
		Active:        active,
		FeeMode:       feeMode,
		EffectiveFrom: effectiveFrom,
	}, nil
}

// Root reads the registry's configVersion-derived root. Callers rebuild their
// reserved-set snapshot only when this value changes.
func (gc *GenesisContractsClient) Root(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
) (common.Hash, error) {
	if !gc.HasReservedRegistry() {
		return common.Hash{}, nil
	}

	values, err := gc.callReservedRegistry(state, number, hash, "root")
	if err != nil {
		return common.Hash{}, err
	}
	if len(values) != 1 {
		return common.Hash{}, fmt.Errorf("reserved registry root returned %d values", len(values))
	}

	root, ok := values[0].([32]byte)
	if !ok {
		return common.Hash{}, fmt.Errorf("reserved registry root has type %T", values[0])
	}

	return common.Hash(root), nil
}

func (gc *GenesisContractsClient) ReservedClientByID(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
	clientID *big.Int,
) (ReservedClient, error) {
	if !gc.HasReservedRegistry() {
		return ReservedClient{}, errReservedRegistryNotConfigured
	}
	if clientID == nil {
		return ReservedClient{}, errors.New("reserved registry client id is nil")
	}

	values, err := gc.callReservedRegistry(state, number, hash, "getClient", clientID)
	if err != nil {
		return ReservedClient{}, err
	}
	if len(values) != 5 {
		return ReservedClient{}, fmt.Errorf("reserved registry getClient returned %d values", len(values))
	}

	admin, ok := values[0].(common.Address)
	if !ok {
		return ReservedClient{}, fmt.Errorf("reserved registry admin has type %T", values[0])
	}
	gasQuota, ok := values[1].(uint64)
	if !ok {
		return ReservedClient{}, fmt.Errorf("reserved registry gas quota has type %T", values[1])
	}
	active, ok := values[2].(bool)
	if !ok {
		return ReservedClient{}, fmt.Errorf("reserved registry active flag has type %T", values[2])
	}
	metadata, ok := values[3].(string)
	if !ok {
		return ReservedClient{}, fmt.Errorf("reserved registry metadata has type %T", values[3])
	}

	addresses, err := gc.ClientAddresses(state, number, hash, clientID)
	if err != nil {
		return ReservedClient{}, err
	}

	return ReservedClient{
		ClientID:  new(big.Int).Set(clientID),
		Admin:     admin,
		GasQuota:  gasQuota,
		Active:    active,
		Metadata:  metadata,
		Addresses: addresses,
	}, nil
}

func (gc *GenesisContractsClient) ClientAddresses(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
	clientID *big.Int,
) ([]common.Address, error) {
	if !gc.HasReservedRegistry() {
		return nil, errReservedRegistryNotConfigured
	}
	if clientID == nil {
		return nil, errors.New("reserved registry client id is nil")
	}

	values, err := gc.callReservedRegistry(state, number, hash, "getClientAddresses", clientID)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("reserved registry getClientAddresses returned %d values", len(values))
	}

	addresses, ok := values[0].([]common.Address)
	if !ok {
		return nil, fmt.Errorf("reserved registry addresses have type %T", values[0])
	}

	return addresses, nil
}

func (gc *GenesisContractsClient) WhitelistedAddresses(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
) ([]common.Address, error) {
	if !gc.HasReservedRegistry() {
		return nil, nil
	}

	values, err := gc.callReservedRegistry(state, number, hash, "getWhitelistedAddresses")
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("reserved registry getWhitelistedAddresses returned %d values", len(values))
	}

	addresses, ok := values[0].([]common.Address)
	if !ok {
		return nil, fmt.Errorf("reserved registry whitelist has type %T", values[0])
	}

	return addresses, nil
}

func (gc *GenesisContractsClient) ReservedClients(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
) ([]ReservedClientLookup, error) {
	if !gc.HasReservedRegistry() {
		return nil, nil
	}

	values, err := gc.callReservedRegistry(state, number, hash, "getReservedClients")
	if err != nil {
		return nil, err
	}
	if len(values) != 3 {
		return nil, fmt.Errorf("reserved registry getReservedClients returned %d values", len(values))
	}

	clientIDs, ok := values[0].([]*big.Int)
	if !ok {
		return nil, fmt.Errorf("reserved registry client ids have type %T", values[0])
	}
	admins, ok := values[1].([]common.Address)
	if !ok {
		return nil, fmt.Errorf("reserved registry admins have type %T", values[1])
	}
	gasQuotas, ok := values[2].([]uint64)
	if !ok {
		return nil, fmt.Errorf("reserved registry quotas have type %T", values[2])
	}
	if len(clientIDs) != len(admins) || len(clientIDs) != len(gasQuotas) {
		return nil, fmt.Errorf("reserved registry returned mismatched client slices: ids=%d admins=%d quotas=%d", len(clientIDs), len(admins), len(gasQuotas))
	}

	clients := make([]ReservedClientLookup, len(clientIDs))
	for i := range clientIDs {
		clients[i] = ReservedClientLookup{
			ClientID: new(big.Int).Set(clientIDs[i]),
			GasQuota: gasQuotas[i],
			Admin:    admins[i],
			Active:   true,
		}
	}

	return clients, nil
}

func (gc *GenesisContractsClient) TotalReservedGas(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
) (uint64, error) {
	if !gc.HasReservedRegistry() {
		return 0, nil
	}

	values, err := gc.callReservedRegistry(state, number, hash, "totalReservedGas")
	if err != nil {
		return 0, err
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("reserved registry totalReservedGas returned %d values", len(values))
	}

	total, ok := values[0].(uint64)
	if !ok {
		return 0, fmt.Errorf("reserved registry totalReservedGas returned %T", values[0])
	}

	return total, nil
}

func (gc *GenesisContractsClient) callReservedRegistry(
	state *state.StateDB,
	number uint64,
	hash common.Hash,
	method string,
	args ...interface{},
) ([]interface{}, error) {
	if !gc.HasReservedRegistry() {
		return nil, errReservedRegistryNotConfigured
	}
	if gc.ethAPI == nil {
		return nil, errors.New("reserved registry eth api is nil")
	}

	data, err := gc.reservedRegistryABI.Pack(method, args...)
	if err != nil {
		return nil, err
	}

	msgData := (hexutil.Bytes)(data)
	toAddress := common.HexToAddress(gc.ReservedRegistryContract)
	blockNr := rpc.BlockNumber(number)
	result, err := gc.ethAPI.CallWithState(ethapi.WithBorInternalCall(context.Background()), ethapi.TransactionArgs{
		Gas:  &borabi.SystemTxGas,
		To:   &toAddress,
		Data: &msgData,
	}, &rpc.BlockNumberOrHash{BlockNumber: &blockNr, BlockHash: &hash}, state, nil, nil)
	if err != nil {
		return nil, err
	}

	return gc.reservedRegistryABI.Unpack(method, result)
}
