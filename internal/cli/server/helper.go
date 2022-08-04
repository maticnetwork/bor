package server

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"
)

// findAvailablePort returns the next available port
func findAvailablePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

func CreateMockServer(config *Config) (*Server, error) {
	if config == nil {
		config = DefaultConfig()
	}

	// find available port for grpc server
	rand.Seed(time.Now().UnixNano())

	port, err := findAvailablePort()
	if err != nil {
		return nil, err
	}

	// grpc port
	config.GRPC.Addr = fmt.Sprintf(":%d", port)

	// datadir
	datadir, _ := os.MkdirTemp("/tmp", "bor-cli-test")
	config.DataDir = datadir

	port, err = findAvailablePort()
	if err != nil {
		return nil, err
	}

	config.JsonRPC.Http.Port = uint64(port)

	// start the server
	return NewServer(config)
}

func CloseMockServer(server *Server) {
	// remove the contents of temp data dir
	os.RemoveAll(server.config.DataDir)

	// close the server
	server.Stop()
}
