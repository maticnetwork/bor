package hmm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/ethereum/go-ethereum/log"
)

var IsHeimdallV2 bool

func StartHeimdallMigrationMonitor(heimdallUrl string) {
	go heimdallMigrationMonitor(heimdallUrl)
}

func heimdallMigrationMonitor(heimdallUrl string) {
	for {
		time.Sleep(10 * time.Second)

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

		// Set flag to true if version is 0.38.x or above
		if minor >= 38 {
			IsHeimdallV2 = true
		} else {
			IsHeimdallV2 = false
		}
	}
}
