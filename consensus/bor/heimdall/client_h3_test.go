package heimdall

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"
)

func TestNewHeimdallClientFetchStatusOverQUIC(t *testing.T) {
	t.Parallel()

	tlsConf, err := newTestQUICServerTLSConfig()
	require.NoError(t, err)

	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	require.NoError(t, err)
	udpConn, err := net.ListenUDP("udp", udpAddr)
	require.NoError(t, err)
	port := udpConn.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, udpConn.Close())

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(ctypes.SyncInfo{}))
	})

	server := &http3.Server{
		Addr:      net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		TLSConfig: tlsConf,
		Handler:   mux,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	t.Cleanup(func() {
		require.NoError(t, server.Close())
		select {
		case err := <-errCh:
			require.ErrorIs(t, err, http.ErrServerClosed)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for QUIC server shutdown")
		}
	})

	time.Sleep(150 * time.Millisecond)

	client, err := NewHeimdallClient("h3://127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	require.NoError(t, err)
	status, err := client.FetchStatus(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status)
}

func TestNewHeimdallClientRejectsInvalidQUICEndpoint(t *testing.T) {
	t.Parallel()

	client, err := NewHeimdallClient("h3:///status-only", 5*time.Second)
	require.Nil(t, client)
	require.ErrorContains(t, err, "empty host")
}

func TestExternalHeimdallFetchStatusOverQUIC(t *testing.T) {
	endpoint := os.Getenv("BOR_HEIMDALL_H3_URL")
	if endpoint == "" {
		t.Skip("set BOR_HEIMDALL_H3_URL to run against a live Heimdall QUIC sidecar")
	}

	client, err := NewHeimdallClient(endpoint, 10*time.Second)
	require.NoError(t, err)

	status, err := client.FetchStatus(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status)
}

func TestExternalHeimdallEndpointSuiteOverQUIC(t *testing.T) {
	endpoint := os.Getenv("BOR_HEIMDALL_H3_URL")
	if endpoint == "" {
		t.Skip("set BOR_HEIMDALL_H3_URL to run against a live Heimdall QUIC sidecar")
	}

	client, err := NewHeimdallClient(endpoint, 10*time.Second)
	require.NoError(t, err)

	status, err := client.FetchStatus(t.Context())
	require.NoError(t, err)
	require.NotNil(t, status)
	require.False(t, status.CatchingUp)

	latestSpan, err := client.GetLatestSpan(t.Context())
	require.NoError(t, err)
	require.NotNil(t, latestSpan)
	require.GreaterOrEqual(t, latestSpan.Id, uint64(0))
	require.GreaterOrEqual(t, latestSpan.EndBlock, latestSpan.StartBlock)

	spanByID, err := client.GetSpan(t.Context(), latestSpan.Id)
	require.NoError(t, err)
	require.NotNil(t, spanByID)
	require.Equal(t, latestSpan.Id, spanByID.Id)
	require.Equal(t, latestSpan.StartBlock, spanByID.StartBlock)
	require.Equal(t, latestSpan.EndBlock, spanByID.EndBlock)

	checkpointCount, err := client.FetchCheckpointCount(t.Context())
	require.NoError(t, err)
	require.GreaterOrEqual(t, checkpointCount, int64(0))

	milestoneCount, err := client.FetchMilestoneCount(t.Context())
	require.NoError(t, err)
	require.GreaterOrEqual(t, milestoneCount, int64(0))

	events, err := client.StateSyncEvents(t.Context(), 1, time.Now().Add(24*time.Hour).Unix())
	require.NoError(t, err)
	require.NotNil(t, events)

	if os.Getenv("BOR_HEIMDALL_EXPECT_RICH_STATE") != "" {
		require.Greater(t, checkpointCount, int64(0))
		require.Greater(t, milestoneCount, int64(0))
		require.NotEmpty(t, events)

		checkpoint, err := client.FetchCheckpoint(t.Context(), -1)
		require.NoError(t, err)
		require.NotNil(t, checkpoint)
		require.GreaterOrEqual(t, checkpoint.EndBlock, checkpoint.StartBlock)

		milestone, err := client.FetchMilestone(t.Context())
		require.NoError(t, err)
		require.NotNil(t, milestone)
		require.GreaterOrEqual(t, milestone.EndBlock, milestone.StartBlock)
	}
}

func newTestQUICServerTLSConfig() (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  priv,
		}},
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{http3.NextProtoH3},
	}, nil
}
