package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// witnessDir is resolved relative to this source file at init time.
var witnessDir string

func init() {
	_, thisFile, _, _ := runtime.Caller(0)
	witnessDir = filepath.Join(filepath.Dir(thisFile), "blockstm", "testdata")
}

var testBlockHexes = []string{
	"0x4EC6D10", "0x4EC6D11", "0x4EC6D12", "0x4EC6D13",
	"0x4EC6D14", "0x4EC6D15", "0x4EC6D16", "0x4EC6D17", "0x4EC6D18",
}

// benchConsensus is a minimal consensus engine for benchmarking.
// It skips Heimdall-dependent operations (state sync, span commit).
type benchConsensus struct{}

func (b *benchConsensus) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

func (b *benchConsensus) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return nil
}

func (b *benchConsensus) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	for range headers {
		results <- nil
	}
	return abort, results
}

func (b *benchConsensus) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	return nil
}

func (b *benchConsensus) Prepare(chain consensus.ChainHeaderReader, header *types.Header, waitOnPrepare bool) error {
	return nil
}

func (b *benchConsensus) Finalize(chain consensus.ChainHeaderReader, header *types.Header, stateDB vm.StateDB, body *types.Body, receipts []*types.Receipt) ([]*types.Receipt, error) {
	return receipts, nil
}

func (b *benchConsensus) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, stateDB *state.StateDB, body *types.Body, receipts []*types.Receipt) (*types.Block, []*types.Receipt, time.Duration, error) {
	return types.NewBlock(header, body, receipts, trie.NewStackTrie(nil)), receipts, 0, nil
}

func (b *benchConsensus) Seal(chain consensus.ChainHeaderReader, block *types.Block, witness *stateless.Witness, results chan<- *consensus.NewSealedBlockEvent, stop <-chan struct{}) error {
	return nil
}

func (b *benchConsensus) SealHash(header *types.Header) common.Hash {
	return header.Hash()
}

func (b *benchConsensus) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return big.NewInt(1)
}

func (b *benchConsensus) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return nil
}

func (b *benchConsensus) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// JSON types for witness and block parsing
// ---------------------------------------------------------------------------

type rpcJSONResult struct {
	Result json.RawMessage `json:"result"`
}

type witnessJSON struct {
	Context    json.RawMessage   `json:"context"`
	Headers    []json.RawMessage `json:"headers"`
	Codes      []hexutil.Bytes   `json:"codes"`
	State      []hexutil.Bytes   `json:"state"`
	PreState   common.Hash       `json:"preStateRoot"`
	CodesCount int               `json:"codesCount"`
	StateCount int               `json:"stateNodesCount"`
}

type blockJSON struct {
	Transactions []json.RawMessage `json:"transactions"`
}

// ---------------------------------------------------------------------------
// Witness / block loading
// ---------------------------------------------------------------------------

// errLFSPointer is returned when a file under core/blockstm/testdata/ is
// still a Git LFS pointer (text stub) rather than the actual data blob —
// i.e. the contributor or CI runner hasn't run `git lfs pull`. Callers
// should treat this as "skip the test, fixtures unavailable" rather than
// fail the run.
var errLFSPointer = errors.New("git LFS pointer file (run `git lfs pull` to materialize testdata)")

// isLFSPointer reports whether b looks like a Git LFS pointer file. The
// canonical first line is `version https://git-lfs.github.com/spec/v1`,
// so checking the leading "version " prefix is enough.
func isLFSPointer(b []byte) bool {
	const lfsPrefix = "version https://git-lfs.github.com/spec/"
	return len(b) >= len(lfsPrefix) && string(b[:len(lfsPrefix)]) == lfsPrefix
}

// readFileMaybeGz reads a file, decompressing if it ends with .gz. Returns
// errLFSPointer (wrapped) if the file is a Git LFS pointer rather than
// real content — common on fresh clones / CI runners that haven't pulled
// LFS yet.
func readFileMaybeGz(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if isLFSPointer(raw) {
		return nil, fmt.Errorf("%s: %w", path, errLFSPointer)
	}

	if strings.HasSuffix(path, ".gz") {
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		return io.ReadAll(gr)
	}
	return raw, nil
}

func loadWitnessFromJSON(path string) (*stateless.Witness, error) {
	data, err := readFileMaybeGz(path)
	if err != nil {
		return nil, fmt.Errorf("reading witness file: %w", err)
	}

	var rpcResp rpcJSONResult
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return nil, fmt.Errorf("parsing JSON-RPC envelope: %w", err)
	}

	var wj witnessJSON
	if err := json.Unmarshal(rpcResp.Result, &wj); err != nil {
		return nil, fmt.Errorf("parsing witness result: %w", err)
	}

	var contextHeader types.Header
	if err := json.Unmarshal(wj.Context, &contextHeader); err != nil {
		return nil, fmt.Errorf("parsing context header: %w", err)
	}

	headers := make([]*types.Header, len(wj.Headers))
	for i, raw := range wj.Headers {
		var h types.Header
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("parsing header %d: %w", i, err)
		}
		headers[i] = &h
	}

	stateMap := make(map[string]struct{}, len(wj.State))
	for _, node := range wj.State {
		stateMap[string(node)] = struct{}{}
	}

	codesMap := make(map[string]struct{}, len(wj.Codes))
	for _, code := range wj.Codes {
		codesMap[string(code)] = struct{}{}
	}

	contextHeader.Root = common.Hash{}
	contextHeader.ReceiptHash = common.Hash{}

	witness, err := stateless.NewWitness(&contextHeader, nil)
	if err != nil {
		return nil, fmt.Errorf("creating witness: %w", err)
	}
	witness.Headers = headers
	witness.Codes = codesMap
	witness.State = stateMap

	return witness, nil
}

var rpcClient = &http.Client{Timeout: 120 * time.Second}

func alchemyRPC(url string, method string, params []any) (json.RawMessage, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})

	resp, err := rpcClient.Post(url, "application/json", bytes.NewReader(reqBody)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("RPC request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var rpcResp rpcJSONResult
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parsing RPC response: %w", err)
	}

	return rpcResp.Result, nil
}

func fetchAndCacheBlock(blockHex string, alchemyURL string) ([]byte, error) {
	cachePath := filepath.Join(witnessDir, blockHex+".block")
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	if alchemyURL == "" {
		return nil, fmt.Errorf("block %s not cached and ALCHEMY_URL not set", blockHex)
	}

	result, err := alchemyRPC(alchemyURL, "eth_getBlockByNumber", []any{blockHex, true})
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(cachePath, result, 0644); err != nil {
		return nil, fmt.Errorf("caching block: %w", err)
	}

	return result, nil
}

func parseBlockFromJSON(data []byte) (*types.Block, common.Hash, common.Hash, error) {
	var header types.Header
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, common.Hash{}, common.Hash{}, fmt.Errorf("parsing block header: %w", err)
	}

	origStateRoot := header.Root
	origReceiptHash := header.ReceiptHash

	var bj blockJSON
	if err := json.Unmarshal(data, &bj); err != nil {
		return nil, common.Hash{}, common.Hash{}, fmt.Errorf("parsing block transactions: %w", err)
	}

	txs := make([]*types.Transaction, 0, len(bj.Transactions))
	for i, raw := range bj.Transactions {
		var tx types.Transaction
		if err := json.Unmarshal(raw, &tx); err != nil {
			return nil, common.Hash{}, common.Hash{}, fmt.Errorf("parsing tx %d: %w", i, err)
		}
		txs = append(txs, &tx)
	}

	header.Root = common.Hash{}
	header.ReceiptHash = common.Hash{}

	block := types.NewBlockWithHeader(&header).WithBody(types.Body{
		Transactions: txs,
	})

	return block, origStateRoot, origReceiptHash, nil
}

func fetchAndCacheCode(addr common.Address, blockHex string, alchemyURL string) ([]byte, error) {
	codeDir := filepath.Join(witnessDir, "codes")
	os.MkdirAll(codeDir, 0755) //nolint:errcheck

	cachePath := filepath.Join(codeDir, addr.Hex()+"-"+blockHex+".bin")
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	if alchemyURL == "" {
		return nil, fmt.Errorf("code for %s not cached and ALCHEMY_URL not set", addr.Hex())
	}

	result, err := alchemyRPC(alchemyURL, "eth_getCode", []any{addr.Hex(), blockHex})
	if err != nil {
		return nil, err
	}

	var codeHex string
	if err := json.Unmarshal(result, &codeHex); err != nil {
		return nil, fmt.Errorf("parsing code response: %w", err)
	}

	code := common.FromHex(codeHex)
	if err := os.WriteFile(cachePath, code, 0644); err != nil {
		return nil, fmt.Errorf("caching code: %w", err)
	}

	return code, nil
}

func prewarmCodes(diskdb ethdb.Database, witness *stateless.Witness, block *types.Block, _ string, _ *params.ChainConfig, alchemyURL string) error {
	memdb := witness.MakeHashDB(diskdb)
	db, err := state.New(witness.Root(), state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
	if err != nil {
		return fmt.Errorf("opening state: %w", err)
	}

	parentBlockNum := new(big.Int).Sub(witness.Header().Number, big.NewInt(1))
	parentBlockHex := fmt.Sprintf("0x%x", parentBlockNum)

	seen := make(map[common.Address]bool)
	emptyCodeHash := crypto.Keccak256Hash(nil)

	checkAndFetch := func(addr common.Address) error {
		if seen[addr] {
			return nil
		}
		seen[addr] = true

		codeHash := db.GetCodeHash(addr)
		if codeHash == (common.Hash{}) || codeHash == emptyCodeHash {
			return nil
		}

		if existing := rawdb.ReadCode(diskdb, codeHash); len(existing) > 0 {
			return nil
		}

		code, err := fetchAndCacheCode(addr, parentBlockHex, alchemyURL)
		if err != nil {
			return fmt.Errorf("fetching code for %s: %w", addr.Hex(), err)
		}
		if got := crypto.Keccak256Hash(code); got != codeHash {
			return fmt.Errorf("code for %s at %s hashes to %x, account expects %x", addr.Hex(), parentBlockHex, got, codeHash)
		}

		if len(code) > 0 {
			rawdb.WriteCode(diskdb, codeHash, code)
		}

		return nil
	}

	for _, tx := range block.Transactions() {
		if tx.To() != nil {
			if err := checkAndFetch(*tx.To()); err != nil {
				return err
			}
		}
		for _, auth := range tx.SetCodeAuthorizations() {
			authority, err := auth.Authority()
			if err != nil {
				continue
			}
			if err := checkAndFetch(authority); err != nil {
				return err
			}
		}
	}

	return nil
}

type codeCachingDB struct {
	ethdb.Database
	codeDir string
}

func newCodeCachingDB(codeDir string) *codeCachingDB {
	return &codeCachingDB{
		Database: rawdb.NewMemoryDatabase(),
		codeDir:  codeDir,
	}
}

// supplementalCodes are EIP-7702 delegation designators that the dataset builder
// missed: prewarmCodes originally fetched code only for tx.To() addresses, never
// for authorities, and cached by address without a block dimension, so an
// authority re-delegated between dataset blocks kept a stale designator. These
// are the pre-block designators of authorities re-delegated in blocks 83014074,
// 83014100 and 83020871, keyed by content hash like every other code blob.
var supplementalCodes = []string{
	"ef0100bee2f0de34362ff34a9189c2b2e67ed61a3300e9",
	"ef010089248eec91b1436da005c405eb5fbcb0d0088e1e",
	"ef0100879a23a785796bfca1d22f48c91818e1157a01b4",
}

func (db *codeCachingDB) loadCodesFromDisk() error {
	// Preferred path: a single gzipped tar archive (codes.tar.gz) stored next to
	// the codes directory. This avoids having tens of thousands of individual
	// files in the repo. Falls back to (and additionally loads) loose .bin files
	// for entries that were cached at runtime via fetchAndCacheCode but haven't
	// been rolled into the archive yet.
	parent := filepath.Dir(db.codeDir)
	archivePath := filepath.Join(parent, "codes.tar.gz")
	if f, err := os.Open(archivePath); err == nil {
		defer f.Close()
		if err := loadCodesFromTarGz(db.Database, f); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(db.codeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".bin" {
			continue
		}
		code, err := os.ReadFile(filepath.Join(db.codeDir, entry.Name()))
		if err != nil {
			continue
		}
		if len(code) > 0 {
			codeHash := crypto.Keccak256Hash(code)
			rawdb.WriteCode(db.Database, codeHash, code)
		}
	}
	for _, hexCode := range supplementalCodes {
		code := common.FromHex(hexCode)
		rawdb.WriteCode(db.Database, crypto.Keccak256Hash(code), code)
	}
	return nil
}

// loadCodesFromTarGz streams a codes.tar.gz archive and writes each entry as a
// keccak256-keyed code blob into db. Directory entries and zero-length files
// are skipped.
func loadCodesFromTarGz(db ethdb.Database, r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("codes tar.gz: gzip open: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("codes tar.gz: read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		code, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("codes tar.gz: read %s: %w", hdr.Name, err)
		}
		if len(code) == 0 {
			continue
		}
		rawdb.WriteCode(db, crypto.Keccak256Hash(code), code)
	}
}

// ---------------------------------------------------------------------------
// Test data types and preparation
// ---------------------------------------------------------------------------

type testBlockData struct {
	witness     *stateless.Witness
	block       *types.Block
	stateRoot   common.Hash
	receiptRoot common.Hash
}

type preparedBlock struct {
	block       *types.Block
	witness     *stateless.Witness
	memdb       ethdb.Database
	tdb         *triedb.Database
	baseState   *state.StateDB
	headerCache *lru.Cache[common.Hash, *types.Header]
	stateRoot   common.Hash
	receiptRoot common.Hash
	author      common.Address
}

func prepareBlocks(blocks []testBlockData, diskdb ethdb.Database, config *params.ChainConfig) []preparedBlock {
	prepared := make([]preparedBlock, len(blocks))
	for i, bd := range blocks {
		memdb := bd.witness.MakeHashDB(diskdb)
		tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
		root := bd.witness.Root()
		db, err := state.New(root, state.NewDatabase(tdb, nil))
		if err != nil {
			panic(fmt.Sprintf("state.New for block %d: %v", i, err))
		}
		hc := lru.NewCache[common.Hash, *types.Header](256)
		for _, h := range bd.witness.Headers {
			hc.Add(h.Hash(), h)
		}
		prepared[i] = preparedBlock{
			block:       bd.block,
			witness:     bd.witness,
			memdb:       memdb,
			tdb:         tdb,
			baseState:   db,
			headerCache: hc,
			stateRoot:   bd.stateRoot,
			receiptRoot: bd.receiptRoot,
			author:      getAuthor(config, bd.witness.Header()),
		}
	}
	return prepared
}

var benchVMConfig = vm.Config{
	EnableEVMSwitchDispatch: true,
}

func processSerial(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine) (*ProcessResult, error) {
	db := pb.baseState.Copy()
	hc := &HeaderChain{
		config:      config,
		chainDb:     pb.memdb,
		headerCache: pb.headerCache,
		engine:      engine,
	}
	return NewStateProcessor(hc).Process(pb.block, db, benchVMConfig, &pb.author, context.Background())
}

func processParallel(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine, numProcs int) (*ProcessResult, error) {
	db := pb.baseState.Copy()
	bc := &BlockChain{
		chainConfig:                  config,
		hc:                           &HeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine},
		parallelSpeculativeProcesses: numProcs,
	}
	hc := &benchHeaderChain{
		config:      config,
		chainDb:     pb.memdb,
		headerCache: pb.headerCache,
		engine:      engine,
	}
	return NewParallelStateProcessor(hc, bc).Process(pb.block, db, vm.Config{}, &pb.author, context.Background())
}

func processV2(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine, numWorkers int) (*ProcessResult, *state.StateDB, error) {
	db := pb.baseState.Copy()
	bc := &BlockChain{
		chainConfig: config,
		hc:          &HeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine},
	}
	hc := &benchHeaderChain{
		config:      config,
		chainDb:     pb.memdb,
		headerCache: pb.headerCache,
		engine:      engine,
	}
	res, err := NewV2StateProcessor(hc, bc, numWorkers).Process(pb.block, db, benchVMConfig, &pb.author, context.Background())
	return res, db, err
}

// ---------------------------------------------------------------------------
// Execution helpers
// ---------------------------------------------------------------------------

func executeStatelessSerial(config *params.ChainConfig, block *types.Block, witness *stateless.Witness, author *common.Address, engine consensus.Engine, diskdb ethdb.Database) (common.Hash, common.Hash, *ProcessResult, error) {
	memdb := witness.MakeHashDB(diskdb)
	db, err := state.New(witness.Root(), state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
	if err != nil {
		return common.Hash{}, common.Hash{}, nil, err
	}

	headerChain := &HeaderChain{
		config:      config,
		chainDb:     memdb,
		headerCache: lru.NewCache[common.Hash, *types.Header](256),
		engine:      engine,
	}
	processor := NewStateProcessor(headerChain)

	res, err := processor.Process(block, db, vm.Config{}, author, context.Background())
	if err != nil {
		return common.Hash{}, common.Hash{}, nil, err
	}
	if err := db.Error(); err != nil {
		return common.Hash{}, common.Hash{}, nil, fmt.Errorf("serial base read: %w", err)
	}

	receiptRoot := types.DeriveSha(res.Receipts, trie.NewStackTrie(nil))
	stateRoot := db.IntermediateRoot(config.IsEIP158(block.Number()))
	return stateRoot, receiptRoot, res, nil
}

type benchHeaderChain struct {
	config      *params.ChainConfig
	chainDb     ethdb.Database
	headerCache *lru.Cache[common.Hash, *types.Header]
	engine      consensus.Engine
}

func (hc *benchHeaderChain) Config() *params.ChainConfig                    { return hc.config }
func (hc *benchHeaderChain) CurrentHeader() *types.Header                   { return nil }
func (hc *benchHeaderChain) GetHeaderByNumber(number uint64) *types.Header  { return nil }
func (hc *benchHeaderChain) GetHeaderByHash(hash common.Hash) *types.Header { return nil }
func (hc *benchHeaderChain) GetTd(hash common.Hash, number uint64) *big.Int { return nil }
func (hc *benchHeaderChain) Engine() consensus.Engine                       { return hc.engine }

func (hc *benchHeaderChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if header, ok := hc.headerCache.Get(hash); ok {
		return header
	}
	// Fall back to rawdb (needed for BLOCKHASH opcode resolution).
	header := rawdb.ReadHeader(hc.chainDb, hash, number)
	if header != nil {
		hc.headerCache.Add(hash, header)
	}
	return header
}

func executeStatelessParallel(config *params.ChainConfig, block *types.Block, witness *stateless.Witness, author *common.Address, engine consensus.Engine, diskdb ethdb.Database, numProcs int) (common.Hash, common.Hash, *ProcessResult, error) {
	memdb := witness.MakeHashDB(diskdb)
	db, err := state.New(witness.Root(), state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
	if err != nil {
		return common.Hash{}, common.Hash{}, nil, err
	}

	hc := &benchHeaderChain{
		config:      config,
		chainDb:     memdb,
		headerCache: lru.NewCache[common.Hash, *types.Header](256),
		engine:      engine,
	}

	for _, h := range witness.Headers {
		hc.headerCache.Add(h.Hash(), h)
	}

	bc := &BlockChain{
		chainConfig:                  config,
		hc:                           &HeaderChain{config: config, chainDb: memdb, headerCache: hc.headerCache, engine: engine},
		parallelSpeculativeProcesses: numProcs,
	}

	processor := NewParallelStateProcessor(hc, bc)

	res, err := processor.Process(block, db, vm.Config{}, author, context.Background())
	if err != nil {
		return common.Hash{}, common.Hash{}, nil, err
	}

	receiptRoot := types.DeriveSha(res.Receipts, trie.NewStackTrie(nil))
	stateRoot := db.IntermediateRoot(config.IsEIP158(block.Number()))
	return stateRoot, receiptRoot, res, nil
}

func getAlchemyURL(t testing.TB) string {
	t.Helper()
	return os.Getenv("ALCHEMY_URL") // empty string is fine if all data is cached
}

func getAuthor(config *params.ChainConfig, header *types.Header) common.Address {
	if config.Bor != nil && config.Bor.IsRio(header.Number) {
		coinbase := common.HexToAddress(config.Bor.CalculateCoinbase(header.Number.Uint64()))
		if coinbase != (common.Address{}) {
			return coinbase
		}
	}
	return header.Coinbase
}

// ---------------------------------------------------------------------------
// Block loading
// ---------------------------------------------------------------------------

func loadTestBlocks(t testing.TB, alchemyURL string) ([]testBlockData, ethdb.Database) {
	t.Helper()

	if _, err := os.Stat(witnessDir); os.IsNotExist(err) {
		t.Skipf("witness directory %s not found", witnessDir)
	}

	codeDir := filepath.Join(witnessDir, "codes")
	diskdb := newCodeCachingDB(codeDir)
	diskdb.loadCodesFromDisk() //nolint:errcheck

	var blocks []testBlockData

	for _, blockHex := range testBlockHexes {
		witnessPath := filepath.Join(witnessDir, blockHex+".witness")
		if _, err := os.Stat(witnessPath); os.IsNotExist(err) {
			t.Skipf("witness file %s not found", witnessPath)
		}

		witness, err := loadWitnessFromJSON(witnessPath)
		if err != nil {
			t.Fatalf("loading witness %s: %v", blockHex, err)
		}

		blockData, err := fetchAndCacheBlock(blockHex, alchemyURL)
		if err != nil {
			t.Fatalf("fetching block %s: %v", blockHex, err)
		}

		block, stateRoot, receiptRoot, err := parseBlockFromJSON(blockData)
		if err != nil {
			t.Fatalf("parsing block %s: %v", blockHex, err)
		}

		if err := prewarmCodes(diskdb, witness, block, blockHex, params.BorMainnetChainConfig, alchemyURL); err != nil {
			t.Logf("warning: prewarm codes for %s: %v", blockHex, err)
		}

		blocks = append(blocks, testBlockData{
			witness:     witness,
			block:       block,
			stateRoot:   stateRoot,
			receiptRoot: receiptRoot,
		})

		t.Logf("loaded block %s: %d txs, %d gas", blockHex, len(block.Transactions()), block.GasUsed())
	}

	return blocks, diskdb
}

// embeddedBlockHexes are the 9 representative blocks used for quick embedded tests.
// These blocks were specifically selected for high contention (USDC DEX swaps).
var embeddedBlockHexes = []string{
	"0x4EC6D13", "0x4EC6D15", "0x4EC6D16",
	"0x4F2B1C8", "0x4F2B1C9",
	"0x4F2C022", "0x4F2C03F",
	"0x4F2CC35", "0x4F2CC6A",
}

// loadEmbeddedBlocks loads only the 9 representative witness blocks for quick tests.
func loadEmbeddedBlocks(t testing.TB) ([]testBlockData, ethdb.Database) {
	t.Helper()

	codeDir := filepath.Join(witnessDir, "codes")
	diskdb := newCodeCachingDB(codeDir)
	diskdb.loadCodesFromDisk() //nolint:errcheck

	var blocks []testBlockData
	for _, blockHex := range embeddedBlockHexes {
		witnessPath := filepath.Join(witnessDir, blockHex+".witness.gz")
		if _, err := os.Stat(witnessPath); os.IsNotExist(err) {
			witnessPath = filepath.Join(witnessDir, blockHex+".witness")
		}
		witness, err := loadWitnessFromJSON(witnessPath)
		if err != nil {
			if errors.Is(err, errLFSPointer) {
				t.Skipf("testdata not materialized: %v", err)
			}
			t.Fatalf("loading witness %s: %v", blockHex, err)
		}
		blockPath := filepath.Join(witnessDir, blockHex+".block")
		blockData, err := os.ReadFile(blockPath)
		if err != nil {
			t.Fatalf("loading block %s: %v", blockHex, err)
		}
		if isLFSPointer(blockData) {
			t.Skipf("testdata not materialized: %s: %v", blockPath, errLFSPointer)
		}
		block, stateRoot, receiptRoot, err := parseBlockFromJSON(blockData)
		if err != nil {
			t.Fatalf("parsing block %s: %v", blockHex, err)
		}
		blocks = append(blocks, testBlockData{
			witness: witness, block: block,
			stateRoot: stateRoot, receiptRoot: receiptRoot,
		})
	}
	t.Logf("loaded %d blocks from %s", len(blocks), witnessDir)
	return blocks, diskdb
}

func loadBlocksFromDir(t testing.TB, dir string, alchemyURL string) ([]testBlockData, ethdb.Database) {
	t.Helper()

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skipf("witness directory %s not found", dir)
	}

	codeDir := filepath.Join(witnessDir, "codes")
	diskdb := newCodeCachingDB(codeDir)
	diskdb.loadCodesFromDisk() //nolint:errcheck

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading directory %s: %v", dir, err)
	}

	var blocks []testBlockData

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		// Support both plain (.witness) and gzipped (.witness.gz) formats
		var blockHex string
		if strings.HasSuffix(name, ".witness.gz") {
			blockHex = strings.TrimSuffix(name, ".witness.gz")
		} else if strings.HasSuffix(name, ".witness") {
			blockHex = strings.TrimSuffix(name, ".witness")
		} else {
			continue
		}

		witnessPath := filepath.Join(dir, name)
		witness, err := loadWitnessFromJSON(witnessPath)
		if err != nil {
			if errors.Is(err, errLFSPointer) {
				t.Skipf("testdata not materialized: %v", err)
			}
			t.Fatalf("loading witness %s: %v", blockHex, err)
		}

		// Try block file: .block first, then fetch from RPC
		var blockData []byte
		if data, err := os.ReadFile(filepath.Join(dir, blockHex+".block")); err == nil {
			if isLFSPointer(data) {
				t.Skipf("testdata not materialized: %s: %v", filepath.Join(dir, blockHex+".block"), errLFSPointer)
			}
			blockData = data
		} else if data, err := fetchAndCacheBlock(blockHex, alchemyURL); err == nil {
			blockData = data
		} else {
			t.Fatalf("loading block %s: %v", blockHex, err)
		}

		block, stateRoot, receiptRoot, err := parseBlockFromJSON(blockData)
		if err != nil {
			t.Fatalf("parsing block %s: %v", blockHex, err)
		}

		if err := prewarmCodes(diskdb, witness, block, blockHex, params.BorMainnetChainConfig, alchemyURL); err != nil {
			t.Logf("warning: prewarm codes for %s: %v", blockHex, err)
		}

		blocks = append(blocks, testBlockData{
			witness:     witness,
			block:       block,
			stateRoot:   stateRoot,
			receiptRoot: receiptRoot,
		})
	}

	t.Logf("loaded %d blocks from %s", len(blocks), dir)

	return blocks, diskdb
}

// ---------------------------------------------------------------------------
// Consistency tests
// ---------------------------------------------------------------------------

// TestMainnetWitnessLoad verifies that witness files can be loaded.
func TestMainnetWitnessLoad(t *testing.T) {
	alchemyURL := getAlchemyURL(t)
	blocks, _ := loadTestBlocks(t, alchemyURL)
	t.Logf("loaded %d blocks", len(blocks))
}

// TestMainnetWitnessSerial verifies serial execution produces valid state.
func TestMainnetWitnessSerial(t *testing.T) {
	alchemyURL := getAlchemyURL(t)
	blocks, diskdb := loadTestBlocks(t, alchemyURL)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	for i, bd := range blocks {
		author := getAuthor(config, bd.witness.Header())
		stateRoot, receiptRoot, _, err := executeStatelessSerial(config, bd.block, bd.witness, &author, engine, diskdb)
		if err != nil {
			t.Fatalf("block %s: %v", testBlockHexes[i], err)
		}
		t.Logf("block %s: state=%s receipt=%s", testBlockHexes[i], stateRoot.Hex()[:10], receiptRoot.Hex()[:10])
	}
}

// TestBaselineConsistency compares serial vs baseline parallel (abort-and-retry BlockSTM).
func TestBaselineConsistency(t *testing.T) {
	alchemyURL := getAlchemyURL(t)
	blocks, diskdb := loadTestBlocks(t, alchemyURL)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	numProcs := runtime.NumCPU()

	for i, bd := range blocks {
		author := getAuthor(config, bd.witness.Header())

		serialState, serialReceipt, _, err := executeStatelessSerial(config, bd.block, bd.witness, &author, engine, diskdb)
		if err != nil {
			t.Fatalf("block %s serial: %v", testBlockHexes[i], err)
		}

		parallelState, parallelReceipt, _, err := executeStatelessParallel(config, bd.block, bd.witness, &author, engine, diskdb, numProcs)
		if err != nil {
			t.Fatalf("block %s parallel: %v", testBlockHexes[i], err)
		}

		if serialState != parallelState {
			t.Errorf("block %s: stateRoot mismatch: serial=%s parallel=%s", testBlockHexes[i], serialState.Hex(), parallelState.Hex())
		}
		if serialReceipt != parallelReceipt {
			t.Errorf("block %s: receiptRoot mismatch: serial=%s parallel=%s", testBlockHexes[i], serialReceipt.Hex(), parallelReceipt.Hex())
		}

		if serialState == parallelState && serialReceipt == parallelReceipt {
			t.Logf("block %s: consistent (state=%s receipt=%s)", testBlockHexes[i], serialState.Hex()[:10], serialReceipt.Hex()[:10])
		}
	}
}

// ValidatingParallelStateDB wraps ParallelStateDB and compares reads against a reference StateDB.
type ValidatingParallelStateDB struct {
	*state.ParallelStateDB
	ref       *state.StateDB
	tb        *testing.T
	diffCount int
	maxDiffs  int
}

func NewValidatingParallelStateDB(txIndex int, base *state.StateDB, store *blockstm.MVStore, bals *blockstm.MVBalanceStore, ref *state.StateDB, tb *testing.T) *ValidatingParallelStateDB {
	return &ValidatingParallelStateDB{
		ParallelStateDB: state.NewParallelStateDB(txIndex, state.NewSafeBase(base, 1), store, bals),
		ref:             ref,
		tb:              tb,
		maxDiffs:        20,
	}
}

func (v *ValidatingParallelStateDB) GetBalance(addr common.Address) *uint256.Int {
	result := v.ParallelStateDB.GetBalance(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetBalance(addr)
		if sResult.Cmp(result) != 0 {
			v.diffCount++
			v.tb.Logf("    [%d] GetBalance(%s): ref=%s v2=%s", v.diffCount, addr.Hex()[:10],
				sResult.ToBig().String(), result.ToBig().String())
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	result := v.ParallelStateDB.GetState(addr, key)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetState(addr, key)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] GetState(%s, slot=%s): ref=%s v2=%s", v.diffCount,
				addr.Hex()[:10], key.Hex(), sResult.Hex(), result.Hex())
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) GetCommittedState(addr common.Address, key common.Hash) common.Hash {
	result := v.ParallelStateDB.GetCommittedState(addr, key)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetCommittedState(addr, key)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] GetCommittedState(%s, slot=%s): ref=%s v2=%s", v.diffCount,
				addr.Hex()[:10], key.Hex(), sResult.Hex(), result.Hex())
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) GetStateAndCommittedState(addr common.Address, key common.Hash) (common.Hash, common.Hash) {
	s := v.GetState(addr, key)
	c := v.GetCommittedState(addr, key)
	return s, c
}

func (v *ValidatingParallelStateDB) GetCodeHash(addr common.Address) common.Hash {
	result := v.ParallelStateDB.GetCodeHash(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetCodeHash(addr)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] GetCodeHash(%s): ref=%s v2=%s", v.diffCount,
				addr.Hex(), sResult.Hex()[:10], result.Hex()[:10])
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) Exist(addr common.Address) bool {
	result := v.ParallelStateDB.Exist(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.Exist(addr)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] Exist(%s): ref=%v v2=%v", v.diffCount, addr.Hex()[:10], sResult, result)
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) GetNonce(addr common.Address) uint64 {
	result := v.ParallelStateDB.GetNonce(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetNonce(addr)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] GetNonce(%s): ref=%d v2=%d", v.diffCount, addr.Hex()[:10], sResult, result)
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) GetCode(addr common.Address) []byte {
	result := v.ParallelStateDB.GetCode(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetCode(addr)
		if len(sResult) != len(result) {
			v.diffCount++
			v.tb.Logf("    [%d] GetCode(%s): ref_len=%d v2_len=%d", v.diffCount,
				addr.Hex()[:10], len(sResult), len(result))
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) GetCodeSize(addr common.Address) int {
	result := v.ParallelStateDB.GetCodeSize(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.GetCodeSize(addr)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] GetCodeSize(%s): ref=%d v2=%d", v.diffCount,
				addr.Hex()[:10], sResult, result)
		}
	}
	return result
}

func (v *ValidatingParallelStateDB) Empty(addr common.Address) bool {
	result := v.ParallelStateDB.Empty(addr)
	if v.diffCount < v.maxDiffs {
		sResult := v.ref.Empty(addr)
		if sResult != result {
			v.diffCount++
			v.tb.Logf("    [%d] Empty(%s): ref=%v v2=%v", v.diffCount, addr.Hex()[:10], sResult, result)
		}
	}
	return result
}

// TestAllBlocksConsistency tests serial vs parallel consistency for all 241 blocks.
// Slow (~4min). Run with: BOR_BLOCKSTM_TEST=1 go test -run TestAllBlocksConsistency ./core/
func TestAllBlocksConsistency(t *testing.T) {
	if os.Getenv("BOR_BLOCKSTM_TEST") == "" {
		t.Skip("skipping slow test: set BOR_BLOCKSTM_TEST=1 to run")
	}
	alchemyURL := getAlchemyURL(t)
	blocks, diskdb := loadBlocksFromDir(t, witnessDir, alchemyURL)
	runConsistencyCheck(t, blocks, diskdb)
}

func runConsistencyCheck(t *testing.T, blocks []testBlockData, diskdb ethdb.Database) {
	t.Helper()

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	numProcs := runtime.NumCPU()
	failures := 0

	for i, bd := range blocks {
		author := getAuthor(config, bd.witness.Header())
		blockNum := bd.block.NumberU64()

		serialState, serialReceipt, _, err := executeStatelessSerial(config, bd.block, bd.witness, &author, engine, diskdb)
		if err != nil {
			t.Errorf("block %d (#%d): serial error: %v", blockNum, i, err)
			failures++
			continue
		}

		parallelState, parallelReceipt, _, err := executeStatelessParallel(config, bd.block, bd.witness, &author, engine, diskdb, numProcs)
		if err != nil {
			t.Logf("block %d (#%d) parallel error (skipping): %v", blockNum, i, err)
			continue
		}

		if serialState != parallelState || serialReceipt != parallelReceipt {
			t.Errorf("block %d (#%d): mismatch serial_state=%s par_state=%s serial_receipt=%s par_receipt=%s",
				blockNum, i, serialState.Hex()[:10], parallelState.Hex()[:10],
				serialReceipt.Hex()[:10], parallelReceipt.Hex()[:10])
			failures++
		}

		// GC between blocks to avoid OOM on large test sets
		if i > 0 && i%20 == 0 {
			runtime.GC()
		}
	}

	t.Logf("%d/%d blocks consistent (%d failures)", len(blocks)-failures, len(blocks), failures)
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkMainnetStatelessSerial(b *testing.B) {
	alchemyURL := getAlchemyURL(b)
	blocks, diskdb := loadTestBlocks(b, alchemyURL)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	prepared := prepareBlocks(blocks, diskdb, config)

	totalGas := uint64(0)
	for _, pb := range prepared {
		totalGas += pb.block.GasUsed()
	}

	b.Run("AllBlocks", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range prepared {
				if _, err := processSerial(&prepared[j], config, engine); err != nil {
					b.Fatalf("block %s: %v", testBlockHexes[j], err)
				}
			}
		}
		b.StopTimer()
		mgasps := float64(totalGas) * float64(b.N) / b.Elapsed().Seconds() / 1e6
		b.ReportMetric(mgasps, "mgas/s")
	})

	for i := range prepared {
		pb := &prepared[i]
		name := fmt.Sprintf("Block_%s_%dtx_%dMgas", testBlockHexes[i], len(pb.block.Transactions()), pb.block.GasUsed()/1e6)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				if _, err := processSerial(pb, config, engine); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			mgasps := float64(pb.block.GasUsed()) * float64(b.N) / b.Elapsed().Seconds() / 1e6
			b.ReportMetric(mgasps, "mgas/s")
		})
	}
}

func BenchmarkMainnetStatelessParallel(b *testing.B) {
	alchemyURL := getAlchemyURL(b)
	blocks, diskdb := loadTestBlocks(b, alchemyURL)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	numProcs := runtime.NumCPU()
	prepared := prepareBlocks(blocks, diskdb, config)

	totalGas := uint64(0)
	for _, pb := range prepared {
		totalGas += pb.block.GasUsed()
	}

	b.Run("AllBlocks", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range prepared {
				if _, err := processParallel(&prepared[j], config, engine, numProcs); err != nil {
					b.Fatalf("block %s: %v", testBlockHexes[j], err)
				}
			}
		}
		b.StopTimer()
		mgasps := float64(totalGas) * float64(b.N) / b.Elapsed().Seconds() / 1e6
		b.ReportMetric(mgasps, "mgas/s")
		b.ReportMetric(float64(numProcs), "workers")
	})

	for i := range prepared {
		pb := &prepared[i]
		name := fmt.Sprintf("Block_%s_%dtx_%dMgas", testBlockHexes[i], len(pb.block.Transactions()), pb.block.GasUsed()/1e6)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				if _, err := processParallel(pb, config, engine, numProcs); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			mgasps := float64(pb.block.GasUsed()) * float64(b.N) / b.Elapsed().Seconds() / 1e6
			b.ReportMetric(mgasps, "mgas/s")
		})
	}
}

// processV2Serial is the legacy serial-V2 path (one tx at a time through ParallelStateDB).
func processV2Serial(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine) error {
	_, _, err := processV2(pb, config, engine, 1)
	return err
}

// v2TxResult holds the result of a single tx execution on ParallelStateDB.
type v2TxResult struct {
	txIdx int
	pdb   *state.ParallelStateDB
	tx    *types.Transaction
	err   error
}

func processV2Parallel(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine, numWorkers int) error {
	finalDB := pb.baseState.Copy()

	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	signer := types.MakeSigner(config, pb.block.Number(), pb.block.Time())
	blockContext := NewEVMBlockContext(pb.block.Header(), &BlockChain{
		chainConfig: config,
		hc: &HeaderChain{config: config, chainDb: pb.memdb,
			headerCache: pb.headerCache, engine: engine},
	}, &pb.author)

	// Build task list (skip state sync txs)
	type txTask struct {
		idx int
		tx  *types.Transaction
		msg *Message
	}
	var tasks []txTask
	for i, tx := range pb.block.Transactions() {
		if tx.Type() == types.StateSyncTxType {
			continue
		}
		msg, err := TransactionToMessage(tx, signer, pb.block.Header().BaseFee)
		if err != nil {
			return fmt.Errorf("tx %d: %w", i, err)
		}
		tasks = append(tasks, txTask{idx: i, tx: tx, msg: msg})
	}

	// Execute all txs in parallel using a worker pool.
	// Each worker gets its own copy of baseState (state.StateDB is not thread-safe).
	results := make([]v2TxResult, len(tasks))
	taskCh := make(chan int, len(tasks))
	for i := range tasks {
		taskCh <- i
	}
	close(taskCh)

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerBase := pb.baseState.Copy()
		go func(base *state.StateDB) {
			defer wg.Done()
			for taskIdx := range taskCh {
				t := &tasks[taskIdx]
				pdb := state.NewParallelStateDB(t.idx, state.NewSafeBase(base, 1), store, bals)
				evm := vm.NewEVM(blockContext, pdb, config, vm.Config{})
				evm.SetTxContext(NewEVMTxContext(t.msg))

				func() {
					defer func() {
						if r := recover(); r != nil {
							results[taskIdx] = v2TxResult{txIdx: t.idx, pdb: pdb, tx: t.tx, err: fmt.Errorf("panic: %v", r)}
						}
					}()
					_, err := ApplyMessage(evm, t.msg, new(GasPool).AddGas(pb.block.GasLimit()))
					results[taskIdx] = v2TxResult{txIdx: t.idx, pdb: pdb, tx: t.tx, err: err}
				}()
			}
		}(workerBase)
	}
	wg.Wait()

	// Settle in order (skip failed txs — they may have nonce conflicts)
	for _, r := range results {
		if r.err != nil {
			continue
		}
		finalDB.SetTxContext(r.tx.Hash(), r.txIdx)
		r.pdb.SettleTo(finalDB)
	}

	engine.Finalize(nil, pb.block.Header(), finalDB, pb.block.Body(), nil)
	return nil
}

func BenchmarkParallelStateDBV2(b *testing.B) {
	alchemyURL := getAlchemyURL(b)
	blocks, diskdb := loadTestBlocks(b, alchemyURL)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	prepared := prepareBlocks(blocks, diskdb, config)

	totalGas := uint64(0)
	for _, pb := range prepared {
		totalGas += pb.block.GasUsed()
	}

	b.Run("AllBlocks", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range prepared {
				if err := processV2Serial(&prepared[j], config, engine); err != nil {
					b.Fatalf("block %s: %v", testBlockHexes[j], err)
				}
			}
		}
		b.StopTimer()
		mgasps := float64(totalGas) * float64(b.N) / b.Elapsed().Seconds() / 1e6
		b.ReportMetric(mgasps, "mgas/s")
	})

	for i := range prepared {
		pb := &prepared[i]
		name := fmt.Sprintf("Block_%s_%dtx_%dMgas", testBlockHexes[i], len(pb.block.Transactions()), pb.block.GasUsed()/1e6)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				if err := processV2Serial(pb, config, engine); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			mgasps := float64(pb.block.GasUsed()) * float64(b.N) / b.Elapsed().Seconds() / 1e6
			b.ReportMetric(mgasps, "mgas/s")
		})
	}
}

func BenchmarkParallelStateDBV2Parallel(b *testing.B) {
	alchemyURL := getAlchemyURL(b)
	blocks, diskdb := loadTestBlocks(b, alchemyURL)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	prepared := prepareBlocks(blocks, diskdb, config)

	totalGas := uint64(0)
	for _, pb := range prepared {
		totalGas += pb.block.GasUsed()
	}

	for _, numWorkers := range []int{2, 4, 8, 16} {
		b.Run(fmt.Sprintf("AllBlocks/%dworkers", numWorkers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j := range prepared {
					if err := processV2Parallel(&prepared[j], config, engine, numWorkers); err != nil {
						b.Fatalf("block %s: %v", testBlockHexes[j], err)
					}
				}
			}
			b.StopTimer()
			mgasps := float64(totalGas) * float64(b.N) / b.Elapsed().Seconds() / 1e6
			b.ReportMetric(mgasps, "mgas/s")
		})
	}
}

// TestV2BlockSTM tests parallel V2 with BlockSTM validation.
func TestV2BlockSTMWorkerScaling(t *testing.T) {
	alchemyURL := getAlchemyURL(t)
	blocks, diskdb := loadTestBlocks(t, alchemyURL)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	// Only test the 630-tx block (index 3 = 0x4EC6D13)
	bd := blocks[3]
	author := getAuthor(config, bd.witness.Header())
	for _, nw := range []int{1, 2, 4, 8, 16} {
		signer := types.MakeSigner(config, bd.block.Number(), bd.block.Time())
		v2Memdb := bd.witness.MakeHashDB(diskdb)
		v2BaseDB, _ := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(v2Memdb, triedb.HashDefaults), nil))
		store := blockstm.NewMVStore()
		bals := blockstm.NewMVBalanceStore()
		blockContext := NewEVMBlockContext(bd.block.Header(), &BlockChain{
			chainConfig: config,
			hc: &HeaderChain{config: config, chainDb: v2Memdb,
				headerCache: lru.NewCache[common.Hash, *types.Header](256), engine: engine},
		}, &author)
		var tasks []V2Task
		for j, tx := range bd.block.Transactions() {
			if tx.Type() == types.StateSyncTxType {
				continue
			}
			msg, _ := TransactionToMessage(tx, signer, bd.block.Header().BaseFee)
			tasks = append(tasks, V2Task{Index: j, Tx: tx, Msg: msg})
		}
		readBase, _ := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(bd.witness.MakeHashDB(diskdb), triedb.HashDefaults), nil))
		finalDB := v2BaseDB
		result := ExecuteV2BlockSTM(context.Background(), tasks, readBase, store, bals, blockContext, bd.block.Hash(), vm.Config{}, config, bd.block.GasLimit(), nw, finalDB, nil)
		t.Logf("%dw: %d txs, p1=%v settle=%v, execs=%d vfails=%d",
			nw, len(tasks), result.Phase1, result.SettleDur, result.ExecCount, result.VFailCount)
	}
}

func processV2BlockSTM(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine, numWorkers int) error {
	_, _, err := processV2(pb, config, engine, numWorkers)
	return err
}

// TestV2ChainWaitDiagnostic measures how much of V2's per-block wall time is
// spent in the validation goroutine BLOCKED on execDone[i] — the chain-wait
// that the in-order validateOne loop incurs. This is the bottleneck a slow
// tx can impose on every later tx's validation/re-exec.
//
// Run with: BOR_BLOCKSTM_TEST=1 go test -run='^TestV2ChainWaitDiagnostic$' -v ./core/ -timeout 600s
//
// What it prints:
//   - Per-block: total Phase1, ValWaitDur, the wait fraction, exec/vfail counts.
//   - Aggregate: distribution of wait fractions, top-10 worst blocks.
//
// Reading the output:
//   - Wait fractions consistently <5%: the in-order walk is NOT a real
//     bottleneck on this workload. The "slow tx blocks parallel re-exec"
//     scenario is theoretical; production blocks don't hit it.
//   - Wait fractions consistently >20%: the bottleneck is real. Switching
//     to dep-aware/cascading validation would yield meaningful throughput.
//   - Mixed: dig into the top-10 worst-block listing for the workload
//     pattern that's penalised.
func TestV2ChainWaitDiagnostic(t *testing.T) {
	if os.Getenv("BOR_BLOCKSTM_TEST") == "" {
		t.Skip("skipping slow diagnostic: set BOR_BLOCKSTM_TEST=1 to run")
	}
	alchemyURL := getAlchemyURL(t)
	allBlocks, diskdb := loadBlocksFromDir(t, witnessDir, alchemyURL)
	t.Logf("loaded %d blocks", len(allBlocks))

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	type blockStat struct {
		num        uint64
		txs        int
		phase1     time.Duration
		waitDur    time.Duration
		checkDur   time.Duration
		reexecDur  time.Duration
		settleDur  time.Duration
		execCount  int
		vfailCount int
		waitPct    float64 // ValWaitDur / Phase1 * 100
		reexecPct  float64 // ValReexDur / Phase1 * 100
	}
	stats := make([]blockStat, 0, len(allBlocks))

	for _, bd := range allBlocks {
		author := getAuthor(config, bd.witness.Header())
		signer := types.MakeSigner(config, bd.block.Number(), bd.block.Time())

		v2Memdb := bd.witness.MakeHashDB(diskdb)
		v2BaseDB, err := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(v2Memdb, triedb.HashDefaults), nil))
		if err != nil {
			t.Logf("block %d: skip (state init: %v)", bd.block.NumberU64(), err)
			continue
		}
		readBase, _ := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(bd.witness.MakeHashDB(diskdb), triedb.HashDefaults), nil))
		store := blockstm.NewMVStore()
		bals := blockstm.NewMVBalanceStore()
		blockCtx := NewEVMBlockContext(bd.block.Header(), &BlockChain{
			chainConfig: config,
			hc: &HeaderChain{config: config, chainDb: v2Memdb,
				headerCache: lru.NewCache[common.Hash, *types.Header](256), engine: engine},
		}, &author)

		var tasks []V2Task
		for j, tx := range bd.block.Transactions() {
			if tx.Type() == types.StateSyncTxType {
				continue
			}
			msg, _ := TransactionToMessage(tx, signer, bd.block.Header().BaseFee)
			tasks = append(tasks, V2Task{Index: j, Tx: tx, Msg: msg})
		}

		v2BaseDB.StartPrefetcher("diag", nil, nil)
		result := ExecuteV2BlockSTM(context.Background(), tasks, readBase, store, bals, blockCtx, bd.block.Hash(),
			vm.Config{}, config, bd.block.GasLimit(), 8, v2BaseDB, nil)
		v2BaseDB.StopPrefetcher()

		s := blockStat{
			num:        bd.block.NumberU64(),
			txs:        len(tasks),
			phase1:     result.Phase1,
			waitDur:    result.ValWaitDur,
			checkDur:   result.ValCheckDur,
			reexecDur:  result.ValReexDur,
			settleDur:  result.SettleDur,
			execCount:  result.ExecCount,
			vfailCount: result.VFailCount,
		}
		if result.Phase1 > 0 {
			s.waitPct = float64(result.ValWaitDur) / float64(result.Phase1) * 100
			s.reexecPct = float64(result.ValReexDur) / float64(result.Phase1) * 100
		}
		stats = append(stats, s)
	}

	if len(stats) == 0 {
		t.Fatal("no blocks processed")
	}

	// Aggregate: mean / median / p95 / p99 of waitPct.
	waitPcts := make([]float64, len(stats))
	for i, s := range stats {
		waitPcts[i] = s.waitPct
	}
	sort.Float64s(waitPcts)
	pct := func(p float64) float64 {
		idx := int(float64(len(waitPcts)-1) * p / 100)
		return waitPcts[idx]
	}
	var sum float64
	for _, v := range waitPcts {
		sum += v
	}
	mean := sum / float64(len(waitPcts))

	t.Logf("=== chain-wait fraction (ValWaitDur / Phase1) across %d blocks ===", len(stats))
	t.Logf("  mean   = %.1f%%", mean)
	t.Logf("  median = %.1f%%", pct(50))
	t.Logf("  p75    = %.1f%%", pct(75))
	t.Logf("  p95    = %.1f%%", pct(95))
	t.Logf("  p99    = %.1f%%", pct(99))
	t.Logf("  max    = %.1f%%", pct(100))

	// Top-10 by waitPct, descending.
	sort.Slice(stats, func(i, j int) bool { return stats[i].waitPct > stats[j].waitPct })
	t.Logf("\n=== top 10 blocks by chain-wait fraction ===")
	t.Logf("%-10s %4s %8s %8s %8s %5s %4s %5s",
		"block", "txs", "phase1", "wait", "reexec", "wait%", "vfl", "exec")
	for i := 0; i < 10 && i < len(stats); i++ {
		s := stats[i]
		t.Logf("%-10d %4d %8s %8s %8s %4.1f%% %4d %5d",
			s.num, s.txs,
			s.phase1.Round(time.Microsecond),
			s.waitDur.Round(time.Microsecond),
			s.reexecDur.Round(time.Microsecond),
			s.waitPct, s.vfailCount, s.execCount)
	}
}

// BenchmarkV2Embedded benchmarks Serial vs V2 on the 10 embedded testdata blocks.
// No external data or Alchemy URL needed — runs in CI.
func BenchmarkV2Embedded(b *testing.B) {
	blocks, diskdb := loadEmbeddedBlocks(b)

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	prepared := prepareBlocks(blocks, diskdb, config)

	totalGas := uint64(0)
	for _, pb := range prepared {
		totalGas += pb.block.GasUsed()
	}

	b.Run("Serial", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range prepared {
				processSerial(&prepared[j], config, engine)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(totalGas)*float64(b.N)/b.Elapsed().Seconds()/1e6, "mgas/s")
	})

	for _, numWorkers := range []int{4, 8, 16} {
		b.Run(fmt.Sprintf("V2/%dw", numWorkers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j := range prepared {
					processV2BlockSTM(&prepared[j], config, engine, numWorkers)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(totalGas)*float64(b.N)/b.Elapsed().Seconds()/1e6, "mgas/s")
		})
	}
}

// BenchmarkV2AllBlocks benchmarks all 241 witness blocks.
// Run with: BOR_BLOCKSTM_TEST=1 go test -run='^$' -bench=BenchmarkV2AllBlocks ./core/
func BenchmarkV2AllBlocks(b *testing.B) {
	if os.Getenv("BOR_BLOCKSTM_TEST") == "" {
		b.Skip("skipping slow benchmark: set BOR_BLOCKSTM_TEST=1 to run")
	}
	alchemyURL := getAlchemyURL(b)
	allBlocks, diskdb := loadBlocksFromDir(b, witnessDir, alchemyURL)
	b.Logf("loaded %d blocks total", len(allBlocks))

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	prepared := prepareBlocks(allBlocks, diskdb, config)

	totalGas := uint64(0)
	for _, pb := range prepared {
		totalGas += pb.block.GasUsed()
	}

	b.Run("Serial", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for j := range prepared {
				processSerial(&prepared[j], config, engine)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(totalGas)*float64(b.N)/b.Elapsed().Seconds()/1e6, "mgas/s")
	})

	for _, numWorkers := range []int{4, 8, 16} {
		b.Run(fmt.Sprintf("V2/%dw", numWorkers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j := range prepared {
					processV2BlockSTM(&prepared[j], config, engine, numWorkers)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(totalGas)*float64(b.N)/b.Elapsed().Seconds()/1e6, "mgas/s")
		})
	}

	// V2 with witness collection: each block runs through V2 with a freshly
	// allocated *stateless.Witness attached to the StateDB. Measures the
	// overhead of populating prevalueTracer + dumping reader/codeCache into
	// the witness vs the baseline V2 path above. Fewer worker variants here
	// to keep total runtime bounded.
	for _, numWorkers := range []int{4, 8} {
		b.Run(fmt.Sprintf("V2-witness/%dw", numWorkers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for j := range prepared {
					processV2BlockSTMWithWitness(&prepared[j], config, engine, numWorkers)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(totalGas)*float64(b.N)/b.Elapsed().Seconds()/1e6, "mgas/s")
		})
	}
}

// processV2BlockSTMWithWitness is like processV2BlockSTM but attaches a
// fresh *stateless.Witness to the StateDB so V2 exercises its witness
// collection path (trie tracer + reader.CollectStateWitness +
// safeBase.CollectCodeWitness).
func processV2BlockSTMWithWitness(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine, numWorkers int) error {
	db := pb.baseState.Copy()
	w, err := stateless.NewWitness(pb.block.Header(), nil)
	if err != nil {
		return err
	}
	db.SetWitness(w)
	bc := &BlockChain{
		chainConfig: config,
		hc:          &HeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine},
	}
	hc := &benchHeaderChain{
		config:      config,
		chainDb:     pb.memdb,
		headerCache: pb.headerCache,
		engine:      engine,
	}
	_, err = NewV2StateProcessor(hc, bc, numWorkers).Process(pb.block, db, benchVMConfig, &pb.author, context.Background())
	return err
}

// ---------------------------------------------------------------------------
// V2 BlockSTM consistency on 261+ blocks
// ---------------------------------------------------------------------------

func runV2BlockSTMConsistency(t *testing.T, blocks []testBlockData, diskdb ethdb.Database) {
	t.Helper()

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	numWorkers := 4
	failures := 0
	totalTxs, totalExecs, totalVFails := 0, 0, 0

	for i, bd := range blocks {
		author := getAuthor(config, bd.witness.Header())
		blockNum := bd.block.NumberU64()

		// Serial execution (reference)
		tSerial := time.Now()
		serialState, serialReceiptRoot, serialResult, err := executeStatelessSerial(config, bd.block, bd.witness, &author, engine, diskdb)
		serialDur := time.Since(tSerial)
		if err != nil {
			t.Errorf("block %d (#%d): serial error: %v", blockNum, i, err)
			failures++
			continue
		}

		// V2 BlockSTM: use separate MakeHashDB for finalDB and readBase.
		// This is critical: workers read from readBase while settlement writes to finalDB.
		// Sharing the same underlying trie DB causes corruption on some blocks.
		finalMemdb := bd.witness.MakeHashDB(diskdb)
		finalDB, err := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(finalMemdb, triedb.HashDefaults), nil))
		if err != nil {
			t.Logf("block %d (#%d) v2 finalDB error (skipping): %v", blockNum, i, err)
			continue
		}

		readBase, _ := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(bd.witness.MakeHashDB(diskdb), triedb.HashDefaults), nil))

		// Build tasks
		signer := types.MakeSigner(config, bd.block.Number(), bd.block.Time())
		blockContext := NewEVMBlockContext(bd.block.Header(), &BlockChain{
			chainConfig: config,
			hc: &HeaderChain{config: config, chainDb: finalMemdb,
				headerCache: lru.NewCache[common.Hash, *types.Header](256), engine: engine},
		}, &author)

		// Apply pre-execution system calls (EIP-4788 beacon root, EIP-2935 parent hash)
		// to both finalDB and readBase so V2 workers see the same state as serial.
		for _, sdb := range []*state.StateDB{finalDB, readBase} {
			sysEvm := vm.NewEVM(blockContext, sdb, config, vm.Config{})
			if beaconRoot := bd.block.BeaconRoot(); beaconRoot != nil {
				ProcessBeaconBlockRoot(*beaconRoot, sysEvm)
			}
			if config.IsPrague(bd.block.Number()) || config.IsVerkle(bd.block.Number()) {
				ProcessParentBlockHash(bd.block.ParentHash(), sysEvm)
			}
			sdb.Finalise(true)
		}

		var tasks []V2Task
		for j, tx := range bd.block.Transactions() {
			if tx.Type() == types.StateSyncTxType {
				continue
			}
			msg, _ := TransactionToMessage(tx, signer, bd.block.Header().BaseFee)
			tasks = append(tasks, V2Task{Index: j, Tx: tx, Msg: msg})
		}

		// Execute
		store := blockstm.NewMVStore()
		bals := blockstm.NewMVBalanceStore()
		result := ExecuteV2BlockSTM(context.Background(), tasks, readBase, store, bals, blockContext, bd.block.Hash(), vm.Config{}, config, bd.block.GasLimit(), numWorkers, finalDB, nil)
		// A base read failure makes V2 drop the tx at settlement, so the root
		// comparison below would report a misleading mismatch. Surface it as
		// what it is: incomplete state (usually a code blob missing from the
		// testdata archive).
		if result.ReadErr != nil {
			t.Errorf("block %d (#%d): v2 base read error: %v", blockNum, i, result.ReadErr)
			failures++
			continue
		}

		engine.Finalize(nil, bd.block.Header(), finalDB, bd.block.Body(), nil)
		v2State := finalDB.IntermediateRoot(config.IsEIP158(bd.block.Number()))
		v2ReceiptRoot := types.DeriveSha(result.Receipts, trie.NewStackTrie(nil))

		// Validate V2 matches serial execution
		if serialState != v2State {
			t.Errorf("block %d (#%d): stateRoot mismatch serial=%s v2=%s",
				blockNum, i, serialState.Hex()[:10], v2State.Hex()[:10])
			failures++
		} else if serialReceiptRoot != v2ReceiptRoot {
			t.Errorf("block %d (#%d): receiptRoot mismatch serial=%s v2=%s",
				blockNum, i, serialReceiptRoot.Hex()[:10], v2ReceiptRoot.Hex()[:10])
			failures++
		} else if serialResult.GasUsed != result.GasUsed {
			t.Errorf("block %d (#%d): gasUsed mismatch serial=%d v2=%d",
				blockNum, i, serialResult.GasUsed, result.GasUsed)
			failures++
		}

		// Verify state root, receipt root, and gas match the block header.
		if bd.stateRoot != (common.Hash{}) && serialState != bd.stateRoot {
			t.Errorf("block %d (#%d): stateRoot mismatch vs header: got=%s want=%s",
				blockNum, i, serialState.Hex()[:10], bd.stateRoot.Hex()[:10])
			failures++
		}
		if bd.receiptRoot != (common.Hash{}) && serialReceiptRoot != bd.receiptRoot {
			t.Errorf("block %d (#%d): receiptRoot mismatch vs header: got=%s want=%s",
				blockNum, i, serialReceiptRoot.Hex()[:10], bd.receiptRoot.Hex()[:10])
			failures++
		}
		if serialResult.GasUsed != bd.block.GasUsed() {
			t.Errorf("block %d (#%d): gasUsed mismatch vs header: got=%d want=%d",
				blockNum, i, serialResult.GasUsed, bd.block.GasUsed())
			failures++
		}

		totalTxs += len(tasks)
		totalExecs += result.ExecCount
		totalVFails += result.VFailCount
		t.Logf("  block %d: serial=%v v2_total=%v vfails=%d/%d",
			blockNum, serialDur.Round(time.Millisecond),
			result.Phase1.Round(time.Millisecond),
			result.VFailCount, len(tasks))
		if i > 0 && i%20 == 0 {
			runtime.GC()
			t.Logf("  progress: %d/%d (%d failures)", i, len(blocks), failures)
		}
	}

	vfailPct := float64(0)
	if totalTxs > 0 {
		vfailPct = float64(totalVFails) * 100 / float64(totalTxs)
	}
	t.Logf("%d/%d blocks consistent (%d failures)", len(blocks)-failures, len(blocks), failures)
	t.Logf("V2 stats: %d txs, %d execs, %d vfails (%.1f%%)", totalTxs, totalExecs, totalVFails, vfailPct)
}

// TestV2BlockSTMEmbedded runs V2 consistency on the 10 representative blocks
// committed to the repo. This is the primary CI test — no external data needed.
func TestV2BlockSTMEmbedded(t *testing.T) {
	blocks, diskdb := loadEmbeddedBlocks(t)
	runV2BlockSTMConsistency(t, blocks, diskdb)
}

// TestV2BlockSTMAllBlocks tests V2 BlockSTM consistency for all 241 blocks.
// Slow (~4min). Run with: BOR_BLOCKSTM_TEST=1 go test -run TestV2BlockSTMAllBlocks ./core/
func TestV2BlockSTMAllBlocks(t *testing.T) {
	if os.Getenv("BOR_BLOCKSTM_TEST") == "" {
		t.Skip("skipping slow test: set BOR_BLOCKSTM_TEST=1 to run")
	}
	alchemyURL := getAlchemyURL(t)
	blocks, diskdb := loadBlocksFromDir(t, witnessDir, alchemyURL)
	runV2BlockSTMConsistency(t, blocks, diskdb)
}
