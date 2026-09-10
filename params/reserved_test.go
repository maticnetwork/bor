package params

import (
	"math/big"
	"strings"
	"testing"
)

func TestIsReservedBlockspace(t *testing.T) {
	t.Parallel()

	cfg := &BorConfig{ReservedBlockspaceBlock: big.NewInt(100)}
	if cfg.IsReservedBlockspace(big.NewInt(99)) {
		t.Error("should not be active at N-1")
	}
	if !cfg.IsReservedBlockspace(big.NewInt(100)) {
		t.Error("should be active at N")
	}
	if !cfg.IsReservedBlockspace(big.NewInt(101)) {
		t.Error("should be active at N+1")
	}

	// nil fork block = never active.
	none := &BorConfig{}
	if none.IsReservedBlockspace(big.NewInt(1_000_000)) {
		t.Error("nil ReservedBlockspaceBlock should never be active")
	}
}

func TestReservedBlockspaceForkOrder(t *testing.T) {
	t.Parallel()

	const reg = "0x0000000000000000000000000000000000001002"
	tests := []struct {
		name    string
		cfg     *ChainConfig
		wantErr bool
	}{
		{"nil bor", &ChainConfig{}, false},
		{"unscheduled", &ChainConfig{Bor: &BorConfig{}}, false},
		{"reserved before cancun", &ChainConfig{CancunBlock: big.NewInt(100), Bor: &BorConfig{GiuglianoBlock: big.NewInt(10), ReservedBlockspaceBlock: big.NewInt(50), ReservedRegistryContract: reg}}, true},
		{"reserved without cancun", &ChainConfig{Bor: &BorConfig{GiuglianoBlock: big.NewInt(10), ReservedBlockspaceBlock: big.NewInt(50), ReservedRegistryContract: reg}}, true},
		{"reserved before giugliano", &ChainConfig{CancunBlock: big.NewInt(10), Bor: &BorConfig{GiuglianoBlock: big.NewInt(100), ReservedBlockspaceBlock: big.NewInt(50), ReservedRegistryContract: reg}}, true},
		{"reserved without giugliano", &ChainConfig{CancunBlock: big.NewInt(10), Bor: &BorConfig{ReservedBlockspaceBlock: big.NewInt(50), ReservedRegistryContract: reg}}, true},
		{"reserved at cancun and giugliano", &ChainConfig{CancunBlock: big.NewInt(50), Bor: &BorConfig{GiuglianoBlock: big.NewInt(50), ReservedBlockspaceBlock: big.NewInt(50), ReservedRegistryContract: reg}}, false},
		{"reserved after cancun and giugliano", &ChainConfig{CancunBlock: big.NewInt(10), Bor: &BorConfig{GiuglianoBlock: big.NewInt(10), ReservedBlockspaceBlock: big.NewInt(50), ReservedRegistryContract: reg}}, false},
		{"scheduled without registry", &ChainConfig{CancunBlock: big.NewInt(10), Bor: &BorConfig{GiuglianoBlock: big.NewInt(10), ReservedBlockspaceBlock: big.NewInt(50)}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.checkReservedBlockspaceForkOrder(); (err != nil) != tt.wantErr {
				t.Errorf("checkReservedBlockspaceForkOrder() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDescriptionReservedBlockspace(t *testing.T) {
	t.Parallel()

	// When scheduled, the startup banner must print the fork (hardfork-rollout
	// review requires a visible activation height).
	scheduled := &ChainConfig{
		ChainID: big.NewInt(137),
		Bor:     &BorConfig{ReservedBlockspaceBlock: big.NewInt(500)},
	}
	if got := scheduled.Description(); !strings.Contains(got, "ReservedBlockspace") || !strings.Contains(got, "500") {
		t.Errorf("banner should advertise ReservedBlockspace #500, got:\n%s", got)
	}

	// Unscheduled (nil) must not print the line.
	unscheduled := &ChainConfig{ChainID: big.NewInt(137), Bor: &BorConfig{}}
	if strings.Contains(unscheduled.Description(), "ReservedBlockspace") {
		t.Error("banner must omit ReservedBlockspace when unscheduled")
	}
}
