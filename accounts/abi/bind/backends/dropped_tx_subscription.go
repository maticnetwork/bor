package backends

import (
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/event"
)

func (fb *filterBackend) SubscribeDropTxsEvent(ch chan<- core.DropTxsEvent) event.Subscription {
	return nullSubscription()
}
