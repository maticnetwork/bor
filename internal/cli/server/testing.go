package server

import (
	"io/ioutil"
	"os"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/internal/freeport"
	"github.com/stretchr/testify/assert"
)

func NewTestServer(t *testing.T) (*Server, func()) {

	// get the default config
	config := DefaultConfig()

	// enable developer mode
	config.Developer.Enabled = true
	config.Developer.Period = 2 // block time

	// run in archive mode
	config.GcMode = "archive"

	// grpc port
	port := strconv.Itoa(int(freeport.NextPort()))
	config.GRPC.Addr = ":" + port

	// datadir
	datadir, _ := ioutil.TempDir("/tmp", "bor-cli-test")
	config.DataDir = datadir

	// start the server
	server, err := NewServer(config)
	assert.NoError(t, err)

	closeFn := func() {
		server.Stop()
		os.RemoveAll(datadir)
	}
	return server, closeFn
}
