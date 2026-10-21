package server

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestMCPDelegationRateLimitPrecedesIntrospection(t *testing.T) {
	x := newAuthHarness(t, nil)
	cookie, owner := delegationLogin(t, x, "limited-owner")
	grant := delegationSeed(t, x.a, cookie, owner, "rate-test-secret", time.Minute*3, "project-a")
	clock := x.a.now()
	x.a.now = func() time.Time { return clock }
	x.a.mu.Lock()
	grant.windowStart, grant.requests = clock, 120
	x.a.mu.Unlock()
	x.iss.mu.Lock()
	x.iss.down = true
	x.iss.mu.Unlock()
	_, response, ok := delegationAuth(x.a, mcpDelegationPrefix+"rate-test-secret")
	if ok || response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatal("rate limit did not precede upstream request")
	}
	clock = clock.Add(time.Minute)
	_, response, ok = delegationAuth(x.a, mcpDelegationPrefix+"rate-test-secret")
	if ok || response.Code != http.StatusServiceUnavailable {
		t.Fatal("rate window did not reopen with fail-closed introspection")
	}
}

func TestMCPScopeRejectsSQLite(t *testing.T) {
	s := testServer(t)
	if _, _, err := s.DB.scopedProjects(context.Background(), "alice", []string{}); err == nil {
		t.Fatal("SQLite silently ignored a restricted scope")
	}
}

func TestCorporateMCPSchemaGuardRejectsUnsafeFunction(t *testing.T) {
	owner, root := corporateFixture(t)
	if err := root.DB.validateRuntimeSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.DB.Exec(`SELECT corporate_owner_effective_role('alice','x','y')`); err == nil {
		t.Fatal("runtime called unfiltered owner role helper")
	}
	if _, err := owner.DB.Exec(`GRANT EXECUTE ON FUNCTION corporate_owner_effective_role(text,text,text) TO aiwiki_app`); err != nil {
		t.Fatal(err)
	}
	if err := root.DB.validateRuntimeSchema(); err == nil {
		t.Fatal("unsafe function grant passed startup guard")
	}
	if _, err := owner.DB.Exec(`REVOKE EXECUTE ON FUNCTION corporate_owner_effective_role(text,text,text) FROM aiwiki_app`); err != nil {
		t.Fatal(err)
	}
}
