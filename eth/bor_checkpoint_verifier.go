// nolint
package eth

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	// errMissingCurrentBlock is returned when we don't have the current block
	// present locally.
	errMissingCurrentBlock = errors.New("current block missing")

	// errChainOutOfSync is returned when we're trying to process a future
	// checkpoint/milestone and we haven't reached at that number yet.
	errChainOutOfSync = errors.New("chain out of sync")

	// errRootHash is returned when the root hash calculation for a range of blocks fails.
	errRootHash = errors.New("root hash calculation failed")

	// errHashMismatch is returned when the local hash doesn't match
	// with the hash of checkpoint/milestone. It is the root hash of blocks
	// in case of checkpoint and is end block hash in case of milestones.
	errHashMismatch = errors.New("hash mismatch")

	// errEndBlock is returned when we're unable to fetch a block locally.
	errEndBlock = errors.New("failed to get end block")

	// errEndBlock is returned when we're unable to fetch the tip confirmation block locally.
	errTipConfirmationBlock = errors.New("failed to get tip confirmation block")

	// rewindLengthMeter for collecting info about the length of chain rewinded
	rewindLengthMeter = metrics.NewRegisteredMeter("chain/autorewind/length", nil)
)

const maxRewindLen uint64 = 126

type borVerifier struct {
	verify func(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, hash string, isCheckpoint bool) (string, error)
}

func newBorVerifier() *borVerifier {
	return &borVerifier{borVerify}
}

func borVerify(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, hash string, isCheckpoint bool) (string, error) {
	str := "milestone"
	if isCheckpoint {
		str = "checkpoint"
	}

	log.Info("[Stateless][borVerify] Starting verification", "type", str, "start", start, "end", end, "hash", hash)

	// check if we have the given blocks
	currentBlock := eth.BlockChain().CurrentBlock()
	if currentBlock == nil {
		log.Info("[Stateless][borVerify] Failed to fetch current block from blockchain", "type", str)
		return hash, errMissingCurrentBlock
	}

	head := currentBlock.Number.Uint64()
	log.Info("[Stateless][borVerify] Current chain head", "head", head, "end", end, "type", str)

	if head < end {
		log.Info("[Stateless][borVerify] Current head block behind incoming block", "type", str, "head", head, "end", end)
		return hash, errChainOutOfSync
	}

	var localHash string

	// verify the hash
	if isCheckpoint {
		log.Info("[Stateless][borVerify] Calculating root hash for checkpoint verification", "start", start, "end", end)
		var err error

		// in case of checkpoint get the rootHash
		localHash, err = handler.ethAPI.GetRootHash(ctx, start, end)

		if err != nil {
			log.Info("[Stateless][borVerify] Failed to calculate root hash", "start", start, "end", end, "err", err)
			return hash, fmt.Errorf("%w: %v", errRootHash, err)
		}
		log.Info("[Stateless][borVerify] Root hash calculated successfully", "localHash", localHash)
	} else {
		log.Info("[Stateless][borVerify] Getting end block hash for milestone verification", "end", end)
		// in case of milestone(isCheckpoint==false) get the hash of endBlock
		block, err := handler.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(end), false)
		if err != nil || block == nil || block["hash"] == nil {
			log.Info("[Stateless][borVerify] Failed to get end block hash", "number", end, "err", err)
			return hash, fmt.Errorf("%w: %v", errEndBlock, err)
		}

		localHash = fmt.Sprintf("%v", block["hash"])[2:]
		log.Info("[Stateless][borVerify] End block hash retrieved successfully", "localHash", localHash)
	}

	//nolint
	if localHash != hash {
		log.Info("[Stateless][borVerify] Hash mismatch detected", "type", str, "expected", localHash, "got", hash)

		ethHandler := (*ethHandler)(eth.handler)

		var (
			rewindTo uint64
			doExist  bool
		)

		if doExist, rewindTo, _ = ethHandler.downloader.GetWhitelistedMilestone(); doExist {
			log.Info("[Stateless][borVerify] Found whitelisted milestone for rewind", "rewindTo", rewindTo)
		} else if doExist, rewindTo, _ = ethHandler.downloader.GetWhitelistedCheckpoint(); doExist {
			log.Info("[Stateless][borVerify] Found whitelisted checkpoint for rewind", "rewindTo", rewindTo)
		} else {
			if start <= 0 {
				rewindTo = 0
			} else {
				rewindTo = start - 1
			}
			log.Info("[Stateless][borVerify] No whitelisted blocks found, using calculated rewind", "rewindTo", rewindTo)
		}

		if head-rewindTo > maxRewindLen {
			rewindTo = head - maxRewindLen
			log.Info("[Stateless][borVerify] Adjusted rewind to maximum allowed length", "rewindTo", rewindTo, "maxRewindLen", maxRewindLen)
		}

		log.Info("[Stateless][borVerify] Rewinding chain due to hash mismatch", "type", str, "rewindTo", rewindTo)

		var canonicalChain []*types.Block

		length := end - rewindTo

		log.Info("[Stateless][borVerify] Fetching canonical chain blocks", "hash", hash, "length", length)
		canonicalChain = eth.BlockChain().GetBlocksFromHash(common.HexToHash(hash), int(length))

		// Reverse the canonical chain
		for i, j := 0, len(canonicalChain)-1; i < j; i, j = i+1, j-1 {
			canonicalChain[i], canonicalChain[j] = canonicalChain[j], canonicalChain[i]
		}

		log.Info("[Stateless][borVerify] Initiating chain rewind", "from", head, "to", rewindTo)
		rewindBack(eth, head, rewindTo)

		if canonicalChain != nil && len(canonicalChain) == int(length) {
			log.Info("[Stateless][borVerify] Inserting canonical chain", "from", canonicalChain[0].NumberU64(), "hash", canonicalChain[0].Hash(), "to", canonicalChain[len(canonicalChain)-1].NumberU64(), "hash", canonicalChain[len(canonicalChain)-1].Hash())
			_, err := eth.BlockChain().InsertChain(canonicalChain, eth.config.SyncAndProduceWitnesses)
			if err != nil {
				log.Info("[Stateless][borVerify] Failed to insert canonical chain", "err", err)
				return hash, err
			} else {
				log.Info("[Stateless][borVerify] Successfully inserted canonical chain")
			}
		} else {
			log.Info("[Stateless][borVerify] Failed to insert canonical chain due to length mismatch", "chainLength", len(canonicalChain), "expected", length)
			return hash, errHashMismatch
		}
	} else {
		log.Info("[Stateless][borVerify] Hash verification successful", "type", str, "hash", localHash)
	}

	// fetch the end block hash
	log.Info("[Stateless][borVerify] Fetching final end block hash", "end", end)
	block, err := handler.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(end), false)
	if err != nil {
		log.Info("[Stateless][borVerify] Failed to get end block hash", "end", end, "err", err)
		return hash, fmt.Errorf("%w: %v", errEndBlock, err)
	}

	hash = fmt.Sprintf("%v", block["hash"])
	log.Info("[Stateless][borVerify] Verification completed successfully", "type", str, "finalHash", hash)

	return hash, nil
}

// Stop the miner if the mining process is running and rewind back the chain
func rewindBack(eth *Ethereum, head uint64, rewindTo uint64) {
	if eth.Miner() != nil && eth.Miner().Mining() {
		ch := make(chan struct{})
		eth.Miner().Stop(ch)

		<-ch
		rewind(eth, head, rewindTo)

		eth.Miner().Start()
	} else {
		rewind(eth, head, rewindTo)
	}
}

func rewind(eth *Ethereum, head uint64, rewindTo uint64) {
	eth.handler.downloader.Cancel()
	err := eth.blockchain.SetHead(rewindTo)

	if err != nil {
		log.Error("Error while rewinding the chain", "to", rewindTo, "err", err)
	} else {
		rewindLengthMeter.Mark(int64(head - rewindTo))
	}
}
