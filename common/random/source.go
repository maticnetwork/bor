package random

import (
	"crypto/rand"
	"encoding/binary"
)

type CryptoSource struct{}

func (CryptoSource) Seed(_ int64) {}

func (s CryptoSource) Int63() int64 {
	return int64(s.Uint64() & ^uint64(1<<63))
}

func (CryptoSource) Uint64() (v uint64) {
	_ = binary.Read(rand.Reader, binary.BigEndian, &v)

	return v
}
