package filters

import (
	"context"
	"errors"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// SetChainConfig sets chain config
func (api *FilterAPI) SetChainConfig(chainConfig *params.ChainConfig) {
	api.chainConfig = chainConfig
}

func (api *FilterAPI) GetBorBlockLogs(ctx context.Context, crit FilterCriteria) ([]*types.Log, error) {
	if api.chainConfig == nil {
		return nil, errors.New("no chain config found. Proper PublicFilterAPI initialization required")
	}

	// get sprint from bor config
	borConfig := api.chainConfig.Bor

	var filter *BorBlockLogsFilter
	if crit.BlockHash != nil {
		// Block filter requested, construct a single-shot filter
		filter = NewBorBlockLogsFilter(api.sys.backend, borConfig, *crit.BlockHash, crit.Addresses, crit.Topics)
	} else {
		// Convert the RPC block numbers into internal representations
		begin := rpc.LatestBlockNumber.Int64()
		if crit.FromBlock != nil {
			begin = crit.FromBlock.Int64()
		}

		end := rpc.LatestBlockNumber.Int64()
		if crit.ToBlock != nil {
			end = crit.ToBlock.Int64()
		}
		if begin > 0 && end > 0 && begin > end {
			return nil, errInvalidBlockRange
		}
		head := api.sys.backend.CurrentHeader().Number.Uint64()
		if err := checkBlockRangeLimit(begin, end, head, api.sys.cfg.RangeLimit); err != nil {
			return nil, err
		}
		// Construct the range filter
		filter = NewBorBlockLogsRangeFilter(api.sys.backend, borConfig, begin, end, crit.Addresses, crit.Topics)
	}

	// Run the filter and return all the logs
	logs, err := filter.Logs(ctx)
	if err != nil {
		return nil, err
	}

	return returnLogs(logs), err
}

// NewDeposits send a notification each time a new deposit received from bridge.
func (api *FilterAPI) NewDeposits(ctx context.Context, crit ethereum.StateSyncFilter) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	go func() {
		stateSyncData := make(chan *types.StateSyncData, 10)
		stateSyncSub := api.events.SubscribeNewDeposits(stateSyncData)

		stop := make(chan struct{})
		defer close(stop)
		client := notifyAsync(notifier, rpcSub.ID, stop)

		//nolint:staticcheck
		for {
			select {
			case h := <-stateSyncData:
				if h != nil && (crit.ID == h.ID || crit.Contract == h.Contract ||
					(crit.ID == 0 && crit.Contract == common.Address{})) {
					client.send(h)
				}
			case <-client.failed:
				stateSyncSub.Unsubscribe()
				return
			case <-rpcSub.Err():
				stateSyncSub.Unsubscribe()
				return
			case <-notifier.Closed():
				stateSyncSub.Unsubscribe()
				return
			}
		}
	}()

	return rpcSub, nil
}
