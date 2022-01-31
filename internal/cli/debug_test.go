package cli

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/internal/cli/server"
	"github.com/ethereum/go-ethereum/internal/cli/server/proto"
	"github.com/mitchellh/cli"
	"github.com/stretchr/testify/assert"
)

func TestCommand_DebugBlock(t *testing.T) {
	// Start a blockchain in developer mode and get trace of block
	config := server.DefaultConfig()

	// enable developer mode
	config.Developer.Enabled = true
	config.Developer.Period = 2 // block time

	// enable archive mode for getting traces of ancient blocks
	config.GcMode = "archive"

	srv, err := server.NewServer(config)
	assert.NoError(t, err)
	defer srv.Stop()

	// wait for 4 seconds to mine a 2 blocks
	time.Sleep(2 * time.Duration(config.Developer.Period) * time.Second)

	// initialize the debug block command
	ui := &cli.BasicUi{
		Reader:      os.Stdin,
		Writer:      os.Stdout,
		ErrorWriter: os.Stderr,
	}

	meta2 := &Meta2{
		UI: ui,
	}
	meta2.addr = "127.0.0.1:3131"

	command := &DebugBlockCommand{
		Meta2: meta2,
	}

	// initialize the bor client
	borClt, err := command.BorConn()
	if err != nil {
		t.Fatalf("unable to initialize bor client")
	}

	start := time.Now()

	// create a new debug environment with predefined output
	dEnv1 := &debugEnv{
		output: "debug_block_test",
		prefix: "trace-1-",
	}
	if err := dEnv1.init(); err != nil {
		t.Fatalf("unable to initialize debug environment")
	}

	// get trace of 1st block
	req1 := &proto.DebugBlockRequest{Number: 1}
	stream1, err1 := borClt.DebugBlock(context.Background(), req1)
	if err1 != nil {
		t.Fatalf("unable to perform block trace for block number: %d", req1.Number)
	}

	if err := dEnv1.writeFromStream("block.json", stream1); err != nil {
		t.Fatalf("unable to write block trace for block number: %d", req1.Number)
	}

	// check if the trace file is created at the destination
	p := path.Join(dEnv1.dst, "block.json")
	if file, err := os.Stat(p); err == nil {
		// check if the file has content
		if file.Size() <= 0 {
			t.Fatalf("Unable to gather block trace for block number: %d", req1.Number)
		}
	}

	t.Logf("Verified traces created at %s", p)
	t.Logf("Completed trace of block %d in %d", req1.Number, time.Since(start).Milliseconds())

	start = time.Now()
	latestBlock := srv.GetLatestBlockNumber().Int64()

	// create a new debug environment with predefined output
	dEnv2 := &debugEnv{
		output: "debug_block_test",
		prefix: "trace-2-",
	}
	if err := dEnv2.init(); err != nil {
		t.Fatalf("unable to initialize debug environment")
	}

	// get trace of latest block
	req2 := &proto.DebugBlockRequest{Number: -1}
	stream2, err2 := borClt.DebugBlock(context.Background(), req2)
	if err2 != nil {
		t.Fatalf("unable to perform block trace for latest block with number: %d", latestBlock)
	}

	if err := dEnv2.writeFromStream("block.json", stream2); err != nil {
		t.Fatalf("unable to write block trace for block number: %d", latestBlock)
	}

	// check if the trace file is created at the destination
	p = path.Join(dEnv2.dst, "block.json")
	if file, err := os.Stat(p); err == nil {
		// check if the file has content
		if file.Size() <= 0 {
			t.Fatalf("Unable to gather block trace for block number: %d", latestBlock)
		}
	}

	t.Logf("Verified traces created at %s", p)
	t.Logf("Completed trace of block %d in %d ms", latestBlock, time.Since(start).Milliseconds())

	// delete the traces created
	currDir, _ := os.Getwd()
	tempDir := path.Join(currDir, dEnv1.output)
	os.RemoveAll(tempDir)
	tempDir = path.Join(currDir, dEnv2.output)
	os.RemoveAll(tempDir)

}
