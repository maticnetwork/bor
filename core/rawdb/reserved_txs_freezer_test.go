package rawdb

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
)

// newTestReservedFreezerTables is a minimal two-table stand-in for
// chainFreezerTableConfigs: "core" represents every pre-existing chain
// freezer table collectively (the migration logic treats them uniformly),
// and the reserved-tx table is configured exactly as in production.
func newTestReservedFreezerTables() map[string]freezerTableConfig {
	return map[string]freezerTableConfig{
		"core":                       {noSnappy: true, prunable: true},
		ChainFreezerReservedTxsTable: {noSnappy: false, prunable: true},
	}
}

// buildCoreOnlyHistory creates n items of "core"-table-only history at dir,
// with no reserved-tx table at all, simulating a freezer written by a
// pre-upgrade bor version.
func buildCoreOnlyHistory(t *testing.T, dir string, n uint64) {
	t.Helper()

	f, err := newFreezer(dir, "", false, 0, 2049, map[string]freezerTableConfig{"core": {noSnappy: true, prunable: true}}, nil)
	require.NoError(t, err)

	_, err = f.ModifyAncients(func(op ethdb.AncientWriteOp) error {
		for i := uint64(0); i < n; i++ {
			if err := op.AppendRaw("core", i, []byte{byte(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// padStandaloneReservedTable creates the reserved-tx table file at dir
// directly (bypassing the full Freezer/repair machinery) and appends n empty
// entries to it, letting a test hand-construct a specific pre-migration disk
// state (e.g. "already padded partway, then crashed").
func padStandaloneReservedTable(t *testing.T, dir string, n uint64) {
	t.Helper()

	cfg := newTestReservedFreezerTables()[ChainFreezerReservedTxsTable]
	rt, err := newFreezerTable(dir, ChainFreezerReservedTxsTable, cfg, false)
	require.NoError(t, err)

	batch := rt.newBatch(0)
	for i := uint64(0); i < n; i++ {
		require.NoError(t, batch.AppendRaw(i, rlp.EmptyList))
	}
	require.NoError(t, batch.commit())
	require.NoError(t, rt.Close())
}

// newTestReservedFreezerTablesMultiCore is a two-core-table stand-in for
// chainFreezerTableConfigs. newTestReservedFreezerTables' single "core"
// table always has coreMin == coreMax by construction; reaching the
// coreMin != coreMax branch of the migration (two pre-existing tables left
// at different lengths by an ordinary crash) needs at least two.
func newTestReservedFreezerTablesMultiCore() map[string]freezerTableConfig {
	return map[string]freezerTableConfig{
		"coreA":                      {noSnappy: true, prunable: true},
		"coreB":                      {noSnappy: true, prunable: true},
		ChainFreezerReservedTxsTable: {noSnappy: false, prunable: true},
	}
}

// buildStandaloneCoreTable creates a single named core table file at dir,
// bypassing Freezer.ModifyAncients (which requires every table in one commit
// to land on the same item count, so it cannot itself produce two core
// tables of different lengths), and appends n raw items to it.
func buildStandaloneCoreTable(t *testing.T, dir, name string, n uint64) {
	t.Helper()

	ct, err := newFreezerTable(dir, name, freezerTableConfig{noSnappy: true, prunable: true}, false)
	require.NoError(t, err)

	batch := ct.newBatch(0)
	for i := uint64(0); i < n; i++ {
		require.NoError(t, batch.AppendRaw(i, []byte{byte(i)}))
	}
	require.NoError(t, batch.commit())
	require.NoError(t, ct.Close())
}

func TestReservedTxsFreezerMigration_FreshNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := memorydb.New()

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Zero(t, head)

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.True(t, marker.Done)
	require.Zero(t, marker.Target)
}

func TestReservedTxsFreezerMigration_PadsPreExistingHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildCoreOnlyHistory(t, dir, 10)

	db := memorydb.New()
	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(10), head, "pre-existing core-table history must not be truncated")
	require.Equal(t, uint64(10), f.tables[ChainFreezerReservedTxsTable].items.Load())

	for i := uint64(0); i < 10; i++ {
		data, err := f.Ancient(ChainFreezerReservedTxsTable, i)
		require.NoError(t, err)
		require.Equal(t, rlp.EmptyList, data)
	}

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.True(t, marker.Done)
	require.Equal(t, uint64(10), marker.Target)
}

func TestReservedTxsFreezerMigration_CrashMidPad_ResumesToRecordedTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildCoreOnlyHistory(t, dir, 10)

	// Hand-simulate a crash partway through the initial pad: 4 of the
	// eventual 10 empty entries already landed on disk, and the marker was
	// durably written (at migration start, before any padding) recording
	// target 10.
	padStandaloneReservedTable(t, dir, 4)

	db := memorydb.New()
	require.NoError(t, writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: 10}))

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(10), head)
	require.Equal(t, uint64(10), f.tables[ChainFreezerReservedTxsTable].items.Load())

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.True(t, marker.Done)
	require.Equal(t, uint64(10), marker.Target)
}

// TestReservedTxsFreezerMigration_PostDone_CrashMidFreeze_FallsThroughToRepair
// covers the disambiguation's first branch: once migrated, a reserved table
// that has fallen behind mutually-consistent core tables, with the gap
// blocks' hot data still present, is left alone so repair()'s ordinary
// crash-recovery truncates every table back to the common head instead of
// inventing empty classifications for blocks that may have reserved
// transactions.
func TestReservedTxsFreezerMigration_PostDone_CrashMidFreeze_FallsThroughToRepair(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildCoreOnlyHistory(t, dir, 12)
	padStandaloneReservedTable(t, dir, 10)

	db := memorydb.New()
	require.NoError(t, writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: 10, Done: true}))
	// Hot data for the gap blocks (10, 11) still present: post-freeze cleanup
	// only runs after a successful freeze.
	require.NoError(t, db.Put(headerHashKey(10), common.BytesToHash([]byte{0xaa}).Bytes()))
	require.NoError(t, db.Put(headerHashKey(11), common.BytesToHash([]byte{0xbb}).Bytes()))

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(10), head, "repair() truncates every table to the common head, not just reserved")
	require.Equal(t, uint64(10), f.tables[ChainFreezerReservedTxsTable].items.Load())

	// The marker is untouched: this path never re-records or re-pads.
	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.Equal(t, uint64(10), marker.Target)
}

// TestReservedTxsFreezerMigration_DowngradeGap_PadsAndHeals covers the
// disambiguation's second branch: the gap blocks' hot data is gone (an older
// bor froze them successfully with no reserved-tx table at all), so the gap
// is permanently unrecoverable and gets healed with empty entries instead of
// truncating the otherwise-healthy core tables.
func TestReservedTxsFreezerMigration_DowngradeGap_PadsAndHeals(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildCoreOnlyHistory(t, dir, 12)
	padStandaloneReservedTable(t, dir, 10)

	db := memorydb.New()
	require.NoError(t, writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: 10, Done: true}))
	// No hot data for blocks 10, 11: cleaned up by a successful, older-bor freeze.

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(12), head, "core-table history is preserved, not truncated to reserved's lagging head")
	require.Equal(t, uint64(12), f.tables[ChainFreezerReservedTxsTable].items.Load())

	for i := uint64(10); i < 12; i++ {
		data, err := f.Ancient(ChainFreezerReservedTxsTable, i)
		require.NoError(t, err)
		require.Equal(t, rlp.EmptyList, data)
	}

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.True(t, marker.Done)
	require.Equal(t, uint64(12), marker.Target)
}

// TestReservedTxsFreezerMigration_MultiCoreCrashRepro reproduces the
// confirmed data-loss scenario: the migration completed at head 5, then a
// downgrade let an old bor with no knowledge of the reserved-tx table freeze
// blocks 5..10 into the core tables alone and clean up their hot data,
// before dying uncleanly and leaving the two core tables themselves at
// unequal lengths (12 and 11). Before the fix, marker.Done && coreMin !=
// coreMax returned nil unconditionally, so repair() computed the head as the
// minimum over every table including the stale reserved one (5) and
// truncated every core table down to 5, destroying blocks 5..10 for good.
// The fix must pad reserved up to coreMin (11) first, leaving only the
// ordinary 11-vs-12 disagreement between the core tables for repair() to
// resolve on its own.
func TestReservedTxsFreezerMigration_MultiCoreCrashRepro(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildStandaloneCoreTable(t, dir, "coreA", 12)
	buildStandaloneCoreTable(t, dir, "coreB", 11)
	padStandaloneReservedTable(t, dir, 5)

	db := memorydb.New()
	require.NoError(t, writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: 5, Done: true}))
	// Hot data for block 11 only: blocks 5..10 were already frozen and
	// hot-cleaned by the old bor before it crashed.
	require.NoError(t, db.Put(headerHashKey(11), common.BytesToHash([]byte{0xcc}).Bytes()))

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTablesMultiCore(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(11), head, "must resolve to the ordinary 11-vs-12 crash range, not truncate to the stale reserved head 5")
	require.Equal(t, uint64(11), f.tables["coreA"].items.Load())
	require.Equal(t, uint64(11), f.tables["coreB"].items.Load())
	require.Equal(t, uint64(11), f.tables[ChainFreezerReservedTxsTable].items.Load())

	for i := uint64(5); i < 11; i++ {
		data, err := f.Ancient(ChainFreezerReservedTxsTable, i)
		require.NoError(t, err)
		require.Equal(t, rlp.EmptyList, data, "blocks 5..10 lost their classification to the downgrade and must be padded, not left to drag the core tables down with them")
	}

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.True(t, marker.Done)
	require.Equal(t, uint64(11), marker.Target)
}

// TestReservedTxsFreezerMigration_MultiCoreCrashMidFreeze_NoPadding covers an
// ordinary crash-mid-freeze even when the core tables themselves disagree:
// the reserved table's one-block deficit is fully backed by still-present
// hot data, so it must be left alone (no padding, marker untouched) and
// folded into repair()'s single common-length truncation together with the
// core tables' own 11-vs-12 disagreement.
func TestReservedTxsFreezerMigration_MultiCoreCrashMidFreeze_NoPadding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildStandaloneCoreTable(t, dir, "coreA", 12)
	buildStandaloneCoreTable(t, dir, "coreB", 11)
	padStandaloneReservedTable(t, dir, 10)

	db := memorydb.New()
	require.NoError(t, writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: 10, Done: true}))
	// Hot data for block 10 still present: the freeze that would have
	// written it (and cleaned it up) never completed.
	require.NoError(t, db.Put(headerHashKey(10), common.BytesToHash([]byte{0xdd}).Bytes()))

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTablesMultiCore(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(10), head, "repair() truncates every table to the common head, reserved's hot-data-backed deficit included")
	require.Equal(t, uint64(10), f.tables["coreA"].items.Load())
	require.Equal(t, uint64(10), f.tables["coreB"].items.Load())
	require.Equal(t, uint64(10), f.tables[ChainFreezerReservedTxsTable].items.Load())

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.Equal(t, uint64(10), marker.Target, "no padding happened, so the marker must be untouched")
}

// TestReservedTxsFreezerMigration_ResumeStaleTarget_PadsToCoreMin covers the
// resume branch's counterpart to the crash repro above: the migration was
// interrupted mid-pad with recorded target 8, then a downgrade froze further
// blocks into the core tables alone (which also end up disagreeing with each
// other, 15 vs 14) before an unclean crash. Resuming must honor the recorded
// target first (padding to 8, never re-derived downward), then detect the
// additional, no-longer-hot-data-backed gap up to the current coreMin (14)
// and pad that too, rather than marking done at the stale target 8 and
// leaving repair() to see an unbacked reserved deficit again.
func TestReservedTxsFreezerMigration_ResumeStaleTarget_PadsToCoreMin(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildStandaloneCoreTable(t, dir, "coreA", 15)
	buildStandaloneCoreTable(t, dir, "coreB", 14)
	padStandaloneReservedTable(t, dir, 3)

	db := memorydb.New()
	require.NoError(t, writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: 8}))
	// No hot data anywhere in 3..13: the original crash's own gap (3..8) and
	// the later downgrade's gap (8..14) were both cleaned up by successful
	// freezes.

	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTablesMultiCore(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	head, err := f.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(14), head)
	require.Equal(t, uint64(14), f.tables["coreA"].items.Load())
	require.Equal(t, uint64(14), f.tables["coreB"].items.Load())
	require.Equal(t, uint64(14), f.tables[ChainFreezerReservedTxsTable].items.Load())

	for i := uint64(3); i < 14; i++ {
		data, err := f.Ancient(ChainFreezerReservedTxsTable, i)
		require.NoError(t, err)
		require.Equal(t, rlp.EmptyList, data)
	}

	marker, found := readReservedTxsMigrationMarker(db)
	require.True(t, found)
	require.True(t, marker.Done)
	require.Equal(t, uint64(14), marker.Target, "resume must not leave the stale recorded target as the final one")
}

// TestReservedTxsFreezerMigration_ReadOnlyOpenBeforeMigrationFails documents
// the accepted limitation: a read-only open of a freezer that has not yet
// run the read-write migration fails validate() with a differing-head error,
// exactly as opening any newly introduced table read-only against existing
// history already would. The remedy is one read-write start.
func TestReservedTxsFreezerMigration_ReadOnlyOpenBeforeMigrationFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildCoreOnlyHistory(t, dir, 5)

	_, err := newFreezer(dir, "", true, 0, 2049, newTestReservedFreezerTables(), nil)
	require.Error(t, err)
}

// TestReservedTxsFreezerMigration_ReadOnlyOpenAfterMigrationSucceeds is the
// counterpart: once a read-write start has completed the migration, a
// subsequent read-only open validates cleanly.
func TestReservedTxsFreezerMigration_ReadOnlyOpenAfterMigrationSucceeds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	buildCoreOnlyHistory(t, dir, 5)

	db := memorydb.New()
	f, err := newFreezer(dir, "", false, 0, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	f2, err := newFreezer(dir, "", true, 0, 2049, newTestReservedFreezerTables(), nil)
	require.NoError(t, err)
	defer f2.Close()

	head, err := f2.Ancients()
	require.NoError(t, err)
	require.Equal(t, uint64(5), head)
}

// TestReservedTxsFreezerMigration_PrunedOffsetFreezer covers a freezer whose
// items don't start at global block 0 (e.g. after ancient-store pruning
// rebased it), confirming the pad target and appended item numbers are
// computed consistently with the freezer's offset.
func TestReservedTxsFreezerMigration_PrunedOffsetFreezer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const offset = uint64(1000)

	f0, err := newFreezer(dir, "", false, offset, 2049, map[string]freezerTableConfig{"core": {noSnappy: true, prunable: true}}, nil)
	require.NoError(t, err)
	_, err = f0.ModifyAncients(func(op ethdb.AncientWriteOp) error {
		for i := uint64(0); i < 5; i++ {
			if err := op.AppendRaw("core", offset+i, []byte{byte(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, f0.Close())

	db := memorydb.New()
	f, err := newFreezer(dir, "", false, offset, 2049, newTestReservedFreezerTables(), newReservedTxsMigrationHook(db))
	require.NoError(t, err)
	defer f.Close()

	require.Equal(t, uint64(5), f.tables[ChainFreezerReservedTxsTable].items.Load())
	for i := uint64(0); i < 5; i++ {
		data, err := f.Ancient(ChainFreezerReservedTxsTable, offset+i)
		require.NoError(t, err)
		require.Equal(t, rlp.EmptyList, data)
	}
}

// TestReservedTxsMigrationMarker_CorruptTreatedAsAbsent covers a marker value
// that fails to RLP-decode: it must be treated as absent (triggering a fresh
// migration attempt) rather than propagating a decode error up through
// freezer construction.
func TestReservedTxsMigrationMarker_CorruptTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	db := memorydb.New()
	require.NoError(t, db.Put(reservedTxsMigrationKey, []byte{0x01, 0x02, 0x03}))

	_, found := readReservedTxsMigrationMarker(db)
	require.False(t, found)
}
