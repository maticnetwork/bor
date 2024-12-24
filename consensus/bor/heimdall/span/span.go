package span

import (
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
)

// Span Bor represents a current bor span
type Span struct {
	ID         uint64 `protobuf:"varint,1,opt,name=id,proto3" json:"id,omitempty"`
	StartBlock uint64 `protobuf:"varint,2,opt,name=start_block,json=startBlock,proto3" json:"start_block,omitempty"`
	EndBlock   uint64 `protobuf:"varint,3,opt,name=end_block,json=endBlock,proto3" json:"end_block,omitempty"`
}

// HeimdallSpan represents span from heimdall APIs
type HeimdallSpan struct {
	Span
	ValidatorSet      valset.ValidatorSet `protobuf:"bytes,4,opt,name=validator_set,json=validatorSet,proto3" json:"validator_set"`
	SelectedProducers []valset.Validator  `protobuf:"bytes,5,rep,name=selected_producers,json=selectedProducers,proto3" json:"selected_producers"`
	ChainID           string              `protobuf:"bytes,6,opt,name=bor_chain_id,json=borChainId,proto3" json:"bor_chain_id,omitempty"`
}
