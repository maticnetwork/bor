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
)

const fetchStatesABI = `[{"constant":true,"inputs":[{"internalType":"uint256","name":"id","type":"uint256"},{"internalType":"address","name":"receiver","type":"address"},{"internalType":"string","name":"url","type":"string"}],"name":"fetchStates","outputs":[{"internalType":"string","name":"","type":"string"}],"payable":false,"stateMutability":"view","type":"function"}]`

var graphProtocolAddress = common.HexToAddress("0x0000000000000000000000000000000000000420")
var zeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")

func interceptForGraphProtocol(args []byte) (*core.ExecutionResult, error) {
	fABI, err := abi.JSON(strings.NewReader(fetchStatesABI))
	if err != nil {
		return nil, err
	}

	// decode txInput method signature
	method := fABI.Methods["fetchStates"]
	if !bytes.Equal(method.ID, args[:4]) {
		return nil, errors.New("eth_call not supported for other methods for this address")
	}

	fetchResult := make(map[string]interface{})

	// unpack method inputs
	if err := method.Inputs.UnpackIntoMap(fetchResult, args[4:]); err != nil {
		return nil, err
	}

	// fetch data
	url := fetchResult["url"].(string)
	lastID := fetchResult["id"].(*big.Int)
	receiver := fetchResult["receiver"].(common.Address)
	body, err := getRemoteData(lastID.Int64(), receiver, url)
	if err != nil {
		return nil, err
	}
	fmt.Println("fetchResult", string(body))

	// output
	output, err := method.Outputs.Pack(string(body))
	if err != nil {
		return nil, err
	}

	return &core.ExecutionResult{
		UsedGas:    0,
		Err:        nil,
		ReturnData: output,
	}, nil
}

func getRemoteData(id int64, receiver common.Address, url string) ([]byte, error) {
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
				id
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

	resp, err := netClient.Post(url, "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return ioutil.ReadAll(resp.Body)
}
