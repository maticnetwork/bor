package wit

import (
	"errors"

	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/p2p"
)

// PSP - TODO - in this file, check the data types passed to the p2p.Send function

// broadcastWitness is a write loop that multiplexes witness and witness announcements
// to the remote peer. The goal is to have an async writer that does not lock up
// node internals and at the same time rate limits queued data.
func (p *Peer) broadcastWitness() {
	defer p.logger.Info("witness propagator stopped")

	for {
		select {
		case witness := <-p.witBroadcast:
			if err := p.sendWitness(witness); err != nil {
				return
			}
			p.logger.Trace("propagated witness", "witness", witness)

		case <-p.term:
			return
		}
	}
}

// sendWitness sends witness to the peer
func (p *Peer) sendWitness(witness *stateless.Witness) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.knownWitnesses.Add(witness)

	return p2p.Send(p.rw, MsgWitness, witness)
}

// requestHandler handles request dispatching and response processing
func (p *Peer) requestHandler() {
	defer p.logger.Info("request handler stopped")

	for {
		select {
		// PSP - confirm if we need this in the flow
		/*
			case req := <-p.reqDispatch:
				if err := p.sendRequest(req); err != nil {
					return
				}

			case cancel := <-p.reqCancel:
				if err := p.cancelRequest(cancel); err != nil {
					return
				}
		*/

		case res := <-p.resDispatch:
			if err := p.processResponse(res); err != nil {
				return
			}

		case <-p.term:
			return
		}
	}
}

// PSP - confirm if we need this in the flow
/*
// sendRequest sends a witness request to the peer
func (p *Peer) sendRequest(req *request) error {
	return p2p.Send(p.rw, MsgWitnessRequest, req)
}

// cancelRequest cancels an outstanding witness request
func (p *Peer) cancelRequest(cancel *cancel) error {
	return p2p.Send(p.rw, MsgWitnessCancel, cancel)
}
*/

// processResponse processes a received witness response
func (p *Peer) processResponse(res *response) error {
	// PSP - TODO: Implement storage or relay logic
	if res.Res == nil {
		return errors.New("response is nil")
	}

	p.knownWitnesses.Add(res.Res.Data.(*stateless.Witness))
	p.logger.Info("added witness to known witnesses", "witness", res.Res.Data)

	return nil
}
