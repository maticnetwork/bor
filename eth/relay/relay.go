package relay

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

var (
	errRelayNotConfigured = errors.New("relay service not configured")
)

type Config struct {
	// for relay
	enablePreconf   bool
	enablePrivateTx bool

	// for block producers
	acceptPreconfTx bool
	acceptPrivateTx bool
}

// RelayService handles all preconf and private transaction related services
type RelayService struct {
	config         Config
	privateTxStore *PrivateTxStore
	txRelay        *Service
}

func Init(enablePreconf, enablePrivateTx, acceptPreconfTx, acceptPrivateTx bool, blockProducerURLs []string) *RelayService {
	config := Config{
		enablePreconf:   enablePreconf,
		enablePrivateTx: enablePrivateTx,
		acceptPreconfTx: acceptPreconfTx,
		acceptPrivateTx: acceptPrivateTx,
	}
	var privateTxStore *PrivateTxStore
	if enablePrivateTx || acceptPrivateTx {
		privateTxStore = NewPrivateTxStore()
	}
	var txRelay *Service
	if enablePreconf || enablePrivateTx {
		if len(blockProducerURLs) == 0 {
			log.Warn("Relay service enabled but no block producer URLs provided; relay will be non-functional")
		}
		txRelay = NewService(blockProducerURLs, nil)
	}
	return &RelayService{
		config:         config,
		privateTxStore: privateTxStore,
		txRelay:        txRelay,
	}
}

func (s *RelayService) RecordPrivateTx(hash common.Hash) {
	if s.privateTxStore != nil {
		s.privateTxStore.Add(hash)
	}
}

func (s *RelayService) PurgePrivateTx(hash common.Hash) {
	if s.privateTxStore != nil {
		s.privateTxStore.Purge(hash)
	}
}

func (s *RelayService) GetPrivateTxGetter() PrivateTxGetter {
	var getter PrivateTxGetter
	if s.privateTxStore != nil {
		getter = s.privateTxStore
	}
	return getter
}

func (s *RelayService) SetchainEventSubFn(fn func(ch chan<- core.ChainEvent) event.Subscription) {
	if s.privateTxStore != nil {
		s.privateTxStore.SetchainEventSubFn(fn)
	}
}

func (s *RelayService) SetTxPoolChecker(checker TxPoolChecker) {
	if s.privateTxStore != nil {
		s.privateTxStore.SetTxPoolChecker(checker)
	}
}

func (s *RelayService) SetTxGetter(getter TxGetter) {
	if s.txRelay != nil {
		s.txRelay.SetTxGetter(getter)
	}
}

func (s *RelayService) PreconfEnabled() bool {
	return s.config.enablePreconf
}

func (s *RelayService) PreconfRelayAvailable() bool {
	return s.txRelay != nil && s.txRelay.multiclient != nil
}

func (s *RelayService) PrivateTxEnabled() bool {
	return s.config.enablePrivateTx
}

func (s *RelayService) AcceptPreconfTxs() bool {
	return s.config.acceptPreconfTx
}

func (s *RelayService) AcceptPrivateTxs() bool {
	return s.config.acceptPrivateTx
}

// SubmitPreconfTransaction submits a transaction for preconfirmation to block producers
func (s *RelayService) SubmitPreconfTransaction(tx *types.Transaction) error {
	if s.txRelay == nil {
		return fmt.Errorf("request dropped: %w", errRelayNotConfigured)
	}
	err := s.txRelay.SubmitTransactionForPreconf(tx)
	if err != nil {
		return fmt.Errorf("request dropped: %w", err)
	}
	return nil
}

func (s *RelayService) SubmitPreconfTransactionSync(ctx context.Context, tx *types.Transaction) error {
	if s.txRelay == nil {
		return fmt.Errorf("request dropped: %w", errRelayNotConfigured)
	}
	if err := s.txRelay.SubmitTransactionForPreconfSync(ctx, tx); err != nil {
		return fmt.Errorf("request dropped: %w", err)
	}
	return nil
}

// SubmitPrivateTransaction submits a private transaction to block producers.
// Note: We deliberately do NOT purge the private-tx hash from the store on BP
// rejection. Various types of tx rejections are possible.
// - Permanent rejection (i.e. tx which will always be rejected). As the tx has
// already passed the relay's local txpool validation, invalid txs are already
// filtered out.
// - Permanent rejection due to BP config (e.g. tx fee cap set by BP) which will
// lead to relay accepting the tx but BPs rejecting it. Such instances should be
// logged but as they can't be fixed immediately by the relay, they'll be pruned
// after the default private tx store timeout.
// - Transient rejection due to reorg or nonce race. The relay's local pool will
// evict the tx eventually once it catches up with the txpool-aware sweep.
// - Transient rejection due to BPs current mempool state (e.g. pool full). This
// will be handled by local re-submissions in BPs. If the tx still remains invalid
// it will be evicted after the default private tx store timeout.
func (s *RelayService) SubmitPrivateTransaction(tx *types.Transaction) error {
	if s.txRelay == nil {
		return fmt.Errorf("request dropped: %w", errRelayNotConfigured)
	}
	err := s.txRelay.SubmitPrivateTx(tx, true)
	if err != nil {
		// Don't add extra context to this error as it will be floated back to user
		return err
	}
	return nil
}

// CheckPreconfStatus checks the preconfirmation status of a transaction
func (s *RelayService) CheckPreconfStatus(hash common.Hash) (bool, error) {
	if s.txRelay == nil {
		return false, fmt.Errorf("request dropped: %w", errRelayNotConfigured)
	}
	preconf, err := s.txRelay.CheckTxPreconfStatus(hash)
	if err != nil {
		return false, fmt.Errorf("unable to offer preconf: %w", err)
	}
	return preconf, nil
}

// Close closes the relay service and all its components
func (s *RelayService) Close() {
	if s.txRelay != nil {
		s.txRelay.close()
	}
	if s.privateTxStore != nil {
		s.privateTxStore.Close()
	}
}
