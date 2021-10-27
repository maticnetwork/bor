package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/command/server/pprof"
	"github.com/ethereum/go-ethereum/command/server/proto"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func (s *Server) Pprof(ctx context.Context, req *proto.PprofRequest) (*proto.PprofResponse, error) {
	var payload []byte
	var headers map[string]string
	var err error

	switch req.Type {
	case proto.PprofRequest_CPU:
		payload, headers, err = pprof.CPUProfile(ctx, int(req.Seconds))
	case proto.PprofRequest_TRACE:
		payload, headers, err = pprof.Trace(ctx, int(req.Seconds))
	case proto.PprofRequest_LOOKUP:
		payload, headers, err = pprof.Profile(req.Profile, 0, 0)
	}
	if err != nil {
		return nil, err
	}

	resp := &proto.PprofResponse{
		Payload: hex.EncodeToString(payload),
		Headers: headers,
	}
	return resp, nil
}

func (s *Server) PeersAdd(ctx context.Context, req *proto.PeersAddRequest) (*proto.PeersAddResponse, error) {
	node, err := enode.Parse(enode.ValidSchemes, req.Enode)
	if err != nil {
		return nil, fmt.Errorf("invalid enode: %v", err)
	}
	srv := s.node.Server()
	if req.Trusted {
		srv.AddTrustedPeer(node)
	} else {
		srv.AddPeer(node)
	}
	return &proto.PeersAddResponse{}, nil
}

func (s *Server) PeersRemove(ctx context.Context, req *proto.PeersRemoveRequest) (*proto.PeersRemoveResponse, error) {
	node, err := enode.Parse(enode.ValidSchemes, req.Enode)
	if err != nil {
		return nil, fmt.Errorf("invalid enode: %v", err)
	}
	srv := s.node.Server()
	if req.Trusted {
		srv.RemoveTrustedPeer(node)
	} else {
		srv.RemovePeer(node)
	}
	return &proto.PeersRemoveResponse{}, nil
}

func (s *Server) PeersList(ctx context.Context, req *proto.PeersListRequest) (*proto.PeersListResponse, error) {
	resp := &proto.PeersListResponse{}

	peers := s.node.Server().PeersInfo()
	for _, p := range peers {
		resp.Peers = append(resp.Peers, peerInfoToPeer(p))
	}
	return resp, nil
}

func (s *Server) PeersStatus(ctx context.Context, req *proto.PeersStatusRequest) (*proto.PeersStatusResponse, error) {
	var peerInfo *p2p.PeerInfo
	for _, p := range s.node.Server().PeersInfo() {
		if strings.HasPrefix(p.ID, req.Enode) {
			if peerInfo != nil {
				return nil, fmt.Errorf("more than one peer with the same prefix")
			}
			peerInfo = p
		}
	}
	resp := &proto.PeersStatusResponse{}
	if peerInfo != nil {
		resp.Peer = peerInfoToPeer(peerInfo)
	}
	return resp, nil
}

func peerInfoToPeer(info *p2p.PeerInfo) *proto.Peer {
	return &proto.Peer{
		Id:      info.ID,
		Enode:   info.Enode,
		Enr:     info.ENR,
		Caps:    info.Caps,
		Name:    info.Name,
		Trusted: info.Network.Trusted,
		Static:  info.Network.Static,
	}
}

func (s *Server) AccountsNew(ctx context.Context, req *proto.AccountsNewRequest) (*proto.AccountsNewResponse, error) {
	account, err := s.keystore.NewAccount(req.Password)
	if err != nil {
		return nil, err
	}
	resp := &proto.AccountsNewResponse{
		Account: accountToProtoAccount(account),
	}
	return resp, nil
}

func (s *Server) AccountsList(ctx context.Context, req *proto.AccountsListRequest) (*proto.AccountsListResponse, error) {
	accounts := []*proto.Account{}
	for _, a := range s.keystore.Accounts() {
		accounts = append(accounts, accountToProtoAccount(a))
	}
	resp := &proto.AccountsListResponse{
		Accounts: accounts,
	}
	return resp, nil
}

func (s *Server) AccountsImport(ctx context.Context, req *proto.AccountsImportRequest) (*proto.AccountsImportResponse, error) {
	key, err := crypto.HexToECDSA(req.Key)
	if err != nil {
		return nil, err
	}
	account, err := s.keystore.ImportECDSA(key, req.Password)
	if err != nil {
		return nil, err
	}
	resp := &proto.AccountsImportResponse{
		Account: accountToProtoAccount(account),
	}
	return resp, nil
}

func accountToProtoAccount(acc accounts.Account) *proto.Account {
	return &proto.Account{
		Address: acc.Address.String(),
		Url:     acc.URL.String(),
	}
}
