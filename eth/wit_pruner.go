package eth

import (
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
)

type witPruner struct {
	ethAPI                *ethapi.BlockChainAPI
	database              ethdb.Database
	witnessPruneThreshold uint64        // Minimum necessary distance between local header and latest non pruned witness
	witnessPruneInterval  time.Duration // The time interval between each witness prune routine
	quitPrune             chan struct{}
}

func NewWitPruner(
	api *ethapi.BlockChainAPI,
	db ethdb.Database,
	threshold uint64,
	interval time.Duration,
) *witPruner {
	return &witPruner{
		ethAPI:                api,
		database:              db,
		witnessPruneThreshold: threshold,
		witnessPruneInterval:  interval,
		quitPrune:             make(chan struct{}),
	}
}

// pruneWitnessLoop starts a background goroutine that prunes old witnesses every h.witnessPruneInterval.
// Close h.quitPrune to stop it.
func (wp *witPruner) Start() {
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				wp.pruneWitness()
				timer.Reset(wp.witnessPruneInterval)
			case <-wp.quitPrune:
				log.Info("quit prune witness")
				return
			}
		}
	}()
}

func (wp *witPruner) pruneWitness() {
	cursor := rawdb.ReadWitnessPruneCursor(wp.database)
	latest := uint64(wp.ethAPI.BlockNumber())
	var cutoff uint64
	if latest > wp.witnessPruneThreshold {
		cutoff = latest - wp.witnessPruneThreshold
	}

	if cursor == nil {
		cursor = &cutoff
	}

	batch := wp.database.NewBatch()
	if *cursor < cutoff {
		allHashes := rawdb.ReadAllHashesInRange(wp.database, *cursor, cutoff-1)

		for _, hash := range allHashes {
			rawdb.DeleteWitness(batch, hash.Hash)
		}
		*cursor = cutoff
	}

	rawdb.WriteWitnessPruneCursor(batch, *cursor)

	if err := batch.Write(); err != nil {
		log.Error("error while pruning old witnesses", "writeErr", err)
	}
}
