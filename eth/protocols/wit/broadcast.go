package wit

// PSP - TODO - in this file, check the data types passed to the p2p.Send function

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

		case <-p.term:
			return
		}
	}
}
