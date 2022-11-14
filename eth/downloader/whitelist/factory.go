package whitelist

import (
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/ethdb"
)

func New(chainDb ethdb.Database, whitelistFeature bool) ethereum.ChainValidator {
	var checker ethereum.ChainValidator

	if whitelistFeature {
		checker = NewService(chainDb)
	} else {
		checker = DummyService{}
	}

	return checker
}
