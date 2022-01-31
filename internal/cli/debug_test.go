package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/internal/cli/server"
	"github.com/ethereum/go-ethereum/internal/cli/server/proto"
	"github.com/mitchellh/cli"
	grpc_net_conn "github.com/mitchellh/go-grpc-net-conn"
	"github.com/stretchr/testify/assert"

	"github.com/golang/protobuf/jsonpb"
	gproto "github.com/golang/protobuf/proto"
)

type BlockData struct {
	number int64 `json:"number"`
}
type Block struct {
	block BlockData `json:"block"`
}

func TestCommand_DebugBlock(t *testing.T) {
	// Start a blockchain in developer mode and get trace of block
	config := server.DefaultConfig()

	// enable developer mode
	config.Developer.Enabled = true
	config.Developer.Period = 5 // block time

	// enable archive mode for getting traces of ancient blocks
	config.GcMode = "archive"

	srv, err := server.NewServer(config)
	assert.NoError(t, err)
	defer srv.Stop()

	// wait for 10 seconds to mine a 2 blocks
	time.Sleep(2 * time.Duration(config.Developer.Period) * time.Second)

	start := time.Now()

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

	// create a new debug environment
	dEnv := &debugEnv{
		output: "debug_block_test",
		prefix: "bor-block-trace-test-",
	}
	if err := dEnv.init(); err != nil {
		t.Fatalf("unable to initialize debug environment")
	}

	// get trace of 1st block
	req1 := &proto.DebugBlockRequest{Number: 1}
	stream1, err1 := borClt.DebugBlock(context.Background(), req1)
	if err1 != nil {
		t.Log(err1)
		t.Fatalf("unable to perform block trace for block number: %d", req1.Number)
	}

	// decode the stream
	block1, err1 := decodeStream(stream1)
	if err1 != nil {
		t.Fatalf(err1.Error())
	}

	if matched := assert.Equal(t, block1.block.number, int64(1)); matched == false {
		t.Fatalf("trace failed: block number mismatch, expected: %d, got: %d", 1, block1.block.number)
	}

	end := time.Now()
	diff := end.Sub(start)
	period := time.Duration(config.Developer.Period) * time.Second
	t.Log("trace of block 1 took:", diff.Seconds(), "seconds")

	// update the latest block if the initial trace took
	// longer than `config.Developer.Period` seconds
	latestBlock := int64(2)
	if diff.Seconds() > period.Seconds() {
		latestBlock += int64(diff.Seconds() / period.Seconds())
	}

	// get trace of latest block
	req2 := &proto.DebugBlockRequest{Number: -1}
	stream2, err2 := borClt.DebugBlock(context.Background(), req2)
	if err2 != nil {
		t.Fatalf("unable to perform block trace for latest block with number: %d", latestBlock)
	}

	// decode the stream
	block2, err2 := decodeStream(stream2)
	if err2 != nil {
		t.Fatalf(err2.Error())
	}

	if matched := assert.Equal(t, block2.block.number, latestBlock); matched == false {
		t.Fatalf("trace failed: block number mismatch, expected: %d, got: %d", 1, block2.block.number)
	}
}

func decodeStream(stream proto.Bor_DebugBlockClient) (*Block, error) {
	msg, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("unable to decode trace stream")
	}
	if _, ok := msg.Event.(*proto.DebugFileResponse_Open_); !ok {
		return nil, fmt.Errorf("unable to decode trace stream")
	}

	decode := grpc_net_conn.SimpleDecoder(func(msg gproto.Message) *[]byte {
		return &msg.(*proto.DebugFileResponse).GetInput().Data
	})
	res, err := decode(msg, 0, []byte{})
	if err != nil {
		return nil, fmt.Errorf("unable to decode trace stream")
	}

	// res, err := marshal(msg)
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to decode trace stream")
	// }

	// decode := grpc_net_conn.SimpleDecoder(func(msg gproto.Message) *[]byte {
	// 	return &msg.(*proto.DebugFileResponse_Input).Data
	// })

	// res1, err1 := conn.Decode(msg, 0, []byte{})
	// res1, err1 := nil, nil
	// if err1 != nil {
	// 	return nil, fmt.Errorf("unable to decode trace stream")
	// }

	var block Block
	err = json.Unmarshal([]byte(res), &block)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall trace block")
	}
	return &block, nil
}

func marshal(message gproto.Message) (string, error) {
	marshaller := jsonpb.Marshaler{
		Indent: "",
	}
	return marshaller.MarshalToString(message)
}
