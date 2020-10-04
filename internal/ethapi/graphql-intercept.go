package ethapi

import (
	"bytes"
	"encoding/hex"
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

const fetchStatesABI = `[{"constant":true,"inputs":[{"internalType":"uint256","name":"id","type":"uint256"},{"internalType":"string","name":"url","type":"string"}],"name":"fetchStates","outputs":[{"internalType":"string","name":"","type":"string"}],"payable":false,"stateMutability":"view","type":"function"}]`

var GraphProtocolAddress = common.HexToAddress("0x0000000000000000000000000000000000000420")

func interceptForGraphProtocol(args []byte) (*core.ExecutionResult, error) {
	fABI, err1 := abi.JSON(strings.NewReader(fetchStatesABI))
	if err1 != nil {
		return nil, err1
	}

	// decode txInput method signature
	decodedSig, err := hex.DecodeString("32dfeba3")
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(decodedSig, args[:4]) {
		return nil, errors.New("eth_call not supported for other methods for this address")
	}

	// recover Method from signature and ABI
	method, err := fABI.MethodById(decodedSig)
	if err != nil {
		return nil, err
	}

	fetchResult := make(map[string]interface{})
	// unpack method inputs
	err = method.Inputs.UnpackIntoMap(fetchResult, args[4:])
	if err != nil {
		return nil, err
	}

	// fetch data
	url := fetchResult["url"].(string)
	lastID := fetchResult["id"].(*big.Int)
	body, err := getRemoteData(lastID.Int64(), url)
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

func getRemoteData(id int64, url string) ([]byte, error) {
	var netClient = &http.Client{
		Timeout: time.Second * 10,
	}

	requestBody, err := json.Marshal(map[string]string{
		"query": fmt.Sprintf(`
		{
			stateSyncs(first: 10, orderBy: stateId, orderDirection: asc, where: { stateId_gt: %v } ) {
				id
				stateId
				contract
				data
			}
		}				
		`, id),
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
