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
	"bytes"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// codeKey returns the database key for contract code using the same
// format as rawdb (CodePrefix + hash)
func makeCodeKey(hash common.Hash) []byte {
	// CodePrefix is "c" as defined in rawdb/schema.go
	return append([]byte("c"), hash.Bytes()...)
}

// TestCodeRoutingDB tests that CodeRoutingDB correctly routes code operations to diskdb
func TestCodeRoutingDB(t *testing.T) {
	diskdb := rawdb.NewMemoryDatabase()
	routingDB := NewCodeRoutingDB(diskdb)

	// Test data
	codeHash := crypto.Keccak256Hash([]byte("test code"))
	codeKey := makeCodeKey(codeHash)
	codeData := []byte("contract bytecode")

	nonCodeKey := []byte("not-a-code-key")
	nonCodeData := []byte("some other data")

	// Test Put operations
	if err := routingDB.Put(codeKey, codeData); err != nil {
		t.Fatalf("Failed to put code: %v", err)
	}
	if err := routingDB.Put(nonCodeKey, nonCodeData); err != nil {
		t.Fatalf("Failed to put non-code data: %v", err)
	}

	// Verify code was stored in diskdb
	if data, err := diskdb.Get(codeKey); err != nil {
		t.Fatalf("Code not found in diskdb: %v", err)
	} else if !bytes.Equal(data, codeData) {
		t.Fatalf("Code data mismatch in diskdb: got %v, want %v", data, codeData)
	}

	// Verify non-code was stored in memory db
	if _, err := diskdb.Get(nonCodeKey); err == nil {
		t.Fatalf("Non-code data should not be in diskdb")
	}

	// Test Get operations
	if data, err := routingDB.Get(codeKey); err != nil {
		t.Fatalf("Failed to get code: %v", err)
	} else if !bytes.Equal(data, codeData) {
		t.Fatalf("Code data mismatch: got %v, want %v", data, codeData)
	}

	if data, err := routingDB.Get(nonCodeKey); err != nil {
		t.Fatalf("Failed to get non-code data: %v", err)
	} else if !bytes.Equal(data, nonCodeData) {
		t.Fatalf("Non-code data mismatch: got %v, want %v", data, nonCodeData)
	}

	// Test Has operations
	if has, err := routingDB.Has(codeKey); err != nil || !has {
		t.Fatalf("Code should exist: err=%v, has=%v", err, has)
	}
	if has, err := routingDB.Has(nonCodeKey); err != nil || !has {
		t.Fatalf("Non-code data should exist: err=%v, has=%v", err, has)
	}

	// Test Delete operations
	if err := routingDB.Delete(codeKey); err != nil {
		t.Fatalf("Failed to delete code: %v", err)
	}
	if has, err := diskdb.Has(codeKey); err != nil || has {
		t.Fatalf("Code should be deleted from diskdb: err=%v, has=%v", err, has)
	}
}

// TestCodeRoutingBatch tests that batch operations correctly route to diskdb
func TestCodeRoutingBatch(t *testing.T) {
	diskdb := rawdb.NewMemoryDatabase()
	routingDB := NewCodeRoutingDB(diskdb)

	// Test batch operations
	batch := routingDB.NewBatch()

	// Add multiple code and non-code entries
	codes := make(map[common.Hash][]byte)
	nonCodes := make(map[string][]byte)

	for i := 0; i < 5; i++ {
		// Code entries
		code := []byte{byte(i), byte(i + 1), byte(i + 2)}
		hash := crypto.Keccak256Hash(code)
		codes[hash] = code
		batch.Put(makeCodeKey(hash), code)

		// Non-code entries
		key := []byte{byte(i + 10)}
		data := []byte{byte(i + 20)}
		nonCodes[string(key)] = data
		batch.Put(key, data)
	}

	// Write batch
	if err := batch.Write(); err != nil {
		t.Fatalf("Failed to write batch: %v", err)
	}

	// Verify codes are in diskdb
	for hash, code := range codes {
		if data, err := diskdb.Get(makeCodeKey(hash)); err != nil {
			t.Fatalf("Code not found in diskdb: %v", err)
		} else if !bytes.Equal(data, code) {
			t.Fatalf("Code data mismatch in diskdb")
		}
	}

	// Verify non-codes are not in diskdb
	for key := range nonCodes {
		if _, err := diskdb.Get([]byte(key)); err == nil {
			t.Fatalf("Non-code data should not be in diskdb")
		}
	}
}

// TestCodeRoutingConcurrent tests concurrent access to CodeRoutingDB
func TestCodeRoutingConcurrent(t *testing.T) {
	diskdb := rawdb.NewMemoryDatabase()
	routingDB := NewCodeRoutingDB(diskdb)

	const goroutines = 10
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Concurrent writes
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				// Write code
				code := []byte{byte(id), byte(i)}
				hash := crypto.Keccak256Hash(code)
				if err := routingDB.Put(makeCodeKey(hash), code); err != nil {
					t.Errorf("Failed to put code: %v", err)
				}

				// Write non-code
				key := []byte{byte(id + 100), byte(i)}
				data := []byte{byte(id + 200), byte(i)}
				if err := routingDB.Put(key, data); err != nil {
					t.Errorf("Failed to put non-code: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()

	// Concurrent reads
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				// Read code
				code := []byte{byte(id), byte(i)}
				hash := crypto.Keccak256Hash(code)
				if data, err := routingDB.Get(makeCodeKey(hash)); err != nil {
					t.Errorf("Failed to get code: %v", err)
				} else if !bytes.Equal(data, code) {
					t.Errorf("Code data mismatch")
				}

				// Read non-code
				key := []byte{byte(id + 100), byte(i)}
				expectedData := []byte{byte(id + 200), byte(i)}
				if data, err := routingDB.Get(key); err != nil {
					t.Errorf("Failed to get non-code: %v", err)
				} else if !bytes.Equal(data, expectedData) {
					t.Errorf("Non-code data mismatch")
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestMakeHashDBWithDiskDB tests witness import with code routing to disk
func TestMakeHashDBWithDiskDB(t *testing.T) {
	// Create a disk database with some pre-existing codes
	diskdb := rawdb.NewMemoryDatabase()

	// Pre-populate diskdb with some codes
	existingCode := []byte("existing contract code")
	existingHash := crypto.Keccak256Hash(existingCode)
	rawdb.WriteCode(diskdb, existingHash, existingCode)

	// Create a witness with headers and state nodes
	header := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: common.HexToHash("0x1234"),
		Root:       common.HexToHash("0x5678"),
	}

	witness, err := NewWitness(header, nil)
	if err != nil {
		t.Fatalf("Failed to create witness: %v", err)
	}

	// Add some headers
	witness.Headers = []*types.Header{
		header,
		{
			Number:     big.NewInt(99),
			ParentHash: common.HexToHash("0xabcd"),
			Root:       common.HexToHash("0xef01"),
		},
	}

	// Add some state nodes
	witness.State["node1"] = struct{}{}
	witness.State["node2"] = struct{}{}

	// Create hash DB from witness
	hashDB := witness.MakeHashDB(diskdb)

	// Verify headers were written
	for _, hdr := range witness.Headers {
		storedHeader := rawdb.ReadHeader(hashDB, hdr.Hash(), hdr.Number.Uint64())
		if storedHeader == nil {
			t.Fatalf("Header not found: %v", hdr.Hash())
		}
		if storedHeader.Number.Cmp(hdr.Number) != 0 {
			t.Fatalf("Header number mismatch")
		}
	}

	// Verify state nodes were written
	hasher := crypto.NewKeccakState()
	hash := make([]byte, 32)
	for node := range witness.State {
		blob := []byte(node)
		hasher.Reset()
		hasher.Write(blob)
		hasher.Read(hash)

		// Legacy trie nodes are stored directly by hash
		if data, err := hashDB.Get(common.BytesToHash(hash).Bytes()); err != nil {
			t.Fatalf("State node not found: %v", err)
		} else if !bytes.Equal(data, blob) {
			t.Fatalf("State node data mismatch")
		}
	}

	// Verify pre-existing code is accessible through hashDB
	if code := rawdb.ReadCode(hashDB, existingHash); code == nil {
		t.Fatalf("Failed to get existing code")
	} else if !bytes.Equal(code, existingCode) {
		t.Fatalf("Existing code mismatch")
	}

	// Write new code through hashDB and verify it goes to diskdb
	newCode := []byte("new contract code")
	newHash := crypto.Keccak256Hash(newCode)
	rawdb.WriteCode(hashDB, newHash, newCode)

	// Verify new code is in diskdb
	if code := rawdb.ReadCode(diskdb, newHash); code == nil {
		t.Fatalf("New code not found in diskdb")
	} else if !bytes.Equal(code, newCode) {
		t.Fatalf("New code mismatch in diskdb")
	}
}

// TestCodePersistenceAcrossWitnesses tests that codes persist across multiple witness imports
func TestCodePersistenceAcrossWitnesses(t *testing.T) {
	diskdb := rawdb.NewMemoryDatabase()

	// First witness import - add some codes
	witness1, _ := NewWitness(&types.Header{Number: big.NewInt(100)}, nil)
	hashDB1 := witness1.MakeHashDB(diskdb)

	code1 := []byte("contract1")
	hash1 := crypto.Keccak256Hash(code1)
	rawdb.WriteCode(hashDB1, hash1, code1)

	// Second witness import - verify code1 is still accessible
	witness2, _ := NewWitness(&types.Header{Number: big.NewInt(101)}, nil)
	hashDB2 := witness2.MakeHashDB(diskdb)

	// Verify code1 is accessible through new hashDB
	if code := rawdb.ReadCode(hashDB2, hash1); code == nil {
		t.Fatalf("Code1 not accessible in second import")
	} else if !bytes.Equal(code, code1) {
		t.Fatalf("Code1 data mismatch in second import")
	}

	// Add another code through second hashDB
	code2 := []byte("contract2")
	hash2 := crypto.Keccak256Hash(code2)
	rawdb.WriteCode(hashDB2, hash2, code2)

	// Third witness import - verify both codes are accessible
	witness3, _ := NewWitness(&types.Header{Number: big.NewInt(102)}, nil)
	hashDB3 := witness3.MakeHashDB(diskdb)

	for hash, expectedCode := range map[common.Hash][]byte{hash1: code1, hash2: code2} {
		if code := rawdb.ReadCode(hashDB3, hash); code == nil {
			t.Fatalf("Code not accessible in third import")
		} else if !bytes.Equal(code, expectedCode) {
			t.Fatalf("Code data mismatch in third import")
		}
	}
}

// TestIsCodeKey tests the rawdb.IsCodeKey function with various inputs
func TestIsCodeKey(t *testing.T) {
	tests := []struct {
		name     string
		key      []byte
		expected bool
	}{
		{
			name:     "Valid code key",
			key:      makeCodeKey(common.HexToHash("0x1234")),
			expected: true,
		},
		{
			name:     "Header key",
			key:      append(append([]byte("h"), make([]byte, 8)...), common.HexToHash("0x1234").Bytes()...),
			expected: false,
		},
		{
			name:     "Random key",
			key:      []byte("random"),
			expected: false,
		},
		{
			name:     "Empty key",
			key:      []byte{},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isCode, _ := rawdb.IsCodeKey(test.key)
			if isCode != test.expected {
				t.Errorf("IsCodeKey(%v) = %v, want %v", test.key, isCode, test.expected)
			}
		})
	}
}
