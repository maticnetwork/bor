package filters

import "github.com/ethereum/go-ethereum"

type FilterCriteria ethereum.FilterQuery

func (crit FilterCriteria) query() ethereum.FilterQuery { return ethereum.FilterQuery(crit) }
