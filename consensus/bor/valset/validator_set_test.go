package valset

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"gotest.tools/assert"
)

var (
	// addr1 = 0x96C42C56fdb78294F96B0cFa33c92bed7D75F96a
	signer1 = "c8deb0bea5c41afe8e37b4d1bd84e31adff11b09c8c96ff4b605003cce067cd9"
	key1, _ = crypto.HexToECDSA(signer1)
	addr1   = crypto.PubkeyToAddress(key1.PublicKey)
	val1    = NewValidator(addr1, 100)

	// addr2 = 0x98925BE497f6dFF6A5a33dDA8B5933cA35262d69
	signer2 = "c8deb0bea5c41afe8e37b4d1bd84e31adff11b09c8c96ff4b605003cce067cd8"
	key2, _ = crypto.HexToECDSA(signer2)
	addr2   = crypto.PubkeyToAddress(key2.PublicKey)
	val2    = NewValidator(addr2, 200)

	//addr3 = 0x648Cf2A5b119E2c04061021834F8f75735B1D36b
	signer3 = "c8deb0bea5c41afe8e37b4d1bd84e31adff11b09c8c96ff4b605003cce067cd7"
	key3, _ = crypto.HexToECDSA(signer3)
	addr3   = crypto.PubkeyToAddress(key3.PublicKey)
	val3    = NewValidator(addr3, 300)

	//addr4 = 0x168f220B3b313D456eD4797520eFdFA9c57E6C45
	signer4 = "c8deb0bea5c41afe8e37b4d1bd84e31adff11b09c8c96ff4b605003cce067cd6"
	key4, _ = crypto.HexToECDSA(signer4)
	addr4   = crypto.PubkeyToAddress(key4.PublicKey)
	val4    = NewValidator(addr4, 400)
)

func TestIncrementProposerPriority(t *testing.T) {

	validators := []*Validator{val1, val2, val3, val4}
	valSet := NewValidatorSet(validators)

	expectedPropsers := []*Validator{val3, val2, val4, val3, val1, val4, val2, val3, val4, val4}

	for i := 0; i < 10; i++ {

		valSet.IncrementProposerPriority(1)

		assert.Equal(t, expectedPropsers[i].Address, valSet.GetProposer().Address)

	}

}

func TestRescalePriorities(t *testing.T) {

	validators := []*Validator{val1, val2, val3, val4}
	valSet := NewValidatorSet(validators)

	valSet.RescalePriorities(10)

	expectedPriorities := []int64{-6, 3, 1, 2}
	for i, val := range valSet.Validators {
		assert.Equal(t, expectedPriorities[i], val.ProposerPriority)
	}
}

func TestGetValidatorByAddressAndIndex(t *testing.T) {
	validators := []*Validator{val1, val2, val3, val4}
	valSet := NewValidatorSet(validators)

	for _, val := range valSet.Validators {
		idx, valByAddress := valSet.GetByAddress(val.Address)
		addr, valByIndex := valSet.GetByIndex(idx)

		assert.Equal(t, val.String(), valByIndex.String())
		assert.Equal(t, val.String(), valByAddress.String())
		assert.Equal(t, val.Address, common.BytesToAddress(addr))
	}
}

func TestUpdateWithChangeSet(t *testing.T) {
	validators := []*Validator{val1, val2, val3, val4}
	valSet := NewValidatorSet(validators)

	// doubled the power of val4 and halved the power of val3
	val3 = NewValidator(addr3, 150)
	val4 = NewValidator(addr4, 800)

	// check totalVotingPower before updating validator set
	assert.Equal(t, int64(1000), valSet.TotalVotingPower())

	valSet.UpdateWithChangeSet([]*Validator{val3, val4})

	// check totalVotingPower after updating validator set
	assert.Equal(t, int64(1250), valSet.TotalVotingPower())

	_, updatedVal3 := valSet.GetByAddress(val3.Address)
	assert.Equal(t, int64(150), updatedVal3.VotingPower)

	_, updatedVal4 := valSet.GetByAddress(val4.Address)
	assert.Equal(t, int64(800), updatedVal4.VotingPower)
}
