package hmm

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	log "github.com/ethereum/go-ethereum/log"
)

var IsHeimdallV2, firstSuccessfulCheckPassed bool

func StartHeimdallMigrationMonitor(heimdallAPIUrl string, db ethdb.Database) {
	parsedURL, err := url.Parse(heimdallAPIUrl)
	if err != nil {
		panic(fmt.Errorf("error parsing heimdallUrl: %w", err))
	}

	// TODO: Do we need special configuration for this port?
	host, _, splitErr := net.SplitHostPort(parsedURL.Host)
	if splitErr != nil {
		panic(fmt.Errorf("error splitting host and port: %w", splitErr))
	}

	parsedURL.Host = net.JoinHostPort(host, "26657")

	heimdallRPCUrl := parsedURL.String()

	IsHeimdallV2 = getIsHeimdallV2Flag(db)

	go heimdallMigrationMonitor(heimdallRPCUrl, db)
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

		resp, err := http.Get(fmt.Sprintf("%s/status", heimdallUrl))
		if err != nil {
			log.Error("Error fetching status", "err", err)
			continue
		}

		var statusResponse struct {
			Result struct {
				NodeInfo struct {
					Version string `json:"version"`
				} `json:"node_info"`
			} `json:"result"`
		}

		err = json.NewDecoder(resp.Body).Decode(&statusResponse)
		resp.Body.Close()
		if err != nil {
			log.Error("Error decoding response", "err", err)
			continue
		}

		version := statusResponse.Result.NodeInfo.Version
		parts := strings.Split(version, ".")
		if len(parts) < 2 {
			log.Error("Unexpected version format", "version", version)
			continue
		}

		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			log.Error("Error parsing minor version", "err", err)
			continue
		}

		// TODO: We are not interested just in the version but if also the network is building blocks
		// Set flag to true if version is 0.38.x or above
		if minor >= 38 {
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
