package admin

// Dashboard endpoint tests (FAMILIAR-DASHBOARD-SPEC Phase F).
//
// Each endpoint is exercised for:
//   • Role-scoping — non-admin sees own data regardless of ?user_id=
//     spoof, admin sees own data when no override, admin honors
//     ?user_id=<id>.
//   • Empty-data rendering — a fresh user with zero data returns
//     valid JSON (zeros / [] / null), never a 500.
//
// The role-scope + override behavior is tested by asserting on the
// userID the handler passed to the store, not on the output alone —
// that way a fake that accidentally returned the right data for the
// wrong reason still fails.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/memory"
	"github.com/familiar/gateway/internal/session"
	"github.com/familiar/gateway/internal/shards"
)

// ──────────────────────────────────────────────────────────────────
// Dashboard-specific fakes
// ──────────────────────────────────────────────────────────────────

// fakeDashboardMem is a MemoryBrowser fake that records which userID
// the dashboard methods were called with so tests can assert
// scoping. All non-dashboard methods panic so accidental use is
// loud.
type fakeDashboardMem struct {
	// Canned responses keyed by the userID the caller asks for.
	factCounts map[string]int
	recent     map[string][]memory.MemoryRow
	sparkline  map[string][]memory.GrowthPoint

	// Last call args — for assertions.
	lastFactUser     string
	lastFactShards   bool
	lastRecentUser   string
	lastRecentLimit  int
	lastRecentShards bool
	lastSparkUser    string
	lastSparkDays    int
}

func (f *fakeDashboardMem) CountFactsForUser(_ context.Context, userID string, includeShards bool) (int, error) {
	f.lastFactUser = userID
	f.lastFactShards = includeShards
	return f.factCounts[userID], nil
}
func (f *fakeDashboardMem) RecentFactsForUser(_ context.Context, userID string, limit int, includeShards bool) ([]memory.MemoryRow, error) {
	f.lastRecentUser = userID
	f.lastRecentLimit = limit
	f.lastRecentShards = includeShards
	rows := f.recent[userID]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
func (f *fakeDashboardMem) GrowthSparkline(_ context.Context, userID string, days int) ([]memory.GrowthPoint, error) {
	f.lastSparkUser = userID
	f.lastSparkDays = days
	return f.sparkline[userID], nil
}

// Unused on dashboard-path calls; panic on accidental use.
func (f *fakeDashboardMem) ListMemories(context.Context, memory.MemoryFilter, int, int) ([]memory.MemoryRow, error) {
	panic("ListMemories not used")
}
func (f *fakeDashboardMem) CountMemories(context.Context, memory.MemoryFilter) (int, error) {
	panic("CountMemories not used")
}
func (f *fakeDashboardMem) GetMemory(context.Context, string) (*memory.MemoryRow, error) {
	panic("GetMemory not used")
}
func (f *fakeDashboardMem) DeleteMemory(context.Context, string) error {
	panic("DeleteMemory not used")
}
func (f *fakeDashboardMem) UpdateMemoryContent(context.Context, string, string, string, []float32) error {
	panic("UpdateMemoryContent not used")
}
func (f *fakeDashboardMem) ChainForMemory(context.Context, string) ([]memory.MemoryRow, error) {
	return nil, nil
}
func (f *fakeDashboardMem) CollapseChain(context.Context, string) (int64, string, error) {
	return 0, "", nil
}
func (f *fakeDashboardMem) MemoryHealth(context.Context, string) (memory.HealthStats, error) {
	return memory.HealthStats{}, nil
}
func (f *fakeDashboardMem) ListVersions(context.Context, string) ([]memory.MemoryVersion, error) {
	panic("ListVersions not used")
}
func (f *fakeDashboardMem) DistinctScopes(context.Context) ([]string, error) {
	panic("DistinctScopes not used")
}
func (f *fakeDashboardMem) DistinctSourceTypes(context.Context) ([]string, error) {
	panic("DistinctSourceTypes not used")
}
func (f *fakeDashboardMem) DistinctUsers(context.Context) ([]string, error) {
	panic("DistinctUsers not used")
}

// fakeDashboardGraph mirrors the same pattern for GraphStore.
type fakeDashboardGraph struct {
	entityCount map[string]int
	relCount    map[string]int
	breakdown   map[string][]memory.EntityTypeCount
	topEdges    map[string][]memory.GraphEdge

	lastEntitiesUser  string
	lastRelsUser      string
	lastBreakdownUser string
	lastTopUser       string
	lastTopLimit      int
}

func (f *fakeDashboardGraph) CountEntities(_ context.Context, userID string) (int, error) {
	f.lastEntitiesUser = userID
	return f.entityCount[userID], nil
}
func (f *fakeDashboardGraph) CountRelationships(_ context.Context, userID string) (int, error) {
	f.lastRelsUser = userID
	return f.relCount[userID], nil
}
func (f *fakeDashboardGraph) EntityBreakdown(_ context.Context, userID string) ([]memory.EntityTypeCount, error) {
	f.lastBreakdownUser = userID
	return f.breakdown[userID], nil
}
func (f *fakeDashboardGraph) GraphTop(_ context.Context, userID string, limit int) ([]memory.GraphEdge, error) {
	f.lastTopUser = userID
	f.lastTopLimit = limit
	return f.topEdges[userID], nil
}

// Unused on dashboard-path.
func (f *fakeDashboardGraph) GraphAround(context.Context, string, string, int, int) ([]memory.GraphEdge, error) {
	panic("GraphAround not used")
}
func (f *fakeDashboardGraph) SearchEntities(context.Context, string, string, int) ([]memory.EntityMatch, error) {
	panic("SearchEntities not used")
}
func (f *fakeDashboardGraph) ListBySourceFact(context.Context, string) ([]memory.GraphEdge, error) {
	return nil, nil
}
func (f *fakeDashboardGraph) FactsForEntity(context.Context, string, string, int) ([]memory.MemoryRow, error) {
	panic("FactsForEntity not used")
}
func (f *fakeDashboardGraph) MergeEntities(context.Context, string, string, string) (int64, error) {
	panic("MergeEntities not used")
}
func (f *fakeDashboardGraph) UpdateRelationship(context.Context, string, string, string, *float64) (*memory.GraphEdge, error) {
	panic("UpdateRelationship not used")
}
func (f *fakeDashboardGraph) OrphanEdges(context.Context, string) (int, error) {
	return 0, nil
}

func (f *fakeDashboardGraph) GetRelationship(context.Context, string, string) (*memory.GraphEdge, error) {
	panic("GetRelationship not used")
}
func (f *fakeDashboardGraph) DeleteRelationship(context.Context, string, string) error {
	panic("DeleteRelationship not used")
}
func (f *fakeDashboardGraph) DeleteEntity(context.Context, string, string) (int64, error) {
	panic("DeleteEntity not used")
}

// ──────────────────────────────────────────────────────────────────
// Fixtures
// ──────────────────────────────────────────────────────────────────

func makeDashHandler() (*Handler, *fakeDashboardMem, *fakeDashboardGraph, *fakeSessionLister, *fakeShardStore, *fakeUserManager) {
	h := &Handler{}
	mem := &fakeDashboardMem{
		factCounts: map[string]int{"operator": 933, "alison": 0},
		recent: map[string][]memory.MemoryRow{
			"operator": {
				{ID: "m1", Content: "Alison's birthday is May 12", SourceType: "save_fact", CreatedAt: time.Now()},
				{ID: "m2", Content: "Notes on qwen3.5 inference", SourceType: "save_fact", CreatedAt: time.Now().Add(-time.Hour)},
			},
		},
		sparkline: map[string][]memory.GrowthPoint{
			"operator": {
				{Date: "2026-04-22", FactCount: 920, EntityCount: 275},
				{Date: "2026-04-23", FactCount: 933, EntityCount: 287},
			},
		},
	}
	gr := &fakeDashboardGraph{
		entityCount: map[string]int{"operator": 287, "alison": 0},
		relCount:    map[string]int{"operator": 1204, "alison": 0},
		breakdown: map[string][]memory.EntityTypeCount{
			"operator": {{Type: "entity", Count: 287}},
		},
		topEdges: map[string][]memory.GraphEdge{
			"operator": {
				{ID: "r1", Subject: "operator", Predicate: "owns", Object: "rivian"},
				{ID: "r2", Subject: "operator", Predicate: "lives_in", Object: "menlo park"},
			},
		},
	}
	s1 := &session.Session{ID: "s1", ChannelID: "openai:operator", LastActive: time.Now()}
	s1.SetIdentity("openai", "operator")
	s2 := &session.Session{ID: "s2", ChannelID: "slack:operator", LastActive: time.Now().Add(-10 * time.Minute)}
	s2.SetIdentity("slack", "operator")
	sl := &fakeSessionLister{sessions: []*session.Session{s1, s2}}
	ss := newFakeShardStore()
	ss.addShard(&shards.Shard{ID: "extractor", OwnerID: "operator", Name: "Extractor"})
	ss.addShard(&shards.Shard{ID: "classifier", OwnerID: "operator", Name: "Classifier"})
	um := &fakeUserManager{users: map[string]*identity.User{
		"operator": {ID: "operator", DisplayName: "Operator", Role: "admin"},
		"alison":   {ID: "alison", DisplayName: "Alison", Role: "user"},
	}}

	h.AttachMemoryBrowser(mem)
	h.AttachGraphStore(gr)
	h.AttachChatSessionLister(sl)
	h.AttachShardStore(ss)
	h.AttachUserManager(um)
	return h, mem, gr, sl, ss, um
}

// ──────────────────────────────────────────────────────────────────
// /overview
// ──────────────────────────────────────────────────────────────────

func TestDashboardOverview_NonAdminScopedToSelf(t *testing.T) {
	h, mem, gr, _, _, _ := makeDashHandler()

	// Alison spoofs ?user_id=operator — should be silently ignored.
	req := httptest.NewRequest("GET", "/console/api/dashboard/overview?user_id=operator", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardOverview(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp dashboardOverviewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mem.lastFactUser != "alison" {
		t.Errorf("spoofed ?user_id=operator leaked to mem; lastFactUser = %q, want %q", mem.lastFactUser, "alison")
	}
	if gr.lastEntitiesUser != "alison" {
		t.Errorf("spoofed ?user_id leaked to graph; lastEntitiesUser = %q, want %q", gr.lastEntitiesUser, "alison")
	}
	if resp.UserID != "alison" || resp.FactCount != 0 {
		t.Errorf("response shows operator's data for alison: %+v", resp)
	}
}

func TestDashboardOverview_AdminDefaultsToSelf(t *testing.T) {
	h, mem, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/overview", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardOverview(rec, req)

	if mem.lastFactUser != "operator" {
		t.Errorf("admin default target = %q, want operator", mem.lastFactUser)
	}
	var resp dashboardOverviewDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.FactCount != 933 {
		t.Errorf("admin default fact_count = %d, want 933", resp.FactCount)
	}
	if resp.DisplayName != "Operator" {
		t.Errorf("display_name = %q, want Operator", resp.DisplayName)
	}
	if resp.SessionCountLive != 2 {
		t.Errorf("session_count_live = %d, want 2", resp.SessionCountLive)
	}
	if resp.ShardCount != 2 {
		t.Errorf("shard_count = %d, want 2", resp.ShardCount)
	}
}

func TestDashboardOverview_AdminOverrideHonored(t *testing.T) {
	h, mem, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/overview?user_id=alison", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardOverview(rec, req)

	if mem.lastFactUser != "alison" {
		t.Errorf("admin ?user_id=alison not honored; lastFactUser = %q", mem.lastFactUser)
	}
	var resp dashboardOverviewDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.UserID != "alison" {
		t.Errorf("response user_id = %q, want alison", resp.UserID)
	}
	if resp.DisplayName != "Alison" {
		t.Errorf("display_name = %q, want Alison", resp.DisplayName)
	}
}

func TestDashboardOverview_EmptyUserReturnsZeros(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/overview", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardOverview(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp dashboardOverviewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FactCount != 0 || resp.EntityCount != 0 || resp.RelationshipCount != 0 {
		t.Errorf("empty user got non-zero counts: %+v", resp)
	}
	if resp.LastChatAt != nil {
		t.Errorf("empty user got last_chat_at = %v, want null", resp.LastChatAt)
	}
}

// ──────────────────────────────────────────────────────────────────
// /recent_sessions
// ──────────────────────────────────────────────────────────────────

func TestDashboardRecentSessions_SortedAndLimited(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/recent_sessions?limit=1", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardRecentSessions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []dashboardRecentSessionDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("len = %d, want 1 (limit honored)", len(resp.Items))
	}
	// Most recent session is s1 (LastActive = now).
	if resp.Items[0].ID != "s1" {
		t.Errorf("first item = %q, want s1 (most recent)", resp.Items[0].ID)
	}
}

func TestDashboardRecentSessions_NonAdminSpoofIgnored(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/recent_sessions?user_id=operator", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardRecentSessions(rec, req)

	var resp struct {
		Items []dashboardRecentSessionDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// Alison has no sessions in the fake lister — spoof must not
	// surface operator's sessions to her.
	if len(resp.Items) != 0 {
		t.Errorf("spoof surfaced %d items, want 0", len(resp.Items))
	}
}

// ──────────────────────────────────────────────────────────────────
// /recent_writes
// ──────────────────────────────────────────────────────────────────

func TestDashboardRecentWrites_AdminOverrideHonored(t *testing.T) {
	h, mem, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/recent_writes?user_id=operator&limit=5", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardRecentWrites(rec, req)

	if mem.lastRecentUser != "operator" {
		t.Errorf("lastRecentUser = %q, want operator", mem.lastRecentUser)
	}
	if mem.lastRecentShards {
		t.Errorf("includeShards default = true, want false")
	}
}

func TestDashboardRecentWrites_IncludeShardsToggle(t *testing.T) {
	h, mem, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/recent_writes?include_shards=true", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardRecentWrites(rec, req)

	if !mem.lastRecentShards {
		t.Errorf("include_shards=true not honored")
	}
}

func TestDashboardRecentWrites_SnippetTruncated(t *testing.T) {
	h := &Handler{}
	long := make([]rune, recentWriteSnippetMax+50)
	for i := range long {
		long[i] = 'x'
	}
	mem := &fakeDashboardMem{
		recent: map[string][]memory.MemoryRow{
			"operator": {{ID: "m1", Content: string(long), CreatedAt: time.Now()}},
		},
	}
	h.AttachMemoryBrowser(mem)

	req := httptest.NewRequest("GET", "/console/api/dashboard/recent_writes", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardRecentWrites(rec, req)

	var resp struct {
		Items []dashboardRecentWriteDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("len = %d, want 1", len(resp.Items))
	}
	// Expect maxRunes x's + one ellipsis char.
	if got := len([]rune(resp.Items[0].Snippet)); got != recentWriteSnippetMax+1 {
		t.Errorf("snippet rune len = %d, want %d (max + ellipsis)", got, recentWriteSnippetMax+1)
	}
}

// ──────────────────────────────────────────────────────────────────
// /entity_breakdown
// ──────────────────────────────────────────────────────────────────

func TestDashboardEntityBreakdown_NonAdminScopedToSelf(t *testing.T) {
	h, _, gr, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/entity_breakdown?user_id=operator", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardEntityBreakdown(rec, req)

	if gr.lastBreakdownUser != "alison" {
		t.Errorf("spoof leaked; lastBreakdownUser = %q, want alison", gr.lastBreakdownUser)
	}
}

func TestDashboardEntityBreakdown_EmptyReturnsEmptyArray(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()
	req := httptest.NewRequest("GET", "/console/api/dashboard/entity_breakdown", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardEntityBreakdown(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	// Ensure the response is "breakdown":[] not null — JS iteration
	// must work without a null-check.
	if got := rec.Body.String(); !containsExact(got, `"breakdown":[]`) {
		t.Errorf("empty user body = %s; want breakdown:[]", got)
	}
}

// ──────────────────────────────────────────────────────────────────
// /shard_summary
// ──────────────────────────────────────────────────────────────────

func TestDashboardShardSummary_ScopedToUser(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()

	// Admin default: sees own shards.
	req := httptest.NewRequest("GET", "/console/api/dashboard/shard_summary", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardShardSummary(rec, req)

	var resp struct {
		Shards []dashboardShardDTO `json:"shards"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Shards) != 2 {
		t.Errorf("operator shards len = %d, want 2", len(resp.Shards))
	}
}

func TestDashboardShardSummary_NonAdminSpoofIgnored(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()
	req := httptest.NewRequest("GET", "/console/api/dashboard/shard_summary?user_id=operator", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardShardSummary(rec, req)

	var resp struct {
		Shards []dashboardShardDTO `json:"shards"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Shards) != 0 {
		t.Errorf("alison saw %d shards (spoofed to operator)", len(resp.Shards))
	}
}

// ──────────────────────────────────────────────────────────────────
// /graph_preview
// ──────────────────────────────────────────────────────────────────

func TestDashboardGraphPreview_NodesAndEdges(t *testing.T) {
	h, _, gr, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/graph_preview?limit=15", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardGraphPreview(rec, req)

	if gr.lastTopUser != "operator" || gr.lastTopLimit != 15 {
		t.Errorf("GraphTop call = (%q, %d), want (operator, 15)", gr.lastTopUser, gr.lastTopLimit)
	}
	var resp dashboardGraphPreviewDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// 2 edges, 3 unique nodes (operator, rivian, menlo park).
	if len(resp.Edges) != 2 {
		t.Errorf("edges = %d, want 2", len(resp.Edges))
	}
	if len(resp.Nodes) != 3 {
		t.Errorf("nodes = %d, want 3", len(resp.Nodes))
	}
	// Highest-degree node should be operator (touches both edges).
	if resp.Nodes[0].ID != "operator" || resp.Nodes[0].Degree != 2 {
		t.Errorf("top node = %+v, want {operator, degree=2}", resp.Nodes[0])
	}
}

func TestDashboardGraphPreview_EmptyUser(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()
	req := httptest.NewRequest("GET", "/console/api/dashboard/graph_preview", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardGraphPreview(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp dashboardGraphPreviewDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// Must return empty arrays, not null.
	if resp.Nodes == nil || resp.Edges == nil {
		t.Errorf("empty user got null arrays: %+v", resp)
	}
}

// ──────────────────────────────────────────────────────────────────
// /growth_sparkline
// ──────────────────────────────────────────────────────────────────

func TestDashboardGrowthSparkline_DaysDefault(t *testing.T) {
	h, mem, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/growth_sparkline", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardGrowthSparkline(rec, req)

	if mem.lastSparkDays != 30 {
		t.Errorf("default days = %d, want 30", mem.lastSparkDays)
	}
}

func TestDashboardGrowthSparkline_AdminOverrideHonored(t *testing.T) {
	h, mem, _, _, _, _ := makeDashHandler()

	req := httptest.NewRequest("GET", "/console/api/dashboard/growth_sparkline?user_id=alison&days=7", nil).
		WithContext(ctxWithAuth(context.Background(), operatorAdmin()))
	rec := httptest.NewRecorder()
	h.dashboardGrowthSparkline(rec, req)

	if mem.lastSparkUser != "alison" || mem.lastSparkDays != 7 {
		t.Errorf("admin override = (%q, %d), want (alison, 7)", mem.lastSparkUser, mem.lastSparkDays)
	}
}

func TestDashboardGrowthSparkline_EmptyReturnsEmptyArray(t *testing.T) {
	h, _, _, _, _, _ := makeDashHandler()
	req := httptest.NewRequest("GET", "/console/api/dashboard/growth_sparkline", nil).
		WithContext(ctxWithAuth(context.Background(), alisonUser()))
	rec := httptest.NewRecorder()
	h.dashboardGrowthSparkline(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); !containsExact(got, `"series":[]`) {
		t.Errorf("empty user body = %s; want series:[]", got)
	}
}

// ──────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────

// containsExact is a substring check — small wrapper to avoid pulling
// strings.Contains import noise across every assertion site.
func containsExact(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
