package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTest(t *testing.T) *http.ServeMux {
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

func post(mux *http.ServeMux, path string, form url.Values) int {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func get(mux *http.ServeMux, path string) int {
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
		"/start/kidworld", "/restart/kidworld", "/delete/kidworld"} {
		if c := post(mux, p, nil); c != 403 {
			t.Fatalf("other user POST %s = %d, want 403", p, c)
		}
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
