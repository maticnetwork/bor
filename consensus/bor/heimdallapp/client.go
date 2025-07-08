package heimdallapp

import (
	"github.com/cosmos/cosmos-sdk/types"

	"github.com/ethereum/go-ethereum/log"

	"github.com/0xPolygon/heimdall-v2/app"
)

const (
	stateFetchLimit = 50
)

type HeimdallAppClient struct {
	hApp *app.HeimdallApp
}

func NewHeimdallAppClient() *HeimdallAppClient {
	return &HeimdallAppClient{
		// TODO HV2: Implement according to the new setup
		// hApp: service.GetHeimdallApp(),
	}
}

func (h *HeimdallAppClient) Close() {
	// Nothing to close as of now
	log.Warn("Shutdown detected, Closing Heimdall App conn")
}

func (h *HeimdallAppClient) NewContext() types.Context {
	return h.hApp.NewContext(true)
}
