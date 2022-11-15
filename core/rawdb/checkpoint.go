package rawdb

import (
	"errors"
)

var (
	lastCheckpoint = []byte("LastCheckpoint")

	ErrEmptyLastFinality        = errors.New("empty response while getting last finality")
	ErrIncorrectFinality        = errors.New("last checkpoint in the DB is incorrect")
	ErrIncorrectFinalityToStore = errors.New("failed to marshal the last finality struct")
	ErrDBNotResponding          = errors.New("failed to store the last finality struct")
)

type Checkpoint struct {
	Finality
}
