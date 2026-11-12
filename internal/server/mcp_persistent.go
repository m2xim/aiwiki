package server

// Persistent MCP credentials and service accounts.
//
// A credential is a long-lived bearer secret (aiwiki_pat_ + 32 random bytes,
// base64url) minted from an authenticated browser session. It names an exact
// list of agent projects and a read-only switch. Only the SHA-256 of the full
// token is stored; the token is shown exactly once, in the mint response.
//
// Nothing about a credential is cached in process memory, so restarts and
// multiple replicas behave identically. Admission is decided locally, from the
// database only; the identity provider is not contacted per request. Every
// request:
//
//  1. looks the credential up by hash on the separate authentication pool and
//     rejects expired, revoked, unknown, malformed or inconsistent records, a
//     locally disabled owner or service account, and an owner whose last
//     successful browser login is older than mcpPersistentLoginWindow;
//  2. consumes one slot of the per-credential rate limit with a single atomic
//     UPDATE;
//  3. looks the credential up again and re-applies every rule, so a revocation
//     or disable that committed in between is honoured.
//
// Practical boundary: a request that passed step 3 before a revocation
// committed may still run to completion. Anything arriving afterwards is
// refused. Database faults fail closed (503).
//
// Leaving the directory is enforced by the login window: a user who can no
// longer sign in loses every credential at most mcpPersistentLoginWindow after
// the last login. Disable the user locally to revoke immediately.
//
// The owner (sponsor) of a credential or service account is immutable. A
// service account is only an attribution label for audit and authorship; all
// ACL decisions use the sponsoring owner.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	mcpPersistentDefaultTTL = 30 * 24 * time.Hour
	mcpPersistentMinTTL     = 60 * time.Second
	mcpPersistentMaxTTL     = 180 * 24 * time.Hour
	// mcpPersistentLoginWindow: credentials stop working when the owner has not
	// completed a browser login for this long (inactive or departed user).
	mcpPersistentLoginWindow = 14 * 24 * time.Hour
	mcpPersistentMaxName     = 80
	mcpPersistentMaxProjects = 20
	mcpPersistentActiveCap   = 20  // live credentials per owner
	mcpPersistentTotalCap    = 200 // all credentials per owner, revoked and expired included
	mcpPersistentAccountCap  = 20  // service accounts per owner, disabled included
	mcpPersistentRateLimit   = 120 // requests per credential per minute
	mcpPersistentSecretLen   = 43  // base64url (unpadded) of 32 bytes
	mcpPersistentReadRank    = 1   // reader
	mcpPersistentWriteRank   = 3   // editor
)

type mcpCredentialRequest struct {
	Name             string   `json:"name"`
	Projects         []string `json:"projects"`
	ReadOnly         bool     `json:"read_only"`
	TTLSeconds       int      `json:"ttl_seconds"`
	ServiceAccountID string   `json:"service_account_id"`
	// Removed per-row fields: any use is answered with "use projects".
	Scope      json.RawMessage `json:"scope"`
	ProjectIDs json.RawMessage `json:"project_ids"`
	Mode       json.RawMessage `json:"mode"`
}

// mcpProjectIDs validates the project list of a key: 1..20 distinct, well-formed ids.
func mcpProjectIDs(ids []string) ([]string, bool) {
	if len(ids) < 1 || len(ids) > mcpPersistentMaxProjects {
		return nil, false
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !validPathID(id) || seen[id] {
			return nil, false
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

// mcpCredentialIssued is the only shape that ever carries the secret.
type mcpCredentialIssued struct {
	mcpCredential
	Token string `json:"token"`
}

type mcpServiceAccountRequest struct {
	Name string `json:"name"`
}

// mcpPersistentError carries the HTTP outcome of a failed operation.
type mcpPersistentError struct {
	status int
	msg    string
}

func (e *mcpPersistentError) Error() string { return e.msg }

func mcpPersistentFail(status int, msg string) error {
	return &mcpPersistentError{status: status, msg: msg}
}

func mcpPersistentWriteErr(w http.ResponseWriter, err error) {
	var pe *mcpPersistentError
	if errors.As(err, &pe) {
		apiErr(w, pe.status, pe.msg)
		return
	}
	log.Printf("mcp persistent: %v", err)
	apiErr(w, http.StatusServiceUnavailable, "persistent MCP access unavailable")
}

func mcpPersistentNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// clock tolerates a nil receiver so that revocation works with the feature off.
func (p *mcpPersistentAccess) clock() time.Time {
	now := time.Now
	if p != nil && p.now != nil {
		now = p.now
	}
	return now().UTC().Truncate(time.Microsecond) // PostgreSQL precision
}

// mcpPersistentText is the generic refusal text: credential-state problems
// never say which rule failed.
func mcpPersistentText(status int) string {
	if status == http.StatusUnauthorized {
		return "invalid token"
	}
	return authFailText(status)
}

func (p *mcpPersistentAccess) ready() bool {
	return p != nil && p.db != nil
}

// persistentAccess returns the configured feature or nil when it is disabled.
func (s *Server) persistentAccess() *mcpPersistentAccess {
	if s == nil || s.auth == nil || !s.auth.persistent.ready() {
		return nil
	}
	return s.auth.persistent
}

func (s *Server) persistentNow() time.Time {
	var p *mcpPersistentAccess
	if s != nil && s.auth != nil {
		p = s.auth.persistent
	}
	return p.clock()
}

func mcpPersistentHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mcpPersistentWellFormed accepts only the prefix plus a canonical unpadded
// base64url encoding of exactly 32 bytes.
func mcpPersistentWellFormed(raw string) bool {
	secret, ok := strings.CutPrefix(raw, mcpPersistentPrefix)
	if !ok || len(secret) != mcpPersistentSecretLen {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	return err == nil && len(b) == 32
}

// mcpPersistentProjects parses the stored project list. Anything but a non-empty, duplicate-free
// array of at most 20 valid ids is refused (fail closed).
func mcpPersistentProjects(raw []byte) ([]string, bool) {
	var in []string
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false
	}
	return mcpProjectIDs(in)
}

// mcpProjectViews describes the projects of a key for its owner. A project that is deleted or no
// longer visible to the owner is listed as unavailable and carries no name.
func mcpProjectViews(q queryer, ids []string) []mcpProjectView {
	out := make([]mcpProjectView, 0, len(ids))
	for _, id := range ids {
		v := mcpProjectView{ID: id}
		if q.QueryRow(`SELECT name,visibility FROM agent_projects WHERE id=?`, id).Scan(&v.Name, &v.Visibility) == nil {
			v.Available = true
		}
		out = append(out, v)
	}
	return out
}

// mcpPersistentOwner authorises the browser-session caller of a management
// endpoint. Credentials and delegations can never manage credentials.
func (s *Server) mcpPersistentOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, ok := s.corporateActor(w, r, false)
	if !ok {
		return "", false
	}
	if !s.outsideMCP() {
		apiErr(w, http.StatusForbidden, "forbidden")
		return "", false
	}
	return owner, true
}

// mcpPersistentAdmit requires the caller to be a browser-session identity of
// the configured issuer. Authority is only created from such a session, which
// already passed the login-time access-role check; the owner row is then
// locked and re-checked (not disabled) inside the creating transaction.
func (s *Server) mcpPersistentAdmit() error {
	pr := s.principal
	if pr == nil || pr.Issuer == "" || pr.Subject == "" || pr.Issuer != s.auth.cfg.IssuerURL || authUserID(pr.Issuer, pr.Subject) != pr.ID {
		return mcpPersistentFail(http.StatusForbidden, "forbidden")
	}
	return nil
}

func (s *Server) mcpPersistentTx(fn func(tx *transaction) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// mcpPersistentLockOwner serialises every mutation for one owner and, when
// verify is set, requires the locked users row to be the live caller identity.
func (s *Server) mcpPersistentLockOwner(tx *transaction, owner string, verify bool) error {
	var issuer, subject string
	var disabled int
	err := tx.QueryRow(`SELECT issuer,subject,disabled FROM users WHERE id=? FOR UPDATE`, owner).Scan(&issuer, &subject, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpPersistentFail(http.StatusForbidden, "forbidden")
	}
	if err != nil {
		return err
	}
	if disabled != 0 {
		return mcpPersistentFail(http.StatusForbidden, "forbidden")
	}
	if verify && (s.principal == nil || issuer != s.principal.Issuer || subject != s.principal.Subject || authUserID(issuer, subject) != owner) {
		return mcpPersistentFail(http.StatusForbidden, "forbidden")
	}
	return nil
}

func (s *Server) mcpCredentials(w http.ResponseWriter, r *http.Request) {
	mcpPersistentNoStore(w)
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.mcpPersistentOwner(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		s.mcpCredentialMint(w, r, owner)
		return
	}
	rows, err := s.DB.Query(`SELECT id,name,service_account_id,agent_project_ids,read_only,created_at,expires_at,revoked_at FROM mcp_credentials WHERE owner_id=? ORDER BY created_at,id`, owner)
	if err != nil {
		log.Printf("mcp persistent: list credentials: %v", err)
		apiErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := make([]mcpCredential, 0)
	var lists [][]string
	for rows.Next() {
		var c mcpCredential
		var projects []byte
		var revoked sql.NullTime
		if err = rows.Scan(&c.ID, &c.Name, &c.ServiceAccountID, &projects, &c.ReadOnly, &c.CreatedAt, &c.ExpiresAt, &revoked); err != nil {
			break
		}
		c.OwnerID = owner
		c.CreatedAt, c.ExpiresAt = c.CreatedAt.UTC(), c.ExpiresAt.UTC()
		if revoked.Valid {
			t := revoked.Time.UTC()
			c.RevokedAt = &t
		}
		// Invalid stored projects still list (so the key can be revoked) but grant nothing.
		if stored, ok := mcpPersistentProjects(projects); ok {
			lists = append(lists, stored)
		} else {
			lists = append(lists, nil)
		}
		out = append(out, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		log.Printf("mcp persistent: list credentials: %v", err)
		apiErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	for i := range out {
		out[i].Projects = mcpProjectViews(s.DB, lists[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.persistentAccess() != nil, "credentials": out})
}

// mcpCredentialValidate normalises a mint request; msg is the 400 text for any invalid field.
func mcpCredentialValidate(in mcpCredentialRequest) (mcpCredentialRequest, time.Duration, string) {
	if in.Scope != nil || in.ProjectIDs != nil || in.Mode != nil {
		return in, 0, "use projects"
	}
	name, ok := corporateName(in.Name, mcpPersistentMaxName)
	if !ok {
		return in, 0, "invalid request"
	}
	in.Name = name
	projects, ok := mcpProjectIDs(in.Projects)
	if !ok {
		return in, 0, "invalid projects"
	}
	in.Projects = projects
	if in.ServiceAccountID != "" && !validPathID(in.ServiceAccountID) {
		return in, 0, "invalid request"
	}
	ttl := mcpPersistentDefaultTTL
	if in.TTLSeconds != 0 {
		if in.TTLSeconds < 0 || in.TTLSeconds > int(mcpPersistentMaxTTL/time.Second) {
			return in, 0, "invalid request"
		}
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	if ttl < mcpPersistentMinTTL || ttl > mcpPersistentMaxTTL {
		return in, 0, "invalid request"
	}
	return in, ttl, ""
}

func (s *Server) mcpCredentialMint(w http.ResponseWriter, r *http.Request, owner string) {
	p := s.persistentAccess()
	if p == nil {
		apiErr(w, http.StatusServiceUnavailable, "persistent MCP access is not configured")
		return
	}
	var raw mcpCredentialRequest
	if err := corporateDecode(r, &raw); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	in, ttl, msg := mcpCredentialValidate(raw)
	if msg != "" {
		apiErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.mcpPersistentAdmit(); err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	secret, err := authRandom(32)
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	idPart, err := authRandom(12)
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	token := mcpPersistentPrefix + secret
	now := p.clock()
	cred := mcpCredential{
		ID:               "pat_" + idPart,
		Name:             in.Name,
		OwnerID:          owner,
		ServiceAccountID: in.ServiceAccountID,
		CreatedAt:        now,
		ExpiresAt:        now.Add(ttl),
	}
	projectsJSON, err := json.Marshal(in.Projects)
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}

	err = s.mcpPersistentTx(func(tx *transaction) error {
		if err := s.mcpPersistentLockOwner(tx, owner, true); err != nil {
			return err
		}
		if cred.ServiceAccountID != "" {
			var disabled int
			err := tx.QueryRow(`SELECT disabled FROM mcp_service_accounts WHERE id=? AND owner_id=? FOR UPDATE`, cred.ServiceAccountID, owner).Scan(&disabled)
			if errors.Is(err, sql.ErrNoRows) || err == nil && disabled != 0 {
				return mcpPersistentFail(http.StatusForbidden, "forbidden")
			}
			if err != nil {
				return err
			}
		}
		if err := mcpPersistentCheckProjects(tx, in.Projects); err != nil {
			return err
		}
		var active, total int
		if err := tx.QueryRow(`SELECT count(*) FILTER (WHERE revoked_at IS NULL AND expires_at>?),count(*) FROM mcp_credentials WHERE owner_id=?`, now, owner).Scan(&active, &total); err != nil {
			return err
		}
		if active >= mcpPersistentActiveCap || total >= mcpPersistentTotalCap {
			return mcpPersistentFail(http.StatusTooManyRequests, "too many credentials")
		}
		if _, err := tx.Exec(`INSERT INTO mcp_credentials(id,owner_id,service_account_id,name,secret_hash,agent_project_ids,read_only,created_at,expires_at) VALUES(?,?,?,?,?,?::jsonb,?,?,?)`,
			cred.ID, owner, cred.ServiceAccountID, cred.Name, mcpPersistentHash(token), string(projectsJSON), in.ReadOnly, cred.CreatedAt, cred.ExpiresAt); err != nil {
			return err
		}
		return corporateAudit(tx, owner, "mcp_credential.create", "mcp_credential", cred.ID, map[string]any{
			"name": cred.Name, "projects": in.Projects, "read_only": in.ReadOnly,
			"service_account_id": cred.ServiceAccountID, "expires_at": cred.ExpiresAt.Format(time.RFC3339Nano),
		})
	})
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	cred.Projects = mcpProjectViews(s.DB, in.Projects)
	cred.ReadOnly = in.ReadOnly
	writeJSON(w, http.StatusCreated, mcpCredentialIssued{mcpCredential: cred, Token: token})
}

// mcpPersistentCheckProjects requires every project to be visible to the owner right now (own or
// public; row-level security answers for the owner's session). Nothing else is checked: a project
// grants no rights, the owner's own roles are applied at request time.
func mcpPersistentCheckProjects(tx *transaction, ids []string) error {
	var visible int
	if err := tx.QueryRow(`SELECT count(*) FROM agent_projects WHERE id=ANY(?::text[])`, ids).Scan(&visible); err != nil {
		return err
	}
	if visible != len(ids) {
		return mcpPersistentFail(http.StatusBadRequest, "project is not available")
	}
	return nil
}

type mcpCredentialUpdate struct {
	Projects *[]string `json:"projects"`
	ReadOnly *bool     `json:"read_only"`
	// Removed per-row fields: any use is answered with "use projects".
	Scope      json.RawMessage `json:"scope"`
	ProjectIDs json.RawMessage `json:"project_ids"`
	Mode       json.RawMessage `json:"mode"`
}

// mcpCredentialUpdate handles PATCH /api/mcp/credentials/{id}: the owner may change the projects and
// the read-only switch of a live credential. Everything else (owner, secret, service account,
// expiry) stays immutable. New projects must be visible to the owner right now.
func (s *Server) mcpCredentialUpdate(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mcpPersistentOwner(w, r)
	if !ok {
		return
	}
	if s.persistentAccess() == nil {
		apiErr(w, http.StatusServiceUnavailable, "persistent MCP access is not configured")
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	var raw mcpCredentialUpdate
	if err := corporateDecode(r, &raw); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if raw.Scope != nil || raw.ProjectIDs != nil || raw.Mode != nil {
		apiErr(w, http.StatusBadRequest, "use projects")
		return
	}
	if raw.Projects == nil && raw.ReadOnly == nil {
		apiErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	var newProjects []string
	if raw.Projects != nil {
		var ok bool
		if newProjects, ok = mcpProjectIDs(*raw.Projects); !ok {
			apiErr(w, http.StatusBadRequest, "invalid projects")
			return
		}
	}
	if err := s.mcpPersistentAdmit(); err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	now := s.persistentNow()
	var out mcpCredential
	err := s.mcpPersistentTx(func(tx *transaction) error {
		if err := s.mcpPersistentLockOwner(tx, owner, true); err != nil {
			return err
		}
		var current []byte
		var currentRO bool
		err := tx.QueryRow(`SELECT agent_project_ids,read_only FROM mcp_credentials WHERE id=? AND owner_id=? AND revoked_at IS NULL AND expires_at>? FOR UPDATE`, id, owner, now).Scan(&current, &currentRO)
		if errors.Is(err, sql.ErrNoRows) {
			return mcpPersistentFail(http.StatusNotFound, "not found")
		}
		if err != nil {
			return err
		}
		if newProjects == nil {
			var ok bool
			if newProjects, ok = mcpPersistentProjects(current); !ok {
				return mcpPersistentFail(http.StatusConflict, "key projects are invalid; set projects")
			}
		}
		readOnly := currentRO
		if raw.ReadOnly != nil {
			readOnly = *raw.ReadOnly
		}
		// Friendly 400 first for projects that are new to the key; the database function re-checks.
		var old []string
		if prev, ok := mcpPersistentProjects(current); ok {
			old = prev
		}
		var fresh []string
		for _, p := range newProjects {
			if !slices.Contains(old, p) {
				fresh = append(fresh, p)
			}
		}
		if len(fresh) > 0 {
			if err := mcpPersistentCheckProjects(tx, fresh); err != nil {
				return err
			}
		}
		projectsJSON, err := json.Marshal(newProjects)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`SELECT corporate_mcp_credential_set_projects(?,?::jsonb,?)`, id, string(projectsJSON), readOnly); err != nil {
			return err
		}
		var created, expires time.Time
		var name, service string
		if err := tx.QueryRow(`SELECT name,service_account_id,created_at,expires_at FROM mcp_credentials WHERE id=? AND owner_id=?`, id, owner).Scan(&name, &service, &created, &expires); err != nil {
			return err
		}
		out = mcpCredential{ID: id, Name: name, ServiceAccountID: service, Projects: mcpProjectViews(tx, newProjects), ReadOnly: readOnly, CreatedAt: created.UTC(), ExpiresAt: expires.UTC()}
		return nil
	})
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mcpCredential handles DELETE /api/mcp/credentials/{id}. It needs neither the
// feature nor the identity provider: revocation must always be possible.
// PATCH changes projects and mode (mcpCredentialUpdate).
func (s *Server) mcpCredential(w http.ResponseWriter, r *http.Request) {
	mcpPersistentNoStore(w)
	if r.Method == http.MethodPatch {
		s.mcpCredentialUpdate(w, r)
		return
	}
	if r.Method == http.MethodPatch {
		s.mcpServiceAccountRename(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.mcpPersistentOwner(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	now := s.persistentNow()
	err := s.mcpPersistentTx(func(tx *transaction) error {
		var name, service string
		var scope []byte
		err := tx.QueryRow(`UPDATE mcp_credentials SET revoked_at=? WHERE id=? AND owner_id=? AND revoked_at IS NULL RETURNING name,service_account_id,coalesce(agent_project_ids,'[]'::jsonb)`, now, id, owner).Scan(&name, &service, &scope)
		if errors.Is(err, sql.ErrNoRows) {
			// Revoking again is a no-op that keeps the original revoked_at.
			var one int
			err = tx.QueryRow(`SELECT 1 FROM mcp_credentials WHERE id=? AND owner_id=?`, id, owner).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return mcpPersistentFail(http.StatusNotFound, "not found")
			}
			return err
		}
		if err != nil {
			return err
		}
		return corporateAudit(tx, owner, "mcp_credential.revoke", "mcp_credential", id, map[string]any{
			"name": name, "projects": json.RawMessage(scope), "service_account_id": service,
		})
	})
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mcpServiceAccounts(w http.ResponseWriter, r *http.Request) {
	mcpPersistentNoStore(w)
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.mcpPersistentOwner(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		s.mcpServiceAccountCreate(w, r, owner)
		return
	}
	rows, err := s.DB.Query(`SELECT id,name,owner_id,disabled,created_at FROM mcp_service_accounts WHERE owner_id=? ORDER BY created_at,id`, owner)
	if err != nil {
		log.Printf("mcp persistent: list service accounts: %v", err)
		apiErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := make([]mcpServiceAccount, 0)
	for rows.Next() {
		var a mcpServiceAccount
		var disabled int
		if err = rows.Scan(&a.ID, &a.Name, &a.SponsorID, &disabled, &a.CreatedAt); err != nil {
			break
		}
		a.Disabled, a.CreatedAt = disabled != 0, a.CreatedAt.UTC()
		out = append(out, a)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		log.Printf("mcp persistent: list service accounts: %v", err)
		apiErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.persistentAccess() != nil, "service_accounts": out})
}

func (s *Server) mcpServiceAccountCreate(w http.ResponseWriter, r *http.Request, owner string) {
	p := s.persistentAccess()
	if p == nil {
		apiErr(w, http.StatusServiceUnavailable, "persistent MCP access is not configured")
		return
	}
	var in mcpServiceAccountRequest
	if err := corporateDecode(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	name, ok := corporateName(in.Name, mcpPersistentMaxName)
	if !ok {
		apiErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	if err := s.mcpPersistentAdmit(); err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	idPart, err := authRandom(12)
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	account := mcpServiceAccount{ID: "svc_" + idPart, Name: name, SponsorID: owner, CreatedAt: p.clock()}
	err = s.mcpPersistentTx(func(tx *transaction) error {
		if err := s.mcpPersistentLockOwner(tx, owner, true); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM mcp_service_accounts WHERE owner_id=?`, owner).Scan(&count); err != nil {
			return err
		}
		if count >= mcpPersistentAccountCap {
			return mcpPersistentFail(http.StatusTooManyRequests, "too many service accounts")
		}
		if _, err := tx.Exec(`INSERT INTO mcp_service_accounts(id,owner_id,name,disabled,created_at) VALUES(?,?,?,0,?)`, account.ID, owner, account.Name, account.CreatedAt); err != nil {
			return err
		}
		return corporateAudit(tx, owner, "mcp_service_account.create", "mcp_service_account", account.ID, map[string]any{"name": account.Name})
	})
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, account)
}

// mcpServiceAccountRename handles PATCH /api/mcp/service-accounts/{id} {"name"}: the
// sponsor renames a live account. Authorship stays the stored svc_ id, so old
// revisions, comments and artifact history show the new name. A disabled account
// answers 409.
func (s *Server) mcpServiceAccountRename(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mcpPersistentOwner(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	var in mcpServiceAccountRequest
	if err := corporateDecode(r, &in); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	name, ok := corporateName(in.Name, mcpPersistentMaxName)
	if !ok {
		apiErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	if err := s.mcpPersistentAdmit(); err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	var out mcpServiceAccount
	err := s.mcpPersistentTx(func(tx *transaction) error {
		if err := s.mcpPersistentLockOwner(tx, owner, true); err != nil {
			return err
		}
		var old string
		var disabled int
		err := tx.QueryRow(`SELECT name,disabled FROM mcp_service_accounts WHERE id=? AND owner_id=? FOR UPDATE`, id, owner).Scan(&old, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return mcpPersistentFail(http.StatusNotFound, "not found")
		}
		if err != nil {
			return err
		}
		if disabled != 0 {
			return mcpPersistentFail(http.StatusConflict, "service account is disabled")
		}
		if _, err = tx.Exec(`UPDATE mcp_service_accounts SET name=? WHERE id=? AND owner_id=?`, name, id, owner); err != nil {
			return err
		}
		if err = tx.QueryRow(`SELECT id,name,owner_id,disabled,created_at FROM mcp_service_accounts WHERE id=?`, id).Scan(&out.ID, &out.Name, &out.SponsorID, &disabled, &out.CreatedAt); err != nil {
			return err
		}
		out.Disabled, out.CreatedAt = disabled != 0, out.CreatedAt.UTC()
		return corporateAudit(tx, owner, "mcp_service_account.rename", "mcp_service_account", id, map[string]any{"old_name": old, "name": name})
	})
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mcpServiceAccount handles DELETE /api/mcp/service-accounts/{id}: disabling is
// permanent (there is no re-enable) and revokes every credential of the account
// in the same transaction. It works while the identity provider is down.
func (s *Server) mcpServiceAccount(w http.ResponseWriter, r *http.Request) {
	mcpPersistentNoStore(w)
	if r.Method == http.MethodPatch {
		s.mcpServiceAccountRename(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ok := s.mcpPersistentOwner(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	now := s.persistentNow()
	err := s.mcpPersistentTx(func(tx *transaction) error {
		if err := s.mcpPersistentLockOwner(tx, owner, false); err != nil {
			return err
		}
		var name string
		var disabled int
		err := tx.QueryRow(`SELECT name,disabled FROM mcp_service_accounts WHERE id=? AND owner_id=? FOR UPDATE`, id, owner).Scan(&name, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return mcpPersistentFail(http.StatusNotFound, "not found")
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE mcp_service_accounts SET disabled=1 WHERE id=? AND owner_id=?`, id, owner); err != nil {
			return err
		}
		rows, err := tx.Query(`UPDATE mcp_credentials SET revoked_at=? WHERE service_account_id=? AND owner_id=? AND revoked_at IS NULL RETURNING id,name,coalesce(agent_project_ids,'[]'::jsonb)`, now, id, owner)
		if err != nil {
			return err
		}
		type revoked struct {
			id, name string
			scope    []byte
		}
		var done []revoked
		for rows.Next() {
			var c revoked
			if err = rows.Scan(&c.id, &c.name, &c.scope); err != nil {
				break
			}
			done = append(done, c)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if disabled != 0 && len(done) == 0 {
			return nil // already disabled and nothing left to revoke
		}
		sort.Slice(done, func(i, j int) bool { return done[i].id < done[j].id })
		ids := make([]string, 0, len(done))
		for _, c := range done {
			ids = append(ids, c.id)
			if err = corporateAudit(tx, owner, "mcp_credential.revoke", "mcp_credential", c.id, map[string]any{
				"name": c.name, "projects": json.RawMessage(c.scope), "service_account_id": id, "reason": "service_account_disabled",
			}); err != nil {
				return err
			}
		}
		return corporateAudit(tx, owner, "mcp_service_account.disable", "mcp_service_account", id, map[string]any{
			"name": name, "revoked_credential_ids": ids,
		})
	})
	if err != nil {
		mcpPersistentWriteErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- authentication ---

type mcpPersistentRecord struct {
	id, owner, service    string
	projects              []byte
	readOnly              bool
	expires               time.Time
	revoked               sql.NullTime
	issuer, subject, name string
	ownerDisabled         bool
	lastLogin             sql.NullTime
	svcID, svcOwner       sql.NullString
	svcDisabled           sql.NullInt64
}

func (p *mcpPersistentAccess) lookup(ctx context.Context, hash string) (*mcpPersistentRecord, error) {
	var rec mcpPersistentRecord
	var disabled int
	err := p.db.QueryRowContext(ctx, `SELECT c.id,c.owner_id,c.service_account_id,c.agent_project_ids,c.read_only,c.expires_at,c.revoked_at,
u.issuer,u.subject,u.name,u.disabled,u.last_login_at,s.id,s.owner_id,s.disabled
FROM mcp_credentials c JOIN users u ON u.id=c.owner_id
LEFT JOIN mcp_service_accounts s ON s.id=c.service_account_id
WHERE c.secret_hash=?`, hash).Scan(&rec.id, &rec.owner, &rec.service, &rec.projects, &rec.readOnly, &rec.expires, &rec.revoked,
		&rec.issuer, &rec.subject, &rec.name, &disabled, &rec.lastLogin, &rec.svcID, &rec.svcOwner, &rec.svcDisabled)
	if err != nil {
		return nil, err
	}
	rec.ownerDisabled = disabled != 0
	rec.expires = rec.expires.UTC()
	return &rec, nil
}

// persistentVerify applies every local rule to a stored record. A non-zero
// status is the HTTP refusal; credential-state problems are a generic 401.
func (a *AuthService) persistentVerify(rec *mcpPersistentRecord, now time.Time) (Principal, int) {
	if !strings.HasPrefix(rec.id, "pat_") || !validPathID(rec.id) || len(rec.id) > 128 {
		return Principal{}, http.StatusUnauthorized
	}
	if rec.revoked.Valid || !now.Before(rec.expires) {
		return Principal{}, http.StatusUnauthorized
	}
	projects, ok := mcpPersistentProjects(rec.projects)
	if !ok {
		log.Printf("mcp persistent: credential %s has invalid stored projects", rec.id)
		return Principal{}, http.StatusUnauthorized
	}
	if authUserID(rec.issuer, rec.subject) != rec.owner || rec.issuer != a.cfg.IssuerURL || rec.ownerDisabled {
		return Principal{}, http.StatusForbidden
	}
	// Local admission: the owner must have signed in recently.
	if !rec.lastLogin.Valid || !now.Before(rec.lastLogin.Time.Add(mcpPersistentLoginWindow)) {
		return Principal{}, http.StatusForbidden
	}
	actor := rec.owner
	if rec.service != "" {
		if !strings.HasPrefix(rec.service, "svc_") || !validPathID(rec.service) || !rec.svcID.Valid || rec.svcID.String != rec.service || !rec.svcOwner.Valid || rec.svcOwner.String != rec.owner || !rec.svcDisabled.Valid || rec.svcDisabled.Int64 != 0 {
			return Principal{}, http.StatusForbidden
		}
		actor = rec.service
	}
	return Principal{
		ID:              rec.owner,
		Issuer:          rec.issuer,
		Subject:         rec.subject,
		Name:            rec.name,
		Admin:           false,
		MCPKeyProjects:  projects,
		MCPReadOnly:     rec.readOnly,
		MCPDelegationID: rec.id,
		MCPExpiresAt:    rec.expires,
		MCPActorID:      actor,
	}, 0
}

// mcpPersistentFresh is true when the rate window is unset, over, or in the
// future (clock moved back). Each use takes two timestamp arguments.
const mcpPersistentFresh = `(rate_window IS NULL OR rate_window<=?::timestamptz - interval '1 minute' OR rate_window>?::timestamptz)`

// takeRate consumes one request slot of a live credential in one statement.
// false means no slot was taken: rate limited, revoked, expired or unknown.
func (p *mcpPersistentAccess) takeRate(ctx context.Context, hash string, now time.Time) (bool, error) {
	res, err := p.db.ExecContext(ctx, `UPDATE mcp_credentials SET
rate_requests=CASE WHEN `+mcpPersistentFresh+` THEN 1 ELSE rate_requests+1 END,
rate_window=CASE WHEN `+mcpPersistentFresh+` THEN ?::timestamptz ELSE rate_window END
WHERE secret_hash=? AND revoked_at IS NULL AND expires_at>?::timestamptz AND (`+mcpPersistentFresh+` OR rate_requests<?)`,
		now, now, now, now, now, hash, now, now, now, mcpPersistentRateLimit)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// authenticatePersistent resolves a persistent MCP bearer (the full token
// including mcpPersistentPrefix) to an exact-project Principal. On failure it
// has already written the response.
func (a *AuthService) authenticatePersistent(w http.ResponseWriter, r *http.Request, raw string) (Principal, bool) {
	deny := func(status int, msg string) (Principal, bool) {
		switch status {
		case http.StatusUnauthorized:
			w.Header().Set("WWW-Authenticate", a.mcpBearerChallenge("invalid_token"))
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "60")
		}
		authError(w, status, msg)
		return Principal{}, false
	}
	p := a.persistent
	if !p.ready() {
		return deny(http.StatusServiceUnavailable, "persistent MCP access is not enabled")
	}
	if !mcpPersistentWellFormed(raw) {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	ctx, hash := r.Context(), mcpPersistentHash(raw)
	unavailable := func(err error) (Principal, bool) {
		log.Printf("mcp persistent: authentication unavailable: %v", err)
		return deny(http.StatusServiceUnavailable, "authentication unavailable")
	}

	rec, err := p.lookup(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	if err != nil {
		return unavailable(err)
	}
	if _, status := a.persistentVerify(rec, p.clock()); status != 0 {
		return deny(status, mcpPersistentText(status))
	}

	took, err := p.takeRate(ctx, hash, p.clock())
	if err != nil {
		return unavailable(err)
	}
	if !took {
		cur, err := p.lookup(ctx, hash)
		if errors.Is(err, sql.ErrNoRows) {
			return deny(http.StatusUnauthorized, "invalid token")
		}
		if err != nil {
			return unavailable(err)
		}
		if _, status := a.persistentVerify(cur, p.clock()); status != 0 {
			return deny(status, mcpPersistentText(status))
		}
		return deny(http.StatusTooManyRequests, "MCP request limit exceeded")
	}

	if p.checkpoint != nil {
		p.checkpoint()
	}
	// The credential may have been revoked or its owner disabled meanwhile.
	cur, err := p.lookup(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	if err != nil {
		return unavailable(err)
	}
	if cur.id != rec.id || cur.owner != rec.owner || cur.issuer != rec.issuer || cur.subject != rec.subject {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	principal, status := a.persistentVerify(cur, p.clock())
	if status != 0 {
		return deny(status, mcpPersistentText(status))
	}
	return principal, true
}
