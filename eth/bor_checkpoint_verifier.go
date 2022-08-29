package eth

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	// errMissingBlocks is returned when we don't have the blocks locally, yet.
	errMissingBlocks = errors.New("missing checkpoint blocks")

	// errRootHash is returned when we aren't able to calculate the root hash
	// locally for a range of blocks.
	errRootHash = errors.New("failed to get local root hash")

	// errRootHashMismatch is returned when the local root hash doesn't match
	// with the root hash in checkpoint/milestone.
	errRootHashMismatch = errors.New("roothash mismatch")

	// errEndBlock is returned when we're unable to fetch a block locally.
	errEndBlock = errors.New("failed to get end block")
)

type borVerifier struct {
	verify func(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, rootHash string) (string, error)
}

func newBorVerifier(verifyFn func(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, rootHash string) (string, error)) *borVerifier {
	if verifyFn != nil {
		return &borVerifier{verifyFn}
	}

	verifyFn = func(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, rootHash string) (string, error) {
		var hash string

		// check if we have the given blocks
		head := handler.ethAPI.BlockNumber()
		if head < hexutil.Uint64(end) {
			log.Debug("Head block behind given block", "head", head, "end block", end)
			return hash, errMissingBlocks
		}

		// verify the root hash
		localRoothash, err := handler.ethAPI.GetRootHash(ctx, start, end)
		if err != nil {
			log.Debug("Failed to get root hash of given block range while whitelisting", "start", start, "end", end, "err", err)
			return hash, errRootHash
		}

		if localRoothash != rootHash {
			log.Warn("Root hash mismatch while whitelisting", "expected", localRoothash, "got", rootHash)
			ethHandler := (*ethHandler)(eth.handler)

			var rewindTo uint64
			var doExist bool
			if doExist, rewindTo, _ = ethHandler.downloader.GetWhitelistedMilestone(); doExist == true {

				log.Warn("Rewinding chain to :", "block number", rewindTo)

				handler.downloader.Cancel()
				err = eth.blockchain.SetHead(rewindTo)
				if err != nil {
					log.Error("Error while rewinding the chain to", "Block Number", rewindTo, "Error", err)
				}

			} else if doExist, rewindTo, _ = ethHandler.downloader.GetWhitelistedCheckpoint(); doExist == true {

				log.Warn("Rewinding chain to :", "block number", rewindTo)
				handler.downloader.Cancel()
				err = eth.blockchain.SetHead(rewindTo)
				if err != nil {
					log.Error("Error while rewinding the chain to", "Block Number", rewindTo, "Error", err)
				}

			} else {
				if start <= 0 {
					rewindTo = 0
				} else {
					rewindTo = start - 1
				}

				log.Warn("Rewinding chain to :", "block number", rewindTo)
				handler.downloader.Cancel()
				err = eth.blockchain.SetHead(rewindTo)
				if err != nil {
					log.Error("Error while rewinding the chain to", "Block Number", rewindTo, "Error", err)
				}

			}

			return hash, errRootHashMismatch
		}

		// fetch the end block hash
		block, err := handler.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(end), false)
		if err != nil {
			log.Debug("Failed to get end block hash while whitelisting", "err", err)
			return hash, errEndBlock
		}

		hash = fmt.Sprintf("%v", block["hash"])

		return hash, nil
	}

	return &borVerifier{verifyFn}
}
