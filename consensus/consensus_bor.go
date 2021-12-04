package consensus

type ConsensusNoPoW interface {
	SetBlockchain(chain ChainHeaderReader)
}
