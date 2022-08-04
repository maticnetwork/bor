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
	datadir, err := os.MkdirTemp("/tmp", "bor-cli-test")
	if err != nil {
		return nil, err
	}

	config.DataDir = datadir

	jsonRPCPort, jsonRPCListener, err := network.FindAvailablePort()
	if err != nil {
		return nil, err
	}

	config.JsonRPC.Http.Port = uint64(jsonRPCPort)

	// start the server
	return NewServer(config, WithGRPCListener(gRPCListener), WithJSONRPCListener(jsonRPCListener))
}

func CloseMockServer(server *Server) {
	// remove the contents of temp data dir
	os.RemoveAll(server.config.DataDir)

	// close the server
	server.Stop()
}
