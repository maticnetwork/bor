package server

import (
	"os"

	"github.com/ethereum/go-ethereum/common/network"
)

func CreateMockServer(config *Config) (*Server, error) {
	if config == nil {
		config = DefaultConfig()
	}

	_, gRPCListener, err := network.FindAvailablePort()
	if err != nil {
		return nil, err
	}

	// datadir
	datadir, err := os.MkdirTemp("", "bor-cli-test")
	if err != nil {
		return nil, err
	}

	config.DataDir = datadir
	config.JsonRPC.Http.Port = 0

	// start the server
	return NewServer(config, WithGRPCListener(gRPCListener))
}

func CloseMockServer(server *Server) {
	// remove the contents of temp data dir
	os.RemoveAll(server.config.DataDir)

	// close the server
	server.Stop()
}
