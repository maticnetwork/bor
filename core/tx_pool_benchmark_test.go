package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
)

// Benchmarks the speed of validating the contents of the pending queue of the
// transaction pool.
func BenchmarkPendingDemotion100(b *testing.B)   { benchmarkPendingDemotion(b, 100) }
func BenchmarkPendingDemotion1000(b *testing.B)  { benchmarkPendingDemotion(b, 1000) }
func BenchmarkPendingDemotion10000(b *testing.B) { benchmarkPendingDemotion(b, 10000) }

func benchmarkPendingDemotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupTxPool()
	defer pool.Stop()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(i), 100000, key)
		pool.promoteTx(account, tx.Hash(), tx)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.demoteUnexecutables()
	}
}

// Benchmarks the speed of scheduling the contents of the future queue of the
// transaction pool.
func BenchmarkFuturePromotion100(b *testing.B)   { benchmarkFuturePromotion(b, 100) }
func BenchmarkFuturePromotion1000(b *testing.B)  { benchmarkFuturePromotion(b, 1000) }
func BenchmarkFuturePromotion10000(b *testing.B) { benchmarkFuturePromotion(b, 10000) }

func benchmarkFuturePromotion(b *testing.B, size int) {
	// Add a batch of transactions to a pool one by one
	pool, key := setupTxPool()
	defer pool.Stop()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	for i := 0; i < size; i++ {
		tx := transaction(uint64(1+i), 100000, key)
		pool.enqueueTx(tx.Hash(), tx, false, true)
	}
	// Benchmark the speed of pool validation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.promoteExecutables(nil)
	}
}

// Benchmarks the speed of batched transaction insertion.
func BenchmarkPoolBatchInsert100(b *testing.B)   { benchmarkPoolBatchInsert(b, 100, false) }
func BenchmarkPoolBatchInsert1000(b *testing.B)  { benchmarkPoolBatchInsert(b, 1000, false) }
func BenchmarkPoolBatchInsert10000(b *testing.B) { benchmarkPoolBatchInsert(b, 10000, false) }

func BenchmarkPoolBatchLocalInsert100(b *testing.B)   { benchmarkPoolBatchInsert(b, 100, true) }
func BenchmarkPoolBatchLocalInsert1000(b *testing.B)  { benchmarkPoolBatchInsert(b, 1000, true) }
func BenchmarkPoolBatchLocalInsert10000(b *testing.B) { benchmarkPoolBatchInsert(b, 10000, true) }

func benchmarkPoolBatchInsert(b *testing.B, size int, local bool) {
	// Generate a batch of transactions to enqueue into the pool
	pool, key := setupTxPool()
	defer pool.Stop()

	account := crypto.PubkeyToAddress(key.PublicKey)
	testAddBalance(pool, account, big.NewInt(1000000))

	batches := make([]types.Transactions, b.N)
	for i := 0; i < b.N; i++ {
		batches[i] = make(types.Transactions, size)
		for j := 0; j < size; j++ {
			batches[i][j] = transaction(uint64(size*i+j), 100000, key)
		}
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for _, batch := range batches {
		if local {
			pool.AddLocals(batch)
		} else {
			pool.AddRemotes(batch)
		}
	}
}

func BenchmarkInsertRemoteWithAllLocals(b *testing.B) {
	// Allocate keys for testing
	key, _ := crypto.GenerateKey()
	account := crypto.PubkeyToAddress(key.PublicKey)

	remoteKey, _ := crypto.GenerateKey()
	remoteAddr := crypto.PubkeyToAddress(remoteKey.PublicKey)

	locals := make([]*types.Transaction, 4096+1024) // Occupy all slots
	for i := 0; i < len(locals); i++ {
		locals[i] = transaction(uint64(i), 100000, key)
	}
	remotes := make([]*types.Transaction, 1000)
	for i := 0; i < len(remotes); i++ {
		remotes[i] = pricedTransaction(uint64(i), 100000, big.NewInt(2), remoteKey) // Higher gasprice
	}
	// Benchmark importing the transactions into the queue
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		pool, _ := setupTxPool()
		testAddBalance(pool, account, big.NewInt(100000000))
		for _, local := range locals {
			pool.AddLocal(local)
		}
		b.StartTimer()
		// Assign a high enough balance for testing
		testAddBalance(pool, remoteAddr, big.NewInt(100000000))
		for i := 0; i < len(remotes); i++ {
			pool.AddRemotes([]*types.Transaction{remotes[i]})
		}
		pool.Stop()
	}
}

// Benchmarks the speed of pool content dumping:
// 1. Variation of both sender number and transactions per sender
func BenchmarkPoolContent100Senders10TxsEach(b *testing.B) { benchmarkPoolContent(b, 100, 10) }
func BenchmarkPoolContent300Senders10TxsEach(b *testing.B) { benchmarkPoolContent(b, 300, 10) }
func BenchmarkPoolContent500Senders10TxsEach(b *testing.B) { benchmarkPoolContent(b, 500, 10) }
func BenchmarkPoolContent10Senders100TxsEach(b *testing.B) { benchmarkPoolContent(b, 10, 100) }
func BenchmarkPoolContent10Senders300TxsEach(b *testing.B) { benchmarkPoolContent(b, 10, 300) }
func BenchmarkPoolContent10Senders500TxsEach(b *testing.B) { benchmarkPoolContent(b, 10, 500) }

// 2. Variation of sender number, each sender sends one transaction
func BenchmarkPoolContent1KSenders1TxEach(b *testing.B) { benchmarkPoolContent(b, 1000, 1) }
func BenchmarkPoolContent3KSenders1TxEach(b *testing.B) { benchmarkPoolContent(b, 3000, 1) }
func BenchmarkPoolContent5KSenders1TxEach(b *testing.B) { benchmarkPoolContent(b, 5000, 1) }

func benchmarkPoolContent(b *testing.B, sendersCount int, txCountPerAddress int) {
	// Setup tx pool
	pool, _ := setupTxPool()
	defer pool.Stop()

	// Generate a transactions and add them into the pool
	txs := make([]*types.Transaction, 0, sendersCount*txCountPerAddress)
	for i := 0; i < sendersCount; i++ {
		// Generate and seed sender
		senderKey, _ := createAndSeedSender(pool, big.NewInt(int64(txCountPerAddress)*1000000))

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes txCountPerAddress transactions.
		for j := 0; j < txCountPerAddress; j++ {
			// Create a transaction with some address and add it to slice
			tx := transaction(uint64(j), 100000, senderKey)
			txs = append(txs, tx)
		}
	}
	pool.AddRemotesSync(txs)

	b.ResetTimer()
	// Benchmark TxPool.Content()
	for i := 0; i < b.N; i++ {
		pool.Content()
	}
}

// Benchmark synchronized adding of transactions to pool
func BenchmarkAddRemotes1KSenders(b *testing.B)   { benchmarkAddRemotes(b, 1000, 1) }
func BenchmarkAddRemotes5KSenders(b *testing.B)   { benchmarkAddRemotes(b, 5000, 1) }
func BenchmarkAddRemotes10KSenders(b *testing.B)  { benchmarkAddRemotes(b, 10000, 1) }
func BenchmarkAddRemotes50KSenders(b *testing.B)  { benchmarkAddRemotes(b, 50000, 1) }
func BenchmarkAddRemotes100KSenders(b *testing.B) { benchmarkAddRemotes(b, 100000, 1) }

// TODO: Commented, because working quite slow due to transaction signing.
// Implement persisting transactions to json and loading them when running tests.
// func BenchmarkAddRemotes10KSenders50TxsAscending(b *testing.B) { benchmarkAddRemotes(b, 10000, 50) }
// func BenchmarkAddRemotes50KSenders50TxsAscending(b *testing.B)  { benchmarkAddRemotes(b, 50000, 50) }
// func BenchmarkAddRemotes100KSenders50TxsAscending(b *testing.B) { benchmarkAddRemotes(b, 100000, 50) }

func benchmarkAddRemotes(b *testing.B, sendersCount, txCountPerSender int) {
	// Setup tx pool
	config := testTxPoolConfig
	config.GlobalSlots = uint64(sendersCount)
	config.AccountSlots = uint64(txCountPerSender)

	statedb, _ := state.New(common.Hash{}, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	blockchain := &testBlockChain{1000000, statedb, new(event.Feed)}

	pool := NewTxPool(config, params.TestChainConfig, blockchain)
	defer pool.Stop()

	txs := make([]*types.Transaction, 0, sendersCount)
	accounts := make([]*common.Address, 0, sendersCount)
	for i := 0; i < sendersCount; i++ {
		// Generate and seed sender
		senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(1000000))
		if senderKeyError != nil {
			b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
		}

		for j := 0; j < txCountPerSender; j++ {
			// Create transaction
			tx := transaction(uint64(j), 100000, senderKey)
			txs = append(txs, tx)
		}

		acc := crypto.PubkeyToAddress(senderKey.PublicKey)
		accounts = append(accounts, &acc)
	}

	b.ResetTimer()
	// Benchmark TxPool.AddRemotesSync()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		pool.AddRemotesSync(txs)
		b.StopTimer()

		if err := validateTxPoolInternals(pool); err != nil {
			b.Fatalf("Pool is in incosistent state %v", err)
		}
		pool.mu.Lock()
		// Clean up pool in order to be prepared for the next iteration
		for j := 0; j < len(accounts); j++ {
			pendingTxs := pool.pending[*accounts[j]]
			queuedTxs := pool.queue[*accounts[j]]
			if pendingTxs != nil {
				for _, tx := range pendingTxs.Flatten() {
					pool.removeTx(tx.Hash(), true)
				}
			}
			if queuedTxs != nil {
				for _, tx := range queuedTxs.Flatten() {
					pool.removeTx(tx.Hash(), true)
				}
			}
		}
		pool.mu.Unlock()
	}
}

func BenchmarkPromoteExecutables1000Senders1TxEach(b *testing.B) {
	benchmarkPromoteExecutables(b, 1000, 1)
}
func BenchmarkPromoteExecutables100Senders1TxEach(b *testing.B) {
	benchmarkPromoteExecutables(b, 100, 1)
}
func BenchmarkPromoteExecutables10Senders1TxEach(b *testing.B) { benchmarkPromoteExecutables(b, 10, 1) }
func BenchmarkPromoteExecutables1Senders1TxEach(b *testing.B)  { benchmarkPromoteExecutables(b, 1, 1) }
func BenchmarkPromoteExecutables1Senders10TxEach(b *testing.B) { benchmarkPromoteExecutables(b, 1, 10) }
func BenchmarkPromoteExecutables1Senders64TxEach(b *testing.B) { benchmarkPromoteExecutables(b, 1, 64) }

func benchmarkPromoteExecutables(b *testing.B, senderCount int, txCountPerAddress int) {
	// Setup tx pool
	pool, _ := setupTxPool()
	defer pool.Stop()

	var txGasLimit uint64 = 100000
	var extraGas int64 = 1000000
	accounts := make([]common.Address, 0, senderCount)

	// Generate a transactions and add them into the pool
	txs := make([]*types.Transaction, 0, senderCount*txCountPerAddress)
	for i := 0; i < senderCount; i++ {
		// Generate and seed sender
		senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(int64(txCountPerAddress)*int64(txGasLimit)+extraGas))
		if senderKeyError != nil {
			b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
		}

		senderAccount := crypto.PubkeyToAddress(senderKey.PublicKey)
		accounts = append(accounts, senderAccount)

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes txCountPerAddress transactions.
		for j := 0; j < txCountPerAddress; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.queue since nonce doesn't start with 0
			tx := transaction(uint64(j+1), txGasLimit, senderKey)
			txs = append(txs, tx)
		}
	}

	pool.AddRemotesSync(txs)

	b.Logf("Benchmark promoteExecutables for b.N: %v", b.N)
	b.ResetTimer()
	b.StopTimer()
	// Benchmark promoteExecutables
	for i := 0; i < b.N; i++ {

		// Mark pendingNonces for all accounts to 1 so that all transactions will be promoted
		// since their nonces started with 1
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], 1)
		}

		b.StartTimer()
		pool.promoteExecutables(accounts)
		b.StopTimer()

		// Mark pendingNonces for all accounts to 0 so that all transactions will be demoted
		// since their nonces started with 1
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], 0)
		}
		pool.demoteUnexecutables()
	}
}

func BenchmarkDemoteUnexecutables1000Senders1TxEach(b *testing.B) {
	benchmarkDemoteUnexecutables(b, 1000, 1)
}
func BenchmarkDemoteUnexecutables100Senders1TxEach(b *testing.B) {
	benchmarkDemoteUnexecutables(b, 100, 1)
}
func BenchmarkDemoteUnexecutables10Senders1TxEach(b *testing.B) {
	benchmarkDemoteUnexecutables(b, 10, 1)
}
func BenchmarkDemoteUnexecutables1Senders1TxEach(b *testing.B) { benchmarkDemoteUnexecutables(b, 1, 1) }
func BenchmarkDemoteUnexecutables1Senders10TxEach(b *testing.B) {
	benchmarkDemoteUnexecutables(b, 1, 10)
}
func BenchmarkDemoteUnexecutables1Senders100TxEach(b *testing.B) {
	benchmarkDemoteUnexecutables(b, 1, 100)
}
func BenchmarkDemoteUnexecutables1Senders1000TxEach(b *testing.B) {
	benchmarkDemoteUnexecutables(b, 1, 1000)
}

func benchmarkDemoteUnexecutables(b *testing.B, senderCount int, txCountPerAddress int) {
	// Setup tx pool
	pool, _ := setupTxPool()
	defer pool.Stop()

	var txGasLimit uint64 = 100000
	var extraGas int64 = 1000000
	accounts := make([]common.Address, 0, senderCount)

	// Generate a transactions and add them into the pool
	txs := make([]*types.Transaction, 0, senderCount*txCountPerAddress)
	for i := 0; i < senderCount; i++ {
		// Generate and seed sender
		senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(int64(txCountPerAddress)*int64(txGasLimit)+extraGas))
		if senderKeyError != nil {
			b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
		}

		senderAccount := crypto.PubkeyToAddress(senderKey.PublicKey)
		accounts = append(accounts, senderAccount)

		// set pending nonces to 1, so we can set them to 0 after the transactions are added to pending,
		// in order to make them invalid, and demote them to pool.queue
		pool.pendingNonces.set(senderAccount, 1)

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes txCountPerAddress transactions.
		for j := 0; j < txCountPerAddress; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.pending since we set pendingNonces to 1
			tx := transaction(uint64(j+1), txGasLimit, senderKey)
			txs = append(txs, tx)
		}
	}
	pool.AddRemotesSync(txs)

	b.Logf("Benchmark demoteUnexecutables for b.N: %v", b.N)
	b.ResetTimer()
	b.StopTimer()
	// Benchmark demoteUnexecutables
	for i := 0; i < b.N; i++ {

		// Mark pendingNonces for all accounts to 0 so that all transactions will be demoted
		// since their nonces started with 1
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], 0)
		}

		b.StartTimer()
		pool.demoteUnexecutables()
		b.StopTimer()

		// Mark pendingNonces for all accounts to 1 so that all transactions will be promoted
		// since their nonces started with 1
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], 1)
		}
		pool.promoteExecutables(accounts)
	}
}

func BenchmarkTruncatePending100Senders1TxEach(b *testing.B) { benchmarkTruncatePending(b, 100, 1) }
func BenchmarkTruncatePending10Senders1TxEach(b *testing.B)  { benchmarkTruncatePending(b, 10, 1) }
func BenchmarkTruncatePending1Senders1TxEach(b *testing.B)   { benchmarkTruncatePending(b, 1, 1) }
func BenchmarkTruncatePending1Senders10TxEach(b *testing.B)  { benchmarkTruncatePending(b, 1, 10) }
func BenchmarkTruncatePending1Senders64TxEach(b *testing.B)  { benchmarkTruncatePending(b, 1, 64) }

func benchmarkTruncatePending(b *testing.B, senderCount int, overflowingTxCountPerAddress int) {
	// Setup tx pool
	pool, _ := setupTxPool()
	defer pool.Stop()

	txCount := int(int(testTxPoolConfig.GlobalSlots)/senderCount) + 1
	var txGasLimit uint64 = 100000
	var extraGas int64 = 1000000
	accounts := make([]common.Address, 0, senderCount)

	// Generate a transactions and add them into the pool
	txs := make([]*types.Transaction, 0, senderCount*txCount)
	overflowingTxs := make([]*types.Transaction, 0, senderCount*overflowingTxCountPerAddress)
	for i := 0; i < senderCount; i++ {
		// Generate and seed sender
		senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(int64(overflowingTxCountPerAddress+txCount)*int64(txGasLimit)+extraGas))
		if senderKeyError != nil {
			b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
		}

		senderAccount := crypto.PubkeyToAddress(senderKey.PublicKey)
		accounts = append(accounts, senderAccount)

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes txCount + overflowingTxCountPerAddress transactions.
		for j := 0; j < txCount; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.pending since nonce starts with 0
			tx := transaction(uint64(j), txGasLimit, senderKey)
			txs = append(txs, tx)
		}

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes txCount + overflowingTxCountPerAddress transactions.
		for j := 0; j < overflowingTxCountPerAddress; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.queue since nonce starts with txCount + 1
			tx := transaction(uint64(txCount+j+1), txGasLimit, senderKey)
			overflowingTxs = append(overflowingTxs, tx)
		}
	}

	pool.AddRemotesSync(txs)

	b.Logf("Benchmark truncatePending for b.N: %v", b.N)
	b.ResetTimer()
	b.StopTimer()
	// Benchmark truncatePending
	for i := 0; i < b.N; i++ {

		pool.AddRemotesSync(overflowingTxs)
		// Mark pendingNonces for all accounts to txCount+1 so that all overflowing transactions will be promoted
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], uint64(txCount+1))
		}
		pool.promoteExecutables(accounts)

		b.StartTimer()
		pool.truncatePending()
		b.StopTimer()

		// Mark pendingNonces for all accounts to txCount so that all overflowing transactions will be demoted
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], uint64(txCount))
		}
	}
}

func BenchmarkTruncateQueue1Tx(b *testing.B)    { benchmarkTruncateQueue(b, 1) }
func BenchmarkTruncateQueue10Tx(b *testing.B)   { benchmarkTruncateQueue(b, 10) }
func BenchmarkTruncateQueue100Tx(b *testing.B)  { benchmarkTruncateQueue(b, 100) }
func BenchmarkTruncateQueue1000Tx(b *testing.B) { benchmarkTruncateQueue(b, 1000) }

func benchmarkTruncateQueue(b *testing.B, overflowingTxCountPerAddress int) {
	txCount := int(testTxPoolConfig.GlobalQueue)

	// Setup tx pool
	pool, _ := setupTxPool()
	pool.config.AccountQueue = uint64(txCount)
	pool.config.AccountSlots = uint64(1)
	defer pool.Stop()

	var txGasLimit uint64 = 100000
	var extraGas int64 = 1000000

	// Generate a transactions and add them into the pool
	txs := make([]*types.Transaction, 0, txCount)
	overflowingTxs := make([]*types.Transaction, 0, overflowingTxCountPerAddress)
	// Generate and seed sender
	senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(int64(overflowingTxCountPerAddress+txCount)*int64(txGasLimit)+extraGas))
	if senderKeyError != nil {
		b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
	}

	account := crypto.PubkeyToAddress(senderKey.PublicKey)
	accounts := make([]common.Address, 0, 1)
	accounts = append(accounts, account)

	// Mark pendingNonces for all accounts to 1 so that all transactions will be in pending
	pool.pendingNonces.set(account, uint64(1))

	// Create transactions. Each transaction within loop will be generated with same sender key.
	// We are simulating situation where each sender makes txCount + overflowingTxCountPerAddress transactions.
	for j := 0; j < txCount; j++ {
		// Create a transaction with some address and add it to slice
		// will be added to pool.pending since nonce starts with 1
		tx := transaction(uint64(j+1), txGasLimit, senderKey)
		txs = append(txs, tx)
	}

	// Create transactions. Each transaction within loop will be generated with same sender key.
	// We are simulating situation where each sender makes txCount + overflowingTxCountPerAddress transactions.
	for j := 0; j < overflowingTxCountPerAddress; j++ {
		// Create a transaction with some address and add it to slice
		// will be added to pool.pending since nonce starts with txCount + 1
		tx := transaction(uint64(txCount+j+1), txGasLimit, senderKey)
		overflowingTxs = append(overflowingTxs, tx)
	}

	pool.AddRemotesSync(txs)

	b.Logf("Benchmark truncateQueue for b.N: %v", b.N)
	b.ResetTimer()
	b.StopTimer()
	// Benchmark truncateQueue
	for i := 0; i < b.N; i++ {
		pool.AddRemotesSync(overflowingTxs)

		// Mark pendingNonces for all accounts to 0 so that all transactions will be demoted
		pool.pendingNonces.set(account, uint64(0))

		pool.demoteUnexecutables()

		b.StartTimer()
		pool.truncateQueue()
		b.StopTimer()

		// Mark pendingNonces for all accounts to 1 so that all transactions will be in pending
		pool.pendingNonces.set(account, uint64(1))

		pool.promoteExecutables(accounts)
	}
}

func BenchmarkRunReorg100Senders1TxEach(b *testing.B) { benchmarkRunReorg(b, 100, 1) }
func BenchmarkRunReorg10Senders1TxEach(b *testing.B)  { benchmarkRunReorg(b, 10, 1) }
func BenchmarkRunReorg1Senders1TxEach(b *testing.B)   { benchmarkRunReorg(b, 1, 1) }
func BenchmarkRunReorg1Senders10TxEach(b *testing.B)  { benchmarkRunReorg(b, 1, 10) }
func BenchmarkRunReorg1Senders100TxEach(b *testing.B) { benchmarkRunReorg(b, 1, 100) }

func benchmarkRunReorg(b *testing.B, senderCount int, overflowingTxCountPerAddress int) {
	// Setup tx pool
	pool, _ := setupTxPool()
	defer pool.Stop()

	var txGasLimit uint64 = 100000
	var extraGas int64 = 1000000
	accounts := make([]common.Address, 0, senderCount)
	pendingTxCount := int(int(testTxPoolConfig.GlobalSlots)/senderCount) + 1

	// Generate a transactions and add them into the pool
	pendingTxs := make([]*types.Transaction, 0, senderCount*pendingTxCount)
	overflowingTxs := make([]*types.Transaction, 0, senderCount*overflowingTxCountPerAddress)
	for i := 0; i < senderCount; i++ {
		// Generate and seed sender
		senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(int64(overflowingTxCountPerAddress+pendingTxCount)*int64(txGasLimit)+extraGas))
		if senderKeyError != nil {
			b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
		}

		senderAccount := crypto.PubkeyToAddress(senderKey.PublicKey)
		accounts = append(accounts, senderAccount)

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes pendingTxCount + overflowingTxCountPerAddress transactions.
		for j := 0; j < pendingTxCount; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.pending since nonce starts with 0
			tx := transaction(uint64(j), txGasLimit, senderKey)
			pendingTxs = append(pendingTxs, tx)
		}

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes pendingTxCount + overflowingTxCountPerAddress transactions.
		for j := 0; j < overflowingTxCountPerAddress; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.queue since nonce starts with txCount + 1
			tx := transaction(uint64(pendingTxCount+j+1), txGasLimit, senderKey)
			overflowingTxs = append(overflowingTxs, tx)
		}
	}

	dirtyAccounts := newAccountSet(pool.signer, accounts...)
	queuedEvents := make(map[common.Address]*txSortedMap)
	var reset *txpoolResetRequest = nil
	pool.AddRemotesSync(pendingTxs)

	b.Logf("Benchmark runReorg for b.N: %v", b.N)
	b.ResetTimer()
	b.StopTimer()
	// Benchmark runReorg
	for i := 0; i < b.N; i++ {

		pool.AddRemotesSync(overflowingTxs)

		// Mark pendingNonces for all accounts to 1 so that all transactions will be promoted
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], uint64(pendingTxCount+1))
		}

		done := make(chan struct{})

		b.StartTimer()
		pool.runReorg(done, reset, dirtyAccounts, queuedEvents)
		b.StopTimer()

		// Mark pendingNonces for all accounts to 0 so that all transactions will be demoted
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], uint64(pendingTxCount))
		}
	}
}

func BenchmarkRunReorgWithReset100Senders1TxEach(b *testing.B) { benchmarkRunReorgWithReset(b, 100, 1) }
func BenchmarkRunReorgWithReset10Senders1TxEach(b *testing.B)  { benchmarkRunReorgWithReset(b, 10, 1) }
func BenchmarkRunReorgWithReset1Senders1TxEach(b *testing.B)   { benchmarkRunReorgWithReset(b, 1, 1) }
func BenchmarkRunReorgWithReset1Senders10TxEach(b *testing.B)  { benchmarkRunReorgWithReset(b, 1, 10) }
func BenchmarkRunReorgWithReset1Senders100TxEach(b *testing.B) { benchmarkRunReorgWithReset(b, 1, 100) }
func BenchmarkRunReorgWithReset1Senders1000TxEach(b *testing.B) {
	benchmarkRunReorgWithReset(b, 1, 1000)
}
func BenchmarkRunReorgWithReset1Senders5000TxEach(b *testing.B) {
	benchmarkRunReorgWithReset(b, 1, 5000)
}

func benchmarkRunReorgWithReset(b *testing.B, senderCount int, txCountPerAddress int) {
	// Setup tx pool
	pool, _ := setupTxPool()
	defer pool.Stop()

	var txGasLimit uint64 = 100000
	var extraGas int64 = 1000000
	accounts := make([]common.Address, 0, senderCount)

	// Generate a transactions and add them into the pool
	txs := make([]*types.Transaction, 0, senderCount*txCountPerAddress)
	queuedEvents := make(map[common.Address]*txSortedMap)
	for i := 0; i < senderCount; i++ {
		// Generate and seed sender
		senderKey, senderKeyError := createAndSeedSender(pool, big.NewInt(int64(txCountPerAddress)*int64(txGasLimit)+extraGas))
		if senderKeyError != nil {
			b.Fatalf("Failed to generate sender private key and to seed it to the tx pool")
		}

		senderAccount := crypto.PubkeyToAddress(senderKey.PublicKey)
		accounts = append(accounts, senderAccount)
		queuedEvents[senderAccount] = newTxSortedMap()

		// Create transactions. Each transaction within loop will be generated with same sender key.
		// We are simulating situation where each sender makes txCountPerAddress transactions.
		for j := 0; j < txCountPerAddress; j++ {
			// Create a transaction with some address and add it to slice
			// will be added to pool.queue since nonce starts with 1
			tx := transaction(uint64(j+1), txGasLimit, senderKey)
			txs = append(txs, tx)
			queuedEvents[senderAccount].Put(tx)
		}
	}

	reset := &txpoolResetRequest{nil, pool.chain.CurrentBlock().Header()}
	dirtyAccounts := newAccountSet(pool.signer, accounts...)

	b.Logf("Benchmark runReorg for b.N: %v", b.N)
	b.ResetTimer()
	b.StopTimer()
	// Benchmark runReorg
	for i := 0; i < b.N; i++ {
		// Mark pendingNonces for all accounts to 1 so that all transactions will be in pending
		for j := 0; j < len(accounts); j++ {
			pool.pendingNonces.set(accounts[j], uint64(1))
		}

		pool.AddRemotesSync(txs)

		done := make(chan struct{})

		b.StartTimer()
		pool.runReorg(done, reset, dirtyAccounts, queuedEvents)
		b.StopTimer()

		for j := 0; j < len(accounts); j++ {
			for _, tx := range pool.queue[accounts[j]].Flatten() {
				pool.removeTx(tx.Hash(), true)
			}
		}
	}
}
