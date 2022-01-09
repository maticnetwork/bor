package p2p

import (
	"sync"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

type peerList struct {
	l sync.Mutex
	m map[enode.ID]struct{}
}

func newPeerList() peerList {
	return peerList{m: map[enode.ID]struct{}{}}
}

func (p *peerList) reset() {
	p.m = map[enode.ID]struct{}{}
}

func (p *peerList) len() int {
	p.l.Lock()
	defer p.l.Unlock()

	return len(p.m)
}

func (p *peerList) contains(n enode.ID) bool {
	p.l.Lock()
	defer p.l.Unlock()

	_, ok := p.m[n]
	return ok
}

func (p *peerList) add(n enode.ID) bool {
	p.l.Lock()
	defer p.l.Unlock()

	_, ok := p.m[n]
	if !ok {
		p.m[n] = struct{}{}
	}
	return !ok
}

func (p *peerList) del(n enode.ID) bool {
	p.l.Lock()
	defer p.l.Unlock()

	_, ok := p.m[n]
	if ok {
		delete(p.m, n)
	}
	return ok
}
