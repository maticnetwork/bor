package p2p

// PeerInfo represents a short summary of the information known about a connected
// peer. Sub-protocol independent fields are contained and initialized here, with
// protocol specifics delegated to all connected sub-protocols.
type PeerInfo struct {
	ENR     string   `json:"enr,omitempty"` // Ethereum Node Record
	Enode   string   `json:"enode"`         // Node URL
	ID      string   `json:"id"`            // Unique node identifier
	Name    string   `json:"name"`          // Name of the node, including client type, version, OS, custom data
	Caps    []string `json:"caps"`          // Protocols advertised by this peer
	Network struct {
		LocalAddress  string `json:"localAddress"`  // Local endpoint of the TCP data connection
		RemoteAddress string `json:"remoteAddress"` // Remote endpoint of the TCP data connection
		Inbound       bool   `json:"inbound"`
		Trusted       bool   `json:"trusted"`
		Static        bool   `json:"static"`
	} `json:"network"`
	Protocols map[string]interface{} `json:"protocols"` // Sub-protocol specific metadata fields
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *Peer) Info() *PeerInfo {
	// Gather the protocol capabilities
	var caps []string
	for _, cap := range p.Caps() {
		caps = append(caps, cap.String())
	}
	// Assemble the generic peer metadata
	info := &PeerInfo{
		Enode:     p.Node().URLv4(),
		ID:        p.ID().String(),
		Name:      p.Fullname(),
		Caps:      caps,
		Protocols: make(map[string]interface{}),
	}
	if p.Node().Seq() > 0 {
		info.ENR = p.Node().String()
	}
	info.Network.LocalAddress = p.LocalAddr().String()
	info.Network.RemoteAddress = p.RemoteAddr().String()
	info.Network.Inbound = p.rw.is(inboundConn)
	info.Network.Trusted = p.rw.is(trustedConn)
	info.Network.Static = p.rw.is(staticDialedConn)

	// Gather all the running protocol infos
	for _, proto := range p.running {
		protoInfo := interface{}("unknown")
		if query := proto.proto.PeerInfo; query != nil {
			if metadata := query(p.ID()); metadata != nil {
				protoInfo = metadata
			} else {
				protoInfo = "handshake"
			}
		}
		info.Protocols[proto.proto.Name] = protoInfo
	}
	return info
}
