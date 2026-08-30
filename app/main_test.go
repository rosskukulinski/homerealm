package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTest(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	dataDir = dir
	worldsFile = filepath.Join(dir, "worlds.json")
	composeFile = filepath.Join(dir, "docker-compose.yml")
	tsStateRoot = filepath.Join(dir, "_tailscale")
	os.MkdirAll(tsStateRoot, 0o755)
	orig := requester
	t.Cleanup(func() { requester = orig })
	return routes()
}

func as(login string, rl role) {
	requester = func(*http.Request) (string, role) { return login, rl }
}

func post(mux http.Handler, path string, form url.Values) int {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func get(mux http.Handler, path string) int {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code
}

func TestRoleEnforcement(t *testing.T) {
	mux := setupTest(t)

	// Readers can look but not touch.
	as("", roleReader)
	if c := get(mux, "/"); c != 200 {
		t.Fatalf("reader GET / = %d, want 200", c)
	}
	if c := get(mux, "/api/worlds"); c != 200 {
		t.Fatalf("reader GET /api/worlds = %d, want 200", c)
	}
	if c := post(mux, "/new", url.Values{"name": {"nope"}}); c != 403 {
		t.Fatalf("reader POST /new = %d, want 403", c)
	}

	// A tailnet user creates a world and owns it.
	as("kid@example.com", roleUser)
	if c := post(mux, "/new", url.Values{"name": {"kidworld"}}); c != 302 {
		t.Fatalf("user POST /new = %d, want 302", c)
	}
	ws := load()
	if len(ws) != 1 || ws[0].Owner != "kid@example.com" {
		t.Fatalf("owner not recorded: %+v", ws)
	}

	// Another user can read it but not manage it.
	as("cousin@example.com", roleUser)
	for _, p := range []string{"/settings/kidworld", "/stop/kidworld",
		"/start/kidworld", "/restart/kidworld", "/delete/kidworld",
		"/backup/kidworld", "/command/kidworld"} {
		if c := post(mux, p, nil); c != 403 {
			t.Fatalf("other user POST %s = %d, want 403", p, c)
		}
	}
	if c := get(mux, "/console/kidworld"); c != 403 {
		t.Fatalf("other user GET /console = %d, want 403", c)
	}
	if c := post(mux, "/clone/kidworld", url.Values{"newname": {"steal"}}); c != 403 {
		t.Fatalf("other user POST /clone = %d, want 403", c)
	}
	if c := post(mux, "/permission/kidworld",
		url.Values{"xuid": {"123456789"}, "level": {"operator"}}); c != 403 {
		t.Fatalf("other user POST /permission = %d, want 403", c)
	}

	// The owner manages their own world.
	as("kid@example.com", roleUser)
	if c := post(mux, "/stop/kidworld", nil); c != 302 {
		t.Fatalf("owner POST /stop = %d, want 302", c)
	}
	if c := post(mux, "/settings/kidworld", url.Values{"mode": {"creative"},
		"difficulty": {"peaceful"}, "default_permission": {"member"}}); c != 302 {
		t.Fatalf("owner POST /settings = %d, want 302", c)
	}

	// Admins manage anything, and their clones become theirs.
	as("ross@example.com", roleAdmin)
	if c := post(mux, "/clone/kidworld", url.Values{"newname": {"kidworld-2"}}); c != 302 {
		t.Fatalf("admin POST /clone = %d, want 302", c)
	}
	if wd := findWorld(load(), "kidworld-2"); wd == nil || wd.Owner != "ross@example.com" {
		t.Fatalf("clone owner wrong: %+v", wd)
	}
	if c := post(mux, "/delete/kidworld", nil); c != 302 {
		t.Fatalf("admin POST /delete = %d, want 302", c)
	}
	if findWorld(load(), "kidworld") != nil {
		t.Fatal("world not deleted")
	}
}

func TestOwnerlessWorldsAreAdminOnly(t *testing.T) {
	setupTest(t)
	w := world{Name: "legacy"} // e.g. created before ownership existed
	if canManage("kid@example.com", roleUser, &w) {
		t.Fatal("user may not manage an ownerless world")
	}
	if !canManage("anyone@example.com", roleAdmin, &w) {
		t.Fatal("admin must manage an ownerless world")
	}
	owned := world{Name: "w", Owner: "Kid@Example.com"}
	if !canManage("kid@example.com", roleUser, &owned) {
		t.Fatal("owner match must be case-insensitive")
	}
	if canManage("kid@example.com", roleReader, &owned) {
		t.Fatal("reader may not manage even their own world")
	}
}

func TestParseAdmins(t *testing.T) {
	got := parseAdmins(" Ross@github, ,mom@example.com ")
	if len(got) != 2 || got[0] != "ross@github" || got[1] != "mom@example.com" {
		t.Fatalf("parseAdmins = %v", got)
	}
	if len(parseAdmins("")) != 0 {
		t.Fatal("empty PANEL_ADMINS must parse to no admins")
	}
}

func TestWorldDetailPage(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	if c := post(mux, "/new", url.Values{"name": {"pageworld"}}); c != 302 {
		t.Fatalf("create = %d", c)
	}
	as("", roleReader)
	if c := get(mux, "/world/pageworld"); c != 200 {
		t.Fatalf("reader GET /world/pageworld = %d, want 200", c)
	}
	if c := get(mux, "/world/nope"); c != 404 {
		t.Fatalf("GET /world/nope = %d, want 404", c)
	}
}

func TestBackup(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	if c := post(mux, "/new", url.Values{"name": {"bkworld"}}); c != 302 {
		t.Fatalf("create = %d", c)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "bkworld", "marker.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if c := post(mux, "/backup/bkworld", nil); c != 302 {
		t.Fatalf("backup POST = %d, want 302", c)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "_backups", "bkworld"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 backup file, got %v (err=%v)", entries, err)
	}
	file := entries[0].Name()
	if !backupFileRe.MatchString(file) {
		t.Fatalf("backup file name = %q", file)
	}

	// Owner can download and delete; a non-owner can't reach either.
	if c := get(mux, "/backup/bkworld/"+file); c != 200 {
		t.Fatalf("download = %d, want 200", c)
	}
	as("cousin@example.com", roleUser)
	if c := get(mux, "/backup/bkworld/"+file); c != 403 {
		t.Fatalf("other user download = %d, want 403", c)
	}
	as("kid@example.com", roleUser)
	if c := post(mux, "/backup/bkworld/"+file+"/delete", nil); c != 302 {
		t.Fatalf("delete = %d, want 302", c)
	}
	if entries, _ := os.ReadDir(filepath.Join(dataDir, "_backups", "bkworld")); len(entries) != 0 {
		t.Fatalf("backup file not deleted: %v", entries)
	}
}

func TestPruneBackups(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"20260101-000000.tar.gz", "20260102-000000.tar.gz", "20260103-000000.tar.gz"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneBackups(dir, 2)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("want 2 files after prune, got %v (err=%v)", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "20260101-000000.tar.gz")); !os.IsNotExist(err) {
		t.Fatal("oldest backup should have been pruned")
	}
}

func TestCommand(t *testing.T) {
	mux := setupTest(t)
	origSend := sendCommand
	t.Cleanup(func() { sendCommand = origSend })
	var gotName string
	var gotArgs []string
	sendCommand = func(name string, args []string) error {
		gotName, gotArgs = name, args
		return nil
	}

	as("kid@example.com", roleUser)
	post(mux, "/new", url.Values{"name": {"cmdworld"}})

	if c := post(mux, "/command/cmdworld", url.Values{"cmd": {""}}); c != 400 {
		t.Fatalf("empty command = %d, want 400", c)
	}
	if c := post(mux, "/command/cmdworld", url.Values{"cmd": {"say hello world"}}); c != 204 {
		t.Fatalf("owner command = %d, want 204", c)
	}
	if gotName != "cmdworld" || len(gotArgs) != 3 || gotArgs[0] != "say" {
		t.Fatalf("sendCommand called with (%q, %v)", gotName, gotArgs)
	}

	sendCommand = func(string, []string) error { return errors.New("no such container") }
	if c := post(mux, "/command/cmdworld", url.Values{"cmd": {"say hi"}}); c != 502 {
		t.Fatalf("failed send = %d, want 502", c)
	}
}

func TestWorldDetailOwnerView(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	post(mux, "/new", url.Values{"name": {"detailworld"}})

	req := httptest.NewRequest("GET", "/world/detailworld", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("owner GET /world/detailworld = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Freshly created worlds are stopped in this docker-less test env, so
	// the live console box is hidden in favor of its placeholder message —
	// but the script block (which no-ops without #console-log) still renders.
	for _, want := range []string{
		`Start the world to use the live console.`, `new EventSource(`,
		`action="/backup/detailworld"`, `Back up now`, `No backups yet.`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("owner view missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `id="console-log"`) {
		t.Fatal("stopped world should not render the live console box")
	}
}

func TestTailscalePeersWithoutLocalClient(t *testing.T) {
	// localClient is only set once tsnet actually comes up; before/without
	// that (as in every test, and briefly on real startup) this must
	// degrade to "no known IPs" rather than panic on a nil client.
	if ips := tailscalePeers(context.Background()); len(ips) != 0 {
		t.Fatalf("want empty map with no localClient, got %v", ips)
	}
}

func TestMintAuthKey(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			r.ParseForm()
			if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret" {
				http.Error(w, "bad creds", 401)
				return
			}
			w.Write([]byte(`{"access_token":"tok123"}`))
		case "/api/v2/tailnet/-/keys":
			gotAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.Write([]byte(`{"key":"tskey-auth-minted"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	orig := struct{ base, id, secret, tag string }{tsAPIBase, tsOAuthID, tsOAuthSecret, tsTag}
	t.Cleanup(func() {
		tsAPIBase, tsOAuthID, tsOAuthSecret, tsTag = orig.base, orig.id, orig.secret, orig.tag
	})
	tsAPIBase, tsOAuthID, tsOAuthSecret, tsTag = srv.URL, "cid", "csecret", "tag:test-worlds"

	key, err := mintAuthKey("myworld")
	if err != nil || key != "tskey-auth-minted" {
		t.Fatalf("mintAuthKey = %q, %v", key, err)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("keys call auth = %q", gotAuth)
	}
	for _, want := range []string{`"tag:test-worlds"`, `"preauthorized":true`, `"reusable":false`, `"homerealm world myworld"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("keys request body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestMintAuthKeyNotConfigured(t *testing.T) {
	orig := struct{ id, secret string }{tsOAuthID, tsOAuthSecret}
	t.Cleanup(func() { tsOAuthID, tsOAuthSecret = orig.id, orig.secret })
	tsOAuthID, tsOAuthSecret = "", ""
	if key, err := mintAuthKey("w"); key != "" || err != nil {
		t.Fatalf("unconfigured mint = %q, %v; want empty and no error", key, err)
	}
}

func TestVersionParsing(t *testing.T) {
	logLine := []byte("[2026-08-29 17:47:00:405 INFO] Version: 1.26.45.01")
	if m := versionRe.FindSubmatch(logLine); m == nil || string(m[1]) != "1.26.45.01" {
		t.Fatalf("versionRe on log line = %v", m)
	}
	for _, c := range []struct {
		a, b string
		eq   bool
	}{
		{"1.26.44.3", "1.26.44.03", true}, // zero-padding differs between logs and zip names
		{"1.26.45", "1.26.45.01", true},   // shorter form must not read as an update
		{"1.26.44.3", "1.26.45.01", false},
		{"1.26.44", "1.27.44", false},
	} {
		if got := verEqual(c.a, c.b); got != c.eq {
			t.Fatalf("verEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.eq)
		}
	}
}

func TestLatestBedrock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"result":{"links":[
			{"downloadType":"serverBedrockWindows","downloadUrl":"https://x/bedrock-server-1.26.45.01.zip"},
			{"downloadType":"serverBedrockLinux","downloadUrl":"https://x/bedrock-server-1.26.45.01.zip"}]}}`))
	}))
	defer srv.Close()
	orig := struct {
		url string
		val string
		at  time.Time
	}{bedrockLinksURL, bedrockLatestVal, bedrockLatestAt}
	t.Cleanup(func() { bedrockLinksURL, bedrockLatestVal, bedrockLatestAt = orig.url, orig.val, orig.at })
	bedrockLinksURL, bedrockLatestVal, bedrockLatestAt = srv.URL, "", time.Time{}

	if got := latestBedrock(); got != "1.26.45.01" {
		t.Fatalf("latestBedrock = %q", got)
	}
	// updateAvailable: equal (modulo padding) means no update; older means update.
	if got := updateAvailable("1.26.45.1"); got != "" {
		t.Fatalf("current version flagged for update: %q", got)
	}
	if got := updateAvailable("1.26.44.3"); got != "1.26.45.01" {
		t.Fatalf("stale version not flagged: %q", got)
	}
	if got := updateAvailable(""); got != "" {
		t.Fatalf("unknown running version must not flag an update: %q", got)
	}
}

func TestUpdateRouteAuth(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	post(mux, "/new", url.Values{"name": {"upworld"}})
	as("cousin@example.com", roleUser)
	if c := post(mux, "/update/upworld", nil); c != 403 {
		t.Fatalf("other user POST /update = %d, want 403", c)
	}
	as("kid@example.com", roleUser)
	if c := post(mux, "/update/upworld", nil); c != 302 {
		t.Fatalf("owner POST /update = %d, want 302", c)
	}
}

func TestBuildLabel(t *testing.T) {
	orig := struct{ commit, branch, pr string }{buildCommit, buildBranch, buildPR}
	t.Cleanup(func() { buildCommit, buildBranch, buildPR = orig.commit, orig.branch, orig.pr })

	buildCommit, buildBranch, buildPR = "", "", ""
	if got := buildLabel(); got != "" {
		t.Fatalf("no build info: got %q, want empty", got)
	}

	buildCommit, buildBranch, buildPR = "b76c2500f7a64d39bba101aa43441c76d02f7593", "main", ""
	got := string(buildLabel())
	if !strings.Contains(got, "b76c250") || strings.Contains(got, "main@") {
		t.Fatalf("main branch: got %q", got)
	}
	if !strings.Contains(got, `href="https://github.com/rosskukulinski/homerealm/commit/b76c2500f7a64d39bba101aa43441c76d02f7593"`) {
		t.Fatalf("commit link should use the full sha: got %q", got)
	}

	buildCommit, buildBranch, buildPR = "b76c2500f7a64d39bba101aa43441c76d02f7593", "console-stats-backups", "2"
	got = string(buildLabel())
	for _, want := range []string{"console-stats-backups@b76c250", "PR #2",
		`href="https://github.com/rosskukulinski/homerealm/pull/2"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("feature branch + PR: got %q, missing %q", got, want)
		}
	}
}

func TestWorldDetailLANAddress(t *testing.T) {
	mux := setupTest(t)
	discovery = true
	t.Cleanup(func() { discovery = false })
	as("kid@example.com", roleUser)
	post(mux, "/new", url.Values{"name": {"lanworld"}})

	req := httptest.NewRequest("GET", "/world/lanworld", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "LAN &lt;") {
		t.Fatalf("HomeAuto world should not show the generic LAN placeholder:\n%s", body)
	}
	ws := load()
	if ws[0].IP == "" {
		t.Fatal("expected an allocated macvlan IP")
	}
	if !strings.Contains(body, `data-copy="`+ws[0].IP+`"`) {
		t.Fatalf("expected a copyable real LAN address for %s, got:\n%s", ws[0].IP, body)
	}
}

func TestBackRedirectValidation(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	post(mux, "/new", url.Values{"name": {"redir"}})

	req := httptest.NewRequest("POST", "/stop/redir",
		strings.NewReader(url.Values{"back": {"/world/redir"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code != 302 || loc != "/world/redir" {
		t.Fatalf("back to detail: code=%d loc=%q", rec.Code, loc)
	}

	for _, bad := range []string{"https://evil.example", "//evil.example", "/world/../etc", "/other"} {
		req := httptest.NewRequest("POST", "/stop/redir",
			strings.NewReader(url.Values{"back": {bad}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("back=%q redirected to %q, want /", bad, loc)
		}
	}
}

func TestCSRF(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	if c := post(mux, "/new", url.Values{"name": {"csrfworld"}}); c != 302 {
		t.Fatalf("same-origin create = %d, want 302", c)
	}

	// A browser tags a request forged from another site as cross-site; even
	// though the owner's identity is in scope, it must be rejected before it
	// can act — otherwise ambient Tailscale auth makes the browser a deputy.
	post := func(setHdr func(*http.Request)) int {
		req := httptest.NewRequest("POST", "/delete/csrfworld", nil)
		setHdr(req)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); c != 403 {
		t.Fatalf("cross-site POST = %d, want 403", c)
	}
	if c := post(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); c != 403 {
		t.Fatalf("cross-origin POST = %d, want 403", c)
	}
	if findWorld(load(), "csrfworld") == nil {
		t.Fatal("a blocked cross-site request must not have deleted the world")
	}

	// Same-origin requests (from the panel's own pages/JS) still go through,
	// whether tagged by Sec-Fetch-Site or by a matching Origin.
	if c := post(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }); c != 302 {
		t.Fatalf("same-origin POST = %d, want 302", c)
	}
	as("kid@example.com", roleUser)
	if c := post(func(r *http.Request) { r.Header.Set("Origin", "http://example.com") }); c != 404 {
		// world already deleted above; 404 (not 403) proves CSRF let it through
		t.Fatalf("matching-Origin POST = %d, want 404 (passed CSRF, world gone)", c)
	}
}

func TestAPIWorldsFieldGating(t *testing.T) {
	mux := setupTest(t)
	as("kid@example.com", roleUser)
	post(mux, "/new", url.Values{"name": {"apiworld"}, "seed": {"sup3rseed"}})

	body := func() string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/worlds", nil))
		return rec.Body.String()
	}

	// Anonymous reader (LAN visitor): no owner identity, no seed, no live ops.
	as("", roleReader)
	anon := body()
	for _, leak := range []string{`"owner"`, `"seed"`, `"cpu"`, `"mem"`, `"warn"`, "kid@example.com", "sup3rseed"} {
		if strings.Contains(anon, leak) {
			t.Fatalf("reader /api/worlds leaked %q:\n%s", leak, anon)
		}
	}
	for _, want := range []string{`"name":"apiworld"`, `"status"`, `"mode"`, `"port"`} {
		if !strings.Contains(anon, want) {
			t.Fatalf("reader /api/worlds missing %q:\n%s", want, anon)
		}
	}

	// Signed-in tailnet user: owner is surfaced; the seed still never is.
	as("kid@example.com", roleUser)
	authed := body()
	if !strings.Contains(authed, `"owner":"kid@example.com"`) {
		t.Fatalf("signed-in /api/worlds missing owner:\n%s", authed)
	}
	if strings.Contains(authed, `"seed"`) || strings.Contains(authed, "sup3rseed") {
		t.Fatalf("seed must never be exposed via the API:\n%s", authed)
	}
}
