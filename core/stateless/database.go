// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package stateless

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

// CodeRoutingDB is a database that routes code reads/writes to the diskdb.
type CodeRoutingDB struct {
	ethdb.Database
	diskdb ethdb.Database
}

func NewCodeRoutingDB(diskdb ethdb.Database) *CodeRoutingDB {
	return &CodeRoutingDB{
		Database: rawdb.NewMemoryDatabase(),
		diskdb:   diskdb,
	}
}

func (db *CodeRoutingDB) Get(key []byte) ([]byte, error) {
	if ok, _ := rawdb.IsCodeKey(key); ok {
		return db.diskdb.Get(key)
	}
	return db.Database.Get(key)
}

func (db *CodeRoutingDB) Has(key []byte) (bool, error) {
	if ok, _ := rawdb.IsCodeKey(key); ok {
		return db.diskdb.Has(key)
	}
	return db.Database.Has(key)
}

func (db *CodeRoutingDB) Put(key []byte, value []byte) error {
	if ok, _ := rawdb.IsCodeKey(key); ok {
		return db.diskdb.Put(key, value)
	}
	return db.Database.Put(key, value)
}

func (db *CodeRoutingDB) Delete(key []byte) error {
	if ok, _ := rawdb.IsCodeKey(key); ok {
		return db.diskdb.Delete(key)
	}
	return db.Database.Delete(key)
}

// CodeRoutingBatch is a batch that routes code writes to the diskdb
type CodeRoutingBatch struct {
	ethdb.Batch
	diskdb ethdb.Database
}

func (b *CodeRoutingBatch) Put(key []byte, value []byte) error {
	if ok, _ := rawdb.IsCodeKey(key); ok {
		return b.diskdb.Put(key, value)
	}
	return b.Batch.Put(key, value)
}

func (b *CodeRoutingBatch) Delete(key []byte) error {
	if ok, _ := rawdb.IsCodeKey(key); ok {
		return b.diskdb.Delete(key)
	}
	return b.Batch.Delete(key)
}

func (db *CodeRoutingDB) NewBatch() ethdb.Batch {
	return &CodeRoutingBatch{
		Batch:  db.Database.NewBatch(),
		diskdb: db.diskdb,
	}
}

func (db *CodeRoutingDB) NewBatchWithSize(size int) ethdb.Batch {
	return &CodeRoutingBatch{
		Batch:  db.Database.NewBatchWithSize(size),
		diskdb: db.diskdb,
	}
}

// MakeHashDB imports tries, codes and block hashes from a witness into a new
// hash-based memory db. We could eventually rewrite this into a pathdb, but
// simple is better for now.
//
// Note, this hashdb approach is quite strictly self-validating:
//   - Headers are persisted keyed by hash, so blockhash will error on junk
//   - Codes are persisted keyed by hash, so bytecode lookup will error on junk
//   - Trie nodes are persisted keyed by hash, so trie expansion will error on junk
//
// Acceleration structures built would need to explicitly validate the witness.
func (w *Witness) MakeHashDB(diskdb ethdb.Database) ethdb.Database {
	var (
		routingDB = NewCodeRoutingDB(diskdb)
		hasher    = crypto.NewKeccakState()
		hash      = make([]byte, 32)
	)
	// Inject all the "block hashes" (i.e. headers) into the ephemeral database
	for _, header := range w.Headers {
		rawdb.WriteHeader(routingDB, header)
	}
	// Inject all the MPT trie nodes into the ephemeral database
	for node := range w.State {
		blob := []byte(node)

		hasher.Reset()
		hasher.Write(blob)
		hasher.Read(hash)

		rawdb.WriteLegacyTrieNode(routingDB, common.BytesToHash(hash), blob)
	}
	return routingDB
}
