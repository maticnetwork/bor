package eth

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
)

func wiringChain(t *testing.T, bor *params.BorConfig) *core.BlockChain {
	t.Helper()

	config := *params.TestChainConfig
	config.Bor = bor

	chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(),
		&core.Genesis{Config: &config, GasLimit: 30_000_000},
		ethash.NewFaker(), core.DefaultConfig())
	if err != nil {
		t.Fatalf("chain: %v", err)
	}

	t.Cleanup(chain.Stop)

	return chain
}

// attachSequencer wires by role: producers only on mining nodes, consumers
// only on bor chains, anything unknown rejected, empty disabled.
func TestAttachSequencerRoles(t *testing.T) {
	borConfig := &params.BorConfig{RioBlock: big.NewInt(0)}

	t.Run("empty role is disabled", func(t *testing.T) {
		s := &Ethereum{blockchain: wiringChain(t, borConfig)}
		if err := s.attachSequencer(&ethconfig.Config{}); err != nil || s.seqPublisher != nil || s.seqConsumer != nil {
			t.Fatalf("disabled must wire nothing: %v", err)
		}
	})

	t.Run("producer without a miner is a no-op", func(t *testing.T) {
		s := &Ethereum{blockchain: wiringChain(t, borConfig)}
		cfg := &ethconfig.Config{SequencerRole: "producer"}

		if err := s.attachSequencer(cfg); err != nil || s.seqPublisher != nil {
			t.Fatalf("non-mining producer must wire nothing: %v", err)
		}
	})

	t.Run("producer with an undialable store fails the wiring", func(t *testing.T) {
		s := &Ethereum{blockchain: wiringChain(t, borConfig), miner: &miner.Miner{}}
		cfg := &ethconfig.Config{
			SequencerRole:              "producer",
			SequencerPublisherEndpoint: "bad scheme://\x00",
			SequencerConsumerEndpoint:  "bad scheme://\x00",
		}

		if err := s.attachSequencer(cfg); err == nil {
			t.Fatal("an undialable store must fail producer wiring")
		}
	})

	t.Run("consumer on a non-bor chain is disabled, not fatal", func(t *testing.T) {
		s := &Ethereum{blockchain: wiringChain(t, nil)}
		cfg := &ethconfig.Config{SequencerRole: "consumer", SequencerConsumerEndpoint: "127.0.0.1:1"}

		if err := s.attachSequencer(cfg); err != nil || s.seqConsumer != nil {
			t.Fatalf("non-bor consumer must disable, got err=%v consumer=%v", err, s.seqConsumer)
		}
	})

	t.Run("consumer on a bor chain starts", func(t *testing.T) {
		s := &Ethereum{blockchain: wiringChain(t, borConfig)}
		cfg := &ethconfig.Config{SequencerRole: "consumer", SequencerConsumerEndpoint: "127.0.0.1:1"}

		if err := s.attachSequencer(cfg); err != nil || s.seqConsumer == nil {
			t.Fatalf("bor consumer must start: %v", err)
		}

		s.seqConsumer.Close()
	})

	t.Run("unknown role is rejected", func(t *testing.T) {
		s := &Ethereum{blockchain: wiringChain(t, borConfig)}
		if err := s.attachSequencer(&ethconfig.Config{SequencerRole: "conductor"}); err == nil {
			t.Fatal("unknown role must error")
		}
	})
}

func TestConfigureMinerForSequencer(t *testing.T) {
	tests := []struct {
		name         string
		role         string
		bor          *params.BorConfig
		disabled     bool
		wantDisabled bool
	}{
		{name: "consumer on bor", role: "consumer", bor: &params.BorConfig{}, wantDisabled: true},
		{name: "consumer on non-bor", role: "consumer"},
		{name: "producer", role: "producer", bor: &params.BorConfig{}},
		{name: "sequencer disabled", bor: &params.BorConfig{}},
		{name: "preserve explicit disable", role: "producer", bor: &params.BorConfig{}, disabled: true, wantDisabled: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &ethconfig.Config{
				SequencerRole: test.role,
				Miner: miner.Config{
					DisablePendingBlock: test.disabled,
				},
			}
			configureMinerForSequencer(config, &params.ChainConfig{Bor: test.bor})

			if config.Miner.DisablePendingBlock != test.wantDisabled {
				t.Fatalf("DisablePendingBlock = %t, want %t", config.Miner.DisablePendingBlock, test.wantDisabled)
			}
		})
	}

	t.Run("nil chain config", func(t *testing.T) {
		config := &ethconfig.Config{SequencerRole: "consumer"}
		configureMinerForSequencer(config, nil)
		if config.Miner.DisablePendingBlock {
			t.Fatal("nil chain config must preserve miner pending snapshots")
		}
	})
}
