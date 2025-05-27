package wit

import (
	"crypto/rand"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/assert"
)

func setupPeer() *Peer {
	logger := log.New()
	var id enode.ID
	rand.Read(id[:])
	p2pPeer := p2p.NewPeer(id, "test-peer", nil)
	return NewPeer(1, p2pPeer, nil, logger)
}

var testHeader1 = &types.Header{
	Number: big.NewInt(10),
}

var testHeader2 = &types.Header{
	Number: big.NewInt(15),
}

var testHeader3 = &types.Header{
	Number: big.NewInt(16),
}

// Create context headers for each witness (these will be used for witness.Header().Hash())
var testContextHeader1 = &types.Header{
	Number: big.NewInt(11), // Context should be the block the witness is for
}

var testContextHeader2 = &types.Header{
	Number: big.NewInt(17), // Different from testContextHeader1
}

var testContextHeader3 = &types.Header{
	Number: big.NewInt(18), // Different from both above
}

func createWitness(context *types.Header, headers []*types.Header) *stateless.Witness {
	// Create a new witness with the context and set the headers
	w, _ := stateless.NewWitness(context, nil)
	w.Headers = headers
	return w
}

var testWitness1 = createWitness(testContextHeader1, []*types.Header{testHeader1})
var testWitness2 = createWitness(testContextHeader2, []*types.Header{testHeader2, testHeader3})
var testWitness3 = createWitness(testContextHeader3, []*types.Header{testHeader1, testHeader2, testHeader3})

func TestAddKnownWitness(t *testing.T) {
	peer := setupPeer()

	peer.AddKnownWitness(testWitness1.Header().Hash())
	assert.True(t, peer.KnownWitnessesContains(testWitness1), "Witness should be known by the peer")
	assert.Equal(t, 1, peer.knownWitnesses.Cardinality(), "Known witnesses count should be 1")

	peer.AddKnownWitness(testWitness2.Header().Hash())
	assert.True(t, peer.KnownWitnessesContains(testWitness2), "Witness should be known by the peer")
	assert.Equal(t, 2, peer.knownWitnesses.Cardinality(), "Known witnesses count should be 2")

	peer.AddKnownWitness(testWitness3.Header().Hash())
	assert.True(t, peer.KnownWitnessesContains(testWitness3), "Witness should be known by the peer")

	// The witnesses now have different context headers, so they have different hashes
	// even though testWitness1 and testWitness3 share some of the same headers in their Headers slice
	assert.Equal(t, 3, peer.knownWitnesses.Cardinality(), "Known witnesses count should be 3")
}

/*
func TestProcessResponse(t *testing.T) {
	peer := setupPeer()
	witness := testWitness1
	res := &response{
		Res: &Response{
			Data: witness,
		},
		fail: make(chan error),
	}

	// Send the response and wait for it to be processed
	go func() { peer.resDispatch <- res }()
	time.Sleep(100 * time.Millisecond) // Allow time for processing

	// verify that processResponse did its job
	assert.True(t, peer.KnownWitnessesContains(witness), "Witness should be tracked in knownWitnesses")
}
*/
