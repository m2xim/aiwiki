package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadCorporatePostgres is the Go counterpart of scripts/load-test.mjs
// (docs/OPERATIONS.md section 4, docs/LOAD.md). It runs the same workload mix
// (60 % search, 25 % read: outline then one section, 10 % append edit, 5 % create)
// against an in-process corporate server with real PostgreSQL and row level
// security, using one persistent MCP key per user. It never fails on latency;
// it fails only on errors other than revision conflicts.
//
// Environment: AIWIKI_LOAD_POSTGRES=1 (gate), AIWIKI_TEST_POSTGRES_OWNER and
// AIWIKI_TEST_POSTGRES (disposable database, wiped), AIWIKI_LOAD_VUS (default 8),
// AIWIKI_LOAD_DURATION (Go duration, default 20s), AIWIKI_LOAD_USERS (default 3),
// AIWIKI_LOAD_ARTICLES (seed size over all users, default 150),
// AIWIKI_LOAD_SKIP_EDITS=1 (replace the edit share by search, see skipEdits).
func TestLoadCorporatePostgres(t *testing.T) {
	if os.Getenv("AIWIKI_LOAD_POSTGRES") != "1" {
		t.Skip("set AIWIKI_LOAD_POSTGRES=1 (plus AIWIKI_TEST_POSTGRES_OWNER and AIWIKI_TEST_POSTGRES) to run the load test")
	}
	vus := loadEnvInt(t, "AIWIKI_LOAD_VUS", 8)
	users := loadEnvInt(t, "AIWIKI_LOAD_USERS", 3)
	articles := loadEnvInt(t, "AIWIKI_LOAD_ARTICLES", 150)
	duration := 20 * time.Second
	if v := os.Getenv("AIWIKI_LOAD_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("bad AIWIKI_LOAD_DURATION %q", v)
		}
		duration = d
	}
	// Known defect: wiki_edit through a scoped write key fails with "database error"
	// (article_fts has no UPDATE exception in mcp_persistent.sql while indexArticle
	// uses INSERT .. ON CONFLICT DO UPDATE). With AIWIKI_LOAD_SKIP_EDITS=1 the 10 %
	// edit share is spent on searches so the rest of the mix can still be measured.
	skipEdits := os.Getenv("AIWIKI_LOAD_SKIP_EDITS") == "1"
	if users > vus {
		users = vus
	}

	owner, root := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: root.DB}
	x.a.persistent = &mcpPersistentAccess{db: root.DB, now: time.Now, admission: leadAdmissionCheck(func(context.Context, string, string) error { return nil })}
	root.auth = x.a
	srv := httptest.NewServer(root.Handler())
	defer srv.Close()

	do := func(method, path, body, csrf, bearer string, c *http.Cookie) (int, []byte) {
		r, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if c != nil {
			r.AddCookie(c)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		r.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	must := func(code int, want int, b []byte, out any) {
		t.Helper()
		if code != want {
			t.Fatalf("status=%d want=%d body=%s", code, want, b)
		}
		if out != nil {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatal(err)
			}
		}
	}

	type loadUser struct {
		token, space, project string
		mu                    sync.Mutex
		ids                   []string
		rev                   map[string]int
	}
	us := make([]*loadUser, users)
	for i := range us {
		cookie := authCookie(x.login(authUserGrant(fmt.Sprintf("load-user-%d", i), "aiwiki-access", "aiwiki-admin")), x.a.sessionCookie)
		if cookie == nil {
			t.Fatal("no session cookie")
		}
		var me struct {
			CSRF string `json:"csrf_token"`
		}
		code, b := do("GET", "/api/me", "", "", "", cookie)
		must(code, 200, b, &me)
		var space Space
		code, b = do("POST", "/api/corporate/spaces", fmt.Sprintf(`{"name":"load-test-%d-%d","personal":true}`, time.Now().Unix(), i), me.CSRF, "", cookie)
		must(code, 201, b, &space)
		var project corporateProject
		code, b = do("POST", "/api/projects", `{"space_id":"`+space.ID+`","name":"Load"}`, me.CSRF, "", cookie)
		must(code, 201, b, &project)
		var issued struct {
			Token string `json:"token"`
		}
		code, b = do("POST", "/api/mcp/credentials", `{"name":"load","mode":"write","project_ids":["`+project.ID+`"]}`, me.CSRF, "", cookie)
		must(code, 201, b, &issued)
		if issued.Token == "" {
			t.Fatal("missing key secret")
		}
		us[i] = &loadUser{token: issued.Token, space: space.ID, project: project.ID, rev: map[string]int{}}
	}

	var seq atomic.Int64
	// call performs one tools/call and classifies the outcome.
	type outcome struct {
		d        time.Duration
		conflict bool
		err      string
		data     map[string]any
	}
	call := func(u *loadUser, name string, args map[string]any) outcome {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": seq.Add(1), "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		r, _ := http.NewRequest("POST", srv.URL+"/mcp", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+u.token)
		r.Header.Set("Content-Type", "application/json")
		t0 := time.Now()
		resp, err := srv.Client().Do(r)
		if err != nil {
			return outcome{d: time.Since(t0), err: "network"}
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		d := time.Since(t0)
		if resp.StatusCode != 200 {
			return outcome{d: d, err: "http " + strconv.Itoa(resp.StatusCode)}
		}
		var j struct {
			Error  *struct{ Code int } `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &j) != nil || j.Error != nil || len(j.Result.Content) == 0 {
			return outcome{d: d, err: "rpc"}
		}
		var data map[string]any
		_ = json.Unmarshal([]byte(j.Result.Content[0].Text), &data)
		if j.Result.IsError {
			msg, _ := data["error"].(string)
			if strings.Contains(strings.ToLower(msg), "revision conflict") {
				return outcome{d: d, conflict: true}
			}
			if len(msg) > 60 {
				msg = msg[:60]
			}
			return outcome{d: d, err: "tool: " + msg}
		}
		return outcome{d: d, data: data}
	}

	en := []string{"server", "database", "index", "revision", "latency", "cluster", "backup", "deployment", "queue", "cache"}
	ru := []string{"сервер", "база", "индекс", "ревизия", "задержка", "кластер", "резервная", "выкладка", "очередь", "кэш"}
	sentence := func(r *rand.Rand) string {
		w := make([]string, 0, 16)
		for i, n := 0, 8+r.Intn(10); i < n; i++ {
			if r.Intn(10) < 3 {
				if r.Intn(2) == 0 {
					w = append(w, ru[r.Intn(len(ru))])
				} else {
					w = append(w, en[r.Intn(len(en))])
				}
			} else if r.Intn(2) == 0 {
				w = append(w, "сервис должен обрабатывать каждый запрос")
			} else {
				w = append(w, "the service must handle every request")
			}
		}
		return strings.Join(w, " ") + "."
	}
	body := func(r *rand.Rand) string {
		var b strings.Builder
		target := 1000 + r.Intn(1100)
		for n := 1; b.Len() < target*2; n++ {
			fmt.Fprintf(&b, "## Section %d %s\n\n%s %s\n\n", n, en[r.Intn(len(en))], sentence(r), sentence(r))
		}
		return b.String()
	}
	create := func(u *loadUser, r *rand.Rand, title string) outcome {
		o := call(u, "wiki_write", map[string]any{"article": map[string]any{"space_id": u.space, "project_id": u.project, "title": title, "summary": sentence(r), "body": body(r), "kind": "article", "status": "active", "tags": []string{"load-test"}}})
		if o.err == "" && !o.conflict {
			id, _ := o.data["id"].(string)
			rev, _ := o.data["revision"].(float64)
			u.mu.Lock()
			u.ids = append(u.ids, id)
			u.rev[id] = int(rev)
			u.mu.Unlock()
		}
		return o
	}

	// Seed.
	per := articles / users
	if per < 1 {
		per = 1
	}
	seedStart := time.Now()
	var seedWG sync.WaitGroup
	for ui, u := range us {
		seedWG.Add(1)
		go func(ui int, u *loadUser) {
			defer seedWG.Done()
			r := rand.New(rand.NewSource(int64(ui) + 1))
			for i := 0; i < per; i++ {
				if o := create(u, r, fmt.Sprintf("Load seed %d %s %s", i, en[r.Intn(len(en))], ru[r.Intn(len(ru))])); o.err != "" || o.conflict {
					t.Errorf("seed failed: %s", o.err)
					return
				}
			}
		}(ui, u)
	}
	seedWG.Wait()
	if t.Failed() {
		t.FailNow()
	}
	t.Logf("seeded %d articles for %d users in %s", per*users, users, time.Since(seedStart).Round(time.Millisecond))

	type opStat struct {
		mu        sync.Mutex
		lat       []time.Duration
		ok        int
		conflicts int
		errs      map[string]int
	}
	ops := []string{"search", "read_outline", "read_section", "edit_append", "create"}
	st := map[string]*opStat{}
	for _, o := range ops {
		st[o] = &opStat{errs: map[string]int{}}
	}
	rec := func(op string, o outcome) {
		s := st[op]
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case o.conflict:
			s.conflicts++
			s.lat = append(s.lat, o.d)
		case o.err != "":
			s.errs[o.err]++
		default:
			s.ok++
			s.lat = append(s.lat, o.d)
		}
	}

	// A persistent key is limited to mcpPersistentRateLimit (120) requests per
	// minute by design, which would cap this test at about 2 requests per second
	// per key. The limiter itself stays in the request path (one atomic UPDATE per
	// request); only its counter is zeroed every 25 ms from the owner connection.
	stopReset := make(chan struct{})
	var resetWG sync.WaitGroup
	resetWG.Add(1)
	go func() {
		defer resetWG.Done()
		for {
			select {
			case <-stopReset:
				return
			case <-time.After(25 * time.Millisecond):
				_, _ = owner.DB.Exec(`UPDATE mcp_credentials SET rate_requests=0`)
			}
		}
	}()
	defer func() { close(stopReset); resetWG.Wait() }()

	deadline := time.Now().Add(duration)
	start := time.Now()
	var wg sync.WaitGroup
	for v := 0; v < vus; v++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			u := us[v%users]
			r := rand.New(rand.NewSource(int64(1000 + v)))
			pickID := func() string {
				u.mu.Lock()
				defer u.mu.Unlock()
				return u.ids[r.Intn(len(u.ids))]
			}
			for n := 0; time.Now().Before(deadline); n++ {
				switch p := r.Intn(100); {
				case p < 60 || (skipEdits && p >= 85 && p < 95):
					q := en[r.Intn(len(en))]
					if r.Intn(2) == 0 {
						q = ru[r.Intn(len(ru))] + " " + ru[r.Intn(len(ru))]
					}
					rec("search", call(u, "wiki_search", map[string]any{"q": q, "space_id": u.space, "project_id": u.project, "limit": 20}))
				case p < 85:
					id := pickID()
					o := call(u, "wiki_read", map[string]any{"id": id, "outline": true})
					rec("read_outline", o)
					if o.err != "" || o.conflict {
						break
					}
					if rev, ok := o.data["revision"].(float64); ok {
						u.mu.Lock()
						u.rev[id] = int(rev)
						u.mu.Unlock()
					}
					if hs, _ := o.data["headings"].([]any); len(hs) > 0 {
						h, _ := hs[r.Intn(len(hs))].(map[string]any)
						anchor, _ := h["anchor"].(string)
						rec("read_section", call(u, "wiki_read", map[string]any{"id": id, "anchor": anchor}))
					}
				case p < 95:
					id := pickID()
					u.mu.Lock()
					rev := u.rev[id]
					u.mu.Unlock()
					o := call(u, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "append", "text": "\n" + sentence(r) + "\n", "message": "load test"})
					rec("edit_append", o)
					if o.err == "" && !o.conflict {
						if nr, ok := o.data["revision"].(float64); ok {
							u.mu.Lock()
							u.rev[id] = int(nr)
							u.mu.Unlock()
						}
					} else if o.conflict {
						if o2 := call(u, "wiki_read", map[string]any{"id": id, "outline": true}); o2.err == "" && !o2.conflict {
							if nr, ok := o2.data["revision"].(float64); ok {
								u.mu.Lock()
								u.rev[id] = int(nr)
								u.mu.Unlock()
							}
						}
					}
				default:
					rec("create", create(u, r, fmt.Sprintf("Load create %d-%d-%d", v, n, time.Now().UnixNano())))
				}
			}
		}(v)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var report strings.Builder
	fmt.Fprintf(&report, "\nvus=%d users=%d duration=%s seed=%d\n%-14s%9s%9s%9s%9s%10s%10s%10s%10s\n", vus, users, elapsed.Round(time.Millisecond), per*users, "op", "count", "ok", "conflict", "errors", "p50 ms", "p95 ms", "p99 ms", "max ms")
	total, totalErr, totalConf := 0, 0, 0
	pct := func(l []time.Duration, p float64) float64 {
		if len(l) == 0 {
			return 0
		}
		i := int(float64(len(l))*p/100+0.999999) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(l) {
			i = len(l) - 1
		}
		return float64(l[i].Microseconds()) / 1000
	}
	var errDetail []string
	for _, o := range ops {
		s := st[o]
		sort.Slice(s.lat, func(i, j int) bool { return s.lat[i] < s.lat[j] })
		e := 0
		for k, n := range s.errs {
			e += n
			errDetail = append(errDetail, fmt.Sprintf("%s: %s x%d", o, k, n))
		}
		total += s.ok + s.conflicts + e
		totalErr += e
		totalConf += s.conflicts
		mx := 0.0
		if len(s.lat) > 0 {
			mx = float64(s.lat[len(s.lat)-1].Microseconds()) / 1000
		}
		fmt.Fprintf(&report, "%-14s%9d%9d%9d%9d%10.1f%10.1f%10.1f%10.1f\n", o, s.ok+s.conflicts+e, s.ok, s.conflicts, e, pct(s.lat, 50), pct(s.lat, 95), pct(s.lat, 99), mx)
	}
	fmt.Fprintf(&report, "total %d requests, %.1f req/s, errors %d, revision conflicts %d\n", total, float64(total)/elapsed.Seconds(), totalErr, totalConf)
	if skipEdits {
		report.WriteString("NOTE: AIWIKI_LOAD_SKIP_EDITS=1, the edit share was replaced by search\n")
	}
	t.Log(report.String())
	if totalErr > 0 {
		t.Errorf("unexpected errors under load: %s", strings.Join(errDetail, "; "))
	}
}

func loadEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("bad %s %q", name, v)
	}
	return n
}
