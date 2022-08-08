package blockstm

import (
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type ExecResult struct {
	err      error
	ver      Version
	txIn     TxnInput
	txOut    TxnOutput
	txAllOut TxnOutput
}

type ExecTask interface {
	Execute(mvh *MVHashMap, incarnation int) error
	MVReadList() []ReadDescriptor
	MVWriteList() []WriteDescriptor
	MVFullWriteList() []WriteDescriptor
	Sender() common.Address
}

type ExecVersionView struct {
	ver    Version
	et     ExecTask
	mvh    *MVHashMap
	sender common.Address
}

func (ev *ExecVersionView) Execute() (er ExecResult) {
	er.ver = ev.ver
	if er.err = ev.et.Execute(ev.mvh, ev.ver.Incarnation); er.err != nil {
		return
	}

	er.txIn = ev.et.MVReadList()
	er.txOut = ev.et.MVWriteList()
	er.txAllOut = ev.et.MVFullWriteList()

	return
}

type ErrExecAbortError struct {
	DependencyIndices []int
}

func (e ErrExecAbortError) Error() string {
	if len(e.DependencyIndices) > 0 {
		return fmt.Sprintf("Execution aborted due to dependency %d", e.DependencyIndices)
	} else {
		return "Execution aborted"
	}
}

const numGoProcs = 10

// nolint: gocognit
func ExecuteParallel(tasks []ExecTask) (lastTxIO *TxnInputOutput, err error) {
	if len(tasks) == 0 {
		return MakeTxnInputOutput(len(tasks)), nil
	}

	chTasks := make(chan ExecVersionView, len(tasks))
	chSpeculativeTasks := make(chan ExecVersionView, len(tasks))
	chResults := make(chan ExecResult, len(tasks))
	chSpeculativeResults := make(chan ExecResult, len(tasks))
	mutMap := map[common.Address]*sync.RWMutex{}

	for _, t := range tasks {
		if _, ok := mutMap[t.Sender()]; !ok {
			mutMap[t.Sender()] = &sync.RWMutex{}
		}
	}

	var cntExec, cntSuccess, cntAbort, cntTotalValidations, cntValidationFail int

	mvh := MakeMVHashMap()

	for i := 0; i < numGoProcs; i++ {
		go func(procNum int) {
			if procNum < numGoProcs/2 { // nolint: nestif
				for task := range chSpeculativeTasks {
					m := mutMap[task.sender]
					if !m.TryLock() {
						chSpeculativeTasks <- task
					} else {
						res := task.Execute()
						if res.err == nil {
							mvh.FlushMVWriteSet(res.txAllOut)
						}
						chSpeculativeResults <- res
						m.Unlock()
					}
				}
			} else {
				for task := range chTasks {
					res := task.Execute()
					if res.err == nil {
						mvh.FlushMVWriteSet(res.txAllOut)
					}
					chResults <- res
				}
			}
		}(i)
	}

	execTasks := makeStatusManager(len(tasks))
	validateTasks := makeStatusManager(0)

	// bootstrap execution

	for x := 0; x < numGoProcs/2; x++ {
		tx := execTasks.takeNextPending()
		if tx != -1 {
			cntExec++

			chSpeculativeTasks <- ExecVersionView{ver: Version{tx, 0}, et: tasks[tx], mvh: mvh, sender: tasks[tx].Sender()}
		}
	}

	lastTxIO = MakeTxnInputOutput(len(tasks))
	txIncarnations := make([]int, len(tasks))

	diagExecSuccess := make([]int, len(tasks))
	diagExecAbort := make([]int, len(tasks))

	estimateDeps := make(map[int][]int, len(tasks))

	for i := 0; i < len(tasks); i++ {
		estimateDeps[i] = make([]int, 0)
	}

	for {
		var res ExecResult
		if len(chResults) > 0 {
			res = <-chResults
		} else if len(chSpeculativeResults) > 0 {
			res = <-chSpeculativeResults
		} else {
			continue
		}

		if res.err == nil { //nolint:nestif
			lastTxIO.recordRead(res.ver.TxnIndex, res.txIn)

			if res.ver.Incarnation == 0 {
				lastTxIO.recordWrite(res.ver.TxnIndex, res.txOut)
				lastTxIO.recordAllWrite(res.ver.TxnIndex, res.txAllOut)
			} else {
				if res.txAllOut.hasNewWrite(lastTxIO.AllWriteSet(res.ver.TxnIndex)) {
					validateTasks.pushPendingSet(execTasks.getRevalidationRange(res.ver.TxnIndex + 1))
				}

				prevWrite := lastTxIO.AllWriteSet(res.ver.TxnIndex)

				// Remove entries that were previously written but are no longer written

				cmpMap := make(map[Key]bool)

				for _, w := range res.txAllOut {
					cmpMap[w.Path] = true
				}

				for _, v := range prevWrite {
					if _, ok := cmpMap[v.Path]; !ok {
						mvh.Delete(v.Path, res.ver.TxnIndex)
					}
				}

				lastTxIO.recordWrite(res.ver.TxnIndex, res.txOut)
				lastTxIO.recordAllWrite(res.ver.TxnIndex, res.txAllOut)
			}

			validateTasks.pushPending(res.ver.TxnIndex)
			execTasks.markComplete(res.ver.TxnIndex)
			diagExecSuccess[res.ver.TxnIndex]++
			cntSuccess++

			execTasks.removeDependency(res.ver.TxnIndex)
		} else if execErr, ok := res.err.(ErrExecAbortError); ok {

			addedDependencies := false

			if len(execErr.DependencyIndices) > 0 {
				for _, dep := range execErr.DependencyIndices {
					l := len(estimateDeps[res.ver.TxnIndex])
					for l > 0 && estimateDeps[res.ver.TxnIndex][l-1] > dep {
						execTasks.removeDependency(estimateDeps[res.ver.TxnIndex][l-1])
						estimateDeps[res.ver.TxnIndex] = estimateDeps[res.ver.TxnIndex][:l-1]
						l--
					}
				}
				addedDependencies = execTasks.addDependencies(execErr.DependencyIndices, res.ver.TxnIndex)
			} else if len(execTasks.blockCount[res.ver.TxnIndex]) == 0 {
				estimate := 0

				if len(estimateDeps[res.ver.TxnIndex]) > 0 {
					estimate = estimateDeps[res.ver.TxnIndex][len(estimateDeps[res.ver.TxnIndex])-1]
				}
				addedDependencies = execTasks.addDependencies([]int{estimate}, res.ver.TxnIndex)
				newEstimate := estimate + 1
				if newEstimate >= res.ver.TxnIndex {
					newEstimate = res.ver.TxnIndex - 1
				}
				estimateDeps[res.ver.TxnIndex] = append(estimateDeps[res.ver.TxnIndex], newEstimate)
			}

			execTasks.clearInProgress(res.ver.TxnIndex)
			if !addedDependencies {
				execTasks.pushPending(res.ver.TxnIndex)
			}
			txIncarnations[res.ver.TxnIndex]++
			diagExecAbort[res.ver.TxnIndex]++
			cntAbort++
		} else {
			err = res.err
			break
		}

		// do validations ...
		maxComplete := execTasks.maxAllComplete()

		var toValidate []int

		for validateTasks.minPending() <= maxComplete && validateTasks.minPending() >= 0 {
			toValidate = append(toValidate, validateTasks.takeNextPending())
		}

		for i := 0; i < len(toValidate); i++ {
			cntTotalValidations++

			tx := toValidate[i]

			if ValidateVersion(tx, lastTxIO, mvh) {
				validateTasks.markComplete(tx)
			} else {
				cntValidationFail++
				diagExecAbort[tx]++
				for _, v := range lastTxIO.AllWriteSet(tx) {
					mvh.MarkEstimate(v.Path, tx)
				}
				// 'create validation tasks for all transactions > tx ...'
				validateTasks.pushPendingSet(execTasks.getRevalidationRange(tx + 1))
				validateTasks.clearInProgress(tx) // clear in progress - pending will be added again once new incarnation executes
				if execTasks.checkPending(tx) {
					// println() // have to think about this ...
				} else {
					execTasks.pushPending(tx)
					execTasks.clearComplete(tx)
					txIncarnations[tx]++
				}
			}
		}

		maxValidated := validateTasks.maxAllComplete()

		// if we didn't queue work previously, do check again so we keep making progress ...
		if execTasks.minPending() != -1 && execTasks.minPending() <= maxValidated+10 {
			nextTx := execTasks.takeNextPending()
			if nextTx != -1 {
				cntExec++

				chTasks <- ExecVersionView{ver: Version{nextTx, txIncarnations[nextTx]}, et: tasks[nextTx], mvh: mvh, sender: tasks[nextTx].Sender()}
			}
		}

		for execTasks.minPending() != -1 && len(chSpeculativeTasks) < numGoProcs/2 {
			nextTx := execTasks.takeNextPending()
			if nextTx != -1 {
				cntExec++

				chSpeculativeTasks <- ExecVersionView{ver: Version{nextTx, txIncarnations[nextTx]}, et: tasks[nextTx], mvh: mvh, sender: tasks[nextTx].Sender()}
			}
		}

		if validateTasks.countComplete() == len(tasks) && execTasks.countComplete() == len(tasks) {
			log.Debug("blockstm exec summary", "execs", cntExec, "success", cntSuccess, "aborts", cntAbort, "validations", cntTotalValidations, "failures", cntValidationFail, "#tasks/#execs", fmt.Sprintf("%.2f%%", float64(len(tasks))/float64(cntExec)*100))
			break
		}
	}

	close(chTasks)
	close(chSpeculativeTasks)
	close(chResults)
	close(chSpeculativeResults)

	return
}
