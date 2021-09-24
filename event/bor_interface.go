package event

type Mux interface {
	Subscribe(types ...interface{}) MuxSubscription
	Post(ev interface{}) error
	Stop()
}

type MuxSubscription interface {
	Chan() <-chan *TypeMuxEvent
	Unsubscribe()
	Closed() bool
}
