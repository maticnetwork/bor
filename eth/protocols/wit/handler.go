package wit

import (
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/params"
)

// TODO(@pratikspatil024) - Once everything is working, check if need to set limits
// like we do in eth/handler.go

// Handler is a callback to invoke from an outside runner after the boilerplate
// exchanges have passed.
type Handler func(peer *Peer) error

// Backend defines the data retrieval methods to serve remote requests and the
// callback methods to invoke on remote deliveries.
type Backend interface {
	// Chain retrieves the blockchain object to serve data.
	Chain() *core.BlockChain

	// RunPeer is invoked when a peer joins on the `wit` protocol. The handler
	// should do any peer maintenance work, handshakes and validations. If all
	// is passed, control should be given back to the `handler` to process the
	// inbound messages going forward.
	RunPeer(peer *Peer, handler Handler) error

	// PeerInfo retrieves all known `wit` information about a peer.
	PeerInfo(id enode.ID) interface{}

	// Handle is a callback to be invoked when a data packet is received from
	// the remote peer. Only packets not consumed by the protocol handler will
	// be forwarded to the backend.
	Handle(peer *Peer, packet Packet) error
}

// MakeProtocols constructs the P2P protocol definitions for `wit`.
func MakeProtocols(backend Backend, network uint64) []p2p.Protocol {
	protocols := make([]p2p.Protocol, 0, len(ProtocolVersions))
	for _, version := range ProtocolVersions {
		version := version // Closure
		protocols = append(protocols, p2p.Protocol{
			Name:    ProtocolName,
			Version: version,
			Length:  protocolLengths[version],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				log.Debug("witness Run", "peer", p)
				peer := NewPeer(version, p, rw, log.New())
				defer peer.Close()
				return backend.RunPeer(peer, func(peer *Peer) error {
					return Handle(backend, peer)
				})
			},
			NodeInfo: func() interface{} {
				return nodeInfo(backend.Chain(), network)
			},
			PeerInfo: func(id enode.ID) interface{} {
				return backend.PeerInfo(id)
			},
			// TODO(@pratikspatil024) - check if we need DialCandidates (I think not)
			// TODO(@pratikspatil024) - check if we need this
			Attributes: []enr.Entry{currentENREntry(backend.Chain())},
		})

	}

	return protocols
}

// NodeInfo represents a short summary of the `wit` sub-protocol metadata
// known about the host peer.
// TODO(@pratikspatil024) - evaluate if we need all these fields
type NodeInfo struct {
	Network    uint64              `json:"network"`    // Ethereum network ID (1=Mainnet, Holesky=17000)
	Difficulty *big.Int            `json:"difficulty"` // Total difficulty of the host's blockchain
	Genesis    common.Hash         `json:"genesis"`    // SHA3 hash of the host's genesis block
	Config     *params.ChainConfig `json:"config"`     // Chain configuration for the fork rules
	Head       common.Hash         `json:"head"`       // Hex hash of the host's best owned block
}

// nodeInfo retrieves some `wit` protocol metadata about the running host node.
func nodeInfo(chain *core.BlockChain, network uint64) *NodeInfo {
	head := chain.CurrentBlock()
	hash := head.Hash()

	return &NodeInfo{
		Network:    network,
		Difficulty: chain.GetTd(hash, head.Number.Uint64()),
		Genesis:    chain.Genesis().Hash(),
		Config:     chain.Config(),
		Head:       hash,
	}
}

// Handle is invoked whenever an `wit` connection is made that successfully passes
// the protocol handshake. This method will keep processing messages until the
// connection is torn down.
func Handle(backend Backend, peer *Peer) error {
	for {
		if err := handleMessage(backend, peer); err != nil {
			peer.Log().Debug("Message handling failed in `wit`", "err", err)
			return err
		}
	}
}

type msgHandler func(backend Backend, msg Decoder, peer *Peer) error
type Decoder interface {
	Decode(val interface{}) error
	Time() time.Time
}

var wit0 = map[uint64]msgHandler{
	GetMsgWitness:       handleGetWitness,
	MsgWitness:          handleWitness,
	NewWitnessMsg:       handleNewWitness,
	NewWitnessHashesMsg: handleNewWitnessHashes,
}

// HandleMessage is invoked whenever an inbound message is received from a
// remote peer on the `wit` protocol. The remote connection is torn down upon
// returning any error.
func handleMessage(backend Backend, peer *Peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := peer.rw.ReadMsg()
	log.Debug("witness handleMessage", "msg", msg, "err", err)
	if err != nil {
		return err
	}

	if msg.Size > maxMessageSize {
		return fmt.Errorf("%w: %v > %v", errMsgTooLarge, msg.Size, maxMessageSize)
	}

	defer msg.Discard()

	start := time.Now()
	// Track the amount of time it takes to serve the request and run the handler
	if metrics.Enabled {
		h := fmt.Sprintf("%s/%s/%d/%#02x", p2p.HandleHistName, ProtocolName, peer.Version(), msg.Code)
		defer func(start time.Time) {
			sampler := func() metrics.Sample {
				return metrics.ResettingSample(
					metrics.NewExpDecaySample(1028, 0.015),
				)
			}
			metrics.GetOrRegisterHistogramLazy(h, nil, sampler).Update(time.Since(start).Microseconds())
		}(start)
	}

	if handler := wit0[msg.Code]; handler != nil {
		return handler(backend, msg, peer)
	}

	return fmt.Errorf("%w: %v", errInvalidMsgCode, msg.Code)
}
