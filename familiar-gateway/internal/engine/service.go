package engine

import (
	"context"

	pb "github.com/familiar/gateway/proto/engine"
)

// Service defines the engine operations used by the pipeline and other consumers.
// This interface allows mocking the engine in tests without a real gRPC connection.
// The engine migration deleted the gRPC client; the only live
// implementation is internal/memengine.MemEngine. The interface
// remains as the type the pipeline + adapters hold so memengine
// stays swappable for a fake in tests, but no production caller
// dials anything across a wire.
type Service interface {
	// Close releases any underlying resources. The in-process
	// MemEngine is a no-op; mock implementations in tests may do
	// real cleanup.
	Close() error
	Ping(ctx context.Context) (*pb.PingResponse, error)
	AssembleContext(ctx context.Context, sessionID, userMsg string, vis *pb.VisibilityContext, memBudget, convBudget uint32, queryVec []float32) (*pb.AssembleContextResponse, error)
	CommitFacts(ctx context.Context, sessionID string, facts []*pb.FactProto) (*pb.CommitFactsResponse, error)
	QueryMemory(ctx context.Context, req *pb.MemoryQueryRequest) (*pb.MemoryQueryResponse, error)
	DeleteFact(ctx context.Context, sessionID, factID string, vis *pb.VisibilityContext) (*pb.DeleteFactResponse, error)
	UpdateFact(ctx context.Context, sessionID, factID, newContent string, newEmbedding []float32, vis *pb.VisibilityContext) (*pb.UpdateFactResponse, error)
	VaultGet(ctx context.Context, key string) (string, bool, error)
	VaultSet(ctx context.Context, key, value string) error
	GetAgentIdentity(ctx context.Context) (*pb.AgentIdentityResponse, error)
	GetBriefing(ctx context.Context) (*pb.BriefingResponse, error)
	StartSleep(ctx context.Context, phases []string) (string, error)
	SleepStatus(ctx context.Context, handle string) (*pb.SleepStatusResponse, error)
	WakeSleep(ctx context.Context, handle string) error
}

// Interface satisfaction is checked at every call site that holds
// an engine.Service-typed field (pipeline.Deps, adapter wiring,
// admin handlers). No compile-time assertion lives here now that
// memengine is in a sibling package.
