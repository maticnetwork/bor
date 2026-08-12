package snap

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/p2p"
)

var (
	errSnapDisconnected        = errors.New("disconnected")
	errSnapDanglingResponse    = errors.New("response to non-existent request")
	errSnapMismatchingResponse = errors.New("mismatching response type")
)

// Request tracks an in-flight snap request that expects a response to be
// delivered back to the caller on a chosen sink.
type Request struct {
	peer *Peer
	id   uint64

	sink   chan *Response
	cancel chan struct{}

	code uint64
	want uint64
	data interface{}

	Peer string
	Sent time.Time
}

// Close aborts an in-flight request and discards any later response.
func (r *Request) Close() error {
	if r == nil {
		return nil
	}
	if r.peer != nil {
		r.peer.untrackRequest(r.id, r.code)
	}
	if r.cancel != nil {
		select {
		case <-r.cancel:
		default:
			close(r.cancel)
		}
	}
	return nil
}

// Response is the snap reply delivered for a tracked request.
type Response struct {
	id   uint64
	recv time.Time
	code uint64

	Req  *Request
	Res  interface{}
	Time time.Duration
	Done chan error
}

type trackedResponse interface {
	requestID() uint64
}

func (p *Peer) dispatchRequest(req *Request) error {
	if req == nil {
		return errors.New("nil request")
	}
	req.cancel = make(chan struct{})
	req.peer = p
	req.Peer = p.id
	req.Sent = time.Now()

	p.pendingLock.Lock()
	if p.pending == nil {
		p.pending = make(map[uint64]*Request)
	}
	p.pending[req.id] = req
	p.pendingLock.Unlock()

	requestTracker.Track(p.id, p.version, req.code, req.want, req.id)
	if err := p2p.Send(p.rw, req.code, req.data); err != nil {
		p.untrackRequest(req.id, req.code)
		return err
	}
	return nil
}

func (p *Peer) dispatchResponse(code uint64, packet trackedResponse) (bool, error) {
	if packet == nil {
		return false, nil
	}
	id := packet.requestID()

	p.pendingLock.Lock()
	req := p.pending[id]
	if req != nil {
		delete(p.pending, id)
	}
	p.pendingLock.Unlock()

	requestTracker.Fulfil(p.id, p.version, code, id)
	if req == nil {
		return false, nil
	}
	if req.want != code {
		return true, fmt.Errorf("%w: have %d want %d", errSnapMismatchingResponse, code, req.want)
	}
	res := &Response{
		id:   id,
		recv: time.Now(),
		code: code,
		Req:  req,
		Res:  packet,
		Time: time.Since(req.Sent),
		Done: make(chan error, 1),
	}
	select {
	case <-req.cancel:
		return true, nil
	default:
	}
	select {
	case req.sink <- res:
		return true, <-res.Done
	case <-req.cancel:
		return true, nil
	}
}

func (p *Peer) untrackRequest(id, code uint64) {
	p.pendingLock.Lock()
	req := p.pending[id]
	if req != nil {
		delete(p.pending, id)
	}
	p.pendingLock.Unlock()
	if req != nil {
		requestTracker.Fulfil(p.id, p.version, code, id)
	}
}
