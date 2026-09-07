package ethapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

const ErrHeaderNotFound = "current header not found"

// errInvalidBlockHash is a sentinel error for invalid block hash queries
var errInvalidBlockHash = errors.New("invalid block hash")

// LogFilterOptions specifies options for GetLatestLogs
type LogFilterOptions struct {
	LogCount          *uint64 `json:"logCount,omitempty"`          // Max number of logs to return
	BlockCount        *uint64 `json:"blockCount,omitempty"`        // Max number of blocks to scan
	IgnoreTopicsOrder bool    `json:"ignoreTopicsOrder,omitempty"` // Match topics in any order
}

// FilterCriteria represents a request to filter logs.
// This is the same as ethereum.FilterQuery but with proper JSON unmarshaling
// that handles the RPC-standard "address" field (singular) and validates
// mutually exclusive fields.
type FilterCriteria ethereum.FilterQuery

const (
	// GetLatestLogMaxLogCount is the maximum number of logs that can be retrieved
	GetLatestLogMaxLogCount = 30000
	// GetLatestLogMaxBlockCount is the maximum number of blocks that can be scanned
	GetLatestLogMaxBlockCount = 1000
	// GetLatestLogMaxBlockScan caps blocks scanned in LogCount
	GetLatestLogMaxBlockScan = 100000
	// GetLogsMaxBlockRange is the maximum block range for bor_getLogs
	GetLogsMaxBlockRange = 1000
)

// borInternalCallKey is an unexported type used as a context key.
// Because the type is unexported, no package outside internal/ethapi
// can construct a value of this type, making it impossible for
// external RPC callers to forge this context value.
type borInternalCallKey struct{}

// WithBorInternalCall marks a context as originating from an internal
// consensus call. This allows Call to differentiate internal calls.
func WithBorInternalCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, borInternalCallKey{}, true)
}

// isBorInternalCall returns true if the context was marked as an internal call.
func isBorInternalCall(ctx context.Context) bool {
	v, _ := ctx.Value(borInternalCallKey{}).(bool)
	return v
}

// isBorSystemTx checks if the tx is for bor genesis contract addresses or not
func isBorSystemTx(borCfg *params.BorConfig, to *common.Address) bool {
	if borCfg == nil {
		return false
	}
	if to == nil {
		return false
	}

	validatorContract := common.HexToAddress(borCfg.ValidatorContract)
	stateReceiverContract := common.HexToAddress(borCfg.StateReceiverContract)
	if to.Cmp(validatorContract) == 0 || to.Cmp(stateReceiverContract) == 0 {
		return true
	}

	return false
}

// GetRootHash returns root hash for a given start and end block
func (api *BlockChainAPI) GetRootHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64) (string, error) {
	root, err := api.b.GetRootHash(ctx, starBlockNr, endBlockNr)
	if err != nil {
		return "", err
	}

	return root, nil
}

func (api *BlockChainAPI) GetBorBlockReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	return api.b.GetBorBlockReceipt(ctx, hash)
}

func (api *BlockChainAPI) GetVoteOnHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64, hash string, milestoneId string) (bool, error) {
	return api.b.GetVoteOnHash(ctx, starBlockNr, endBlockNr, hash, milestoneId)
}

//
// Bor transaction utils
//

func (api *BlockChainAPI) appendRPCMarshalBorTransaction(ctx context.Context, block *types.Block, fields map[string]interface{}, fullTx bool) map[string]interface{} {
	if block != nil {
		txHash := types.GetDerivedBorTxHash(types.BorReceiptKey(block.Number().Uint64(), block.Hash()))

		borTx, blockHash, blockNumber, txIndex, _ := api.b.GetBorBlockTransactionWithBlockHash(ctx, txHash, block.Hash())
		if borTx != nil {
			formattedTxs := fields["transactions"].([]interface{})

			if fullTx {
				marshalledTx := newRPCTransaction(borTx, blockHash, blockNumber, block.Time(), txIndex, block.BaseFee(), api.b.ChainConfig())
				// newRPCTransaction calculates hash based on RLP of the transaction data.
				// In the case of bor block tx, we need simple derived tx hash (same as function argument) instead of RLP hash
				marshalledTx.Hash = txHash
				marshalledTx.ChainID = nil
				fields["transactions"] = append(formattedTxs, marshalledTx)
			} else {
				fields["transactions"] = append(formattedTxs, txHash)
			}
		}
	}

	return fields
}

// BorAPI provides an API to access Bor related information.
type BorAPI struct {
	b Backend
}

// NewBorAPI creates a new Bor protocol API.
func NewBorAPI(b Backend) *BorAPI {
	return &BorAPI{b}
}

// SendRawTransactionConditional will add the signed transaction to the transaction pool.
// The sender/bundler is responsible for signing the transaction
func (api *BorAPI) SendRawTransactionConditional(ctx context.Context, input hexutil.Bytes, options types.OptionsPIP15) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return common.Hash{}, err
	}

	currentHeader := api.b.CurrentHeader()
	currentState, _, err := api.b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(currentHeader.Number.Int64()))

	if currentState == nil || err != nil {
		return common.Hash{}, err
	}

	// check block number range
	if err := currentHeader.ValidateBlockNumberOptionsPIP15(options.BlockNumberMin, options.BlockNumberMax); err != nil {
		return common.Hash{}, &rpc.OptionsValidateError{Message: "out of block range. err: " + err.Error()}
	}

	// check timestamp range
	if err := currentHeader.ValidateTimestampOptionsPIP15(options.TimestampMin, options.TimestampMax); err != nil {
		return common.Hash{}, &rpc.OptionsValidateError{Message: "out of time range. err: " + err.Error()}
	}

	// check knownAccounts length (number of slots/accounts) should be less than 1000
	if err := options.KnownAccounts.ValidateLength(); err != nil {
		return common.Hash{}, &rpc.KnownAccountsLimitExceededError{Message: "limit exceeded. err: " + err.Error()}
	}

	// check knownAccounts
	if err := currentState.ValidateKnownAccounts(options.KnownAccounts); err != nil {
		return common.Hash{}, &rpc.OptionsValidateError{Message: "storage error. err: " + err.Error()}
	}

	// put options data in Tx to use it later while block building
	tx.PutOptions(&options)

	return SubmitTransaction(ctx, api.b, tx)
}

func (api *BorAPI) GetVoteOnHash(ctx context.Context, starBlockNr uint64, endBlockNr uint64, hash string, milestoneId string) (bool, error) {
	return api.b.GetVoteOnHash(ctx, starBlockNr, endBlockNr, hash, milestoneId)
}

// GetWitnessByNumber returns the witness for the given block number.
func (api *BorAPI) GetWitnessByNumber(ctx context.Context, number rpc.BlockNumber) (map[string]interface{}, error) {
	witness, err := api.b.WitnessByNumber(ctx, number)
	if witness == nil || err != nil {
		return nil, err
	}
	return RPCMarshalWitness(witness), nil
}

// GetWitnessByHash returns the witness for the given block hash.
func (api *BorAPI) GetWitnessByHash(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	witness, err := api.b.WitnessByHash(ctx, hash)
	if witness == nil || err != nil {
		return nil, err
	}
	return RPCMarshalWitness(witness), nil
}

// GetWitnessByBlockNumberOrHash returns the witness for the given block number or hash.
func (api *BorAPI) GetWitnessByBlockNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (map[string]interface{}, error) {
	witness, err := api.b.WitnessByNumberOrHash(ctx, blockNrOrHash)
	if witness == nil || err != nil {
		return nil, err
	}
	return RPCMarshalWitness(witness), nil
}

// GetBlockReceiptsByBlockHash returns all transaction receipts for a block by hash.
//
// Parameters:
//   - blockHash: canonical block hash
//
// Returns an array of marshaled receipts, or error if the block is not found
func (api *BorAPI) GetBlockReceiptsByBlockHash(ctx context.Context, blockHash common.Hash) ([]map[string]interface{}, error) {
	// Get the block by hash
	block, err := api.b.BlockByHash(ctx, blockHash)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, fmt.Errorf("block not found %x", blockHash)
	}

	// Verify that the block is canonical
	blockNumber := block.Number().Uint64()
	canonicalHash := rawdb.ReadCanonicalHash(api.b.ChainDb(), blockNumber)
	if canonicalHash != blockHash {
		return nil, fmt.Errorf("hash %x is not currently canonical", blockHash)
	}

	// Get receipts for this block
	receipts, err := api.b.GetReceipts(ctx, blockHash)
	if err != nil {
		return nil, err
	}
	if receipts == nil {
		return nil, nil
	}

	chainConfig := api.b.ChainConfig()
	txs := block.Transactions()

	// Validate receipt/transaction count match
	if len(txs) != len(receipts) {
		return nil, fmt.Errorf("receipts length mismatch: %d vs %d", len(txs), len(receipts))
	}

	signer := types.MakeSigner(chainConfig, block.Number(), block.Time())

	// Marshal each receipt
	result := make([]map[string]interface{}, 0, len(receipts))
	for i, receipt := range receipts {
		marshaled := marshalReceipt(receipt, blockHash, blockNumber, signer, txs[i], i, false)
		result = append(result, marshaled)
	}

	// Handle state-sync receipts post Madhuguri HF
	if chainConfig.Bor != nil && chainConfig.Bor.IsMadhugiri(block.Number()) {
		return result, nil
	}

	// Pre-Madhugiri: fetch state-sync receipt separately
	stateSyncReceipt, err := api.b.GetBorBlockReceipt(ctx, blockHash)
	if err != nil && !errors.Is(err, ethereum.NotFound) {
		return nil, err
	}
	if stateSyncReceipt != nil {
		tx, _, _, _ := rawdb.ReadBorTransaction(api.b.ChainDb(), stateSyncReceipt.TxHash)
		if tx != nil {
			result = append(result, marshalReceipt(stateSyncReceipt, blockHash, blockNumber, signer, tx, len(result), true))
		}
	}

	return result, nil
}

// GetHeaderByHash returns a block's header by hash.
// It retrieves the header without transactions.
//
// Parameters:
//   - hash: Block hash
//
// Returns the block header or error if not found
func (api *BorAPI) GetHeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	// Get the header for the specified block hash
	header, err := api.b.HeaderByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if header == nil {
		// Return error for missing header
		return nil, fmt.Errorf("block header not found: %s", hash.String())
	}

	return header, nil
}

// GetHeaderByNumber returns a block's header by number.
// It retrieves the header, without transactions.
//
// Parameters:
//   - blockNumber: Block number tag (latest, earliest, pending, safe, finalized, or numeric)
//
// Returns the block header or error if not found
func (api *BorAPI) GetHeaderByNumber(ctx context.Context, blockNumber rpc.BlockNumber) (*types.Header, error) {
	// Pending block is only known by the miner/builder
	if blockNumber == rpc.PendingBlockNumber {
		block, _, _ := api.b.Pending()
		if block == nil {
			return nil, nil
		}
		// Return header directly
		return block.Header(), nil
	}

	// Get the header for the specified block number
	header, err := api.b.HeaderByNumber(ctx, blockNumber)
	if err != nil {
		return nil, err
	}
	if header == nil {
		// Return error for missing header
		return nil, fmt.Errorf("block header not found: %d", blockNumber)
	}

	return header, nil
}

// RPCBlockExtraData contains the parsed fields from the block header's Extra field.
// Only populated for post-Cancun blocks that use RLP-encoded BlockExtraData.
// TxDependency is deprecated: always null post-Austin, kept for
// backward compatibility.
type RPCBlockExtraData struct {
	GasTarget                *hexutil.Uint64 `json:"gasTarget"`
	BaseFeeChangeDenominator *hexutil.Uint64 `json:"baseFeeChangeDenominator"`
	TxDependency             [][]uint64      `json:"txDependency"`
}

// marshalBlockExtraData decodes the BlockExtraData from a header's Extra field
// and returns it as an RPC-marshalable struct. Returns nil for pre-Cancun blocks.
func marshalBlockExtraData(header *types.Header, chainConfig *params.ChainConfig) *RPCBlockExtraData {
	bed := header.DecodeBlockExtraData(chainConfig)
	if bed == nil {
		return nil
	}

	result := &RPCBlockExtraData{TxDependency: bed.TxDependency}

	if bed.GasTarget != nil {
		gt := hexutil.Uint64(*bed.GasTarget)
		result.GasTarget = &gt
	}

	if bed.BaseFeeChangeDenominator != nil {
		d := hexutil.Uint64(*bed.BaseFeeChangeDenominator)
		result.BaseFeeChangeDenominator = &d
	}

	return result
}

// appendBorExtraData adds the parsed block extra data to the response map if borExtra is true.
func appendBorExtraData(response map[string]interface{}, block *types.Block, borExtra *bool, chainConfig *params.ChainConfig) {
	if borExtra == nil || !*borExtra || response == nil {
		return
	}

	if extraData := marshalBlockExtraData(block.Header(), chainConfig); extraData != nil {
		response["decodedExtra"] = extraData
	}
}

// BlockGasParamsResult contains the EIP-1559 gas parameters stored in a block header.
type BlockGasParamsResult struct {
	GasTarget                *hexutil.Uint64 `json:"gasTarget"`
	BaseFeeChangeDenominator *hexutil.Uint64 `json:"baseFeeChangeDenominator"`
}

// GetBlockGasParams returns the EIP-1559 gas target and base fee change denominator
// stored in the block header's extra field. Only available for post-Giugliano blocks.
func (api *BorAPI) GetBlockGasParams(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*BlockGasParamsResult, error) {
	var header *types.Header

	if blockNr, ok := blockNrOrHash.Number(); ok {
		var err error
		header, err = api.b.HeaderByNumber(ctx, blockNr)
		if err != nil {
			return nil, err
		}
	} else if hash, ok := blockNrOrHash.Hash(); ok {
		var err error
		header, err = api.b.HeaderByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
	}

	if header == nil {
		return nil, fmt.Errorf("header not found")
	}

	gasTarget, bfcd := header.GetBaseFeeParams(api.b.ChainConfig())

	result := &BlockGasParamsResult{}
	if gasTarget != nil {
		gt := hexutil.Uint64(*gasTarget)
		result.GasTarget = &gt
	}
	if bfcd != nil {
		d := hexutil.Uint64(*bfcd)
		result.BaseFeeChangeDenominator = &d
	}

	return result, nil
}

// BlockNumber returns the block number for the given block tag:
// - nil input → latest executed (CurrentBlock)
// - "latest" → the latest head (CurrentHeader)
// - "pending" → latest executed (CurrentBlock)
// - "earliest" → 0
// - "safe" → safe block
// - "finalized" → finalized block
// - numeric (>=0) → that block number
// - unknown negative → latest executed (CurrentBlock)
//
// Parameters:
//   - blockNrPtr: Optional block number or tag
//     If nil, returns the latest executed block number
//
// Returns the block number as hexutil.Uint64
func (api *BorAPI) BlockNumber(ctx context.Context, blockNrPtr *rpc.BlockNumber) (hexutil.Uint64, error) {
	// Handle nil input separately, returns the latest executed block
	if blockNrPtr == nil {
		block := api.b.CurrentBlock()
		if block == nil {
			return 0, errors.New("current block not found")
		}
		return hexutil.Uint64(block.Number.Uint64()), nil
	}

	blockNr := *blockNrPtr

	// Get the appropriate block number based on the tag
	var blockNum uint64
	switch blockNr {
	case rpc.LatestBlockNumber:
		// "latest" returns the latest head (forkchoice head), not the executed one
		header := api.b.CurrentHeader()
		if header == nil {
			return 0, errors.New(ErrHeaderNotFound)
		}
		blockNum = header.Number.Uint64()

	case rpc.EarliestBlockNumber:
		blockNum = 0

	case rpc.SafeBlockNumber:
		// Get the safe block from the blockchain
		header := api.b.CurrentSafeBlock()
		if header == nil {
			return 0, errors.New("safe block not found")
		}
		blockNum = header.Number.Uint64()

	case rpc.FinalizedBlockNumber:
		// Get the finalized block using Heimdall's logic (milestone/checkpoint)
		finalNum, err := api.b.GetFinalizedBlockNumber(ctx)
		if err != nil {
			return 0, err
		}
		blockNum = finalNum

	case rpc.PendingBlockNumber:
		// Pending: return the latest executed block
		block := api.b.CurrentBlock()
		if block == nil {
			return 0, errors.New("current block not found")
		}
		blockNum = block.Number.Uint64()

	default:
		// Concrete block number
		if blockNr >= 0 {
			blockNum = uint64(blockNr)
		} else {
			block := api.b.CurrentBlock()
			if block == nil {
				return 0, errors.New("current block not found")
			}
			blockNum = block.Number.Uint64()
		}
	}

	return hexutil.Uint64(blockNum), nil
}

// Forks is a data type to record a list of forks
type Forks struct {
	GenesisHash common.Hash `json:"genesis"`
	HeightForks []uint64    `json:"heightForks"`
	TimeForks   []uint64    `json:"timeForks"`
}

// Forks implements bor_forks. Returns the genesis block hash and a sorted list of all forks block numbers.
//
// Returns:
//   - GenesisHash: The hash of the genesis block
//   - HeightForks: Sorted list of all block number forks
//   - TimeForks: Sorted list of all timestamp forks
func (api *BorAPI) Forks(ctx context.Context) (Forks, error) {
	// Get genesis block
	genesis, err := api.b.BlockByNumber(ctx, rpc.BlockNumber(0))
	if err != nil {
		return Forks{}, err
	}
	if genesis == nil {
		return Forks{}, errors.New("genesis block not found")
	}

	// Get chain config
	chainConfig := api.b.ChainConfig()
	if chainConfig == nil {
		return Forks{}, errors.New("chain config not found")
	}

	// Gather forks from chain config
	heightForks, timeForks := forkid.GatherForks(chainConfig, genesis.Time())

	return Forks{
		GenesisHash: genesis.Hash(),
		HeightForks: heightForks,
		TimeForks:   timeForks,
	}, nil
}

// GetBlockByTimestamp returns the first block with a timestamp greater than or equal to the given timestamp.
//
// Parameters:
//   - timestamp: Unix timestamp in seconds (accepts both hex strings and decimal numbers)
//   - fullTx: If true, returns full transaction objects; if false, only transaction hashes
//
// Returns the block in RPC format, or nil if not found.
func (api *BorAPI) GetBlockByTimestamp(ctx context.Context, timestamp rpc.Timestamp, fullTx bool) (map[string]interface{}, error) {
	// Convert rpc.Timestamp to uint64
	ts := uint64(timestamp)

	// Get current header
	currentHeader := api.b.CurrentHeader()
	if currentHeader == nil {
		return nil, errors.New(ErrHeaderNotFound)
	}

	// If the current block's time <= timestamp, return the latest block
	if currentHeader.Time <= ts {
		block, err := api.b.BlockByNumber(ctx, rpc.LatestBlockNumber)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, nil
		}
		return RPCMarshalBlock(block, true, fullTx, api.b.ChainConfig(), api.b.ChainDb()), nil
	}

	// Get genesis header
	genesisHeader, err := api.b.HeaderByNumber(ctx, rpc.BlockNumber(0))
	if err != nil {
		return nil, err
	}
	if genesisHeader == nil {
		return nil, errors.New("no genesis header found")
	}

	// If genesis time >= timestamp, return the genesis block (timestamp is before genesis)
	if genesisHeader.Time >= ts {
		block, err := api.b.BlockByNumber(ctx, rpc.BlockNumber(0))
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, nil
		}
		return RPCMarshalBlock(block, true, fullTx, api.b.ChainConfig(), api.b.ChainDb()), nil
	}

	// Binary search for the first block with time >= timestamp.
	// Uses explicit loop instead of sort.Search to respect context cancellation.
	low, high := uint64(0), currentHeader.Number.Uint64()
	for low < high {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mid := low + (high-low)/2
		header, err := api.b.HeaderByNumber(ctx, rpc.BlockNumber(mid))
		if err != nil {
			return nil, err
		}
		if header == nil {
			return nil, fmt.Errorf("no header found for block %d", mid)
		}
		if header.Time >= ts {
			high = mid
		} else {
			low = mid + 1
		}
	}

	// Get the final block
	block, err := api.b.BlockByNumber(ctx, rpc.BlockNumber(low))
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil
	}

	return RPCMarshalBlock(block, true, fullTx, api.b.ChainConfig(), api.b.ChainDb()), nil
}

// GetBalanceChangesInBlock returns balance changes for accounts affected by the block.
// This method uses a heuristic approach to discover changed accounts by examining:
//   - Transaction senders and recipients
//   - Contract creation addresses
//   - Miner/coinbase address
//   - Addresses appearing in transaction logs
//
// Unlike Erigon's temporal-database approach that scans account history changes for the
// block's transaction range, this may miss some accounts with balance changes from:
//   - Internal CALL value transfers to addresses not emitting logs
//   - SELFDESTRUCT operations to recipients not otherwise tracked
//   - Other EVM operations that modify balances without explicit tracking
//
// Parameters:
//   - blockNrOrHash: Block number, hash, or tag (latest, earliest, pending, safe, finalized)
//
// Returns a map of addresses to their post-block balances (only for discovered accounts).
func (api *BorAPI) GetBalanceChangesInBlock(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (map[common.Address]*hexutil.Big, error) {
	// Resolve block number, hash, and canonical requirement
	blockNumber, hash, hasHash, requireCanonical, err := resolveBlockNumberOrHashWithCanonical(blockNrOrHash)
	if err != nil {
		return nil, err
	}

	// Handle pending block
	if !hasHash && blockNumber == rpc.PendingBlockNumber {
		return api.getBalanceChangesForPending(ctx)
	}

	// Get the specific block by hash or number
	// (LatestBlockNumber is resolved by BlockByNumber; all downstream code uses block.NumberU64())
	var block *types.Block
	if hasHash {
		block, err = api.b.BlockByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, fmt.Errorf("block not found")
		}
		blockNumber = rpc.BlockNumber(block.NumberU64())

		// Check canonicality if required
		if requireCanonical {
			canonicalBlock, err := api.b.BlockByNumber(ctx, blockNumber)
			if err != nil {
				return nil, err
			}
			if canonicalBlock == nil || canonicalBlock.Hash() != hash {
				return nil, fmt.Errorf("hash %x is not currently canonical", hash)
			}
		}
	} else {
		block, err = api.b.BlockByNumber(ctx, blockNumber)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, fmt.Errorf("block not found")
		}
	}

	// Genesis block has no balance changes
	if block.NumberU64() == 0 {
		return make(map[common.Address]*hexutil.Big), nil
	}

	// Get parent state and current state for comparison
	parentState, _, err := api.b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(block.NumberU64()-1))
	if err != nil {
		return nil, fmt.Errorf("failed to get parent state: %w", err)
	}
	if parentState == nil {
		return nil, fmt.Errorf("parent state not found")
	}

	currentState, _, err := api.b.StateAndHeaderByNumber(ctx, rpc.BlockNumber(block.NumberU64()))
	if err != nil {
		return nil, fmt.Errorf("failed to get current state: %w", err)
	}
	if currentState == nil {
		return nil, fmt.Errorf("current state not found")
	}

	// Collect all potentially modified addresses
	modifiedAddresses := make(map[common.Address]bool)

	// Add miner
	modifiedAddresses[block.Coinbase()] = true

	// Process all transactions to collect explicit addresses
	signer := types.MakeSigner(api.b.ChainConfig(), block.Number(), block.Time())
	for _, tx := range block.Transactions() {
		// skip state-sync transactions (no sender/recipient)
		if tx.Type() == types.StateSyncTxType {
			continue
		}

		// Add sender
		if sender, err := types.Sender(signer, tx); err == nil {
			modifiedAddresses[sender] = true
		}

		// Add the recipient or contract creation address
		if tx.To() != nil {
			modifiedAddresses[*tx.To()] = true
		} else {
			// Contract creation
			if sender, err := types.Sender(signer, tx); err == nil {
				contractAddr := crypto.CreateAddress(sender, tx.Nonce())
				modifiedAddresses[contractAddr] = true
			}
		}
	}

	// heuristic: check receipts for contract addresses in logs
	receipts, err := api.b.GetReceipts(ctx, block.Hash())
	if err == nil && receipts != nil {
		for _, receipt := range receipts {
			// Add the contract address if it exists
			if receipt.ContractAddress != (common.Address{}) {
				modifiedAddresses[receipt.ContractAddress] = true
			}
			// Add addresses from logs
			for _, log := range receipt.Logs {
				modifiedAddresses[log.Address] = true
			}
		}
	}

	// Compare balances for all identified addresses
	balanceChanges := make(map[common.Address]*hexutil.Big)
	for addr := range modifiedAddresses {
		oldBalance := parentState.GetBalance(addr)
		newBalance := currentState.GetBalance(addr)

		// Include it only if the balance changed
		if oldBalance.Cmp(newBalance) != 0 {
			balanceChanges[addr] = (*hexutil.Big)(newBalance.ToBig())
		}
	}

	return balanceChanges, nil
}

// resolveBlockNumberOrHashWithCanonical resolves a BlockNumberOrHash including canonical requirement
func resolveBlockNumberOrHashWithCanonical(blockNrOrHash rpc.BlockNumberOrHash) (rpc.BlockNumber, common.Hash, bool, bool, error) {
	if blockNr, ok := blockNrOrHash.Number(); ok {
		return blockNr, common.Hash{}, false, false, nil
	}
	if hash, ok := blockNrOrHash.Hash(); ok {
		requireCanonical := blockNrOrHash.RequireCanonical
		return 0, hash, true, requireCanonical, nil
	}
	return 0, common.Hash{}, false, false, fmt.Errorf("invalid block number or hash")
}

// getBalanceChangesForPending returns balance changes for the pending block
func (api *BorAPI) getBalanceChangesForPending(ctx context.Context) (map[common.Address]*hexutil.Big, error) {
	// Get pending block and state
	pendingBlock, pendingReceipts, pendingState := api.b.Pending()
	if pendingBlock == nil || pendingState == nil {
		return nil, fmt.Errorf("pending state not available")
	}

	// Get parent state (current confirmed state)
	parentNumber := rpc.BlockNumber(pendingBlock.NumberU64() - 1)
	parentState, _, err := api.b.StateAndHeaderByNumber(ctx, parentNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to get parent state: %w", err)
	}
	if parentState == nil {
		return nil, fmt.Errorf("parent state not found")
	}

	// Collect modified addresses from pending transactions
	modifiedAddresses := make(map[common.Address]bool)

	// Add miner
	modifiedAddresses[pendingBlock.Coinbase()] = true

	// Process all pending transactions
	signer := types.MakeSigner(api.b.ChainConfig(), pendingBlock.Number(), pendingBlock.Time())
	for _, tx := range pendingBlock.Transactions() {
		// skip state-sync txs (no sender/recipient)
		if tx.Type() == types.StateSyncTxType {
			continue
		}

		if sender, err := types.Sender(signer, tx); err == nil {
			modifiedAddresses[sender] = true
		}
		if tx.To() != nil {
			modifiedAddresses[*tx.To()] = true
		} else {
			if sender, err := types.Sender(signer, tx); err == nil {
				contractAddr := crypto.CreateAddress(sender, tx.Nonce())
				modifiedAddresses[contractAddr] = true
			}
		}
	}

	// Add addresses from pending receipts if available
	if pendingReceipts != nil {
		for _, receipt := range pendingReceipts {
			if receipt.ContractAddress != (common.Address{}) {
				modifiedAddresses[receipt.ContractAddress] = true
			}
			for _, log := range receipt.Logs {
				modifiedAddresses[log.Address] = true
			}
		}
	}

	// Compare balances
	balanceChanges := make(map[common.Address]*hexutil.Big)
	for addr := range modifiedAddresses {
		oldBalance := parentState.GetBalance(addr)
		newBalance := pendingState.GetBalance(addr)

		if oldBalance.Cmp(newBalance) != 0 {
			balanceChanges[addr] = (*hexutil.Big)(newBalance.ToBig())
		}
	}

	return balanceChanges, nil
}

// GetLogsByHash returns the logs generated by the transactions by the block's hash.
//
// Parameters:
//   - hash: Block hash
//
// Returns an array where each element is the logs array for the corresponding transaction in the block.
// Returns nil if the block is not found.
func (api *BorAPI) GetLogsByHash(ctx context.Context, hash common.Hash) ([][]*types.Log, error) {
	// Get the block by hash
	block, err := api.b.BlockByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil
	}

	// Get receipts for this block
	receipts, err := api.b.GetReceipts(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("getReceipts error: %w", err)
	}
	if receipts == nil {
		return nil, nil
	}

	// Extract logs from receipts (one array per transaction)
	logs := make([][]*types.Log, len(receipts))
	for i, receipt := range receipts {
		if len(receipt.Logs) > 0 {
			logs[i] = receipt.Logs
		}
	}

	// Handle Bor state-sync logs (pre-Madhugiri)
	// State-sync receipts are stored with normal block receipts post Madhugiri HF
	chainConfig := api.b.ChainConfig()
	if chainConfig.Bor != nil && chainConfig.Bor.IsMadhugiri(block.Number()) {
		return logs, nil
	}

	// Pre-Madhuguri: fetch the state-sync receipt separately and append its logs
	stateSyncReceipt, err := api.b.GetBorBlockReceipt(ctx, hash)
	if err != nil && !errors.Is(err, ethereum.NotFound) {
		return nil, fmt.Errorf("getReceipts error: %w", err)
	}
	if stateSyncReceipt != nil {
		// Always append an entry for the state-sync receipt, even if it has no logs.
		logs = append(logs, stateSyncReceipt.Logs)
	}

	return logs, nil
}

// GetLogs returns all logs matching the filter criteria.
//
// Parameters:
//   - crit: Standard log filter criteria (addresses, topics, from/to blocks, or specific block hash)
//
// Returns logs in ascending order (earliest first) with BlockTimestamp populated.
func (api *BorAPI) GetLogs(ctx context.Context, crit FilterCriteria) ([]*types.Log, error) {
	// Convert to ethereum.FilterQuery for internal use
	filterQuery := ethereum.FilterQuery(crit)

	// Determine block range
	begin, end, err := api.determineBlockRange(ctx, filterQuery)
	if err != nil {
		// Special case: invalid BlockHash returns (nil, nil) for getLogs (Erigon behavior)
		if errors.Is(err, errInvalidBlockHash) {
			return nil, nil
		}
		return nil, err
	}

	// Enforce max block range (inclusive: 0..999 = 1000 blocks)
	if end-begin+1 > GetLogsMaxBlockRange {
		return nil, &clientLimitExceededError{message: fmt.Sprintf("block range exceeds maximum of %d blocks", GetLogsMaxBlockRange)}
	}

	// Check context before iterating
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Build address set for lookup
	addressSet := make(map[common.Address]struct{}, len(filterQuery.Addresses))
	for _, addr := range filterQuery.Addresses {
		addressSet[addr] = struct{}{}
	}

	// Collect logs in ascending order
	var result []*types.Log

	// Iterate blocks from the beginning to the end
	for blockNum := begin; blockNum <= end; blockNum++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		block, receipts, err := api.getBlockAndReceipts(ctx, blockNum)
		if err != nil {
			return nil, err
		}
		if block == nil {
			// Block not available (e.g., reorged out)
			break
		}

		// Extract logs from receipts
		for _, receipt := range receipts {
			for _, log := range receipt.Logs {
				// Apply filter
				if matchesFilter(log, addressSet, filterQuery.Topics, false) {
					// Copy log to avoid mutating cached receipt data
					logCopy := *log
					logCopy.BlockTimestamp = block.Time()
					result = append(result, &logCopy)
				}
			}
		}
	}

	return result, nil
}

// GetLatestLogs returns the latest N logs matching the filter criteria in descending order.
//
// Parameters:
//   - crit: Standard log filter criteria (addresses, topics, from/to blocks)
//   - logOptions: Pagination and filtering options
//   - LogCount: Max number of logs to return
//   - BlockCount: Max number of blocks to scan
//   - IgnoreTopicsOrder: Match topics in any order instead of positional matching
//
// Note: LogCount and BlockCount are mutually exclusive. If both are 0, defaults to BlockCount=1.
//
// Returns logs in descending order (latest first) with BlockTimestamp populated.
func (api *BorAPI) GetLatestLogs(ctx context.Context, crit FilterCriteria, logOptions LogFilterOptions) ([]*types.Log, error) {
	// Validate that LogCount and BlockCount are not both specified
	if logOptions.LogCount != nil && *logOptions.LogCount != 0 && logOptions.BlockCount != nil && *logOptions.BlockCount != 0 {
		return nil, errors.New("logs count & block count are ambiguous")
	}

	// Set defaults if both are 0
	logCount := uint64(0)
	blockCount := uint64(1) // Default to 1 block
	if logOptions.LogCount != nil {
		logCount = *logOptions.LogCount
		blockCount = 0
	}
	if logOptions.BlockCount != nil {
		blockCount = *logOptions.BlockCount
		logCount = 0
	}

	// Apply max limits
	if logCount > GetLatestLogMaxLogCount {
		logCount = GetLatestLogMaxLogCount
	}
	if blockCount > GetLatestLogMaxBlockCount {
		blockCount = GetLatestLogMaxBlockCount
	}

	// Convert to ethereum.FilterQuery for internal use
	filterQuery := ethereum.FilterQuery(crit)

	// Determine block range
	begin, end, err := api.determineBlockRange(ctx, filterQuery)
	if err != nil {
		if errors.Is(err, errInvalidBlockHash) {
			return nil, fmt.Errorf("block header not found %x", *filterQuery.BlockHash)
		}
		return nil, err
	}

	// Build address set for lookup
	addressSet := make(map[common.Address]struct{}, len(filterQuery.Addresses))
	for _, addr := range filterQuery.Addresses {
		addressSet[addr] = struct{}{}
	}

	// Check context before iterating
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Collect logs in descending order
	var result []*types.Log
	var blocksScanned uint64

	// Iterate blocks from the end to the beginning
	for blockNum := end; blockNum >= begin && blockNum <= end; blockNum-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Check the block count limit
		if blockCount > 0 && blocksScanned >= blockCount {
			break
		}
		// Hard cap on blocks scanned to prevent unbounded chain scans in LogCount mode
		if blocksScanned >= GetLatestLogMaxBlockScan {
			break
		}
		blocksScanned++

		block, receipts, err := api.getBlockAndReceipts(ctx, blockNum)
		if err != nil {
			return nil, err
		}
		if block == nil {
			// Block not available (e.g., reorged out)
			break
		}

		// Extract logs from receipts
		for _, receipt := range receipts {
			for _, log := range receipt.Logs {
				// Apply filter
				if matchesFilter(log, addressSet, filterQuery.Topics, logOptions.IgnoreTopicsOrder) {
					// Copy log to avoid mutating cached receipt data
					logCopy := *log
					logCopy.BlockTimestamp = block.Time()
					result = append(result, &logCopy)

					// Check log count
					if logCount > 0 && uint64(len(result)) >= logCount {
						return result, nil
					}
				}
			}
		}

		// Stop at genesis
		if blockNum == 0 {
			break
		}
	}

	return result, nil
}

// matchesFilter checks if a log matches the filter criteria.
// addressSet is pre-built by the caller for O(1) lookups.
func matchesFilter(log *types.Log, addressSet map[common.Address]struct{}, topics [][]common.Hash, ignoreTopicsOrder bool) bool {
	// Check address filter
	if len(addressSet) > 0 {
		if _, ok := addressSet[log.Address]; !ok {
			return false
		}
	}

	// Check topics filter
	if len(topics) > 0 {
		if ignoreTopicsOrder {
			// Match topics in any order
			return matchesTopicsUnordered(log.Topics, topics)
		}

		// Match topics positionally
		return matchesTopicsOrdered(log.Topics, topics)
	}

	return true
}

// matchesTopicsOrdered checks if log topics match filter topics positionally
func matchesTopicsOrdered(logTopics []common.Hash, filterTopics [][]common.Hash) bool {
	// Filter topics length cannot exceed log topics
	if len(filterTopics) > len(logTopics) {
		return false
	}

	for i, filterTopic := range filterTopics {
		if len(filterTopic) == 0 {
			// Empty filter means "match any"
			continue
		}

		match := false
		for _, topic := range filterTopic {
			if logTopics[i] == topic {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}

	return true
}

// matchesTopicsUnordered checks if log topics contain all filter topics in any order
func matchesTopicsUnordered(logTopics []common.Hash, filterTopics [][]common.Hash) bool {
	// Build the set of log topics
	logTopicSet := make(map[common.Hash]bool)
	for _, topic := range logTopics {
		logTopicSet[topic] = true
	}

	// Check if all filter topics are present
	for _, filterTopic := range filterTopics {
		if len(filterTopic) == 0 {
			continue
		}

		match := false
		for _, topic := range filterTopic {
			if logTopicSet[topic] {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}

	return true
}

// determineBlockRange determines the start and end block numbers for a log query.
// It handles both BlockHash and FromBlock/ToBlock queries, with proper validation
// and clamping to the latest available block.
func (api *BorAPI) determineBlockRange(ctx context.Context, crit ethereum.FilterQuery) (begin, end uint64, err error) {
	if crit.BlockHash != nil {
		// Query a specific block by hash
		header, err := api.b.HeaderByHash(ctx, *crit.BlockHash)
		if err != nil {
			return 0, 0, err
		}
		if header == nil {
			// Return sentinel error for invalid block hash (caller handles differently for getLogs vs getLatestLogs)
			return 0, 0, errInvalidBlockHash
		}
		return header.Number.Uint64(), header.Number.Uint64(), nil
	}

	// Determine range from block numbers
	currentHeader := api.b.CurrentHeader()
	if currentHeader == nil {
		return 0, 0, errors.New(ErrHeaderNotFound)
	}
	latest := currentHeader.Number.Uint64()

	begin = 0
	if crit.FromBlock != nil {
		if crit.FromBlock.Sign() >= 0 {
			begin = crit.FromBlock.Uint64()
		} else if !crit.FromBlock.IsInt64() || crit.FromBlock.Int64() != int64(rpc.LatestBlockNumber) {
			return 0, 0, fmt.Errorf("negative value for FromBlock: %v", crit.FromBlock)
		}
	}

	end = latest
	if crit.ToBlock != nil {
		if crit.ToBlock.Sign() >= 0 {
			requestedEnd := crit.ToBlock.Uint64()
			// Clamp to the latest available block (don't error on future blocks)
			if requestedEnd > latest {
				end = latest
			} else {
				end = requestedEnd
			}
		} else if !crit.ToBlock.IsInt64() || crit.ToBlock.Int64() != int64(rpc.LatestBlockNumber) {
			return 0, 0, fmt.Errorf("negative value for ToBlock: %v", crit.ToBlock)
		}
	}

	// Validate range
	if end < begin {
		return 0, 0, fmt.Errorf("end (%d) < begin (%d)", end, begin)
	}

	return begin, end, nil
}

// getBlockAndReceipts fetches a block and its receipts for the given block number.
// Returns nil block (without error) if the block is not available (e.g., reorg).
func (api *BorAPI) getBlockAndReceipts(ctx context.Context, blockNum uint64) (*types.Block, types.Receipts, error) {
	block, err := api.b.BlockByNumber(ctx, rpc.BlockNumber(blockNum))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get block %d: %w", blockNum, err)
	}
	if block == nil {
		// Block not available (e.g., reorged out)
		return nil, nil, nil
	}

	receipts, err := api.b.GetReceipts(ctx, block.Hash())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get receipts for block %d: %w", blockNum, err)
	}

	chainConfig := api.b.ChainConfig()
	if chainConfig.Bor != nil && !chainConfig.Bor.IsMadhugiri(block.Number()) {
		stateSyncReceipt, err := api.b.GetBorBlockReceipt(ctx, block.Hash())
		if err != nil && !errors.Is(err, ethereum.NotFound) {
			return nil, nil, fmt.Errorf("failed to get Bor receipt for block %d: %w", blockNum, err)
		}
		if stateSyncReceipt != nil {
			receipts = append(receipts, stateSyncReceipt)
		}
	}

	return block, receipts, nil
}

// UnmarshalJSON parses JSON log filter criteria.
// Handles:
// - "address" field (singular) that accepts both single string and array
// - Validates blockHash is mutually exclusive with fromBlock/toBlock
// - Converts rpc.BlockNumber to *big.Int
func (fc *FilterCriteria) UnmarshalJSON(data []byte) error {
	type input struct {
		BlockHash *common.Hash     `json:"blockHash"`
		FromBlock *rpc.BlockNumber `json:"fromBlock"`
		ToBlock   *rpc.BlockNumber `json:"toBlock"`
		Addresses interface{}      `json:"address"` // string or []string
		Topics    []interface{}    `json:"topics"`
	}

	var raw input
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Validate blockHash is mutually exclusive with fromBlock/toBlock
	if raw.BlockHash != nil {
		if raw.FromBlock != nil || raw.ToBlock != nil {
			return errors.New("cannot specify both BlockHash and FromBlock/ToBlock, choose one or the other")
		}
		fc.BlockHash = raw.BlockHash
	} else {
		if raw.FromBlock != nil {
			fc.FromBlock = big.NewInt(raw.FromBlock.Int64())
		}
		if raw.ToBlock != nil {
			fc.ToBlock = big.NewInt(raw.ToBlock.Int64())
		}
	}

	// Parse addresses - handle both single string and array of strings
	fc.Addresses = []common.Address{}
	if raw.Addresses != nil {
		switch addrs := raw.Addresses.(type) {
		case []interface{}:
			for i, addr := range addrs {
				if strAddr, ok := addr.(string); ok {
					address, err := decodeAddress(strAddr)
					if err != nil {
						return fmt.Errorf("invalid address at index %d: %v", i, err)
					}
					fc.Addresses = append(fc.Addresses, address)
				} else {
					return fmt.Errorf("non-string address at index %d", i)
				}
			}
		case string:
			address, err := decodeAddress(addrs)
			if err != nil {
				return fmt.Errorf("invalid address: %v", err)
			}
			fc.Addresses = []common.Address{address}
		default:
			return errors.New("invalid addresses in query")
		}
	}

	// Parse topics
	if len(raw.Topics) > 0 {
		fc.Topics = make([][]common.Hash, len(raw.Topics))
		for i, t := range raw.Topics {
			switch topic := t.(type) {
			case nil:
				// nil matches any topic
			case string:
				hash, err := decodeTopic(topic)
				if err != nil {
					return fmt.Errorf("invalid topic at index %d: %v", i, err)
				}
				fc.Topics[i] = []common.Hash{hash}
			case []interface{}:
				for _, rawTopic := range topic {
					if rawTopic == nil {
						continue
					}
					if strTopic, ok := rawTopic.(string); ok {
						hash, err := decodeTopic(strTopic)
						if err != nil {
							return fmt.Errorf("invalid topic: %v", err)
						}
						fc.Topics[i] = append(fc.Topics[i], hash)
					} else {
						return fmt.Errorf("invalid topic type")
					}
				}
			default:
				return fmt.Errorf("invalid topic type at index %d", i)
			}
		}
	}

	return nil
}

// decodeAddress decodes a hex-encoded address with strict validation.
// Uses hexutil.Decode to properly validate hex encoding and reject invalid characters.
func decodeAddress(s string) (common.Address, error) {
	b, err := hexutil.Decode(s)
	if err == nil && len(b) != common.AddressLength {
		err = fmt.Errorf("hex has invalid length %d after decoding; expected %d for address", len(b), common.AddressLength)
	}
	return common.BytesToAddress(b), err
}

// decodeTopic decodes a hex-encoded topic hash with strict validation.
// Uses hexutil.Decode to properly validate hex encoding and reject invalid characters.
func decodeTopic(s string) (common.Hash, error) {
	b, err := hexutil.Decode(s)
	if err == nil && len(b) != common.HashLength {
		err = fmt.Errorf("hex has invalid length %d after decoding; expected %d for topic", len(b), common.HashLength)
	}
	return common.BytesToHash(b), err
}
