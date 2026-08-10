// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"compress/gzip"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	ethproto "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// AdminAPI is the collection of Ethereum full node related APIs for node
// administration.
type AdminAPI struct {
	eth *Ethereum
}

// NewAdminAPI creates a new instance of AdminAPI.
func NewAdminAPI(eth *Ethereum) *AdminAPI {
	return &AdminAPI{eth: eth}
}

// ExportChain exports the current blockchain into a local file,
// or a range of blocks if first and last are non-nil.
func (api *AdminAPI) ExportChain(file string, first *uint64, last *uint64) (bool, error) {
	if first == nil && last != nil {
		return false, errors.New("last cannot be specified without first")
	}
	if first != nil && last == nil {
		head := api.eth.BlockChain().CurrentHeader().Number.Uint64()
		last = &head
	}
	if _, err := os.Stat(file); err == nil {
		// File already exists. Allowing overwrite could be a DoS vector,
		// since the 'file' may point to arbitrary paths on the drive.
		return false, errors.New("location would overwrite an existing file")
	}
	// Make sure we can create the file to export into
	out, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return false, err
	}
	defer out.Close()

	var writer io.Writer = out
	if strings.HasSuffix(file, ".gz") {
		writer = gzip.NewWriter(writer)
		defer writer.(*gzip.Writer).Close()
	}

	// Export the blockchain
	if first != nil {
		if err := api.eth.BlockChain().ExportN(writer, *first, *last); err != nil {
			return false, err
		}
	} else if err := api.eth.BlockChain().Export(writer); err != nil {
		return false, err
	}
	return true, nil
}

func hasAllBlocks(chain *core.BlockChain, bs []*types.Block) bool {
	for _, b := range bs {
		if !chain.HasBlock(b.Hash(), b.NumberU64()) {
			return false
		}
	}

	return true
}

// ImportChain imports a blockchain from a local file.
func (api *AdminAPI) ImportChain(file string) (bool, error) {
	// Make sure the can access the file to import
	in, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer in.Close()

	var reader io.Reader = in
	if strings.HasSuffix(file, ".gz") {
		if reader, err = gzip.NewReader(reader); err != nil {
			return false, err
		}
	}

	// Run actual the import in pre-configured batches
	stream := rlp.NewStream(reader, 0)

	blocks, index := make([]*types.Block, 0, 2500), 0
	for batch := 0; ; batch++ {
		// Load a batch of blocks from the input file
		for len(blocks) < cap(blocks) {
			block := new(types.Block)
			if err := stream.Decode(block); err == io.EOF {
				break
			} else if err != nil {
				return false, fmt.Errorf("block %d: failed to parse: %v", index, err)
			}
			// ignore the genesis block when importing blocks
			if block.NumberU64() == 0 {
				continue
			}
			blocks = append(blocks, block)
			index++
		}
		if len(blocks) == 0 {
			break
		}

		if hasAllBlocks(api.eth.BlockChain(), blocks) {
			blocks = blocks[:0]
			continue
		}
		// Import the batch and reset the buffer
		if _, err := api.eth.BlockChain().InsertChain(blocks, false); err != nil {
			return false, fmt.Errorf("batch %d: failed to insert: %v", batch, err)
		}
		blocks = blocks[:0]
	}
	return true, nil
}

// TrafficTriggerResult reports the peer fanout and object hash for an injected
// network trigger emitted through the admin namespace.
type TrafficTriggerResult struct {
	PeerCount   int         `json:"peerCount"`
	TxHash      common.Hash `json:"txHash,omitempty"`
	BlockHash   common.Hash `json:"blockHash,omitempty"`
	BlockNumber uint64      `json:"blockNumber,omitempty"`
}

type txGossipPeer interface {
	ID() string
	SendTransactions(types.Transactions) error
}

type blockAnnouncementPeer interface {
	ID() string
	SendNewBlockHashes([]common.Hash, []uint64) error
}

type txFetchPeer interface {
	ID() string
	RequestTxs([]common.Hash) error
}

type blockBodyFetchPeer interface {
	ID() string
	RequestBodies([]common.Hash, chan *ethproto.Response) (*ethproto.Request, error)
}

// TriggerTxGossip injects a synthetic transaction directly onto connected eth
// peers. This is intended for operator testing of peer propagation paths.
func (api *AdminAPI) TriggerTxGossip(peerID *string) (*TrafficTriggerResult, error) {
	peers, err := api.targetEthPeers(peerID)
	if err != nil {
		return nil, err
	}
	tx, err := api.syntheticTransaction()
	if err != nil {
		return nil, err
	}
	if err := triggerTxGossipToPeers(asTxGossipPeers(peers), tx); err != nil {
		return nil, err
	}
	return &TrafficTriggerResult{
		PeerCount: len(peers),
		TxHash:    tx.Hash(),
	}, nil
}

// TriggerBlockAnnouncement injects a new-block-hashes announcement for the
// current local head onto connected eth peers. This is intended for operator
// testing of peer announcement paths.
func (api *AdminAPI) TriggerBlockAnnouncement(peerID *string) (*TrafficTriggerResult, error) {
	peers, err := api.targetEthPeers(peerID)
	if err != nil {
		return nil, err
	}
	head := api.eth.BlockChain().CurrentBlock()
	if head == nil {
		return nil, errors.New("current head unavailable")
	}
	hash := head.Hash()
	number := head.Number.Uint64()
	if err := triggerBlockAnnouncementToPeers(asBlockAnnouncementPeers(peers), hash, number); err != nil {
		return nil, err
	}
	return &TrafficTriggerResult{
		PeerCount:   len(peers),
		BlockHash:   hash,
		BlockNumber: number,
	}, nil
}

// TriggerTxFetch injects a pooled-transaction retrieval request onto connected
// peers. This is intended for operator testing of fetcher-style bulk traffic.
func (api *AdminAPI) TriggerTxFetch(peerID *string) (*TrafficTriggerResult, error) {
	peers, err := api.targetEthPeers(peerID)
	if err != nil {
		return nil, err
	}
	hashes := []common.Hash{randomTriggerHash(), randomTriggerHash()}
	if err := triggerTxFetchToPeers(asTxFetchPeers(peers), hashes); err != nil {
		return nil, err
	}
	return &TrafficTriggerResult{
		PeerCount: len(peers),
		TxHash:    hashes[0],
	}, nil
}

// TriggerBlockBodyFetch injects a block body retrieval request for the latest
// block hash advertised by connected peers. This is intended for operator
// testing of bulk retrieval request/response paths.
func (api *AdminAPI) TriggerBlockBodyFetch(peerID *string) (*TrafficTriggerResult, error) {
	peers, err := api.targetEthPeers(peerID)
	if err != nil {
		return nil, err
	}
	hashes := make([]common.Hash, 0, len(peers))
	for _, peer := range peers {
		hash, ok := api.bodyFetchHashForPeer(peer)
		if !ok {
			return nil, fmt.Errorf("peer %s has no advertised block hash", peer.ID())
		}
		hashes = append(hashes, hash)
	}
	if err := triggerBlockBodyFetchToPeers(asBlockBodyFetchPeers(peers), hashes); err != nil {
		return nil, err
	}
	return &TrafficTriggerResult{
		PeerCount: len(peers),
		BlockHash: hashes[0],
	}, nil
}

func (api *AdminAPI) targetEthPeers(peerID *string) ([]*ethPeer, error) {
	if api.eth == nil || api.eth.handler == nil || api.eth.handler.peers == nil {
		return nil, errors.New("eth protocol handler unavailable")
	}
	if peerID != nil {
		peer := api.eth.handler.peers.peer(*peerID)
		if peer == nil {
			return nil, fmt.Errorf("peer %s not found", *peerID)
		}
		return []*ethPeer{peer}, nil
	}
	peers := api.eth.handler.peers.all()
	if len(peers) == 0 {
		return nil, errors.New("no connected eth peers")
	}
	slices.SortFunc(peers, func(a, b *ethPeer) int {
		return strings.Compare(a.ID(), b.ID())
	})
	return peers, nil
}

func (api *AdminAPI) syntheticTransaction() (*types.Transaction, error) {
	head := api.eth.BlockChain().CurrentBlock()
	if head == nil {
		return nil, errors.New("current head unavailable")
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	config := api.eth.BlockChain().Config()
	to := common.Address{}
	nonce := uint64(time.Now().UnixNano())
	if head.BaseFee != nil {
		return types.SignNewTx(key, types.LatestSigner(config), &types.DynamicFeeTx{
			ChainID:   config.ChainID,
			Nonce:     nonce,
			To:        &to,
			Gas:       params.TxGas,
			GasTipCap: big.NewInt(1),
			GasFeeCap: new(big.Int).Add(head.BaseFee, big.NewInt(1)),
			Value:     big.NewInt(0),
			Data:      randomTriggerData(),
		})
	}
	return types.SignTx(
		types.NewTransaction(nonce, to, big.NewInt(0), params.TxGas, big.NewInt(1), randomTriggerData()),
		types.LatestSigner(config),
		key,
	)
}

func randomTriggerData() []byte {
	payload := make([]byte, 8)
	if _, err := rand.Read(payload); err != nil {
		big.NewInt(time.Now().UnixNano()).FillBytes(payload)
	}
	return payload
}

func randomTriggerHash() common.Hash {
	var hash common.Hash
	if _, err := rand.Read(hash[:]); err != nil {
		big.NewInt(time.Now().UnixNano()).FillBytes(hash[:])
	}
	return hash
}

func asTxGossipPeers(peers []*ethPeer) []txGossipPeer {
	out := make([]txGossipPeer, 0, len(peers))
	for _, peer := range peers {
		out = append(out, peer.Peer)
	}
	return out
}

func asBlockAnnouncementPeers(peers []*ethPeer) []blockAnnouncementPeer {
	out := make([]blockAnnouncementPeer, 0, len(peers))
	for _, peer := range peers {
		out = append(out, peer.Peer)
	}
	return out
}

func asTxFetchPeers(peers []*ethPeer) []txFetchPeer {
	out := make([]txFetchPeer, 0, len(peers))
	for _, peer := range peers {
		out = append(out, peer.Peer)
	}
	return out
}

func asBlockBodyFetchPeers(peers []*ethPeer) []blockBodyFetchPeer {
	out := make([]blockBodyFetchPeer, 0, len(peers))
	for _, peer := range peers {
		out = append(out, peer.Peer)
	}
	return out
}

func triggerTxGossipToPeers(peers []txGossipPeer, tx *types.Transaction) error {
	for _, peer := range peers {
		if err := peer.SendTransactions(types.Transactions{tx}); err != nil {
			return fmt.Errorf("send transactions to peer %s: %w", peer.ID(), err)
		}
	}
	return nil
}

func triggerBlockAnnouncementToPeers(peers []blockAnnouncementPeer, hash common.Hash, number uint64) error {
	for _, peer := range peers {
		if err := peer.SendNewBlockHashes([]common.Hash{hash}, []uint64{number}); err != nil {
			return fmt.Errorf("send block announcement to peer %s: %w", peer.ID(), err)
		}
	}
	return nil
}

func triggerTxFetchToPeers(peers []txFetchPeer, hashes []common.Hash) error {
	for _, peer := range peers {
		if err := peer.RequestTxs(hashes); err != nil {
			return fmt.Errorf("request transactions from peer %s: %w", peer.ID(), err)
		}
	}
	return nil
}

func triggerBlockBodyFetchToPeers(peers []blockBodyFetchPeer, hashes []common.Hash) error {
	for i, peer := range peers {
		req, err := peer.RequestBodies([]common.Hash{hashes[i]}, make(chan *ethproto.Response, 1))
		if err != nil {
			return fmt.Errorf("request bodies from peer %s: %w", peer.ID(), err)
		}
		if req != nil {
			_ = req.Close()
		}
	}
	return nil
}

func (api *AdminAPI) bodyFetchHashForPeer(peer *ethPeer) (common.Hash, bool) {
	if peer == nil {
		return common.Hash{}, false
	}
	if blockRange := peer.BlockRange(); blockRange != nil && blockRange.LatestBlockHash != (common.Hash{}) {
		return blockRange.LatestBlockHash, true
	}
	head, _ := peer.Head()
	if head != (common.Hash{}) {
		return head, true
	}
	currentHead := api.eth.BlockChain().CurrentBlock()
	if currentHead == nil {
		return common.Hash{}, false
	}
	return currentHead.Hash(), true
}
