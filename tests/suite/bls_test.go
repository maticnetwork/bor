package suite

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/influxdata/influxdb/stress"
	"github.com/stretchr/testify/suite"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/abi"
	"github.com/umbracle/ethgo/contract"
	"math/big"
	"testing"
)

type blsTestSuite struct {
	BorServerTestSuite
}

func TestBLSTestSuite(t *testing.T) {
	suite.Run(t, new(blsTestSuite))
}

func (s *blsTestSuite) TestBasic() {

	ec := s.getEthClient()
	defer ec.Close()

	checkBlock, err := ec.Eth().BlockNumber()
	s.NoError(err)
	s.True(checkBlock > 0)

	blsArt, err := getTestArtifact("BLS.json")
	s.NoError(err)
	s.True(len(blsArt.Bin) > 0)

	blsAbi, err := abi.NewABI(blsArt.Abi)
	s.NoError(err)

	txn, err := contract.DeployContract(blsAbi, common.FromHex(blsArt.Bin), nil,
		contract.WithJsonRPC(ec.Eth()), contract.WithSender(s.fc))
	s.NoError(err)

	err = txn.Do()
	s.NoError(err)
	rcpt, err := txn.Wait()
	s.NoError(err)

	maybeCode, err := ec.Eth().GetCode(rcpt.ContractAddress, ethgo.Latest)
	s.NoError(err)
	s.True(len(common.FromHex(maybeCode)) > 0, "code should be deployed at contract address")

	blsContract := contract.NewContract(rcpt.ContractAddress, blsAbi, contract.WithJsonRPC(ec.Eth()))

	// function hashToPoint(bytes32 domain, bytes memory message) external view returns (uint256[2] memory)
	domain := [32]byte{}
	message := common.FromHex("deadbeefdeadbeefdeadbeefdeadbeef")
	tm := stress.NewTimer()
	res, err := blsContract.Call("hashToPoint", ethgo.Latest, domain, message)
	tm.StopTimer()
	s.NoError(err)
	s.True(len(res) > 0)
	println(tm.Elapsed().Milliseconds())

	retVal0, ok := res["0"].([2]*big.Int)
	s.True(ok)
	_ = retVal0

}
