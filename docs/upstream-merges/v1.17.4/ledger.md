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

- **`params.Rules.ChainID` retained** — upstream removed the `ChainID` field from `Rules` (struct-def deletion auto-merged; nothing in Bor reads `rules.ChainID`, build-confirmed). Restored it because Bor's `TestReinforceMultiClientPreCompilesTest` guard (`core/vm/contracts_test.go`) reflects over `Rules` fields and lists `ChainID` as expected — the guard exists specifically to force review of Bor↔Erigon precompile/Rules parity, so silently editing its expected list during a merge is exactly what it forbids. Restored the field + `chainID` local + `ChainID: new(big.Int).Set(chainID)` initializer; kept Bor's block-based `Rules()` (`_ uint64` param, single-arg `Is<Fork>(num)`, Bor fork bools, no `IsAmsterdam`). **Correction (v1.17.1 milestone 3): dropping `IsAmsterdam` here was a mistake** — every other upstream fork (Prague/Osaka/Verkle) is kept wired block-based-nil so it can be enabled by just setting the block. Amsterdam should have been converted the same way, not dropped. It was harmless in v1.17.0 (no code gated on it) but forced a strip-and-neutralize in v1.17.1 batches 14–15. Fixed by wiring `AmsterdamBlock`/`IsAmsterdam(num)`/`Rules.IsAmsterdam` block-based-nil (see the v1.17.1 batch 2/2 entry).
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

## v1.17.1 batch 1/2 (`9ecb6c4ae`, plan row 14) — merged `189030866` — first Amsterdam/EIP batch

Milestone v1.17.1 opened; branch `ppatil-upstream-v1.17.1` cut off `ppatil-upstream-v1.17.0`
(HEAD `451b3b445`). 20 first-parent commits `0cf3d3ba4..9ecb6c4ae`. 14 conflicted files
(10 content + go.yml DU + version.go); the rest auto-merged.

Fork/EIP surfaces (invariant 6 — all merged DORMANT, no activation moved on any Bor network):

- **Amsterdam precompile touch (#33742, `cbf3d8fed`, `core/vm`)** — upstream added a
  `stateDB.Exist(address)` "touch" in `RunPrecompiledContract` for EIP-7928 BAL recording,
  gated on a non-nil `stateDB` that upstream passes only when `rules.IsAmsterdam`. Adapted Bor's
  `runPrecompile`/`runEcrecoverWithCache` wrappers to the new
  `RunPrecompiledContract(stateDB, p, address, input, gas, logger)` signature. **Gated on
  `evm.chainRules.IsAmsterdam`** (see the batch-15 Amsterdam-wiring correction) — dormant on Bor
  (`AmsterdamBlock` nil → `IsAmsterdam` false → `stateDB` nil → no touch). Threaded the touch onto
  Bor's ecrecover-cache fast path too (dormant, faithful). Guard tests pass:
  `TestReinforceMultiClientPreCompilesTest`, `TestBorHardforkPrecompileContinuityProfiles`,
  `TestBorHardforkPrecompileContinuityAtNetworkBoundaries` — active precompile set unchanged.
- **Syscall value-transfer disable (#33741, `01fe1d716`, `core/vm/evm.go`)** — the
  `if !syscall { Transfer(...) }` guard auto-merged cleanly; Bor's `syscall := isSystemCall(caller)`
  already gates the balance check, so this is behavior-preserving for Bor's system-call
  (state-sync) path.
- **BAL code-change type → list (#33774, `199ac16e0`, `core/types/bal/*`)** — auto-merged
  type/encoding refactor only. BAL is not wired into Bor's `state_processor`/`blockchain`
  (dormant). `core/types/bal` tests pass.
- **core/state slot-read tracking for empty storage (#33743, `e636e4e3c`)** — auto-merged;
  adds a reader call on destructed-account committed-state reads for BAL readlist, but the
  value is discarded and `originStorage`/return stay empty → state root unchanged
  (behavior-preserving; committed-state reads are pre-block-immutable, no BlockSTM impact).
  `core/state` tests pass.
  - **Correction (review of #2325, 2026-09-08, raised by @lucca30):** "behavior-preserving" held for
    state but not for the **witness**, and the read arrived ungated while every other Amsterdam
    surface in this milestone is gated. `NewTrieOnly` forces reads through the MPT so the witness
    captures the nodes walked, so this read adds the destructed account's storage proof path to the
    witness. A node on this version reading against a witness from a producer without it gets a
    reader error → `setError` → `dbErr` → commit aborts (`statedb.go:2177`) → import fails. Reachable
    by create-and-selfdestruct in one tx then reading that address's storage later in the same block.
    That is the producer/consumer witness skew Hampi exists for, and an ungated fork surface also
    fails invariant 9 as `fork-register.md` defines `verified-dormant`. Gated on a new
    `StateDB.amsterdam`, set from `rules.IsAmsterdam` in `Prepare` and carried through `Copy`;
    `TestDestructedSlotReadIsAmsterdamGated` fails without the gate. Deliberate divergence from
    upstream, which does not gate it. **Caveat:** `Prepare` is per-transaction, so a path reaching
    `GetCommittedState` without preparing a transaction (system calls, state-sync in `Finalize`, the
    pooled `StateDB`s behind `SafeBase`) leaves the flag false and skips the read — correct while
    Amsterdam is dormant, but it must be revisited when the fork is scheduled, or BAL will
    under-record on those paths.
  - **Measured, not argued (2026-09-08):** `TestDestructedReadWitnessSkew` (`core/state`)
    drives one destructed-account read against a committed storage trie and harvests the
    witness both ways: **1 node with the gate on, 5 with it off — 4 extra nodes the reading
    path demands, a clean superset.** It fails if the gate is removed, so it pins the
    direction rather than restating it.
  - **Subtlety worth keeping, because it nearly caused a wrong retraction:** #2180's comment
    in `stateObject.updateTrie` says reader reads go through "a separate trie with its own
    PrevalueTracer" and their nodes "are NOT in obj.trie". That is true, and it reads exactly
    like "reader reads never reach the witness" — which is false. They reach it by a
    different route: `trieReader.CollectStateWitness` harvests the reader's `mainTrie` and
    per-address sub-tries, and `StateDB.CollectStateWitness()` is called from
    `core/parallel_state_processor.go:1192` on the V2 BlockSTM path. "Not in `obj.trie`" and
    "not in the witness" are different claims; only the first holds.

Notable resolutions:

- **version/version.go** — declined upstream's `Patch = 1` bump (`105427690` begin v1.17.1
  cycle); kept Bor's `Patch = 0` / `Meta = "unstable"` (Bor doesn't track upstream version
  numbers). Net-zero vs HEAD.
- **.github/workflows/go.yml (DU)** — deleted-by-us; Bor removed this workflow (`#1723`).
  `git rm` to keep the deletion (upstream's #33866 tweak declined).
- **v5wire (#31547, `p2p/discover/v5wire/encoding.go`, `cmd/devp2p/internal/v5test/discv5tests.go`)**
  — Bor had no real divergence (stray blank lines only); converged to upstream (`createMask(iv)`
  + `applyMasking`; `var challenge1`). v5wire tests pass.
- **metrics influxdb interval (#33767, `cmd/geth/config.go`)** — adopted; supporting
  `MetricsInfluxDBIntervalFlag`/`InfluxDBInterval` auto-merged.
- **eth_getStorageValues test (#32591, `internal/ethapi/api_test.go`)** — additive-both-sides
  (Bor `TestCoinbase`/AccountAt vs upstream `TestGetStorageValues`); combined both, closing
  Bor's last func and keeping the shared trailing brace for upstream's. Both tests pass.

Deferred (recorded in needs-wiring.md):

- **On-chain tx check in fetcher (#33607, `59ad40e56`)** — builds on the declined #33378
  (`validateMeta`/`txMetadataWithSeq`); Bor keeps `hasTx func(common.Hash) bool`. Sole
  batch-14 commit in its 5 files → restored `eth/fetcher/{tx_fetcher,tx_fetcher_test,metrics}.go`,
  `eth/handler.go`, `tests/fuzzers/txfetcher/txfetcher_fuzzer.go` to HEAD. No dangling refs.
- **testing_buildBlockV1 (#33656, `e40aa46e8`)** — test-only catalyst Engine API entangled
  with Bor's VEBLOP `getWork` worker restructure. Sole batch-14 commit in its files →
  reverted 8 modified files to HEAD, `git rm` 2 new files. No dangling refs.

Verification (per-batch tier): `go build ./...` pass (rc=0); `go vet` clean on touched
packages (`core/vm`, `core/state`, `core/types/bal`, `p2p/discover/v5wire`, `cmd/geth`,
`internal/ethapi`, `miner`, `eth/fetcher`) except the pre-existing `//nolint:govet` copylocks.
`go test` pass: `core/vm` (guard tests green; only `TestInterruptDuringExecution` 5ms flake,
passes on isolated re-run), `core/state`, `core/types/bal`, `p2p/discover/v5wire`, `eth/fetcher`
(137s), `internal/ethapi` (`TestCoinbase` + `TestGetStorageValues` both pass). No leftover
conflict markers. 46 files net-changed vs HEAD.

## v1.17.1 batch 2/2 (`16783c167`, plan row 15) — merged `b6175113d` — MILESTONE-CLOSING — first Amsterdam/EIP batch (2)

20 first-parent commits `9ecb6c4ae..16783c167` (ends at the v1.17.1 release tag). 30 conflicted files.
55 files net-changed vs HEAD.

Fork/EIP surfaces (invariant 6 — all merged DORMANT):

- **Amsterdam fork gate WIRED block-based-nil (correction — see note below)** — `params/config.go` gains
  `AmsterdamBlock *big.Int` (nil on every preset), `IsAmsterdam(num) = IsLondon(num) && isBlockForked(AmsterdamBlock, num)`,
  `Rules.IsAmsterdam` + `Rules()` population, mirroring Osaka/Prague/Verkle. `IsAmsterdam` added to
  `TestReinforceMultiClientPreCompilesTest`'s expected Rules-field list. This corrects the v1.17.0 drop of
  upstream's `AmsterdamTime`/`IsAmsterdam` — enabling Amsterdam later is now "set `AmsterdamBlock`", like the
  other dormant forks.
  - **Correction (review of #2325, 2026-09-07):** the block-based conversion carried the field, `IsAmsterdam`
    and `Rules` but dropped three surfaces upstream wires for `AmsterdamTime` — `CheckConfigForkOrder`'s
    ordered list (upstream `params/config.go:954`), `checkCompatible`'s `isForkTimestampIncompatible` call
    (upstream `:1125`) and the startup banner (upstream `:690`). Left as-is, a genesis could schedule
    `amsterdamBlock` before `osakaBlock` and pass validation, and moving it under an already-synced head
    would produce no `ConfigCompatError` and no rewind. Latent while nil on every preset, live the moment any
    config sets it. Added all three plus `TestAmsterdamForkGuards`, which fails on each surface without the
    fix. `ChainConfig.Block(forks.Amsterdam)` was examined and deliberately left alone — the method has no
    callers in the tree.
- **EIP-7843 SLOTNUM (#33589, `f811bfe4f`)** — opcode `SLOTNUM (0x4b)` + `opSlotNum` + `enable7843` wired into
  `newAmsterdamInstructionSet`, instantiated (`amsterdamInstructionSet` var in `jump_table.go`) and dispatched
  (`case evm.chainRules.IsAmsterdam` in `core/vm/evm.go`, above `IsOsaka`). Header field `SlotNumber *uint64
  \`rlp:"optional"\`` populated under the `IsAmsterdam(num)` gate in `core/genesis.go` + miner `makeHeader`
  (`miner/worker.go`, with a `slotNum *uint64` field on Bor's `generateParams`), validated under the gate in
  `consensus/beacon/consensus.go`, and read into `BlockContext.SlotNum` in `core/evm.go`. All dormant
  (`IsAmsterdam` false → `amsterdamInstructionSet` never selected, `SlotNumber` never set → nil → RLP unchanged,
  core/types tests pass). **Enabling SLOTNUM additionally needs Bor's producer to supply `genParams.slotNum`
  (Bor has no beacon slot) — the miner errors otherwise.**
- **EIP-8024 in Amsterdam (#33928, `2726c9ef9`)** — `enable8024` in `newAmsterdamInstructionSet`, reachable only
  via the (dormant) Amsterdam instruction set.
- **bintrie endianness (#33900, `95c6b0580`)** — 2-line fix; binary-tree is verkle-gated, dormant on Bor.
- Guards pass: `TestReinforceMultiClientPreCompilesTest`, both `TestBorHardforkPrecompileContinuity*`.
- Restore-when-Amsterdam-lands tracked in `fork-register.md`.

Adopted:

- **c-kzg `v2.1.5→v2.1.6` (#33901)** — auto-merged; `go mod tidy` reconciled go.sum. Keeps blob-KZG parity
  with upstream. keeper module kept at Bor HEAD.
- **olekukonko/tablewriter → internal/tablewriter (#28892)** — adopted upstream's migration; removed the
  duplicate import merge artifact in `core/rawdb/database.go`, fixed `MakeChainDatabase` 3→4 args (Bor's
  `disableFreeze`) in `cmd/geth/dbcmd.go`. olekukonko dropped from go.mod.
- **p2p/tracker crash fix (#33940, `9962e2c9f`)** — adopted (`if t.expire == nil { return }` + schedule
  refactor); applies to Bor's tracker (pre-#33835 API kept). `git rm` DU tracker_test.go.
- **trie/proof error propagation (#33898, `be92f5487`)** — adopted (`if err := tr.Update(...); err != nil`).
- **blobpool low-fee-delay test (#33893)** — combined Bor's `testChainConfig` + upstream's low-fee (1/1) values.
- **miner payload-rebuild timer (#b25080cac)** — adopted with Bor's `w.config` naming
  (`timer.Reset(max(0, w.config.Recommit-time.Since(start)))`).
- **metrics influxdb interval (#33767)** — adopted (already staged in batch prep).

Declined / kept-Bor:

- version.go (declined `Patch=1` bump), AGENTS.md (kept Bor's), Dockerfiles (Bor already Go 1.26.4).
- **eth/68 drop (#33511, `723aae2b4`)** — deferred; full 16-file footprint reverted to HEAD (+`git rm` DU
  `eth/downloader/downloader_test.go`). Piggybacked #2a4527240 (handshake metrics) + #cee751a1e (sync test)
  reverted with it. See needs-wiring.md.

Verification (per-batch tier): `go build ./...` rc=0; `go vet` clean on touched packages (bar pre-existing
copylocks); `go mod tidy` clean (go.mod diff = c-kzg bump + olekukonko removal). `go test` pass: `core/types`,
`core/vm` (guards), `core/` (182s), `miner` (196s), `eth/protocols/eth`, `trie`, `core/txpool/blobpool`,
`consensus/beacon`. No leftover conflict markers.

## v1.17.1 MILESTONE-TIER VERIFICATION (2026-09-08)

Run against `ppatil-upstream-v1.17.1` at `4570596178` plus the uncommitted N-1/N/N+1
boundary test. First milestone-tier run recorded in this ledger; every prior entry is
per-batch tier only.

**Use the repo's own contract, not `go test ./...`.** `make test` is:

```
GODEBUG=cgocheck=0 go test -p 1 --timeout 30m -cover -short $(go list ./... | grep -v go-ethereum/cmd/)
```

Three differences from a naive `go test ./...`, each of which manufactures a false failure:

- **`cmd/` is excluded** from `TESTALL`. Running it surfaces `cmd/evm`, `cmd/geth` and
  `cmd/devp2p/internal/ethtest` failures that are not part of the suite. The `cmd/evm`
  t8n entries on this ledger's known-red list came from a run that was outside the
  contract in the same way.
- **`--timeout 30m`**, not the 10m default. `core` takes 604s under parallel package
  execution and is killed at 600s; the failure presents as a `subfetcher` goroutine dump
  that reads like a prefetcher deadlock and is not one.
- **`-p 1`** (serial packages). With it, `core` finishes in 175s — the 604s figure was
  CPU contention, not a slow test.

Result: **144 packages pass, 0 failures, no panics, exit 0.** Slowest: `miner` 220s,
`core` 175s, `rlp/rlpgen` 141s, `eth/fetcher` 137s.

Adjacent finding (pre-existing, not introduced by this stack): `miner/worker.go:913`
calls `w.chainConfig.Bor.CalculatePeriod(...)` with no nil check on `w.chainConfig.Bor`,
so `newWorkLoop` segfaults on a chain config with no Bor section. Identical on `develop`,
on the base branch and here (3 occurrences each). Unreachable in Bor production, where
`Bor` is always set; it only shows up in the excluded `cmd/` tests, which spin standard
Ethereum configs. Left alone — out of scope for a sync PR.

### Known-red list, corrected (2026-09-08)

Every batch entry above carries a "pre-existing failures" line. Re-checked at
`6b4820b5e` against the actual test contract; none of the three recurring
entries is currently a known-red.

- **`core/state TestPDBMethodParity` — RESOLVED, remove.** Passes. The standing
  TODO ("milestone fix = add `DumpBinTrieLeaves` to `pdbExemptMethods`") is
  stale; the `#32445` drift no longer reproduces. It was already observed
  passing during the Hampi/#2319 cascade and has stayed green since.
- **`core/vm TestAbortDuringJump` / `TestInterruptDuringExecution` — not
  currently failing.** They are *not* skipped by `-short` (0 skips), and ran
  20/20 green over 10 iterations. The flakiness recorded above predates the
  cascade that brought in `2ea9635db` ("core, miner: fix pipeline race test
  failures", #2371), which is a plausible cause but has not been confirmed.
  Worth leaving on a watch list rather than a known-red list.
- **`cmd/evm TestT8n` / `TestEvmRun` / `TestEVMTracing` / `TestEvmRunRegEx` —
  category error, remove.** `cmd/` is excluded from `TESTALL`, so these were
  never part of the suite. They were recorded as accepted breakage from runs
  made outside the contract. The t8n golden drift is real if you run those
  packages directly; it simply is not something this sync gates on, and listing
  it as known-red trains reviewers to expect red where the suite is in fact
  green.

The milestone-tier run is **144 packages, 0 failures**. Treat any red as new
until shown otherwise.
## v1.17.2 batch 1/4 (`00540f946`, plan row 16) — merged `09c784851` — MILESTONE-OPENING

Branch `ppatil-upstream-v1.17.2` cut from `ppatil-upstream-v1.17.1` @ `dbae0f4a1` (stacked).
20 first-parent commits, 26 conflicted files (24 content + 2 delete/modify), 65 files in the merge
(+441 / −297).

Adopted:

- **EIP-7778 block gas accounting without refunds (#33593, `6d0dd0886`)** — the batch's substantive
  change. `core.GasPool` goes from `type GasPool uint64` (+`AddGas`/`SetGas`) to a struct
  (`remaining`/`initial`/`cumulativeUsed`) with
  `NewGasPool`/`ReturnGas`/`Used`/`CumulativeUsed`/`Snapshot`/`Set`; the `usedGas *uint64`
  out-parameter is dropped from `ApplyTransaction`, `ApplyTransactionWithEVM` and `MakeReceipt`
  (receipts read `gp.CumulativeUsed()`, headers read `gp.Used()`). Took `core/gaspool.go` from
  upstream verbatim and ported every Bor call site: `core/state_processor.go` (kept Bor's
  `interruptCtx` guard and state-sync/Finalize block), `core/parallel_state_processor.go` (3
  BlockSTM per-task pools — V1/V2 keep their own `totalUsedGas`, so parallel accounting is
  untouched), `miner/worker.go` (`gasPool.Set(gp)` snapshot-restore;
  `env.header.GasUsed = env.gasPool.Used()` replaces the out-parameter),
  `cmd/evm/internal/t8ntool`, `core/state_prefetcher.go`, `eth/{state_accessor,tracers}`,
  `internal/ethapi`, `accounts/abi/bind/backends`, `tests/bor/helper.go` + 8 test files.
  **Pre-Amsterdam equivalence:** old `SubGas(limit)` + `AddGas(remaining)` and new
  `SubGas(limit)` + `ReturnGas(remaining, gasUsed())` have identical net pool delta
  (`limit − remaining == gasUsed()`), so `gp.Used()` == the old `*usedGas` and
  `gp.CumulativeUsed()` == the old receipt `CumulativeGasUsed`. The refund-excluding branch is
  gated on the dormant `rules.IsAmsterdam`.
- **Amsterdam jump table in lookup (#33947, `fe3a74e61`)** — `LookupInstructionSet` dispatches
  `case rules.IsAmsterdam → newAmsterdamInstructionSet()`, placed below Bor's
  Chicago/LisovoPro/Lisovo/MadhugiriPro/Madhugiri cases and above Osaka to match the
  `core/vm/evm.go` order wired in batch 15. Dormant (gate nil).
- **EIP-8024 branchless normalization + extended EXCHANGE (#33869, `814edc530`)** — auto-merged;
  reachable only via `enable8024` in `newAmsterdamInstructionSet` → dormant.
- **batch close (#33708, `dd202d428`)** — `defer blockBatch.Close()` added to Bor's diverged
  `writeBlockWithState` (which carries the state-sync-log and witness-write blocks); the
  `writeHeadBlock` / `reorg` call sites auto-merged.
- **TestProcessVerkle flaky fix (#33971, `ecee64ecd`)** — adopted upstream's `genesisTriedb`
  explicit-close plus reuse of the committed block in `GenerateChainWithGenesis`; kept Bor's 2-arg
  `genesis.Commit(db, triedb)`.
- **default cache 4096 (#33836, `28dad943f`)** — adopted: `utils.CacheFlag` default 1024→4096 and
  the mainnet-only bump block deleted from `cmd/geth/main.go`. Affects only the geth-style binary;
  the production `bor server` path keeps its own `CacheConfig.Cache = 1024`.
- **eth_simulateV1 gas cap / MaxUsedGas (#33952, #32789)** — combined Bor's `gasBudget`, `gasCapped`
  return and bor-internal-call gas-cap bypass with upstream's `gp`-based accounting
  (`header.GasUsed = gp.Used()`, `gp.CumulativeUsed()` into `MakeReceipt`).
- **go-eth-kzg `v1.4.0→v1.5.0` (#33963)** — adopted in the root module; `go mod tidy` reconciled
  go.sum. keeper module kept at Bor HEAD.

Declined / kept-Bor:

- **prevent state flushing in RPC (#33931, `6d99759f0`)** — **whole feature deferred.** Introduces
  `core.ExecuteConfig` threaded through `BlockChain.ProcessBlock`, moves
  `StatelessSelfValidation` / `EnableWitnessStats` from `vm.Config` to `BlockChainConfig`, and
  rewrites `debug_executionWitness` to run with `WriteState: false`. Bor has forked every target:
  the live `ProcessBlock` is the tuple-returning `(block, parent, witness, followupInterrupt)` form
  at `core/blockchain.go:857` (upstream's survives only as dead `processBlock`,
  `// nolint : unused`), `insertChain` is restructured, and `eth/api_debug.go` has its own
  `ExecutionWitness` / `ExecutionWitnessByHash` built on the Bor signature — which returns a statedb
  without persisting, so Bor does not have the bug being fixed. Reverted `core/vm/interpreter.go`,
  `eth/api_debug.go`, `eth/backend.go`, `internal/web3ext/web3ext.go`, `tests/block_test_util.go`
  wholesale, and the #33931 hunks only in `cmd/utils/flags.go` and `core/blockchain.go` (shared with
  #33836 / #33708). See needs-wiring.md.
- **miner/stress/main.go (new file, 210 lines, added by #33593)** — Engine-API-driven block-builder
  stress harness; needs `AmsterdamTime` (Bor's gate is block-based), `BlobScheduleConfig.Amsterdam`
  and `ethconfig.SlowBlockThreshold` (slow-block deferred in v1.17.0). Removed; see needs-wiring.md.
- **eth/fetcher chain-event nil-guard (#33950, `344ce84a4`)** — the subscription it guards doesn't
  exist in Bor (it came with the declined #33378). Kept Bor's.
- **otel `1.39→1.40` (#33946) and `golang.org/x/sys 0.39→0.40`** — declined; Bor is already ahead
  (otel 1.43.0 / sdk 1.43.0, x/sys 0.45.0), so adopting would be a downgrade.
- version.go — declined the `Patch=2` bump; kept Bor `Patch=0` / `unstable`.
- DU (deleted-at-HEAD / modified-upstream), removed to keep the deletions:
  `eth/tracers/internal/tracetest/selfdestruct_state_test.go` (deferred selfdestruct cluster) and
  `internal/telemetry/tracesetup/setup.go` (deferred OTel).

Merge artifacts fixed — upstream hunks that auto-merged into the wrong place in Bor's restructured
files, all caught by build/vet:

- `miner/worker.go` — #33945's `func (env *environment) discard()` landed as a duplicate of Bor's
  own nil-guarded `discard` (redeclared), and its `defer work.discard()` landed inside `newWorker`
  where `work` is undefined. Bor's `generateWork` already has `defer work.discard()` and already
  starts the prefetcher unconditionally, so **#33945 is converged in Bor**; both artifacts removed.
- `internal/ethapi/simulate.go` — `var withdrawalsHash *common.Hash` was dropped (upstream moved the
  declaration a few lines down); restored at Bor's position inside the loop.
- `eth/tracers/api.go` — `traceTx` returned the removed `usedGas`; now holds the pool
  (`gp := core.NewGasPool(message.GasLimit)`) and returns `gp.Used()` (same value).
- `core/state_processor.go` — dangling `spanEnd(&err)` left by upstream's telemetry move landing in
  Bor's de-telemetried loop.

Verification (per-batch tier): `go build ./...` rc=0; `go vet ./...` clean bar the two pre-existing
`//nolint` copylocks (`trie/secure_trie.go:88`, `core/parallel_state_processor.go:341`); `gofmt -l`
clean on all 65 changed files; `go mod tidy` clean (go.mod diff = go-eth-kzg bump only). `go test`
pass: `core/` (183s), `miner/...` (197s), `core/vm/...`, `core/types/...`, `core/state/...`,
`internal/ethapi/...`, `eth/tracers/...`, `eth/fetcher` (139s), `eth/`, `consensus/...`, `trie/...`,
`triedb/...`, `ethdb/...`, `core/rawdb/...`. Fork guards green
(`TestReinforceMultiClientPreCompilesTest`, `TestBorHardforkPrecompileContinuity*`,
`TestV2ForkParity`). `cmd/evm` 4 failures (`TestT8n`, `TestEVMTracing`, `TestEvmRun`,
`TestEvmRunRegEx`) proven pre-existing — the identical set fails on a clean worktree at pre-merge
`dbae0f4a1`. No leftover conflict markers.

## v1.17.2 batch 2/4 (`77e7e5ad1`, plan row 17) — merged `0a83ed542`

20 first-parent commits, 23 conflicted files (21 content + 2 delete/modify), 45 files in the merge
(+920 / −177).

Adopted:

- **EIP-7954 increase maximum contract size (#33832, `95b9a2ed7`)** — adds
  `params.MaxCodeSizeAmsterdam` (32768) / `MaxInitCodeSizeAmsterdam` (65536) and two helpers in
  `core/vm/common.go`: `CheckMaxCodeSize(rules, size)` (`IsAmsterdam` → 32768, else `IsEIP158` →
  24576) and `CheckMaxInitCodeSize(rules, size)` (`IsAmsterdam` → 65536, else `IsShanghai` →
  49152). Helpers taken byte-identical. The initcode call sites (`core/state_transition.go`,
  `core/txpool/validation.go`, `core/vm/gas_table.go` ×2) auto-merged and are behavior-preserving
  pre-Amsterdam; `gasCreate*Eip3860` only runs in Shanghai+ jump tables.
  **The code-size call site needed hand-resolution: Bor already raises the same cap under its own
  fork.** `MaxCodeSizePostAhmedabad = 32768` is gated on `Bor.IsAhmedabad(blockNumber)` (mainnet
  `62278656`, Amoy `11865856` — live today), which is a `BorConfig` block gate rather than a
  `params.Rules` field, so upstream's rules-only helper cannot express it; and Bor's
  `initNewContract` **assigns `err` and continues** (still charging deployment gas and calling
  `SetCode`) where upstream returns immediately. Resolved as: Ahmedabad branch first (Bor's inline
  check, unchanged), otherwise upstream's helper with its dormant Amsterdam branch. Both caps are
  32768 so they agree on code size; the initcode limits would differ (49152 → 65536) if Amsterdam
  were ever enabled — recorded in fork-register.
- **trienode history alongside existing data (#33934, `7d13acd03`)** — freezer/pathdb changes
  adopted, including the `Freezer.frozen` → `Freezer.head` rename, the early-return `doSync`
  (Bor's `trackError` variant differed only in wsl blank-line style and error ordering), and the
  tail-over-head truncation reset + its new test case.
- **history pruning cutoff in GetFilterLogs (#33823, `189f9d0b1`)** — added the
  `HistoryPruningCutoff` / `PrunedHistoryError` check to `GetFilterLogs`, ordered before Bor's
  `checkBlockRangeLimit`, matching how `GetLogs` already sequences the two.
- **accessList StorageKeys never null (#33976, `f6068e3fb`)** — adopted upstream's
  `slices.SortedFunc` + nil-guard. Bor's unsorted variant dates from the original #22550/#23225
  geth commits plus a Bor wsl-lint pass, not a deliberate divergence, so this is convergence.
  `eth_createAccessList` output is now deterministically ordered.
- **fetchpayload utility (#33919, `59512b184`)** — adopted the `fromExtWitness` →
  `FromExtWitness` export (keeping Bor's `w.context = ext.Context` line) plus the new
  `cmd/fetchpayload`. It is a plain `ethclient`/`rpc` client with no Engine-API dependency and
  feeds Bor's existing `cmd/keeper`.
- **Prague pruning points (#33657, `3c20e08cb`)** — adopted in `core/blockchain.go`
  (`initializeHistoryPruning` now consults `history.MergePrunePoints` and `PraguePrunePoints`).
- **karalabe/hid bump (#34008)** — FreeBSD ports build fix. Plus #34006's `-signify` flag-name fix
  in `build/ci.go`.

Declined / kept-Bor:

- **codedb + simplify cachingDB (#33816, `91cec92bf`)** — **whole feature deferred.** A 20-file,
  526-insertion rewrite of the state database layer: new `core/state/database_code.go` (`CodeDB`)
  and `core/state/reader_stater.go` (`ReaderStater`), `BlockChain.statedb *state.CachingDB`
  replaced by `codedb *state.CodeDB` with the `state.Database` constructed per call
  (`state.NewDatabase(bc.triedb, bc.codedb).WithSnapshot(bc.snaps)`), and — the blocker — the
  `ContractCodeReader` interface changes shape (`Code`/`CodeSize` drop their `error` return, `Has`
  added). That interface is where Bor's pipelined-SRC instrumentation lives
  (`ContractCodeReaderStats`, `ContractCodeReaderWithStats`, `ReaderStats`, `GetStats()`,
  `ReadersWithCacheStats()`), consumed by `blockProcessingResult.stats`; upstream replaces the
  concrete-typed `GetStats()` with a `state.ReaderStater` type assertion. Adapting means rewriting
  Bor's cache-stats reader layer and its parallel/BlockSTM `StateDB` interactions — authoring new
  Bor code on a state-root-determinism-critical path. Direct successor to the batch-6 state
  code-read-metrics deferral. Reverted `core/blockchain_reader.go`,
  `core/blockchain_sethead_test.go`, `core/blockchain_test.go`,
  `core/state/{database,database_history,iterator,reader,state_object,statedb,statedb_fuzz_test,statedb_test,stateupdate,sync_test}.go`,
  `miner/miner_test.go`, `tests/state_test_util.go`, `triedb/hashdb/database.go` to HEAD; `git rm`
  of the two new files and of `core/blockchain_stats.go` (DU, already deleted at HEAD). In
  `core/blockchain.go` (shared with #33657) only the #33816 hunks were reverted — **including two
  that auto-merged silently**: the `statedb *state.CachingDB` field replaced by
  `codedb *state.CodeDB`, and the removal of the snapshot re-init
  `bc.statedb = state.NewDatabase(bc.triedb, bc.snaps)` in `setupSnapshot`. See needs-wiring.md.
- **telemetry span for ApplyTransactionWithEVM errors (#33955, `32f05d68a`)** — Bor's tx loop has
  no telemetry (OTel deferred since v1.17.0). Both hunks dropped; the error-path `spanEnd(&err)`
  had auto-merged into the de-telemetried loop and was removed.
- **remove stale-pivot detection in processSnapSyncContent (#33150, `27c4ca9df`)** — upstream
  deletes the block as redundant for its beacon-header-driven path. Bor's forked downloader carries
  its own variant (`eth/downloader/bor_downloader.go:2048`, with Bor's `newPivotNum`) and Bor's
  snap sync is not beacon-driven, so the rationale doesn't transfer. Kept Bor's deletion of
  `eth/downloader/downloader.go` (DU); `bor_downloader.go` untouched. The companion
  `eth/catalyst/api.go` hunk auto-merged (catalyst unused in Bor).

Adaptations to Bor divergences (all found by build/vet):

- `core/rawdb/{freezer,freezer_resettable}.go` — Bor's offset-aware helpers
  (`ItemAmountInAncient`, the `freezer.frozen.Add(offset)` in `NewFreezer`, the open-log) still
  used the pre-#33934 field name; renamed to `head`, semantics unchanged.
- `core/rawdb/freezer_table_test.go` — #33934's two new call sites use upstream's `newBatch()`;
  Bor's signature is `newBatch(offset uint64)`, so they take `0` like every other Bor call site.
- `core/vm/gas_table_test.go` — restored the `chainConfig := params.AllEthashProtocolChanges`
  declaration dropped during conflict resolution while the rest of #33832's hunk auto-merged.
- `eth/tracers/logger/access_list_tracer.go` — added the `slices` import for the adopted sort.

Verification (per-batch tier): `go build ./...` rc=0; `go vet ./...` clean bar the two pre-existing
`//nolint` copylocks; `go mod tidy` clean (go.mod diff vs batch 16 = karalabe/hid bump only);
`gofmt -l` clean on every changed file except `build/ci.go`, whose misalignment is pre-existing
(HEAD's copy fails the same check; the merge only renamed the `-signify` flag). `go test` pass:
`core/` (184s), `miner/...` (196s), `eth/` (48s), `core/rawdb/...` (62s), `core/vm/...`,
`core/state/...`, `core/stateless/...`, `core/txpool/...`, `eth/filters/...`, `eth/tracers/...`,
`triedb/...` (incl. `pathdb` 57s). Fork guards green (`TestReinforceMultiClientPreCompilesTest`,
`TestBorHardforkPrecompileContinuity*`, `TestV2ForkParity`, `TestCreateGas`). No leftover conflict
markers.

## v1.17.2 batch 3/4 (`e23b0cbc2`, plan row 18) — merged `1abb57b8f`

20 first-parent commits, 32 conflicted files (30 content + 2 delete/modify), 30 files in the merge
(+1177 / −259). Decline-heavy: three whole-feature declines cover 27 of the 32 conflicts, and
nothing consensus-affecting was adopted.

Adopted:

- **txLookupLock leak fix (#34039, `b6115e9a3`)** — `defer bc.txLookupLock.Unlock()` in `reorg()`
  so the early error returns can't leave the mutex held.
- **single storage-trie traversal (#34051, `a3083ff5d`)** — took upstream's `cmd/geth/snapshot.go`
  rewrite (`traverseStorage` helper + `--account` flag) wholesale; Bor's only divergences there
  were the 4-arg `MakeChainDatabase(ctx, stack, X, false)` (`disableFreeze`, 8 call sites) and
  `_, _ =` lint discards on the hasher, both re-applied.
- **binary-trie IntermediateRoot bypass (#34022, `77779d109`)** — adopted the
  `&& !s.db.TrieDB().IsVerkle()` prefetcher condition, kept Bor's `skipTimers` guard. Verkle is
  dormant on Bor so the branch is never taken.
- **alloc-free flatReader hashing (#34025, `4b915af2c`)** — `addr[:]` / `key[:]` instead of
  `.Bytes()`, applied inside Bor's `addrCache` (pipelined-SRC) rather than replacing it.
- **history index initer (#33640, `9b2ce121d`)** — `triedb/pathdb` `NoHistoryIndexDelay` combined
  with Bor's `MaxDiffLayers`.
- **rangeLogs invalid-range error (#33763, `3341d8ace`)** — `filter.go` fix auto-merged
  (`firstBlock > lastBlock` now returns `errInvalidBlockRange` instead of `nil, nil`); adopted the
  matching test expectation while keeping Bor's 4-arg `NewRangeFilter` (Bor carries the range limit
  on `api.sys.cfg`, not per-filter).

Declined / kept-Bor:

- **miner OpenTelemetry spans (#33773, `98b13f342`)** — **whole feature deferred**, the batch's
  largest cluster (23 files, 17 conflicting). Threads `ctx context.Context` through
  `consensus.Engine`, the txpool `SubPool` interface, `miner.{BuildPayload,generateWork}`,
  `eth/handler`, `eth/sync`, `eth/api_backend` and the whole catalyst surface, purely to attach OTel
  spans to block building. Identical class to the batch-12 #33521 decline: Bor's OTel stack was
  deferred in v1.17.0 (#33452/#33484) so there is nothing for the spans to attach to, and the
  interface changes collide with Bor's VEBLOP miner restructure (`getWorkCh`/async work path) and
  its 4-return `FinalizeAndAssemble`. Reverted all 23 files to HEAD; `git rm` for
  `eth/catalyst/witness.go` and `miner/payload_building_test.go` (DU, already deleted at HEAD).
- **call-variant gas measurement rework (#33648, `fd859638b`)** — **whole feature deferred.** Splits
  each call variant's inner gas calculation into stateless (memory expansion + value transfer +
  EIP-2929) and stateful (`Empty`/`Exist` probe, EIP-7702 delegation resolution) halves, with an
  early `if contract.Gas < intrinsic { return ErrOutOfGas }` between them so a call that cannot pay
  never touches state — preparation for EIP-7928 block access lists. The change is gas-equivalent
  (same components, same totals; both orderings end in error-plus-all-gas-consumed when the caller
  is short) but it **reorders state reads** and is **not fork-gated**, so on Bor it would shift
  witness contents and the BlockSTM MVHashMap read set in out-of-gas cases. Deterministic, hence not
  a split risk, but it is an unconditional change to the consensus-critical EVM gas path whose only
  beneficiary is dormant. Decisive factor: it lands in `core/vm/{gas_table,operations_acl}.go`, the
  exact pair where Bor has already deferred #33281 (write-protection relocated into the gas
  handlers), #33637 (per-opcode read-only checks) and #33450 (selfdestruct cold-access early-return),
  all blocked behind the #32919 selfdestruct rework — Bor's `gasCall` accordingly has no `readOnly`
  check and its `gasCallEIP7702` is a plain alias where upstream now has a BAL-motivated wrapper.
  Reverted `core/vm/{gas,gas_table,operations_acl}.go` to HEAD. See needs-wiring.md.
- **history pruning configuration refactor (#34036, `6ae3f9fa5`)** — **whole feature deferred.**
  Introduces `history.HistoryPolicy` (user intent) beside the persisted prune point, replaces
  `BlockChainConfig.ChainHistoryMode` with `HistoryPolicy`, and rewrites `initializeHistoryPruning`;
  `eth/backend.go` builds it via `history.NewPolicy(config.HistoryMode, genesisHash)`. Bor's
  `eth/backend.go` **never calls `core.LoadChainConfig`** — it uses `config.Genesis.Config` and a
  Bor-specific `CreateConsensusEngine(config.Genesis.Config, config, chainDb, blockChainAPI, vmCfg)`
  — so no `genesisHash` is in scope. Adopting means new Bor plumbing for an operational feature Bor
  doesn't use: no Polygon genesis hash appears in `MergePrunePoints`/`PraguePrunePoints`, so Bor
  nodes are always `KeepAll`. Reverted `cmd/geth/chaincmd.go`, `cmd/workload/testsuite.go`,
  `core/blockchain_test.go`, `core/history/historymode.go`, `eth/backend.go`; `git rm` of the new
  `core/history/historymode_test.go`.
- **stateless code-database initialization fix (#34011, `a7d09cc14`)** — one-line fix binding
  `state.NewCodeDB(memdb)` in `core/stateless.go`; `CodeDB` arrives with #33816, deferred in batch
  17. Kept Bor's form (which also passes `diskdb` to `MakeHashDB`, its own divergence).
- **#33150 / #33955 companions** — no action needed this batch.

Merge artifact fixed:

- `core/blockchain.go` — upstream's #34036 rewrite of `initializeHistoryPruning` had auto-merged in
  fragments, so resolving only the marked conflict hunks left the function syntactically broken
  (orphaned `case` arms after the `switch` was replaced). Restored the file wholesale to HEAD and
  re-applied only #34039's two-line `txLookupLock` fix on top.

Verification (per-batch tier): `go build ./...` rc=0 first try; `go vet ./...` clean bar the two
pre-existing `//nolint` copylocks; `go mod tidy` clean (no dependency change); `gofmt -l` clean on
all changed files. **No fork surface touched** — `git diff HEAD -- params/ core/forkid/
internal/cli/server/chains/ builder/files/` is empty, no new `params.Rules` field and no new
`forkExpectations` entry. `go test` pass: `core/` (197s), `miner/...` (198s), `eth/` (51s),
`consensus/...` (incl. `consensus/bor` 43s), `core/state/...`, `core/stateless/...`,
`core/rawdb/...` (62s), `core/txpool/...`, `internal/ethapi/...`, `eth/filters/...`, `triedb/...`
(incl. `pathdb` 76s). Two failing packages, both **proven pre-existing** by re-running the same
tests on a clean worktree at batch-17 tip `0a83ed542`: `cmd/geth` (`TestConsoleWelcome`,
`TestCustomBackend`, `TestCustomGenesis`, `TestExport`, `TestAttachWelcome` — the documented
VEBLOP/non-Bor-genesis nil-deref class; `TestAttachWelcome` fails identically at 360s with all
three subtests timing out) and `core/vm` (`TestAbortDuringJump`, `TestInterruptDuringExecution` —
the documented interrupt-timing flake). Fork guards green
(`TestReinforceMultiClientPreCompilesTest`, `TestBorHardforkPrecompileContinuity*`,
`TestV2ForkParity`). No leftover conflict markers.

## v1.17.2 batch 4/4 (`be4dc0c4b`, plan row 19) — merged `682b4c380` — MILESTONE-CLOSING

17 first-parent commits, 12 conflicted files, 50 files in the merge (+689 / −135). The batch's
weight is EIP-7708, the first Amsterdam EIP whose content had to be reshaped rather than copied:
three separate adaptations were needed because Bor's transfer, selfdestruct, and parallel-state
paths all diverge from upstream's.

Adopted:

- **EIP-7708, ETH transfers as logs (#33645, `b87340a85`)** — **wired dormant** behind Bor's
  block-based Amsterdam gate. Two new topic constants in `params/protocol_params.go`
  (`EthTransferLogEvent`, `EthBurnLogEvent`), `types.EthTransferLog` / `types.EthBurnLog`,
  `StateDB.EmitLogsForBurnAccounts` (+ the `vm.StateDB` interface entry and the hooked
  forwarder), the `rules.IsAmsterdam` call in `stateTransition.execute`, and the
  `Transfer`/`TransferFunc` signature change to carry `*params.Rules`. Three adaptations below.
- **witness stats relocation (#34106, `c3467dd8b`)** — stats move from `StateDB`/`BlockChain`
  locals into `Witness` itself: `NewWitness` gains `enableStats bool`, `AddState` gains
  `owner common.Hash`, `StartPrefetcher` drops its stats parameter, `Witness.ReportMetrics`
  replaces the caller-side reporting. No consensus effect; swept across ~25 Bor call sites.
- **eth_getProof key cap (#34617, `95705e8b7`)**, **eth_simulateV1 block/call caps (#34616,
  `ceabc3930`)**, **`vm.ErrMaxInitCodeSizeExceeded` RPC error remap (#34067, `a61e5ccb1`)**,
  **freezer `dir.Sync()` Windows split (#34115, `e585ad3b4`)**, **`types.Sender` in legacypool
  (#34059, `d1369b69f`)**, **womir keeper target (#34079, `bd3c8431d`)**, **discv5 bootstrap DNS
  resolution (#34101, `a2496852e`)** — all auto-merged, verified present.
- **discv5 PingMultiIP session key (#34031, `e951bcbff`)** — decode against `tc.remoteAddr`
  rather than the packet source address; Bor's only divergence in the hunk was wsl blank lines.

### EIP-7708 adaptation 1 — log ordering across Bor's two transfer paths

Upstream appends the transfer log directly after the balance change in `Transfer`. Bor has two
transfer functions, and the Bor one has an early-return fast path:

- `Transfer` — additionally emits the 0x1010 `LogTransfer`, and returns early when
  `db.RecordTransfer` reports that V2 BlockSTM captured the transfer for settlement.
- `EthereumTransfer` — the no-log variant used when the chain carries no `Bor` config
  (execution-spec-tests). It now carries the EIP-7708 emission too, making it byte-equivalent to
  upstream's `Transfer`.

The emission was factored into `emitEthTransferLog` and called at the same point relative to the
balance change on all three paths. This is load-bearing, not cosmetic: on the V2 path Bor's 0x1010
log is generated later, at settlement, so placing the EIP-7708 log after `AddTransferLog` on the
serial path would order the pair one way under V1 and the other way under V2 — a receipt-root
divergence that would only surface once Amsterdam is enabled.

Recorded for the enablement decision: with Amsterdam active, a plain value transfer on Bor emits
**both** the EIP-7708 system log and the existing 0x1010 `LogTransfer`. Bloom and receipt-size
change, not a correctness problem, but a deliberate product call. See fork-register.md.

### EIP-7708 adaptation 2 — selfdestruct without #32919

Upstream's `opSelfdestruct6780` hunk reads `newContract` from `StateDB.IsNewContract`, which
arrives with the #32919 selfdestruct rework Bor declined. Instead of declining the branch, the
signal was recovered from Bor's own API: `SelfDestruct6780` already returns
`(balance, wasNewContract)` and the opcode handler was discarding both. Capturing the second
return reproduces upstream's semantics exactly, verified across all four cases against Bor's
shape (which always does `SubBalance(this)` + `AddBalance(beneficiary)` before the call):
`this != beneficiary` → transfer log regardless of newness; `this == beneficiary` → burn log only
when the contract was created in the same tx.

Declining would **not** have been covered by the adopted `EmitLogsForBurnAccounts` safety net:
Bor's `SelfDestruct6780` → `SelfDestruct` zeroes the balance in place, so the tx-boundary sweep
(which requires `!obj.Balance().IsZero()`) skips the account and the burn log is lost silently.

### EIP-7708 adaptation 3 — `ParallelStateDB.EmitLogsForBurnAccounts`

The new `vm.StateDB` method broke Bor's `ParallelStateDB`. Implemented against the parallel
executor's `destructed` map, address-sorted to match the serial executor's ordering (V1 sorts its
journal dirties for the same reason). Dead while Amsterdam is dormant; exists for V1/V2 parity.

### EIP-7708 test adaptation

Upstream's new `core/eth_transfer_logs_test.go` activates the fork with
`config.AmsterdamTime = new(uint64)` and pokes `BlobScheduleConfig.Amsterdam`; Bor is block-based
and carries no Amsterdam blob schedule, so the setup is `config.AmsterdamBlock = new(big.Int)`.
`MergedTestChainConfig` carries a `Bor` stanza, so the scenario runs the Bor transfer path and the
receipt interleaves 0x1010 logs. Rather than hardcode Bor's gas-dependent balance-snapshot
payloads, the assertion filters fee-address logs out and pins the interleaving through each
expected log's `Index`. Index 6 (the SELFDESTRUCT transfer) having no 0x1010 sibling is the direct
observable consequence of adaptation 2.

### #34106 Bor-specific fallout

- `core/state/statedb.go` kept its `witStart` / `s.WitnessCollection` timer around account-trie
  witness collection.
- Bor's V2-only `CollectStateWitness` / `trieReader.CollectStateWitness` callback grew the owner
  hash: main trie passes the zero hash, each sub-trie passes `crypto.Keccak256Hash(addr[:])`.
  Passing the zero hash for sub-tries would have misfiled every V2 worker-read storage node as an
  account-trie access in the stats.
- `core/stateless/witness.go` kept Bor's `sync.RWMutex` while taking upstream's new `stats` field.
- The now-redundant `witnessStats` local in Bor's `nolint:unused` `processBlock` was removed;
  `insertChain`'s two `NewWitness` sites pass `bc.cfg.VmConfig.EnableWitnessStats`, preserving
  Bor's existing opt-in.

Declined:

- **history import batching (#33894, `8f9061f93`)** — **whole commit deferred.** Rewrites
  `ImportHistory`'s per-block insert into a batched flush; six hunks conflicted. Bor's copy of the
  function is independently diverged — `era.NewIterator(e)` rather than `e.Iterator()`, a `forker`,
  and header insertion via `chain.HeaderChain().InsertHeaderChain` before the receipt chain, none
  of which upstream does. Throughput-only change on an offline CLI path with no consensus surface;
  hand-merging batched flush semantics into Bor's header-inserting loop risks silently producing a
  corrupt imported history db. Reverted to HEAD, filed against the era cluster (#32157).
- **OTel SampleRatio IsSet guard (#34062, `745b0a8c0`)** — dependent decline; the whole
  `setOpenTelemetry` function is absent from Bor's `cmd/utils/flags.go` (OTel line declined in
  v1.17.0, #33452 / #33484).
- **slot number in test payload (#34094, `acdd13971`)** — dependent decline; the only changed line
  is inside `BuildTestingPayload`, which Bor deleted with `testing_buildBlockV1` (#33656).
- **v1.17.2 release commit (`be4dc0c4b`)** — `version/version.go` auto-took the bump to
  `1.17.2-stable`; reverted to Bor's `Major=1 / Minor=17 / Patch=0 / Meta="unstable"`.

Verification (per-batch tier): `go build ./...` clean; `go vet ./...` clean bar the two pre-existing
`//nolint` copylocks; `gofmt -l` clean on all changed files; `go mod tidy` no-op. **Fork surface is
`params/protocol_params.go` only** (+7, two event-topic constants) — no new fork gate, no new
`params.Rules` field, no new `forkExpectations` entry, and the diff against `core/forkid/`,
`internal/cli/server/chains/` and `builder/files/` is empty. Both topic constants were verified
against `crypto.Keccak256Hash` rather than trusted from the diff. Fork guards green
(`TestReinforceMultiClientPreCompilesTest`, `TestBorHardforkPrecompileContinuity*`,
`TestV2ForkParity`). `go test` pass: `core/` (181s), `core/state/...`, `core/stateless/...`,
`core/vm/runtime`, `core/vm/program`, `miner/...` (199s), `params/...`, `consensus/bor/...`,
`core/rawdb/...` (56s), `core/txpool/...`, `core/types/...`, `core/tracing`, `eth/` (51s),
`eth/fetcher/...` (140s), `eth/protocols/...`, `internal/ethapi/...`, `p2p/discover/...` (31s),
`trie/...`, `cmd/devp2p`. Two failing packages, both **proven pre-existing** on a detached worktree
at batch-18 tip `1abb57b8f`: `core/vm` (`TestAbortDuringJump`, `TestInterruptDuringExecution` — the
documented interrupt-timing flake; baseline fails 5 of 6 runs with byte-identical assertions, the
branch 2 of 3, no rate change) and `cmd/devp2p/internal/ethtest` (`BorConfig.CalculatePeriod`
nil-deref panic from `miner.newWorkLoop` — identical panic and stack at baseline, the documented
VEBLOP/non-Bor-genesis class). No leftover conflict markers.

## v1.17.2 milestone triage (upstream PRs `16783c167..be4dc0c4b`, 77 first-parent commits)

The wiring pass over the whole milestone, independent of which commits conflicted: an upstream PR
can merge completely clean and still leave Bor un-wired. Classification per §6.1.

**One finding needing a decision, one confirmation, the rest inert.**

| Class | Count | PRs |
| --- | --- | --- |
| **needs wiring** | 1 | #33836 / #33975 default cache bump |
| **consensus-relevant** | 5 | #33593 (EIP-7778), #33869 (EIP-8024 update), #33947 (Amsterdam jump table), #33832 (EIP-7954), #33645 (EIP-7708) |
| **operator-visible, no wiring** | 7 | #34617, #34616, #32789/#33952, #33763, #33976, #34005, #34101 |
| **deferred** (rows in needs-wiring.md) | 12 | #33931, #33816, #33773, #33648, #34036, #34011, #33894, #34062, #34094, #33950, #33955, #33150 |
| **inert** | 52 | refactors, bugfixes, tests, build, dependency bumps, dormant-feature work |

### needs wiring — default cache bump (#33836, #33975)

The one case in this milestone where a clean merge left Bor's behavior unchanged when it looks
like it changed. Upstream raised the default total cache 1024 MB → 4096 MB, which shows up in this
milestone's diff as `ethconfig.Defaults` moving to `DatabaseCache 2048 / TrieCleanCache 614 /
TrieDirtyCache 1024 / SnapshotCache 409`. Bor's `bor server` path never reads those: `internal/cli/
server/config.go` carries its own `Cache: 1024` and a 50/15/25/10 percentage split whose `calcPerc`
output overwrites all four fields, and that split against 1024 MB reproduces geth's **old** defaults
(512 / 153 / 256 / 102) exactly. So the bump reaches `cmd/geth` and tests but not Bor's production
startup. Deliberately not taken — quadrupling the default memory footprint is a PoS/devops call.
Row filed in `needs-wiring.md` with the one-line change if the team wants parity.

### consensus-relevant — register review

Reviewed `fork-register.md` for completeness across the milestone. All five entries are
**enable-by-block, dormant**, gated on `IsAmsterdam`, which is `nil` on every shipped Bor preset:
EIP-7778 (batch 16), the EIP-8024 branchless/EXCHANGE update and the Amsterdam jump-table dispatch
(batch 16), EIP-7954 (batch 17), EIP-7708 (batch 19). No fork block, `params.Rules` field, forkid
input, chain preset, or genesis file changed anywhere in the milestone. Two entries carry
enable-time consequences beyond "set the block" and are recorded as such in the register: EIP-7954
overlaps Bor's live Ahmedabad code-size cap (and would move initcode 49152 → 65536), and EIP-7708
duplicates Bor's 0x1010 `LogTransfer` while depending on a workaround for the declined #32919.

### operator-visible, no wiring

Behavior changes an operator could notice but which need no Bor-side plumbing: hardcoded RPC caps
on `eth_getProof` keys (#34617) and `eth_simulateV1` blocks/calls (#34616); the new `MaxUsedGas`
field in the `eth_simulateV1` response (#32789, gas-cap fix #33952); `eth_getLogs` now erroring on
an inverted block range instead of returning `nil, nil` (#33763); `eth_createAccessList` returning
`[]` rather than `null` for empty `StorageKeys` (#33976); hex-encoded `slotNumber` in
`RPCMarshalHeader` (#34005, dormant — Bor's header field is nil pre-Amsterdam); and DNS hostname
resolution for bootstrap nodes (#34101).

### inert

The remaining 52 are internal refactors, bugfixes, test fixes, build changes and dependency bumps
with no Bor-facing surface. Two sub-groups worth naming rather than listing: the binary-trie work
(#34056, #34032, #33989, #33961, #33951, #34021, #34022) is real but sits behind Verkle/bintrie,
dormant on Bor; and the new upstream tooling that merged in unused (`cmd/fetchpayload` #33919, the
`womir` keeper target #34079) builds clean and needs no Bor wiring — Bor ships its CLI from
`internal/cli`, not `cmd/geth`.

## v1.17.2 milestone full-gate (invariant 8)

Run at milestone tip `682b4c380`.

| Gate | Result |
| --- | --- |
| `go build ./...` | clean |
| `go vet ./...` | clean bar the two pre-existing `//nolint` copylocks |
| `make lint` (golangci-lint v2.11.4, repo `.golangci.yml`) | **0 issues** |
| `gofmt -l` / `go mod tidy` | clean / no-op |
| `go test ./...` | **148 packages pass**, 4 fail — all pre-existing (below) |
| `make test-integration` | **`tests/bor` ok, 628 s, 77.6% coverage**; `tests` ok but see caveat |
| govulncheck | 3 called vulns, unchanged from before the milestone |
| kurtosis devnet | **8/8 checks pass** + EIP-7708 dormancy proof (below) |

### Pre-existing test failures (no new names)

`cmd/geth` (`TestAttachWelcome` 360 s, `TestConsoleWelcome`, `TestCustomBackend`,
`TestCustomGenesis`, `TestExport`) and `cmd/devp2p/internal/ethtest` — the VEBLOP/non-Bor-genesis
nil-deref class; `cmd/evm` (`TestT8n`, `TestEVMTracing`, `TestEvmRun`, `TestEvmRunRegEx`) — t8n
golden drift; `core/vm` (`TestAbortDuringJump`) — the interrupt-timing flake. `ethtest` and
`core/vm` were re-baselined on a detached worktree at `1abb57b8f` during batch 19 and fail
identically there.

### Caveat — `make test-integration`'s `./tests` package is a no-op here

It reports `ok` in 1.2 s at 0% coverage because `tests/testdata` (the ethereum consensus fixtures)
is absent, and `.gitmodules` defines only `tests/evm-benchmarks`, so CI's
`git submodule update --init --recursive` doesn't fetch them either — `TestState`, `TestBlockchain`,
`TestTransaction`, `TestRLP` and `TestDifficulty` all skip rather than fail. Pre-existing repo
property, not caused by this milestone, but worth knowing: the integration signal comes from
`tests/bor`, not from the upstream consensus suite.

### govulncheck — unchanged

Three called vulnerabilities, identical to before the milestone, all existing team follow-ups:
`GO-2026-5970` (`golang.org/x/text@v0.37.0`, fixed in 0.39.0), `GO-2026-5932`
(`golang.org/x/crypto@v0.51.0`, no fix available), `GO-2026-5856` (`crypto/tls@go1.26.4`, fixed in
Go 1.26.5). No new entries from this milestone's dependency bumps.

### Kurtosis devnet — EIP-7708 dormancy proven on a live chain

`small` preset (1 validator + 1 RPC + bridge/tx spammers + observability) on `bor:v1172-sync`,
built from this tip. Block production ~1 s with validator and RPC in lockstep (168 → 236 over
80 s), 45–100 txs/block, 29 `StateSynced` logs, span 3 active, checkpoint 6 submitted, zero
`ERROR`/`panic`/`FATAL` in either client's logs.

The check that matters for this milestone: over the last 50 blocks,

| Log source | Count |
| --- | --- |
| `0xfffffffffffffffffffffffffffffffffffffffe` (EIP-7708 `SystemAddress`) | **0** |
| `0x0000000000000000000000000000000000001010` (Bor `LogTransfer`) | **2598** |

The 2598 is what makes the 0 meaningful — thousands of value transfers went through
`core.Transfer` in that window and none emitted a system log. A leaked gate, or an emission placed
outside the `rules.IsAmsterdam` check on any of the three transfer paths (serial, V2 BlockSTM fast
path, `EthereumTransfer`), would have produced a non-zero count. Report:
`runs/pos-spawn-devnet/2026-07-27T08-07-02Z-claude-pos-v1172-sync/summary.md`.

## v1.17.3 batch 1/7 (`04e40995d`, plan row 20) — merged `d9a3a0bc9` — MILESTONE-OPENING

Branch `ppatil-upstream-v1.17.3`, cut from the `core/vm` catch-up tip `8b71184d4`
rather than from `ppatil-upstream-v1.17.2` — batch 21 consumes the four
`gas*Intrinsic` functions that catch-up introduced. Cut-point guard passed
(`upstream-merge-v1.17.2-done` is an ancestor). 20 upstream commits, **30
conflicts**, 58 files, +2926/−672.

Two whole features deferred; they account for 18 of the 30 conflicts.

### Consensus surface

| File | PR | Class | Decision |
| ---- | -- | ----- | -------- |
| `consensus/beacon/consensus.go` | #34064 | 3 (hardfork surface) | **Combined.** Upstream added a `BlockAccessListHash` existence/non-existence check beside the existing EIP-7843 `SlotNumber` check. Kept Bor's **block-based** `IsAmsterdam(header.Number)` gate and took upstream's two-field body, so both fields activate together when Amsterdam is enabled. Dormant today: the else-branch asserts both are nil, which is what every Bor header carries. |

### Whole-feature deferrals

| Feature | PR | Why | Footprint reverted |
| ------- | -- | --- | ------------------ |
| eth/70 partial receipt lists | #33153 | Bor's `ExcludeStateSyncReceipt()` is a `ReceiptList` interface method feeding **receipt-root derivation**, fork-gated on Madhugiri; its heuristic only defines behaviour on a *complete* list, so `ReceiptList70` needs a design decision for partial responses. Bor also stays `[ETH69, ETH68]` vs upstream's `[ETH70, ETH69]`. | 31 files to HEAD, incl. auto-merged companions (`ethtest/{chain,conn}.go`, regenerated testdata, `rlp/rlpgen/testdata/pkgclash.*`); `rm` kept Bor's deletions of `downloader_test.go` and upstream's `fetchers_concurrent_receipts.go` (Bor's is `bor_fetchers_concurrent_receipts.go`, so the conflict surfaced as a rename). |
| snap/2 + BAL serving | #34083 | Serves block access lists, which Bor cannot produce while Amsterdam is dormant. Upstream ships the dispatch commented out (`//case SNAP2:`). | `eth/protocols/snap/{handler,handler_fuzzing_test,protocol}.go`, `core/blockchain_reader.go` to HEAD; new `handlers.go`, `handler_test.go` removed. |

### Remaining resolutions

| File | PR | Class | Decision |
| ---- | -- | ----- | -------- |
| `version/version.go` | #34619 | 1 | Kept Bor's `Patch=0`/`unstable`; declined the v1.17.3 release bump (recurring auto-merge trap). |
| `eth/gasestimator/gasestimator.go` | #34081 | 2 | Took upstream's restructure (overrides now applied to the header by the caller) and re-added Bor's **Madhugiri** gate. Bor's side read `opts.BlockOverrides`, a field the struct had already lost via auto-merge — it would not have compiled. |
| `core/state/statedb.go` | #33102 | 1 | Import combine; kept `snapshot` (upstream dropped it) for Bor's BlockSTM-only `NewWithMVHashmap`. |
| `core/types/block.go` | #34064 | 2 | Import combine — `core/types/bal` alongside Bor's `log`/`params`. |
| `core/rawdb/schema.go` | #34064 | 2 | Combine — Bor's block-prune keys plus upstream's `accessListPrefix`. |
| `core/state/database.go`, `database_history.go` | #33102 | 1 | Kept Bor's `Snapshot()` and Bor's removal of `Commit` (Bor uses `CommitWithUpdate`); **re-added upstream's `Iteratee()` implementations**, which the hunk-level take-ours dropped while the interface retained the method. Build caught it. |
| `core/state/dump.go` | #33102 | 2 | Took upstream's `acctIt.Hash().Hex()`; Bor's `it.Key` was stale once the iterator refactor auto-merged around it. |
| `core/rawdb/accessors_chain_test.go` | #34064 | 2 | Append collision, base empty — combined both sides. Ours' two closing braces were consumed by the conflict split; `gofmt` caught the imbalance. |
| `eth/api_backend.go` | #33102 | 1 | Kept Bor's `Miner()`/`StartMining()`; took upstream's `StateAtBlock` without `reexec`. |
| `eth/tracers/api.go` (6 hunks), `api_test.go` | #33102, #34093 | 1 | Dropped all `reexec` plumbing; kept Bor's `gasBailout` field and develop's `canonicalTxTraceEnv` refactor. |
| `eth/protocols/eth/handler.go` | #33153, #34083 | 3 | Kept Bor's eth68/eth69 maps + dispatch. Also declines #34083's removal of `Time()` from `Decoder`, consistent with Bor's deferral of #33835 which introduced it. |
| `tests/init.go` | #34671 | 3 | Kept Bor's deletion — upstream's Amsterdam statetest keys on `AmsterdamTime` with a BPO4 blob schedule, fields Bor lacks. Coverage gap. |
| `core/blockchain_reader.go` | #34083 / #34633 | 3 | Split decision: dropped #34083's `GetAccessListRLP`, **re-applied #34633's** `StateIndexProgress` to `(uint64, uint64, error)`. The wholesale revert for the deferral had discarded both. |

### Bor-only twin scan (§4.6, first run)

**Checked; no mirroring required.** Batch 20 does not touch `core/vm` at all
(a name-only diff of `be4dc0c4b..04e40995d` over `core/vm/` is empty), so the
PIP-88 twins are untouched. The twins that *are* in range —
`ReceiptList68`/`ReceiptList69` and `bor_fetchers_concurrent_receipts.go` — sit
inside #33153's deferred footprint and intentionally stay at Bor HEAD.

**Gap the step does not cover, found the hard way.** Bor-only *features* that
depend on an upstream signature produce **no conflict** and surface only at build
time. #33102 removed the `reexec` parameter from
`StateAtBlock`/`StateAtTransaction`, breaking develop's Parity tracer across six
Bor-only files (`eth/tracers/parity{,_block,_call,_replay_block,_replay_tx}.go`
plus the `prunedTestBackend` helper in `api_test.go`). Adapted rather than
reverted — outcome 2, keep Bor's logic and take upstream's signature. A test named
`TestIntermediateRoots_WithReexecOverride` was renamed to
`TestIntermediateRoots_WithNonNilConfig`, since the override it named no longer
exists. **The twin scan should be widened to "Bor-only code depending on a
changed upstream signature", not just near-verbatim clones.**

### Verification

Build, `go vet` (core/eth/tests/consensus/internal), `gofmt` all clean apart from
the two documented `//nolint` copylocks. Tests green: `core/state`, `core/rawdb`,
`core/types`, `core/types/bal`, `consensus/beacon`, all `eth/tracers/...`,
`eth/protocols/{eth,snap,wit}`, `eth/downloader`, `core` (172.9 s), `eth` (47.9 s),
`internal/ethapi`. Three defects were found by build/vet during verification and
fixed rather than shipped: the missing `Iteratee` implementations, the over-broad
`blockchain_reader.go` revert, and the Parity tracer's `reexec` dependence.

## v1.17.3 out-of-band adoption — EIP-7975 / eth/70 (#33153, `965bd6b6a`)

Branch `ppatil-upstream-eth70`, cut from batch 20's tip `d9a3a0bc9`. Not a merge
batch: batch 20 reverted #33153's 31-file footprint wholesale, so this is a
hand-written port of the feature onto Bor's diverged receipt code, taken before
batch 21 so the tree still matches the state upstream's commit expected.
21 files changed plus one new test file, +635/-159.

### What the deferral note got wrong

Two of the five planned adoption steps evaporated on contact with the code, and
the risk the note flagged as the blocker turned out not to exist.

- **No `ReceiptList70` is needed.** EIP-7975 changes the packet envelope, not the
  receipt-list encoding, so eth/70 carries `ReceiptList69`. Upstream reuses its
  single `ReceiptList` for both `ReceiptsPacket69` and `ReceiptsPacket70` for the
  same reason.
- **The "what does exclusion mean for a partial list?" decision does not arise.**
  Bor never applies `ExcludeStateSyncReceipt()` in the p2p handler:
  `handleReceipts` passes a deliberately nil `metadata` func because it has no
  block number, and the Madhugiri-gated exclusion runs in the downloader queue
  (`bor_fetchers_concurrent_receipts.go:99` → `EncodeReceiptsAndPrepareHasher`).
  Upstream never sinks a partial list to the downloader, so the exclusion only
  ever sees a reassembled complete list — unchanged from today.

### Consensus surface

| Area | Decision |
| ---- | -------- |
| State-sync receipt across a chunk boundary | The exclusion is positional (last element) and the serving side identifies the state-sync receipt by absolute index. `blockReceiptsToNetwork69`'s loop counter stays absolute when receipts are skipped, and the tx-type iterator is advanced in lockstep, so a truncated response cannot displace it. Covered by `TestPartialReceipts_StateSyncAcrossChunkBoundary`, which splits every fixture at every size limit and asserts the reassembled receipt root on **both** sides of the Madhugiri gate. |
| Amsterdam gate on the response bound | Upstream keys the reduced minimum tx gas (4500 vs 21000) off `AmsterdamTime`. Bor has no Amsterdam timestamp, so this is rewired to block-based `IsAmsterdam(number)`, and `RequestReceipts` carries block **numbers** where upstream carries timestamps. `AmsterdamBlock` is nil, so the 21000 bound applies today; both sides pinned by `TestValidateLastBlockReceiptAmsterdamGate`. |
| Receipt-count bound vs Bor's free state-sync tx | Upstream bounds a truncated response by `receipts <= gasUsed/minTxGas`. A Bor block carries one receipt more than its gas can account for, because the state-sync transaction burns none, so the bound gained a `+1` tolerance. Without it a peer serving a legitimate Bor block could be rejected. |

### Resolutions

| File | Decision |
| ---- | -------- |
| `eth/protocols/eth/receipt.go` | Added `Append` and `LogsSize` to `ReceiptList69`. Bor stores decoded `[]Receipt` where upstream keeps raw RLP, so both are simpler here than upstream's iterator-based versions. |
| `eth/protocols/eth/receipt.go` | **Extended `blockReceiptsToNetwork69` with `receiptQueryParams` rather than cloning it** — see the twin scan below. Its zero value reproduces the previous whole-block behavior. One behavior change on the eth/69 path, taken deliberately and aligned with upstream: a receipt with no corresponding body transaction is now an error instead of being encoded as type 0. Reachable only from locally corrupt data, and the caller skips the block. |
| `eth/protocols/eth/protocol.go` | `ETH70` const; `ProtocolVersions = [ETH70, ETH69, ETH68]` (Bor keeps eth/68, having declined #33511, against upstream's `[ETH70, ETH69]`); `protocolLengths[ETH70] = 18`; `GetReceiptsPacket70`, `ReceiptsPacket70`, `ReceiptsRLPPacket70`. Bor's existing `GetReceiptsPacket` is shared by eth/68 and eth/69 and is **not** renamed to `…69` as upstream did, since in Bor it serves two versions. |
| `eth/protocols/eth/peer.go` | Ported `receiptRequest`, `receiptBuffer` + lock, `requestPartialReceipts`, `bufferReceipts`, `flushReceipts`, `validateLastBlockReceipt`, `ReplyReceiptsRLP70`. `NewPeer` gains a `*params.ChainConfig`. Two Bor-side additions to upstream: the buffer entry is dropped when `dispatchRequest` fails (upstream leaks it; its own TODO acknowledges the concern), and `bufferReceipts` rejects a response claiming more blocks than were requested before indexing `buffer.gasUsed`. |
| `eth/protocols/eth/handlers.go` | **Extracted `gatherBlockReceipts`** from `ServiceGetReceiptsQuery69` — see the twin scan. Added `handleGetReceipts70`, `ServiceGetReceiptsQuery70`, `handleReceipts70`. The eth/70 size limit reuses Bor's existing `maxMessageSize` (10 MiB) instead of adding upstream's duplicate `maxPacketSize` const. |
| `eth/protocols/eth/handler.go` | Added the `eth70` handler map, mirroring Bor's `eth69` map (which keeps `NewBlockHashesMsg`/`NewBlockMsg`, unlike upstream). Version dispatch converted from if/else to a switch. |
| `eth/protocols/eth/dispatcher.go` | A continuation re-uses the original request ID, so `pending` is no longer overwritten; the receipt buffer is dropped on cancel. |
| `eth/downloader/{peer.go, bor_fetchers_concurrent_receipts.go}` | `RequestReceipts` gains `gasUsed` and `numbers`. Upstream passes timestamps; Bor passes block numbers, to match its block-based fork gate. |
| `EncodeReceiptsAndPrepareHasher` | **Unchanged.** eth/70 delivers `[]*ReceiptList69`, which its existing type switch already handles. |

### Bor-only twin scan (§4.6)

**Ran against my own diff, and it found something.** The first cut added
`blockReceiptsToNetwork70` as a near-verbatim clone of `blockReceiptsToNetwork69`
differing only by the bounds, and duplicated the ~70-line pre-Madhugiri
state-sync merge into a second service function. That is exactly the drift
pattern the step exists to catch, and creating two new twins in the change that
documents twins as a hazard would have been perverse. Both were folded back:
`blockReceiptsToNetwork69` now takes `receiptQueryParams` (matching upstream's
single parameterised function), and the merge logic lives in one
`gatherBlockReceipts` used by both service functions. This widens the eth/69
serving-path diff and is deliberate scope.

The known PIP-88 twins are untouched — this change does not enter `core/vm`.

### Verification

Build, `go vet ./...`, `gofmt` clean apart from the two documented `//nolint`
copylocks; `go mod tidy` a no-op. Tests green: `eth/protocols/{eth,snap,wit}`,
`eth/downloader`, `eth/downloader/whitelist`, `eth`, `eth/fetcher`, plus both
fork meta-guards (`TestReinforceMultiClientPreCompilesTest`, `TestV2ForkParity`).
New coverage in `eth/protocols/eth/receipt70_test.go` (chunked reassembly across
every splitting size limit, state-sync across a chunk boundary, first-receipt
overflow, `firstIndex` resumption, `Append`, `LogsSize`, both response bounds,
the Amsterdam gate) and an `ETH70` case in `testGetBlockReceipts` covering the
handler round trip and a resumed request.

**Coverage gap, recorded rather than closed:** the truncation path only fires on
blocks whose receipts exceed 10 MiB. No devnet produces those, so the split
behavior is exercised by unit tests over synthetic size limits, not end to end.
Upstream's `cmd/devp2p/internal/ethtest` eth/70 suite and its regenerated
testdata were not ported (geth-chain-specific; Bor's ethtest is already a
known-failing surface).
