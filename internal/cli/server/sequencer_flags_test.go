package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSequencerFlags(t *testing.T) {
	t.Parallel()

	var c Command

	args := []string{
		"--sequencer.enabled",
		"--sequencer.publisher-endpoint", "127.0.0.1:9550",
		"--sequencer.consumer-endpoint", "127.0.0.1:9551",
		"--sequencer.poll", "150ms",
	}

	require.NoError(t, c.extractFlags(args))
	require.True(t, c.config.Sequencer.Enabled)
	require.Equal(t, "127.0.0.1:9550", c.config.Sequencer.PublisherEndpoint)
	require.Equal(t, "127.0.0.1:9551", c.config.Sequencer.ConsumerEndpoint)
	require.Equal(t, 150*time.Millisecond, c.config.Sequencer.Poll)
}

func TestSequencerDefaults(t *testing.T) {
	t.Parallel()

	def := DefaultConfig()
	require.False(t, def.Sequencer.Enabled)
	require.Equal(t, "", def.Sequencer.PublisherEndpoint)
	require.Equal(t, "", def.Sequencer.ConsumerEndpoint)
	require.Equal(t, 200*time.Millisecond, def.Sequencer.Poll)
}

func TestSequencerSettingsDerivation(t *testing.T) {
	t.Parallel()

	base := func(enabled, sealer bool, pubEndpoint, consEndpoint string) *Config {
		c := DefaultConfig()
		c.Sequencer.Enabled = enabled
		c.Sequencer.PublisherEndpoint = pubEndpoint
		c.Sequencer.ConsumerEndpoint = consEndpoint
		c.Sealer.Enabled = sealer

		return c
	}

	cases := []struct {
		name     string
		config   *Config
		wantRole string
		wantErr  bool
	}{
		{"disabled", base(false, true, "h:1", "h:2"), "", false},
		{"nil block", &Config{Sealer: DefaultConfig().Sealer}, "", false},
		{"enabled without publisher endpoint", base(true, true, "", "h:2"), "", true},
		{"enabled without consumer endpoint", base(true, true, "h:1", ""), "", true},
		{"enabled mining node", base(true, true, "h:1", "h:2"), "producer", false},
		{"enabled non-mining node", base(true, false, "h:1", "h:2"), "consumer", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, pubEndpoint, consEndpoint, poll, err := tc.config.sequencerSettings()

			if tc.wantErr {
				require.Error(t, err)
				require.Empty(t, role, "error return must not report a sequencer role")

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantRole, role)

			if role != "" {
				require.Equal(t, "h:1", pubEndpoint)
				require.Equal(t, "h:2", consEndpoint)
				require.Equal(t, 200*time.Millisecond, poll)
			}
		})
	}
}

func TestSequencerPollFromConfigFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "seq.toml")
	require.NoError(t, os.WriteFile(path, []byte("[sequencer]\nenabled = true\npublisher-endpoint = \"h:1\"\nconsumer-endpoint = \"h:2\"\npoll = \"75ms\"\n"), 0o600))

	var c Command

	require.NoError(t, c.extractFlags([]string{"--config", path}))
	require.True(t, c.config.Sequencer.Enabled)
	require.Equal(t, "h:1", c.config.Sequencer.PublisherEndpoint)
	require.Equal(t, "h:2", c.config.Sequencer.ConsumerEndpoint)
	require.Equal(t, 75*time.Millisecond, c.config.Sequencer.Poll)
}

// An HCL/JSON config without a [sequencer] block decodes the field as nil
// (only the TOML path starts from DefaultConfig); flag registration must
// install the defaults rather than dereference it.
func TestSequencerFlagsWithoutConfigBlock(t *testing.T) {
	t.Parallel()

	var c Command

	cfg := DefaultConfig()
	cfg.Sequencer = nil

	require.NotNil(t, c.Flags(cfg))
	require.NotNil(t, c.cliConfig.Sequencer)
	require.False(t, c.cliConfig.Sequencer.Enabled)
	require.Equal(t, 200*time.Millisecond, c.cliConfig.Sequencer.Poll)
}

// The audit depth only reaches the node when the integration is on; zero
// leaves the sequencer package default in place.
func TestSequencerAuditWindow(t *testing.T) {
	cases := []struct {
		name   string
		config *Config
		want   uint64
	}{
		{name: "no sequencer block", config: &Config{}, want: 0},
		{name: "disabled", config: &Config{Sequencer: &SequencerConfig{AuditWindow: 900}}, want: 0},
		{name: "enabled", config: &Config{Sequencer: &SequencerConfig{Enabled: true, AuditWindow: 900}}, want: 900},
		{name: "enabled and unset", config: &Config{Sequencer: &SequencerConfig{Enabled: true}}, want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.sequencerAuditWindow(); got != tc.want {
				t.Fatalf("sequencerAuditWindow() = %d, want %d", got, tc.want)
			}
		})
	}
}
