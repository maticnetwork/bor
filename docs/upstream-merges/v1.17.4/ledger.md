# Decision ledger: go-ethereum v1.17.4 sync

Append-only. One line per non-trivial resolution: file — class — upstream PR — decision — why.

## v1.16.9

- `crypto/ecies/ecies.go` — class 3 — #33669 — took upstream's `IsOnCurve` validation in `GenerateShared` — genuine invalid-curve guard absent from Bor; expect the duplicate commit (`c974722dc`, v1.17.0 batch 10) to now merge clean.
- `crypto/secp256k1/curve.go` — class 3 — upstream `895a8597c` — kept Bor's text — Bor pre-applied the identical coordinate check via `d9fac7a4c`; only delta was Bor's comment.
- `crypto/secp256k1/ext.h` — class 3 — upstream `895a8597c` — took upstream's combined `if (!a || !b)` form over Bor's equivalent two-if variant — zero behavior delta; converges text with upstream to de-conflict future syncs.

## v1.17.0

### batch 1/12 (boundary `f23d506b7`, 20 commits)

- `core/state/statedb.go` — class 3 ⚠ — #33082 — kept Bor's `Logs()` unsorted (dropped upstream's `sort.Slice` + now-unused `sort` import) — Bor sorts HF-gated at `blockchain.go:2380` (pre-Madhugiri only) and relies on `Logs()` = receipt logs ++ state-sync logs tail order (`blockLogs[len(logs):]`); upstream's unconditional in-`Logs()` sort would reorder post-Madhugiri where Bor deliberately does not. Consensus-relevant; `core/state` tests green.
- `eth/filters/filter_system.go` — class 3 — #33108 — took upstream's `txHashes map[common.Hash]struct{}` + kept Bor's `stateSyncData` field — Bor's own `hashSet` already builds `struct{}`, the sole read (`filter.go:601`) is comma-ok/type-agnostic; `eth/filters` tests green.
- `core/txpool/blobpool/blobpool_test.go` — class 3 (test) — #33161 — union: added upstream's `BlobTxs: true` field + kept Bor's `, nil` 2nd arg to `Pending()` — orthogonal; blobpool tests green.
- `core/blockchain_insert.go` — class 3 — #33155 — took upstream's deletion of unused `insertIterator.peek()` — provenance: Bor side was a blank-line churn over base (`nolint:unused`, no callers); converges and de-conflicts.
- `core/rawdb/database.go` — class 1 — #33025 — kept Bor's `ethDb := &freezerdb{…}` form + added upstream's `readOnly: opts.ReadOnly` field — Bor needs the `ethDb` ref for the witness-store setup below; `readOnly` struct field auto-merged; build + rawdb tests green.
- `common/types.go` — class 1 — #32998 — kept both `HexToRefHash` (Bor) and `IsHexHash` (upstream) — orthogonal additions at the same location.
- `cmd/geth/config.go` — class 1 — #32998 — removed both unused imports (`crypto`, `hexutil`) — upstream reworked config.go to use `common.IsHexHash`; the merged Bor body references neither; build-confirmed.
- `rpc/http.go` — class 2 — #33122 — took upstream's `cleanlyCloseBody(respBody)` — provenance: Bor side was lint/merge churn (`f3ffacf2d wsl lint`), no deliberate divergence; converges; rpc tests green.
- `rpc/http_test.go` — class 2 — #33122 — took upstream's `cleanlyCloseBody(resp.Body)` — provenance: Bor side was upstream code carried via a prior merge (`f3c19b1a1`, #30384); converges.
- `version/version.go` — class 1 — v1.16.8 dev cycle — kept HEAD (`1.16.9`/`stable`) — avoids a mid-stack version regression; the v1.17.0 release batch (12/12) sets `1.17.0`.

### batch 2/12 (boundary `cf93077fa`, 20 commits) — verkle/UBT groundwork, rawdb+rlp hardening

Consensus / concurrency (full write-up):

- `core/state_processor.go` — class 3 ⚠ — #33124 (tracer relocation) — **merged**: kept Bor's `interruptCtx` nil-init and `NewEVMBlockContext(header, p.chain, author)` (Bor passes the author), adopted upstream's relocation of `tracingStateDB` to before the DAO-fork block (required — `ApplyDAOHardFork` now takes it) and dropped the second, now-duplicate definition. `config` == `p.chainConfig()`. Bounded `core` Process tests green.
- `triedb/pathdb/history_index.go` — class 3 ⚠ concurrency — upstream refactored `readGreaterThan` to `return it.ID(), nil` (iterator-based) — **took upstream's refactor and PORTED Bor's `#1875` RWMutex** into `newIterator`'s closure (the new home of the `r.readers` access), keeping `refresh()`'s existing lock; imports keep `sync`, drop now-unused `sort`. Validated with `go test -race ./triedb/pathdb` (green, no data race).

Bor-owned surfaces kept (class 1):

- `core/rawdb/accessors_chain.go` — class 1 — upstream removed 4 accessors — **kept Bor's HEAD**: `ReadAllHashesInRange` is used in prod (`blockchain.go`, witness/block pruners); `FindCommonAncestor` is used by Bor's `eth/bor_checkpoint_verifier_test.go`; `ReadAllCanonicalHashes`/`DeleteBadBlocks` used in rawdb tests. Bor depends on all four.
- `core/rawdb/accessors_chain_test.go` — class 1 — kept HEAD (tests for the retained accessors); re-added dropped `reflect` import.
- `internal/ethapi/simulate.go` — class 3 — kept Bor's `remaining`-based gas-limit check returning `(bool, error)` (deliberate signature/logic divergence); upstream's change was only a `*gasUsed` fmt fix to code Bor replaced.
- `tests/init.go` — class 3 — kept ours (Bor's `params.ChainConfig` lacks the BPO time-based fields upstream's new test fork configs reference).

Take-theirs / converge (class 2):

- `rlp/iterator.go` — class 2 — took upstream's `it.err = nil` (clear transient error after a successful `next`); Bor matched base modulo a blank line.
- `accounts/keystore/keystore.go` — class 2 — took upstream's `defer zeroKey(key.PrivateKey)` (×2, key hygiene); Bor matched base.
- `cmd/geth/dbcmd.go` — class 2 — took upstream's `rawdb.NewTableWriter` (replaces `olekukonko/tablewriter`); dropped the now-unused `tablewriter` import + a duplicate `cli`.
- `cmd/geth/verkle.go` — class 2 — took upstream's `resolver(childC[:])` reordering.
- `cmd/evm/internal/t8ntool/transition.go` — class 2/3 — took upstream's binary-trie (`bt`) output support + imports; dropped a duplicate `cli`; adapted `IsVerkle` call to Bor's 1-arg signature.

Accept upstream deletion / removal:

- `core/chain_makers.go` — class 3 — accepted upstream's deletion of `GenerateVerkleChain`/`GenerateVerkleChainWithGenesis` (zero usage outside the also-deleted verkle test); dropped `go-verkle`, de-duplicated `uint256`, adapted two `IsVerkle` calls to Bor's 1-arg signature.
- `core/verkle_witness_test.go` — class 3 — accepted upstream's deletion (used the removed `GenerateVerkleChain`).
- `trie/bintrie/iterator_test.go` — class 3 — accepted upstream's deletion (Bor's only change was `167e5aee4` reformat churn).
- `core/bintrie_witness_test.go` — class 3 (**coverage gap flagged**) — **removed** upstream's newly-added binary-trie witness test: it can't compile against Bor (uses time-based `ShanghaiTime`/`VerkleTime` config fields + `u64` helper Bor lacks, and Bor's different `InsertChain` signature). Re-add and adapt if Bor ever adopts the binary-trie transition.

Merge-integration fixes (silently-merged upstream code vs Bor APIs; caught by build/test tier):

- `core/rawdb/database_tablewriter.go` (upstream-added) — replaced builtin `max(int,int)` usage with explicit comparisons (Bor's rawdb defines a `max(uint64,uint64)` in `pruner.go` that shadows the builtin). `database_tablewriter_unix.go` deleted by upstream (auto-merged).
- `trie/transitiontrie/transition.go`, `triedb/pathdb/database.go` — dropped now-unused `go-verkle` imports (verkle-removal artifacts).
- `cmd/evm/internal/t8ntool/execution.go`, `tests/block_test_util.go` — adapted upstream code to Bor's block-based verkle API (`IsVerkle` 1-arg; `IsVerkleGenesis()` instead of `VerkleTime`).
- `go.mod`/`go.sum`, `cmd/keeper/go.{mod,sum}` — kept ours (Bor's dep graph is ahead: `liner` v1.2.2, `stun/v3`, `go-toml`, bumped `cpuid`/`mapstructure`), then `go mod tidy` on both modules to reconcile.

### batch 3/12 (boundary `a122dbe45`, 20 commits) — EIP-8024, miner `--miner.maxblobs`, gnark-crypto bump

Fork/EIP (see `fork-register.md`):

- **EIP-8024** (DUPN/SWAPN/EXCHANGE) — **dormant / defer**. Registered in `core/vm/eips.go` `activators` map but not wired into any fork instruction set and no Bor preset sets `ExtraEips`; opcodes absent from every Bor jump table. Disabled by default (invariant 9).

Deferred feature (needs-wiring, tracked for milestone triage):

- `--miner.maxblobs` — **deferred, not adopted this batch.** Upstream added a configurable per-block blob cap (`MinerMaxBlobsFlag`, `miner.Config.MaxBlobsPerBlock`, `(*Miner).maxBlobsPerBlock`, replacing `eip4844.MaxBlobsPerBlock(...)` call sites). Bor's miner is heavily diverged (`worker` struct with `w.chainConfig`, restructured `commitTransaction`, and it deleted the isCancun blob-space-left block), so wiring the knob requires adapting it to Bor's worker structure + flag→config plumbing — out of scope for a merge batch and not consensus-relevant. Kept Bor's behavior (blob cap = `eip4844.MaxBlobsPerBlock` protocol default; backward-compatible). Resolutions: `miner/miner.go`, `miner/worker.go` (4 hunks), `cmd/utils/flags.go` (flag def) → took ours; removed the auto-merged plumbing that referenced the dropped flag/field — `cmd/utils/flags.go` `setMiner` block and `cmd/geth/main.go` flag registration.

Other resolutions:

- `eth/downloader/queue.go` — class 2 — took upstream's removal of the `proc` throttle counter (3 hunks); Bor matched base modulo whitespace (no deliberate divergence).
- `node/defaults.go` — class 1 — merged: kept Bor's `TxArrivalWait: 500ms` + adopted upstream's `DiscoveryV4/V5: true` defaults.
- `.github/workflows/validate_pr.yml` — non-code — kept Bor's deletion (modify/delete; Bor removed this upstream CI workflow, has its own).
- `go.mod`/`go.sum`, `cmd/keeper/go.{mod,sum}` — kept ours + `go mod tidy` (both modules).

Merge-integration fix:

- `tests/block_test_util.go` — restored Bor's single `run(..., useV2 bool)` method (Forks lookup inside). The auto-merge had spliced upstream's `run(config, ...)` refactor in, producing a **duplicate `run` method** and orphaning `useV2` in the implementation (a compile error). Kept Bor's V2-BlockSTM (`Run`/`RunV2`) structure.

### batch 4/12 (boundary `228933a66`, 20 commits) — catalyst getPayload fork checks, pebble config, slow-block stats, downloader syncmode

Conflicts: 20 files (18 content, 2 modify/delete). Central theme = upstream's #33157 "keep current syncmode in downloader only" refactor colliding with Bor's deep downloader fork; plus two independent observability refactors (#32812 slow-block, #33254 reader stats) that Bor diverged from. All three refactors **deferred** (tracked in `needs-wiring.md`); the batch's genuinely-independent bugfixes (#33331, #33330, #32999, #33353, #33361, #33280, #33320, #33336, #33329, #33058, #33338, #33355) were adopted.

Fork/EIP (see `fork-register.md`): EIP-8024 PC-increment fix (#33361, still dormant), Fulu beacon types (#33349, inert — CL fork), catalyst getPayload fork-timestamp guard (#32754, inert — Engine API unused in Bor). No gate flipped; invariant 9 holds.

Big decision — **defer #33157 (syncModer refactor) entirely**:

- Bor keeps `SyncMode` in `eth/downloader/modes.go` (with Bor-only `StatelessSync`/`BytecodeOnlySnapSync`); upstream moved the type to `eth/ethconfig` and pushed mode selection into a new `syncModer`. Porting would relocate the type (breaking Bor's extra modes' package home) and rework Bor's forked `bor_downloader.go` — out of scope, non-consensus. Kept Bor's mode-taking `synchronise`/`BeaconSync`/`BeaconExtend`/`BeaconDevSync` signatures, `h.snapSync.Load()` checks, and `Ethereum.SyncMode()`.
- Removed the added `eth/downloader/syncmode.go`.
- Reverted to Bor HEAD (their entire delta this batch was #33157): `eth/handler.go`, `eth/handler_test.go`, `eth/handler_eth_test.go`, `eth/sync_test.go`, `eth/downloader/beaconsync.go`, `eth/downloader/beacondevsync.go`. Reverting `handler.go` also preserved Bor's auto-disable-snap logic in `enableSyncedFeatures` (upstream removed it, relying on the deferred `syncModer.disableSnap`).
- `eth/backend.go` — class 1 — took ours (kept Bor's `SyncMode()` method that upstream removed); the auto-merged `SlowBlockThreshold` line reverted with the #32812 deferral.
- `eth/syncer/syncer.go` — class 1 — took ours (`BeaconDevSync(downloader.FullSync, target)`, dropped upstream's `ConfigSyncMode` guard).
- `eth/catalyst/api.go` — class 1 — took ours on the 2 sync-mode conflicts (`api.eth.SyncMode()` vs upstream `Downloader().ConfigSyncMode()`); restored Bor's `mode` arg on the auto-merged `BeaconSync`/`BeaconExtend` call sites (build-caught). `#32754` getPayload guard auto-merged, kept (inert).

**Defer #32812 (slow-block stats) entirely** — logging entangled with upstream's `blockProcessingResult.stats`/`logSlow`, which Bor's tuple-returning `ProcessBlock` doesn't share. Removed `core/blockchain_stats.go` (new upstream file) and the `--debug.logslowblock` flag (`cmd/utils/flags.go` + `cmd/geth/{main,chaincmd}.go` registrations — build-caught dangling refs); reverted `core/blockchain.go`, `eth/backend.go`, `eth/ethconfig/{config,gen_config}.go` to Bor HEAD (their only delta this batch was #32812).

**Defer #33254 (reader cache-stats rename)** — Bor's `core/state/reader.go` carries richer prefetch-attribution instrumentation (the pipelined-SRC work) using the old field names, consumed by `core/blockchain.go` via `GetStats()`. Half-merged rename would break the build; reverted `core/state/reader.go` + `core/state/statedb.go` to Bor HEAD.

Independent conflicts resolved (Bor had no meaningful divergence → converged to upstream):

- `common/bitutil/bitutil.go` + `_test.go` — class 2 — took theirs: `XORBytes` now delegates to `crypto/subtle.XORBytes` (#33331); Bor's only divergence was blank-line reformat.
- `ethdb/pebble/pebble.go` — class 2 (1 hunk) — took theirs: `MemTableStopWritesThreshold: memTableNumber * 2` (#33353). Bor had independently set `* 2` (`memTableLimit`); upstream renamed the var to `memTableNumber` (auto-merged) and adopted the same `* 2`, so Bor's intent converged. Bor's custom pebble amplification/IO/WAL metrics (auto-merged block) preserved.
- `p2p/rlpx/rlpx.go` — class 2 (1 hunk, + build fix) — body auto-merged to `subtle.XORBytes`; the import conflict's "theirs" side was empty (upstream dropped `bitutil`), which also dropped Bor's `snappy`/`sha3`. Corrected: kept `snappy` + `sha3` (Bor's body uses both), dropped `bitutil`.
- `cmd/utils/flags.go` — class 2 (3 hunks) — took theirs: `--networkid` override relocation + explicit-set warning (#32999), and the preimages config-file bugfix (`if ctx.IsSet(CachePreimagesFlag.Name)`, #33330). Bor matched base on all three.
- `eth/api_debug.go` — class 1 (4 hunks) — took ours: Bor's `ProcessBlock(parentBlock, header, nil, nil)` (BlockSTM-V2/stateless) signature; base == theirs (upstream didn't touch these lines).

Verification (per-batch tier): `go build ./...` pass; `go vet` clean on all touched + `eth/…` packages (test files compile); `go test` pass on `common/bitutil`, `ethdb/pebble`, `p2p/rlpx`, `core/filtermaps`, `eth/filters`, `p2p/nat`, `core/vm` (+`program`,`runtime`), `triedb/pathdb`. gofmt clean. `misc/` not staged.

### batch 5/12 (boundary `5dfcffcf3`, 20 commits) — state/code-read metrics, tx fetcher validation, blobpool legacy sidecar removal

Conflicts: 15 files (13 content, 2 modify/delete). No hardfork/EIP introduced (see `fork-register.md`). Three refactors deferred (Bor-diverged, tracked in `needs-wiring.md`); several independent fixes adopted; one latent HEAD test break fixed.

Adopted (Bor had no meaningful divergence, or clean bugfix):

- **#33352 blobpool legacy sidecar removal** — took theirs on `core/txpool/blobpool/blobpool.go` (3 hunks: removed `conversionTimeWindow`, `convertLegacySidecar(s)`, `compareAndSwap`, `preCheck`), adopted the auto-merged deletion of `conversion.go`, and removed `conversion_test.go` (modify/delete: upstream deleted it; Bor's only change was an import reorder + one `t.Skip`). Bor's copy was dormant dead code (`//nolint:unused`; Osaka block-gated and inactive), merely carried from upstream — not a deliberate divergence, so converged. `blobpool_test.go` resolved per-hunk (theirs, theirs, **ours**): removed `setHeadTime` + `TestSidecarConversion` (test the removed machinery), **kept Bor's `TestSubscribeRebroadcastTransactions`**; upstream's now-unused `newUint64` dropped with it.
- **#33370** `p2p/tracker/tracker.go` — took theirs (the `wasHead` fix; the `wasHead :=` precompute auto-merged, and Bor's `req.expire.Prev()==nil` re-check after `Remove` was the pre-fix bug).
- **#33447** `tests/fuzzers/rangeproof/rangeproof-fuzzer.go` — took theirs (kv struct lost its unused 3rd field; the struct def auto-merged to 2 fields).
- `ethclient/gethclient/gethclient.go` — took theirs (`return &result, nil`).
- Auto-merged clean and kept: #33280, #33320, #33336, #33329, #33044, #33344, #33381, #33363, #33058, #33338, #32247/#32637 (state proof / overlay), #33440 (catalyst timestamp log), fulu beacon types #33349.

Preserved Bor divergence:

- `core/txpool/validation.go` — took **ours** on the EIP-7623 floor-data-gas gate: `opts.Config.IsPrague(head.Number) && opts.Config.Bor != nil` (block-based + Bor-chain guard) over upstream's `rules.IsPrague`. Fixed the auto-merged blob-sidecar-version check `opts.Config.IsOsaka(head.Number, head.Time)` → block-based `IsOsaka(head.Number)` (Bor's 1-arg signature).
- `tests/block_test_util.go` — took **ours** (Bor's `Run`/`RunV2`/`run(...,useV2 bool)` V2-BlockSTM differential harness) over #33383's inlining of `run` into `Run`; removed the duplicate `Network()` method the merge left (upstream added one; Bor already had it).

Deferred (Bor-diverged refactors → `needs-wiring.md`):

- **#33378 tx-announcement metadata validation** — upstream replaced the fetcher's `hasTx func(common.Hash) bool` callback with `validateMeta func(common.Hash, byte) error`, converting all call sites. Bor's fetcher is diverged (`txMetadata` type, 34 old-signature test call sites). Reverted `eth/fetcher/tx_fetcher.go`, `tx_fetcher_test.go`, `eth/handler.go`, `eth/handler_test.go`, `tests/fuzzers/txfetcher/txfetcher_fuzzer.go` to Bor HEAD (their entire batch delta was #33378 + the already-deferred #33157). Security-positive; good adopt candidate later.
- **State code-metrics changeset (#33442 code-read stats, #33376 code-metric fix, #33415 code-existence fix, #33400, #33254)** — entangled with Bor's diverged state package (`CommitWithUpdate`/`stateUpdate`, prefetch-attribution instrumentation, BlockSTM-V2, direct-timer metrics) and its `Reader` interface / `commitAndFlush` signature. First attempted to adopt just the `CodeReads`/`CodeLoaded` fields, but the auto-merge had silently replaced Bor's consensus-critical `CommitWithUpdate` with upstream's `CommitAndTrack` and cascaded into test-helper breaks (`Reader.Has`, `commitAndFlush` arity). Deferred the whole changeset: reverted `core/state/{statedb,state_object,reader,state_sizer,stateupdate}.go`, `core/blockchain.go`, and their tests to Bor HEAD.
- **#33157 syncModer** (from batch 4) recurred via `eth/handler.go`/`handler_test.go`; kept deferred (reverted to HEAD).

Latent HEAD fix:

- `core/blockchain_test.go` — Bor HEAD called `blockchain.reportBadBlock(...)` but Bor's `blockchain.go` defines `reportBlock` (upstream's rename was declined in batch 4; the test wasn't updated and batch 4's verification didn't vet `core/` test files). Fixed the 2 call sites to `reportBlock`. This makes the `core` test package compile again.

Verification (per-batch tier): `go build ./...` pass; `go vet` clean except a pre-existing `//nolint:govet`-suppressed copylocks in `core/parallel_state_processor.go` (Bor BlockSTM-V1 rerun path; golangci-lint honors the nolint, raw `go vet` doesn't); `go test` pass on `core/txpool/blobpool`, `core/txpool`, `core/txpool/legacypool`, `ethclient/gethclient`, `triedb/pathdb`, `eth/tracers/native`; `core` test binary compiles. gofmt clean; `misc/` not staged.

### batch 6/12 (boundary `710008450`, 20 commits) — snap-sync locking, getBlobsV3, pathdb history indexing, go-verkle removal

Large batch — ~40 conflicts (content + many modify/deletes). Dominated by three cross-cutting upstream removals colliding with Bor divergences: go-verkle/go-ipa removal (#33461), deprecated vuln-check command removal (#33498), and the recurring syncmode/downloader refactors. No hardfork/EIP introduced (see `fork-register.md`).

Adopted (Bor no divergence / clean):

- **#33498 remove deprecated `version-check` command** — Bor's copy was inherited (version_check.go diff vs base was cosmetic; upstream also deleted the whole `vcheck` testdata dir, so keeping Bor's test would break). Removed `cmd/geth/version_check.go`, `version_check_test.go`, `testdata/vcheck/vulnerabilities.json` (+ auto-deleted testdata), took theirs on `misccmd.go` (drop `versionCheckCommand`), and dropped the `versionCheckCommand` registration in `main.go` (kept `verkleCommand`).
- **#33474 blobpool slotter** — Bor deliberately disabled the EIP-7594 slotter migration (`// TODO enable and fix?`, commented out; Osaka dormant); took ours and removed the now-unused `eip4844` import.
- `SECURITY.md` — took ours (Bor's Polygon security-contact doc).
- Auto-merged clean & kept: #33404 getBlobsV3 (+ new `eth/catalyst/metrics.go`), #33196, #33322, #33440, #33303 (pathdb history indexing), #33044, #33505, #33225, #33444, #33541 (in reverted files), #33483, #33395's non-Bor parts.

Preserved Bor divergence — **kept Bor's Verkle (declined #33461)**:

- Upstream #33461 removes all go-verkle/go-ipa references (go-ethereum abandoned the go-verkle-based verkle implementation). Bor **deferred** Verkle (user-directed; `VerkleBlock=nil`, dormant) and its `core/types/block.go` `ExecutionWitness` (`verkle.StateDiff`/`VerkleProof`), `core/state` trie readers (`newTrieReader` `PointCache`), and trie scaffolding still use go-verkle. Removing Bor's dormant verkle is a deliberate deferred-feature decision, not a merge call — so reverted all 15 #33461-touched Bor files to HEAD (`core/types/block.go`, `core/state/{statedb,statedb_hooked,database,access_events,reader,state_object,database_history,access_events_test}.go`, `core/vm/{evm,interface}.go`, `consensus/beacon/consensus.go`, `beacon/engine/{types,gen_ed}.go`, `trie/bintrie/{trie,key_encoding}.go`), kept the verkle files (`trie/verkle*.go`, `trie/utils/verkle*.go`, `cmd/geth/verkle.go`), kept go-verkle/go-ipa in `go.mod` (took ours + `go mod tidy`), and kept `verkleCommand` in `main.go`. **Flagged for the team** (`fork-register.md` + `needs-wiring.md`): keeping Bor's dormant go-verkle now costs a full #33461-file revert every batch and the impl is upstream-orphaned; the team should decide whether to drop Bor's verkle.

Deferred (Bor-diverged / entangled → `needs-wiring.md`):

- **#33157 syncModer** (recurring) — reverted `eth/handler.go` to HEAD.
- **State code-metrics** (recurring #33442/#33376/#32812) — auto-merged as ours (Bor's no-metrics version).
- **#33486 finalized-block cleanup on rewind** — initially took theirs, but that **collaterally dropped Bor's own `SnapSyncCommitHead`** (adjacent Bor method required by `bor_downloader.go`'s interface) — reverted `core/blockchain.go` to HEAD. Correctness fix; adopt later into Bor's forked setHeadBeyondRoot.
- **#33428 snap-sync lock protection** — touches Bor's forked `blockchain.go`/`skeleton.go`/`beaconsync.go`; reverted those to HEAD. Concurrency-safety fix; adopt later.
- **#33481 stale beacon header deletion** — Bor's forked `skeleton.go`/`beaconsync.go`; reverted to HEAD. Correctness fix; adopt later.
- **#33395 ethstats newPayload processing time** — needs a `newPayloadFeed` in Bor's forked `blockchain.go`; reverted `ethstats.go`, `core/events.go`, `blockchain_reader.go`, `api_backend.go` to HEAD and surgically removed the auto-merged `SendNewPayloadEvent` emit + `start`/`processingTime` from `eth/catalyst/api.go`.

Deferred-recurrence modify/deletes (kept Bor's deletion): `.github/workflows/validate_pr.yml`, `core/blockchain_stats.go` (#32812), `eth/downloader/downloader.go` (Bor's bor_downloader rename), `eth/downloader/syncmode.go` (#33157).

Tests: took ours — `core/blockchain_test.go` (Bor's `TestStatelessModeRewind`), `eth/catalyst/api_test.go` (Bor's skipped `TestGetBlobsV2`).

Verification (per-batch tier): `go build ./...` pass; `go vet ./core/... ./eth/... ./cmd/geth/... ./trie/... ./consensus/... ./ethstats/...` clean except two pre-existing `//nolint:govet` copylocks (`core/parallel_state_processor.go`, `trie/secure_trie.go` — Bor concurrency divergences, golangci-lint honors the nolint); `go test` pass on `core/txpool/blobpool`, `trie`, `trie/utils`; `core`/`eth/catalyst`/`cmd/geth`/`eth/downloader` test binaries compile. `go mod tidy` clean (go-verkle retained). gofmt clean; `misc/` not staged.

### follow-up commit (on top of batch 6/12 `85a1750c5`) — remove dormant go-verkle scaffolding (mirror upstream #33461)

User-approved 2026-07-23. go-ethereum abandoned the go-verkle/go-ipa verkle implementation (#33461); Bor never enabled Verkle (`VerkleBlock=nil`). Instead of re-reverting #33461 every batch (orphaned, unmaintained), Bor **mirrors upstream's removal patch-by-patch** — this commit mirrors #33461; future upstream verkle stub PRs get mirrored as they arrive in later batches.

Mirrored #33461 onto Bor's diverged files (preserving BlockSTM / `CommitWithUpdate` / prefetch / stateless):

- **Deleted:** `trie/verkle.go`, `trie/verkle_test.go`, `trie/utils/verkle.go` (incl. `PointCache`), `cmd/geth/verkle.go`.
- **`core/types/block.go`** — removed the verkle `ExecutionWitness` type (`StateDiff`/`VerkleProof`), the `witness` field, `WithWitness`, `ExecutionWitness()`, and the go-verkle import. (Bor's *stateless* client uses `stateless.Witness`, untouched.)
- **`core/vm/interface.go`** — removed `PointCache()` from the `StateDB` interface + `trie/utils` import. **`core/vm/evm.go`** — `NewAccessEvents(evm.StateDB.PointCache())` → `NewAccessEvents()`.
- **`core/state`** — removed `PointCache` field/method/interface-member and `trie/utils` imports from `database.go`, `database_history.go`, `reader.go` (`newTrieReader` drops the `cache` param), `statedb.go`, `statedb_hooked.go`, and Bor's `parallel_statedb.go`; removed the `trie.VerkleTrie` `mustCopyTrie`/`deepCopy` cases (→ `bintrie.BinaryTrie`). Took theirs for `access_events.go`/`access_events_test.go` (key derivation switches from verkle `PointCache` to `bintrie.GetBinaryTreeKey`; no Bor divergence) and `trie/bintrie/{key_encoding,trie}.go` (binary tree kept + extended — provides the key funcs `AccessEvents` now uses). Fixed Bor's `parallel_statedb_coverage_test.go` (dropped the `PointCache()` accessor assertion).
- **`beacon/engine/{types,gen_ed}.go`** — took theirs (removed the verkle `ExecutionWitness` payload field; low Bor divergence). `consensus/beacon/consensus.go` — **no change needed** (Bor's version already has no verkle witness-attachment; its `consensus.Engine` diverges heavily, so left at HEAD).
- **`go.mod`/`go.sum` + `cmd/keeper`** — removed `go-verkle`/`go-ipa`; `go mod tidy`. **`cmd/geth/main.go`** — removed `verkleCommand` registration.
- **Kept:** the dormant Verkle fork gate (`VerkleBlock`/`IsVerkle`/`OverrideVerkle`) — upstream keeps `isVerkle` too per #33461's author note.

Verification: `go build ./...` pass; `go vet ./core/... ./eth/... ./trie/... ./consensus/... ./beacon/... ./cmd/geth/...` clean except the two pre-existing `//nolint:govet` copylocks. `go test` pass: `core/state` (excl. pre-existing `TestPDBMethodParity` drift — see below), `trie`, `trie/bintrie`, `trie/trienode`, `core/state/snapshot`, `core/stateless`. gofmt clean; `go mod tidy` clean.

**Pre-existing latent failures found (NOT caused by this change; confirmed via a worktree at `85a1750c5`):**
- `core/state` `TestPDBMethodParity` — `*StateDB.DumpBinTrieLeaves` has no `*ParallelStateDB` equivalent/exemption (drift introduced by an earlier merge #32445; fails at the batch-6 tip too). Fix: add `DumpBinTrieLeaves` to `pdbExemptMethods`. Tracked for milestone verification.
- `core/vm` `TestInterruptDuringExecution`/`TestAbortDuringJump` — **flaky** (timing/interrupt-based; fail ~1/3 runs, pass at batch-6 tip and on retry).
- Root cause both slipped through: per-batch tier compiled but did not *run* the full `core/state`/`core/vm` suites. Milestone tier must run them; consider adding `core/state` to the per-batch run.

## v1.17.0 batch 7/12 (`94710f79a`, batch 8)

Merge `94710f79a` into `ppatil-upstream-v1.17.0` (on top of the go-verkle-removal commit `b66d3f157`). 20 first-parent commits: keystore panic fix, ecies aes blocksize, KZG-proof peer-drop, v1.17.0 release-cycle version bump, rpc buffer limit, RPC tx formatter refactor, rawdb tx-unindex robustness, chainHeadFeed dedup, ethclient BlockReceipts, blobpool gaps, trienode-history compression, pathdb history-index extension, state update hook (#33490), code-reader cache stats (#33532). 15 conflicted files.

No hardfork/EIP introduced (see `fork-register.md`).

Converged with upstream (class 2 — Bor had no meaningful divergence / only reformat):

- **`version/version.go`** — took theirs (`1.17.0 unstable`). Bor's `1.16.9 stable` came solely from merging upstream's v1.16.9 release commit; this file tracks the upstream base, so it advances onto the v1.17.0 line (matches batch-1 precedent).
- **`accounts/keystore/presale.go`** — took theirs (the `len(cipherText)%aes.BlockSize` panic guard, #33602). Bor's only divergence was a stray blank line (reformat). Security-positive.
- **`core/rawdb/chain_iterator.go`** — took theirs on the decode-error hunk (#33573: build a `blockTxHashes{err}` and skip missing bodies instead of `return`). Bor's `var result` is upstream's (auto-merged before the conflict), so the `result :=` short-decl side couldn't compile. Preserved Bor's ancient-pruning divergence (`adjustRangeForBor`, `AncientOffSet`, `ItemAmountInAncient`, `to <= from`) — Bor-only code the merge base never had, so it survived auto-merge untouched (verified in the working tree).
- **`internal/ethapi/api.go`** (x4 hunks) — took theirs (#33582's `flattenTxs(...)` helper). Helper auto-merged in and calls the same `NewRPCPendingTransaction`, so Bor's path is preserved transitively. Bor's only divergence was a blank line.

Preserved Bor divergence (class 1 — kept ours):

- **`eth/fetcher/tx_fetcher.go`** — kept Bor's richer protocol-violation log (`"direct"`, `"hashes"` fields from Bor's peer-jailing #2283) over upstream's slimmer line. Logging-only superset, no consensus impact; all referenced `txDelivery` fields exist.

Combined both intents (class 3):

- **`triedb/pathdb/history_index.go` + `history_index_iterator.go`** — upstream #33399 added an `indexReader.bitmapSize` field and (via #32981) relocated `newIterator` into `history_index_iterator.go` with a new `newIterator(filter)` signature, deleting the old no-arg method. Bor #1875 added `mu sync.RWMutex` to `indexReader` for thread-safe `readers`-map access. Resolution: (1) struct — kept `mu` and added `bitmapSize`; (2) `refresh()` — took upstream's unconditional `delete(r.readers, last.id)` under Bor's `r.mu.Lock/Unlock`; (3) took upstream's deletion of the old `newIterator` (relocated); (4) ported Bor's #1875 mutex onto the relocated cache access in `history_index_iterator.go`'s `newBlockIter` (`r.mu.RLock` around the read, `r.mu.Lock` around the write) — without this, deferring conflict 3 alone would have moved `indexReader.readers` access to an unprotected site and reintroduced the concurrent-map race #1875 fixed (state-security). `history_reader.go`'s #1875 mutex survived auto-merge intact (no action). `newIndexIterator` fully removed upstream (no orphan). Verified: `go test -race` on the pathdb history-index/iterator tests passes.

Deferred (Bor-diverged / entangled -> `needs-wiring.md`):

- **#33490 new state update hook** (`01b39c96b`) — entangled state-tracking continuation of the batch-6 code-metrics line, colliding with Bor's `CommitWithUpdate`/`stateUpdate`/BlockSTM divergence. Every one of its 14 touched files had #33490 as its sole upstream PR this batch (verified), so reverted all 14 to Bor HEAD: `core/blockchain.go`, `core/genesis.go`, `core/genesis_test.go`, `core/state/{statedb,state_sizer,stateupdate,state_object}.go`, `core/tracing/hooks.go`, `cmd/geth/chaincmd.go`, `core/chain_makers.go`, `core/headerchain_test.go`, `eth/filters/{filter_system_test,filter_test}.go`, `tests/block_test_util.go`. The `SetupGenesisBlockWithOverride` `tracer`-arg signature change had auto-merged into genesis.go/blockchain.go/chaincmd.go call sites — reverting all together keeps the signature consistent (no half-applied arg).
- **#33532 code-reader cache stats** (`9623dcbca`) — same reader-instrumentation surface Bor diverges on (batch-5 #33254). Reverted `core/state/database.go` + `core/state/reader.go` to HEAD (each #33532-only this batch).
- **#33563 duplicate `chainHeadFeed.Send` dedup** (`127d1f42b`) — trivial `sendChainHeadEvent()` extraction that auto-merged cleanly, but shares `core/blockchain.go` with #33490. blockchain.go reverted wholesale to Bor HEAD for safety (most consensus-critical file), deferring this clean cleanup with it. No behavior change; easy re-apply.

Merge artifact fixed:

- **`eth/fetcher/tx_fetcher_test.go`** — upstream `5b99d2bba` (KZG peer-drop) re-added `makeInvalidBlobTx` + a running `TestTransactionProtocolViolation` using #33378's `func(common.Hash, byte) error` `NewTxFetcher` signature; Bor already carries an adapted copy (same helper + a `t.Skip("bor: not relevant, ...")` test against Bor's retained `func(common.Hash) bool` signature). Auto-merge left duplicate `makeInvalidBlobTx`/`TestTransactionProtocolViolation` declarations (vet: "redeclared in this block"). Deleted upstream's duplicate copy (which wouldn't compile against the merged `NewTxFetcher`); kept Bor's skipped copy (matches the merged signature). Recorded in `needs-wiring.md` (wontfix, depends on the #33378 fetcher-signature adoption).

Verification (per-batch tier): `go build ./...` pass (rc=0); `go vet ./core/... ./eth/... ./triedb/... ./internal/ethapi/... ./accounts/keystore/... ./version/...` clean except the two pre-existing `//nolint:govet` copylocks (`core/parallel_state_processor.go`, `trie/secure_trie.go`); unit tests pass on `accounts/keystore`, `core/rawdb`, `internal/ethapi`, `version`, `eth/fetcher` (137s), `core/state/snapshot`, and `triedb/pathdb` (fresh `-count=1 -race`). `core/state` fails only on the pre-existing `TestPDBMethodParity` (`DumpBinTrieLeaves` drift from #32445 — present at HEAD/batch-6 tip, not a batch-7 regression; milestone fix = add to `pdbExemptMethods`). No leftover conflict markers; `git diff --cached --check` clean; `misc/` not staged.

## v1.17.0 batch 8/12 (`8fad02ac6`, batch 9)

Merge `8fad02ac6` into `ppatil-upstream-v1.17.0`. 20 first-parent commits, 25 conflicted files across ~7 feature clusters. No hardfork/EIP introduced (see `fork-register.md`). Large, entanglement-dense batch — resolved by adopting clean standalone features and deferring the ones tangled with Bor's consensus-critical divergences.

Adopted:

- **Trienode-history (#32621 enable, #33551 indexing, #33584 big-endian bitmap)** — files already existed in Bor HEAD (prior-batch groundwork), mostly auto-merged; 2 conflicts combined: `triedb/pathdb/buffer.go` `flush` signature (upstream's `freezers []ethdb.AncientWriter` + Bor's `nodesCache *AddressBiasedCache` — the auto-merged body already uses `syncHistory(freezers...)` and Bor's AddressBiasedCache), and `history_indexer.go` imports (added `golang.org/x/exp/maps`; deduped `errgroup` already at line 27). `BlockChainConfig` (`core/blockchain.go`) + backend init (`eth/backend.go`) combined Bor's `AddressCacheSizes`/`PreloadRateLimit` with upstream's `TrienodeHistory`. Default `TrienodeHistory: -1` (disabled/dormant). `gen_config.go` TOML marshaling for the field deferred to milestone regen (`needs-wiring.md`). `-race` pathdb tests pass.
- **Grafana Pyroscope profiling (#33623)** — `internal/debug/flags.go` + new `internal/debug/pyroscope.go` (auto-merged); `go mod tidy` added `github.com/grafana/pyroscope-go v1.4.1`.
- **legacypool per-account metric (#33646)** — combined: kept Bor's extensive txpool metrics block + added `pendingAddrsGauge`/`queuedAddrsGauge` (used by the auto-merged body).

Declined — already ours / equivalent (`needs-wiring.md` wontfix):

- **rpc.rangelimit (#33163)** — Bor already has `RPCBlockRangeLimit` → `filters.Config.RangeLimit` (covers eth_getLogs *and* bor_getLogs) via Bor's own `RPCGlobalRangeLimitFlag` on the same `rpc.rangelimit` CLI name. Auto-merge produced duplicate flag defs (`flags.go` 672 & 690), duplicate setters (setting the declined `cfg.RangeLimit`), and duplicate main.go registration — two flags with the same name would panic at startup. Removed upstream's duplicate flag/setter/registration + `ethconfig.RangeLimit` field + `NewRangeFilter` `rangeLimit` param; reverted `eth/filters/{api,filter,filter_system,filter_test}.go` + `graphql/graphql.go` to HEAD; kept Bor's config/flag/gen_config.

Deferred — entangled with Bor consensus divergence (`needs-wiring.md`):

- **core/vm write-protection + selfdestruct cluster (#33281 gas-handler write-protection, #33637 per-opcode read-only, #33450 selfdestruct cold-access gas, #32919 selfdestruct tracer hooks)** — intertwined across `gas_table.go`/`operations_acl.go`/`instructions.go`/`interface.go`. #32919 makes `StateDB.SelfDestruct` void, removes `SelfDestruct6780` from the `vm.StateDB` interface, adds `IsNewContract` — clashes with Bor's BlockSTM `*StateDB.SelfDestruct` (returns prevBalance + `MVWrite`; `SelfDestruct6780` consumes the return). All four intertwined → reverted `core/vm/{gas_table,operations_acl,instructions,interface}.go`, `core/state/{statedb,statedb_hooked,statedb_hooked_test}.go`, `core/tracing/{hooks,gen_nonce_change_reason_stringer}.go` to HEAD; removed the new `eth/tracers/internal/tracetest/selfdestruct_state_test.go` + `selfdestruct_test_contracts/*.yul`. Bor's existing write-protection + gas accounting (EIP-214 unconditional; EIP-2929 cold-access) are correct and tested (`core/vm` tests pass).
- **#33644 deterministic hook emission order** — `statedb_hooked.go`; builds on the batch-8-deferred #33490 hook infra. Reverted to HEAD.
- **OpenTelemetry JSON-RPC tracing (#33452)** — new `internal/telemetry` pkg + `tracerProvider` threaded through `rpc/{client,handler,server,service}.go` (`newHandler` collides with Bor's `pool *SafePool`; `serviceRegistry.callback` → 3 returns). Reverted the 4 rpc files to HEAD; removed `internal/telemetry/telemetry.go` + `rpc/tracing_test.go`; otel deps dropped by tidy.
  - **Correction (review of #2319, 2026-09-01):** the revert missed `rpc/http.go`, which kept #33452's `otel.GetTextMapPropagator().Extract(...)` in `Server.ServeHTTP` plus the two otel imports. Not inert — Bor installs a global `propagation.TraceContext` at `internal/cli/server/server.go:470`, so caller-supplied `traceparent` headers would have been stitched into Bor's own spans while nothing in `rpc` consumed the extracted context. Removed, completing the decline. Bor's own otel usage (`common/tracing`, `internal/cli/server`, `miner/worker.go`) is unaffected and still holds the module in `go.mod`.
- **#33647 signature-length panic fix** — Bor keeps its per-fork signer chain (`pragueSigner`/`cancunSigner`) + 3-return `decodeSignature`; the fix is embedded in the `modernSigner` refactor + `tx_setcode.go`. Reverted `core/types/{transaction_signing,transaction_signing_test,tx_setcode}.go` to HEAD. Bor's `decodeSignature` still panics on bad length, but only on the signing path (crypto-generated 65-byte sigs), not the peer-facing recover path — low external exploitability.
- **#33610 fetcher test refactor** — depends on the batch-6-declined #33378 `NewTxFetcher(func(common.Hash, byte) error, …)` signature; reverted `eth/fetcher/tx_fetcher_test.go` to HEAD.

Deps: took Bor's `go.mod`/`go.sum` + `cmd/keeper/go.{mod,sum}`; `go mod tidy` reconciled (added pyroscope, no otel).

Verification (per-batch tier): `go build ./...` pass (rc=0); `gofmt` clean on edited files; `go vet ./core/... ./eth/... ./triedb/... ./cmd/... ./rpc/... ./internal/... ./graphql/... ./accounts/...` clean except the two pre-existing `//nolint:govet` copylocks. `go test` pass: `triedb/pathdb` (29s, incl. `-race` earlier), `core/txpool/legacypool` (18s), `eth/ethconfig`, `core/vm` (5s), `core/types`, `core/state/snapshot`, `eth/filters`. `core/state` fails only on the pre-existing `TestPDBMethodParity` (`DumpBinTrieLeaves`/#32445; present at HEAD; milestone fix). No leftover conflict markers; `git diff --cached --check` clean; `misc/` not staged.

## v1.17.0 batch 9/12 (`845009f68`, batch 10)

Merge `845009f68` into `ppatil-upstream-v1.17.0`. 20 first-parent commits, 17 conflicted paths (16 `UU` + 1 `DU`). Fork surface: EIP-8024 immediate-byte update (#33614) — dormant, no gate touched (see `fork-register.md`). Two real features (eth_getProofs-for-history, callTracer log index) mostly auto-merged; the recurring code-stats / slow-block deferred lines resurfaced as base-vs-HEAD conflicts.

Adopted:

- **eth_getProofs for history (#32727)** — mostly auto-merged (`core/state/database_history.go` +199; `triedb/pathdb/{history_reader.go +201, reader.go +87, history_index_iterator.go +45, history_trienode.go, nodes.go, disklayer.go, metrics.go}`). Conflicts: `triedb/pathdb/history_indexer.go` — removed the now-**unused** `golang.org/x/exp/maps` import (#33681 dropped its usage; HEAD had it unused, exactly why upstream removed it), kept `errgroup`; `triedb/pathdb/history_reader.go` — added `sync/atomic` (auto-merged code uses it); `core/rawdb/database.go` — combined known-metadata keys (Bor's `bytecodeSyncLastBlockKey` + upstream's `headTrienodeHistoryIndexKey`).
- **NodeFullValueCheckpoint / trienode `FullValueCheckpoint` compression knob (#32727)** — consistent with batch-8's adopted trienode-history. `triedb/pathdb/config.go`: combined Bor's `defaultStateReservation`/`defaultPreloadRateLimit` consts + `StateReservation` field with upstream's `maxFullValueCheckpoint`/`defaultFullValueCheckpoint` consts + `FullValueCheckpoint` field + sanitization block (deduped against the auto-merged struct-top fields that upstream relocated). `FullValueCheckpoint` is consumed by auto-merged `disklayer.go`/`reader.go`. `NodeFullValueCheckpoint` field + CLI flag (`--history.trienode.full-value-checkpoint`) auto-merged into `ethconfig.Config`/`cmd/utils/flags.go`; added `Defaults.NodeFullValueCheckpoint = pathdb.Defaults.FullValueCheckpoint` + the `core.BlockChainConfig` literal wiring (`eth/backend.go`, `core/blockchain.go`, historical-state group). Dormant while `TrienodeHistory = -1`. TOML marshaling → milestone `gen_config` regen (`needs-wiring.md`).
- **callTracer log index (#33629)** — added `Index` to the tracer `callLog` (`eth/tracers/native/call.go`, auto-merged) **and** the test-local `callLog` type (`calltrace_test.go`, was missing it → `want` dropped index on round-trip). Regenerated all 6 `call_tracer_withLog` golden fixtures from actual Bor tracer output, **preserving Bor's synthetic `0x1010` fee-transfer logs** (the `_withLog` harness injects `BorConfig`). Fixed the inline `TestInternals` "Mem expansion in LOG0" want (LOG0 = index `0x0`, Bor fee log = `0x1`). Regen used a temporary env-gated hook (`REGEN_WITHLOG`), since removed; the hook only rewrote each fixture's `result`, preserving `genesis`/`context`/`input`/`tracerConfig` verbatim.
- **legacypool non-executable heartbeat fix (#33704)** — added the missing `pool.priced.Removed(len(olds) + len(drops))` to `demoteUnexecutables` and adopted the clarified `Lifetime` comment; kept Bor's `txConditionalsRemoved` handling.
- **crypto/ecies (#33669)** — v1.16.9 duplicate; the `IsOnCurve` invalid-curve check is already present at HEAD. Converged the stray blank-line conflict to upstream form.
- Auto-merged: alloc reductions (#33698/#33690/#33689/#33703), #33697 pebble seek-compaction, #33711/#33694 trie fixes, #33712 heap-eviction bounds check, #33686 keeper wasm (`cmd/keeper/getpayload_wasm.go`, new), #33693 ethclient timeout, #33653/#33654 legacypool gauge/counter, #33681 trienode reader.

Deferred — entangled with Bor divergence (`needs-wiring.md`):

- **#33659 extend the code reader statistics** — continuation of the batch-5 (#33254) / batch-6 (#33442 line) code-read-metrics deferrals. Clashes with Bor's `PrefetchStats` / pipelined-SRC reader instrumentation + direct-`*Timer.Update()` stats. Reverted `core/state/{reader,state_object,statedb}.go` to HEAD; kept `core/blockchain_stats.go` deleted (Bor removed it earlier — `DU` modify/delete, kept deleted); did not add `codeReadTimer`/`codeReadBytesTimer`. `core/blockchain.go` conflicts (metrics timers, `BlockChainConfig` field, `processBlock` signature, per-block stats block) all resolved to Bor HEAD, preserving the adopted `FullValueCheckpoint` wiring.
- **#33655 standardize slow-block JSON output** — the slow-block feature was deferred wholesale in batch 5 (#32812; Bor's tuple-returning `ProcessBlock` lacks upstream's `stats` struct). This batch's format refinement kept deferred: `cmd/utils/flags.go`, `core/blockchain.go`, `eth/ethconfig/{config,gen_config}.go` resolved to HEAD (no `SlowBlockThreshold`/`LogSlowBlockFlag`).

Verification (per-batch tier): `go build ./...` pass (rc=0); `gofmt` clean on edited files; `go vet ./core/... ./eth/... ./triedb/... ./cmd/... ./internal/... ./crypto/...` clean except the pre-existing `//nolint:govet` copylocks (`core/parallel_state_processor.go:341`). `go test` pass: `triedb/pathdb` (54s), `core/txpool/legacypool` (20s), `eth/ethconfig`, `eth/tracers/...` (all incl. `tracetest` `_withLog`), `crypto/ecies`, `core/rawdb` (57s), `core/state/snapshot`, `core/vm` (EIP-8024 + continuity + jump-table). `core/state` fails only on the pre-existing `TestPDBMethodParity` (`DumpBinTrieLeaves`/#32445; milestone fix). `core/vm` `TestInterruptDuringExecution`/`TestAbortDuringJump` flaky (2/3 pass) — `dispatch_test.go` unchanged by this batch, `time.Sleep(5ms)`-based interrupt race, pre-existing. No leftover conflict markers; `git diff --cached --check` clean; `misc/` not staged.

## v1.17.0 batch 10/12 (`c12959dc8`, batch 11)

Merge `c12959dc8` into `ppatil-upstream-v1.17.0`. 20 first-parent commits, 22 conflicted paths (all `UU`). **No new fork/EIP surface**; no fork gating / precompile / gas-schedule change (see `fork-register.md`). The dominant theme is the `crypto/keccak` vendoring (#33323) rippling across every Bor-touched hasher file, plus two batch-wide upstream type migrations (`vm.TxContext.GasPrice` and `stateObject.addrHash`) that surfaced in Bor-only code the merge couldn't auto-fix.

Adopted (converged to upstream, Bor divergences preserved):

- **keccak vendoring (#33323)** — mechanical `golang.org/x/crypto/sha3` → `github.com/ethereum/go-ethereum/crypto/keccak` rename. 12 conflicted files resolved to Bor's side + `sha3.`→`keccak.` + `goimports` (preserves Bor's `-local` grouping): `common/types.go`, `internal/blocktest/test_hash.go`, `consensus/ethash/consensus.go`, `cmd/evm/internal/t8ntool/execution.go` (`rlpHash`), `core/rawdb/accessors_chain_test.go`, `core/rlp_test.go`, `core/state_processor_test.go`, `core/state/snapshot/generate_test.go`, `eth/protocols/snap/sync_test.go`, `p2p/rlpx/rlpx.go`, `tests/state_test_util.go`, `trie/trie_test.go`. Ripple (auto-merged call sites left dangling `sha3` imports) fixed via `goimports`: `accounts/accounts.go`, `p2p/enode/idscheme.go`, `p2p/dnsdisc/tree.go`, `consensus/clique/clique.go`. Bor's own `consensus/bor/*` + `tests/bor/*` keep `x/crypto/sha3` (not a conflict; Bor-authored, dep retained).
- **metrics registry API (#33699/#33748/#33749)** — upstream moved `getOrRegister(name, ctor, r)` → `r.GetOrRegister(name, func() any {…}).(T)`. `metrics/histogram.go` (2 hunks) + `metrics/runtimehistogram.go` resolved to upstream: upstream's `if r == nil { r = DefaultRegistry }` subsumes Bor's Yoda `nil == r` nil-guard (semantically identical); Bor's divergence was cosmetic (blank lines / Yoda style).
- **rlp `RawList` / iterator offset (#33755)** — `rlp/iterator.go` (`listIterator`→`Iterator` + `offset` field) and `rlp/iterator_test.go` (additive `Offset()` assertion) resolved to upstream; Bor divergence was blank-line-only.
- **`vm.TxContext.GasPrice` `*big.Int`→`*uint256.Int` migration** — struct field + main `NewEVMTxContext` bridge (`uint256.MustFromBig`) auto-merged. Adapted Bor-only sites the merge couldn't reach: `core/evm.go` `NewEVMTxContextForStateSync` (`uint256.NewInt(0)`); `core/vm/dispatch_test.go` (×2) + `core/vm/dispatch_bench_test.go` (Bor EVM-switch-dispatch tests). `opGasprice` (`core/vm/instructions.go`) → upstream `scope.Stack.push(evm.GasPrice.Clone())` (Bor divergence was a stray blank line). Non-`TxContext` `GasPrice` fields (Miner/Message/CallMsg/args) correctly left `*big.Int`.
- **`stateObject.addrHash` field→method (`addrHash() common.Hash`, lazy-hash alloc reduction)** — upstream call sites auto-merged; adapted Bor's pipelined-SRC prefetch loop in `core/state/statedb.go` (`obj.addrHash` → `obj.addrHash()`).
- **eip4844 blob config API (`latestBlobConfig` `*BlobConfig`→`(BlobConfig, error)`)** — the function body + 5 of 7 callers auto-merged to upstream's error form, so keeping Bor's pointer signature would have broken them. Resolved the 2 conflict hunks to upstream's return type while preserving Bor's divergences: signature keeps Bor's `_ uint64` (block-based, no timestamp); body keeps Bor's single-arg `IsOsaka/IsPrague/IsCancun(london)` gating + no BPO forks (auto-merged correctly); `CalcExcessBlobGas` keeps Bor's **`return 0` on no-config** (consensus-safe: Bor can call this before blob activation on its block schedule) instead of upstream's `panic`, adapted to `if err != nil`.
- **freezer table close (#33776)** — `core/rawdb/ancient_utils.go` took upstream's `defer table.Close()`.
- Auto-merged (no conflict): metrics alloc/GetAll cases across `counter*/gauge*/meter/timer/registry`, `rlp/raw.go` `RawList`, `signer/core/apitypes` cell proofs (#32910) + 128KB-blob copy avoidance (#33717), Ledger Gen5 (#33297), eth_getTransactionByHash timestamp (#33709), eth_simulateV1 revert code (#33007), bintrie witness fix (#33739), miner block-building alloc (#33375), pathdb encode/decode preallocation (#33736/#33715).

Kept Bor divergence (declined upstream change):

- **`params.Rules.ChainID` retained** — upstream removed the `ChainID` field from `Rules` (struct-def deletion auto-merged; nothing in Bor reads `rules.ChainID`, build-confirmed). Restored it because Bor's `TestReinforceMultiClientPreCompilesTest` guard (`core/vm/contracts_test.go`) reflects over `Rules` fields and lists `ChainID` as expected — the guard exists specifically to force review of Bor↔Erigon precompile/Rules parity, so silently editing its expected list during a merge is exactly what it forbids. Restored the field + `chainID` local + `ChainID: new(big.Int).Set(chainID)` initializer; kept Bor's block-based `Rules()` (`_ uint64` param, single-arg `Is<Fork>(num)`, Bor fork bools, no `IsAmsterdam`).
- **`opKeccak256` `Keccak256Cache` (`core/vm/instructions.go`)** — upstream retired the reused-`evm.hasher` pattern for `hash := crypto.Keccak256Hash(data)` and deleted the `hasher`/`hasherBuf` EVM struct fields (deletion auto-merged). Kept Bor's actual optimization (the 64-byte Solidity-slot result cache) and adapted it to upstream's shape: cache stores/loads `common.Hash`, miss path uses `crypto.Keccak256Hash`, preimage recording preserved via the shared tail. `evm.go` kept Bor's `jumpDests: jd` and dropped the now-removed `hasher:` init.

Deferred/declined — entangled with Bor divergence (`needs-wiring.md`):

- **legacypool alloc reduction (#33701)** — upstream reworked `addTxsLocked(txs, errs []error)` (caller-provided errs slice, in-place error set, skip pre-errored). Entangled with Bor's async txpool divergence (`addTxs(txs, async bool)` + `pool.add(tx, async)` + nilSlot merge in `Add`); the refactor's non-conflicting parts auto-merged and clashed with Bor's async signature. #33701 was the sole batch-10 commit touching `legacypool.go`, so restored the file wholesale to HEAD (declined). Micro-opt (one slice alloc/batch); revisit harmonizing into Bor's async path.

Verification (per-batch tier): `go build ./...` pass (rc=0); `gofmt` clean on all edited files. `go vet` on the touched set clean except the pre-existing `//nolint:govet` copylocks (`core/parallel_state_processor.go:341`; `atomic.Int64` fields predate this batch; raw `go vet` doesn't honor the nolint). `go test` pass: `metrics`, `rlp`, `common`, `params`, `consensus/misc/eip4844`, `consensus/ethash`, `p2p/rlpx`, `p2p/dnsdisc`, `p2p/enode`, `consensus/clique`, `core/rawdb`, `trie`, `core/state/snapshot`, `eth/protocols/snap`, `eth/tracers/...`, `accounts`, `core/vm` (incl. `Keccak256Cache` preimage test + precompile guard, minus the flaky). Pre-existing failures (all reproduced at pre-merge HEAD `0f76e0c30`, none batch-introduced): `core/state TestPDBMethodParity` (`DumpBinTrieLeaves`/#32445; milestone fix); `core/vm TestAbortDuringJump`/`TestInterruptDuringExecution` (5ms-sleep interrupt race, 4/5 pass); `cmd/evm TestT8n`/`TestEvmRun`/`TestEVMTracing`/`TestEvmRunRegEx` (t8n eip-validation/golden drift — `invalid eip number 1346` — confirmed identical failure at HEAD via throwaway worktree). No leftover conflict markers; `misc/` not staged.

## v1.17.0 batch 11/12 (`c50e5edfa`, plan row 12) — merged `2c0608fc6`

20 first-parent commits, 17 conflicts (15 `UU`, 2 `DU`). No new fork/EIP surface
(EIP-8024 touch test-only, #33787). Theme: EraE format + rlp iterators + HTTP/2
JSON-RPC + OpenTelemetry CLI wiring — the two large features both declined/deferred.

Adopted (correctness fixes):

- **trie/node.go (#33803)** — `decodeNodeUnsafe` now surfaces the `rlp.CountValues`
  error (`invalid node list`) instead of discarding it. Bor divergence was a
  cosmetic blank line; took upstream (lower `case c == 17:`/`default:` already
  merged to upstream's form). RLP robustness on trie decode.
- **core/rawdb/freezer_resettable.go (#33798)** — `cleanup()` moves the dir close
  to `defer dir.Close()` after `os.Open`, closing the fd on the `Readdirnames`
  error path. Bor divergence cosmetic; took upstream.
- **cmd/evm/internal/t8ntool/transaction.go + core/rawdb/accessors_indexes.go
  (#33820)** — relocated the RLP-iterator `Err()` check from inside the loop to
  after it (only meaningful once iteration terminates). Uses Bor's unchanged
  iterator API; `accessors_indexes.go` auto-merged the same relocation.
- **rlp/iterator_test.go (#33841)** — added the `txit.Count()==2` assertion for
  the re-added `Iterator.Count`; Bor divergence was a stray blank line.
- **eth/backend.go (#33810)** — nil-guard on `chainView` in `updateFilterMapsHeads`
  (filtermaps/log-index, non-consensus).

Adopted (auto-merged, no Bor divergence): ethclient/gethclient callTracer methods
(#31510, added the missing `context` import the auto-merge left out); node HTTP/2
JSON-RPC (#33812); triedb/pathdb nodeLoc-by-value (#33819, didn't touch Bor's
`AddressBiasedCache`/history-mutex); trie/bintrie storage-slot key mapping (#33807,
dormant); rlp `RawList` `AppendRaw`/validate+cache (#33834/#33840/#33818); influxdb
metrics error-message fix (#33804, kept — telemetry co-located in the same file
stripped); eth/tracers bad-block tracing tests (#33821); core/tracing gen stringers;
ethdb/pebble comment (#33805).

Declined — continuation of the batch-9 OTel deferral (`needs-wiring.md`):

- **OpenTelemetry JSON-RPC CLI wiring (#33484) + spanEnd-by-pointer fix (#33772)**
  — re-adds the `internal/telemetry` package (absent at HEAD since #33452 was
  deferred in batch 9), the `RPCTelemetry*` flags, `node.Config.OpenTelemetry`,
  `tracesetup.SetupTelemetry`, and threads the tracer through `rpc/handler.go`.
  Declined: reverted `rpc/handler.go`, `node/config.go`, `cmd/geth/main.go`,
  `cmd/utils/flags.go` to HEAD; removed `internal/telemetry/telemetry.go`,
  `internal/telemetry/tracesetup/setup.go`, `rpc/tracing_test.go`; stripped the
  telemetry block from `cmd/geth/config.go`. Bor's own pre-existing gRPC-server
  OTel (`internal/cli/server/server.go`) is a separate feature, untouched.

Deferred — class 3, entangled with Bor divergence (`needs-wiring.md`):

- **EraE archive-format rewrite (#32157/#33827)** — 15-file `internal/era`
  restructure (onedb/execdb split, `proof.go`, `SlimReceipt`, `--era.format` flag,
  `era.Era`/`Builder`/`ReadAtSeekCloser` interface). Blocked by two Bor divergences:
  upstream's `ExportHistory` assumes TD is no longer DB-stored (Bor still reads
  `bc.GetTd`), and Bor's `ImportHistory(chain, db, dir, network)` adds a `db` param
  + explicit `InsertHeaderChain` upstream lacks. Reverted the whole footprint to
  HEAD; `git rm` of new `onedb`/`execdb`/`proof.go`; `core/types/receipt.go`+test
  reverted (the orphaned `SlimReceipt`).

Verification (per-batch tier): `go build ./...` pass (rc=0); `gofmt` clean;
`go mod tidy` no changes (go.mod/go.sum revert to HEAD; declined features add no
deps). `go vet` clean except the pre-existing `//nolint:govet` copylocks
(`trie/secure_trie.go:88`, `core/parallel_state_processor.go:341`; both at HEAD).
`go test` pass: `trie`, `trie/bintrie`, `core/rawdb`, `rlp`, `triedb/pathdb`,
`ethclient/gethclient`, `node`, `core/types`, `core/tracing`, `ethdb/pebble`,
`core/` (183s), `tests/bor` (51s). Pre-existing failures (unchanged since HEAD
`f4f2c07fd`, none batch-introduced): `core/state TestPDBMethodParity`
(`DumpBinTrieLeaves`/#32445; milestone fix); `core/vm TestAbortDuringJump`/
`TestInterruptDuringExecution` (5ms interrupt race; `dispatch_test.go` untouched;
both pass on isolated re-run). No leftover conflict markers; docs + `misc/` not staged.

## v1.17.0 batch 12/12 (`0cf3d3ba4`, plan row 13) — merged `70994671a` — MILESTONE-CLOSING

7 first-parent commits, 28 conflicts (25 `UU`, 3 `DU`). The v1.17.0 release batch.
No new fork/EIP. Two large features declined/deferred; the only net code change is a
one-line CLI fix. The merge still advances the merge base past all 7 commits.

Adopted:

- **internal/download/download.go (#33842)** — `DownloadFile` wraps the writer in the
  progress-bar `downloadWriter` only when `resp.ContentLength > 0`. Clean auto-merge;
  isolated CLI tooling. The only net code change in this batch.
- **secp256k1/curve.go (`9b78f45e3`, v1.16.9 dup)** — Bor already carries the
  coordinate-check fix (applied in the v1.16.9 milestone `a06dbd7d2`); conflict was a
  comment-only divergence. Took ours.
- **consensus/misc/eip1559.go (#33860)** — the defensive parent-baseFee nil-check in
  `VerifyEIP1559Header` is already present in Bor HEAD (`eip1559.go:57`); merged as a
  no-op (converged). Consensus-security hardening, so verified present, not dropped.

Declined — OpenTelemetry newPayload tracing (#33521) + telemetry spans (#33780),
continuation of the batch-9 (#33452) / batch-11 (#33484) OTel line:

- #33521 threads `ctx context.Context` through the whole block-processing chain
  (`Processor.Process`, `BlockChain.{insertChain,ProcessBlock,insertSideChain}`,
  `ExecuteStateless`, all call sites) solely to carry `telemetry.StartSpan` server
  spans for `engine_newPayload`. Entangled with Bor's diverged signatures: Bor's
  `Process(block, statedb, cfg, author, interruptCtx)` (BlockSTM interrupt ctx + author),
  6-return `ProcessBlock(block, header, …)`, 5-return `ExecuteStateless(…, author,
  consensus, diskdb)`. Reverted to HEAD: `core/types.go`, `core/state_processor.go`
  (kept Bor's Prague `Bor == nil` gating vs upstream's `postExecution` wrap),
  `core/blockchain.go` (kept Bor's `reportBlock` naming), `core/stateless.go`,
  `eth/state_accessor.go`, `eth/api_debug.go`, `eth/catalyst/{api,simulated_beacon,
  api_test}.go`, `cmd/keeper/main.go`, `core/{blockchain,block_validator}_test.go`;
  removed `internal/telemetry/{telemetry,telemetry_test}.go`. `version/version.go`
  reverted to HEAD (Bor keeps its own version, not upstream's `Meta = "stable"`).

Deferred (class 3) — delayed p2p message decoding (#33835):

- 26-file eth/snap protocol refactor: `BlockBodiesResponse`/`GetTrieNodesPacket`
  fields → `rlp.RawList[...]` (decode-on-demand DoS hardening), `p2p/tracker` API →
  typed `Track(req Request) error`/`Fulfil(resp Response) error`, all response
  handlers reworked. Entangled with Bor's `ExcludeStateSyncReceipt()` receipt-interface
  divergence (state-sync consensus), Bor's `newBlockPushIntervalTimer`/
  `lastNewBlockPushUnix` NewBlock push metrics, the snap `requestTracker` singleton,
  Bor's forked downloader + wit protocol. Security-positive; good future-adopt. Reverted
  the full footprint to HEAD (`eth/protocols/eth/*`, `eth/protocols/snap/*` incl.
  restoring `snap/tracker.go` upstream deleted, `eth/downloader/{queue,queue_test,
  bor_fetchers_concurrent_bodies}.go`, `eth/handler_eth{,_test}.go`, `p2p/tracker/tracker.go`,
  `cmd/devp2p/internal/ethtest/*`); `git rm` of `eth/downloader/downloader_test.go` (DU),
  `eth/catalyst/witness.go` (DU), new `p2p/tracker/tracker_test.go`.

Verification (per-batch tier): `go build ./...` pass (rc=0); `gofmt` clean;
`go mod tidy` no changes (root go.mod/go.sum unchanged vs HEAD; declined features add
no deps). `go vet` clean except pre-existing `//nolint:govet` copylocks
(`trie/secure_trie.go:88`, `core/parallel_state_processor.go:341`). `go test` pass:
`eth/protocols/eth`, `eth/protocols/snap`, `eth/downloader` (+whitelist), `eth/catalyst`,
`internal/download`, `core/` (195s), `tests/bor`. Pre-existing failures unchanged since
HEAD `2c0608fc6` (only net code change is `internal/download`): `core/state
TestPDBMethodParity`, `core/vm TestAbortDuringJump`/`TestInterruptDuringExecution`
(5ms flake). No leftover conflict markers; docs + `misc/` not staged.
