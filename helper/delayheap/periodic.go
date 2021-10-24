package delayheap

import (
	"sync"
	"time"
)

type PeriodicDispatcher struct {
	l          sync.RWMutex
	delayHeap  *DelayHeap
	dispatcher Dispatcher
	updateCh   chan struct{}
	closeCh    chan struct{}
}

type Dispatcher interface {
	Enqueue(HeapNode)
}

func NewPeriodicDispatcher(dispatcher Dispatcher) *PeriodicDispatcher {
	return &PeriodicDispatcher{
		l:          sync.RWMutex{},
		delayHeap:  NewDelayHeap(),
		updateCh:   make(chan struct{}),
		closeCh:    make(chan struct{}),
		dispatcher: dispatcher,
	}
}

func (p *PeriodicDispatcher) ContainsID(id string) bool {
	p.l.Lock()
	defer p.l.Unlock()

	return p.delayHeap.ContainsID(id)
}

func (p *PeriodicDispatcher) Add(dataNode HeapNode, next time.Time) {
	p.l.Lock()
	p.delayHeap.Push(dataNode, next)
	p.l.Unlock()

	// Notify an update.
	select {
	case p.updateCh <- struct{}{}:
	default:
	}
}

// nextDelayedEval returns the next delayed eval to launch and when it should be enqueued.
func (p *PeriodicDispatcher) nextDelayedEval() (HeapNode, time.Time) {
	p.l.RLock()
	defer p.l.RUnlock()

	// If there is nothing wait for an update.
	if p.delayHeap.Length() == 0 {
		return nil, time.Time{}
	}
	nextEval := p.delayHeap.Peek()
	if nextEval == nil {
		return nil, time.Time{}
	}
	eval := nextEval.Node
	return eval, nextEval.WaitUntil
}

// Run is a long-lived function that waits till a time deadline is met for
// pending enqueued evaluations
func (p *PeriodicDispatcher) Run() {
	go p.run()
}

func (p *PeriodicDispatcher) run() {
	var timerChannel <-chan time.Time
	var delayTimer *time.Timer
	for {
		eval, waitUntil := p.nextDelayedEval()
		if waitUntil.IsZero() {
			timerChannel = nil
		} else {
			launchDur := waitUntil.Sub(time.Now().UTC())
			if delayTimer == nil {
				delayTimer = time.NewTimer(launchDur)
			} else {
				delayTimer.Reset(launchDur)
			}
			timerChannel = delayTimer.C
		}

		select {
		case <-p.closeCh:
			return
		case <-timerChannel:
			// remove from the heap since we can enqueue it now
			p.l.Lock()
			p.delayHeap.Remove(eval)
			p.dispatcher.Enqueue(eval)
			p.l.Unlock()
		case <-p.updateCh:
			continue
		}
	}
}
