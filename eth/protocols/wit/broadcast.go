package wit

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p"
)

// witnessPropagator handles witness propagation
func (p *Peer) witnessPropagator() {
	defer p.logger.Info("witness propagator stopped")

	for {
		select {
		case hashes := <-p.witBroadcast:
			if err := p.sendWitnessHashes(hashes); err != nil {
				return
			}
			p.logger.Trace("propagated witness hashes", "hashes", hashes)

		case hashes := <-p.witAnnounce:
			if err := p.announceWitnesses(hashes); err != nil {
				return
			}
			p.logger.Trace("announced witness hashes", "hashes", hashes)

		case <-p.term:
			return
		}
	}
}

// sendWitnessHashes sends witness hashes to the peer
func (p *Peer) sendWitnessHashes(hashes []common.Hash) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Filter out known hashes
	var newHashes []common.Hash
	for _, hash := range hashes {
		p.knownWitnesses.Add(hash)
		newHashes = append(newHashes, hash)
	}

	// PSP - update the data (maybe?)
	return p2p.Send(p.rw, eth.MsgWitnessHashes, newHashes)
}

// announceWitnesses announces witness hashes to the peer
func (p *Peer) announceWitnesses(hashes []common.Hash) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Announce only new hashes
	var newHashes []common.Hash
	for _, hash := range hashes {
		p.knownWitnesses.Add(hash)
		newHashes = append(newHashes, hash)
	}

	// PSP - update the data
	return p2p.Send(p.rw, eth.MsgWitnessAnnounce, newHashes)
}

// requestHandler handles request dispatching and response processing
func (p *Peer) requestHandler() {
	defer p.logger.Info("request handler stopped")

	for {
		select {
		case req := <-p.reqDispatch:
			if err := p.sendRequest(req); err != nil {
				return
			}

		case cancel := <-p.reqCancel:
			if err := p.cancelRequest(cancel); err != nil {
				return
			}

		case res := <-p.resDispatch:
			if err := p.processResponse(res); err != nil {
				return
			}

		case <-p.term:
			return
		}
	}
}

// sendRequest sends a witness request to the peer
func (p *Peer) sendRequest(req *request) error {
	return p2p.Send(p.rw, eth.MsgWitnessRequest, req)
}

// cancelRequest cancels an outstanding witness request
func (p *Peer) cancelRequest(cancel *cancel) error {
	return p2p.Send(p.rw, eth.MsgWitnessCancel, cancel)
}

// processResponse processes a received witness response
func (p *Peer) processResponse(res *response) error {
	// PSP - TODO: Implement storage or relay logic
	if res.Res == nil {
		return errors.New("response is nil")
	}

	return nil
}
