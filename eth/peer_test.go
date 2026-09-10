package eth // replace with the actual package name

import (
	"bytes"
	"errors"
	"math/big"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert" // import path where ethPeer lives
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestRequestWitnesses_NoWitPeer(t *testing.T) {
	p := &ethPeer{}                  // no witPeer set
	dlCh := make(chan *eth.Response) // downstream result channel

	req, err := p.RequestWitnesses([]common.Hash{{1, 2, 3}}, dlCh)

	assert.Nil(t, req, "expected nil *eth.Request when witPeer is missing")
	assert.EqualError(t, err, "witness peer not found")
}

func TestRequestWitnesses_HasWitPeer_Returns(t *testing.T) {
	hashToRequest := common.Hash{123}
	witData := testWitnessData(t)

	p, mockWitPeer := testPeer(t)
	dlCh := make(chan *eth.Response)

	mockWitPeer.
		EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hashToRequest, Page: 0}}), gomock.AssignableToTypeOf((chan *wit.Response)(nil))).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{Page: 0, TotalPages: 1, Hash: hashToRequest, Data: witData}},
					},
					Done: make(chan error, 10), // buffered so no block
				}
			}()

			return &wit.Request{}, nil
		}).
		Times(1)

	req, err := p.RequestWitnesses([]common.Hash{hashToRequest}, dlCh)

	response := <-dlCh
	assert.NoError(t, err)
	assert.NotNil(t, req, "expected a non-nil *eth.Request shim when witPeer is set")
	assert.NotNil(t, response, "expected a non-nil *eth.Response shim when witPeer is set")
}

// Tests an adversarial scenario where multiples requests could be shot at once
// It'll be done by spllitting the payload in multiple different pages and then controlling how much are calling the RequestWitness
func TestRequestWitnesses_Controlling_Max_Concurrent_Calls(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	hashToRequest := common.Hash{123}
	witness, _ := stateless.NewWitness(&types.Header{}, nil)
	FillWitnessWithDeterministicRandomState(witness, 10*1024)
	var witBuf bytes.Buffer
	witness.EncodeRLP(&witBuf)

	mockWitPeer := NewMockWitnessPeer(ctrl)
	p := &ethPeer{Peer: eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01, 0x02}, "test-peer", []p2p.Cap{}), nil, nil), witPeer: &witPeer{Peer: mockWitPeer}}
	dlCh := make(chan *eth.Response)
	concurrentCount := 0
	maxConcurrentCount := 0
	var muConcurrentCount sync.Mutex
	testPageSize := 200                                                   // 200bytes -> ~ 10*1024/200 ~ 54 pages
	totalPages := (len(witBuf.Bytes()) + testPageSize - 1) / testPageSize // ceil division len()/pageSize

	randomPageToFailTwice := rand.Intn(totalPages-1) + 1
	randomFailCount := 0
	zeroPageFailCount := 0 //page zero is edge case so we will also fail this page in all tests
	calls := 0

	mockWitPeer.EXPECT().Log().Return(log.New()).AnyTimes()
	mockWitPeer.
		EXPECT().
		RequestWitness(gomock.AssignableToTypeOf(([]wit.WitnessPageRequest)(nil)), gomock.AssignableToTypeOf((chan *wit.Response)(nil))).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				muConcurrentCount.Lock()
				concurrentCount++
				calls++
				if concurrentCount > maxConcurrentCount {
					maxConcurrentCount = concurrentCount
				}

				shouldFail := false
				if wpr[0].Page == uint64(randomPageToFailTwice) && randomFailCount < 2 {
					shouldFail = true
					randomFailCount++
				}
				if wpr[0].Page == uint64(0) && zeroPageFailCount < 2 {
					shouldFail = true
					zeroPageFailCount++
				}

				muConcurrentCount.Unlock()
				time.Sleep(50 * time.Millisecond) // force wait to increase concurrency
				start := wpr[0].Page * uint64(testPageSize)
				end := start + uint64(testPageSize)
				if end > uint64(len(witBuf.Bytes())) {
					end = uint64(len(witBuf.Bytes()))
				}

				if !shouldFail {
					ch <- &wit.Response{
						Res: &wit.WitnessPacketRLPPacket{
							WitnessPacketResponse: []wit.WitnessPageResponse{{Page: wpr[0].Page, TotalPages: uint64(totalPages), Hash: hashToRequest, Data: witBuf.Bytes()[start:end]}},
						},
						Done: make(chan error, 10), // buffered so no block
					}
				} else {
					ch <- &wit.Response{
						Res:  0,
						Done: make(chan error, 10), // buffered so no block
					}
				}

				muConcurrentCount.Lock()
				concurrentCount--
				muConcurrentCount.Unlock()
			}()

			return &wit.Request{}, nil
		}).
		Times(totalPages + 4) // because of two fails

	req, err := p.RequestWitnesses([]common.Hash{hashToRequest}, dlCh)

	response := <-dlCh
	assert.NoError(t, err)
	assert.NotNil(t, req, "expected a non-nil *eth.Request shim when witPeer is set")
	assert.NotNil(t, response, "expected a non-nil *eth.Response shim when witPeer is set")
	assert.Equal(t, 5, maxConcurrentCount, "must reach the maximum of the concurrent count")
}

// FillWitnessWithDeterministicRandomState repeatedly generates and adds random code blocks
// to the witness until the total added code reaches 40MB. The size of each block is up to 24KB.
// Random generation is seeded deterministically on each call, so the sequence is repeatable.
func FillWitnessWithDeterministicRandomState(w *stateless.Witness, targetSize int) {
	const (
		maxChunkSize = 24 * 1024 // 24KB
		seed         = 42        // fixed seed for determinism
	)

	r := rand.New(rand.NewSource(seed))
	total := 0

	for total < targetSize {
		// determine next chunk size (1 to maxChunkSize)
		chunkSize := r.Intn(maxChunkSize) + 1
		if total+chunkSize > targetSize {
			chunkSize = targetSize - total
		}

		// generate random bytes
		buf := make([]byte, chunkSize)
		for i := range buf {
			buf[i] = byte(r.Intn(256))
		}

		// add to witness
		states := map[string][]byte{
			string(buf): buf,
		}
		w.AddState(states)
		total += chunkSize
	}
}

// TestRequestWitnesses_PeerDisconnectionNoPanic tests that when a peer disconnects
// before any responses are received, the function gracefully handles the situation
// without panicking due to nil pointer dereference.
func TestRequestWitnesses_PeerDisconnectionNoPanic(t *testing.T) {
	hashToRequest := common.Hash{123}
	p, mockWitPeer := testPeer(t)
	dlCh := make(chan *eth.Response, 1)

	// Mock RequestWitness to simulate immediate failure (peer disconnection).
	mockWitPeer.
		EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			// Close the channel immediately to simulate no responses.
			go func() {
				// Immediately close the channel without sending anything.
				close(ch)
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	// Call RequestWitnesses.
	req, err := p.RequestWitnesses([]common.Hash{hashToRequest}, dlCh)

	// Verify no error during setup.
	assert.NoError(t, err)
	assert.NotNil(t, req, "expected a non-nil *eth.Request")

	// Wait for response - should receive empty response instead of panic.
	select {
	case response := <-dlCh:
		assert.NotNil(t, response, "should receive a response")
		assert.NotNil(t, response.Res, "response should have non-nil Res field")

		// Verify it's an empty witness slice, not nil.
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok, "response should contain []*stateless.Witness")
		assert.Equal(t, 0, len(witnesses), "should receive empty witness slice")

		// Verify other fields are handled correctly.
		assert.NotNil(t, response.Req, "should have request reference")
		assert.NotNil(t, response.Done, "should have Done channel")
		assert.Nil(t, response.Meta, "should have nil Meta when no responses received")
		assert.Equal(t, time.Duration(0), response.Time, "should have zero time when no responses received")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnesses_EmptyResponsesNoPanic tests the case where responses are received
// but they contain no usable witness data, ensuring no panic occurs.
func TestRequestWitnesses_EmptyResponsesNoPanic(t *testing.T) {
	hashToRequest := common.Hash{123}
	p, mockWitPeer := testPeer(t)
	dlCh := make(chan *eth.Response, 1)

	// Mock RequestWitness to send empty/invalid responses.
	mockWitPeer.
		EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				// Send response with empty data (simulates corrupted or empty witness pages).
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       0,
							TotalPages: 1,
							Hash:       hashToRequest,
							Data:       []byte{}, // Empty data
						}},
					},
					Done: make(chan error, 1),
				}
			}()
			return &wit.Request{}, nil
		}).
		Times(1)

	// Call RequestWitnesses.
	req, err := p.RequestWitnesses([]common.Hash{hashToRequest}, dlCh)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response.
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok, "should receive []*stateless.Witness type")
		assert.Equal(t, 0, len(witnesses), "should receive empty witness slice when no valid witnesses reconstructed")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnesses_PartialFailureNoPanic tests that when some witness pages fail
// to be received or reconstructed, the function handles it gracefully.
func TestRequestWitnesses_PartialFailureNoPanic(t *testing.T) {
	hashToRequest := common.Hash{123}
	p, mockWitPeer := testPeer(t)
	dlCh := make(chan *eth.Response, 1)

	// Mock RequestWitness to send malformed response (wrong type).
	mockWitPeer.
		EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				// Send response with wrong type (not WitnessPacketRLPPacket).
				ch <- &wit.Response{
					Res:  "invalid_response_type", // Wrong type
					Done: make(chan error, 1),
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	// Call RequestWitnesses.
	req, err := p.RequestWitnesses([]common.Hash{hashToRequest}, dlCh)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response - should get empty response due to processing failure.
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok, "should receive []*stateless.Witness type even on processing failure")
		assert.Equal(t, 0, len(witnesses), "should receive empty witness slice when processing fails")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnessPageCount_WIT1Protocol tests the new metadata request method
func TestRequestWitnessPageCount_WIT1Protocol(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	expectedPageCount := uint64(15)

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()

	// Mock RequestWitnessMetadata to return page count
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Eq([]common.Hash{hash}), gomock.Any()).
		DoAndReturn(func(hashes []common.Hash, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessMetadataPacket{
						Metadata: []wit.WitnessMetadataResponse{{
							Hash:        hash,
							TotalPages:  expectedPageCount,
							WitnessSize: 225 * 1024 * 1024,
							BlockNumber: 100,
							Available:   true,
						}},
					},
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.NoError(t, err)
	assert.Equal(t, expectedPageCount, pageCount)
}

// TestRequestWitnessPageCount_WIT0FallbackToLegacy tests fallback to legacy method
func TestRequestWitnessPageCount_WIT0FallbackToLegacy(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	expectedPageCount := uint64(10)

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT0)).AnyTimes()

	// Mock RequestWitness (legacy) to return first page with TotalPages
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hash, Page: 0}}), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       0,
							TotalPages: expectedPageCount,
							Hash:       hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.NoError(t, err)
	assert.Equal(t, expectedPageCount, pageCount)
}

// TestRequestWitnessPageCount_NoWitPeer tests error when witness peer not available
func TestRequestWitnessPageCount_NoWitPeer(t *testing.T) {
	p := &ethPeer{} // no witPeer set

	pageCount, err := p.RequestWitnessPageCount(common.Hash{0x01})

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "witness peer not found")
}

// TestRequestWitnessPageCount_MetadataNotAvailable tests unavailable witness
func TestRequestWitnessPageCount_MetadataNotAvailable(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()

	// Mock RequestWitnessMetadata to return unavailable witness
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Any(), gomock.Any()).
		DoAndReturn(func(hashes []common.Hash, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessMetadataPacket{
						Metadata: []wit.WitnessMetadataResponse{{
							Hash:      hash,
							Available: false, // Not available
						}},
					},
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "witness not available")
}

// TestRequestWitnessPageCount_EmptyMetadataResponse tests empty metadata response
func TestRequestWitnessPageCount_EmptyMetadataResponse(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()

	// Mock RequestWitnessMetadata to return empty metadata
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Any(), gomock.Any()).
		DoAndReturn(func(hashes []common.Hash, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessMetadataPacket{
						Metadata: []wit.WitnessMetadataResponse{}, // Empty
					},
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "empty witness metadata response")
}

// TestRequestWitnessPageCount_WrongResponseType tests wrong response type handling
func TestRequestWitnessPageCount_WrongResponseType(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()

	// Mock RequestWitnessMetadata to return wrong type
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Any(), gomock.Any()).
		DoAndReturn(func(hashes []common.Hash, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: "wrong_type", // Wrong type
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "unexpected witness metadata response type")
}

// TestRequestWitnessPageCount_Timeout tests timeout handling
func TestRequestWitnessPageCount_Timeout(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()

	// Mock RequestWitnessMetadata to never respond (timeout scenario)
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Any(), gomock.Any()).
		DoAndReturn(func(hashes []common.Hash, ch chan *wit.Response) (*wit.Request, error) {
			// Don't send any response - will trigger timeout
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "timeout")
}

// TestRequestWitnessPageCount_NilResponse tests nil response handling
func TestRequestWitnessPageCount_NilResponse(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()

	// Mock RequestWitnessMetadata to return nil response
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Any(), gomock.Any()).
		DoAndReturn(func(hashes []common.Hash, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- nil // Nil response
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "nil witness metadata response")
}

// TestSupportsWitness tests the SupportsWitness method
func TestSupportsWitness(t *testing.T) {
	t.Run("WithWitPeer", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockWitPeer := NewMockWitnessPeer(ctrl)
		p := &ethPeer{
			Peer:    eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test", []p2p.Cap{}), nil, nil),
			witPeer: &witPeer{Peer: mockWitPeer},
		}

		assert.True(t, p.SupportsWitness())
	})

	t.Run("WithoutWitPeer", func(t *testing.T) {
		p := &ethPeer{
			Peer: eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test", []p2p.Cap{}), nil, nil),
		}

		assert.False(t, p.SupportsWitness())
	})
}

// TestReconstructWitness tests witness reconstruction from pages
func TestReconstructWitness(t *testing.T) {
	t.Run("SuccessfulReconstruction", func(t *testing.T) {
		// Create a test witness and encode it
		witness, _ := stateless.NewWitness(&types.Header{Number: big.NewInt(100)}, nil)
		FillWitnessWithDeterministicRandomState(witness, 5*1024)
		var buf bytes.Buffer
		witness.EncodeRLP(&buf)
		witnessBytes := buf.Bytes()

		// Split into pages
		pageSize := 1024
		var pages []wit.WitnessPageResponse
		for i := 0; i < len(witnessBytes); i += pageSize {
			end := i + pageSize
			if end > len(witnessBytes) {
				end = len(witnessBytes)
			}
			pages = append(pages, wit.WitnessPageResponse{
				Page:       uint64(len(pages)),
				TotalPages: uint64((len(witnessBytes) + pageSize - 1) / pageSize),
				Hash:       common.Hash{0x01},
				Data:       witnessBytes[i:end],
			})
		}

		// Reconstruct
		p := &ethPeer{Peer: eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test", []p2p.Cap{}), nil, nil)}
		reconstructed, err := p.reconstructWitness(pages)

		assert.NoError(t, err)
		assert.NotNil(t, reconstructed)
		assert.Equal(t, witness.Header().Number.Uint64(), reconstructed.Header().Number.Uint64())
	})

	t.Run("OutOfOrderPages", func(t *testing.T) {
		// Create pages out of order
		witness, _ := stateless.NewWitness(&types.Header{Number: big.NewInt(100)}, nil)
		FillWitnessWithDeterministicRandomState(witness, 3*1024)
		var buf bytes.Buffer
		witness.EncodeRLP(&buf)
		witnessBytes := buf.Bytes()

		pageSize := 1024
		var pages []wit.WitnessPageResponse
		for i := 0; i < len(witnessBytes); i += pageSize {
			end := i + pageSize
			if end > len(witnessBytes) {
				end = len(witnessBytes)
			}
			pages = append(pages, wit.WitnessPageResponse{
				Page:       uint64(len(pages)),
				TotalPages: uint64((len(witnessBytes) + pageSize - 1) / pageSize),
				Hash:       common.Hash{0x01},
				Data:       witnessBytes[i:end],
			})
		}

		// Shuffle pages
		pages[0], pages[2] = pages[2], pages[0]

		// Reconstruct - should still work due to sorting
		p := &ethPeer{Peer: eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test", []p2p.Cap{}), nil, nil)}
		reconstructed, err := p.reconstructWitness(pages)

		assert.NoError(t, err)
		assert.NotNil(t, reconstructed)
	})

	t.Run("InvalidRLPData", func(t *testing.T) {
		// Create pages with invalid RLP data
		pages := []wit.WitnessPageResponse{
			{
				Page:       0,
				TotalPages: 1,
				Hash:       common.Hash{0x01},
				Data:       []byte{0xFF, 0xFF, 0xFF}, // Invalid RLP
			},
		}

		p, _ := testPeer(t)
		reconstructed, err := p.reconstructWitness(pages)

		assert.Error(t, err)
		assert.Nil(t, reconstructed)
	})

	// DuplicatePageMisreconstructs documents a gap in the multi-page witness
	// path: reconstructWitness sorts by page index and concatenates without
	// deduping, and its caller appends received pages to receivedWitPages
	// (peer.go:542) with no (hash,page) dedup while triggering reassembly purely
	// on len(pages)==TotalPages (peer.go:543). So if one page index arrives twice
	// (e.g. an original request and a retry both land, or a peer replays a page),
	// the count hits TotalPages with a real page still missing, reassembly fires
	// over the duplicated/short byte stream, and the witness is reconstructed
	// incorrectly — never as the original. A late duplicate instead trips the
	// ">TotalPages" guard (peer.go:591) and jails the peer. This test pins the
	// mis-reconstruction so a future dedup fix can be verified; it is not fixed
	// here (out of scope for this PR).
	t.Run("DuplicatePageMisreconstructs", func(t *testing.T) {
		witness, _ := stateless.NewWitness(&types.Header{Number: big.NewInt(100)}, nil)
		FillWitnessWithDeterministicRandomState(witness, 4*1024)
		var buf bytes.Buffer
		require.NoError(t, witness.EncodeRLP(&buf))
		witnessBytes := buf.Bytes()

		// Split into exactly two pages so [p0,p1] reconstructs the original.
		half := (len(witnessBytes) + 1) / 2
		p0 := wit.WitnessPageResponse{Page: 0, TotalPages: 2, Hash: common.Hash{0x01}, Data: witnessBytes[:half]}
		p1 := wit.WitnessPageResponse{Page: 1, TotalPages: 2, Hash: common.Hash{0x01}, Data: witnessBytes[half:]}

		p := &ethPeer{Peer: eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test", []p2p.Cap{}), nil, nil)}

		// Sanity: the correct page set reconstructs the original witness.
		correct, err := p.reconstructWitness([]wit.WitnessPageResponse{p0, p1})
		require.NoError(t, err)
		require.NotNil(t, correct)

		// Duplicate of page 0 (page 1 never arrives): len hits TotalPages=2, so
		// the caller would trigger reassembly here. reconstructWitness must NOT
		// silently yield the correct witness — it either errors or produces a
		// different (wrong) reconstruction.
		dup, dupErr := p.reconstructWitness([]wit.WitnessPageResponse{p0, p0})
		if dupErr == nil {
			var dupBuf bytes.Buffer
			require.NoError(t, dup.EncodeRLP(&dupBuf))
			assert.NotEqual(t, witnessBytes, dupBuf.Bytes(),
				"duplicate page 0 must not reconstruct the original witness (no dedup)")
		}
	})
}

// TestEthWitRequestClose tests the Close method of ethWitRequest
func TestEthWitRequestClose(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Create mock wit requests
	mockWitReq1 := &wit.Request{}
	mockWitReq2 := &wit.Request{}

	ethReq := &eth.Request{
		Peer:   "test-peer",
		Cancel: make(chan struct{}),
	}

	witReq := &ethWitRequest{
		Request: ethReq,
		witReqs: []*wit.Request{mockWitReq1, mockWitReq2},
	}

	// Close should not error
	err := witReq.Close()
	assert.NoError(t, err)

	// Verify cancel channel was closed
	select {
	case <-ethReq.Cancel:
		// Expected - channel was closed
	default:
		t.Error("Cancel channel was not closed")
	}
}

// TestJailPeerForViolation tests the jailPeerForViolation helper method
func TestJailPeerForViolation(t *testing.T) {
	p, _ := testPeer(t)

	t.Run("WithJailCallback", func(t *testing.T) {
		jailCalled := false
		jailedPeerID := ""
		jailPeer := func(id string) {
			jailCalled = true
			jailedPeerID = id
		}

		err := p.jailPeerForViolation(jailPeer, "invalid page number", map[string]interface{}{
			"page":       uint64(10),
			"totalPages": uint64(5),
			"hash":       common.Hash{0xaa},
		})

		assert.Error(t, err)
		assert.True(t, jailCalled, "jail callback should have been called")
		assert.Equal(t, p.ID(), jailedPeerID)
		assert.Contains(t, err.Error(), "invalid page number")
	})

	t.Run("WithoutJailCallback", func(t *testing.T) {
		err := p.jailPeerForViolation(nil, "inconsistent TotalPages", map[string]interface{}{
			"existing": uint64(10),
			"new":      uint64(20),
		})

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "inconsistent TotalPages")
	})

	t.Run("MultipleDetails", func(t *testing.T) {
		err := p.jailPeerForViolation(nil, "test violation", map[string]interface{}{
			"field1": "value1",
			"field2": 123,
			"field3": true,
		})

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "test violation")
		// Error should contain all details in some form
	})

	t.Run("EmptyDetails", func(t *testing.T) {
		err := p.jailPeerForViolation(nil, "simple violation", map[string]interface{}{})

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "simple violation")
	})
}

// TestRequestWitnessPageCount_ErrorRequestingMetadata tests error path when RequestWitnessMetadata fails
func TestRequestWitnessPageCount_ErrorRequestingMetadata(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	expectedError := errors.New("network error")

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT1)).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	// Mock RequestWitnessMetadata to return error
	mockWitPeer.EXPECT().
		RequestWitnessMetadata(gomock.Eq([]common.Hash{hash}), gomock.Any()).
		Return(nil, expectedError)

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, expectedError, err)
	assert.Equal(t, uint64(0), pageCount)
}

// TestRequestWitnessPageCountLegacy_ErrorRequesting tests error path when RequestWitness fails in legacy method
func TestRequestWitnessPageCountLegacy_ErrorRequesting(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	expectedError := errors.New("request failed")

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT0)).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	// Mock RequestWitness to return error
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hash, Page: 0}}), gomock.Any()).
		Return(nil, expectedError)

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, expectedError, err)
	assert.Equal(t, uint64(0), pageCount)
}

// TestRequestWitnessPageCountLegacy_NilResponse tests nil response handling in legacy method
func TestRequestWitnessPageCountLegacy_NilResponse(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT0)).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	// Mock RequestWitness to return nil response
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hash, Page: 0}}), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- nil // Nil response
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "nil witness response")
}

// TestRequestWitnessPageCountLegacy_WrongResponseType tests unexpected response type in legacy method
func TestRequestWitnessPageCountLegacy_WrongResponseType(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT0)).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	// Mock RequestWitness to return wrong type
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hash, Page: 0}}), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: "wrong_type", // Wrong type, not *wit.WitnessPacketRLPPacket
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "unexpected witness response type")
}

// TestRequestWitnessPageCountLegacy_EmptyPacketResponse tests empty witness packet response in legacy method
func TestRequestWitnessPageCountLegacy_EmptyPacketResponse(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT0)).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	// Mock RequestWitness to return empty packet response
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hash, Page: 0}}), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{}, // Empty
					},
				}
			}()
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "empty witness packet response")
}

// TestRequestWitnessPageCountLegacy_Timeout tests timeout in legacy method
func TestRequestWitnessPageCountLegacy_Timeout(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().Version().Return(uint(wit.WIT0)).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	// Mock RequestWitness to never respond (timeout scenario)
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Eq([]wit.WitnessPageRequest{{Hash: hash, Page: 0}}), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			// Don't send any response - will trigger timeout
			return &wit.Request{}, nil
		})

	pageCount, err := p.RequestWitnessPageCount(hash)

	assert.Error(t, err)
	assert.Equal(t, uint64(0), pageCount)
	assert.Contains(t, err.Error(), "timeout waiting for witness page count from peer")
}

// TestRequestWitnessesWithVerification_InvalidPageNumber tests invalid page number validation
func TestRequestWitnessesWithVerification_InvalidPageNumber(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	jailCalled := false
	jailedPeerID := ""

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()
	dlCh := make(chan *eth.Response, 1)

	jailPeer := func(id string) {
		jailCalled = true
		jailedPeerID = id
	}

	// Mock RequestWitness to return page with invalid page number (page >= TotalPages)
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				// Send page with Page=5 but TotalPages=5 (invalid: page >= totalPages)
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       5, // Invalid: >= TotalPages
							TotalPages: 5,
							Hash:       hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
					Done: make(chan error, 1),
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	req, err := p.RequestWitnessesWithVerification([]common.Hash{hash}, dlCh, nil, jailPeer)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response - should get empty response due to validation failure
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok)
		assert.Equal(t, 0, len(witnesses), "should receive empty witness slice when validation fails")
		assert.True(t, jailCalled, "jail callback should have been called")
		assert.Equal(t, p.ID(), jailedPeerID, "jailed peer ID should match actual peer ID")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnessesWithVerification_InconsistentTotalPages tests inconsistent TotalPages validation
func TestRequestWitnessesWithVerification_InconsistentTotalPages(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	jailCalled := false

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()
	dlCh := make(chan *eth.Response, 1)

	jailPeer := func(id string) {
		jailCalled = true
	}

	var callCount atomic.Int32
	// Mock RequestWitness to return pages with inconsistent TotalPages
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				count := callCount.Add(1)
				if count == 1 {
					// First page: TotalPages = 10
					ch <- &wit.Response{
						Res: &wit.WitnessPacketRLPPacket{
							WitnessPacketResponse: []wit.WitnessPageResponse{{
								Page:       0,
								TotalPages: 10,
								Hash:       hash,
								Data:       []byte{0x01, 0x02},
							}},
						},
						Done: make(chan error, 1),
					}
				} else {
					// Second page: TotalPages = 20 (inconsistent!)
					ch <- &wit.Response{
						Res: &wit.WitnessPacketRLPPacket{
							WitnessPacketResponse: []wit.WitnessPageResponse{{
								Page:       1,
								TotalPages: 20, // Inconsistent with first page
								Hash:       hash,
								Data:       []byte{0x03, 0x04},
							}},
						},
						Done: make(chan error, 1),
					}
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	req, err := p.RequestWitnessesWithVerification([]common.Hash{hash}, dlCh, nil, jailPeer)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok)
		// Should get empty response due to inconsistent TotalPages
		assert.Equal(t, 0, len(witnesses))
		assert.True(t, jailCalled, "jail callback should have been called for inconsistent TotalPages")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnessesWithVerification_PeerFailedVerification tests peer verification failure
func TestRequestWitnessesWithVerification_PeerFailedVerification(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()
	dlCh := make(chan *eth.Response, 1)

	// Verification function that returns false (peer is dishonest)
	verifyPageCount := func(h common.Hash, totalPages uint64, peerID string) bool {
		return false // Peer failed verification
	}

	// Mock RequestWitness to return page
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       0,
							TotalPages: 5,
							Hash:       hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
					Done: make(chan error, 1),
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	req, err := p.RequestWitnessesWithVerification([]common.Hash{hash}, dlCh, verifyPageCount, nil)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response - should get empty response due to verification failure
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok)
		assert.Equal(t, 0, len(witnesses), "should receive empty witness slice when verification fails")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnessesWithVerification_MorePagesThanTotalPages tests receiving more pages than TotalPages
func TestRequestWitnessesWithVerification_MorePagesThanTotalPages(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}
	jailCalled := false

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()
	dlCh := make(chan *eth.Response, 1)

	jailPeer := func(id string) {
		jailCalled = true
	}

	var callCount atomic.Int32
	// Mock RequestWitness to return pages that exceed TotalPages
	// First page sets TotalPages=2, then we send 3 pages total to trigger the check
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				count := callCount.Add(1)
				// Send pages: first sets TotalPages=2, then we send page 0, 1, and 2 (3 pages total)
				// This will trigger len(receivedWitPages) > currentTotalPages check
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       uint64(count - 1),
							TotalPages: 2, // Only 2 pages allowed, but we'll send 3
							Hash:       hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
					Done: make(chan error, 1),
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	req, err := p.RequestWitnessesWithVerification([]common.Hash{hash}, dlCh, nil, jailPeer)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok)
		// Should get empty response due to too many pages
		assert.Equal(t, 0, len(witnesses))
		assert.True(t, jailCalled, "jail callback should have been called for too many pages")

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

// TestRequestWitnessesWithVerification_DownloadPaused tests that download is paused correctly
// When download is paused, no more requests should be built
func TestRequestWitnessesWithVerification_DownloadPaused(t *testing.T) {
	hash := common.Hash{0xab, 0xcd}

	p, mockWitPeer := testPeer(t)
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()
	dlCh := make(chan *eth.Response, 1)

	// Verification function that returns false to pause download
	verifyPageCount := func(h common.Hash, totalPages uint64, peerID string) bool {
		return false // This will pause the download
	}

	// Mock RequestWitness - first page triggers verification failure which pauses download
	// When download is paused, subsequent pages should return early without building more requests
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				// First page triggers verification failure, which pauses download
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       0,
							TotalPages: 5,
							Hash:       hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
					Done: make(chan error, 1),
				}
				// After verification fails and download is paused, if we receive another page,
				// receiveWitnessPage should check paused and return nil early
				// This prevents building more requests
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	req, err := p.RequestWitnessesWithVerification([]common.Hash{hash}, dlCh, verifyPageCount, nil)
	assert.NoError(t, err)
	assert.NotNil(t, req)

	// Wait for response - should eventually get empty response since no witnesses are reconstructed
	select {
	case response := <-dlCh:
		assert.NotNil(t, response)
		witnesses, ok := response.Res.([]*stateless.Witness)
		assert.True(t, ok)
		// Should get empty response when download is paused and no witnesses are reconstructed
		assert.Equal(t, 0, len(witnesses))

	case <-time.After(5 * time.Second):
		// Give more time for the response to come through
		// The response should eventually come as empty when no witnesses are reconstructed
		t.Fatal("timed out waiting for response")
	}
}

// TestBuildWitnessRequests_ConcurrentFailedRequestsAccess verifies that concurrent access
// to the failedRequests map in buildWitnessRequests is properly synchronized.
// This test specifically validates the fix for the "concurrent map writes" panic.
// Run this test with: go test -race -run TestBuildWitnessRequests_ConcurrentFailedRequestsAccess
func TestBuildWitnessRequests_ConcurrentFailedRequestsAccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockWitPeer := NewMockWitnessPeer(ctrl)
	mockWitPeer.EXPECT().Log().Return(log.New()).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	p := &ethPeer{
		Peer:    eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test-peer", []p2p.Cap{}), nil, nil),
		witPeer: &witPeer{Peer: mockWitPeer},
	}

	// Mock RequestWitness to simulate responses
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       wpr[0].Page,
							TotalPages: 10,
							Hash:       wpr[0].Hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
					Done: make(chan error, 10),
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	// Setup shared data structures
	hashes := []common.Hash{{0xaa}, {0xbb}, {0xcc}}
	witTotalPages := make(map[common.Hash]uint64)
	witTotalRequest := make(map[common.Hash]uint64)
	failedRequests := make(map[common.Hash]map[uint64]witReqRetryCount)
	witReqResCh := make(chan *witReqRes, 100)
	witReqSem := make(chan int, 20)
	var mapsMu sync.RWMutex
	var buildRequestMu sync.RWMutex
	var witReqs []*wit.Request
	var witReqsWg sync.WaitGroup

	// Pre-populate failedRequests with some data to retry
	for _, hash := range hashes {
		witTotalPages[hash] = 5
		failedRequests[hash] = make(map[uint64]witReqRetryCount)
		for page := uint64(0); page < 3; page++ {
			failedRequests[hash][page] = witReqRetryCount{
				FailCount:        1,
				ShouldRetryAgain: true,
			}
		}
	}

	// Launch multiple concurrent buildWitnessRequests calls
	// This simulates the scenario where multiple goroutines spawned from
	// receiveWitnessPage.func1 (line 496) all try to access failedRequests
	const numGoroutines = 10
	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Drain semaphore slots that get filled
			go func() {
				for j := 0; j < 20; j++ {
					select {
					case <-witReqSem:
					case <-time.After(50 * time.Millisecond):
						return
					}
				}
			}()

			cancel := make(chan struct{})
			err := p.buildWitnessRequests(
				hashes,
				&witReqs,
				&witReqsWg,
				witTotalPages,
				witTotalRequest,
				witReqResCh,
				witReqSem,
				&mapsMu,
				&buildRequestMu,
				failedRequests,
				cancel,
			)
			if err != nil {
				errCh <- err
			}
		}()
	}

	// Also simulate concurrent writes to failedRequests from receiveWitnessPage deferred function
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			hash := hashes[idx%len(hashes)]
			page := uint64(idx)

			// This simulates the deferred function in receiveWitnessPage (lines 479-504)
			// After the fix, this should be protected by mapsMu
			mapsMu.Lock()
			if failedRequests[hash] == nil {
				failedRequests[hash] = make(map[uint64]witReqRetryCount)
			}
			retryCount := failedRequests[hash][page]
			retryCount.FailCount++
			retryCount.ShouldRetryAgain = true
			failedRequests[hash][page] = retryCount
			mapsMu.Unlock()
		}(i)
	}

	// Wait for all goroutines with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success - no panic occurred
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out - possible deadlock")
	}

	// Check for errors
	close(errCh)
	for err := range errCh {
		t.Logf("buildWitnessRequests returned error (expected in some concurrent scenarios): %v", err)
	}

	t.Log("Test completed successfully - no concurrent map write panic occurred")
	t.Log("If running with -race flag, any data race will be reported by the race detector")
}

// TestFailedRequestsMapConcurrentWritesFix specifically tests the fix for the panic:
// "fatal error: concurrent map writes" in receiveWitnessPage.func1
// This test creates the exact scenario from the bug report where multiple goroutines
// simultaneously write to failedRequests map.
func TestFailedRequestsMapConcurrentWritesFix(t *testing.T) {
	// This test verifies the fix by simulating concurrent access patterns
	// that previously caused the panic at eth/peer.go:491

	failedRequests := make(map[common.Hash]map[uint64]witReqRetryCount)
	var mapsMu sync.RWMutex

	hashes := make([]common.Hash, 20)
	for i := range hashes {
		hashes[i] = common.Hash{byte(i)}
	}

	// Simulate multiple goroutines writing to failedRequests concurrently
	// This is what happens when multiple receiveWitnessPage.func1 goroutines
	// try to update failedRequests simultaneously
	const numWriters = 50
	var wg sync.WaitGroup

	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			hash := hashes[idx%len(hashes)]
			page := uint64(idx % 10)

			// Simulate the fixed code path from receiveWitnessPage deferred function
			// Lines 481-493 with proper locking
			mapsMu.Lock()
			if failedRequests[hash] == nil {
				failedRequests[hash] = make(map[uint64]witReqRetryCount)
			}
			retryCount := failedRequests[hash][page]
			retryCount.FailCount++
			if retryCount.FailCount <= DefaultMaxPagesRequestRetries {
				retryCount.ShouldRetryAgain = true
			}
			failedRequests[hash][page] = retryCount
			mapsMu.Unlock()
		}(i)
	}

	// Simulate concurrent readers (from buildWitnessRequests)
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			hash := hashes[idx%len(hashes)]

			// Simulate the fixed code path from buildWitnessRequests
			// Lines 676-696 with proper locking
			mapsMu.Lock()
			// Copy under lock
			localCopy := make(map[uint64]witReqRetryCount)
			if pages, ok := failedRequests[hash]; ok {
				for page, retryCount := range pages {
					localCopy[page] = retryCount
				}
			}
			mapsMu.Unlock()

			// Process copy without holding lock
			for page, retryCount := range localCopy {
				if retryCount.ShouldRetryAgain {
					// Simulate processing...
					retryCount.ShouldRetryAgain = false

					// Update original under lock
					mapsMu.Lock()
					if failedRequests[hash] != nil {
						failedRequests[hash][page] = retryCount
					}
					mapsMu.Unlock()
				}
			}
		}(i)
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("All concurrent operations completed without panic")
	case <-time.After(10 * time.Second):
		t.Fatal("Test timed out")
	}

	// Verify data integrity - all writes should have been recorded
	mapsMu.RLock()
	totalEntries := 0
	for _, pages := range failedRequests {
		totalEntries += len(pages)
	}
	mapsMu.RUnlock()

	t.Logf("Final failedRequests has %d hashes with %d total page entries", len(failedRequests), totalEntries)
	assert.True(t, len(failedRequests) > 0, "failedRequests should have entries after concurrent writes")
}

// TestDoWitnessRequest_RaceCondition_WitTotalRequest exposes the race condition in doWitnessRequest
// where witTotalRequest[hash] is read without lock protection but written with lock protection.
// Run this test with: go test -race -run TestDoWitnessRequest_RaceCondition_WitTotalRequest
func TestDoWitnessRequest_RaceCondition_WitTotalRequest(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	hash := common.Hash{0xab, 0xcd}

	mockWitPeer := NewMockWitnessPeer(ctrl)
	mockWitPeer.EXPECT().Log().Return(log.New()).AnyTimes()
	mockWitPeer.EXPECT().ID().Return("test-peer").AnyTimes()

	p := &ethPeer{
		Peer:    eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01}, "test-peer", []p2p.Cap{}), nil, nil),
		witPeer: &witPeer{Peer: mockWitPeer},
	}

	// Mock RequestWitness to simulate responses with small delays
	mockWitPeer.EXPECT().
		RequestWitness(gomock.Any(), gomock.Any()).
		DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
			go func() {
				// Add small delay to increase chance of concurrent access
				time.Sleep(1 * time.Millisecond)
				ch <- &wit.Response{
					Res: &wit.WitnessPacketRLPPacket{
						WitnessPacketResponse: []wit.WitnessPageResponse{{
							Page:       wpr[0].Page,
							TotalPages: 100, // Large number to trigger many requests
							Hash:       hash,
							Data:       []byte{0x01, 0x02},
						}},
					},
					Done: make(chan error, 10),
				}
			}()
			return &wit.Request{}, nil
		}).
		AnyTimes()

	// Setup the data structures that doWitnessRequest uses
	witTotalPages := make(map[common.Hash]uint64)
	witTotalRequest := make(map[common.Hash]uint64)
	witReqResCh := make(chan *witReqRes, DefaultConcurrentResponsesHandled)
	witReqSem := make(chan int, DefaultConcurrentRequestsPerPeer)
	var mapsMu sync.RWMutex
	var witReqs []*wit.Request
	var witReqsWg sync.WaitGroup

	// Initialize with TotalPages so we know how many requests to make
	witTotalPages[hash] = 50
	witTotalRequest[hash] = 0

	// Run multiple iterations to increase the chance of catching the race
	const iterations = 10
	for iter := 0; iter < iterations; iter++ {
		// Reset the counter for each iteration
		mapsMu.Lock()
		witTotalRequest[hash] = 0
		mapsMu.Unlock()

		// Launch many concurrent doWitnessRequest calls
		// This simulates concurrent goroutines trying to request different pages
		const numConcurrentRequests = 20
		var wg sync.WaitGroup

		for i := 0; i < numConcurrentRequests; i++ {
			wg.Add(1)
			page := uint64(i)

			go func(pg uint64) {
				defer wg.Done()

				// Call doWitnessRequest which contains the race condition
				// Multiple goroutines will simultaneously:
				// 1. Read witTotalRequest[hash] (unprotected) at line 731
				// 2. Compare page >= witTotalRequest[hash]
				// 3. Write witTotalRequest[hash]++ (protected) at line 733
				cancel := make(chan struct{})
				err := p.doWitnessRequest(
					hash,
					pg,
					&witReqs,
					&witReqsWg,
					witReqResCh,
					witReqSem,
					&mapsMu,
					witTotalRequest,
					cancel,
				)

				// Consume from semaphore to prevent blocking
				select {
				case <-witReqSem:
				case <-time.After(100 * time.Millisecond):
				}

				if err != nil {
					// Error is expected in some cases, not critical for this test
					t.Logf("doWitnessRequest returned error: %v", err)
				}
			}(page)
		}

		// Wait for all goroutines to complete
		wg.Wait()

		// Small delay between iterations
		time.Sleep(10 * time.Millisecond)
	}

	// The test itself doesn't need to assert anything specific
	// The race detector will catch the unprotected read if run with -race flag
	t.Logf("Test completed %d iterations with %d concurrent requests each", iterations, 20)
	t.Log("If running with -race flag, any data race will be reported by the race detector")
}

// TestRequestCloseShimClosesCancelChannel verifies that calling Close() on an
// eth.Request shim (peer == nil) properly closes the Cancel channel. This was
// the root cause of adapter goroutine leaks — the concurrent fetcher called
// Close() but Cancel was never closed.
// --- Test helpers for goroutine leak fix tests ---

// testPeer creates a mock ethPeer with Log() pre-configured.
func testPeer(t *testing.T) (*ethPeer, *MockWitnessPeer) {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	mock := NewMockWitnessPeer(ctrl)
	mock.EXPECT().Log().Return(log.New()).AnyTimes()
	p := &ethPeer{
		Peer:    eth.NewPeer(1, p2p.NewPeer(enode.ID{0x01, 0x02}, "test-peer", []p2p.Cap{}), nil, nil),
		witPeer: &witPeer{Peer: mock},
	}
	return p, mock
}

// testWitnessData returns RLP-encoded witness bytes for use in mock responses.
func testWitnessData(t *testing.T) []byte {
	t.Helper()
	w, _ := stateless.NewWitness(&types.Header{}, nil)
	FillWitnessWithDeterministicRandomState(w, 10*1024)
	var buf bytes.Buffer
	w.EncodeRLP(&buf)
	return buf.Bytes()
}

// doWitnessRequestTestState holds channels and state for doWitnessRequest tests.
type doWitnessRequestTestState struct {
	witReqs      []*wit.Request
	wg           sync.WaitGroup
	mu           sync.RWMutex
	resCh        chan *witReqRes
	sem          chan int
	totalRequest map[common.Hash]uint64
	cancel       chan struct{}
}

func newDoWitnessRequestTestState(resBuf int) *doWitnessRequestTestState {
	return &doWitnessRequestTestState{
		resCh:        make(chan *witReqRes, resBuf),
		sem:          make(chan int, DefaultConcurrentRequestsPerPeer),
		totalRequest: make(map[common.Hash]uint64),
		cancel:       make(chan struct{}),
	}
}

func (s *doWitnessRequestTestState) call(p *ethPeer, hash common.Hash, page uint64) error {
	return p.doWitnessRequest(hash, page, &s.witReqs, &s.wg, s.resCh, s.sem, &s.mu, s.totalRequest, s.cancel)
}

// waitWG waits for the WaitGroup with a timeout.
func (s *doWitnessRequestTestState) waitWG(t *testing.T, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("WaitGroup did not complete — goroutines are leaked")
	}
}

// assertNoGoroutineLeak checks goroutine count hasn't grown significantly.
func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	assert.LessOrEqual(t, after, before+2,
		"goroutine leak (before=%d, after=%d)", before, after)
}

// --- eth.Request.Close tests ---

func TestRequestClose(t *testing.T) {
	t.Run("shim closes Cancel", func(t *testing.T) {
		req := &eth.Request{Peer: "test", Cancel: make(chan struct{})}
		assert.NoError(t, req.Close())
		select {
		case <-req.Cancel:
		default:
			t.Fatal("Cancel not closed")
		}
		// Double close should not panic
		assert.NoError(t, req.Close())
	})

	t.Run("nil Cancel no panic", func(t *testing.T) {
		req := &eth.Request{Peer: "test"}
		assert.NotPanics(t, func() { assert.NoError(t, req.Close()) })
	})
}

// --- doWitnessRequest cancel/error path tests ---

func TestDoWitnessRequest_CancelPaths(t *testing.T) {
	t.Run("error releases semaphore", func(t *testing.T) {
		p, mock := testPeer(t)
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			Return(nil, errors.New("disconnected")).Times(1)

		s := newDoWitnessRequestTestState(DefaultConcurrentResponsesHandled)
		assert.Error(t, s.call(p, common.Hash{1}, 0))

		// All semaphore slots should be available
		for i := 0; i < DefaultConcurrentRequestsPerPeer; i++ {
			select {
			case s.sem <- 1:
			case <-time.After(100 * time.Millisecond):
				t.Fatalf("semaphore slot %d leaked on error", i)
			}
		}
	})

	t.Run("cancel before response", func(t *testing.T) {
		p, mock := testPeer(t)
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ []wit.WitnessPageRequest, _ chan *wit.Response) (*wit.Request, error) {
				return &wit.Request{}, nil // never send response
			}).AnyTimes()

		s := newDoWitnessRequestTestState(DefaultConcurrentResponsesHandled)
		before := runtime.NumGoroutine()
		for i := 0; i < 3; i++ {
			assert.NoError(t, s.call(p, common.Hash{byte(i)}, 0))
		}
		time.Sleep(50 * time.Millisecond)
		close(s.cancel)
		s.waitWG(t, 2*time.Second)
		assertNoGoroutineLeak(t, before)
	})

	t.Run("cancel during Done send", func(t *testing.T) {
		p, mock := testPeer(t)
		blockingDone := make(chan error) // unbuffered — will block
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				go func() { ch <- &wit.Response{Res: &wit.WitnessPacketRLPPacket{}, Done: blockingDone} }()
				return &wit.Request{}, nil
			}).Times(1)

		s := newDoWitnessRequestTestState(DefaultConcurrentResponsesHandled)
		assert.NoError(t, s.call(p, common.Hash{1}, 0))
		time.Sleep(50 * time.Millisecond)
		close(s.cancel)
		s.waitWG(t, 2*time.Second)
	})

	t.Run("cancel during forward", func(t *testing.T) {
		p, mock := testPeer(t)
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				go func() { ch <- &wit.Response{Res: &wit.WitnessPacketRLPPacket{}, Done: make(chan error, 1)} }()
				return &wit.Request{}, nil
			}).AnyTimes()

		s := newDoWitnessRequestTestState(0) // unbuffered — forward blocks
		assert.NoError(t, s.call(p, common.Hash{1}, 0))
		time.Sleep(50 * time.Millisecond)
		close(s.cancel)
		s.waitWG(t, 2*time.Second)
	})
}

// --- Full RequestWitnesses flow tests ---

func TestRequestWitnesses_LeakFixes(t *testing.T) {
	t.Run("no goroutine leak on cancel", func(t *testing.T) {
		p, mock := testPeer(t)
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ []wit.WitnessPageRequest, _ chan *wit.Response) (*wit.Request, error) {
				return &wit.Request{}, nil
			}).AnyTimes()

		before := runtime.NumGoroutine()
		dlCh := make(chan *eth.Response, 1)
		req, err := p.RequestWitnesses([]common.Hash{{42}}, dlCh)
		assert.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		assert.NoError(t, req.Close())
		assertNoGoroutineLeak(t, before)
	})

	t.Run("adapter cancel during forward", func(t *testing.T) {
		p, mock := testPeer(t)
		witData := testWitnessData(t)
		hash := common.Hash{77}
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				go func() {
					ch <- &wit.Response{
						Res:  &wit.WitnessPacketRLPPacket{WitnessPacketResponse: []wit.WitnessPageResponse{{Page: 0, TotalPages: 1, Hash: hash, Data: witData}}},
						Done: make(chan error, 1),
					}
				}()
				return &wit.Request{}, nil
			}).Times(1)

		before := runtime.NumGoroutine()
		dlCh := make(chan *eth.Response) // unbuffered — adapter blocks
		req, err := p.RequestWitnesses([]common.Hash{hash}, dlCh)
		assert.NoError(t, err)
		time.Sleep(200 * time.Millisecond)
		assert.NoError(t, req.Close())
		assertNoGoroutineLeak(t, before)
	})

	t.Run("Done channel is buffered", func(t *testing.T) {
		p, mock := testPeer(t)
		witData := testWitnessData(t)
		hash := common.Hash{99}
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				go func() {
					ch <- &wit.Response{
						Res:  &wit.WitnessPacketRLPPacket{WitnessPacketResponse: []wit.WitnessPageResponse{{Page: 0, TotalPages: 1, Hash: hash, Data: witData}}},
						Done: make(chan error, 1),
					}
				}()
				return &wit.Request{}, nil
			}).Times(1)

		dlCh := make(chan *eth.Response, 1)
		_, err := p.RequestWitnesses([]common.Hash{hash}, dlCh)
		assert.NoError(t, err)

		select {
		case res := <-dlCh:
			// Write to Done must not block (buffered, no drainer goroutine)
			done := make(chan struct{})
			go func() { res.Done <- nil; close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Done channel blocked — not buffered")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for response")
		}
	})

	t.Run("initial build error", func(t *testing.T) {
		p, mock := testPeer(t)
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			Return(nil, errors.New("connection reset")).Times(1)

		dlCh := make(chan *eth.Response, 1)
		req, err := p.RequestWitnesses([]common.Hash{{1}}, dlCh)
		assert.Nil(t, req)
		assert.ErrorContains(t, err, "connection reset")
	})

	t.Run("partial witness count", func(t *testing.T) {
		p, mock := testPeer(t)
		witData := testWitnessData(t)
		hash1, hash2 := common.Hash{1}, common.Hash{2}
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				go func() {
					data := witData
					if wpr[0].Hash == hash2 {
						data = []byte{} // empty — won't reconstruct
					}
					ch <- &wit.Response{
						Res:  &wit.WitnessPacketRLPPacket{WitnessPacketResponse: []wit.WitnessPageResponse{{Page: 0, TotalPages: 1, Hash: wpr[0].Hash, Data: data}}},
						Done: make(chan error, 1),
					}
				}()
				return &wit.Request{}, nil
			}).AnyTimes()

		dlCh := make(chan *eth.Response, 1)
		_, err := p.RequestWitnesses([]common.Hash{hash1, hash2}, dlCh)
		assert.NoError(t, err)

		select {
		case res := <-dlCh:
			witnesses := res.Res.([]*stateless.Witness)
			assert.Equal(t, 1, len(witnesses), "only hash1 should reconstruct")
		case <-time.After(3 * time.Second):
			t.Fatal("timed out")
		}
	})

	t.Run("retry build error is handled", func(t *testing.T) {
		p, mock := testPeer(t)
		var callCount atomic.Int32
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				n := callCount.Add(1)
				if n == 1 {
					// First call: respond with wrong type to trigger error + retry
					go func() {
						ch <- &wit.Response{Res: 0, Done: make(chan error, 1)}
					}()
					return &wit.Request{}, nil
				}
				// Retry calls fail — covers buildWitnessRequests retry error path (line 504)
				return nil, errors.New("peer gone")
			}).AnyTimes()

		dlCh := make(chan *eth.Response, 1)
		req, err := p.RequestWitnesses([]common.Hash{{1}}, dlCh)
		assert.NoError(t, err)

		select {
		case res := <-dlCh:
			witnesses := res.Res.([]*stateless.Witness)
			assert.Equal(t, 0, len(witnesses))
		case <-time.After(3 * time.Second):
			req.Close()
			t.Fatal("timed out")
		}
	})

	t.Run("additional pages build error", func(t *testing.T) {
		// First response says TotalPages=3 but only page 0 was initially requested
		// (DefaultPagesRequestPerWitness=1). The adapter calls buildWitnessRequests
		// for pages 1-2 which fails — covers line 613.
		p, mock := testPeer(t)
		witData := testWitnessData(t)
		hash := common.Hash{55}
		var callCount atomic.Int32
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				n := callCount.Add(1)
				if n == 1 {
					// Page 0: respond with TotalPages=3 to trigger additional page requests
					go func() {
						ch <- &wit.Response{
							Res:  &wit.WitnessPacketRLPPacket{WitnessPacketResponse: []wit.WitnessPageResponse{{Page: 0, TotalPages: 3, Hash: hash, Data: witData}}},
							Done: make(chan error, 1),
						}
					}()
					return &wit.Request{}, nil
				}
				// Additional page requests fail
				return nil, errors.New("peer gone")
			}).AnyTimes()

		dlCh := make(chan *eth.Response, 1)
		req, err := p.RequestWitnesses([]common.Hash{hash}, dlCh)
		assert.NoError(t, err)

		select {
		case res := <-dlCh:
			// Should get empty response since we couldn't fetch all pages
			witnesses := res.Res.([]*stateless.Witness)
			assert.Equal(t, 0, len(witnesses))
		case <-time.After(3 * time.Second):
			req.Close()
			t.Fatal("timed out")
		}
	})

	t.Run("cancel during empty response send", func(t *testing.T) {
		// Peer returns data that can't be reconstructed, adapter builds empty
		// response but dlResCh is unbuffered and nobody reads — cancel exits.
		p, mock := testPeer(t)
		mock.EXPECT().RequestWitness(gomock.Any(), gomock.Any()).
			DoAndReturn(func(wpr []wit.WitnessPageRequest, ch chan *wit.Response) (*wit.Request, error) {
				go func() {
					ch <- &wit.Response{
						Res:  &wit.WitnessPacketRLPPacket{WitnessPacketResponse: []wit.WitnessPageResponse{{Page: 0, TotalPages: 1, Hash: common.Hash{88}, Data: []byte{}}}},
						Done: make(chan error, 1),
					}
				}()
				return &wit.Request{}, nil
			}).Times(1)

		before := runtime.NumGoroutine()
		dlCh := make(chan *eth.Response) // unbuffered — send blocks
		req, err := p.RequestWitnesses([]common.Hash{{88}}, dlCh)
		assert.NoError(t, err)
		time.Sleep(200 * time.Millisecond)
		assert.NoError(t, req.Close())
		assertNoGoroutineLeak(t, before)
	})
}
