package server

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/stretchr/testify/assert"
)

var initialPort uint64 = 61000

// nextPort gives the next available port starting from 60000
func nextPort() uint64 {
	log.Info("Checking for new port", "current", initialPort)
	port := atomic.AddUint64(&initialPort, 1)
	addr := fmt.Sprintf("localhost:%d", port)
	if _, err := net.Listen("tcp", addr); err == nil {
		return port
	} else {
		return nextPort()
	}
}

func TestServer_DeveloperMode(t *testing.T) {
	// get the default config
	config := DefaultConfig()

	// enable developer mode
	config.Developer.Enabled = true
	config.Developer.Period = 2 // block time

	// grpc port
	port := strconv.Itoa(int(nextPort()))
	config.GRPC.Addr = ":" + port

	// datadir
	datadir, _ := ioutil.TempDir("/tmp", "bor-cli-test")
	config.DataDir = datadir
	defer os.RemoveAll(datadir)

	// start the server
	server, err := NewServer(config)
	assert.NoError(t, err)
	defer server.Stop()

	// record the initial block number
	blockNumber := server.backend.BlockChain().CurrentBlock().Header().Number.Int64()

	var i int64 = 0
	for i = 0; i < 3; i++ {
		// We expect the node to mine blocks every `config.Developer.Period` time period
		time.Sleep(time.Duration(config.Developer.Period) * time.Second)
		currBlock := server.backend.BlockChain().CurrentBlock().Header().Number.Int64()
		expected := blockNumber + i + 1
		if res := assert.Equal(t, currBlock, expected); res == false {
			break
		}
	}
}
