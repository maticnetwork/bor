package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

func waitCachedTask(t *testing.T, service *Service, hash common.Hash) TxTask {
	t.Helper()

	return waitCachedTaskWhere(t, service, hash, func(TxTask) bool { return true }, "expected task to be stored after processing")
}

func waitCachedTaskWhere(t *testing.T, service *Service, hash common.Hash, check func(TxTask) bool, msg string) TxTask {
	t.Helper()

	var task TxTask
	require.Eventually(t, func() bool {
		service.storeMu.RLock()
		defer service.storeMu.RUnlock()

		var exists bool
		task, exists = service.store[hash]
		return exists && check(task)
	}, 2*time.Second, 10*time.Millisecond, msg)

	return task
}

func TestNewService(t *testing.T) {
	t.Parallel()

	t.Run("service initializes with valid URLs", func(t *testing.T) {
		// Create mock servers
		var rpcServers []*mockRpcServer = make([]*mockRpcServer, 2)
		var urls []string = make([]string, 2)
		for i := 0; i < 2; i++ {
			rpcServers[i] = newMockRpcServer()
			urls[i] = rpcServers[i].server.URL
		}
		defer func() {
			for _, s := range rpcServers {
				s.close()
			}
		}()

		defaultConfig := DefaultServiceConfig
		service := NewService(urls, nil)
		require.NotNil(t, service, "expected non-nil service")
		require.NotNil(t, service.multiclient, "expected non-nil multiclient")
		require.NotNil(t, service.store, "expected non-nil store")
		require.NotNil(t, service.taskCh, "expected non-nil task channel")
		require.Equal(t, defaultConfig.maxQueuedTasks, cap(service.taskCh), "expected task channel capacity to match maxQueuedTasks")
		require.Equal(t, defaultConfig.maxConcurrentTasks, cap(service.semaphore), "expected semaphore capacity to match maxConcurrentTasks")

		service.close()
	})

	t.Run("service initializes with nil multiclient when no URLs", func(t *testing.T) {
		service := NewService([]string{}, nil)
		require.NotNil(t, service, "expected non-nil service")
		require.Nil(t, service.multiclient, "expected nil multiclient with empty URLs")

		service.close()
	})
}

func TestSubmitTransactionForPreconf(t *testing.T) {
	t.Parallel()

	// Create mock servers
	var rpcServers []*mockRpcServer = make([]*mockRpcServer, 2)
	var urls []string = make([]string, 2)
	for i := 0; i < 2; i++ {
		rpcServers[i] = newMockRpcServer()
		urls[i] = rpcServers[i].server.URL
	}
	defer func() {
		for _, s := range rpcServers {
			s.close()
		}
	}()

	t.Run("error when multiclient is nil", func(t *testing.T) {
		service := NewService([]string{}, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.ErrorIs(t, err, errRpcClientUnavailable, "expected errRpcClientUnavailable error on nil multiclient")
	})

	t.Run("queue valid tx for preconf", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task")

		// Give some time to process
		time.Sleep(100 * time.Millisecond)

		// Check task was stored
		service.storeMu.RLock()
		task, exists := service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored after processing")
		require.True(t, task.preconfirmed, "expected task to be preconfirmed")
	})

	t.Run("wait for producer submission", func(t *testing.T) {
		for _, server := range rpcServers {
			server.setHandleSendPreconfTx(func(w http.ResponseWriter, id int, params json.RawMessage) {
				time.Sleep(20 * time.Millisecond)
				defaultHandleSendPreconfTx(w, id, params)
			})
		}
		defer func() {
			for _, server := range rpcServers {
				server.setHandleSendPreconfTx(defaultHandleSendPreconfTx)
			}
		}()

		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		started := time.Now()
		err := service.SubmitTransactionForPreconfSync(context.Background(), tx)
		require.NoError(t, err)
		require.GreaterOrEqual(t, time.Since(started), 20*time.Millisecond)
	})

	t.Run("stop waiting when context closes", func(t *testing.T) {
		for _, server := range rpcServers {
			server.setHandleSendPreconfTx(func(w http.ResponseWriter, id int, params json.RawMessage) {
				time.Sleep(100 * time.Millisecond)
				defaultHandleSendPreconfTx(w, id, params)
			})
		}
		defer func() {
			for _, server := range rpcServers {
				server.setHandleSendPreconfTx(defaultHandleSendPreconfTx)
			}
		}()

		service := NewService(urls, nil)
		defer service.close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		require.ErrorIs(t, service.SubmitTransactionForPreconfSync(ctx, tx), context.DeadlineExceeded)
	})

	t.Run("queue invalid tx for preconf", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Mock the server to send no preconf but accept tx submission
		rpcServers[0].setHandleSendPreconfTx(handleSendPreconfTxWithRejection)

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task")

		// Give some time to process
		time.Sleep(100 * time.Millisecond)

		// Check task was stored
		service.storeMu.RLock()
		task, exists := service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored after processing")
		require.False(t, task.preconfirmed, "expected task to be preconfirmed")
		require.ErrorIs(t, task.err, errPreconfValidationFailed, "expected preconf validation failed error")
	})

	t.Run("queue overflow with burst submissions", func(t *testing.T) {
		// Update the config to a reasonable size for testing
		config := DefaultServiceConfig
		config.maxQueuedTasks = 10
		config.maxConcurrentTasks = 5

		service := NewService(urls, &config)
		defer service.close()

		// Block the semaphore so that tasks are queued entirely
		for i := 0; i < config.maxConcurrentTasks; i++ {
			service.semaphore <- struct{}{}
		}

		// Fill the queue to full capacity. We need to do config.maxQueuedTasks+1 because
		// first task will be consumed.
		for i := 0; i <= config.maxQueuedTasks; i++ {
			tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
			err := service.SubmitTransactionForPreconf(tx)
			require.NoError(t, err, "expected no error for task %d", i)
			if i == 0 {
				// Wait for a very small delay to allow first task to be consumed
				time.Sleep(20 * time.Millisecond)
			}
		}

		// Next submission should fail due to overflow
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.Error(t, err, "expected error when queue is full")
		require.Equal(t, errQueueOverflow, err, "expected errQueueOverflow")
	})

	t.Run("max concurrent tasks", func(t *testing.T) {
		// Update the config to a reasonable size for testing
		config := DefaultServiceConfig
		config.maxQueuedTasks = 10
		config.maxConcurrentTasks = 5

		// Update the rpc server handlers to have a delay in processing tasks
		for _, s := range rpcServers {
			s.setHandleSendPreconfTx(func(w http.ResponseWriter, id int, params json.RawMessage) {
				time.Sleep(time.Second)
				defaultHandleSendPreconfTx(w, id, params)
			})
		}

		service := NewService(urls, &config)
		defer service.close()

		// Start sending `maxConcurrentTasks` tasks to block the queue
		for i := 0; i <= config.maxConcurrentTasks; i++ {
			tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
			err := service.SubmitTransactionForPreconf(tx)
			require.NoError(t, err, "expected no error for task %d", i)
		}

		// While these tasks are being processed, send one more task.
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task within capacity")

		// Check that queue size is 1 (as it should only contain the last task) after a small delay
		time.Sleep(100 * time.Millisecond)
		queueSize := len(service.taskCh)
		require.Equal(t, 1, queueSize, "expected only 1 task in queue")

		// Check again after a small delay
		time.Sleep(500 * time.Millisecond)
		queueSize = len(service.taskCh)
		require.Equal(t, 1, queueSize, "expected only 1 task in queue")

		// Check again after a small delay. By now, at least one of the tasks
		// would have been processed.
		time.Sleep(500 * time.Millisecond)
		queueSize = len(service.taskCh)
		require.Equal(t, 0, queueSize, "expected no tasks in queue")

		// Reset all rpc servers
		for _, s := range rpcServers {
			s.setHandleSendPreconfTx(defaultHandleSendPreconfTx)
		}
	})

	t.Run("error when service is closing", func(t *testing.T) {
		service := NewService(urls, nil)

		// Close service first
		service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.Error(t, err, "expected error when service is closing")
		require.Equal(t, errRpcClientUnavailable, err, "expected errRpcClientUnavailable")
	})

	t.Run("concurrent preconf task submissions", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		var wg sync.WaitGroup
		numTasks := 2_000
		successCount := atomic.Int32{}

		var nonce atomic.Uint64

		// Launch goroutines in batches to avoid overwhelming the system
		batchSize := 100
		for batch := 0; batch < numTasks/batchSize; batch++ {
			for i := 0; i < batchSize; i++ {
				wg.Add(1)
				idx := batch*batchSize + i
				go func(taskIdx int) {
					defer wg.Done()

					tx := types.NewTransaction(nonce.Add(1), common.Address{}, nil, 0, nil, nil)
					err := service.SubmitTransactionForPreconf(tx)
					if err == nil {
						successCount.Add(1)
					}
				}(idx)
			}
		}

		wg.Wait()
		require.Equal(t, int32(numTasks), successCount.Load(), "expected all tasks to be queued without any errors")

		// Wait for all tasks to be processed
		time.Sleep(3 * time.Second)

		// Verify tasks were processed
		service.storeMu.RLock()
		storeSize := len(service.store)
		require.Equal(t, numTasks, storeSize, "expected store size to be same as number of tasks")
		for hash, task := range service.store {
			require.NoError(t, task.err, "expected no error in task %s", hash.Hex())
			require.True(t, task.preconfirmed, "expected task %s to be preconfirmed", hash.Hex())
		}
		service.storeMu.RUnlock()
	})
}

func TestServiceSubmitPrivateTx(t *testing.T) {
	t.Parallel()

	// Create mock servers
	var rpcServers []*mockRpcServer = make([]*mockRpcServer, 2)
	var urls []string = make([]string, 2)
	for i := 0; i < 2; i++ {
		rpcServers[i] = newMockRpcServer()
		urls[i] = rpcServers[i].server.URL
	}
	defer func() {
		for _, s := range rpcServers {
			s.close()
		}
	}()

	t.Run("error when multiclient is nil", func(t *testing.T) {
		service := NewService([]string{}, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitPrivateTx(tx, false)
		require.ErrorIs(t, err, errRpcClientUnavailable, "expected errRpcClientUnavailable error on nil multiclient")
	})

	t.Run("submit valid private tx", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitPrivateTx(tx, false)
		require.NoError(t, err, "expected no error submitting private tx")
	})

	t.Run("error when submission fails", func(t *testing.T) {
		// Mock server to fail private tx submissions
		rpcServers[0].setHandleSendPrivateTx(func(w http.ResponseWriter, id int, params json.RawMessage) {
			defaultSendError(w, id, -32601, "internal server error")
		})

		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitPrivateTx(tx, false)
		require.Equal(t, errPrivateTxSubmissionFailed, err, "expected errPrivateTxSubmissionFailed")

		// Reset handler
		rpcServers[0].setHandleSendPrivateTx(defaultHandleSendPrivateTx)
	})

	t.Run("concurrent private tx submissions", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		var wg sync.WaitGroup
		numTxs := 50
		successCount := atomic.Int32{}

		for i := 0; i < numTxs; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				tx := types.NewTransaction(uint64(idx), common.Address{}, nil, 0, nil, nil)
				err := service.SubmitPrivateTx(tx, false)
				if err == nil {
					successCount.Add(1)
				}
			}(i)
		}

		wg.Wait()
		require.Equal(t, int32(numTxs), successCount.Load(), "expected all private txs to be submitted successfully")
	})
}

func TestCheckTxPreconfStatus(t *testing.T) {
	t.Parallel()

	// Create mock servers
	var rpcServers []*mockRpcServer = make([]*mockRpcServer, 2)
	var urls []string = make([]string, 2)
	for i := 0; i < 2; i++ {
		rpcServers[i] = newMockRpcServer()
		urls[i] = rpcServers[i].server.URL
	}
	defer func() {
		for _, s := range rpcServers {
			s.close()
		}
	}()

	t.Run("respond task preconfirmation result from cache", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Submit and wait for processing
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)

		// Check preconfirmation status
		preconfirmed, err := service.CheckTxPreconfStatus(tx.Hash())
		require.NoError(t, err, "expected no error when checking preconf status")
		require.True(t, preconfirmed, "expected preconfirmation to be true")
	})

	// Case when task is not available in cache and we do the status check by hash
	// against block producers and it passes.
	t.Run("check tx status when task not available in cache", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Track call count to ensure checkTxStatus is called
		var callCount [2]atomic.Int32
		for i, server := range rpcServers {
			server.setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				callCount[i].Add(1)
				defaultHandleTxStatus(w, id, params)
			})
		}

		// Confirm that unknown tx is not present in cache
		unknownHash := common.HexToHash("0x1")
		service.storeMu.RLock()
		_, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.False(t, exists, "expected task to not exist in cache")

		// Check preconfirmation status
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.NoError(t, err, "expected no error when checking preconf status for unknown tx")
		require.True(t, preconfirmed, "expected preconfirmation to be true for unknown tx")

		// Ensure that checkTxStatus was called on all rpc servers
		for i := range rpcServers {
			require.Equal(t, int32(1), callCount[i].Load(), "expected checkTxStatus to be called once on rpc server %d", i)
		}

		// Ensure that cache is updated
		service.storeMu.RLock()
		task, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.True(t, task.preconfirmed, "expected task to be preconfirmed in cache")

		// Check preconfirmation status again to verify cache hit
		preconfirmed, err = service.CheckTxPreconfStatus(unknownHash)
		require.NoError(t, err, "expected no error when checking preconf status for unknown tx")
		require.True(t, preconfirmed, "expected preconfirmation to be true for unknown tx")

		// Ensure checkTxStatus wasn't called again
		for i := range rpcServers {
			require.Equal(t, int32(1), callCount[i].Load(), "expected checkTxStatus to be called once on rpc server %d", i)
		}
	})

	// Case when task is not available in cache and we do the status check by hash
	// against block producers. The call passes but returns false suggesting tx is
	// not preconfirmed.
	t.Run("tx status returns no preconfirmation when task not available in cache", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Track call count to ensure checkTxStatus is called and it returns no preconf
		var callCount [2]atomic.Int32
		handleTxStatus := makeTxStatusHandler(map[common.Hash]txpool.TxStatus{})
		for i, server := range rpcServers {
			server.setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				callCount[i].Add(1)
				handleTxStatus(w, id, params)
			})
		}

		// Confirm that unknown tx is not present in cache
		unknownHash := common.HexToHash("0x1")
		service.storeMu.RLock()
		_, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.False(t, exists, "expected task to not exist in cache")

		// Check preconfirmation status
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.Error(t, err, "expected error when checking preconf status for unknown tx")
		require.ErrorIs(t, err, errPreconfValidationFailed, "expected errPreconfValidationFailed")
		require.False(t, preconfirmed, "expected preconfirmation to be false for unknown tx")

		// Ensure that checkTxStatus was called on all rpc servers
		for i := range rpcServers {
			require.Equal(t, int32(1), callCount[i].Load(), "expected checkTxStatus to be called once on rpc server %d", i)
		}

		// Ensure that cache is updated
		service.storeMu.RLock()
		task, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.False(t, task.preconfirmed, "expected task to be not preconfirmed in cache")
		require.ErrorIs(t, task.err, errPreconfValidationFailed, "expected task error to be errPreconfValidationFailed")

		// Check preconfirmation status again to ensure tx status is re-checked
		preconfirmed, err = service.CheckTxPreconfStatus(unknownHash)
		require.Error(t, err, "expected error when checking preconf status for unknown tx")
		require.ErrorIs(t, err, errPreconfValidationFailed, "expected errPreconfValidationFailed")
		require.False(t, preconfirmed, "expected preconfirmation to be false for unknown tx")

		// Ensure checkTxStatus was called again
		for i := range rpcServers {
			require.Equal(t, int32(2), callCount[i].Load(), "expected checkTxStatus to be called twice on rpc server %d", i)
		}
	})

	// Case when task is not available in cache and we do the status check by hash
	// against block producers. The call fails suggesting tx is not preconfirmed.
	t.Run("tx status check fails when task not available in cache", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Track call count to ensure checkTxStatus is called and the call fails.
		var callCount [2]atomic.Int32
		for i, server := range rpcServers {
			server.setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				callCount[i].Add(1)
				defaultSendError(w, id, -32603, "internal server error")
			})
		}

		// Confirm that unknown tx is not present in cache
		unknownHash := common.HexToHash("0x1")
		service.storeMu.RLock()
		_, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.False(t, exists, "expected task to not exist in cache")

		// Check preconfirmation status
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.Error(t, err, "expected error when checking preconf status for unknown tx")
		require.NotErrorIs(t, err, errPreconfValidationFailed, "expected an error other than errPreconfValidationFailed")
		require.False(t, preconfirmed, "expected preconfirmation to be false for unknown tx")

		// Ensure that checkTxStatus was called on all rpc servers
		for i := range rpcServers {
			require.Equal(t, int32(1), callCount[i].Load(), "expected checkTxStatus to be called once on rpc server %d", i)
		}

		// Ensure that cache is updated
		service.storeMu.RLock()
		task, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.False(t, task.preconfirmed, "expected task to be not preconfirmed in cache")
		require.NotErrorIs(t, task.err, errPreconfValidationFailed, "expected an error other than errPreconfValidationFailed")

		// Check preconfirmation status again to ensure tx status is re-checked
		preconfirmed, err = service.CheckTxPreconfStatus(unknownHash)
		require.Error(t, err, "expected error when checking preconf status for unknown tx")
		require.NotErrorIs(t, err, errPreconfValidationFailed, "expected an error other than errPreconfValidationFailed")
		require.False(t, preconfirmed, "expected preconfirmation to be false for unknown tx")

		// Ensure checkTxStatus was called again
		for i := range rpcServers {
			require.Equal(t, int32(2), callCount[i].Load(), "expected checkTxStatus to be called twice on rpc server %d", i)
		}
	})

	// Case when task is not available in cache and we do the status check by hash
	// against block producers. The call fails initially but later passes second time.
	t.Run("tx status check fails first and then passes when task not available in cache", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Track call count to ensure checkTxStatus is called and the call fails.
		var callCount [2]atomic.Int32
		for i, server := range rpcServers {
			server.setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				callCount[i].Add(1)
				defaultSendError(w, id, -32603, "internal server error")
			})
		}

		// Confirm that unknown tx is not present in cache
		unknownHash := common.HexToHash("0x1")
		service.storeMu.RLock()
		_, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.False(t, exists, "expected task to not exist in cache")

		// Check preconfirmation status
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.Error(t, err, "expected error when checking preconf status for unknown tx")
		require.NotErrorIs(t, err, errPreconfValidationFailed, "expected an error other than errPreconfValidationFailed")
		require.False(t, preconfirmed, "expected preconfirmation to be false for unknown tx")

		// Ensure that checkTxStatus was called on all rpc servers
		for i := range rpcServers {
			require.Equal(t, int32(1), callCount[i].Load(), "expected checkTxStatus to be called once on rpc server %d", i)
		}

		// Ensure that cache is updated
		service.storeMu.RLock()
		task, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.False(t, task.preconfirmed, "expected task to be not preconfirmed in cache")
		require.NotErrorIs(t, task.err, errPreconfValidationFailed, "expected an error other than errPreconfValidationFailed")

		// Update the handler to return preconfirmed status
		for i := range rpcServers {
			handleTxStatus := makeTxStatusHandler(map[common.Hash]txpool.TxStatus{
				unknownHash: txpool.TxStatusPending,
			})
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				callCount[i].Add(1)
				handleTxStatus(w, id, params)
			})
		}

		// Check preconfirmation status again to ensure tx status is re-checked
		preconfirmed, err = service.CheckTxPreconfStatus(unknownHash)
		require.NoError(t, err, "expected no error when checking preconf status for unknown tx")
		require.True(t, preconfirmed, "expected preconfirmation to be true for unknown tx")

		// Ensure checkTxStatus was called again
		for i := range rpcServers {
			require.Equal(t, int32(2), callCount[i].Load(), "expected checkTxStatus to be called twice on rpc server %d", i)
		}

		// Ensure that cache is updated to preconfirmed
		service.storeMu.RLock()
		task, exists = service.store[unknownHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.True(t, task.preconfirmed, "expected task to be preconfirmed in cache")

		// Ensure checkTxStatus wasn't called again to verify cache hit
		for i := range rpcServers {
			require.Equal(t, int32(2), callCount[i].Load(), "expected checkTxStatus to be called twice on rpc server %d", i)
		}
	})

	t.Run("re-checks status when not preconfirmed initially", func(t *testing.T) {
		// Mock servers to reject preconf initially
		rpcServers[0].setHandleSendPreconfTx(handleSendPreconfTxWithRejection)

		service := NewService(urls, nil)
		defer service.close()

		// Submit and wait for processing
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err)
		time.Sleep(200 * time.Millisecond)

		// Ensure that the preconfirmation task is stored as not preconfirmed
		service.storeMu.RLock()
		task, exists := service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored")
		require.False(t, task.preconfirmed, "expected task to be not preconfirmed")

		// Mock servers to return unknown tx status on initial status
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(makeTxStatusHandler(map[common.Hash]txpool.TxStatus{}))
		}

		// Check status - should re-check via checkTxStatus
		preconfirmed, err := service.CheckTxPreconfStatus(tx.Hash())
		require.Equal(t, errPreconfValidationFailed, err, "expected errPreconfValidationFailed")
		require.False(t, preconfirmed, "expected preconfirmed to be false after re-check")

		// Now update the mock servers to return pending status
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(makeTxStatusHandler(map[common.Hash]txpool.TxStatus{
				tx.Hash(): txpool.TxStatusPending,
			}))
		}

		// Check status - should again re-check via checkTxStatus
		preconfirmed, err = service.CheckTxPreconfStatus(tx.Hash())
		require.NoError(t, err, "expected no error on re-check with pending status")
		require.True(t, preconfirmed, "expected preconfirmed to be true after re-check")

		// Reset handlers
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(defaultHandleTxStatus)
			rpcServers[i].setHandleSendPreconfTx(defaultHandleSendPreconfTx)
		}
	})

	t.Run("tx found in local database via txGetter", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Track if checkTxStatus is called (it shouldn't be)
		var checkTxStatusCalled atomic.Int32
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				checkTxStatusCalled.Add(1)
				defaultHandleTxStatus(w, id, params)
			})
		}

		// Create a transaction that will be "found" in local database
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		txHash := tx.Hash()

		// Set up mock txGetter that returns the transaction
		service.SetTxGetter(func(hash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
			if hash == txHash {
				return true, tx, common.Hash{}, 0, 0
			}
			return false, nil, common.Hash{}, 0, 0
		})

		// Check preconfirmation status - should find in local DB
		preconfirmed, err := service.CheckTxPreconfStatus(txHash)
		require.NoError(t, err, "expected no error when tx found in local database")
		require.True(t, preconfirmed, "expected preconfirmation to be true when tx found in local database")

		// Verify checkTxStatus was not called
		require.Equal(t, int32(0), checkTxStatusCalled.Load(), "expected checkTxStatus to not be called when tx found in local database")

		// Verify cache was updated
		service.storeMu.RLock()
		task, exists := service.store[txHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.True(t, task.preconfirmed, "expected task to be preconfirmed in cache")
		require.NoError(t, task.err, "expected no error as tx was found in local database")

		// Check again - should hit cache and not call txGetter or checkTxStatus
		preconfirmed, err = service.CheckTxPreconfStatus(txHash)
		require.NoError(t, err, "expected no error on second check")
		require.True(t, preconfirmed, "expected preconfirmation to be true on second check")
		require.Equal(t, int32(0), checkTxStatusCalled.Load(), "expected checkTxStatus to still not be called")
	})

	t.Run("tx not found in local database falls back to checkTxStatus", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Track if checkTxStatus is called
		var checkTxStatusCalled atomic.Int32
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				checkTxStatusCalled.Add(1)
				defaultHandleTxStatus(w, id, params)
			})
		}

		unknownHash := common.HexToHash("0x1")

		// Set up mock txGetter that doesn't find the transaction
		var txGetterCalled atomic.Int32
		service.SetTxGetter(func(hash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
			txGetterCalled.Add(1)
			return false, nil, common.Hash{}, 0, 0
		})

		// Check preconfirmation status - should fall back to checkTxStatus
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.NoError(t, err, "expected no error when falling back to checkTxStatus")
		require.True(t, preconfirmed, "expected preconfirmation to be true from checkTxStatus")

		// Verify txGetter was called
		require.Equal(t, int32(1), txGetterCalled.Load(), "expected txGetter to be called once")

		// Verify checkTxStatus was called as fallback
		require.Equal(t, int32(2), checkTxStatusCalled.Load(), "expected checkTxStatus to be called on both servers")

		// Verify cache was updated
		service.storeMu.RLock()
		task, exists := service.store[unknownHash]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored in cache")
		require.True(t, task.preconfirmed, "expected task to be preconfirmed in cache")
	})

	t.Run("txGetter not set falls back to checkTxStatus", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Don't set txGetter - it should fall back to checkTxStatus

		// Track if checkTxStatus is called
		var checkTxStatusCalled atomic.Int32
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				checkTxStatusCalled.Add(1)
				defaultHandleTxStatus(w, id, params)
			})
		}

		unknownHash := common.HexToHash("0x1")

		// Check preconfirmation status - should go straight to checkTxStatus
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.NoError(t, err, "expected no error when txGetter not set")
		require.True(t, preconfirmed, "expected preconfirmation to be true from checkTxStatus")

		// Verify checkTxStatus was called
		require.Equal(t, int32(2), checkTxStatusCalled.Load(), "expected checkTxStatus to be called on both servers")
	})

	t.Run("tx found in local database updates cache for non-preconfirmed task", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Submit tx that gets rejected
		rpcServers[0].setHandleSendPreconfTx(handleSendPreconfTxWithRejection)
		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)

		// Verify task exists but not preconfirmed
		service.storeMu.RLock()
		task, exists := service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to be stored")
		require.False(t, task.preconfirmed, "expected task to be not preconfirmed initially")

		// Set up txGetter that finds the transaction (simulating it got included)
		service.SetTxGetter(func(hash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
			if hash == tx.Hash() {
				return true, tx, common.Hash{}, 0, 0
			}
			return false, nil, common.Hash{}, 0, 0
		})

		// Track if checkTxStatus is called (it shouldn't be)
		var checkTxStatusCalled atomic.Int32
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				checkTxStatusCalled.Add(1)
				defaultHandleTxStatus(w, id, params)
			})
		}

		// Check status - should find in local DB and update cache
		preconfirmed, err := service.CheckTxPreconfStatus(tx.Hash())
		require.NoError(t, err, "expected no error when tx found in local database")
		require.True(t, preconfirmed, "expected preconfirmation to be true when tx found in local database")

		// Verify checkTxStatus was not called
		require.Equal(t, int32(0), checkTxStatusCalled.Load(), "expected checkTxStatus to not be called")

		// Verify cache was updated to preconfirmed
		service.storeMu.RLock()
		task, exists = service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to still be stored")
		require.True(t, task.preconfirmed, "expected task to be preconfirmed after update")
		require.NoError(t, task.err, "expected task error to be nil after update")

		// Reset handler
		rpcServers[0].setHandleSendPreconfTx(defaultHandleSendPreconfTx)
	})

	t.Run("txGetter returns error still falls back to checkTxStatus", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		unknownHash := common.HexToHash("0xabcd")

		// Set up txGetter that returns false (not found)
		var txGetterCalled atomic.Int32
		service.SetTxGetter(func(hash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
			txGetterCalled.Add(1)
			// Return false indicating not found
			return false, nil, common.Hash{}, 0, 0
		})

		// Track if checkTxStatus is called
		var checkTxStatusCalled atomic.Int32
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				checkTxStatusCalled.Add(1)
				defaultHandleTxStatus(w, id, params)
			})
		}

		// Check status - should try txGetter then fall back to checkTxStatus
		preconfirmed, err := service.CheckTxPreconfStatus(unknownHash)
		require.NoError(t, err, "expected no error")
		require.True(t, preconfirmed, "expected preconfirmation from checkTxStatus")

		// Verify both were called
		require.Equal(t, int32(1), txGetterCalled.Load(), "expected txGetter to be called")
		require.Equal(t, int32(2), checkTxStatusCalled.Load(), "expected checkTxStatus to be called as fallback")
	})
}

// TestTaskCacheOverride tests scenarios where the task cache is being updated
// by multiple services - one being the main process task and other being the
// check preconf status.
func TestTaskCacheOverride(t *testing.T) {
	t.Parallel()

	// Create mock servers
	var rpcServers []*mockRpcServer = make([]*mockRpcServer, 2)
	var urls []string = make([]string, 2)
	for i := 0; i < 2; i++ {
		rpcServers[i] = newMockRpcServer()
		urls[i] = rpcServers[i].server.URL
	}
	defer func() {
		for _, s := range rpcServers {
			s.close()
		}
	}()

	t.Run("updateTaskInCache handles writing tasks to cache as expected", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		task := TxTask{
			hash:         tx.Hash(),
			preconfirmed: false,
			err:          errPreconfValidationFailed,
			insertedAt:   time.Now(),
		}

		service.updateTaskInCache(task)

		// Check if the cache was updated
		service.storeMu.RLock()
		cachedTask, exists := service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to exist in cache")
		require.Equal(t, task.preconfirmed, cachedTask.preconfirmed, "expected preconfirmed status to match")
		require.Equal(t, task.err, cachedTask.err, "expected error to match")

		// Update the error and try to write the task again
		task.err = errRelayNotConfigured
		service.updateTaskInCache(task)

		// Check if the cache was updated with new error
		service.storeMu.RLock()
		cachedTask, exists = service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to exist in cache")
		require.Equal(t, task.preconfirmed, cachedTask.preconfirmed, "expected preconfirmed status to match")
		require.Equal(t, task.err, cachedTask.err, "expected error to be updated in cache")

		// Update preconfirmed to true and error to nil
		task.preconfirmed = true
		task.err = nil
		service.updateTaskInCache(task)

		// Check if the cache was updated with new values
		service.storeMu.RLock()
		cachedTask, exists = service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to exist in cache")
		require.Equal(t, task.preconfirmed, cachedTask.preconfirmed, "expected preconfirmed status to be updated in cache")
		require.Equal(t, task.err, cachedTask.err, "expected error to be updated in cache")

		// Try to change the preconf status which should fail
		task.preconfirmed = false
		task.err = errPreconfValidationFailed
		service.updateTaskInCache(task)

		// Check that the cache still has preconfirmed=true and err=nil
		service.storeMu.RLock()
		cachedTask, exists = service.store[tx.Hash()]
		service.storeMu.RUnlock()
		require.True(t, exists, "expected task to exist in cache")
		require.True(t, cachedTask.preconfirmed, "expected preconfirmed status to remain true in cache")
		require.NoError(t, cachedTask.err, "expected error to remain nil in cache")
	})

	t.Run("processPreconfTask succeeds and CheckTxPreconfStatus try to update same task", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task")

		// Ensure task was stored correctly
		task := waitCachedTask(t, service, tx.Hash())
		require.True(t, task.preconfirmed, "expected task to be preconfirmed")
		require.NoError(t, task.err, "expected no error in task")

		// Now, simulate a scenario where CheckTxPreconfStatus tries to update the same task
		// with a non-preconfirmed status. It won't be possible in reality as the task
		// will be available in cache.
		invalidTask := TxTask{
			hash:         tx.Hash(),
			preconfirmed: false,
			err:          errPreconfValidationFailed,
			insertedAt:   time.Now(),
		}
		service.updateTaskInCache(invalidTask)

		// Verify that the original preconfirmed task remains unchanged
		task = waitCachedTask(t, service, tx.Hash())
		require.True(t, task.preconfirmed, "expected preconfirmed status to remain true")
		require.NoError(t, task.err, "expected error to remain nil")
	})

	t.Run("processPreconfTask fails and CheckTxPreconfStatus try to update same task", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		// Reject the preconf tx to simulate failure
		rpcServers[0].setHandleSendPreconfTx(handleSendPreconfTxWithRejection)

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
		err := service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task")

		// Ensure task was stored correctly
		task := waitCachedTask(t, service, tx.Hash())
		require.False(t, task.preconfirmed, "expected task to not be preconfirmed")
		require.ErrorIs(t, task.err, errPreconfValidationFailed, "expected errPreconfValidationFailed in task")

		// Now, CheckTxPreconfStatus tries to update the same task
		res, err := service.CheckTxPreconfStatus(tx.Hash())
		require.Equal(t, true, res, "expected valid preconf to be returned")
		require.NoError(t, err, "expected no error from CheckTxPreconfStatus")

		// Ensure the underlying task was updated to preconfirmed
		task = waitCachedTask(t, service, tx.Hash())
		require.True(t, task.preconfirmed, "expected preconfirmed status to be updated to true")
		require.NoError(t, task.err, "expected error to be updated to nil")

		// Re-run `SubmitTransactionForPreconf` to force a write with invalid preconf
		// status. It should not override the existing status.
		err = service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task again")

		// Give some time to process
		time.Sleep(100 * time.Millisecond)

		// Verify that the preconfirmed task remains unchanged
		task = waitCachedTask(t, service, tx.Hash())
		require.True(t, task.preconfirmed, "expected preconfirmed status to remain true")
		require.NoError(t, task.err, "expected error to remain nil")

		// Reset handler
		rpcServers[0].setHandleSendPreconfTx(defaultHandleSendPreconfTx)
	})

	t.Run("CheckTxPreconfStatus succeeds and processPreconfTask try to update same task", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)

		// Check the preconf status directly
		res, err := service.CheckTxPreconfStatus(tx.Hash())
		require.Equal(t, true, res, "expected valid preconf to be returned")
		require.NoError(t, err, "expected no error from CheckTxPreconfStatus")

		// Ensure task was stored correctly in cache
		task := waitCachedTask(t, service, tx.Hash())
		require.True(t, task.preconfirmed, "expected task to be preconfirmed")
		require.NoError(t, task.err, "expected no error in task")

		// Reject the preconf tx to simulate failure
		rpcServers[0].setHandleSendPreconfTx(handleSendPreconfTxWithRejection)

		// Now, simulate a scenario where processPreconfTask tries to update the same task
		// with a non-preconfirmed status.
		err = service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task")

		// Give some time to process
		time.Sleep(100 * time.Millisecond)

		// Verify that the original preconfirmed task remains unchanged
		task = waitCachedTask(t, service, tx.Hash())
		require.True(t, task.preconfirmed, "expected preconfirmed status to remain true")
		require.NoError(t, task.err, "expected error to remain nil")

		// Reset handler
		rpcServers[0].setHandleSendPreconfTx(defaultHandleSendPreconfTx)
	})

	t.Run("CheckTxPreconfStatus fails and processPreconfTask try to update same task", func(t *testing.T) {
		service := NewService(urls, nil)
		defer service.close()

		tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)

		// Mock the rpc server to reject the tx status call
		for i := range rpcServers {
			rpcServers[i].setHandleTxStatus(func(w http.ResponseWriter, id int, params json.RawMessage) {
				defaultSendError(w, id, -32603, "internal server error")
			})
		}

		// Check the preconf status directly
		res, err := service.CheckTxPreconfStatus(tx.Hash())
		require.Equal(t, false, res, "expected an invalid preconf to be returned")
		require.Error(t, err, "expected an error from CheckTxPreconfStatus")

		// Ensure task was stored correctly in cache
		task := waitCachedTask(t, service, tx.Hash())
		require.False(t, task.preconfirmed, "expected preconfirmed to be false")
		require.Error(t, task.err, "expected an error in task")

		// Now, simulate a scenario where processPreconfTask tries to update the same task
		// with a preconfirmed status.
		err = service.SubmitTransactionForPreconf(tx)
		require.NoError(t, err, "expected no error queuing task")

		// Give some time to process
		time.Sleep(100 * time.Millisecond)

		// Verify that the underlying task is now updated
		task = waitCachedTaskWhere(t, service, tx.Hash(), func(task TxTask) bool {
			return task.preconfirmed && task.err == nil
		}, "expected task to be updated after processing")
		require.True(t, task.preconfirmed, "expected preconfirmed status to be true")
		require.NoError(t, task.err, "expected no error for the task")

		// Ensure that preconf status will now return valid result from cache
		res, err = service.CheckTxPreconfStatus(tx.Hash())
		require.Equal(t, true, res, "expected valid preconf to be returned from cache")
		require.NoError(t, err, "expected no error from CheckTxPreconfStatus")
	})
}

func TestTaskCleanup(t *testing.T) {
	t.Parallel()

	// Create mock servers
	var rpcServers []*mockRpcServer = make([]*mockRpcServer, 2)
	var urls []string = make([]string, 2)
	for i := 0; i < 2; i++ {
		rpcServers[i] = newMockRpcServer()
		urls[i] = rpcServers[i].server.URL
	}
	defer func() {
		for _, s := range rpcServers {
			s.close()
		}
	}()

	// Use a short expiry interval for testing
	config := DefaultServiceConfig
	config.expiryTickerInterval = 200 * time.Millisecond
	config.expiryInterval = time.Second

	service := NewService(urls, &config)
	defer service.close()

	tx := types.NewTransaction(1, common.Address{}, nil, 0, nil, nil)
	err := service.SubmitTransactionForPreconf(tx)
	require.NoError(t, err, "expected no error queuing task")

	// Give some time to process
	time.Sleep(100 * time.Millisecond)

	// Check task was stored
	service.storeMu.RLock()
	_, exists := service.store[tx.Hash()]
	service.storeMu.RUnlock()
	require.True(t, exists, "expected task to be stored after processing")

	// Wait for longer than expiry interval to allow cleanup to run
	time.Sleep(time.Second + 200*time.Millisecond)

	// Check task was deleted
	service.storeMu.RLock()
	_, exists = service.store[tx.Hash()]
	service.storeMu.RUnlock()
	require.False(t, exists, "expected task to be deleted after expiry interval")
}
