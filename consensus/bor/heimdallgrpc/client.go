package heimdallgrpc

import (
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ethereum/go-ethereum/log"
	grpcRetry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	protoV1 "github.com/maticnetwork/polyproto/heimdall"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	checkpointTypes "github.com/0xPolygon/heimdall-v2/x/checkpoint/types"
	clerkTypes "github.com/0xPolygon/heimdall-v2/x/clerk/types"
	milestoneTypes "github.com/0xPolygon/heimdall-v2/x/milestone/types"
)

const (
	stateFetchLimit = 50
	defaultTimeout  = 30 * time.Second
)

type HeimdallGRPCClient struct {
	conn                  *grpc.ClientConn
	client                protoV1.HeimdallClient
	borQueryClient        borTypes.QueryClient
	checkpointQueryClient checkpointTypes.QueryClient
	clerkQueryClient      clerkTypes.QueryClient
	milestoneQueryClient  milestoneTypes.QueryClient
}

func NewHeimdallGRPCClient(address string) *HeimdallGRPCClient {
	address = removePrefix(address)

	opts := []grpcRetry.CallOption{
		grpcRetry.WithMax(10000),
		grpcRetry.WithBackoff(grpcRetry.BackoffLinear(5 * time.Second)),
		grpcRetry.WithCodes(codes.Internal, codes.Unavailable, codes.Aborted, codes.NotFound),
	}

	conn, err := grpc.NewClient(address,
		grpc.WithStreamInterceptor(grpcRetry.StreamClientInterceptor(opts...)),
		grpc.WithUnaryInterceptor(grpcRetry.UnaryClientInterceptor(opts...)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Crit("Failed to connect to Heimdall gRPC", "error", err)
	}

	log.Info("Connected to Heimdall gRPC server", "address", address)

	return &HeimdallGRPCClient{
		conn:                  conn,
		client:                protoV1.NewHeimdallClient(conn),
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
