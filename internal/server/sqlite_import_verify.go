package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
)

func putImportObject(ctx context.Context, store objectStore, o importObject) error {
	f, err := os.Open(o.Path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", o.Key, err)
	}
	defer f.Close()
	if err = store.Put(ctx, o.Key, f, o.Size, o.MIME); err != nil {
		return fmt.Errorf("uploading %s: %w", o.Key, err)
	}
	return nil
}

// verifyObjects reads every object back and compares size and SHA-256 with the
// source file. It returns one line per mismatch.
func verifyObjects(ctx context.Context, store objectStore, objs []importObject) []string {
	var bad []string
	for _, o := range objs {
		rc, err := store.Get(ctx, o.Key)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: cannot read back (%v)", o.Key, err))
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, rc)
		rc.Close()
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: read error", o.Key))
		case n != o.Size:
			bad = append(bad, fmt.Sprintf("%s: %d bytes in the bucket, %d in the source", o.Key, n, o.Size))
		case hex.EncodeToString(h.Sum(nil)) != o.SHA256:
			bad = append(bad, fmt.Sprintf("%s: SHA-256 differs from the source file", o.Key))
		}
	}
	return bad
}

// diffRows compares two row lists by key; with subset, rows only the target has
// are not a difference. Differing rows are reported with their column names, not
// their content.
func diffRows[T any](want, got []T, key func(T) string, subset bool) []string {
	gm := make(map[string]T, len(got))
	for _, g := range got {
		gm[key(g)] = g
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range want {
		k := key(w)
		seen[k] = true
		g, ok := gm[k]
		if !ok {
			out = append(out, "missing "+k)
			continue
		}
		if reflect.DeepEqual(w, g) {
			continue
		}
		var cols []string
		wv, gv := reflect.ValueOf(w), reflect.ValueOf(g)
		for i := 0; i < wv.NumField(); i++ {
			if !reflect.DeepEqual(wv.Field(i).Interface(), gv.Field(i).Interface()) {
				cols = append(cols, wv.Type().Field(i).Name)
			}
		}
		out = append(out, fmt.Sprintf("%s differs in %s", k, strings.Join(cols, ",")))
	}
	if !subset {
		for _, g := range got {
			if k := key(g); !seen[k] {
				out = append(out, "unexpected "+k)
			}
		}
	}
	return out
}

// expectedTarget is the source as it must read back: the owner identity replaces
// the empty owner of artifacts and links.
func expectedTarget(d *impData) *impData {
	e := *d
	e.Artifacts = append([]impArtifact(nil), d.Artifacts...)
	for i := range e.Artifacts {
		e.Artifacts[i].OwnerID = personalOwnerID
	}
	e.ArtShares = append([]impArtifactShare(nil), d.ArtShares...)
	for i := range e.ArtShares {
		e.ArtShares[i].CreatedBy = personalOwnerID
	}
	return &e
}

func ftsRowsOf(ctx context.Context, q rowQuerier) (map[string][4]string, error) {
	rows, err := scanAll(ctx, q, `SELECT article_id,title,summary,body,tags FROM article_fts ORDER BY article_id`, func(r *sql.Rows) (v [5]string, err error) {
		err = r.Scan(&v[0], &v[1], &v[2], &v[3], &v[4])
		return
	})
	if err != nil {
		return nil, err
	}
	m := make(map[string][4]string, len(rows))
	for _, v := range rows {
		m[v[0]] = [4]string{v[1], v[2], v[3], v[4]}
	}
	return m, nil
}

// compareData prints one verification line per table. blobs: the target rows
// of artifact_blobs were read as well (the runtime role cannot read them).
func (r *importRun) compareData(src *importSource, got *impData, blobs bool) {
	want := expectedTarget(src.Data)
	line := func(name string, nWant, nGot int, problems []string) {
		detail := fmt.Sprintf("source %d, target %d", nWant, nGot)
		if len(problems) > 0 {
			shown := problems
			if len(shown) > 5 {
				shown = append(append([]string(nil), shown[:5]...), fmt.Sprintf("... %d more", len(problems)-5))
			}
			detail += ": " + strings.Join(shown, "; ")
		}
		r.check(len(problems) == 0 && nWant <= nGot, name, detail)
	}
	line("spaces", len(want.Spaces), len(got.Spaces), diffRows(want.Spaces, got.Spaces, func(s impSpace) string { return s.ID }, true))
	line("articles (all fields)", len(want.Articles), len(got.Articles), diffRows(want.Articles, got.Articles, func(a impArticle) string { return a.ID }, false))
	line("revisions (ids, numbers, snapshots)", len(want.Revisions), len(got.Revisions), diffRows(want.Revisions, got.Revisions, func(v impRevision) string { return fmt.Sprint(v.ID) }, false))
	line("comments", len(want.Comments), len(got.Comments), diffRows(want.Comments, got.Comments, func(c impComment) string { return c.ID }, false))
	line("attachments", len(want.Attachments), len(got.Attachments), diffRows(want.Attachments, got.Attachments, func(a impAttachment) string { return a.ID }, false))
	line("favorites", len(want.Favorites), len(got.Favorites), diffRows(want.Favorites, got.Favorites, func(f impFavorite) string { return f.ArticleID }, false))
	line("artifacts", len(want.Artifacts), len(got.Artifacts), diffRows(want.Artifacts, got.Artifacts, func(a impArtifact) string { return a.ID }, false))
	line("artifact revisions", len(want.ArtRevs), len(got.ArtRevs), diffRows(want.ArtRevs, got.ArtRevs, func(v impArtifactRevision) string { return fmt.Sprintf("%s#%d", v.ArtifactID, v.Revision) }, false))
	if blobs {
		line("artifact blobs", len(want.ArtBlobs), len(got.ArtBlobs), diffRows(want.ArtBlobs, got.ArtBlobs, func(b impArtifactBlob) string { return b.SHA256 }, true))
	}
	line("artifact links", len(want.ArtShares), len(got.ArtShares), diffRows(want.ArtShares, got.ArtShares, func(s impArtifactShare) string { return s.ID }, false))
	line("settings", len(want.Settings), len(got.Settings), diffRows(want.Settings, got.Settings, func(s impSetting) string { return s.Key }, true))
	live := 0
	for _, a := range src.Ordered {
		if !a.DeletedAt.Valid {
			live++
		}
	}
	r.check(got.FTSRows == live, "full-text rows", fmt.Sprintf("live articles %d, rows %d (source had %d)", live, got.FTSRows, src.Data.FTSRows))
}

// compareFTS checks the content of the rebuilt search rows against the server's
// indexing rule.
func (r *importRun) compareFTS(src *importSource, got map[string][4]string) {
	var bad []string
	for _, a := range src.Ordered {
		if a.DeletedAt.Valid {
			if _, ok := got[a.ID]; ok {
				bad = append(bad, "deleted article "+a.ID+" is indexed")
			}
			continue
		}
		art := a.article()
		w := [4]string{art.Title, art.Summary, art.Body, join(art.Tags, " ")}
		if g, ok := got[a.ID]; !ok {
			bad = append(bad, "missing "+a.ID)
		} else if g != w {
			bad = append(bad, "differs "+a.ID)
		}
	}
	detail := fmt.Sprintf("%d rows compared with the indexing rule", len(got))
	if len(bad) > 0 {
		if len(bad) > 5 {
			bad = append(bad[:5], "...")
		}
		detail += ": " + strings.Join(bad, "; ")
	}
	r.check(len(bad) == 0, "full-text content", detail)
}

// verify reads the committed data back through the runtime role scoped as the
// owner (so row-level security applies), the bucket, and the owner identity.
func (r *importRun) verify(ctx context.Context, imp *sql.DB, store objectStore, src *importSource) error {
	rt, err := openPostgres(ctx, r.cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("runtime role: %w", err)
	}
	defer rt.Close()
	scoped, cleanup, err := rt.scoped(ctx, personalOwnerID)
	if err != nil {
		return err
	}
	defer cleanup()
	got, err := loadImpData(ctx, scoped, false)
	if err != nil {
		return fmt.Errorf("the owner cannot read the imported data: %w", err)
	}
	r.compareData(src, got, false)
	fts, err := ftsRowsOf(ctx, scoped)
	if err != nil {
		return err
	}
	r.compareFTS(src, fts)

	// Tables the runtime role cannot read, and the identity rows, as the import role.
	blobs, err := scanAll(ctx, imp, `SELECT sha256,size,mime,stored_name,created_at FROM artifact_blobs ORDER BY sha256`, func(rows *sql.Rows) (v impArtifactBlob, err error) {
		err = rows.Scan(&v.SHA256, &v.Size, &v.MIME, &v.StoredName, &v.CreatedAt)
		return
	})
	if err != nil {
		return err
	}
	bp := diffRows(src.Data.ArtBlobs, blobs, func(b impArtifactBlob) string { return b.SHA256 }, true)
	r.check(len(bp) == 0, "artifact blobs", fmt.Sprintf("source %d, target %d%s", len(src.Data.ArtBlobs), len(blobs), listProblems(bp)))

	var name string
	var admin, disabled int
	var issuer, subject string
	err = imp.QueryRowContext(ctx, `SELECT issuer,subject,name,admin,disabled FROM users WHERE id=$1`, personalOwnerID).Scan(&issuer, &subject, &name, &admin, &disabled)
	r.check(err == nil && issuer == personalOwnerIssuer && subject == personalOwnerSubject && name == r.cfg.OwnerName && admin == 1 && disabled == 0,
		"owner identity", "users row id=owner")
	var missing []string
	for _, s := range src.Data.Spaces {
		var n int
		if e := imp.QueryRowContext(ctx, `SELECT count(*) FROM spaces s JOIN memberships m ON m.resource_type='space' AND m.resource_id=s.id AND m.subject_type='user' AND m.subject_id=$1 AND m.role='owner' WHERE s.id=$2 AND s.owner_id=$1 AND s.personal=0`, personalOwnerID, s.ID).Scan(&n); e != nil || n != 1 {
			missing = append(missing, s.ID)
		}
	}
	r.check(len(missing) == 0, "space ownership", fmt.Sprintf("%d spaces owned by the owner with an owner membership%s", len(src.Data.Spaces)-len(missing), listProblems(missing)))
	var states int
	_ = imp.QueryRowContext(ctx, `SELECT count(*) FROM corporate_space_state`).Scan(&states)
	r.check(states >= len(src.Data.Spaces), "archive state rows", fmt.Sprintf("%d rows for %d spaces", states, len(src.Data.Spaces)))

	var foreign int
	err = imp.QueryRowContext(ctx, `SELECT count(*) FROM favorites WHERE user_id<>$1`, personalOwnerID).Scan(&foreign)
	r.check(err == nil && foreign == 0, "favorites of other users", fmt.Sprintf("%d rows with user_id other than owner", foreign))

	var seqOK bool
	err = imp.QueryRowContext(ctx, `SELECT last_value >= (SELECT coalesce(max(id),0) FROM revisions) FROM revisions_id_seq`).Scan(&seqOK)
	r.check(err == nil && seqOK, "revision id sequence", "next id is above every imported id")

	bad := verifyObjects(ctx, store, src.Objects)
	r.check(len(bad) == 0, "bucket objects (SHA-256 read back)", fmt.Sprintf("%d objects%s", len(src.Objects), listProblems(bad)))
	if r.rep.Mismatches == 0 {
		r.printf("Verification passed: %d checks", r.rep.Checks)
	} else {
		r.printf("VERIFICATION FAILED: %d of %d checks", r.rep.Mismatches, r.rep.Checks)
	}
	return nil
}

func listProblems(p []string) string {
	if len(p) == 0 {
		return ""
	}
	sort.Strings(p)
	if len(p) > 5 {
		p = append(p[:5], "...")
	}
	return ": " + strings.Join(p, "; ")
}
