package contract

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	borabi "github.com/ethereum/go-ethereum/consensus/bor/abi"
)

func TestReservedRegistryABIUnpacksBorFacingViews(t *testing.T) {
	registryABI := borabi.ReservedBlockspaceRegistry()

	clientOutput, err := registryABI.Methods["getClient"].Outputs.Pack(
		common.HexToAddress("0x1"),
		uint64(20_000_000),
		true,
		"Polymarket",
		big.NewInt(3),
	)
	if err != nil {
		t.Fatal(err)
	}
	clientValues, err := registryABI.Unpack("getClient", clientOutput)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := clientValues[1].(uint64); !ok {
		t.Fatalf("expected uint64 gas quota, got %T", clientValues[1])
	}

	clientsOutput, err := registryABI.Methods["getReservedClients"].Outputs.Pack(
		[]*big.Int{big.NewInt(1)},
		[]common.Address{common.HexToAddress("0x2")},
		[]uint64{20_000_000},
	)
	if err != nil {
		t.Fatal(err)
	}
	clientsValues, err := registryABI.Unpack("getReservedClients", clientsOutput)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := clientsValues[2].([]uint64); !ok {
		t.Fatalf("expected []uint64 gas quotas, got %T", clientsValues[2])
	}
}
