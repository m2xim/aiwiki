package server

// Temporary read-only MCP delegations.
//
// A delegation is a short-lived bearer secret minted from an authenticated
// browser session. It names an exact set of projects and can only read them.
// Grants live in process memory only, keyed by SHA-256 of the secret, and are
// bound to the issuing browser session: logout, session expiry or a restart
// invalidates them immediately. The parent session's upstream access token is
// never copied; it is read from the session on every request and introspected
// again, so identity provider outages, inactive tokens, lost admission roles
// and disabled users all fail closed.

import (
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	mcpDelegationDefaultTTL = 300
	mcpDelegationMinTTL     = 1
	mcpDelegationMaxTTL     = 600
	mcpDelegationMaxName    = 80
	mcpDelegationMaxProject = 20
	mcpDelegationUserCap    = 20
	mcpDelegationGlobalCap  = 2048
	mcpDelegationMinRole    = 1 // reader
)

type mcpDelegationRequest struct {
	Name       string   `json:"name"`
	ProjectIDs []string `json:"project_ids"`
	TTLSeconds int      `json:"ttl_seconds"`
}

// mcpDelegationIssued is the only shape that ever carries the secret, and it
// is returned exactly once from the mint call.
type mcpDelegationIssued struct {
	mcpDelegation
	Token string `json:"token"`
}

func mcpDelegationNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func (d *mcpDelegation) public() mcpDelegation {
	return mcpDelegation{
		ID:       d.ID,
		Name:     d.Name,
		Projects: append([]string{}, d.Projects...),
		Created:  d.Created,
		Expires:  d.Expires,
	}
}

// delegationLiveLocked reports whether g is still the registered grant for
// key, unexpired, and backed by its parent session. Dead grants are deleted.
// a.mu must be held.
func (a *AuthService) delegationLiveLocked(key [32]byte, g *mcpDelegation, now time.Time) bool {
	if g == nil || a.delegations[key] != g {
		return false
	}
	sess := a.sessions[g.SessionKey]
	if !now.Before(g.Expires) || sess == nil || !now.Before(sess.expires) || sess.userID != g.OwnerID {
		delete(a.delegations, key)
		return false
	}
	return true
}

func (a *AuthService) purgeDelegationsLocked(now time.Time) {
	for key, g := range a.delegations {
		a.delegationLiveLocked(key, g, now)
	}
}

// reserveDelegation registers g under the caps. It returns an HTTP status, 0
// on success. The grant is not usable by anyone until the secret is returned.
func (a *AuthService) reserveDelegation(key [32]byte, g *mcpDelegation) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.delegations == nil {
		a.delegations = map[[32]byte]*mcpDelegation{}
	}
	now := a.now()
	a.purgeDelegationsLocked(now)
	sess := a.sessions[g.SessionKey]
	if sess == nil || !now.Before(sess.expires) || sess.userID != g.OwnerID {
		return http.StatusUnauthorized
	}
	if len(a.delegations) >= mcpDelegationGlobalCap {
		return http.StatusTooManyRequests
	}
	own := 0
	for _, other := range a.delegations {
		if other.OwnerID == g.OwnerID {
			own++
		}
	}
	if own >= mcpDelegationUserCap {
		return http.StatusTooManyRequests
	}
	a.delegations[key] = g
	return 0
}

func (a *AuthService) removeDelegation(key [32]byte) {
	a.mu.Lock()
	delete(a.delegations, key)
	a.mu.Unlock()
}

func (s *Server) mcpDelegations(w http.ResponseWriter, r *http.Request) {
	mcpDelegationNoStore(w)
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	// A delegation can never mint further delegations.
	if s.auth == nil || s.principal.MCPProjects != nil {
		apiErr(w, http.StatusForbidden, "forbidden")
		return
	}
	c, err := r.Cookie(s.auth.sessionCookie)
	if err != nil || c.Value == "" {
		apiErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sessionKey := authHash(c.Value)
	sess := s.auth.sessionSnapshot(sessionKey)
	if sess == nil || sess.userID != actor {
		apiErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if r.Method == http.MethodGet {
		s.mcpDelegationList(w, actor, sessionKey)
		return
	}
	s.mcpDelegationMint(w, r, actor, sessionKey, sess)
}

func (s *Server) mcpDelegationList(w http.ResponseWriter, actor string, sessionKey [32]byte) {
	a := s.auth
	a.mu.Lock()
	a.purgeDelegationsLocked(a.now())
	out := make([]mcpDelegation, 0)
	for _, g := range a.delegations {
		if g.OwnerID == actor && g.SessionKey == sessionKey {
			out = append(out, g.public())
		}
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) mcpDelegationMint(w http.ResponseWriter, r *http.Request, actor string, sessionKey [32]byte, sess *authSession) {
	var in mcpDelegationRequest
	if err := corporateDecode(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	name, ok := corporateName(in.Name, mcpDelegationMaxName)
	if !ok {
		apiErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	if len(in.ProjectIDs) < 1 || len(in.ProjectIDs) > mcpDelegationMaxProject {
		apiErr(w, http.StatusBadRequest, "invalid project_ids")
		return
	}
	seen := make(map[string]bool, len(in.ProjectIDs))
	projects := make([]string, 0, len(in.ProjectIDs))
	for _, id := range in.ProjectIDs {
		if !validPathID(id) || seen[id] {
			apiErr(w, http.StatusBadRequest, "invalid project_ids")
			return
		}
		seen[id] = true
		projects = append(projects, id)
	}
	sort.Strings(projects)
	ttl := in.TTLSeconds
	if ttl == 0 {
		ttl = mcpDelegationDefaultTTL
	}
	if ttl < mcpDelegationMinTTL || ttl > mcpDelegationMaxTTL {
		apiErr(w, http.StatusBadRequest, "invalid ttl_seconds")
		return
	}

	// Exact projects only, each readable by the owner right now and still
	// writable (not archived) at the container level. Unknown, hidden and
	// archived projects are indistinguishable to the caller.
	for _, id := range projects {
		var space string
		var live bool
		err := s.DB.QueryRow(`SELECT space_id,corporate_container_writable(space_id,id) FROM projects WHERE id=?`, id).Scan(&space, &live)
		if err != nil || !live {
			apiErr(w, http.StatusForbidden, "forbidden")
			return
		}
		role, err := s.effectiveRole(space, id)
		if err != nil || role < mcpDelegationMinRole {
			apiErr(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	now := s.auth.now()
	expires := now.Add(time.Duration(ttl) * time.Second)
	if sess.expires.Before(expires) {
		expires = sess.expires
	}
	if !expires.After(now) {
		apiErr(w, http.StatusUnauthorized, "session expired")
		return
	}
	secret, err := s.auth.newDelegationSecret()
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "delegation unavailable")
		return
	}
	id, err := authRandom(12)
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "delegation unavailable")
		return
	}
	grant := &mcpDelegation{
		ID:         "dlg_" + id,
		Name:       name,
		OwnerID:    actor,
		Projects:   projects,
		Created:    now.UTC(),
		Expires:    expires.UTC(),
		SessionKey: sessionKey,
	}
	key := authHash(secret)

	// Reserve the slot first so concurrent mints obey the caps, then audit.
	// The secret is unknown to anyone until the response below, so a failed
	// audit simply withdraws the reservation.
	if status := s.auth.reserveDelegation(key, grant); status != 0 {
		switch status {
		case http.StatusTooManyRequests:
			apiErr(w, status, "too many delegations")
		default:
			apiErr(w, http.StatusUnauthorized, "authentication required")
		}
		return
	}
	details := map[string]any{"name": name, "project_ids": projects, "expires_at": grant.Expires.Format(time.RFC3339Nano)}
	if err := s.mcpDelegationAudit(actor, "mcp_delegation.create", grant.ID, details); err != nil {
		s.auth.removeDelegation(key)
		log.Printf("mcp delegation: audit failed: %v", err)
		apiErr(w, http.StatusServiceUnavailable, "audit unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, mcpDelegationIssued{mcpDelegation: grant.public(), Token: mcpDelegationPrefix + secret})
}

func (a *AuthService) newDelegationSecret() (string, error) { return authRandom(32) }

func (s *Server) mcpDelegationAudit(actor, action, id string, details any) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	if err = corporateAudit(tx, actor, action, "mcp_delegation", id, details); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// mcpDelegation handles DELETE /api/mcp/delegations/{id}.
func (s *Server) mcpDelegation(w http.ResponseWriter, r *http.Request) {
	mcpDelegationNoStore(w)
	if r.Method != http.MethodDelete {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	if s.auth == nil || s.principal.MCPProjects != nil {
		apiErr(w, http.StatusForbidden, "forbidden")
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	a := s.auth
	var removed *mcpDelegation
	a.mu.Lock()
	for key, g := range a.delegations {
		if g.ID == id && g.OwnerID == actor {
			removed = g
			delete(a.delegations, key)
			break
		}
	}
	a.mu.Unlock()
	if removed == nil {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	// The grant is already dead locally; an audit failure is reported but
	// cannot resurrect it.
	if err := s.mcpDelegationAudit(actor, "mcp_delegation.revoke", id, map[string]any{"name": removed.Name, "project_ids": removed.Projects}); err != nil {
		log.Printf("mcp delegation: audit failed: %v", err)
		apiErr(w, http.StatusServiceUnavailable, "revoked, audit unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authenticateDelegation resolves a temporary delegation bearer (the full
// token including mcpDelegationPrefix) to a read-only, exact-project
// Principal. On failure it has already written the response.
func (a *AuthService) authenticateDelegation(w http.ResponseWriter, r *http.Request, raw string) (Principal, bool) {
	deny := func(status int, msg string) (Principal, bool) {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		authError(w, status, msg)
		return Principal{}, false
	}
	secret, found := strings.CutPrefix(raw, mcpDelegationPrefix)
	if !found || secret == "" {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	key := authHash(secret)

	a.mu.Lock()
	g := a.delegations[key]
	if !a.delegationLiveLocked(key, g, a.now()) {
		a.mu.Unlock()
		return deny(http.StatusUnauthorized, "invalid token")
	}
	// Bound identity-provider and read-audit load before making upstream calls.
	now := a.now()
	if g.windowStart.IsZero() || !now.Before(g.windowStart.Add(time.Minute)) {
		g.windowStart, g.requests = now, 0
	}
	if g.requests >= 120 {
		a.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		return deny(http.StatusTooManyRequests, "temporary MCP request limit exceeded")
	}
	g.requests++
	owner := g.OwnerID
	projects := append([]string{}, g.Projects...)
	sess := a.sessions[g.SessionKey]
	token, subject := sess.accessToken, sess.subject
	a.mu.Unlock()
	if len(projects) == 0 {
		return deny(http.StatusUnauthorized, "invalid token")
	}

	info, err := a.introspect(r.Context(), token)
	if err == nil && info.subject != subject {
		err = errAuthTokenInactive
	}
	if err != nil {
		if errors.Is(err, errAuthTokenInactive) {
			a.removeDelegation(key)
			return deny(http.StatusUnauthorized, "invalid token")
		}
		log.Printf("auth: introspection unavailable: %v", err)
		return deny(http.StatusServiceUnavailable, "authentication unavailable")
	}
	p, fail := a.resolve(r.Context(), info, nil, "", authSyncNone)
	if fail != 0 {
		if fail != http.StatusServiceUnavailable {
			a.removeDelegation(key)
		}
		return deny(fail, authFailText(fail))
	}
	if p.ID != owner {
		a.removeDelegation(key)
		return deny(http.StatusUnauthorized, "invalid token")
	}

	// The browser session may have been logged out or the grant revoked while
	// the identity provider was being asked.
	a.mu.Lock()
	live := a.delegationLiveLocked(key, g, a.now())
	a.mu.Unlock()
	if !live {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	p.Admin = false
	p.MCPProjects = projects
	p.MCPDelegationID = g.ID
	p.MCPExpiresAt = g.Expires
	return p, true
}
