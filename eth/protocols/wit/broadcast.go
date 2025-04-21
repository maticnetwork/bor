package wit

import "github.com/ethereum/go-ethereum/common"

// broadcastWitness is a write loop that multiplexes witness and witness announcements
// to the remote peer. The goal is to have an async writer that does not lock up
// node internals and at the same time rate limits queued data.
func (p *Peer) broadcastWitness() {
	defer p.logger.Info("witness propagator stopped")

	for {
		select {
		case witness := <-p.queuedWitness:
			if err := p.sendNewWitness(witness); err != nil {
				return
			}
			p.logger.Trace("propagated witness", "witness", witness)

		case hash := <-p.queuedWitnessAnns:
			if err := p.sendNewWitnessHashes([]common.Hash{hash}); err != nil {
				return
			}
			p.logger.Trace("propagated witness hashes", "hashes", hash)

		case <-p.term:
			return
		}
	}
}
