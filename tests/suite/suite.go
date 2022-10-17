package suite

import (
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/internal/cli/server"
	"github.com/stretchr/testify/suite"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/compiler"
	"github.com/umbracle/ethgo/jsonrpc"
	"io/ioutil"
	"net/url"
	"path/filepath"
	"strings"
)

type faucetSigner struct {
	ks *keystore.KeyStore
}

func (f faucetSigner) Address() ethgo.Address {
	return ethgo.Address(f.ks.Accounts()[0].Address)
}

func (f faucetSigner) Sign(hash []byte) ([]byte, error) {
	return f.ks.SignHash(f.ks.Accounts()[0], hash)
}

var _ ethgo.Key = &faucetSigner{}

type BorServerTestSuite struct {
	suite.Suite

	srv *server.Server
	cfg *server.Config

	fc faucetSigner
}

func (s *BorServerTestSuite) SetupSuite() {

	s.cfg = server.DefaultConfig()

	s.cfg.DataDir, _ = ioutil.TempDir("", "bor-suite-test-*")
	s.cfg.Accounts.UseLightweightKDF = true

	s.cfg.JsonRPC.Http = &server.APIConfig{
		Enabled: true,
		Port:    8545, // TODO: rando port?
		Host:    "localhost",
		API:     []string{"eth", "net", "web3", "txpool"},
		Cors:    []string{"localhost"},
		VHost:   []string{"localhost"},
	}

	// enable developer mode
	s.cfg.Developer.Enabled = true
	s.cfg.Developer.Period = 2 // block time

	var err error
	s.srv, err = server.NewServer(s.cfg)
	s.NoError(err)

	// TODO: oblique way to get at faucet account - any easier way?
	// datadirDefaultKeyStore is "keystore"
	ks := keystore.NewKeyStore(filepath.Join(s.cfg.DataDir, "keystore"), keystore.LightScryptN, keystore.LightScryptP)
	s.True(len(ks.Accounts()) > 0)
	err = ks.Unlock(ks.Accounts()[0], "")
	s.NoError(err)

	s.fc = faucetSigner{ks: ks}

	return
}

func (s *BorServerTestSuite) TearDownSuite() {
	if s.srv != nil {
		s.srv.Stop()
	}
}

func (s *BorServerTestSuite) getClient() *ethclient.Client {
	u := url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%v:%v", s.cfg.JsonRPC.Http.Host, s.cfg.JsonRPC.Http.Port),
	}
	ec, err := ethclient.Dial(u.String())
	s.NoError(err)
	return ec
}

func (s *BorServerTestSuite) getEthClient() *jsonrpc.Client {
	u := url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%v:%v", s.cfg.JsonRPC.Http.Host, s.cfg.JsonRPC.Http.Port),
	}
	jc, err := jsonrpc.NewClient(u.String())
	s.NoError(err)
	return jc
}

// cribbing from ethgo
type jsonArtifact struct {
	Bytecode string          `json:"bytecode"`
	Abi      json.RawMessage `json:"abi"`
}

func getTestArtifact(name string) (art *compiler.Artifact, err error) {

	var jsonBytes []byte
	if jsonBytes, err = ioutil.ReadFile(filepath.Join("testdata", name)); err != nil {
		return
	}
	var jart jsonArtifact
	if err = json.Unmarshal(jsonBytes, &jart); err != nil {
		return
	}

	bc := jart.Bytecode
	if !strings.HasPrefix(bc, "0x") {
		bc = "0x" + bc
	}
	art = &compiler.Artifact{
		Abi: string(jart.Abi),
		Bin: bc,
	}
	return
}
