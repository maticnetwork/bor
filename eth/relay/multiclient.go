package relay

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	rpcTimeout             = 2 * time.Second
	preconfBatchRPCTimeout = 10 * time.Second
	privateTxRetryInterval = 2 * time.Second
	privateTxMaxRetries    = 5
)

var (
	// Metrics for each request made to BP tracking success and failures
	preconfRpcSuccessMeter        = metrics.NewRegisteredMeter("relay/bp/rpc/preconf/success", nil)
	preconfRpcFailureMeter        = metrics.NewRegisteredMeter("relay/bp/rpc/preconf/failure", nil)
	preconfRpcAlreadyKnownMeter   = metrics.NewRegisteredMeter("relay/bp/rpc/preconf/alreadyknown", nil)
	privateTxRpcSuccessMeter      = metrics.NewRegisteredMeter("relay/bp/rpc/privatetx/success", nil)
	privateTxRpcFailureMeter      = metrics.NewRegisteredMeter("relay/bp/rpc/privatetx/failure", nil)
	privateTxRpcAlreadyKnownMeter = metrics.NewRegisteredMeter("relay/bp/rpc/privatetx/alreadyknown", nil)
	checkStatusRpcSuccessMeter    = metrics.NewRegisteredMeter("relay/bp/rpc/checkstatus/success", nil)
	checkStatusRpcFailureMeter    = metrics.NewRegisteredMeter("relay/bp/rpc/checkstatus/failure", nil)

	// Metric for preconf submissions where tx were accepted by all BPs but not preconfirmed
	belowThresholdPreconfMeter = metrics.NewRegisteredMeter("relay/preconf/result/belowthreshold", nil)

	// Metric for private tx submissions that were accepted by all BPs after retries.
	privateTxRetrySuccessMeter = metrics.NewRegisteredMeter("relay/privatetx/retry/success", nil)
)

// isAlreadyKnownError checks if the error indicates the transaction is already known to the node
func isAlreadyKnownError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "already known")
}

// multiClient holds multiple rpc client instances for each block producer
// to perform certain queries across all of them and make a unified decision.
type multiClient struct {
	clients       []*rpc.Client // rpc client instances dialed to each block producer
	transports    []*http.Transport
	closed        atomic.Bool
	retryInterval time.Duration // 0 means use privateTxRetryInterval; configurable for testing

	rejectionTracker rejectionTracker
	reporterDone     chan struct{} // closed to signal the reporter goroutine to exit
	closeOnce        sync.Once
}

func newMultiClient(urls []string) *multiClient {
	return newMultiClientWithLimit(urls, DefaultServiceConfig.maxConcurrentTasks)
}

func newMultiClientWithLimit(urls []string, maxConcurrent int) *multiClient {
	if len(urls) == 0 {
		log.Warn("[tx-relay] No block producer URLs provided")
		return nil
	}

	clients := make([]*rpc.Client, 0, len(urls))
	transports := make([]*http.Transport, 0, len(urls))
	failed := 0
	for i, url := range urls {
		// We use the rpc dialer for primarily 2 reasons:
		// 1. It supports automatic reconnection when connection is lost
		// 2. It allows us to do rpc queries which aren't directly available in ethclient (like txpool_contentFrom)
		client, transport, err := dialRelayEndpoint(url, maxConcurrent)
		if err != nil {
			failed++
			log.Warn("[tx-relay] Failed to dial rpc endpoint, skipping", "url", url, "index", i, "err", err)
			continue
		}

		// Test connection with a simple call
		var blockNumber string
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		err = client.CallContext(ctx, &blockNumber, "eth_blockNumber")
		cancel()
		if err != nil {
			client.Close()
			if transport != nil {
				transport.CloseIdleConnections()
			}
			failed++
			log.Warn("[tx-relay] Failed to fetch latest block number, skipping", "url", url, "index", i, "err", err)
			continue
		}

		number, err := hexutil.DecodeUint64(blockNumber)
		if err != nil {
			client.Close()
			if transport != nil {
				transport.CloseIdleConnections()
			}
			failed++
			log.Warn("[tx-relay] Failed to decode latest block number, skipping", "url", url, "index", i, "err", err)
			continue
		}

		log.Info("[tx-relay] Dial successful", "blockNumber", number, "index", i)
		clients = append(clients, client)
		transports = append(transports, transport)
	}

	if failed == len(urls) {
		log.Info("[tx-relay] Failed to dial all rpc endpoints, disabling completely", "count", len(urls))
		return nil
	}

	log.Info("[tx-relay] Initialised rpc client for each block producer", "success", len(clients), "failed", failed)
	mc := &multiClient{
		clients:      clients,
		transports:   transports,
		reporterDone: make(chan struct{}),
	}
	go mc.reportRejections()
	return mc
}

func dialRelayEndpoint(rawURL string, maxConcurrent int) (*rpc.Client, *http.Transport, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		client, err := rpc.Dial(rawURL)
		return client, nil, err
	}

	maxConnections := max(1, maxConcurrent)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = maxConnections
	transport.MaxIdleConnsPerHost = maxConnections
	transport.MaxConnsPerHost = maxConnections
	client, err := rpc.DialOptions(context.Background(), rawURL, rpc.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, err
	}
	return client, transport, nil
}

type SendTxForPreconfResponse struct {
	TxHash       common.Hash `json:"hash"`
	Preconfirmed bool        `json:"preconfirmed"`
}

type preconfSubmissionResult struct {
	preconfirmed bool
	err          error
}

func (mc *multiClient) submitPreconfTx(rawTx []byte) (bool, error) {
	return mc.submitPreconfTxWithPendingWait(rawTx, true)
}

func (mc *multiClient) submitPreconfTxWithoutPendingWait(rawTx []byte) (bool, error) {
	return mc.submitPreconfTxWithPendingWait(rawTx, false)
}

func (mc *multiClient) submitPreconfTxBatchWithoutPendingWait(rawTxs [][]byte) []preconfSubmissionResult {
	results := make([]preconfSubmissionResult, len(rawTxs))
	if len(rawTxs) == 0 {
		return results
	}
	type clientResults struct {
		responses []SendTxForPreconfResponse
		errors    []error
	}
	allResults := make(chan clientResults, len(mc.clients))
	for _, client := range mc.clients {
		go func(client *rpc.Client) {
			responses, errors, err := callPreconfBatch(client, rawTxs)
			if err != nil {
				responses, errors, _ = callPreconfBatch(client, rawTxs)
			}
			allResults <- clientResults{responses: responses, errors: errors}
		}(client)
	}
	accepted := make([]int, len(rawTxs))
	for range mc.clients {
		clientResult := <-allResults
		for i, err := range clientResult.errors {
			if err != nil {
				preconfRpcFailureMeter.Mark(1)
				mc.rejectionTracker.record(err)
				if isAlreadyKnownError(err) {
					preconfRpcAlreadyKnownMeter.Mark(1)
					accepted[i]++
					continue
				}
				if results[i].err == nil {
					results[i].err = err
				}
				continue
			}
			preconfRpcSuccessMeter.Mark(1)
			if clientResult.responses[i].Preconfirmed {
				accepted[i]++
			}
		}
	}
	for i := range results {
		results[i].preconfirmed = accepted[i] == len(mc.clients)
		if !results[i].preconfirmed && results[i].err == nil {
			belowThresholdPreconfMeter.Mark(1)
		}
	}
	return results
}

func callPreconfBatch(client *rpc.Client, rawTxs [][]byte) ([]SendTxForPreconfResponse, []error, error) {
	responses := make([]SendTxForPreconfResponse, len(rawTxs))
	batch := make([]rpc.BatchElem, len(rawTxs))
	for i, rawTx := range rawTxs {
		batch[i] = rpc.BatchElem{
			Method: "eth_sendRawTransactionForPreconf",
			Args:   []any{hexutil.Encode(rawTx), false},
			Result: &responses[i],
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), preconfBatchRPCTimeout)
	err := client.BatchCallContext(ctx, batch)
	cancel()
	errors := make([]error, len(rawTxs))
	for i := range batch {
		errors[i] = batch[i].Error
		if errors[i] == nil && err != nil {
			errors[i] = err
		}
	}
	return responses, errors, err
}

func (mc *multiClient) submitPreconfTxWithPendingWait(rawTx []byte, waitForPending bool) (bool, error) {
	// Submit tx to all block producers in parallel
	var (
		firstErr            error
		wg                  sync.WaitGroup
		setError            sync.Once
		preconfOfferedCount atomic.Uint64
	)
	for i, client := range mc.clients {
		wg.Add(1)
		go func(client *rpc.Client, index int) {
			defer wg.Done()

			callStart := time.Now()
			var preconfResponse SendTxForPreconfResponse
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			var err error
			if waitForPending {
				err = client.CallContext(ctx, &preconfResponse, "eth_sendRawTransactionForPreconf", hexutil.Encode(rawTx))
			} else {
				err = client.CallContext(ctx, &preconfResponse, "eth_sendRawTransactionForPreconf", hexutil.Encode(rawTx), false)
			}
			cancel()
			elapsed := time.Since(callStart)

			if err != nil {
				preconfRpcFailureMeter.Mark(1)
				mc.rejectionTracker.record(err)
				// If the tx is already known, treat it as preconfirmed for this node
				if isAlreadyKnownError(err) {
					preconfRpcAlreadyKnownMeter.Mark(1)
					preconfOfferedCount.Add(1)
					return
				}
				log.Debug("[tx-relay] Failed to submit preconf tx", "err", err, "producer", index, "elapsed", elapsed)
				setError.Do(func() {
					firstErr = err
				})
				return
			}
			preconfRpcSuccessMeter.Mark(1)
			if preconfResponse.Preconfirmed {
				preconfOfferedCount.Add(1)
			}
		}(client, i)
	}
	wg.Wait()

	// Note: this can be improved later to only check for current block producer instead of all
	// Only offer a preconf if the tx was accepted by all block producers
	if preconfOfferedCount.Load() == uint64(len(mc.clients)) {
		return true, nil
	}

	// All BPs accepted the tx but at least one of them didn't offer a preconf
	if firstErr == nil {
		belowThresholdPreconfMeter.Mark(1)
	}

	return false, firstErr
}

// submitPrivateTx submits a raw private transaction to all block producers. When retry is
// true and some producers fail, a background goroutine retries those producers. The
// returned channel (non-nil only when a retry goroutine is started) is closed when the
// goroutine finishes, allowing callers to wait for completion deterministically.
func (mc *multiClient) submitPrivateTx(rawTx []byte, hash common.Hash, retry bool, txGetter TxGetter) (error, <-chan struct{}) {
	// Submit tx to all block producers in parallel (initial attempt)
	hexTx := hexutil.Encode(rawTx)

	var (
		firstErr error
		setError sync.Once
		wg       sync.WaitGroup
		mu       sync.Mutex
	)
	failedIndices := make([]int, 0)
	successfulIndices := make([]int, 0)

	for i, client := range mc.clients {
		wg.Add(1)
		go func(client *rpc.Client, index int) {
			defer wg.Done()

			callStart := time.Now()
			var txHash common.Hash
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			err := client.CallContext(ctx, &txHash, "eth_sendRawTransactionPrivate", hexTx)
			cancel()
			elapsed := time.Since(callStart)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				privateTxRpcFailureMeter.Mark(1)
				mc.rejectionTracker.record(err)
				// If the tx is already known, treat it as successful submission
				if isAlreadyKnownError(err) {
					privateTxRpcAlreadyKnownMeter.Mark(1)
					successfulIndices = append(successfulIndices, index)
					return
				}
				setError.Do(func() {
					firstErr = err
				})
				failedIndices = append(failedIndices, index)
				log.Debug("[tx-relay] Failed to submit private tx (initial attempt)", "err", err, "producer", index, "hash", hash, "elapsed", elapsed)
			} else {
				privateTxRpcSuccessMeter.Mark(1)
				successfulIndices = append(successfulIndices, index)
			}
		}(client, i)
	}
	wg.Wait()

	// If all submissions successful, return immediately
	if len(failedIndices) == 0 {
		log.Debug("[tx-relay] Successfully submitted private tx to all producers", "hash", hash)
		return nil, nil
	}

	if retry && !mc.closed.Load() {
		// Some submissions failed, start background retry
		log.Debug("[tx-relay] Failed to submit private tx to one or more block producers, starting retry",
			"err", firstErr, "failed", len(failedIndices), "successful", len(successfulIndices), "total", len(mc.clients), "hash", hash)
		done := make(chan struct{})
		go func() {
			mc.retryPrivateTxSubmission(hexTx, hash, failedIndices, txGetter)
			close(done)
		}()
		return firstErr, done
	}

	return firstErr, nil
}

// retryPrivateTxSubmission runs in background to retry private tx submission to producers
// that failed initially. It uses local txGetter to check if tx was included in a block.
func (mc *multiClient) retryPrivateTxSubmission(hexTx string, hash common.Hash, failedIndices []int, txGetter TxGetter) {
	currentFailedIndices := failedIndices

	for retry := 0; retry < privateTxMaxRetries; retry++ {
		if mc.closed.Load() {
			return
		}

		// If no more failed producers, we're done
		if len(currentFailedIndices) == 0 {
			privateTxRetrySuccessMeter.Mark(1)
			return
		}

		// Sleep before retry
		interval := mc.retryInterval
		if interval == 0 {
			interval = privateTxRetryInterval
		}
		time.Sleep(interval)

		log.Debug("[tx-relay] Retrying private tx submission", "producers", len(currentFailedIndices), "attempt", retry+1, "hash", hash)

		// Check if tx was already included in a block in local db. If yes, skip
		// retrying submission altogether.
		if txGetter != nil {
			found, tx, _, _, _ := txGetter(hash)
			if found && tx != nil {
				privateTxRetrySuccessMeter.Mark(1)
				log.Debug("[tx-relay] Transaction found in local database, stopping retry", "hash", hash)
				return
			}
		}

		// Retry submission for failed producers
		var retryWg sync.WaitGroup
		var mu sync.Mutex
		newFailedIndices := make([]int, 0)

		for _, index := range currentFailedIndices {
			retryWg.Add(1)
			go func(client *rpc.Client, idx int) {
				defer retryWg.Done()

				var txHash common.Hash
				ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
				err := client.CallContext(ctx, &txHash, "eth_sendRawTransactionPrivate", hexTx)
				cancel()

				if err != nil {
					privateTxRpcFailureMeter.Mark(1)
					mc.rejectionTracker.record(err)
					// If the tx is already known, treat it as successful submission
					if isAlreadyKnownError(err) {
						privateTxRpcAlreadyKnownMeter.Mark(1)
						return
					}
					mu.Lock()
					newFailedIndices = append(newFailedIndices, idx)
					mu.Unlock()
				} else {
					privateTxRpcSuccessMeter.Mark(1)
				}
			}(mc.clients[index], index)
		}
		retryWg.Wait()

		// Update failed indices for next iteration
		currentFailedIndices = newFailedIndices
	}

	if len(currentFailedIndices) > 0 {
		log.Debug("[tx-relay] Finished retry attempts with some producers still failing",
			"hash", hash, "failed", len(currentFailedIndices))
	} else {
		privateTxRetrySuccessMeter.Mark(1)
		log.Debug("[tx-relay] All producers accepted private tx after retries", "hash", hash)
	}
}

func (mc *multiClient) checkTxStatus(hash common.Hash) (bool, error) {
	// Submit tx to all block producers in parallel
	var (
		firstErr            error
		setError            sync.Once
		wg                  sync.WaitGroup
		preconfOfferedCount atomic.Uint64
	)
	for i, client := range mc.clients {
		wg.Add(1)
		go func(client *rpc.Client, index int) {
			defer wg.Done()

			var txStatus txpool.TxStatus
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			err := client.CallContext(ctx, &txStatus, "txpool_txStatus", hash)
			cancel()
			if err != nil {
				checkStatusRpcFailureMeter.Mark(1)
				setError.Do(func() {
					firstErr = err
				})
				return
			}
			checkStatusRpcSuccessMeter.Mark(1)
			if txStatus == txpool.TxStatusPending {
				preconfOfferedCount.Add(1)
			}
		}(client, i)
	}
	wg.Wait()

	// Only offer a preconf if the tx was accepted by all block producers
	if preconfOfferedCount.Load() == uint64(len(mc.clients)) {
		return true, nil
	}

	// All BPs accepted the tx but at least one of them didn't offer a preconf
	if firstErr == nil {
		belowThresholdPreconfMeter.Mark(1)
	}

	return false, firstErr
}

// Close closes all rpc client connections
func (mc *multiClient) close() {
	mc.closeOnce.Do(func() {
		mc.closed.Store(true)
		if mc.reporterDone != nil {
			close(mc.reporterDone)
		}
		for _, client := range mc.clients {
			client.Close()
		}
		for _, transport := range mc.transports {
			if transport != nil {
				transport.CloseIdleConnections()
			}
		}
	})
}

// reportRejections runs in the background and flushes the rejection tracker on a
// fixed interval, emitting one aggregated error log for different error types.
func (mc *multiClient) reportRejections() {
	ticker := time.NewTicker(rejectionReportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			total, counts := mc.rejectionTracker.flush()
			log.Info("[tx-relay] BP rejection summary",
				"total", total,
				"errors", formatRejectionCounts(counts),
			)
		case <-mc.reporterDone:
			return
		}
	}
}
