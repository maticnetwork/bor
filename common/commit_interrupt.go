package common

import "sync/atomic"

// InterruptBlockBuilding is a global flag which will be set to true to notify
// the miner to stop building the current block. It's also used in EVM interpreter
// to interrupt an ongoing transaction execution. It's reset to false when block
// building time of the next block starts.
var InterruptBlockBuilding atomic.Bool
