package suite

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/suite"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/abi"
	"github.com/umbracle/ethgo/contract"
	"math/big"
	"testing"
)

type authTestSuite struct {
	BorServerTestSuite
}

func TestAuthTestSuite(t *testing.T) {
	suite.Run(t, new(authTestSuite))
}

func (s *authTestSuite) TestBasic() {

	ec := s.getEthClient()
	defer ec.Close()

	checkBlock, err := ec.Eth().BlockNumber()
	s.NoError(err)
	s.True(checkBlock > 0)

	authArt, err := getTestArtifact("AuthExample.json")
	s.NoError(err)
	s.True(len(authArt.Bin) > 0)

	authAbi, err := abi.NewABI(authArt.Abi)
	s.NoError(err)

	txn, err := contract.DeployContract(authAbi, common.FromHex(authArt.Bin), nil,
		contract.WithJsonRPC(ec.Eth()), contract.WithSender(s.fc))
	s.NoError(err)

	err = txn.Do()
	s.NoError(err)
	rcpt, err := txn.Wait()
	s.NoError(err)

	maybeCode, err := ec.Eth().GetCode(rcpt.ContractAddress, ethgo.Latest)
	s.NoError(err)
	s.True(len(common.FromHex(maybeCode)) > 0, "code should be deployed at contract address")
	contractAddr := rcpt.ContractAddress

	authContract := contract.NewContract(rcpt.ContractAddress, authAbi, contract.WithJsonRPC(ec.Eth()),
		contract.WithSender(s.fc))

	res, err := authContract.Call("eoa", ethgo.Latest)
	s.NoError(err)
	s.True(len(res) > 0)

	retVal0, ok := res["0"].(ethgo.Address)
	s.True(ok)
	s.Equal(ethgo.HexToAddress("0x9999999999999999999999999999999999999999"), retVal0, "initial eoa value is hard-coded in contract")

	msg := make([]byte, 65)
	// EIP-3074 messages are of the form keccak256(type ++ invoker ++ commit)
	msg[0] = 0x03
	copy(msg[13:33], contractAddr.Bytes())
	// commit.WriteToSlice(msg[33:65])
	hash := crypto.Keccak256(msg)
	sig, err := s.fc.Sign(hash)
	s.NoError(err)

	rSig := new(big.Int).SetBytes(sig[:32])
	sSig := new(big.Int).SetBytes(sig[32:64])
	vSig := new(big.Int).SetBytes([]byte{sig[64]})

	txn, err = authContract.Txn("auth", vSig, rSig, sSig)
	s.NoError(err)

	err = txn.Do()
	s.NoError(err)
	rcpt, err = txn.Wait()
	s.NoError(err)

	res, err = authContract.Call("eoa", ethgo.Latest)
	s.NoError(err)
	s.True(len(res) > 0)
	retVal0, ok = res["0"].(ethgo.Address)
	s.True(ok)
	s.Equal(s.fc.Address(), retVal0, "eoa is not set to test invoker account")

}
