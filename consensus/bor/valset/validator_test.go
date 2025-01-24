package valset

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseValidators(t *testing.T) {
	t.Parallel()

	vals := GetValidators()

	validatorsBytes := make([]byte, 40*len(vals))
	for i, validator := range vals {
		copy(validatorsBytes[i*40:], validator.HeaderBytes())
	}

	parsedVals, _ := ParseValidators(validatorsBytes)

	require.Equal(t, len(vals), len(parsedVals), "parsed vals does not match")

	for i, _ := range parsedVals {
		require.Equal(t, vals[i].ID, parsedVals[i].ID)
		require.Equal(t, vals[i].Address, parsedVals[i].Address)
		require.Equal(t, vals[i].VotingPower, parsedVals[i].VotingPower)
	}
}
