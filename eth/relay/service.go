package relay

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

const preconfSubmitBatchSize = 64

var (
	errRpcClientUnavailable      = errors.New("rpc client unavailable to submit transactions")
	errQueueOverflow             = errors.New("relay task queue overflow")
	errPreconfValidationFailed   = errors.New("failed to validate transaction inclusion status for issuing preconf")
	errPrivateTxSubmissionFailed = errors.New("private tx submission failed partially, background retry scheduled")
)

var (
	// Latency timers when interacting with BPs via multiclient
	preconfSubmitTimer   = metrics.NewRegisteredTimer("relay/preconf/submit/latency", nil)
	checkTxStatusTimer   = metrics.NewRegisteredTimer("relay/preconf/checkstatus/latency", nil)
	privateTxSubmitTimer = metrics.NewRegisteredTimer("relay/privatetx/submit/latency", nil)

	uniquePreconfsTaskMeter     = metrics.NewRegisteredMeter("relay/preconf/tasks", nil)
	validPreconfsMeter          = metrics.NewRegisteredMeter("relay/preconf/result/valid", nil)
	invalidPreconfsMeter        = metrics.NewRegisteredMeter("relay/preconf/result/invalid", nil)
	invalidToValidPreconfsMeter = metrics.NewRegisteredMeter("relay/preconf/result/recovered", nil)
	txInDbMeter                 = metrics.NewRegisteredMeter("relay/preconf/checkstatus/dbhit", nil)

	uniquePrivateTxRequestMeter     = metrics.NewRegisteredMeter("relay/privatetx/request", nil)
	privateTxSubmissionSuccessMeter = metrics.NewRegisteredMeter("relay/privatetx/success", nil)
	privateTxSubmissionFailureMeter = metrics.NewRegisteredMeter("relay/privatetx/failure", nil)
)

// TxGetter defines a function that retrieves a transaction by its hash from local database.
// Returns: found (bool), transaction, blockHash, blockNumber, txIndex
type TxGetter func(hash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64)

type ServiceConfig struct {
	expiryTickerInterval time.Duration
	expiryInterval       time.Duration
	maxQueuedTasks       int
	maxConcurrentTasks   int
}

var DefaultServiceConfig = ServiceConfig{
	expiryTickerInterval: time.Minute,
	expiryInterval:       10 * time.Minute,
	maxQueuedTasks:       40_000,
	maxConcurrentTasks:   1024,
}

// TxTask represents a transaction submission task
type TxTask struct {
	rawtx      []byte
	hash       common.Hash
	insertedAt time.Time
	result     chan error
	skipWait   bool

	preconfirmed bool // whether block producer preconfirmed the tx or not
	err          error
}

type Service struct {
	config      *ServiceConfig
	multiclient *multiClient
	store       map[common.Hash]TxTask
	storeMu     sync.RWMutex
	wg          sync.WaitGroup // tracks all active goroutines for clean shutdown
	taskCh      chan TxTask    // channel to queue new tasks
	semaphore   chan struct{}
	closeCh     chan struct{} // to limit concurrent tasks

	txGetter TxGetter // function to get transaction from local database
}

func NewService(urls []string, config *ServiceConfig) *Service {
	if config == nil {
		defaultConfig := DefaultServiceConfig
		config = &defaultConfig
	}
	s := &Service{
		config:      config,
		multiclient: newMultiClientWithLimit(urls, config.maxConcurrentTasks),
		store:       make(map[common.Hash]TxTask),
		taskCh:      make(chan TxTask, config.maxQueuedTasks),
		semaphore:   make(chan struct{}, config.maxConcurrentTasks),
		closeCh:     make(chan struct{}),
	}
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.processPreconfTasks() }()
	go func() { defer s.wg.Done(); s.cleanup() }()
	return s
}

// SetTxGetter sets the transaction getter function for querying local database
func (s *Service) SetTxGetter(getter TxGetter) {
	s.txGetter = getter
}

// SubmitTransactionForPreconf attempts to queue a transaction submission task for preconf
// and returns true if the task is successfully queued. It fails if either the rpc clients
// are unavailable or if the task queue is full.
func (s *Service) SubmitTransactionForPreconf(tx *types.Transaction) error {
	return s.submitTransactionForPreconf(tx, nil, false)
}

func (s *Service) SubmitTransactionForPreconfSync(ctx context.Context, tx *types.Transaction) error {
	result := make(chan error, 1)
	if err := s.submitTransactionForPreconf(tx, result, true); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) submitTransactionForPreconf(tx *types.Transaction, result chan error, skipWait bool) error {
	if s.multiclient == nil {
		log.Warn("[tx-relay] No rpc client available to submit transactions")
		return errRpcClientUnavailable
	}

	rawTx, err := tx.MarshalBinary()
	if err != nil {
		log.Warn("[tx-relay] Failed to marshal transaction", "hash", tx.Hash(), "err", err)
		return err
	}

	// First check if service is closed/closing
	select {
	case <-s.closeCh:
		log.Info("[tx-relay] Dropping task, service closing", "hash", tx.Hash())
		return errRpcClientUnavailable
	default:
	}

	// Queue for processing (non-blocking until queue is full)
	select {
	case s.taskCh <- TxTask{rawtx: rawTx, hash: tx.Hash(), result: result, skipWait: skipWait}:
		return nil
	default:
		log.Info("[tx-relay] Task queue full, dropping transaction", "hash", tx.Hash())
		return errQueueOverflow
	}
}

// processPreconfTasks continuously picks new tasks from the queue and
// processes them. It rate limits the number of parallel tasks.
func (s *Service) processPreconfTasks() {
	for {
		select {
		case task := <-s.taskCh:
			if task.skipWait {
				tasks := []TxTask{task}
				for len(tasks) < preconfSubmitBatchSize {
					select {
					case next := <-s.taskCh:
						if !next.skipWait {
							s.startPreconfTasks(tasks)
							s.startPreconfTask(next)
							tasks = nil
							break
						}
						tasks = append(tasks, next)
					default:
						s.startPreconfTasks(tasks)
						tasks = nil
					}
					if tasks == nil {
						break
					}
				}
				if tasks != nil {
					s.startPreconfTasks(tasks)
				}
				continue
			}
			s.startPreconfTask(task)
		case <-s.closeCh:
			return
		}
	}
}

func (s *Service) startPreconfTask(task TxTask) {
	if !s.acquirePreconfSlots(1) {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.releasePreconfSlots(1)
		s.processPreconfTask(task)
	}()
}

func (s *Service) startPreconfTasks(tasks []TxTask) {
	if !s.acquirePreconfSlots(len(tasks)) {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.releasePreconfSlots(len(tasks))
		s.processPreconfTaskBatch(tasks)
	}()
}

func (s *Service) acquirePreconfSlots(count int) bool {
	acquired := 0
	for acquired < count {
		select {
		case s.semaphore <- struct{}{}:
			acquired++
		case <-s.closeCh:
			s.releasePreconfSlots(acquired)
			return false
		}
	}
	return true
}

func (s *Service) releasePreconfSlots(count int) {
	for range count {
		<-s.semaphore
	}
}

func (s *Service) processPreconfTaskBatch(tasks []TxTask) {
	rawTxs := make([][]byte, len(tasks))
	for i := range tasks {
		uniquePreconfsTaskMeter.Mark(1)
		rawTxs[i] = tasks[i].rawtx
	}
	start := time.Now()
	results := s.multiclient.submitPreconfTxBatchWithoutPendingWait(rawTxs)
	elapsed := time.Since(start)
	for i := range tasks {
		preconfSubmitTimer.Update(elapsed)
		s.finishPreconfTask(tasks[i], results[i].preconfirmed, results[i].err, start)
	}
}

// processPreconfTask submits the preconf transaction from the task to the block
// producers via multiclient and updates the status in cache.
func (s *Service) processPreconfTask(task TxTask) {
	// Capture some metrics
	uniquePreconfsTaskMeter.Mark(1)
	start := time.Now()
	var res bool
	var err error
	if task.skipWait {
		res, err = s.multiclient.submitPreconfTxWithoutPendingWait(task.rawtx)
	} else {
		res, err = s.multiclient.submitPreconfTx(task.rawtx)
	}
	submissionErr := err
	preconfSubmitTimer.UpdateSince(start)
	s.finishPreconfTask(task, res, submissionErr, start)
}

func (s *Service) finishPreconfTask(task TxTask, res bool, submissionErr error, start time.Time) {
	err := submissionErr
	// It's possible that the calls succeeded but preconf was not offered in which
	// case err would be nil. Update with a generic error as preconf wasn't offered.
	if !res && err == nil {
		err = errPreconfValidationFailed
	}
	if submissionErr != nil {
		log.Warn("[tx-relay] Error submitting preconf tx", "elapsed", time.Since(start), "err", submissionErr)
	}
	task.preconfirmed = res
	task.err = err
	// Note: We can purge the raw tx here to save memory. Keeping it
	// incase we have some changes in the retry logic.

	s.updateTaskInCache(task)
	if task.result != nil {
		task.result <- submissionErr
	}
}

// updateTaskInCache safely updates or inserts a task in cache by acting as a
// common gateway. A race condition can happen when the process task function
// and check preconf status function try to update the same task concurrently.
// It also ensures that a preconf status once marked is never reverted and
// latest error is preserved. Returns the latest preconf status and error.
func (s *Service) updateTaskInCache(newTask TxTask) (bool, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()

	existingTask, exists := s.store[newTask.hash]
	if !exists {
		// Task doesn't exist, create it and update the cache
		newTask.insertedAt = time.Now()
		if newTask.preconfirmed {
			validPreconfsMeter.Mark(1)
		} else {
			invalidPreconfsMeter.Mark(1)
		}
		s.store[newTask.hash] = newTask
		return newTask.preconfirmed, newTask.err
	}

	// If a task already exists and is preconfirmed, skip doing any updates. It
	// is possible that first write tries to set preconfirmation status but second
	// write contains an error thus making status false. We don't want to revert
	// the status in that case.
	if existingTask.preconfirmed {
		return existingTask.preconfirmed, existingTask.err
	}

	// Update the metric if status was false earlier and is being set to true now
	if newTask.preconfirmed {
		invalidToValidPreconfsMeter.Mark(1)
	}
	existingTask.preconfirmed = newTask.preconfirmed
	existingTask.err = newTask.err
	s.store[newTask.hash] = existingTask
	return existingTask.preconfirmed, existingTask.err
}

// CheckTxPreconfStatus checks whether a given transaction hash has been preconfirmed
// or not. It checks things in following order:
// - Checks the availability of preconf status of the task in cache
// - Checks locally if the transaction is already included in a block
// - Queries all block producers via multiclient to get the preconf status
func (s *Service) CheckTxPreconfStatus(hash common.Hash) (bool, error) {
	s.storeMu.RLock()
	task, exists := s.store[hash]
	s.storeMu.RUnlock()

	// If task exists in cache and is already preconfirmed, return immediately
	if exists && task.preconfirmed {
		return true, nil
	}

	// If task is not in cache or not preconfirmed, check locally if the tx
	// was included in a block or not.
	if s.txGetter != nil {
		found, tx, _, _, _ := s.txGetter(hash)
		if found && tx != nil {
			txInDbMeter.Mark(1)
			s.updateTaskInCache(TxTask{hash: hash, preconfirmed: true, err: nil})
			log.Debug("[tx-relay] Transaction found in local database", "hash", hash)
			return true, nil
		}
	}

	if s.multiclient == nil {
		return false, errRpcClientUnavailable
	}

	// If tx not found locally, query block producers for status
	start := time.Now()
	res, err := s.multiclient.checkTxStatus(hash)
	checkTxStatusTimer.UpdateSince(start)
	// It's possible that the calls succeeded but preconf was not offered in which
	// case err would be nil. Update with a generic error as preconf wasn't offered.
	if !res && err == nil {
		err = errPreconfValidationFailed
	}

	// Update the task in cache and return the latest status
	res, err = s.updateTaskInCache(TxTask{hash: hash, preconfirmed: res, err: err})
	if err != nil {
		log.Info("[tx-relay] Unable to validate tx status for preconf", "err", err)
	}
	return res, err
}

// SubmitPrivateTx attempts to submit a private transaction to all block producers
func (s *Service) SubmitPrivateTx(tx *types.Transaction, retry bool) error {
	if s.multiclient == nil {
		log.Warn("[tx-relay] No rpc client available to submit transactions")
		return errRpcClientUnavailable
	}

	rawTx, err := tx.MarshalBinary()
	if err != nil {
		log.Warn("[tx-relay] Failed to marshal transaction", "hash", tx.Hash(), "err", err)
		return err
	}

	uniquePrivateTxRequestMeter.Mark(1)
	start := time.Now()
	err, _ = s.multiclient.submitPrivateTx(rawTx, tx.Hash(), retry, s.txGetter)
	privateTxSubmitTimer.UpdateSince(start)
	if err != nil {
		privateTxSubmissionFailureMeter.Mark(1)
		log.Warn("[tx-relay] Error submitting private tx to at least one block producer", "hash", tx.Hash(), "elapsed", time.Since(start), "err", err)
		return errPrivateTxSubmissionFailed
	}

	privateTxSubmissionSuccessMeter.Mark(1)
	return nil
}

// cleanup is a periodic routine to delete old preconf results
func (s *Service) cleanup() {
	ticker := time.NewTicker(s.config.expiryTickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			count := 0
			s.storeMu.Lock()
			now := time.Now()
			for hash, task := range s.store {
				if now.Sub(task.insertedAt) > s.config.expiryInterval {
					delete(s.store, hash)
					count++
				}
			}
			s.storeMu.Unlock()
			if count > 0 {
				log.Info("[tx-relay] Purged expired tasks", "count", count)
			}
		case <-s.closeCh:
			return
		}
	}
}

func (s *Service) close() {
	close(s.closeCh)
	if s.multiclient != nil {
		s.multiclient.close()
	}
	// Wait for all go routines to finish for clean shutdown
	s.wg.Wait()
}
