package ethapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/maticnetwork/bor/accounts/abi"
	"github.com/maticnetwork/bor/common"
	"github.com/maticnetwork/bor/core"
	"github.com/maticnetwork/bor/core/types"
)

const fetchStateSyncsABI = `[{"inputs":[{"internalType":"uint256","name":"id","type":"uint256"},{"internalType":"address","name":"receiver","type":"address"}],"name":"fetchStateSyncs","outputs":[{"internalType":"bytes","name":"","type":"bytes"}],"stateMutability":"view","type":"function"}]'`

var stateFetcherAddress = common.HexToAddress("0x0000000000000000000000000000000000000420")
var zeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")

func interceptForStateSyncFetcher(header *types.Header, args []byte) (*core.ExecutionResult, error) {
	fABI, err := abi.JSON(strings.NewReader(fetchStateSyncsABI))
	if err != nil {
		return nil, err
	}

	// fetch method
	method, err := fABI.MethodById(args[:4])
	if err != nil {
		return nil, err
	}

	// handle fetch state syncs
	if bytes.Equal(method.ID, fABI.Methods["fetchStateSyncs"].ID) {
		fetcherInput := make(map[string]interface{})

		// unpack method inputs
		if err := method.Inputs.UnpackIntoMap(fetcherInput, args[4:]); err != nil {
			return nil, err
		}

		// fetch data
		lastID := fetcherInput["id"].(*big.Int)
		receiver := fetcherInput["receiver"].(common.Address)
		body, err := fetchStateSyncs(header, lastID, receiver)
		if err != nil {
			return nil, err
		}

		// output
		output, err := method.Outputs.Pack(body)
		if err != nil {
			return nil, err
		}

		return &core.ExecutionResult{
			UsedGas:    0,
			Err:        nil,
			ReturnData: output,
		}, nil
	}

	return nil, errors.New("eth_call is not supported for other methods for this address")
}

func fetchStateSyncs(header *types.Header, id *big.Int, receiver common.Address) ([]byte, error) {
	var netClient = &http.Client{
		Timeout: time.Second * 10,
	}

	// conditions
	conditions := []string{
		fmt.Sprintf("stateId_gt: %v", id),
	}

	// if receiver is not null or zero address, filter by receiver
	if !bytes.Equal(receiver.Bytes(), zeroAddress.Bytes()) {
		conditions = append(conditions, fmt.Sprintf(`contract: "%v"`, receiver.Hex()))
	}

	requestBody, err := json.Marshal(map[string]string{
		"query": fmt.Sprintf(`
		{
			stateSyncs(first: 10, orderBy: stateId, orderDirection: asc, where: { %v } ) {
				stateId
				contract
				data
			}
		}				
		`, strings.Join(conditions, ",")),
	})
	if err != nil {
		return nil, err
	}

	resp, err := netClient.Post("https://api.thegraph.com/subgraphs/name/maticnetwork/mainnet-root-subgraphs", "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return ioutil.ReadAll(resp.Body)
}
