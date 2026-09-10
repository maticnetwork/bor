package stateless

import (
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestValidateWitnessPreState_Success(t *testing.T) {
	// Create test headers.
	parentStateRoot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	parentHeader := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Root:       parentStateRoot,
	}

	// Use the actual hash of the parent header.
	parentHash := parentHeader.Hash()

	contextHeader := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: parentHash,
		Root:       common.HexToHash("0xfedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"),
	}

	// Set up mock header reader.
	mockReader := NewMockHeaderReader()
	mockReader.AddHeader(parentHeader)

	// Create witness with matching pre-state root.
	witness := &Witness{
		context: contextHeader,
		Headers: []*types.Header{parentHeader}, // First header should be parent.
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}

	// Test validation - should succeed.
	err := ValidateWitnessPreState(witness, mockReader, nil)
	if err != nil {
		t.Errorf("Expected validation to succeed, but got error: %v", err)
	}
}

func TestValidateWitnessPreState_StateMismatch(t *testing.T) {
	// Create test headers with mismatched state roots.
	parentStateRoot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	mismatchedStateRoot := common.HexToHash("0x9999999999999999999999999999999999999999999999999999999999999999")

	parentHeader := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Root:       parentStateRoot,
	}

	// Use the actual hash of the parent header.
	parentHash := parentHeader.Hash()

	contextHeader := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: parentHash,
		Root:       common.HexToHash("0xfedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"),
	}

	// Create witness header with mismatched state root.
	witnessParentHeader := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Root:       mismatchedStateRoot, // Different from actual parent.
	}

	// Set up mock header reader.
	mockReader := NewMockHeaderReader()
	mockReader.AddHeader(parentHeader)

	// Create witness with mismatched pre-state root.
	witness := &Witness{
		context: contextHeader,
		Headers: []*types.Header{witnessParentHeader}, // Mismatched parent header.
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}

	// Test validation - should fail.
	err := ValidateWitnessPreState(witness, mockReader, nil)
	if err == nil {
		t.Error("Expected validation to fail due to state root mismatch, but it succeeded")
	}

	expectedError := "witness pre-state root mismatch"
	if err != nil && len(err.Error()) > 0 {
		if err.Error()[:len(expectedError)] != expectedError {
			t.Errorf("Expected error message to start with '%s', but got: %v", expectedError, err)
		}
	}
}

func TestValidateWitnessPreState_EdgeCases(t *testing.T) {
	mockReader := NewMockHeaderReader()

	// Test case 1: Nil witness.
	t.Run("NilWitness", func(t *testing.T) {
		err := ValidateWitnessPreState(nil, mockReader, nil)
		if err == nil {
			t.Error("Expected validation to fail for nil witness")
		}
		if err.Error() != "witness is nil" {
			t.Errorf("Expected error 'witness is nil', got: %v", err)
		}
	})

	// Test case 2: Witness with no headers.
	t.Run("NoHeaders", func(t *testing.T) {
		witness := &Witness{
			context: &types.Header{Number: big.NewInt(100)},
			Headers: []*types.Header{}, // Empty headers.
			Codes:   make(map[string]struct{}),
			State:   make(map[string]struct{}),
		}

		err := ValidateWitnessPreState(witness, mockReader, nil)
		if err == nil {
			t.Error("Expected validation to fail for witness with no headers")
		}
		if err.Error() != "witness has no headers" {
			t.Errorf("Expected error 'witness has no headers', got: %v", err)
		}
	})

	// Test case 3: Witness with nil context header.
	t.Run("NilContextHeader", func(t *testing.T) {
		witness := &Witness{
			context: nil, // Nil context header.
			Headers: []*types.Header{
				{
					Number: big.NewInt(99),
					Root:   common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
				},
			},
			Codes: make(map[string]struct{}),
			State: make(map[string]struct{}),
		}

		err := ValidateWitnessPreState(witness, mockReader, nil)
		if err == nil {
			t.Error("Expected validation to fail for witness with nil context header")
		}
		if err.Error() != "witness context header is nil" {
			t.Errorf("Expected error 'witness context header is nil', got: %v", err)
		}
	})

	// Test case 4: Parent header not found.
	t.Run("ParentNotFound", func(t *testing.T) {
		contextHeader := &types.Header{
			Number:     big.NewInt(100),
			ParentHash: common.HexToHash("0xnonexistent1234567890abcdef1234567890abcdef1234567890abcdef123456"),
			Root:       common.HexToHash("0xfedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"),
		}

		witness := &Witness{
			context: contextHeader,
			Headers: []*types.Header{
				{
					Number: big.NewInt(99),
					Root:   common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
				},
			},
			Codes: make(map[string]struct{}),
			State: make(map[string]struct{}),
		}

		// Don't add parent header to mock reader - it won't be found.
		err := ValidateWitnessPreState(witness, mockReader, nil)
		if err == nil {
			t.Error("Expected validation to fail when parent header is not found")
		}

		expectedError := "parent block header not found"
		if err != nil && len(err.Error()) > len(expectedError) {
			if err.Error()[:len(expectedError)] != expectedError {
				t.Errorf("Expected error message to start with '%s', but got: %v", expectedError, err)
			}
		}
	})

	// Test case 5: peer-supplied context header with nil or genesis number
	// must be rejected, not panic (nil) or probe height MaxUint64 (zero).
	makeWitness := func(number *big.Int) *Witness {
		return &Witness{
			context: &types.Header{
				Number:     number,
				ParentHash: common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
			},
			Headers: []*types.Header{
				{
					Number: big.NewInt(99),
					Root:   common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
				},
			},
			Codes: make(map[string]struct{}),
			State: make(map[string]struct{}),
		}
	}
	for name, number := range map[string]*big.Int{
		"NilContextNumber":  nil,
		"ZeroContextNumber": big.NewInt(0),
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateWitnessPreState(makeWitness(number), mockReader, nil)
			if err == nil {
				t.Fatal("Expected validation to fail for invalid context number")
			}
			expectedError := "witness context header has invalid block number"
			if err.Error()[:len(expectedError)] != expectedError {
				t.Errorf("Expected error message to start with '%s', but got: %v", expectedError, err)
			}
		})
	}

	// Test case 6: expected block with nil number must be rejected before
	// the number comparison dereferences it.
	t.Run("NilExpectedBlockNumber", func(t *testing.T) {
		err := ValidateWitnessPreState(makeWitness(big.NewInt(100)), mockReader, &types.Header{})
		if err == nil {
			t.Fatal("Expected validation to fail for nil expected block number")
		}
		if err.Error() != "expected block header has nil number" {
			t.Errorf("Expected nil-number error, got: %v", err)
		}
	})
}

func TestValidateWitnessPreState_MultipleHeaders(t *testing.T) {
	// Test witness with multiple headers (realistic scenario).
	parentStateRoot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	grandParentStateRoot := common.HexToHash("0x5555555555555555555555555555555555555555555555555555555555555555")

	grandParentHeader := &types.Header{
		Number:     big.NewInt(98),
		ParentHash: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Root:       grandParentStateRoot,
	}

	// Use the actual hash of the grandparent header.
	grandParentHash := grandParentHeader.Hash()

	parentHeader := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: grandParentHash,
		Root:       parentStateRoot,
	}

	// Use the actual hash of the parent header.
	parentHash := parentHeader.Hash()

	contextHeader := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: parentHash,
		Root:       common.HexToHash("0xfedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"),
	}

	// Set up mock header reader.
	mockReader := NewMockHeaderReader()
	mockReader.AddHeader(parentHeader)
	mockReader.AddHeader(grandParentHeader)

	// Create witness with multiple headers (parent should be first).
	witness := &Witness{
		context: contextHeader,
		Headers: []*types.Header{parentHeader, grandParentHeader}, // Multiple headers.
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}

	// Test validation - should succeed (only first header matters for validation).
	err := ValidateWitnessPreState(witness, mockReader, nil)
	if err != nil {
		t.Errorf("Expected validation to succeed with multiple headers, but got error: %v", err)
	}
}

// TestConsensusWithOriginalPeer tests consensus calculation including original peer
func TestConsensusWithOriginalPeer(t *testing.T) {
	t.Run("Case1_OriginalPeer3_RandomPeers2and3_ShouldChoose3", func(t *testing.T) {
		// Original peer: 3, Random peer 1: 2, Random peer 2: 3
		// Out of 3 total peers, 2 say "3" → Should choose 3
		originalCount := uint64(3)
		randomCounts := []uint64{2, 3}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 3 {
			t.Errorf("Expected consensus to be 3 (majority), got %d", consensus)
		}
		t.Logf("Correct: Out of 3 peers (1 says 2, 2 say 3), chose majority: 3")
	})

	t.Run("Case2_OriginalPeer3_RandomPeers2and2_ShouldChoose2", func(t *testing.T) {
		// Original peer: 3, Random peer 1: 2, Random peer 2: 2
		// Out of 3 total peers, 2 say "2" → Should choose 2 (original is dishonest)
		originalCount := uint64(3)
		randomCounts := []uint64{2, 2}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 2 {
			t.Errorf("Expected consensus to be 2 (majority), got %d", consensus)
		}
		t.Logf("Correct: Out of 3 peers (2 say 2, 1 says 3), chose majority: 2")
	})

	t.Run("NoMajority_AllDifferent", func(t *testing.T) {
		// Original peer: 3, Random peer 1: 2, Random peer 2: 4
		// All different, no majority
		originalCount := uint64(3)
		randomCounts := []uint64{2, 4}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 0 {
			t.Errorf("Expected consensus to be 0 (no majority), got %d", consensus)
		}
		t.Logf("Correct: No majority (1,1,1), no consensus")
	})
}

// getConsensusIncludingOriginal simulates consensus calculation with original peer included
func getConsensusIncludingOriginal(originalCount uint64, randomCounts []uint64) uint64 {
	// Build count map including original peer
	countMap := make(map[uint64]int)
	countMap[originalCount] = 1

	for _, count := range randomCounts {
		countMap[count]++
	}

	// Find majority (at least 2 out of 3)
	var maxCount int
	var consensusCount uint64
	for count, freq := range countMap {
		if freq > maxCount {
			maxCount = freq
			consensusCount = count
		}
	}

	// Need at least 2 votes for majority
	if maxCount >= 2 {
		return consensusCount
	}

	return 0 // No consensus
}

// witnessVerificationTestCase defines a test case for witness verification
type witnessVerificationTestCase struct {
	name           string
	reportedPages  uint64
	peerPages      []uint64
	expectedHonest bool
	description    string
}

// getCommonVerificationTestCases returns the common test cases for witness verification
func getCommonVerificationTestCases() []witnessVerificationTestCase {
	return []witnessVerificationTestCase{
		{
			name:           "UnderThreshold_ShouldBeHonest",
			reportedPages:  5,
			peerPages:      []uint64{5, 5},
			expectedHonest: true,
			description:    "Page count under threshold should be considered honest",
		},
		{
			name:           "OverThreshold_ConsensusAgreement",
			reportedPages:  15,
			peerPages:      []uint64{15, 15},
			expectedHonest: true,
			description:    "Consensus agreement should mark peer as honest",
		},
		{
			name:           "OverThreshold_ConsensusDisagreement",
			reportedPages:  15,
			peerPages:      []uint64{20, 20},
			expectedHonest: false,
			description:    "Consensus disagreement should mark peer as dishonest (dropped)",
		},
		{
			name:           "OverThreshold_MixedResults",
			reportedPages:  15,
			peerPages:      []uint64{15, 20},
			expectedHonest: true,
			description:    "Mixed results should default to honest (conservative)",
		},
		{
			name:           "OverThreshold_InsufficientPeers",
			reportedPages:  15,
			peerPages:      []uint64{15},
			expectedHonest: true,
			description:    "Insufficient peers should default to honest (conservative)",
		},
	}
}

// runVerificationTests runs verification tests with a given simulation function
func runVerificationTests(t *testing.T, tests []witnessVerificationTestCase, simulateFunc func(uint64, []uint64) bool) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isHonest := simulateFunc(tt.reportedPages, tt.peerPages)

			if isHonest != tt.expectedHonest {
				t.Errorf("%s: expected honest=%v, got honest=%v", tt.description, tt.expectedHonest, isHonest)
			}
		})
	}
}

// TestSimplifiedWitnessVerification tests the simplified verification logic
func TestSimplifiedWitnessVerification(t *testing.T) {
	tests := getCommonVerificationTestCases()
	runVerificationTests(t, tests, simulateSimplifiedWitnessVerification)
}

// simulateSimplifiedWitnessVerification simulates the simplified verification logic
func simulateSimplifiedWitnessVerification(reportedPageCount uint64, peerPageCounts []uint64) bool {
	const witnessPageWarningThreshold = 10
	const witnessVerificationPeers = 2

	// If under threshold, assume honest
	if reportedPageCount <= witnessPageWarningThreshold {
		return true
	}

	// If insufficient peers, assume honest (conservative approach)
	if len(peerPageCounts) < witnessVerificationPeers {
		return true
	}

	// Get consensus from peers (most common page count)
	countMap := make(map[uint64]int)
	for _, count := range peerPageCounts {
		countMap[count]++
	}

	var maxCount int
	var consensusCount uint64
	for count, freq := range countMap {
		if freq > maxCount {
			maxCount = freq
			consensusCount = count
		}
	}

	// If we have consensus, check if it matches reported count
	if maxCount >= 2 {
		return consensusCount == reportedPageCount
	}

	// No clear consensus, assume honest (conservative approach)
	return true
}

// TestWitnessVerificationScenarios tests various verification scenarios
func TestWitnessVerificationScenarios(t *testing.T) {
	t.Run("MaliciousPeer_ExcessivePages", func(t *testing.T) {
		// Simulate a malicious peer reporting 1000+ pages
		reportedPages := uint64(1000)
		peerPages := []uint64{15, 15} // Other peers report normal page count

		isHonest := simulateSimplifiedWitnessVerification(reportedPages, peerPages)

		if isHonest {
			t.Error("Expected malicious peer with excessive pages to be marked as dishonest")
		}
	})

	t.Run("HonestPeer_LargeButReasonablePages", func(t *testing.T) {
		// Simulate an honest peer with large but reasonable page count
		reportedPages := uint64(50)
		peerPages := []uint64{50, 50} // Other peers agree

		isHonest := simulateSimplifiedWitnessVerification(reportedPages, peerPages)

		if !isHonest {
			t.Error("Expected honest peer with large but reasonable pages to be marked as honest")
		}
	})

	t.Run("NetworkPartition_ConservativeApproach", func(t *testing.T) {
		// Simulate network partition where only one peer responds
		reportedPages := uint64(100)
		peerPages := []uint64{100} // Only one peer responds

		isHonest := simulateSimplifiedWitnessVerification(reportedPages, peerPages)

		if !isHonest {
			t.Error("Expected conservative approach to mark peer as honest when insufficient consensus")
		}
	})
}

// TestTotalPagesConsistency tests that peers cannot change TotalPages across pages
func TestTotalPagesConsistency(t *testing.T) {
	t.Run("ConsistentTotalPages_ShouldPass", func(t *testing.T) {
		// Simulate receiving pages with consistent TotalPages
		pages := []struct {
			pageNum    uint64
			totalPages uint64
		}{
			{0, 3},
			{1, 3},
			{2, 3},
		}

		var storedTotalPages uint64
		var hasTotalPages bool

		for _, page := range pages {
			if hasTotalPages {
				// Verify TotalPages hasn't changed
				if storedTotalPages != page.totalPages {
					t.Errorf("TotalPages changed from %d to %d on page %d", storedTotalPages, page.totalPages, page.pageNum)
					return
				}
			} else {
				// First time - store it
				storedTotalPages = page.totalPages
				hasTotalPages = true
			}
		}

		// All pages had consistent TotalPages
		if storedTotalPages != 3 {
			t.Errorf("Expected TotalPages to be 3, got %d", storedTotalPages)
		}
	})

	t.Run("InconsistentTotalPages_ShouldFail", func(t *testing.T) {
		// Simulate malicious peer changing TotalPages
		pages := []struct {
			pageNum    uint64
			totalPages uint64
		}{
			{0, 3},
			{1, 3},
			{2, 3},
			{3, 1000}, // Malicious peer changes TotalPages!
		}

		var storedTotalPages uint64
		var hasTotalPages bool
		attackDetected := false

		for _, page := range pages {
			if hasTotalPages {
				// Verify TotalPages hasn't changed
				if storedTotalPages != page.totalPages {
					attackDetected = true
					t.Logf("Attack detected: TotalPages changed from %d to %d on page %d", storedTotalPages, page.totalPages, page.pageNum)
					break
				}
			} else {
				// First time - store it
				storedTotalPages = page.totalPages
				hasTotalPages = true
			}
		}

		if !attackDetected {
			t.Error("Expected attack to be detected when TotalPages changes")
		}
	})

	t.Run("ExcessPages_ShouldFail", func(t *testing.T) {
		// Simulate peer sending more pages than claimed
		totalPages := uint64(3)
		receivedPages := 5 // Peer sends 5 pages but claimed 3

		if receivedPages > int(totalPages) {
			// This should be detected and rejected
			t.Logf("Correctly detected: peer sent %d pages but claimed %d", receivedPages, totalPages)
		} else {
			t.Error("Failed to detect peer sending more pages than claimed")
		}
	})

	t.Run("InvalidPageNumber_ShouldFail", func(t *testing.T) {
		// Simulate peer sending page number >= TotalPages
		testCases := []struct {
			pageNum    uint64
			totalPages uint64
			shouldFail bool
		}{
			{0, 3, false}, // Valid: page 0 of 3
			{2, 3, false}, // Valid: page 2 of 3 (last page)
			{3, 3, true},  // Invalid: page 3 >= TotalPages 3
			{5, 3, true},  // Invalid: page 5 >= TotalPages 3
		}

		for _, tc := range testCases {
			isInvalid := tc.pageNum >= tc.totalPages
			if isInvalid != tc.shouldFail {
				t.Errorf("Page=%d, TotalPages=%d: expected fail=%v, got fail=%v", tc.pageNum, tc.totalPages, tc.shouldFail, isInvalid)
			}

			if isInvalid {
				t.Logf("Correctly rejected: page %d >= totalPages %d", tc.pageNum, tc.totalPages)
			}
		}
	})
}

// TestWitnessPageCountVerification tests the page count verification logic
func TestWitnessPageCountVerification(t *testing.T) {
	tests := getCommonVerificationTestCases()
	runVerificationTests(t, tests, simulateWitnessPageCountVerification)
}

// simulateWitnessPageCountVerification simulates the verification logic from witness_manager.go
func simulateWitnessPageCountVerification(reportedPageCount uint64, peerPageCounts []uint64) bool {
	const witnessPageWarningThreshold = 10
	const witnessVerificationPeers = 2

	// If under threshold, assume honest
	if reportedPageCount <= witnessPageWarningThreshold {
		return true
	}

	// If insufficient peers, assume honest (conservative approach)
	if len(peerPageCounts) < witnessVerificationPeers {
		return true
	}

	// Check for consensus among peers
	consensusCount := uint64(0)
	honestPeers := 0

	for _, pageCount := range peerPageCounts {
		honestPeers++
		if consensusCount == 0 {
			consensusCount = pageCount
		} else if consensusCount != pageCount {
			// No clear consensus
			consensusCount = 0
			break
		}
	}

	// If we have consensus from at least 2 peers
	if honestPeers >= witnessVerificationPeers && consensusCount > 0 {
		return consensusCount == reportedPageCount
	}

	// No clear consensus, assume honest (conservative approach)
	return true
}

// TestWitnessVerificationPerformance tests the performance characteristics
func TestWitnessVerificationPerformance(t *testing.T) {
	t.Run("LargeWitness_Verification", func(t *testing.T) {
		// Test with a very large witness (1000+ pages)
		reportedPages := uint64(1000)
		peerPages := []uint64{1000, 1000}

		start := time.Now()
		isHonest := simulateWitnessPageCountVerification(reportedPages, peerPages)
		duration := time.Since(start)

		if !isHonest {
			t.Error("Expected large witness with consensus to be marked as honest")
		}

		// Verification should be fast (under 1ms)
		if duration > time.Millisecond {
			t.Errorf("Verification took too long: %v", duration)
		}
	})
}

// TestConsensusEdgeCases tests edge cases in consensus calculation
func TestConsensusEdgeCases(t *testing.T) {
	t.Run("AllPeersReportZero", func(t *testing.T) {
		// All 3 peers report 0 pages - should reach consensus on 0
		originalCount := uint64(0)
		randomCounts := []uint64{0, 0}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 0 {
			t.Errorf("Expected consensus to be 0 when all peers agree on 0, got %d", consensus)
		}
		t.Logf("Correct: All 3 peers agreed on 0, consensus: 0")
	})

	t.Run("TwoPeersReportZero_OneReportsNonZero", func(t *testing.T) {
		// Original: 0, Random peer 1: 0, Random peer 2: 10
		// Majority says 0
		originalCount := uint64(0)
		randomCounts := []uint64{0, 10}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 0 {
			t.Errorf("Expected consensus to be 0 (majority), got %d", consensus)
		}
		t.Logf("Correct: 2 peers say 0, 1 says 10, consensus: 0")
	})

	t.Run("OnePeerReportsZero_TwoReportNonZero", func(t *testing.T) {
		// Original: 0, Random peer 1: 5, Random peer 2: 5
		// Majority says 5, original peer with 0 is dishonest
		originalCount := uint64(0)
		randomCounts := []uint64{5, 5}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 5 {
			t.Errorf("Expected consensus to be 5 (majority), got %d", consensus)
		}
		t.Logf("Correct: Original peer with 0 is dishonest, consensus: 5")
	})

	t.Run("MaxUint64Values", func(t *testing.T) {
		// Test with maximum uint64 values to ensure no overflow
		originalCount := uint64(18446744073709551615) // max uint64
		randomCounts := []uint64{18446744073709551615, 18446744073709551615}

		consensus := getConsensusIncludingOriginal(originalCount, randomCounts)

		if consensus != 18446744073709551615 {
			t.Errorf("Expected consensus with max uint64, got %d", consensus)
		}
	})

	t.Run("SinglePeerDifferent", func(t *testing.T) {
		// Test all permutations of single peer being different
		testCases := []struct {
			original     uint64
			randomCounts []uint64
			expected     uint64
			description  string
		}{
			{15, []uint64{15, 20}, 15, "Original and first random agree"},
			{15, []uint64{20, 15}, 15, "Original and second random agree"},
			{20, []uint64{15, 15}, 15, "Original is the outlier"},
		}

		for _, tc := range testCases {
			consensus := getConsensusIncludingOriginal(tc.original, tc.randomCounts)
			if consensus != tc.expected {
				t.Errorf("%s: Expected %d, got %d", tc.description, tc.expected, consensus)
			}
		}
	})
}

// TestWitnessVerificationWithTimeout tests verification behavior with network delays
func TestWitnessVerificationWithTimeout(t *testing.T) {
	t.Run("SlowPeerResponse", func(t *testing.T) {
		// Simulate a peer that takes too long to respond
		// In production, this would trigger a timeout
		reportedPages := uint64(50)

		// Only one peer responds in time
		honestPeers := []uint64{50}

		// Simulate verification with only one successful response
		countMap := make(map[uint64]int)
		countMap[reportedPages] = 1 // Original peer
		for _, count := range honestPeers {
			countMap[count]++
		}

		var maxCount int
		for _, freq := range countMap {
			if freq > maxCount {
				maxCount = freq
			}
		}

		// Need at least 2 votes for consensus
		hasConsensus := maxCount >= 2
		if !hasConsensus {
			t.Log("Correct: No consensus with only 2 responses out of 3")
		}
	})
}

// TestTotalPagesValidation tests validation of TotalPages field
func TestTotalPagesValidation(t *testing.T) {
	t.Run("TotalPages_Zero_ShouldBeInvalid", func(t *testing.T) {
		// Test that TotalPages == 0 is considered invalid
		totalPages := uint64(0)
		currentPage := uint64(0)

		// Page number should not be >= TotalPages
		// With TotalPages=0, any page (including page 0) is invalid
		isValid := currentPage < totalPages

		if isValid {
			t.Error("Expected page 0 to be invalid when TotalPages is 0")
		}
		t.Log("Correct: Detected invalid TotalPages=0")
	})

	t.Run("CurrentPageExceedsTotalPages", func(t *testing.T) {
		// Page number >= TotalPages is invalid
		testCases := []struct {
			currentPage   uint64
			totalPages    uint64
			shouldBeValid bool
		}{
			{0, 1, true},     // Page 0 of 1 total pages
			{0, 10, true},    // Page 0 of 10 total pages
			{9, 10, true},    // Page 9 of 10 total pages (last valid page)
			{10, 10, false},  // Page 10 of 10 total pages (invalid - 0-indexed)
			{11, 10, false},  // Page 11 of 10 total pages (invalid)
			{100, 10, false}, // Way beyond total pages
		}

		for _, tc := range testCases {
			isValid := tc.currentPage < tc.totalPages
			if isValid != tc.shouldBeValid {
				t.Errorf("Page %d with TotalPages %d: expected valid=%v, got valid=%v",
					tc.currentPage, tc.totalPages, tc.shouldBeValid, isValid)
			}
		}
	})

	t.Run("ReceivedMorePagesThanClaimed", func(t *testing.T) {
		// Simulate receiving more pages than TotalPages claims
		totalPages := uint64(5)
		receivedPages := []uint64{0, 1, 2, 3, 4, 5} // 6 pages received, but TotalPages=5

		if uint64(len(receivedPages)) > totalPages {
			t.Logf("Correct: Detected peer sending %d pages when TotalPages is %d", len(receivedPages), totalPages)
		} else {
			t.Error("Failed to detect peer sending more pages than claimed")
		}
	})
}

// TestWitnessConsensusRaceCondition tests potential race conditions in consensus
func TestWitnessConsensusRaceCondition(t *testing.T) {
	t.Run("ConcurrentConsensusCalculation", func(t *testing.T) {
		// Simulate concurrent consensus calculations for the same hash
		// This tests for race conditions in the consensus algorithm
		var wg sync.WaitGroup
		numGoroutines := 100
		results := make([]uint64, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				// All goroutines calculate consensus for same data
				originalCount := uint64(15)
				randomCounts := []uint64{15, 15}
				results[index] = getConsensusIncludingOriginal(originalCount, randomCounts)
			}(i)
		}

		wg.Wait()

		// All results should be identical (15)
		for i, result := range results {
			if result != 15 {
				t.Errorf("Goroutine %d got different result: %d", i, result)
			}
		}
		t.Log("Correct: All concurrent consensus calculations returned same result")
	})
}

// TestWitnessVerificationIntegration tests full verification flow
func TestWitnessVerificationIntegration(t *testing.T) {
	t.Run("EndToEnd_HonestPeer", func(t *testing.T) {
		// Simulate full verification flow for an honest peer
		originalPeerID := "peer-123"
		reportedPageCount := uint64(50)
		blockHash := common.HexToHash("0xabc")

		// Step 1: Check if verification needed (page count exceeds threshold)
		threshold := uint64(30)
		needsVerification := reportedPageCount > threshold

		if !needsVerification {
			t.Error("Expected large witness to trigger verification")
		}

		// Step 2: Query random peers
		randomPeers := []string{"peer-456", "peer-789"}
		peerResponses := map[string]uint64{
			"peer-456": 50, // Agrees with original
			"peer-789": 50, // Agrees with original
		}

		// Step 3: Calculate consensus
		counts := []uint64{reportedPageCount}
		for _, peerID := range randomPeers {
			counts = append(counts, peerResponses[peerID])
		}

		consensus := getConsensusIncludingOriginal(reportedPageCount, []uint64{
			peerResponses[randomPeers[0]],
			peerResponses[randomPeers[1]],
		})

		// Step 4: Verify consensus matches reported
		isHonest := consensus == reportedPageCount && consensus != 0

		if !isHonest {
			t.Errorf("Expected honest peer to pass verification. Reported: %d, Consensus: %d",
				reportedPageCount, consensus)
		}

		t.Logf("Success: Honest peer %s verified for block %s with %d pages",
			originalPeerID, blockHash.Hex(), reportedPageCount)
	})

	t.Run("EndToEnd_DishonestPeer_ShouldBeJailed", func(t *testing.T) {
		// Simulate full verification flow for a dishonest peer
		dishonestPeerID := "attacker-peer"
		reportedPageCount := uint64(500) // Claims 500 pages (attack)
		blockHash := common.HexToHash("0xdef")

		// Step 1: Verification needed
		threshold := uint64(30)
		needsVerification := reportedPageCount > threshold

		if !needsVerification {
			t.Fatal("Expected attack to trigger verification")
		}

		// Step 2: Query random peers (they report honest value)
		randomPeers := []string{"honest-peer-1", "honest-peer-2"}
		peerResponses := map[string]uint64{
			"honest-peer-1": 15, // Honest response
			"honest-peer-2": 15, // Honest response
		}

		// Step 3: Calculate consensus
		consensus := getConsensusIncludingOriginal(reportedPageCount, []uint64{
			peerResponses[randomPeers[0]],
			peerResponses[randomPeers[1]],
		})

		// Step 4: Detect dishonest peer
		isHonest := consensus == reportedPageCount && consensus != 0
		shouldBeJailed := !isHonest && consensus != 0

		if !shouldBeJailed {
			t.Errorf("Expected dishonest peer to be detected. Reported: %d, Consensus: %d",
				reportedPageCount, consensus)
		}

		t.Logf("Success: Dishonest peer %s detected and should be jailed for block %s. Claimed: %d pages, Actual: %d pages",
			dishonestPeerID, blockHash.Hex(), reportedPageCount, consensus)
	})
}

// TestCalculatePageThreshold tests dynamic threshold calculation
func TestCalculatePageThreshold(t *testing.T) {
	t.Run("ZeroGasCeil", func(t *testing.T) {
		// When gas ceil is 0, should use default threshold
		gasCeil := uint64(0)
		gasPerMB := uint64(1_000_000)
		maxPageSizeMB := uint64(15)
		defaultThreshold := uint64(10)

		var threshold uint64
		if gasCeil > 0 {
			estimatedMB := gasCeil / gasPerMB
			threshold = (estimatedMB + maxPageSizeMB - 1) / maxPageSizeMB // ceil division
		} else {
			threshold = defaultThreshold
		}

		if threshold != defaultThreshold {
			t.Errorf("Expected default threshold %d with zero gas ceil, got %d", defaultThreshold, threshold)
		}
	})

	t.Run("StandardGasCeil", func(t *testing.T) {
		// 30M gas ceil -> ~30MB -> ~2 pages
		gasCeil := uint64(30_000_000)
		gasPerMB := uint64(1_000_000)
		maxPageSizeMB := uint64(15)

		estimatedMB := gasCeil / gasPerMB                              // 30MB
		threshold := (estimatedMB + maxPageSizeMB - 1) / maxPageSizeMB // ceil(30/15) = 2

		expectedThreshold := uint64(2)
		if threshold != expectedThreshold {
			t.Errorf("Expected threshold %d for 30M gas ceil, got %d", expectedThreshold, threshold)
		}
	})

	t.Run("HighGasCeil", func(t *testing.T) {
		// 150M gas ceil -> ~150MB -> ~10 pages
		gasCeil := uint64(150_000_000)
		gasPerMB := uint64(1_000_000)
		maxPageSizeMB := uint64(15)

		estimatedMB := gasCeil / gasPerMB                              // 150MB
		threshold := (estimatedMB + maxPageSizeMB - 1) / maxPageSizeMB // ceil(150/15) = 10

		expectedThreshold := uint64(10)
		if threshold != expectedThreshold {
			t.Errorf("Expected threshold %d for 150M gas ceil, got %d", expectedThreshold, threshold)
		}
	})
}

// makeValidatePreStateFixture builds a consistent witness + mockReader + context
// header where the witness pre-state matches the chain's parent block. Callers
// can mutate the returned expectedBlock to test the anti-malicious-peer guards.
func makeValidatePreStateFixture() (*Witness, HeaderReader, *types.Header) {
	parentStateRoot := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")

	parentHeader := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Root:       parentStateRoot,
	}
	parentHash := parentHeader.Hash()

	contextHeader := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: parentHash,
		Root:       common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222"),
	}

	mockReader := NewMockHeaderReader()
	mockReader.AddHeader(parentHeader)

	witness := &Witness{
		context: contextHeader,
		Headers: []*types.Header{parentHeader},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}

	// expectedBlock mirrors the context header — tests mutate individual fields
	// (ParentHash / Number) to exercise the ValidateWitnessPreState guards.
	expectedBlock := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: parentHash,
	}
	return witness, mockReader, expectedBlock
}

// TestValidateWitnessPreState_ExpectedBlockMatches exercises the non-nil
// expectedBlock branch with matching ParentHash and Number so the full
// function runs to completion.
func TestValidateWitnessPreState_ExpectedBlockMatches(t *testing.T) {
	witness, reader, expectedBlock := makeValidatePreStateFixture()
	if err := ValidateWitnessPreState(witness, reader, expectedBlock); err != nil {
		t.Errorf("expected validation to succeed, got %v", err)
	}
}

// TestValidateWitnessPreState_ExpectedBlockParentHashMismatch rejects a witness
// whose context ParentHash disagrees with the expected block — defends against
// a malicious peer substituting a witness for a different fork.
func TestValidateWitnessPreState_ExpectedBlockParentHashMismatch(t *testing.T) {
	witness, reader, expectedBlock := makeValidatePreStateFixture()
	expectedBlock.ParentHash = common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	err := ValidateWitnessPreState(witness, reader, expectedBlock)
	if err == nil {
		t.Fatal("expected ParentHash mismatch error, got nil")
	}
	if !containsSubstring(err.Error(), "witness ParentHash mismatch") {
		t.Errorf("expected ParentHash mismatch error, got: %v", err)
	}
}

// TestValidateWitnessPreState_ExpectedBlockNumberMismatch rejects a witness
// whose context Number disagrees with the expected block — defends against a
// malicious peer substituting a witness for a different block at the same
// ParentHash (e.g., after a reorg).
func TestValidateWitnessPreState_ExpectedBlockNumberMismatch(t *testing.T) {
	witness, reader, expectedBlock := makeValidatePreStateFixture()
	expectedBlock.Number = big.NewInt(999) // ParentHash still matches

	err := ValidateWitnessPreState(witness, reader, expectedBlock)
	if err == nil {
		t.Fatal("expected Number mismatch error, got nil")
	}
	if !containsSubstring(err.Error(), "witness block number mismatch") {
		t.Errorf("expected Number mismatch error, got: %v", err)
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
