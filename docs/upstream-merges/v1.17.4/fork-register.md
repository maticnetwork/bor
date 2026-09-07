# Fork / EIP register: go-ethereum v1.17.4 sync

Per-batch record of every upstream hardfork or EIP encountered during the sync
(invariant 9). **Default policy: any hardfork/EIP introduced upstream is merged
disabled by default in Bor** — its activation gate stays nil/unset so the code
is dormant on every Bor network. Enabling is always a separate, explicit
PoS-team decision (its own change + review + N-1/N/N+1 activation tests).

`decision` ∈ {`never-enable`, `defer`, `adopt-later`, `already-ours`}.
`verified-dormant` = confirmed the Bor gate is nil/unset **and** the behavior is
gate-guarded (not ungated / unconditional).

| EIP / fork | upstream PR | batch | touches | Bor gate (value) | decision | verified-dormant |
| ---------- | ----------- | ----- | ------- | ---------------- | -------- | ---------------- |
| Verkle / binary-trie transition | verkle/UBT groundwork (`cf93077fa`); go-verkle removal `#33461` (`710008450`) | 3 (v1.17.0 2/12) intro → removed batch 6 (`b66d3f157`) | `core/state`, `core/vm`, `core/types/block.go`, `trie/*`, `cmd/*` | `VerkleBlock = nil` (all Bor presets); `IsVerkle(num)` block-based — **fork gate KEPT dormant** | **go-verkle impl REMOVED** (2026-07-23, `b66d3f157`, mirroring #33461); binary tree kept; fork gate kept dormant — see Notes | yes — go-verkle/go-ipa deleted; `VerkleTrie`/`PointCache`/verkle `ExecutionWitness` gone; `AccessEvents` key derivation now `bintrie`. `VerkleBlock: nil` on every preset so `IsVerkle()` never activates; upstream's follow-up verkle stub/rename PRs to be mirrored in later batches. |
| EIP-8024 (DUPN, SWAPN, EXCHANGE opcodes) | boundary `a122dbe45` | 4 (v1.17.0 3/12) | `core/vm/eips.go` (activator), `core/vm/jump_table.go` | registered in the `activators` map only; **not wired into any fork instruction set**; reachable solely via `ChainConfig.ExtraEips`, which is unset on every Bor preset | **defer** — dormant, not adopted | yes — `enable8024` is never invoked by any `new*InstructionSet()` (0 refs in `jump_table.go`); no Bor preset sets `ExtraEips`, so the DUPN/SWAPN/EXCHANGE opcodes are absent from every Bor network's jump table. Not ungated. |
| EIP-8024 PC-increment fix (#33361) | boundary `228933a66` | 5 (v1.17.0 4/12) | `core/vm/instructions.go`, `core/vm/instructions_test.go` | same as above — no gate change; corrects the opcode implementation only | **defer** (unchanged) | yes — re-confirmed `enable8024` still unreferenced by any instruction set and no preset sets `ExtraEips`. The fix corrects the (still-dormant) DUPN/SWAPN/EXCHANGE PC handling; it does not enable them. Auto-merged clean. |
| EIP-8024 missing-immediate-byte update (#33614) | boundary `845009f68` | 10 (v1.17.0 9/12) | `core/vm/instructions.go`, `core/vm/instructions_test.go` | same as above — no gate change; spec-clarification only (missing immediate byte after DUPN/SWAPN/EXCHANGE now treated as `0x00`, matching PUSHn, instead of `ErrInvalidOpCode`) | **defer** (unchanged) | yes — re-confirmed `enable8024` registered only in the `activators` map (`eips.go:46`), never invoked by any `new*InstructionSet()`; no Bor preset sets `ExtraEips`. The opcodes stay absent from every Bor network's jump table; the behavior change is unreachable. Auto-merged clean; `TestEIP8024_Execution` (incl. new `DUPN_MISSING_IMMEDIATE`) passes. |
| Fulu (Ethereum CL fork) beacon types (#33349) | boundary `228933a66` | 5 (v1.17.0 4/12) | `beacon/types/beacon_block.go`, `beacon/types/exec_header.go` | n/a — CL block/header type definitions; not a Bor execution-layer fork gate | **already-ours / inert** | yes — Fulu is an Ethereum consensus-layer fork; `beacon/types` carries CL block/header shapes used by the beacon light client, not Bor's PoA consensus path. No `params/config.go` gate, no execution behavior. Auto-merged clean, inert for Bor networks. |
| Catalyst getPayload fork-timestamp checks (#32754) | boundary `228933a66` | 5 (v1.17.0 4/12) | `eth/catalyst/api.go` | n/a — Engine API `engine_getPayloadVX` version guard against configured fork timestamps | **already-ours / inert** | yes — the Engine API (`eth/catalyst`) is inherited-but-unused in Bor's PoA consensus (per repo `CLAUDE.md`). Adds a version-vs-fork-timestamp guard on getPayload; introduces no new fork and flips no gate. Auto-merged clean; the only catalyst conflicts were sync-mode (`ConfigSyncMode`→`api.eth.SyncMode()`), unrelated to this guard. |

## Notes

- **go-verkle scaffolding REMOVED (adopted #33461)** — **decided 2026-07-23 (user-approved).**
  go-ethereum abandoned the go-verkle/go-ipa verkle implementation (#33461), and Bor's
  verkle was never enabled (`VerkleBlock=nil`). Rather than carry orphaned scaffolding
  and re-revert #33461 every batch, Bor **mirrors upstream's removal patch-by-patch**:
  a dedicated commit on top of batch 6 (`85a1750c5`) mirrors #33461 — deletes
  `trie/verkle*.go`, `trie/utils/verkle.go` (incl. `PointCache`), `cmd/geth/verkle.go`;
  removes the `ExecutionWitness` verkle fields from `core/types/block.go`, `PointCache`
  from the `state.Database`/`vm.StateDB` interfaces + all `core/state` implementers
  (incl. Bor's `ParallelStateDB`), `NewAccessEvents()` drops its `PointCache` arg,
  `AccessEvents` key derivation switches to `bintrie` (binary tree kept); drops
  go-verkle/go-ipa from `go.{mod,sum}` + `cmd/keeper`; removes `verkleCommand`.
  **Kept:** the Verkle **fork gate** (`VerkleBlock`/`IsVerkle`/`OverrideVerkle`,
  dormant) — upstream also keeps `isVerkle` vars per #33461's author note; and Bor's
  stateless client (`stateless.Witness`, distinct from the verkle `ExecutionWitness`).
  **Going forward:** upstream's follow-up verkle stub PRs are mirrored as they arrive
  in later batches (Bor tracks upstream's removal trajectory, staying in lockstep).
- **v1.17.0 batch 4/12 (`5dfcffcf3`, batch 5)** — no hardfork/EIP introduced.
  #33352 removed the blobpool's Osaka legacy-blob-sidecar conversion machinery;
  Osaka remains **dormant** in Bor (`IsOsaka(num)` block-based, unset on every
  preset), so this is dead-code cleanup, not a gate change. The blob-sidecar
  version check in `core/txpool/validation.go` was kept on Bor's block-based
  `IsOsaka(head.Number)`. Fulu (`beacon/types`, #33349) is an Ethereum CL fork,
  inert for Bor's PoA path.
- **v1.17.0 batch 9/12 (`845009f68`, batch 10)** — no hardfork/EIP introduced.
  One fork surface touched, dormant: EIP-8024 missing-immediate-byte update
  (#33614, `251b86310`) refines the DUPN/SWAPN/EXCHANGE opcode behavior but
  changes no gate — `enable8024` remains `ExtraEips`-only (unset on every Bor
  preset), so the opcodes stay unreachable (see table row). The batch's two real
  features (eth_getProofs-for-history #32727, callTracer log index #33629) are
  RPC/tracing, not forks; the adopted trienode `FullValueCheckpoint` /
  `NodeFullValueCheckpoint` knob is a storage-compression tuning parameter
  (dormant while `TrienodeHistory = -1`), not a fork. The deferred code-stats
  (#33659) and slow-block (#33655) lines touch no gate. Nothing enters the
  register.
- **v1.17.0 batch 10/12 (`c12959dc8`, batch 11)** — no hardfork/EIP introduced;
  no fork gating, precompile set, or gas schedule changed. EIP-8024's only touch
  this batch is test-only (#33785 `bc0db302e` adds a PUSH0 handler to the EIP-8024
  *test* mini-interpreter in `core/vm/instructions_test.go`); `enable8024` stays
  `ExtraEips`-only and dormant. The `params.Rules` change was an upstream refactor
  (ChainID field removal + timestamp param name), reconciled preserving Bor's
  block-based `Is<Fork>(num)` gating and Bor fork bools — no activation moved, and
  Bor's `ChainID` field was retained (see ledger + `TestReinforceMultiClientPreCompilesTest`).
  The `crypto/keccak` vendoring, the `vm.TxContext.GasPrice`/`stateObject.addrHash`
  type migrations, and the metrics/rlp/eip4844-API reworks are non-fork. Nothing
  enters the register.
- **v1.17.0 batch 12/12 (`0cf3d3ba4`, batch 13, v1.17.0 release)** — no hardfork/EIP
  introduced. `#33860` (consensus/misc header hardening — defensive parent-baseFee
  nil-check in `VerifyEIP1559Header`) is **already present in Bor HEAD** (`eip1559.go:57`),
  merged as a no-op (verified present, not dropped — it's consensus-security). `9b78f45e3`
  (secp256k1 coordinate check) is the **v1.16.9 duplicate** the plan flagged — already
  applied in the v1.16.9 milestone (`a06dbd7d2`), comment-only conflict, took ours. The
  batch's two large features (OTel newPayload #33521, delayed p2p decoding #33835) are
  declined/deferred and non-fork; `#33842` (download progress bar) is non-fork. Nothing
  enters the register. **All v1.17.0 EIP surfaces remain dormant** (EIP-8024 `ExtraEips`-only,
  Verkle gate nil).
- **v1.17.0 batch 11/12 (`c50e5edfa`, batch 12)** — no hardfork/EIP introduced;
  no fork gating, precompile set, or gas schedule changed. EIP-8024's only touch
  this batch is test-only (#33787 — the DUPN/SWAPN/EXCHANGE test table in
  `core/vm/instructions_test.go` now asserts explicit error *types* + expected
  opcode instead of `wantErr bool`); `enable8024` stays `ExtraEips`-only and
  dormant, no production `core/vm`/`params` code touched. The batch's two large
  features (OpenTelemetry CLI wiring #33484/#33772, EraE format #32157) are
  declined/deferred and non-fork; the adopted fixes (trie/rlp/rawdb/pathdb/
  gethclient/node) are non-fork. Nothing enters the register.
- **v1.17.0 batch 8/12 (`8fad02ac6`, batch 9)** — no hardfork/EIP introduced.
  This batch touched `core/vm` gas/opcode surfaces via four upstream PRs, all
  **deferred** to Bor HEAD (see `needs-wiring.md`), so Bor's EVM behavior is
  unchanged and no gate is affected: `#33281`/`#33637` relocate EIP-214
  write-protection into the gas handlers (unconditional, no fork gate);
  `#33450` is an EIP-2929 cold-access gas early-return (existing fork, no new
  gate); `#32919` reworks selfdestruct balance handling + tracer hooks
  (net-state-neutral, clashes with Bor's BlockSTM `SelfDestruct`). None
  introduces or enables a fork; nothing enters the register. Trienode-history
  (`#32621`/`#33551`/`#33584`) was adopted but is a storage feature, not a
  fork — defaulted to `-1` (disabled).
- **v1.17.0 batch 7/12 (`94710f79a`, batch 8)** — no hardfork/EIP introduced.
  Three fork-adjacent commits, none of which touch a fork gate: `5b99d2bba`
  (drop peers on invalid KZG proofs — txpool peer-drop robustness, no gate),
  `b993cb6f3` #32717 (allow gaps in blobpool — queue behavior, no gate), and
  `a32851fac` #33542 (graphql GasPrice for blob/setcode txs — RPC formatting).
  Osaka/blob forks remain dormant (`IsOsaka(num)` block-based, unset on every
  Bor preset). The batch's large state-tracking PRs (#33490 state update hook,
  #33532 code-reader cache stats) introduce no fork gating and were deferred
  wholesale to Bor HEAD (see `needs-wiring.md`), so nothing enters the register.
- **v1.16.9 (batch 1)** — crypto security backports (ECIES invalid-curve,
  secp256k1 coordinate check). No hardfork/EIP activation; nothing to register.
- **v1.17.0 batch 1/12 (`f23d506b7`, batch 2)** — bugfix sweep (rawdb / state /
  pathdb / RPC / blobpool / filters). No hardfork/EIP introduced; nothing to
  register.
- **Upcoming (from the plan's risk flags), all to be entered dormant/disabled
  unless the PoS team decides otherwise:** EIP-8024 (batches 4, 10, 15),
  EIP-7843 SLOTNUM (15), EIP-7778 (16), EIP-7954 (17), EIP-7708 (19), EIP-7976
  (23), EIP-7981 (25), EIP-8037 (32), Amsterdam jump-table / precompile wiring
  (14–16), Osaka/BPO blob-schedule forks, gas-vector / gas-budget reworks
  (21, 23), jumpdest cache (29).
