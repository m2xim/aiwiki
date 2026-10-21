package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// database is the server's handle to either SQLite (legacy, pg=false) or
// PostgreSQL (corporate, pg=true). Queries are written with "?" placeholders and
// bound for the dialect here.
//
// A root database wraps the pool. A scoped database (see scoped) is bound to one
// dedicated connection carrying the session user; the pool field is private and
// only explicitly scoped operations are available, so a
// handler holding it cannot step outside the scope.
//
// A bound connection runs one statement at a time: do not issue another query
// (or use the same scoped database from another goroutine) while a *sql.Rows from
// it is still open. Read the rows to completion, close them, then continue.
type database struct {
	pool     *sql.DB
	ctx      context.Context
	pg       bool
	conn     *sql.Conn
	release  func()
	resume   func(context.Context) (*database, func(), error)
	activeTx []*sql.Tx
}

// transaction is a *sql.Tx whose statements are bound for the database dialect.
type transaction struct {
	*sql.Tx
	pg  bool
	ctx context.Context
}

func (d *database) bind(q string) string {
	if d.pg {
		return rebindPostgres(q)
	}
	return q
}

func (d *database) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if d.ctx != nil {
		ctx = d.ctx
	}
	q = d.bind(q)
	if d.conn != nil {
		return d.conn.ExecContext(ctx, q, args...)
	}
	return d.pool.ExecContext(ctx, q, args...)
}

func (d *database) Exec(q string, args ...any) (sql.Result, error) {
	return d.ExecContext(d.context(), q, args...)
}

func (d *database) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if d.ctx != nil {
		ctx = d.ctx
	}
	q = d.bind(q)
	if d.conn != nil {
		return d.conn.QueryContext(ctx, q, args...)
	}
	return d.pool.QueryContext(ctx, q, args...)
}

func (d *database) Query(q string, args ...any) (*sql.Rows, error) {
	return d.QueryContext(d.context(), q, args...)
}

func (d *database) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if d.ctx != nil {
		ctx = d.ctx
	}
	q = d.bind(q)
	if d.conn != nil {
		return d.conn.QueryRowContext(ctx, q, args...)
	}
	return d.pool.QueryRowContext(ctx, q, args...)
}

func (d *database) QueryRow(q string, args ...any) *sql.Row {
	return d.QueryRowContext(d.context(), q, args...)
}

func (d *database) BeginTx(ctx context.Context, opts *sql.TxOptions) (*transaction, error) {
	var tx *sql.Tx
	var err error
	if d.conn != nil {
		tx, err = d.conn.BeginTx(ctx, opts)
	} else {
		tx, err = d.pool.BeginTx(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	if d.conn != nil {
		d.activeTx = append(d.activeTx, tx)
	}
	return &transaction{Tx: tx, pg: d.pg, ctx: d.context()}, nil
}

func (d *database) Begin() (*transaction, error) { return d.BeginTx(d.context(), nil) }

// PingContext checks the pool (root) or the pinned connection (scoped).
func (d *database) PingContext(ctx context.Context) error {
	if d.conn != nil {
		return d.conn.PingContext(ctx)
	}
	return d.pool.PingContext(ctx)
}

func (d *database) Ping() error { return d.PingContext(context.Background()) }

// Close closes the pool. On a scoped database it is a no-op: the scope's cleanup
// function owns the connection.
func (d *database) Close() error {
	if d.conn != nil {
		return nil
	}
	return d.pool.Close()
}

// Prepare, PrepareContext and Conn would bypass dialect binding or the scope.
func (d *database) Prepare(string) (*sql.Stmt, error) { return nil, errUnsupportedDB }
func (d *database) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, errUnsupportedDB
}
func (d *database) Conn(context.Context) (*sql.Conn, error) { return nil, errUnsupportedDB }

var errUnsupportedDB = errors.New("unsupported on database wrapper")

func (t *transaction) bind(q string) string {
	if t.pg {
		return rebindPostgres(q)
	}
	return q
}

func (t *transaction) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.bind(q), args...)
}
func (t *transaction) Exec(q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(t.context(), t.bind(q), args...)
}
func (t *transaction) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, t.bind(q), args...)
}
func (t *transaction) Query(q string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(t.context(), t.bind(q), args...)
}
func (t *transaction) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.bind(q), args...)
}
func (t *transaction) QueryRow(q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(t.context(), t.bind(q), args...)
}
func (t *transaction) Prepare(string) (*sql.Stmt, error) { return nil, errUnsupportedDB }
func (t *transaction) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, errUnsupportedDB
}

// scoped returns a database pinned to one connection whose session variable
// app.user_id is userID, plus a cleanup function that must be called exactly
// when the request ends (extra calls are harmless). Cleanup clears the variable
// and verifies it; if that fails the connection is discarded rather than
// returned to the pool. SQLite has no row-level security, so there the result is
// an unpinned view of the same handle.
func (d *database) scoped(ctx context.Context, userID string) (*database, func(), error) {
	if !d.pg {
		return &database{pool: d.pool}, func() {}, nil
	}
	if d.conn != nil || d.pool == nil {
		return nil, nil, errors.New("database is already scoped")
	}
	if userID == "" {
		return nil, nil, errors.New("scoped database requires a user id")
	}
	conn, err := d.pool.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var got string
	err = conn.QueryRowContext(ctx, `SELECT set_config('app.user_id', $1, false)`, userID).Scan(&got)
	if err == nil && got != userID {
		err = errors.New("session user was not applied")
	}
	if err != nil {
		discardConn(conn)
		return nil, nil, err
	}
	view := &database{pg: true, conn: conn, ctx: ctx}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, tx := range view.activeTx {
				_ = tx.Rollback()
			}
			if _, err := conn.ExecContext(cctx, "ROLLBACK"); err != nil {
				discardConn(conn)
				return
			}
			var cleared string
			err := conn.QueryRowContext(cctx, `SELECT set_config('app.user_id', '', false)`).Scan(&cleared)
			if err != nil || cleared != "" {
				discardConn(conn)
				return
			}
			_ = conn.Close()
		})
	}
	view.release = cleanup
	view.resume = func(ctx context.Context) (*database, func(), error) { return d.scoped(ctx, userID) }
	return view, cleanup, nil
}

// discardConn makes database/sql drop the physical connection, then releases it.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// rebindPostgres turns "?" placeholders into $1,$2,... and leaves everything
// inside string literals (including E” strings), quoted identifiers, "--" and
// nested "/* */" comments, and dollar-quoted strings untouched. Existing $n
// placeholders are passed through.
func rebindPostgres(q string) string {
	if !strings.ContainsRune(q, '?') {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'':
			escapes := i > 0 && (q[i-1] == 'e' || q[i-1] == 'E') && (i < 2 || !isIdentByte(q[i-2]))
			j := skipQuoted(q, i, '\'', escapes)
			b.WriteString(q[i:j])
			i = j
		case c == '"':
			j := skipQuoted(q, i, '"', false)
			b.WriteString(q[i:j])
			i = j
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q)
			} else {
				j += i
			}
			b.WriteString(q[i:j])
			i = j
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			j := skipBlockComment(q, i)
			b.WriteString(q[i:j])
			i = j
		case c == '$':
			j := skipDollar(q, i)
			b.WriteString(q[i:j])
			i = j
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// skipQuoted returns the index after the literal that starts at q[start]. A
// doubled quote is an escaped quote; with backslashes, "\x" is skipped too.
// An unterminated literal extends to the end of the text.
func skipQuoted(q string, start int, quote byte, backslash bool) int {
	for i := start + 1; i < len(q); i++ {
		switch {
		case backslash && q[i] == '\\':
			i++
		case q[i] == quote:
			if i+1 < len(q) && q[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(q)
}

func skipBlockComment(q string, start int) int {
	depth := 0
	for i := start; i < len(q); i++ {
		if q[i] == '/' && i+1 < len(q) && q[i+1] == '*' {
			depth++
			i++
		} else if q[i] == '*' && i+1 < len(q) && q[i+1] == '/' {
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(q)
}

// skipDollar handles q[start]=='$': a $n placeholder, a dollar-quoted string, or
// a lone '$' (e.g. inside an identifier). It returns the index after the token.
func skipDollar(q string, start int) int {
	if start > 0 && isIdentByte(q[start-1]) {
		return start + 1
	}
	i := start + 1
	if i < len(q) && q[i] >= '0' && q[i] <= '9' {
		for i < len(q) && q[i] >= '0' && q[i] <= '9' {
			i++
		}
		return i
	}
	for i < len(q) && (q[i] == '_' || q[i] >= 0x80 || q[i] >= 'a' && q[i] <= 'z' || q[i] >= 'A' && q[i] <= 'Z' || i > start+1 && q[i] >= '0' && q[i] <= '9') {
		i++
	}
	if i >= len(q) || q[i] != '$' {
		return start + 1
	}
	delim := q[start : i+1]
	if end := strings.Index(q[i+1:], delim); end >= 0 {
		return i + 1 + end + len(delim)
	}
	return len(q)
}

// The helpers below produce dialect-specific SQL for the JSON text columns
// (articles.tags, articles.properties, revisions.snapshot). Column arguments are
// code constants such as "a.tags"; anything else panics.

func sqlIdent(s string) string {
	if s == "" {
		panic("empty SQL identifier")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			panic("unsafe SQL identifier " + strconv.Quote(s))
		}
	}
	return s
}

// jsonArrayHasSQL is a predicate with one "?": the JSON array in col holds that string.
func (d *database) jsonArrayHasSQL(col string) string {
	col = sqlIdent(col)
	if d.pg {
		return "EXISTS(SELECT 1 FROM jsonb_array_elements_text(aiwiki_json_array(" + col + ")) AS t(value) WHERE t.value=?)"
	}
	return "EXISTS(SELECT 1 FROM json_each(" + col + ") WHERE value=?)"
}

// jsonObjectHasSQL is a predicate with two "?": the JSON object in col has key=? and value=?.
func (d *database) jsonObjectHasSQL(col string) string {
	col = sqlIdent(col)
	if d.pg {
		return "EXISTS(SELECT 1 FROM jsonb_each_text(aiwiki_json_object(" + col + ")) AS p(key,value) WHERE p.key=? AND p.value=?)"
	}
	return "EXISTS(SELECT 1 FROM json_each(" + col + ") p WHERE p.key=? AND p.value=?)"
}

// jsonFieldSQL extracts a top-level field of the JSON object in col as text.
func (d *database) jsonFieldSQL(col, field string) string {
	col, field = sqlIdent(col), sqlIdent(field)
	if d.pg {
		return "(aiwiki_json_object(" + col + ")->>'" + field + "')"
	}
	return "json_extract(" + col + ",'$." + field + "')"
}

// distinctTagsSQL lists the distinct string tags of a space's live articles
// (one "?": space id), case-insensitively ordered.
func (d *database) distinctTagsSQL() string {
	if d.pg {
		return `SELECT value FROM (SELECT DISTINCT e.v #>> '{}' AS value FROM articles a, jsonb_array_elements(aiwiki_json_array(a.tags)) AS e(v) WHERE a.space_id=? AND a.deleted_at IS NULL AND jsonb_typeof(e.v)='string') t ORDER BY lower(value) COLLATE "C", value COLLATE "C"`
	}
	return `SELECT DISTINCT j.value FROM articles a, json_each(a.tags) j WHERE a.space_id=? AND a.deleted_at IS NULL AND j.type='text' ORDER BY j.value COLLATE NOCASE, j.value`
}

func (d *database) context() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}
func (t *transaction) context() context.Context {
	if t.ctx != nil {
		return t.ctx
	}
	return context.Background()
}

func (d *database) releaseForTransfer() {
	if d.release != nil {
		d.release()
	}
}
func (d *database) resumeAfterTransfer(ctx context.Context) (*database, func(), error) {
	if d.resume != nil {
		return d.resume(ctx)
	}
	return d, func() {}, nil
}
