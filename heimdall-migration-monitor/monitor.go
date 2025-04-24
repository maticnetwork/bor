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

	log "github.com/ethereum/go-ethereum/log"
)

var IsHeimdallV2, IsHFApproaching, firstSuccessfulCheckPassed bool

func StartHeimdallMigrationMonitor(heimdallAPIUrl string) {
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

	go heimdallMigrationMonitor(heimdallRPCUrl)
	go heimdallHaltHeightMonitor(heimdallAPIUrl)
}

func WaitFirstSuccessfulCheck() {
	// Wait for the first check to complete
	for range 6 {
		if firstSuccessfulCheckPassed {
			return
		}
		time.Sleep(10 * time.Second)
	}
}

func heimdallHaltHeightMonitor(heimdallUrl string) {
	for {
		time.Sleep(5 * time.Second)

		resp, err := http.Get(fmt.Sprintf("%s/chainmanager/halt-height", heimdallUrl))
		if err != nil {
			// TODO: Better to not log error because after heimdallv2 runs the endpoint exist
			continue
		}

		var haltHeightResponse struct {
			Height string `json:"height"`
			Result int    `json:"result"`
		}

		err = json.NewDecoder(resp.Body).Decode(&haltHeightResponse)
		resp.Body.Close()
		if err != nil {
			log.Error("Error decoding response", "err", err)
			continue
		}

		log.Error("halt height", "height", haltHeightResponse.Height, "result", haltHeightResponse.Result)

		currentHeight, err := strconv.ParseInt(haltHeightResponse.Height, 10, 32)
		if err != nil {
			log.Error("Error parsing halt height", "err", err)
			continue
		}

		if haltHeightResponse.Result > int(currentHeight) && haltHeightResponse.Result-int(currentHeight) < 100 {
			IsHFApproaching = true
		} else {
			IsHFApproaching = false
		}
	}
}

func heimdallMigrationMonitor(heimdallUrl string) {

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
			IsHeimdallV2 = true
		} else {
			IsHeimdallV2 = false
		}

		firstSuccessfulCheckPassed = true
	}
}
