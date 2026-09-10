package rawdb

import "testing"

func TestInvalidPreconfRecords(t *testing.T) {
	db := NewMemoryDatabase()
	for number, reason := range map[uint64]string{7: "skipped", 9: "canonical_mismatch", 8: "reorged"} {
		if err := WriteInvalidPreconf(db, number, reason); err != nil {
			t.Fatalf("write %d: %v", number, err)
		}
	}
	if err := WriteInvalidPreconf(db, 8, "superseded"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	records := ReadInvalidPreconfs(db, 2)
	if len(records) != 2 || records[0].Number != 9 || records[1].Number != 8 || records[1].Reason != "superseded" {
		t.Fatalf("records = %+v", records)
	}
	for number := uint64(10); number < InvalidPreconfQueryLimit+20; number++ {
		if err := WriteInvalidPreconf(db, number, "skipped"); err != nil {
			t.Fatalf("write %d: %v", number, err)
		}
	}
	iterator := db.NewIterator(invalidPreconfPrefix, nil)
	defer iterator.Release()
	stored := 0
	for iterator.Next() {
		stored++
	}
	if stored != InvalidPreconfQueryLimit+13 {
		t.Fatalf("stored records = %d", stored)
	}
	records = ReadInvalidPreconfs(db, InvalidPreconfQueryLimit+1)
	if len(records) != InvalidPreconfQueryLimit || records[0].Number != InvalidPreconfQueryLimit+19 {
		t.Fatalf("bounded query = %d records, newest %d", len(records), records[0].Number)
	}
	if err := WriteInvalidPreconf(db, 1, "late_invalidation"); err != nil {
		t.Fatalf("write old record: %v", err)
	}
	if retained, err := db.Has(invalidPreconfKey(1)); err != nil || !retained {
		t.Fatalf("late invalidation retained = %t, err = %v", retained, err)
	}
	if records = ReadInvalidPreconfs(db, InvalidPreconfQueryLimit); len(records) != InvalidPreconfQueryLimit || records[len(records)-1].Number == 1 {
		t.Fatalf("bounded query includes late old record: %+v", records[len(records)-1])
	}
}

func TestInvalidPreconfZeroLimit(t *testing.T) {
	db := NewMemoryDatabase()
	if err := WriteInvalidPreconf(db, 7, "skipped"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if records := ReadInvalidPreconfs(db, 1); len(records) != 1 {
		t.Fatalf("sanity read = %+v", records)
	}

	records := ReadInvalidPreconfs(db, 0)
	if records == nil {
		t.Fatal("zero limit returned a nil slice, which marshals as null rather than []")
	}
	if len(records) != 0 {
		t.Fatalf("zero limit = %+v, want no records", records)
	}
}

func TestInvalidPreconfsInRange(t *testing.T) {
	db := NewMemoryDatabase()
	for number, reason := range map[uint64]string{7: "skipped", 9: "canonical_mismatch", 8: "reorged"} {
		if err := WriteInvalidPreconf(db, number, reason); err != nil {
			t.Fatalf("write %d: %v", number, err)
		}
	}
	if err := WriteInvalidPreconf(db, 8, "superseded"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	// [8,9] returns 9 then 8, newest first, with the overwritten reason; 7 is
	// below the range and excluded.
	records := ReadInvalidPreconfsInRange(db, 8, 9)
	if len(records) != 2 || records[0].Number != 9 || records[1].Number != 8 || records[1].Reason != "superseded" {
		t.Fatalf("range [8,9] = %+v", records)
	}

	// A bound below the stored set still works; 7 is the only record in [1,7].
	if records = ReadInvalidPreconfsInRange(db, 1, 7); len(records) != 1 || records[0].Number != 7 {
		t.Fatalf("range [1,7] = %+v", records)
	}

	// A single-block range hits exactly one record.
	if records = ReadInvalidPreconfsInRange(db, 9, 9); len(records) != 1 || records[0].Number != 9 {
		t.Fatalf("range [9,9] = %+v", records)
	}

	// A range with no invalidations returns a non-nil empty slice so it marshals
	// as [] rather than null.
	if records = ReadInvalidPreconfsInRange(db, 10, 20); records == nil || len(records) != 0 {
		t.Fatalf("empty range = %+v (nil=%t)", records, records == nil)
	}

	// from > to is treated as an empty (non-nil) range.
	if records = ReadInvalidPreconfsInRange(db, 9, 3); records == nil || len(records) != 0 {
		t.Fatalf("inverted range = %+v (nil=%t)", records, records == nil)
	}

	// The response is capped at InvalidPreconfQueryLimit even for a wide range,
	// returning the newest records first.
	for number := uint64(10); number < InvalidPreconfQueryLimit+20; number++ {
		if err := WriteInvalidPreconf(db, number, "skipped"); err != nil {
			t.Fatalf("write %d: %v", number, err)
		}
	}
	records = ReadInvalidPreconfsInRange(db, 0, InvalidPreconfQueryLimit+100)
	if len(records) != InvalidPreconfQueryLimit || records[0].Number != InvalidPreconfQueryLimit+19 {
		t.Fatalf("capped range = %d records, newest %d", len(records), records[0].Number)
	}
}
