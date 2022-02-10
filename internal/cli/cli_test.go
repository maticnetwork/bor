package cli

import (
	"testing"

	"github.com/ethereum/go-ethereum/internal/cli/flagset"
	"github.com/ethereum/go-ethereum/log"
	"github.com/imdario/mergo"
	"github.com/stretchr/testify/assert"
)

func TestFlagOverride(t *testing.T) {

	// declaring a config
	type Config struct {
		val uint64
	}

	defaultConfig := func() *Config {
		return &Config{val: 10}
	}

	merge := func(t *testing.T, src, dst *Config) {
		err := mergo.Merge(dst, src, mergo.WithOverride, mergo.WithAppendSlice)
		assert.NoError(t, err)
	}

	config1 := defaultConfig() // this is the default config
	config2 := &Config{val: 0} // this config has values provided by the user

	f := flagset.NewFlagSet("")
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:  "flag",
		Value: &config2.val,
	})

	err := f.Parse([]string{})
	log.Info("Parsing config", "config1", config1.val, "config2", config2.val)
	assert.NoError(t, err)
	merge(t, config2, config1)
	log.Info("Post merge config", "config1", config1.val, "config2", config2.val)
	assert.Equal(t, uint64(0), config1.val) // this will be overridden to 0, but should not

}
