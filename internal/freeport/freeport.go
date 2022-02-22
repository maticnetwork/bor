package freeport

import (
	"fmt"
	"net"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/log"
)

var initialPort uint64 = 61000

// nextPort gives the next available port starting from 60000
func NextPort() uint64 {
	log.Info("Checking for new port", "current", initialPort)
	port := atomic.AddUint64(&initialPort, 1)
	addr := fmt.Sprintf("localhost:%d", port)
	lis, err := net.Listen("tcp", addr)
	if err == nil {
		lis.Close()
		return port
	} else {
		return NextPort()
	}
}
