package snap

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/syndtr/goleveldb/leveldb"
)

func main() {
	// Create level db connection for healing requests
	Leveldb, err := leveldb.OpenFile("~/.bor/data/bor/HealingRequests.db", nil)
	if err != nil {
		fmt.Println("Failed to open leveldb", err)
	}

	iter := Leveldb.NewIterator(nil, nil)
	var choice1 uint8
	var trienodePending, trienodeTimeout, trienoderejected, trienodeUnexpected, trienodeCompleted uint64
	var bytecodePending, bytecodeTimeout, bytecodeRejected, bytecodeUnexpected, bytecodeCompleted uint64
	var healReq HealingRequest

	fmt.Println("1. Print number of KV pairs")
	fmt.Println("2. Print all request from some specific time, grouped by status and type")
	fmt.Scanln(&choice1)

	switch choice1 {
	case 1:
		for iter.Next() {
			// Remember that the contents of the returned slice should not be modified, and
			// only valid until the next call to Next.
			// fmt.Printf("key=%s, value=%s\n", iter.Key(), iter.Value())
			rlp.DecodeBytes(iter.Value(), &healReq)
			if healReq.taskType == "trienode" {
				switch healReq.status {
				case "pending":
					trienodePending++
				case "timeout":
					trienodeTimeout++
				case "rejected":
					trienoderejected++
				case "unexpected":
					trienodeUnexpected++
				case "completed":
					trienodeCompleted++
				}
			} else {
				switch healReq.status {
				case "pending":
					bytecodePending++
				case "timeout":
					bytecodeTimeout++
				case "rejected":
					bytecodeRejected++
				case "unexpected":
					bytecodeUnexpected++
				case "completed":
					bytecodeCompleted++
				}
			}
		}
		fmt.Println("Total trienode requests", trienodePending+trienodeTimeout+trienoderejected+trienodeUnexpected+trienodeCompleted)
		fmt.Println("Breakdown:", "Pending", trienodePending, "Timeout", trienodeTimeout, "Rejected", trienoderejected, "Unexpected", trienodeUnexpected, "Completed", trienodeCompleted)
		fmt.Println("Total bytecode requests", bytecodePending+bytecodeTimeout+bytecodeRejected+bytecodeUnexpected+bytecodeCompleted)
		fmt.Println("Breakdown:", "Pending", bytecodePending, "Timeout", bytecodeTimeout, "Rejected", bytecodeRejected, "Unexpected", bytecodeUnexpected, "Completed", bytecodeCompleted)
	case 2:
		var start, end int64
		fmt.Println("Enter the start timestamp (unix): ")
		fmt.Scanln(&start)
		fmt.Println("Enter the end   timestamp (unix): ")
		fmt.Scanln(&end)

		if start > end {
			fmt.Println("Start time cannot be greater than end time")
			return
		}

		startTime := time.Unix(start, 0)
		endTIme := time.Unix(end, 0)

		for iter.Next() {
			// Remember that the contents of the returned slice should not be modified, and
			// only valid until the next call to Next.
			// fmt.Printf("key=%s, value=%s\n", iter.Key(), iter.Value())
			rlp.DecodeBytes(iter.Value(), &healReq)
			// check if healReq.start and healReq.end is between startTime and endTime
			if healReq.start.After(startTime) && healReq.end.Before(endTIme) {
				if healReq.taskType == "trienode" {
					switch healReq.status {
					case "pending":
						trienodePending++
					case "timeout":
						trienodeTimeout++
					case "rejected":
						trienoderejected++
					case "unexpected":
						trienodeUnexpected++
					case "completed":
						trienodeCompleted++
					}
				} else {
					switch healReq.status {
					case "pending":
						bytecodePending++
					case "timeout":
						bytecodeTimeout++
					case "rejected":
						bytecodeRejected++
					case "unexpected":
						bytecodeUnexpected++
					case "completed":
						bytecodeCompleted++
					}
				}
			}
		}
		fmt.Println("Total trienode requests", trienodePending+trienodeTimeout+trienoderejected+trienodeUnexpected+trienodeCompleted)
		fmt.Println("Breakdown:", "Pending", trienodePending, "Timeout", trienodeTimeout, "Rejected", trienoderejected, "Unexpected", trienodeUnexpected, "Completed", trienodeCompleted)
		fmt.Println("Total bytecode requests", bytecodePending+bytecodeTimeout+bytecodeRejected+bytecodeUnexpected+bytecodeCompleted)
		fmt.Println("Breakdown:", "Pending", bytecodePending, "Timeout", bytecodeTimeout, "Rejected", bytecodeRejected, "Unexpected", bytecodeUnexpected, "Completed", bytecodeCompleted)
	default:
		fmt.Println("Invalid choice")
	}
	iter.Release()
	err = iter.Error()
	if err != nil {
		fmt.Println("Error in iteration: ", err)
	}
	defer Leveldb.Close()
}
