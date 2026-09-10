package rawdb

import (
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// reservedTxsMigrationKey is the durable marker recording the reserved-tx
// freezer table's pad target and completion state across restarts. It lives
// in the key-value store paired with the chain freezer, not in the freezer
// itself, since it must survive independently of the ancient tables it
// governs (in particular, it must still be readable after a crash truncates
// those tables back down during repair).
var reservedTxsMigrationKey = []byte("borReservedTxsFreezerMigration")

// reservedTxsMigrationMarker records the reserved-tx freezer table migration's
// progress. Target is fixed once recorded and is never re-derived on resume
// (see newReservedTxsMigrationHook).
type reservedTxsMigrationMarker struct {
	Target uint64
	Done   bool
}

func readReservedTxsMigrationMarker(db ethdb.KeyValueReader) (reservedTxsMigrationMarker, bool) {
	data, err := db.Get(reservedTxsMigrationKey)
	if err != nil || len(data) == 0 {
		return reservedTxsMigrationMarker{}, false
	}

	var marker reservedTxsMigrationMarker
	if err := rlp.DecodeBytes(data, &marker); err != nil {
		log.Error("Invalid reserved-tx freezer migration marker, treating as absent", "err", err)
		return reservedTxsMigrationMarker{}, false
	}

	return marker, true
}

func writeReservedTxsMigrationMarker(db ethdb.KeyValueWriter, marker reservedTxsMigrationMarker) error {
	data, err := rlp.EncodeToBytes(marker)
	if err != nil {
		return fmt.Errorf("failed to encode reserved-tx freezer migration marker: %w", err)
	}
	if err := db.Put(reservedTxsMigrationKey, data); err != nil {
		return fmt.Errorf("failed to persist reserved-tx freezer migration marker: %w", err)
	}
	return nil
}

// newReservedTxsMigrationHook returns the freezerPreRepairFn that reconciles
// the reserved-tx freezer table against pre-existing chain history. db is the
// key-value store holding the durable migration marker; a nil db (the
// in-memory dev freezer) disables the migration entirely, since that freezer
// never has pre-existing on-disk history to reconcile against.
func newReservedTxsMigrationHook(db ethdb.KeyValueStore) freezerPreRepairFn {
	if db == nil {
		return nil
	}
	return func(f *Freezer) error {
		return migrateReservedTxsFreezer(f, db)
	}
}

// migrateReservedTxsFreezer is the entry point run before repair(). See the
// package doc on reservedTxsMigrationMarker and the cases below for the full
// protocol; in short: pad the reserved-tx table up to a durably recorded
// target so introducing it never truncates pre-existing history, and once
// that one-time migration is done, tell an ordinary crash-mid-freeze (still
// recoverable via repair) apart from a downgrade gap (the classification is
// permanently gone; pad with empties and say so loudly).
//
// repair() truncates every table - reserved included - down to the global
// minimum item count. That means coreMin, not coreMax, is what the reserved
// table must never fall behind without disambiguation: if it did, repair()
// would truncate the core tables (headers, hashes, bodies, receipts,
// difficulty, bor-receipts) down to the reserved table's stale head even
// when those core tables only disagree with each other by an ordinary,
// unrelated crash. Both the resume branch and healReservedTxsGap are
// therefore always anchored to coreMin; any residual coreMin..coreMax
// disagreement among the core tables themselves is left for repair()'s own
// common-length truncation to resolve, which is safe once reserved is no
// longer the thing dragging the global minimum down.
func migrateReservedTxsFreezer(f *Freezer, db ethdb.KeyValueStore) error {
	reserved, ok := f.tables[ChainFreezerReservedTxsTable]
	if !ok {
		return nil
	}

	coreMin, _, ok := coreTablesHeadRange(f)
	if !ok {
		return nil
	}

	marker, found := readReservedTxsMigrationMarker(db)
	switch {
	case !found:
		return startReservedTxsMigration(f, db, reserved, coreMin)
	case !marker.Done:
		return resumeReservedTxsMigration(f, db, reserved, marker.Target, coreMin)
	default:
		return healReservedTxsGap(f, db, reserved, coreMin)
	}
}

// coreTablesHeadRange returns the min and max item counts across every table
// except the reserved-tx one. ok is false only if the reserved-tx table is
// somehow the sole table in f, which never happens for the chain freezer.
func coreTablesHeadRange(f *Freezer) (min, max uint64, ok bool) {
	min = math.MaxUint64
	for name, table := range f.tables {
		if name == ChainFreezerReservedTxsTable {
			continue
		}
		items := table.items.Load()
		if items < min {
			min = items
		}
		if items > max {
			max = items
		}
		ok = true
	}
	if !ok {
		min = 0
	}
	return min, max, ok
}

// startReservedTxsMigration handles the marker-absent case: a fresh node (no
// pre-existing core-table history) marks done immediately with nothing to
// pad, while a node upgrading over existing history records the pad target
// durably before padding, so a crash mid-pad resumes to that fixed target
// rather than re-deriving one from whatever the core tables happen to be at
// on the next start.
func startReservedTxsMigration(f *Freezer, db ethdb.KeyValueStore, reserved *freezerTable, coreHead uint64) error {
	if coreHead == 0 {
		return writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Done: true})
	}
	if err := writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: coreHead}); err != nil {
		return err
	}
	return padReservedTxsTable(f, db, reserved, coreHead)
}

// padReservedTxsTable pads reserved up to target and then marks the migration
// done. It is used for a fresh migration's initial pad and wherever a target
// is already known to be safe to pad to outright; the target itself is never
// recomputed here.
func padReservedTxsTable(f *Freezer, db ethdb.KeyValueStore, reserved *freezerTable, target uint64) error {
	if err := padReservedTxsTableTo(f, reserved, target); err != nil {
		return err
	}
	return writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: target, Done: true})
}

// resumeReservedTxsMigration resumes a migration interrupted mid-pad. The
// recorded target is honored as-is first - it is never re-derived downward -
// but the core tables may have advanced past it since the crash: an older
// bor with no knowledge of the reserved-tx table or its marker can freeze
// further blocks into the core tables alone during a downgrade window. Any
// such residual gap up to the current coreMin is therefore handed to
// healReservedTxsGap for the same hot-data disambiguation the post-done path
// uses, so a stale recorded target can never leave repair() looking at an
// unbacked reserved-table deficit either.
func resumeReservedTxsMigration(f *Freezer, db ethdb.KeyValueStore, reserved *freezerTable, target, coreMin uint64) error {
	if err := padReservedTxsTable(f, db, reserved, target); err != nil {
		return err
	}
	return healReservedTxsGap(f, db, reserved, coreMin)
}

// healReservedTxsGap disambiguates a reserved-tx table that has fallen
// behind coreHead (always coreMin - see migrateReservedTxsFreezer). The two
// possible causes need opposite handling: a freeze that crashed before the
// post-freeze hot-KV cleanup ran (hot data for the gap still present, so
// repair()'s truncate-and-refreeze recovers the true classifications, and
// touching the reserved table here would only fight that) versus a downgrade
// window during which an older bor froze blocks with no reserved-tx table at
// all (hot data for the gap already cleaned up, so the classification is
// gone for good and the gap is padded with empties instead, before repair()
// ever gets a chance to see the deficit and truncate every other table down
// to it).
func healReservedTxsGap(f *Freezer, db ethdb.KeyValueStore, reserved *freezerTable, coreHead uint64) error {
	reservedHead := reserved.items.Load()
	if reservedHead >= coreHead {
		return nil
	}

	gapStart := f.offset.Load() + reservedHead
	if reservedTxsGapHotDataPresent(db, gapStart) {
		return nil
	}

	log.Error("Reserved transaction classification lost for frozen blocks written without the reserved-tx freezer table; padding with empty entries",
		"fromBlock", gapStart, "toBlock", f.offset.Load()+coreHead-1)

	if err := writeReservedTxsMigrationMarker(db, reservedTxsMigrationMarker{Target: coreHead}); err != nil {
		return err
	}
	return padReservedTxsTable(f, db, reserved, coreHead)
}

// reservedTxsGapHotDataPresent reports whether the hot canonical-hash mapping
// for blockNumber is still in the key-value store. That mapping (like the
// rest of a block's hot data) is deleted only after a freeze cycle commits
// successfully, so its presence pins the gap to a crash mid-freeze rather
// than a downgrade window; checking the earliest gap block is decisive for
// the whole gap because a freeze batch's hot cleanup is all-or-nothing.
func reservedTxsGapHotDataPresent(db ethdb.KeyValueStore, blockNumber uint64) bool {
	present, err := db.Has(headerHashKey(blockNumber))
	if err != nil {
		// Lookup failure: prefer the safe branch that defers to repair()'s
		// crash recovery instead of permanently discarding classification we
		// could not confirm is actually gone.
		log.Warn("Failed to check hot chain data while migrating reserved-tx freezer table", "block", blockNumber, "err", err)
		return true
	}
	return present
}

// padReservedTxsTableTo appends empty reserved-tx entries to reserved until
// its item count reaches target (a no-op if it is already there), then syncs
// the table so the pad is durable before the caller records the marker done.
//
// freezerTable.Fill (freezer_table.go) looks similar but does not fit: it
// hardcodes newBatch(0), ignoring the freezer's global offset (wrong for a
// pruned/offset freezer), and appends via Append(item, nil), RLP-encoding a
// bare nil rather than an empty list.
func padReservedTxsTableTo(f *Freezer, reserved *freezerTable, target uint64) error {
	if reserved.items.Load() >= target {
		return nil
	}

	targetGlobal := f.offset.Load() + target
	batch := reserved.newBatch(f.offset.Load())
	for batch.curItem < targetGlobal {
		if err := batch.AppendRaw(batch.curItem, rlp.EmptyList); err != nil {
			return fmt.Errorf("failed to pad reserved-tx freezer table to item %d: %w", targetGlobal, err)
		}
	}
	if err := batch.commit(); err != nil {
		return fmt.Errorf("failed to commit padded reserved-tx freezer table: %w", err)
	}
	return reserved.Sync()
}
