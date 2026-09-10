# Upstream merge plan: go-ethereum v1.17.4

| Field           | Value                                                        |
| --------------- | ------------------------------------------------------------ |
| Target          | `v1.17.4` (`36a7dc72e96b3f42846be925cfeb2fad18489917`)        |
| SYNC_BASE       | `v1.16.8` (`abeb78c647e354ed922726a1d719ac7bc64a07e2`) — highest upstream release tag that is an ancestor of `develop`. Persisted; never recompute on resume. |
| Batch size      | 20 first-parent commits                                       |
| Upstream remote | `upstream` (https://github.com/ethereum/go-ethereum.git)      |
| Base branch     | `upstream-merge-v1.17.4` (cut from `origin/develop`). Slash-free on purpose: bor ruleset 626146 excludes `refs/heads/**upstream**` from the signed-commit requirement, and `**` does not cross `/` — slashed names stay rule-covered and reject unsigned upstream commits. Working branches follow the same constraint (`<username>-upstream-<milestone>`). |
| Planned         | 2026-07-06 (plan-only; no merges performed)                   |
| Totals          | 607 first-parent commits, 6 milestones, 33 batches            |

Notes:

- `v1.16.9` is not integrated into `develop` (it lives on upstream `release/1.16`), so it is the first milestone. Expect already-applied no-op conflicts when crossing between release branches (its two crypto fixes reappear on the v1.17.0 line as `c974722dc` / `9b78f45e3`).
- Already-integrated guard: none of the six milestone tags is an ancestor of `origin/develop` (checked 2026-07-06); all six stand.
- Boundary SHAs are upstream first-parent commits (trees upstream CI passed).

## Batches

Status legend: `pending` / `in-progress` / `merged` / `skipped`.

| #  | Milestone | Batch | Boundary    | Commits | Theme                                                                                          | Status  |
| -- | --------- | ----- | ----------- | ------- | ---------------------------------------------------------------------------------------------- | ------- |
| 1  | v1.16.9   | 1/1   | `95665d570` | 3       | crypto security backports (ECIES invalid-curve, secp256k1 coordinate check) + release          | merged (`a06dbd7d2`) |
| 2  | v1.17.0   | 1/12  | `f23d506b7` | 20      | bugfix sweep: rawdb/state/pathdb fixes, RPC framing, blobpool, filters                          | merged (`2a85bf611`) |
| 3  | v1.17.0   | 2/12  | `cf93077fa` | 20      | verkle/UBT groundwork, rawdb+rlp hardening, fusaka beacon update                                | merged (`ad0f54e9c`) |
| 4  | v1.17.0   | 3/12  | `a122dbe45` | 20      | **EIP-8024 introduced** (`core/vm`), miner `--miner.maxblobs`, gnark-crypto bump                | merged (`7278fa559`) |
| 5  | v1.17.0   | 4/12  | `228933a66` | 20      | catalyst getPayload fork checks, pebble config, slow-block stats, downloader syncmode           | merged (`8b4a6095d`) |
| 6  | v1.17.0   | 5/12  | `5dfcffcf3` | 20      | state/code-read metrics, tx fetcher validation, blobpool legacy sidecar removal                 | merged (`4e17d3dc0`) |
| 7  | v1.17.0   | 6/12  | `710008450` | 20      | snap-sync locking, getBlobsV3, pathdb history indexing, go-verkle removal                       | merged (`85a1750c5`) |
| 8  | v1.17.0   | 7/12  | `94710f79a` | 20      | state update hook, trienode history compression, blobpool gaps; KZG peer-drop fix               | merged (`09cddf9b7`) |
| 9  | v1.17.0   | 8/12  | `8fad02ac6` | 20      | OpenTelemetry RPC tracing, **read-only gas handlers (`core/vm`)**, trienode history enable      | merged (`c1537496c`) |
| 10 | v1.17.0   | 9/12  | `845009f68` | 20      | eth_getProofs for history, **EIP-8024 immediate-byte update**, alloc reductions                 | merged (`0f76e0c30`) |
| 11 | v1.17.0   | 10/12 | `c12959dc8` | 20      | perf/alloc sweep, keccak vendoring, Ledger Gen5, rlp RawList                                    | merged (`f4f2c07fd`) |
| 12 | v1.17.0   | 11/12 | `c50e5edfa` | 20      | EraE format, rlp iterators, HTTP/2 JSON-RPC, telemetry CLI wiring                               | merged (`2c0608fc6`) |
| 13 | v1.17.0   | 12/12 | `0cf3d3ba4` | 7       | delayed p2p decoding, header-verification hardening, **v1.17.0 release**                        | merged (`70994671a`) |
| 14 | v1.17.1   | 1/2   | `9ecb6c4ae` | 20      | **syscall value-transfer disable (`core/vm`)**, BAL type changes, **Amsterdam precompile touch** | merged (`189030866`) |
| 15 | v1.17.1   | 2/2   | `16783c167` | 20      | **eth/68 protocol drop**, **EIP-7843 SLOTNUM**, **8024 enabled in Amsterdam**, Go 1.26; release  | merged (`b6175113d`) |
| 16 | v1.17.2   | 1/4   | `00540f946` | 20      | **EIP-7778 block gas accounting**, miner prefetcher, **amsterdam jump table**, default cache 4096 | merged (`09c784851`) |
| 17 | v1.17.2   | 2/4   | `77e7e5ad1` | 20      | codedb refactor, **EIP-7954 max contract size**, trienode history alongside data                | merged (`0a83ed542`) |
| 18 | v1.17.2   | 3/4   | `e23b0cbc2` | 20      | stateless codedb fix, **call-variant gas measurement rework (`core/vm`)**, bintrie parallel hash | merged (`1abb57b8f`) |
| 19 | v1.17.2   | 4/4   | `be4dc0c4b` | 17      | **EIP-7708**, simulateV1/getProofs limits, **v1.17.2 release**                                  | merged (`682b4c380`) |
| 20 | v1.17.3   | 1/7   | `04e40995d` | 20      | **eth/70 partial receipts**, BAL storage layer + snap/2 BAL serving                             | pending |
| 21 | v1.17.3   | 2/7   | `c453b99a5` | 20      | **gas becomes vector <regularGas, stateGas> (`core`)**, bintrie fixes                           | pending |
| 22 | v1.17.3   | 3/7   | `5af5510b1` | 20      | EIP-7610 rework, CachingDB split (merkle/binary), freezer fsync fix                             | pending |
| 23 | v1.17.3   | 4/7   | `33c1bd59f` | 20      | **gas budget (`core/vm`)**, **EIP-7976 calldata floor**, **FinalizeAndAssemble removed (consensus iface)** | pending |
| 24 | v1.17.3   | 5/7   | `41b856d47` | 20      | BAL spec update, **stack arena (`core/vm`)**, skip tx gas cap post-Amsterdam                    | pending |
| 25 | v1.17.3   | 6/7   | `1abbae239` | 20      | **EIP-7981 access-list cost**, eth_call block overrides, TypeMux removal                        | pending |
| 26 | v1.17.3   | 7/7   | `117e067f0` | 16      | misc fixes, **core.Message moves to uint256**, **v1.17.3 release**                              | pending |
| 27 | v1.17.4   | 1/7   | `1149f76dc` | 20      | pre/post-execution wrap (`core`+miner), GasChangeHook v2, block-level accessList                | pending |
| 28 | v1.17.4   | 2/7   | `ca1a027fa` | 20      | **block accessList construction (consensus/miner)**, eth71 BAL response, engine_hasBlobs        | pending |
| 29 | v1.17.4   | 3/7   | `4017efe34` | 20      | **jumpdest bitmap global cache (`core/vm`)**, RPC hardening, deprecated CLI flag removal        | pending |
| 30 | v1.17.4   | 4/7   | `80d9ba5d9` | 20      | blobpool status queue, BAL freezer management, snapv2 skeleton                                  | pending |
| 31 | v1.17.4   | 5/7   | `e444c267a` | 20      | **clef removed**, repo-wide typo sweep (`all:`), blob sidecar fixes                             | pending |
| 32 | v1.17.4   | 6/7   | `8c540cb08` | 20      | **snap/2 sync logic**, **EIP-8037 state-creation gas**, cgroup memlimit                         | pending |
| 33 | v1.17.4   | 7/7   | `36a7dc72e` | 4       | reflect.Pointer sweep (`all:`), **v1.17.4 release**                                             | pending |

## Bor-specific risk flags (advance warning, not a substitute for per-batch review)

- **Consensus interface churn:** batch 23 removes `FinalizeAndAssemble` from the
  consensus interface and batches 27–28 rework block assembly around block
  access lists — `consensus/bor` implements this interface, so expect class-3
  conflicts and real porting work there, not mechanical merges.
- **EVM/hardfork surfaces** (invariant: full individual walkthrough, never
  grouped): EIP-8024 (batches 4, 10, 15), EIP-7843 SLOTNUM (15), EIP-7778 (16),
  EIP-7954 (17), EIP-7708 (19), EIP-7976 (23), EIP-7981 (25), EIP-8037 (32),
  Amsterdam jump-table/precompile wiring (14–16), call-gas rework (18), gas
  vector (21), gas budget (23), jumpdest cache (29). Every one of these touches
  `core/vm` / `params` and lands behind upstream's Amsterdam fork — Bor must
  decide fork-gating against its own hardfork schedule; precompile continuity
  tests (`core/vm/contracts_continuity_test.go`) must be preserved.
- **P2P protocol changes:** eth/68 dropped (15), eth/70 partial receipts (20),
  eth/71 BAL (28), snap/2 (20, 30, 32) — check against Bor's protocol
  registration and peer management.
- **Miner:** prefetcher enable (16), payload rebuild timing (15), slot number
  (27–30) — interacts with Bor's sprint-based mining loop.
- **Wide-but-shallow sweeps:** typo sweep (31) and reflect.Pointer sweep (33)
  touch `all:` — expect many trivial conflicts against Bor-modified files.
- **v1.16.9 duplicates:** its two crypto fixes reappear in batches 10 and 13 —
  expect no-op/already-applied conflicts there.

## Resume anchors

- `PROGRESS_TIP`: `70994671a` (v1.17.0 batch 12/12 `0cf3d3ba4` merged). **All 12 v1.17.0 batches merged.** Next = v1.17.0 **milestone chores** (durable-docs commit + `gen_config.go` regen + `pdbExemptMethods` fix + full integration/kurtosis/diffguard/govulncheck + PR triage), then the v1.17.0 stacked draft PR, then milestone v1.17.1 (batches 14–15, boundary `9ecb6c4ae`).
- Working branch for milestone 1 (`v1.16.9`): `ppatil-upstream-v1.16.9` — cut
  from `upstream-merge-v1.17.4` on the milestone's first batch.
- Full first-parent logs per milestone captured in the planning run directory:
  `runs/pos-merge-upstream/2026-07-06T09-56-56Z-claude-v1.17.4-plan/log-<tag>.txt`.
