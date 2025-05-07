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

var IsHeimdallV2, firstSuccessfulCheckPassed bool

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
