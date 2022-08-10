package blockstm

import (
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type OpType int

const readType = 0
const writeType = 1
const otherType = 2

type Op struct {
	key      Key
	duration time.Duration
	opType   OpType
}

type testExecTask struct {
	txIdx    int
	ops      []Op
	readMap  map[Key]ReadDescriptor
	writeMap map[Key]WriteDescriptor
	sender   common.Address
}

func NewTestExecTask(txIdx int, ops []Op, sender common.Address) *testExecTask {
	return &testExecTask{
		txIdx:    txIdx,
		ops:      ops,
		readMap:  make(map[Key]ReadDescriptor),
		writeMap: make(map[Key]WriteDescriptor),
		sender:   sender,
	}
}

func sleep(i time.Duration) {
	start := time.Now()
	for time.Since(start) < i {
	}
}

func (t *testExecTask) Execute(mvh *MVHashMap, incarnation int) error {
	version := Version{TxnIndex: t.txIdx, Incarnation: incarnation}

	t.readMap = make(map[Key]ReadDescriptor)
	t.writeMap = make(map[Key]WriteDescriptor)

	for _, op := range t.ops {
		k := op.key

		if op.opType == readType { // nolint:nestif
			if _, ok := t.writeMap[k]; ok {
				sleep(op.duration)
				continue
			}

			result := mvh.Read(k, t.txIdx)

			if result.Status() == MVReadResultDependency {
				return ErrExecAbortError{result.depIdx}
			}

			var readKind int

			if result.Status() == MVReadResultDone {
				readKind = ReadKindMap
			} else if result.Status() == MVReadResultNone {
				readKind = ReadKindStorage
			}

			sleep(op.duration)

			t.readMap[k] = ReadDescriptor{k, readKind, Version{TxnIndex: result.depIdx, Incarnation: result.incarnation}}
		} else if op.opType == writeType {
			t.writeMap[k] = WriteDescriptor{k, version, 1}
		} else {
			sleep(op.duration)
		}
	}

	return nil
}

func (t *testExecTask) MVWriteList() []WriteDescriptor {
	return t.MVFullWriteList()
}

func (t *testExecTask) MVFullWriteList() []WriteDescriptor {
	writes := make([]WriteDescriptor, 0, len(t.writeMap))

	for _, v := range t.writeMap {
		writes = append(writes, v)
	}

	return writes
}

func (t *testExecTask) MVReadList() []ReadDescriptor {
	reads := make([]ReadDescriptor, 0, len(t.readMap))

	for _, v := range t.readMap {
		reads = append(reads, v)
	}

	return reads
}

func (t *testExecTask) Sender() common.Address {
	return t.sender
}

func randTimeGenerator(min time.Duration, max time.Duration) func() time.Duration {
	return func() time.Duration {
		return time.Duration(rand.Int63n(int64(max-min))) + min
	}
}

var randomPathGenerator = func(sender common.Address, j int) Key {
	if j == 0 {
		// First op is always related to nonce
		return NewSubpathKey(sender, 2)
	} else {
		return NewStateKey(sender, common.BigToHash((big.NewInt(int64(j)))))
	}
}

var dexPathGenerator = func(sender common.Address, j int) Key {
	// Randomly interact with one of three contracts
	return NewSubpathKey(common.BigToAddress(big.NewInt(int64(0))), 1)
}

var readTime = randTimeGenerator(4*time.Microsecond, 12*time.Microsecond)
var writeTime = randTimeGenerator(2*time.Microsecond, 6*time.Microsecond)
var nonIOTime = randTimeGenerator(1*time.Microsecond, 2*time.Microsecond)

func taskFactory(numTask int, sender func(int) common.Address, readsPerT int, writesPerT int, nonIOPerT int, pathGenerator func(common.Address, int) Key, readTime func() time.Duration, writeTime func() time.Duration, nonIOTime func() time.Duration) ([]ExecTask, time.Duration) {
	exec := make([]ExecTask, 0, numTask)

	var serialDuration time.Duration

	for i := 0; i < numTask; i++ {
		s := sender(i)
		ops := make([]Op, 0, readsPerT+writesPerT+nonIOPerT)

		for j := 0; j < readsPerT; j++ {
			ops = append(ops, Op{opType: readType})
		}

		for j := 0; j < writesPerT; j++ {
			ops = append(ops, Op{opType: writeType})
		}

		for j := 0; j < nonIOPerT; j++ {
			ops = append(ops, Op{opType: otherType})
		}

		// shuffle ops except for the first read op
		for j := 1; j < len(ops); j++ {
			k := rand.Intn(len(ops)-j) + j
			ops[j], ops[k] = ops[k], ops[j]
		}

		for j := 0; j < len(ops); j++ {
			if ops[j].opType == readType {
				ops[j].key = pathGenerator(s, j)
				ops[j].duration = readTime()
			} else if ops[j].opType == writeType {
				ops[j].key = pathGenerator(s, j)
				ops[j].duration = writeTime()
			} else {
				ops[j].duration = nonIOTime()
			}

			serialDuration += ops[j].duration
		}

		t := NewTestExecTask(i, ops, s)
		exec = append(exec, t)
	}

	return exec, serialDuration
}

func testExecutorComb(t *testing.T, totalTxs []int, numReads []int, numWrites []int, numNonIO []int, taskRunner func(numTx int, numRead int, numWrite int, numNonIO int) (time.Duration, time.Duration)) {
	t.Helper()
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlDebug, log.StreamHandler(os.Stderr, log.TerminalFormat(false))))

	improved := 0
	total := 0

	totalExecDuration := time.Duration(0)
	totalSerialDuration := time.Duration(0)

	for _, numTx := range totalTxs {
		for _, numRead := range numReads {
			for _, numWrite := range numWrites {
				for _, numNonIO := range numNonIO {
					log.Info("Executing block", "numTx", numTx, "numRead", numRead, "numWrite", numWrite, "numNonIO", numNonIO)
					execDuration, expectedSerialDuration := taskRunner(numTx, numRead, numWrite, numNonIO)

					if execDuration < expectedSerialDuration {
						improved++
					}
					total++

					performance := "✅"

					if execDuration >= expectedSerialDuration {
						performance = "❌"
					}

					fmt.Printf("exec duration %v, serial duration %v, time reduced %v %.2f%%, %v \n", execDuration, expectedSerialDuration, expectedSerialDuration-execDuration, float64(expectedSerialDuration-execDuration)/float64(expectedSerialDuration)*100, performance)

					totalExecDuration += execDuration
					totalSerialDuration += expectedSerialDuration
				}
			}
		}
	}

	fmt.Println("Improved: ", improved, "Total: ", total, "success rate: ", float64(improved)/float64(total)*100)
	fmt.Printf("Total exec duration: %v, total serial duration: %v, time reduced: %v, time reduced percent: %.2f%%\n", totalExecDuration, totalSerialDuration, totalSerialDuration-totalExecDuration, float64(totalSerialDuration-totalExecDuration)/float64(totalSerialDuration)*100)
}

func runParallel(t *testing.T, tasks []ExecTask, expectedSerialDuration time.Duration, validation func(TxnInputOutput) bool) time.Duration {
	t.Helper()

	start := time.Now()
	txio, _ := ExecuteParallel(tasks)

	// Need to apply the final write set to storage
	for _, output := range txio.allOutputs {
		for i := 0; i < len(output); i++ {
			sleep(writeTime())
		}
	}

	duration := time.Since(start)

	if validation != nil {
		assert.True(t, validation(*txio))
	}

	return duration
}

func TestLessConflicts(t *testing.T) {
	t.Parallel()

	totalTxs := []int{10, 50, 100, 200, 300}
	numReads := []int{20, 100, 200}
	numWrites := []int{20, 100, 200}
	numNonIO := []int{100, 500}

	taskRunner := func(numTx int, numRead int, numWrite int, numNonIO int) (time.Duration, time.Duration) {
		randomness := rand.Intn(10) + 10
		sender := func(i int) common.Address { return common.BigToAddress(big.NewInt(int64(i % randomness))) }
		tasks, serialDuration := taskFactory(numTx, sender, numRead, numWrite, numNonIO, randomPathGenerator, readTime, writeTime, nonIOTime)

		return runParallel(t, tasks, serialDuration, nil), serialDuration
	}

	testExecutorComb(t, totalTxs, numReads, numWrites, numNonIO, taskRunner)
}

func TestMoreConflicts(t *testing.T) {
	t.Parallel()

	totalTxs := []int{10, 50, 100, 200, 300}
	numReads := []int{20, 100, 200}
	numWrites := []int{20, 100, 200}
	numNonIO := []int{100, 500}

	taskRunner := func(numTx int, numRead int, numWrite int, numNonIO int) (time.Duration, time.Duration) {
		randomness := rand.Intn(10) + 10
		sender := func(i int) common.Address { return common.BigToAddress(big.NewInt(int64(i / randomness))) }
		tasks, serialDuration := taskFactory(numTx, sender, numRead, numWrite, numNonIO, randomPathGenerator, readTime, writeTime, nonIOTime)

		return runParallel(t, tasks, serialDuration, nil), serialDuration
	}

	testExecutorComb(t, totalTxs, numReads, numWrites, numNonIO, taskRunner)
}

func TestRandomTx(t *testing.T) {
	t.Parallel()

	totalTxs := []int{10, 50, 100, 200, 300}
	numReads := []int{20, 100, 200}
	numWrites := []int{20, 100, 200}
	numNonIO := []int{100, 500}

	taskRunner := func(numTx int, numRead int, numWrite int, numNonIO int) (time.Duration, time.Duration) {
		// Randomly assign this tx to one of 10 senders
		sender := func(i int) common.Address { return common.BigToAddress(big.NewInt(int64(rand.Intn(10)))) }
		tasks, serialDuration := taskFactory(numTx, sender, numRead, numWrite, numNonIO, randomPathGenerator, readTime, writeTime, nonIOTime)

		return runParallel(t, tasks, serialDuration, nil), serialDuration
	}

	testExecutorComb(t, totalTxs, numReads, numWrites, numNonIO, taskRunner)
}

func TestDexScenario(t *testing.T) {
	t.Parallel()

	totalTxs := []int{10, 50, 100, 200, 300}
	numReads := []int{20, 100, 200}
	numWrites := []int{20, 100, 200}
	numNonIO := []int{100, 500}

	validation := func(txio TxnInputOutput) bool {
		for i, inputs := range txio.inputs {
			for _, input := range inputs {
				if input.V.TxnIndex != i-1 {
					return false
				}
			}
		}

		return true
	}

	taskRunner := func(numTx int, numRead int, numWrite int, numNonIO int) (time.Duration, time.Duration) {
		sender := func(i int) common.Address { return common.BigToAddress(big.NewInt(int64(i))) }
		tasks, serialDuration := taskFactory(numTx, sender, numRead, numWrite, numNonIO, dexPathGenerator, readTime, writeTime, nonIOTime)

		return runParallel(t, tasks, serialDuration, validation), serialDuration
	}

	testExecutorComb(t, totalTxs, numReads, numWrites, numNonIO, taskRunner)
}
