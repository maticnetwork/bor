package heimdallgrpc

import (
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ethereum/go-ethereum/log"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	checkpointTypes "github.com/0xPolygon/heimdall-v2/x/checkpoint/types"
	clerkTypes "github.com/0xPolygon/heimdall-v2/x/clerk/types"
	milestoneTypes "github.com/0xPolygon/heimdall-v2/x/milestone/types"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
)

const (
	stateFetchLimit = 50
)

type HeimdallGRPCClient struct {
	conn                  *grpc.ClientConn
	borQueryClient        borTypes.QueryClient
	checkpointQueryClient checkpointTypes.QueryClient
	clerkQueryClient      clerkTypes.QueryClient
	milestoneQueryClient  milestoneTypes.QueryClient
}

func NewHeimdallGRPCClient(address string) *HeimdallGRPCClient {
	address = removePrefix(address)

	opts := []grpc_retry.CallOption{
		grpc_retry.WithMax(10000),
		grpc_retry.WithBackoff(grpc_retry.BackoffLinear(5 * time.Second)),
		grpc_retry.WithCodes(codes.Internal, codes.Unavailable, codes.Aborted, codes.NotFound),
	}

	conn, err := grpc.NewClient(address,
		grpc.WithStreamInterceptor(grpc_retry.StreamClientInterceptor(opts...)),
		grpc.WithUnaryInterceptor(grpc_retry.UnaryClientInterceptor(opts...)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Crit("Failed to connect to Heimdall gRPC", "error", err)
	}

	log.Info("Connected to Heimdall gRPC server", "address", address)

	return &HeimdallGRPCClient{
		conn:                  conn,
		borQueryClient:        borTypes.NewQueryClient(conn),
		checkpointQueryClient: checkpointTypes.NewQueryClient(conn),
		clerkQueryClient:      clerkTypes.NewQueryClient(conn),
		milestoneQueryClient:  milestoneTypes.NewQueryClient(conn),
	}
}

func (h *HeimdallGRPCClient) Close() {
	log.Debug("Shutdown detected, Closing Heimdall gRPC client")
	h.conn.Close()
}

// removePrefix removes the http:// or https:// prefix from the address, if present.
func removePrefix(address string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return address[strings.Index(address, "//")+2:]
	}
	return address
}
