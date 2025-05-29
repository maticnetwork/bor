package hmm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	log "github.com/ethereum/go-ethereum/log"
)

var IsHeimdallV2, firstSuccessfulCheckPassed bool

func StartHeimdallMigrationMonitor(heimdallAPIUrl string, db ethdb.Database) {
	IsHeimdallV2 = getIsHeimdallV2Flag(db)

	go heimdallMigrationMonitor(heimdallAPIUrl, db)
}

func WaitFirstSuccessfulCheck() {
	// Wait for the first check to complete
	for range 6 {
		if firstSuccessfulCheckPassed {
			return
		}
		fmt.Println("Trying to check Heimdall version...")
		time.Sleep(10 * time.Second)
	}
}

func heimdallMigrationMonitor(heimdallUrl string, db ethdb.Database) {

	isFirstCheck := true
	for {
		if !isFirstCheck {
			time.Sleep(10 * time.Second)
		}
		isFirstCheck = false

		resp, err := http.Get(fmt.Sprintf("%s/chainmanager/params", heimdallUrl))
		if err != nil {
			log.Error("Error fetching status", "err", err)
			continue
		}

		var paramsResponse struct {
			// v1
			Result struct {
				ChainParams struct {
					MaticTokenAddress string `json:"matic_token_address"`
				} `json:"chain_params"`
			} `json:"result"`

			// v2
			Params struct {
				ChainParams struct {
					PolTokenAddress string `json:"pol_token_address"`
				} `json:"chain_params"`
			} `json:"params"`
		}

		err = json.NewDecoder(resp.Body).Decode(&paramsResponse)
		resp.Body.Close()
		if err != nil {
			log.Error("Error decoding response", "err", err)
			continue
		}

		if paramsResponse.Result.ChainParams.MaticTokenAddress == "" && paramsResponse.Params.ChainParams.PolTokenAddress == "" {
			log.Error("Heimdall API did not return chain parameters")
			continue
		}

		if paramsResponse.Result.ChainParams.MaticTokenAddress != "" && paramsResponse.Params.ChainParams.PolTokenAddress != "" {
			log.Error("Heimdall API returned both v1 and v2 chain parameters, please check the API endpoint")
			continue
		}

		if paramsResponse.Params.ChainParams.PolTokenAddress != "" {
			if !IsHeimdallV2 {
				storeIsHeimdallV2Flag(db)
			}
			IsHeimdallV2 = true
		} else {
			if IsHeimdallV2 {
				storeIsHeimdallV2Flag(db)
			}
			IsHeimdallV2 = false
		}

		firstSuccessfulCheckPassed = true
	}
}

func storeIsHeimdallV2Flag(db ethdb.Database) {
	if err := db.Put(rawdb.IsHeimdallV2Key, []byte(strconv.FormatBool(IsHeimdallV2))); err != nil {
		log.Error("Error storing IsHeimdallV2 flag", "err", err)
	}
}

func getIsHeimdallV2Flag(db ethdb.Database) bool {
	value, err := db.Get(rawdb.IsHeimdallV2Key)
	if err != nil {
		log.Error("Error getting IsHeimdallV2 flag", "err", err)
		return false
	}

	if value == nil {
		return false
	}

	isHeimdallV2, err := strconv.ParseBool(string(value))
	if err != nil {
		log.Error("Error parsing IsHeimdallV2 flag", "err", err)
		return false
	}

	return isHeimdallV2
}
