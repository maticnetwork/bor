package bor

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
)

func TestLoadJson(t *testing.T) {
	stateSyncEventsPayload(t)
}

func stateSyncEventsPayload(t *testing.T) []*clerk.EventRecordWithTime {
	t.Helper()

	stateData, err := os.ReadFile("./testdata/states.json")
	if err != nil {
		t.Fatalf("%s", err)
	}

	res := make([]*clerk.EventRecordWithTime, 0)
	if err := json.Unmarshal(stateData, &res); err != nil {
		t.Fatalf("%s", err)
	}

	return res
}
