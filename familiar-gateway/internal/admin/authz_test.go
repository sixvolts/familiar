package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/identity"
	"github.com/familiar/gateway/internal/memory"
)

// ──────────────────────────────────────────────────────────────────
// Fakes
// ──────────────────────────────────────────────────────────────────

// fakeUserManager implements UserManager with an in-memory map. Only
// the methods authz_test actually calls are backed by real logic;
// the rest return zero values so the interface compiles.
type fakeUserManager struct {
	users      map[string]*identity.User
	adminCount int // override; -1 means derive from users map
}

func (f *fakeUserManager) ListUsers(ctx context.Context, statuses []identity.UserStatus) ([]identity.User, error) {
	out := make([]identity.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, *u)
	}
	return out, nil
}
func (f *fakeUserManager) GetUser(ctx context.Context, id string) (*identity.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}
func (f *fakeUserManager) ListIdentitiesForUser(ctx context.Context, userID string) ([]identity.IdentityLink, error) {
	return nil, nil
}
func (f *fakeUserManager) SetUserStatus(ctx context.Context, userID string, status identity.UserStatus, approver string) error {
	u, ok := f.users[userID]
	if !ok {
		return identity.ErrUserNotFound
	}
	u.Status = status
	return nil
}
func (f *fakeUserManager) LinkIdentity(ctx context.Context, userID, platform, platformID, displayName string) error {
	return nil
}
func (f *fakeUserManager) UnlinkIdentity(ctx context.Context, platform, platformID string) error {
	return nil
}
func (f *fakeUserManager) SetUserRole(ctx context.Context, userID, role string) error {
	u, ok := f.users[userID]
	if !ok {
		return identity.ErrUserNotFound
	}
	u.Role = role
	return nil
}
func (f *fakeUserManager) SetUserDisplayName(ctx context.Context, userID, displayName string) error {
	u, ok := f.users[userID]
	if !ok {
		return identity.ErrUserNotFound
	}
	u.DisplayName = displayName
	return nil
}
func (f *fakeUserManager) CountAdmins(ctx context.Context) (int, error) {
	if f.adminCount >= 0 {
		return f.adminCount, nil
	}
	n := 0
	for _, u := range f.users {
		if u.Role == "admin" {
			n++
		}
	}
	return n, nil
}
func (f *fakeUserManager) GetByEmail(ctx context.Context, email string) (*identity.User, error) {
	if email == "" {
		return nil, nil
	}
	for _, u := range f.users {
		if u.Email != nil && *u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeUserManager) CreateFirstRun(ctx context.Context, id, displayName, email string) error {
	if f.users == nil {
		f.users = map[string]*identity.User{}
	}
	if _, dup := f.users[id]; dup {
		return identity.ErrDuplicateLink
	}
	em := email
	f.users[id] = &identity.User{
		ID:          id,
		DisplayName: displayName,
		Email:       &em,
		Role:        "admin",
		Status:      identity.StatusApproved,
	}
	return nil
}

// SearchUsers: case-insensitive prefix match against id /
// display_name / email — same shape as the real Resolver but
// without DB ordering.
func (f *fakeUserManager) SearchUsers(ctx context.Context, query string, limit int) ([]identity.User, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	q := strings.ToLower(query)
	out := make([]identity.User, 0)
	for _, u := range f.users {
		if len(out) >= limit {
			break
		}
		em := ""
		if u.Email != nil {
			em = *u.Email
		}
		if strings.HasPrefix(strings.ToLower(u.ID), q) ||
			strings.HasPrefix(strings.ToLower(u.DisplayName), q) ||
			strings.HasPrefix(strings.ToLower(em), q) {
			out = append(out, *u)
		}
	}
	return out, nil
}

// CreateInvited / InviteUser stubs — added with the email-invite
// path (admin POST /console/api/users). The handler tests in this
// package don't exercise the happy path, but the UserManager
// interface requires the methods so the fake must implement them.
func (f *fakeUserManager) CreateInvited(ctx context.Context, id, displayName, email, role string) error {
	if _, ok := f.users[id]; ok {
		return errors.New("duplicate id")
	}
	em := email
	now := time.Now()
	f.users[id] = &identity.User{
		ID:          id,
		DisplayName: displayName,
		Email:       &em,
		Status:      identity.StatusApproved,
		Role:        role,
		CreatedAt:   now,
	}
	return nil
}
func (f *fakeUserManager) InviteUser(ctx context.Context, displayName, email, role string) (*identity.User, error) {
	id := strings.ToLower(strings.TrimSpace(displayName))
	if id == "" {
		id = strings.SplitN(email, "@", 2)[0]
	}
	if err := f.CreateInvited(ctx, id, displayName, email, role); err != nil {
		return nil, err
	}
	return f.users[id], nil
}

// fakeMemoryBrowser is the minimal MemoryBrowser stub memory-scoping
// tests need. Returns a row from a canned map; GetMemory obeys the
// "owner vs other-user" distinction that loadScopedMemory keys on.
type fakeMemoryBrowser struct {
	rows map[string]*memory.MemoryRow
}

func (f *fakeMemoryBrowser) ListMemories(ctx context.Context, filter memory.MemoryFilter, limit, offset int) ([]memory.MemoryRow, error) {
	out := []memory.MemoryRow{}
	for _, r := range f.rows {
		if filter.UserIDFilterMode == memory.UserIDFilterExact && r.UserID != filter.UserID {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}
func (f *fakeMemoryBrowser) CountMemories(ctx context.Context, filter memory.MemoryFilter) (int, error) {
	rows, _ := f.ListMemories(ctx, filter, 0, 0)
	return len(rows), nil
}
func (f *fakeMemoryBrowser) GetMemory(ctx context.Context, id string) (*memory.MemoryRow, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, memory.ErrMemoryNotFound
	}
	cp := *r
	return &cp, nil
}
func (f *fakeMemoryBrowser) DeleteMemory(ctx context.Context, id string) error {
	if _, ok := f.rows[id]; !ok {
		return memory.ErrMemoryNotFound
	}
	delete(f.rows, id)
	return nil
}
func (f *fakeMemoryBrowser) UpdateMemoryContent(ctx context.Context, id, newContent, changedBy string, embedding []float32) error {
	r, ok := f.rows[id]
	if !ok {
		return memory.ErrMemoryNotFound
	}
	r.Content = newContent
	return nil
}
func (f *fakeMemoryBrowser) ListVersions(ctx context.Context, memoryID string) ([]memory.MemoryVersion, error) {
	return nil, nil
}
func (f *fakeMemoryBrowser) ChainForMemory(ctx context.Context, id string) ([]memory.MemoryRow, error) {
	return nil, nil
}
func (f *fakeMemoryBrowser) CollapseChain(ctx context.Context, id string) (int64, string, error) {
	return 0, id, nil
}
func (f *fakeMemoryBrowser) MemoryHealth(ctx context.Context, userID string) (memory.HealthStats, error) {
	return memory.HealthStats{}, nil
}
func (f *fakeMemoryBrowser) DistinctScopes(ctx context.Context) ([]string, error) { return nil, nil }
func (f *fakeMemoryBrowser) DistinctSourceTypes(ctx context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeMemoryBrowser) DistinctUsers(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range f.rows {
		if r.UserID != "" && !seen[r.UserID] {
			seen[r.UserID] = true
			out = append(out, r.UserID)
		}
	}
	return out, nil
}

// Phase F dashboard-aggregate stubs. Existing non-dashboard tests
// don't need real aggregates, so these return zeros; the dashboard
// tests use dedicated fakes with canned data.
func (f *fakeMemoryBrowser) CountFactsForUser(ctx context.Context, userID string, includeShards bool) (int, error) {
	return 0, nil
}
func (f *fakeMemoryBrowser) RecentFactsForUser(ctx context.Context, userID string, limit int, includeShards bool) ([]memory.MemoryRow, error) {
	return nil, nil
}
func (f *fakeMemoryBrowser) GrowthSparkline(ctx context.Context, userID string, days int) ([]memory.GrowthPoint, error) {
	return nil, nil
}

// ctxWithAuth returns a context carrying an AuthUser, as if authRequired
// had already run. Used to bypass the session-cookie plumbing for
// middleware-layer tests.
func ctxWithAuth(base context.Context, u AuthUser) context.Context {
	return context.WithValue(base, ctxAuthUserKey, u)
}

// ──────────────────────────────────────────────────────────────────
// adminOnly
// ──────────────────────────────────────────────────────────────────

func TestAdminOnly_AllowsAdminRole(t *testing.T) {
	h := &Handler{}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest("GET", "/admin/api/users", nil).WithContext(
		ctxWithAuth(context.Background(), AuthUser{UserID: "owner", Role: "admin"}))
	rec := httptest.NewRecorder()
	h.adminOnly(next).ServeHTTP(rec, req)
	if !called {
		t.Fatalf("admin request must reach downstream handler")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAdminOnly_RejectsUserRole(t *testing.T) {
	h := &Handler{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("downstream handler must not run for user-role")
	})
	req := httptest.NewRequest("GET", "/admin/api/users", nil).WithContext(
		ctxWithAuth(context.Background(), AuthUser{UserID: "alison", Role: "user"}))
	rec := httptest.NewRecorder()
	h.adminOnly(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin role required") {
		t.Errorf("body should mention admin role, got %s", rec.Body.String())
	}
}

func TestAdminOnly_RejectsMissingAuthUser(t *testing.T) {
	// Defence in depth: adminOnly on a route that forgot the
	// authRequired wrap still blocks instead of silently opening.
	h := &Handler{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("should not reach downstream without AuthUser in ctx")
	})
	req := httptest.NewRequest("GET", "/admin/api/users", nil)
	rec := httptest.NewRecorder()
	h.adminOnly(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// ──────────────────────────────────────────────────────────────────
// Memory ownership scoping
// ──────────────────────────────────────────────────────────────────

// The list endpoint is deny-by-default: everyone lands on their own
// rows. Only an admin's EXPLICIT ?user= widens the scope — the
// personal Memory page sends no user param and must never mix other
// users' rows in, whatever the caller's role.
func TestListMemories_ScopeEnforcement(t *testing.T) {
	mb := &fakeMemoryBrowser{rows: map[string]*memory.MemoryRow{
		"m1": {ID: "m1", UserID: "operator", Content: "admin fact"},
		"m2": {ID: "m2", UserID: "alison", Content: "user fact"},
	}}
	h := &Handler{memoryBrowser: mb}

	list := func(au AuthUser, query string) []memoryRowDTO {
		t.Helper()
		req := httptest.NewRequest("GET", "/console/api/memories"+query, nil).
			WithContext(ctxWithAuth(context.Background(), au))
		rec := httptest.NewRecorder()
		h.listMemories(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		var out struct {
			Items []memoryRowDTO `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Items
	}
	onlyUser := func(items []memoryRowDTO, want string) bool {
		for _, it := range items {
			if it.UserID != want {
				return false
			}
		}
		return len(items) > 0
	}

	// Admin, no param → own rows only (the personal page).
	if items := list(operatorAdmin(), ""); !onlyUser(items, "operator") {
		t.Errorf("admin default leaked non-self rows: %+v", items)
	}
	// Admin, explicit all → everything (the admin cross-user view).
	if items := list(operatorAdmin(), "?user=all"); len(items) != 2 {
		t.Errorf("admin user=all rows = %d, want 2", len(items))
	}
	// Admin, explicit other user → that user.
	if items := list(operatorAdmin(), "?user=alison"); !onlyUser(items, "alison") {
		t.Errorf("admin user=alison wrong rows: %+v", items)
	}
	// Role=user with user=all → still self only.
	if items := list(alisonUser(), "?user=all"); !onlyUser(items, "alison") {
		t.Errorf("user=all leaked to role=user: %+v", items)
	}
	// Role=user naming another user → still self only.
	if items := list(alisonUser(), "?user=operator"); !onlyUser(items, "alison") {
		t.Errorf("user=operator leaked to role=user: %+v", items)
	}

	// No auth user in context → 401, not the unfiltered store.
	req := httptest.NewRequest("GET", "/console/api/memories", nil)
	rec := httptest.NewRecorder()
	h.listMemories(rec, req)
	if rec.Code != 401 {
		t.Errorf("unauthenticated list status = %d, want 401", rec.Code)
	}
}

// The user roster in the facets payload is admin-only — a role=user
// session must not receive the instance's user-ID list.
func TestMemoryFacets_UserRosterAdminOnly(t *testing.T) {
	mb := &fakeMemoryBrowser{rows: map[string]*memory.MemoryRow{
		"m1": {ID: "m1", UserID: "operator"},
		"m2": {ID: "m2", UserID: "alison"},
	}}
	h := &Handler{memoryBrowser: mb}

	facets := func(au AuthUser) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/console/api/memories/facets", nil).
			WithContext(ctxWithAuth(context.Background(), au))
		rec := httptest.NewRecorder()
		h.memoryFacets(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if users := facets(alisonUser())["users"].([]any); len(users) != 0 {
		t.Errorf("role=user received the user roster: %v", users)
	}
	if users := facets(operatorAdmin())["users"].([]any); len(users) != 2 {
		t.Errorf("admin roster = %v, want both users", users)
	}
}

func TestLoadScopedMemory_AdminSeesAnyOwner(t *testing.T) {
	h := &Handler{memoryBrowser: &fakeMemoryBrowser{rows: map[string]*memory.MemoryRow{
		"m1": {ID: "m1", Content: "owner fact", UserID: "owner"},
	}}}
	req := httptest.NewRequest("GET", "/admin/api/memories/m1", nil).WithContext(
		ctxWithAuth(context.Background(), AuthUser{UserID: "alison", Role: "admin"}))
	rec := httptest.NewRecorder()
	row, ok := h.loadScopedMemory(rec, req, "m1")
	if !ok {
		t.Fatalf("admin should be able to load someone else's row, body=%s", rec.Body.String())
	}
	if row.Content != "owner fact" {
		t.Errorf("got %q, want 'owner fact'", row.Content)
	}
}

func TestLoadScopedMemory_UserSeesOwn(t *testing.T) {
	h := &Handler{memoryBrowser: &fakeMemoryBrowser{rows: map[string]*memory.MemoryRow{
		"m1": {ID: "m1", Content: "mine", UserID: "alison"},
	}}}
	req := httptest.NewRequest("GET", "/admin/api/memories/m1", nil).WithContext(
		ctxWithAuth(context.Background(), AuthUser{UserID: "alison", Role: "user"}))
	rec := httptest.NewRecorder()
	row, ok := h.loadScopedMemory(rec, req, "m1")
	if !ok {
		t.Fatalf("user should see their own row, body=%s", rec.Body.String())
	}
	if row.Content != "mine" {
		t.Errorf("got %q, want 'mine'", row.Content)
	}
}

func TestLoadScopedMemory_UserGets404ForOthersUUID(t *testing.T) {
	// Spec requirement: non-owner reads return 404, never 403 —
	// leaking existence lets an attacker enumerate UUIDs.
	h := &Handler{memoryBrowser: &fakeMemoryBrowser{rows: map[string]*memory.MemoryRow{
		"m1": {ID: "m1", Content: "owner secret", UserID: "owner"},
	}}}
	req := httptest.NewRequest("GET", "/admin/api/memories/m1", nil).WithContext(
		ctxWithAuth(context.Background(), AuthUser{UserID: "alison", Role: "user"}))
	rec := httptest.NewRecorder()
	row, ok := h.loadScopedMemory(rec, req, "m1")
	if ok || row != nil {
		t.Fatalf("user must NOT see owner's row")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (not 403 — leaks existence)", rec.Code)
	}
}

// ──────────────────────────────────────────────────────────────────
// ensureNotLastAdmin
// ──────────────────────────────────────────────────────────────────

func TestEnsureNotLastAdmin_AllowsWhenOthersExist(t *testing.T) {
	h := &Handler{users: &fakeUserManager{
		users: map[string]*identity.User{
			"owner":  {ID: "owner", Role: "admin", Status: identity.StatusApproved},
			"backup": {ID: "backup", Role: "admin", Status: identity.StatusApproved},
			"alison": {ID: "alison", Role: "user", Status: identity.StatusApproved},
		},
		adminCount: -1,
	}}
	if err := h.ensureNotLastAdmin(context.Background(), "owner"); err != nil {
		t.Errorf("demote allowed when other admins exist, got: %v", err)
	}
}

func TestEnsureNotLastAdmin_BlocksLastAdmin(t *testing.T) {
	h := &Handler{users: &fakeUserManager{
		users: map[string]*identity.User{
			"owner":  {ID: "owner", Role: "admin", Status: identity.StatusApproved},
			"alison": {ID: "alison", Role: "user", Status: identity.StatusApproved},
		},
		adminCount: -1,
	}}
	err := h.ensureNotLastAdmin(context.Background(), "owner")
	if err == nil {
		t.Fatalf("demoting the last admin must error")
	}
	if !strings.Contains(err.Error(), "last admin") {
		t.Errorf("error should mention 'last admin', got: %v", err)
	}
}

func TestEnsureNotLastAdmin_IgnoresNonAdminTarget(t *testing.T) {
	// Demoting a user-role user isn't an admin count change. The
	// guard must not fire just because the caller invoked it.
	h := &Handler{users: &fakeUserManager{
		users: map[string]*identity.User{
			"owner":  {ID: "owner", Role: "admin", Status: identity.StatusApproved},
			"alison": {ID: "alison", Role: "user", Status: identity.StatusApproved},
		},
		adminCount: -1,
	}}
	if err := h.ensureNotLastAdmin(context.Background(), "alison"); err != nil {
		t.Errorf("non-admin target shouldn't trip the guard: %v", err)
	}
}

func TestEnsureNotLastAdmin_CountError(t *testing.T) {
	// If CountAdmins errors, the guard should surface it rather
	// than silently allow a dangerous demotion.
	fm := &fakeUserManager{
		users:      map[string]*identity.User{"owner": {ID: "owner", Role: "admin"}},
		adminCount: -1,
	}
	h := &Handler{users: &countErrorUserManager{fakeUserManager: *fm}}
	err := h.ensureNotLastAdmin(context.Background(), "owner")
	if err == nil {
		t.Fatalf("CountAdmins error should surface from the guard")
	}
}

// countErrorUserManager forces CountAdmins to fail so the guard's
// error-propagation path is exercised.
type countErrorUserManager struct{ fakeUserManager }

func (c *countErrorUserManager) CountAdmins(ctx context.Context) (int, error) {
	return 0, errors.New("db unavailable")
}
