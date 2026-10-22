package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

// Per-space storage quotas (corporate mode, docs/OPERATIONS.md section 5). The
// accounting lives in PostgreSQL (storage_quotas.sql); Go only decides where it
// is asked and how a refusal is reported.

// spaceQuotaMessage is the whole client-visible text of a refusal: no usage and
// no limit, because the usage covers content the caller may not see.
const spaceQuotaMessage = "space storage quota exceeded"

// quotaItem is one stored object an operation would add: 'a:'+stored name for an
// attachment, 'b:'+sha256 for an artifact file.
type quotaItem struct {
	Key  string
	Size int64
}

func attachmentQuotaItem(storedName string, size int64) quotaItem {
	return quotaItem{"a:" + storedName, size}
}

func artifactQuotaItems(artifactID string, blobs []artifactBlob) []quotaItem {
	seen := map[string]bool{}
	var out []quotaItem
	for _, b := range blobs {
		if seen[b.File.SHA256] {
			continue
		}
		seen[b.File.SHA256] = true
		out = append(out, quotaItem{"b:" + artifactID + ":" + b.File.SHA256, b.File.Size})
	}
	return out
}

// quotaApplies reports whether this request is subject to quotas: corporate mode
// only. Trusted (single-user SQLite) mode has none.
func (s *Server) quotaApplies() bool {
	return s != nil && s.DB != nil && s.DB.pg && s.principal != nil
}

// quotaAdmit asks the database whether the items fit. It takes the space's
// advisory lock for the rest of tx, so the caller must insert in the same
// transaction. An operation that adds nothing is not checked.
func (s *Server) quotaAdmit(q queryer, space, project string, items []quotaItem) error {
	if !s.quotaApplies() || len(items) == 0 {
		return nil
	}
	sorted := append([]quotaItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	keys := make([]string, len(sorted))
	sizes := make([]int64, len(sorted))
	for i, it := range sorted {
		keys[i], sizes[i] = it.Key, it.Size
	}
	var ok bool
	if err := q.QueryRow(`SELECT space_storage_admit(?,?,?,?,?)`, space, project, s.spaceQuotaDefault, keys, sizes).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return artErr(http.StatusRequestEntityTooLarge, spaceQuotaMessage)
	}
	return nil
}

// quotaPrecheck is the same question in a transaction of its own, asked before
// any bytes are sent to the object store; the authoritative check is the one
// made in the writing transaction.
func (s *Server) quotaPrecheck(d *database, space, project string, items []quotaItem) error {
	if !s.quotaApplies() || len(items) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return s.quotaAdmit(tx, space, project, items)
}

func isQuotaError(err error) bool {
	var ae *artifactError
	return errors.As(err, &ae) && ae.Code == http.StatusRequestEntityTooLarge && ae.Msg == spaceQuotaMessage
}

// ---- visibility and administration ----

type spaceStorageOut struct {
	Used     int64  `json:"used"`
	Limit    int64  `json:"limit"` // 0 = unlimited
	Override *int64 `json:"override"`
	Default  int64  `json:"default"`
}

// spaceStorage answers GET /api/spaces/{id}/storage for managers and owners of
// the space and for system administrators; everybody else (and every unknown
// space) gets the same 403, so the endpoint tells nothing about spaces the
// caller cannot see.
func (s *Server) spaceStorage(w http.ResponseWriter, r *http.Request) {
	if !s.quotaApplies() {
		apiErr(w, http.StatusNotFound, "not found")
		return
	}
	if s.isMCPScoped() {
		apiErr(w, http.StatusForbidden, "forbidden")
		return
	}
	id := r.PathValue("id")
	var used sql.NullInt64
	if err := s.DB.QueryRow(`SELECT space_storage_used(?)`, id).Scan(&used); err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !used.Valid {
		apiErr(w, http.StatusForbidden, "forbidden")
		return
	}
	var override sql.NullInt64
	if err := s.DB.QueryRow(`SELECT space_storage_override(?)`, id).Scan(&override); err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	out := spaceStorageOut{Used: used.Int64, Limit: s.spaceQuotaDefault, Default: s.spaceQuotaDefault}
	if override.Valid {
		v := override.Int64
		out.Override, out.Limit = &v, v
	}
	writeJSON(w, http.StatusOK, out)
}

// parseQuotaField reads the storage_quota_bytes member of a space update:
// null restores the default, 0 is unlimited, anything else must be a whole
// non-negative number of bytes.
func parseQuotaField(raw json.RawMessage) (*int64, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	var v int64
	if json.Unmarshal(raw, &v) != nil || v < 0 {
		return nil, false
	}
	return &v, true
}

// setSpaceQuota stores the quota and audits it. It runs as the administrator
// (space_set_storage_quota checks that again in the database).
func (s *Server) setSpaceQuota(tx *transaction, actor, space string, quota *int64) (bool, error) {
	var v any
	if quota != nil {
		v = *quota
	}
	var found bool
	if err := tx.QueryRow(`SELECT space_set_storage_quota(?,?::bigint)`, space, v).Scan(&found); err != nil || !found {
		return found, err
	}
	details := map[string]any{"default": true}
	if quota != nil {
		details = map[string]any{"storage_quota_bytes": *quota}
	}
	return true, corporateAudit(tx, actor, "space.storage_quota", "space", space, details)
}

// spaceQuotaOnly handles PATCH /api/spaces/{id} carrying nothing but the quota:
// the administrator needs no role in the space.
func (s *Server) spaceQuotaOnly(w http.ResponseWriter, r *http.Request, actor string, quota *int64) {
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	found, err := s.setSpaceQuota(tx, actor, r.PathValue("id"), quota)
	if err == nil && found {
		err = tx.Commit()
	}
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "42501":
		apiErr(w, 403, "forbidden")
	case err != nil:
		apiErr(w, 500, "database error")
	case !found:
		apiErr(w, 404, "space not found")
	default:
		writeJSON(w, 200, map[string]any{"id": r.PathValue("id"), "storage_quota_bytes": quota})
	}
}

// validateStorageQuotaSchema is the startup check of the quota objects: the
// column, the guard trigger, the probe policies, and that every definer function
// is fenced (fixed search_path, probe flags, owner not the runtime role, no
// PUBLIC execute).
func (d *database) validateStorageQuotaSchema() error {
	const fixedPath = `ARRAY['search_path=pg_catalog, public, pg_temp']`
	const all = `to_regprocedure('public.space_storage_used(text)'),to_regprocedure('public.space_storage_override(text)'),to_regprocedure('public.space_storage_admit(text,text,bigint,text[],bigint[])'),to_regprocedure('public.space_set_storage_quota(text,bigint)'),to_regprocedure('public.space_storage_objects(text)'),to_regprocedure('public.storage_quota_guard()')`
	var ok bool
	if err := d.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='public.spaces'::regclass AND attname='storage_quota_bytes' AND NOT attisdropped)
 AND (SELECT count(*) FROM pg_policy p JOIN (VALUES ('storage_quota_probe_spaces','public.spaces'::regclass,'r'),('storage_quota_probe_spaces_update','public.spaces'::regclass,'w'),('storage_quota_probe_articles','public.articles'::regclass,'r'),('storage_quota_probe_attachments','public.attachments'::regclass,'r')) v(n,r,c) ON p.polname=v.n AND p.polrelid=v.r AND p.polcmd=v.c::"char"
     WHERE p.polpermissive AND pg_get_expr(p.polqual,p.polrelid) ILIKE '%session_user%' AND pg_get_expr(p.polqual,p.polrelid) LIKE '%quota_probe%'
       AND (v.c='r' OR (pg_get_expr(p.polwithcheck,p.polrelid) ILIKE '%session_user%' AND pg_get_expr(p.polwithcheck,p.polrelid) LIKE '%quota_probe%')))=4
 AND (SELECT count(*) FROM pg_policy p WHERE p.polname LIKE 'storage_quota_probe_%')=4
 AND EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid='public.spaces'::regclass AND t.tgname='spaces_storage_quota_guard' AND t.tgenabled IN ('O','A') AND t.tgtype=23 AND t.tgfoid=to_regprocedure('public.storage_quota_guard()'))
 AND EXISTS (SELECT 1 FROM pg_proc p WHERE p.oid=to_regprocedure('public.storage_quota_guard()') AND NOT p.prosecdef AND p.proconfig=` + fixedPath + ` AND p.prosrc LIKE '%quota_probe%' AND p.prosrc LIKE '%session_user%')
 AND EXISTS (SELECT 1 FROM pg_proc p WHERE p.oid=to_regprocedure('public.space_storage_objects(text)') AND NOT p.prosecdef AND p.proconfig=` + fixedPath + ` AND p.prosrc LIKE '%session_user%')`).Scan(&ok); err != nil || !ok {
		return errors.New("storage quota schema (storage_quotas v1) is missing or weakened")
	}
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM pg_proc p WHERE p.oid IN (to_regprocedure('public.space_storage_used(text)'),to_regprocedure('public.space_storage_override(text)'),to_regprocedure('public.space_storage_admit(text,text,bigint,text[],bigint[])'),to_regprocedure('public.space_set_storage_quota(text,bigint)'))
     AND p.prosecdef AND p.proconfig=` + fixedPath + ` AND p.prosrc LIKE '%quota_probe%' AND NOT pg_has_role(current_user,p.proowner,'MEMBER') AND has_function_privilege(current_user,p.oid,'EXECUTE'))=4
 AND NOT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid IN (` + all + `) AND a.grantee=0)
 AND (SELECT count(*) FROM pg_proc p WHERE p.oid IN (to_regprocedure('public.space_storage_used(text)'),to_regprocedure('public.space_storage_admit(text,text,bigint,text[],bigint[])')) AND p.prosrc LIKE '%artifact_probe%')=2
 AND (SELECT count(*) FROM pg_proc p WHERE p.oid=to_regprocedure('public.space_storage_admit(text,text,bigint,text[],bigint[])') AND p.prosrc LIKE '%pg_advisory_xact_lock%' AND p.prosrc LIKE '%space_storage_objects%')=1
 AND (SELECT count(*) FROM pg_proc p WHERE p.oid IN (to_regprocedure('public.space_storage_used(text)'),to_regprocedure('public.space_storage_override(text)')) AND p.prosrc LIKE '%corporate_effective_role%' AND p.prosrc LIKE '%>=4%')=2
 AND (SELECT count(*) FROM pg_proc p WHERE p.oid=to_regprocedure('public.space_set_storage_quota(text,bigint)') AND p.prosrc LIKE '%artifact_is_admin%')=1`).Scan(&ok); err != nil || !ok {
		return errors.New("storage quota functions are missing, weakened, not SECURITY DEFINER, without the fixed search_path or executable by PUBLIC")
	}
	return nil
}
