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

func StartHeimdallMigrationMonitor(heimdallUrl string) {
	go heimdallMigrationMonitor(heimdallUrl)
}

func WaitFirstSuccessfulCheck() {
	// Wait for the first check to complete
	for range 6 {
		if firstSuccessfulCheckPassed {
			log.Error("First successful check passed", IsHeimdallV2)
			return
		}
		time.Sleep(10 * time.Second)
	}
}

func heimdallMigrationMonitor(heimdallUrl string) {
	log.Error("Starting heimdall migration monitor, heimdallUrl=%s", heimdallUrl)

	// Attempt to parse the URL so we can inspect/modify the host/port
	parsedURL, err := url.Parse(heimdallUrl)
	if err != nil {
		panic(fmt.Errorf("error parsing heimdallUrl: %w", err))
	}

	host, port, splitErr := net.SplitHostPort(parsedURL.Host)
	// If net.SplitHostPort succeeds, it means there's a port. We replace it.
	if splitErr == nil && port != "" {
		parsedURL.Host = net.JoinHostPort(host, "26657")
	}
	heimdallUrl = parsedURL.String()
	log.Error("Updated heimdallUrl after checking port: %s", heimdallUrl)

	isFirstCheck := true
	for {
		if !isFirstCheck {
			time.Sleep(10 * time.Second)
		}
		isFirstCheck = false

		resp, err := http.Get(fmt.Sprintf("%s/status", heimdallUrl))
		if err != nil {
			log.Error("Error fetching status: %v", err)
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
			log.Error("Error decoding response: %v", err)
			continue
		}

		version := statusResponse.Result.NodeInfo.Version
		parts := strings.Split(version, ".")
		if len(parts) < 2 {
			log.Error("Unexpected version format: %s", version)
			continue
		}

		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			log.Error("Error parsing minor version: %v", err)
			continue
		}

		log.Error("Heimdall version: %s, minor: %d", version, minor)
		// Flag to true if version is 0.38.x or above
		if minor >= 38 {
			IsHeimdallV2 = true
		} else {
			IsHeimdallV2 = false
		}

		firstSuccessfulCheckPassed = true
	}
}
