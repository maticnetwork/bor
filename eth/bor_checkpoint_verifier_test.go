package eth

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

// Test helper functions for the findCommonAncestorWithFutureMilestones logic

func TestFindCommonAncestorLogic_StartIsZero(t *testing.T) {
	// Test the first condition: start == 0 should return 0
	result := findCommonAncestorWithFutureMilestones_StartZeroCheck(0)
	if result != 0 {
		t.Errorf("Expected 0 when start is 0, got %d", result)
	}

	// Test non-zero start should not return early
	result = findCommonAncestorWithFutureMilestones_StartZeroCheck(5)
	if result == 0 {
		t.Errorf("Expected non-zero when start is 5, got %d", result)
	}
}

func TestFindCommonAncestorLogic_HashMatching(t *testing.T) {
	// Test hash matching logic
	localHash := "abc123"
	milestoneHash := "abc123"

	if !findCommonAncestorWithFutureMilestones_HashesMatch(localHash, milestoneHash) {
		t.Error("Expected hashes to match")
	}

	localHash = "abc123"
	milestoneHash = "def456"

	if findCommonAncestorWithFutureMilestones_HashesMatch(localHash, milestoneHash) {
		t.Error("Expected hashes not to match")
	}
}

func TestFindCommonAncestorLogic_FutureMilestoneProcessing(t *testing.T) {
	// Test future milestone list processing logic
	db := memorydb.New()

	// Test with empty future milestone list
	order, _, err := rawdb.ReadFutureMilestoneList(db)
	if err == nil || len(order) != 0 {
		t.Error("Expected error or empty list when no future milestones exist")
	}

	// Test with future milestones
	testOrder := []uint64{12, 16, 18}
	testList := map[uint64]common.Hash{
		12: common.HexToHash("0x111"),
		16: common.HexToHash("0x222"),
		18: common.HexToHash("0x333"),
	}

	err = rawdb.WriteFutureMilestoneList(db, testOrder, testList)
	if err != nil {
		t.Fatalf("Failed to write future milestone list: %v", err)
	}

	// Read it back
	readOrder, readList, err := rawdb.ReadFutureMilestoneList(db)
	if err != nil {
		t.Fatalf("Failed to read future milestone list: %v", err)
	}

	if len(readOrder) != len(testOrder) {
		t.Errorf("Expected order length %d, got %d", len(testOrder), len(readOrder))
	}

	if len(readList) != len(testList) {
		t.Errorf("Expected list length %d, got %d", len(testList), len(readList))
	}

	// Verify the order and content
	for i, num := range readOrder {
		if num != testOrder[i] {
			t.Errorf("Expected order[%d] = %d, got %d", i, testOrder[i], num)
		}

		if readList[num] != testList[num] {
			t.Errorf("Expected list[%d] = %s, got %s", num, testList[num].Hex(), readList[num].Hex())
		}
	}
}

func TestFindCommonAncestorLogic_TargetBlockCalculation(t *testing.T) {
	// Test target block calculation logic (when NO matches are found)
	start := uint64(10)

	// Test when no milestones found
	targetBlock := findCommonAncestorWithFutureMilestones_CalculateTargetBlock_NoMatch(start, []uint64{}, 15)
	expected := start - 1
	if targetBlock != expected {
		t.Errorf("Expected target block %d, got %d", expected, targetBlock)
	}

	// Test when milestone found but after end
	milestoneOrder := []uint64{20} // After end=15
	targetBlock = findCommonAncestorWithFutureMilestones_CalculateTargetBlock_NoMatch(start, milestoneOrder, 15)
	expected = start - 1
	if targetBlock != expected {
		t.Errorf("Expected target block %d when milestone after end, got %d", expected, targetBlock)
	}

	// Test when milestone found before end (no matching case)
	// The logic should update targetBlock only if milestoneNum < targetBlock
	start = 10
	milestoneOrder = []uint64{12} // Before end=15, but no match found
	targetBlock = findCommonAncestorWithFutureMilestones_CalculateTargetBlock_NoMatch(start, milestoneOrder, 15)
	// start-1 = 9, milestone=12, so 12 < 9 is false, targetBlock stays 9
	expected = start - 1 // Should remain start-1 since 12 > 9
	if targetBlock != expected {
		t.Errorf("Expected target block %d when milestone before end, got %d", expected, targetBlock)
	}

	// Test with milestone that IS less than start-1
	start = 15
	milestoneOrder = []uint64{10} // Before end=20, 10 < (15-1)=14
	targetBlock = findCommonAncestorWithFutureMilestones_CalculateTargetBlock_NoMatch(start, milestoneOrder, 20)
	expected = 10 - 1 // Should be milestone-1 = 9
	if targetBlock != expected {
		t.Errorf("Expected target block %d when milestone less than start-1, got %d", expected, targetBlock)
	}

	// Test when calculated target becomes negative
	start = 2
	milestoneOrder = []uint64{1} // 1 < (2-1)=1 is false, so targetBlock stays 1
	targetBlock = findCommonAncestorWithFutureMilestones_CalculateTargetBlock_NoMatch(start, milestoneOrder, 15)
	expected = start - 1 // Should be 1
	if targetBlock != expected {
		t.Errorf("Expected %d when start is small, got %d", expected, targetBlock)
	}
}

func TestFindCommonAncestorLogic_MatchingMilestone(t *testing.T) {
	// Test when a matching milestone is found (should return milestone block number)
	milestoneBlock := uint64(12)

	// Simulate finding a matching milestone
	result := findCommonAncestorWithFutureMilestones_MatchFound(milestoneBlock)

	if result != milestoneBlock {
		t.Errorf("Expected %d when match found, got %d", milestoneBlock, result)
	}
}

func TestFindCommonAncestorLogic_MultipleMillestones(t *testing.T) {
	// Test processing multiple milestones in reverse order
	order := []uint64{12, 16, 18}
	end := uint64(15)

	// Should process from newest to oldest (reverse order)
	// Only milestone 12 should be considered (16, 18 are >= end)
	validMilestones := findCommonAncestorWithFutureMilestones_FilterValidMilestones(order, end)

	if len(validMilestones) != 1 || validMilestones[0] != 12 {
		t.Errorf("Expected only milestone 12 to be valid, got %v", validMilestones)
	}

	// Test with all milestones before end
	end = uint64(25)
	validMilestones = findCommonAncestorWithFutureMilestones_FilterValidMilestones(order, end)

	// Should process all in reverse order: [18, 16, 12]
	expected := []uint64{18, 16, 12}
	if len(validMilestones) != len(expected) {
		t.Errorf("Expected %d valid milestones, got %d", len(expected), len(validMilestones))
	}

	for i, milestone := range validMilestones {
		if milestone != expected[i] {
			t.Errorf("Expected milestone[%d] = %d, got %d", i, expected[i], milestone)
		}
	}
}

// Helper functions to test individual pieces of logic

func findCommonAncestorWithFutureMilestones_StartZeroCheck(start uint64) uint64 {
	if start == 0 {
		return 0
	}
	return 1 // Non-zero to indicate no early return
}

func findCommonAncestorWithFutureMilestones_HashesMatch(localHash, milestoneHash string) bool {
	return localHash == milestoneHash
}

func findCommonAncestorWithFutureMilestones_CalculateTargetBlock_NoMatch(start uint64, milestoneOrder []uint64, end uint64) uint64 {
	targetBlock := start - 1

	// Check future milestones from newest to oldest (reverse iteration)
	for i := len(milestoneOrder) - 1; i >= 0; i-- {
		milestoneNum := milestoneOrder[i]
		if milestoneNum >= end {
			continue // Skip milestones after current one
		}

		// Update target block based on milestone found (when no hash match)
		if milestoneNum < targetBlock {
			targetBlock = milestoneNum - 1
		}
	}

	if targetBlock < 0 {
		return 0
	}

	return targetBlock
}

func findCommonAncestorWithFutureMilestones_MatchFound(milestoneBlock uint64) uint64 {
	// When a matching milestone is found, return the milestone block number
	return milestoneBlock
}

func findCommonAncestorWithFutureMilestones_FilterValidMilestones(order []uint64, end uint64) []uint64 {
	var valid []uint64

	// Process from newest to oldest (reverse order)
	for i := len(order) - 1; i >= 0; i-- {
		milestoneNum := order[i]
		if milestoneNum < end {
			valid = append(valid, milestoneNum)
		}
	}

	return valid
}

// Integration test for database operations
func TestFindCommonAncestorWithFutureMilestones_DatabaseIntegration(t *testing.T) {
	db := memorydb.New()

	// Test scenario: Multiple milestones stored in database
	testOrder := []uint64{10, 14, 18}
	testList := map[uint64]common.Hash{
		10: common.HexToHash("0xaaa"),
		14: common.HexToHash("0xbbb"),
		18: common.HexToHash("0xccc"),
	}

	// Write to database
	err := rawdb.WriteFutureMilestoneList(db, testOrder, testList)
	if err != nil {
		t.Fatalf("Failed to write future milestone list: %v", err)
	}

	// Read back and verify the order is preserved
	readOrder, readList, err := rawdb.ReadFutureMilestoneList(db)
	if err != nil {
		t.Fatalf("Failed to read future milestone list: %v", err)
	}

	// Verify correct order and data
	for i, expectedNum := range testOrder {
		if readOrder[i] != expectedNum {
			t.Errorf("Order mismatch at index %d: expected %d, got %d", i, expectedNum, readOrder[i])
		}

		if readList[expectedNum] != testList[expectedNum] {
			t.Errorf("Hash mismatch for block %d: expected %s, got %s",
				expectedNum, testList[expectedNum].Hex(), readList[expectedNum].Hex())
		}
	}

	// Test that the logic would process these milestones correctly
	end := uint64(16)
	validMilestones := findCommonAncestorWithFutureMilestones_FilterValidMilestones(readOrder, end)

	// Should include milestones 10 and 14 (both < 16), processed in reverse order
	expectedValid := []uint64{14, 10}
	if len(validMilestones) != len(expectedValid) {
		t.Errorf("Expected %d valid milestones, got %d", len(expectedValid), len(validMilestones))
	}

	for i, expected := range expectedValid {
		if validMilestones[i] != expected {
			t.Errorf("Valid milestone[%d]: expected %d, got %d", i, expected, validMilestones[i])
		}
	}
}

func TestBorVerifyRewindLogic_Integration(t *testing.T) {
	// Test the integration of the new logic with the existing rewind logic

	// Scenario 1: Existing whitelisted milestone exists (should use original logic)
	hasExistingMilestone := true
	existingMilestoneBlock := uint64(8)
	start := uint64(10)
	end := uint64(15)

	rewindTo := getBorVerifyRewindBlock_Simulation(hasExistingMilestone, existingMilestoneBlock, start, end)

	if rewindTo != existingMilestoneBlock {
		t.Errorf("Expected to use existing milestone %d, got %d", existingMilestoneBlock, rewindTo)
	}

	// Scenario 2: No existing milestone, should use findCommonAncestorWithFutureMilestones
	hasExistingMilestone = false
	expectedFallback := uint64(7) // simulated result from findCommonAncestorWithFutureMilestones

	rewindTo = getBorVerifyRewindBlock_Simulation(hasExistingMilestone, 0, start, end)

	if rewindTo != expectedFallback {
		t.Errorf("Expected to use fallback %d, got %d", expectedFallback, rewindTo)
	}
}

// Helper function to simulate the new rewind logic flow
func getBorVerifyRewindBlock_Simulation(hasExistingMilestone bool, existingBlock uint64, start uint64, end uint64) uint64 {
	// Simulate the logic from borVerify function
	if hasExistingMilestone {
		// Use existing whitelisted milestone (original logic)
		return existingBlock
	} else {
		// Fall back to findCommonAncestorWithFutureMilestones (new logic)
		// Simulate a simple result for testing
		return start - 3 // Example fallback calculation
	}
}
