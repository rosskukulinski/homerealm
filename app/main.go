// homerealm — a self-hosted, Realm-style panel for Minecraft Bedrock worlds,
// Tailscale-native.
//
// The panel is a tsnet application: it joins your tailnet as its own node
// (no tailscaled needed on the host) and serves HTTP/HTTPS there directly.
// Each world runs as its own itzg/minecraft-bedrock-server container wrapped
// in a tailscale/tailscale sidecar, so every world is its own tailnet node
// (mc-<name>) answering on the default Bedrock port.
//
// Configuration is via environment variables (see README / .env.example).
// There is no login of its own: requests are authorized by Tailscale identity
// (WhoIs) into three tiers — admins (PANEL_ADMINS) manage everything, other
// tailnet users create worlds and manage their own, everyone else (LAN
// listener, tagged nodes) is read-only.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

const appVersion = "0.2.0"

// ---------- configuration ----------
var (
	dataDir  = getenv("DATA_DIR", "/data") // inside this container
	hostData = getenv("HOST_DATA_DIR", "/volume1/docker/homerealm")
	puid     = getenv("PUID", "1000")
	pgid     = getenv("PGID", "100")

	panelPort = getenvInt("PANEL_PORT", 8090) // LAN listener (optional)
	panelLAN  = getenvBool("PANEL_LISTEN_LAN", true)
	basePort  = getenvInt("BASE_PORT", 19132) // first world's LAN UDP port

	tsHostname     = getenv("TS_HOSTNAME", "homerealm") // panel's tailnet name
	tsAuthKey      = os.Getenv("TS_AUTHKEY")
	bedrockImage   = getenv("BEDROCK_IMAGE", "itzg/minecraft-bedrock-server:latest")
	tailscaleImage = getenv("TAILSCALE_IMAGE", "tailscale/tailscale:latest")

	// Optional LAN auto-discovery (macvlan): each world also gets its own LAN
	// IP on the default Bedrock port so consoles/tablets without Tailscale
	// list it under "LAN Games".
	discovery      = getenvBool("DISCOVERY_ENABLED", false)
	macvlanNetwork = getenv("MACVLAN_NETWORK", "mcnet")
	worldIPPrefix  = getenv("WORLD_IP_PREFIX", "192.168.1.")
	worldIPFirst   = getenvInt("WORLD_IP_FIRST", 250)
	worldIPLast    = getenvInt("WORLD_IP_LAST", 254)
)

const project = "homerealm-worlds" // compose project name

var (
	worldsFile  = filepath.Join(dataDir, "worlds.json")
	composeFile = filepath.Join(dataDir, "docker-compose.yml")
	tsStateRoot = filepath.Join(dataDir, "_tailscale")
)

var (
	diffs = []string{"peaceful", "easy", "normal", "hard"}
	modes = []string{"survival", "creative", "adventure"}
	perms = []string{"visitor", "member", "operator"}

	nameRe = regexp.MustCompile(`^[a-z0-9-]{1,20}$`)
	xuidRe = regexp.MustCompile(`^\d{5,20}$`)
)

type preset struct {
	Key, Label, Icon, Mode, Difficulty string
	Cheats                             bool
}

var presets = []preset{
	{"survival-easy", "Survival (easy)", "⚔️", "survival", "easy", false},
	{"survival-peaceful", "Peaceful survival", "🕊️", "survival", "peaceful", false},
	{"survival-hard", "Hard survival", "💀", "survival", "hard", false},
	{"creative", "Creative sandbox", "🪄", "creative", "peaceful", true},
	{"adventure", "Adventure", "🧭", "adventure", "normal", false},
}

func modeIcon(m string) string {
	switch m {
	case "survival":
		return "⚔️"
	case "creative":
		return "🪄"
	case "adventure":
		return "🧭"
	}
	return "🌍"
}

func diffIcon(d string) string {
	switch d {
	case "peaceful":
		return "🕊️"
	case "easy":
		return "🌤️"
	case "normal":
		return "⛅"
	case "hard":
		return "💀"
	}
	return ""
}

// ---------- model ----------
type world struct {
	Name              string `json:"name"`
	Port              int    `json:"port"`
	Mode              string `json:"mode"`
	IP                string `json:"ip,omitempty"`    // macvlan LAN IP (discovery)
	Owner             string `json:"owner,omitempty"` // tailnet login of the creator
	Difficulty        string `json:"difficulty"`
	Cheats            bool   `json:"cheats"`
	MaxPlayers        int    `json:"max_players"`
	ViewDistance      int    `json:"view_distance"`
	Seed              string `json:"seed"`
	LevelType         string `json:"level_type"`
	DefaultPermission string `json:"default_permission"`
}

func defaultWorld(name string, port int, ip, mode string) world {
	return world{Name: name, Port: port, Mode: mode, IP: ip,
		Difficulty: "easy", MaxPlayers: 10, ViewDistance: 10,
		LevelType: "DEFAULT", DefaultPermission: "member"}
}

var mu sync.Mutex // serializes mutations of worlds.json + compose state

func load() []world {
	b, err := os.ReadFile(worldsFile)
	if err != nil {
		return nil
	}
	var ws []world
	if err := json.Unmarshal(b, &ws); err != nil {
		log.Printf("worlds.json unreadable: %v", err)
		return nil
	}
	return ws
}

func save(ws []world) {
	b, _ := json.MarshalIndent(ws, "", " ")
	if err := os.WriteFile(worldsFile, b, 0o644); err != nil {
		log.Printf("save worlds.json: %v", err)
	}
}

// worldStatus reports the container state, refined for running worlds into
// "starting" until Bedrock has logged readiness — the container is up ~30s
// before anyone can actually join.
func worldStatus(name string) string {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{.State.Status}}|{{.State.StartedAt}}", "mc-"+name).Output()
	if err != nil {
		return "stopped"
	}
	st, startedAt, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	if st == "running" && startedAt != "" {
		logs, _ := exec.Command("docker", "logs", "--since", startedAt, "mc-"+name).CombinedOutput()
		if !bytes.Contains(logs, []byte("Server started")) {
			return "starting"
		}
	}
	return st
}

// regen rewrites the generated compose file: per world, a tailscale/tailscale
// sidecar (the world's tailnet node, mc-<name>) plus the Bedrock server
// sharing its network namespace.
func regen() {
	var b strings.Builder
	b.WriteString("# GENERATED by homerealm — edit via the panel or CLI, not by hand.\nservices:\n")
	for _, w := range load() {
		env := [][2]string{
			{"EULA", "TRUE"}, {"VERSION", "LATEST"}, {"SERVER_NAME", w.Name},
			{"GAMEMODE", w.Mode}, {"DIFFICULTY", w.Difficulty},
			{"LEVEL_NAME", w.Name}, {"ALLOW_CHEATS", strconv.FormatBool(w.Cheats)},
			{"ALLOW_LIST", "false"}, {"MAX_PLAYERS", strconv.Itoa(w.MaxPlayers)},
			{"VIEW_DISTANCE", strconv.Itoa(w.ViewDistance)},
			{"DEFAULT_PLAYER_PERMISSION_LEVEL", w.DefaultPermission},
		}
		if w.Seed != "" {
			env = append(env, [2]string{"LEVEL_SEED", w.Seed})
		}
		if w.LevelType != "" && w.LevelType != "DEFAULT" {
			env = append(env, [2]string{"LEVEL_TYPE", w.LevelType})
		}
		var envs []string
		for _, kv := range env {
			envs = append(envs, fmt.Sprintf("%s: %q", kv[0], fmt.Sprint(kv[1])))
		}
		net := ""
		if discovery && w.IP != "" {
			net = fmt.Sprintf("\n    networks: { default: {}, %s: { ipv4_address: %q } }",
				macvlanNetwork, w.IP)
		}
		fmt.Fprintf(&b, `  ts-%[1]s:
    image: %[2]s
    container_name: ts-%[1]s
    hostname: mc-%[1]s
    restart: unless-stopped
    environment: { TS_AUTHKEY: "${TS_AUTHKEY}", TS_STATE_DIR: "/var/lib/tailscale", TS_USERSPACE: "false", TS_AUTH_ONCE: "true" }
    devices: ["/dev/net/tun"]
    cap_add: ["NET_ADMIN"]
    volumes: ["%[3]s/_tailscale/%[1]s:/var/lib/tailscale"]
    ports: ["%[4]d:19132/udp"]%[5]s
  mc-%[1]s:
    image: %[6]s
    container_name: mc-%[1]s
    restart: unless-stopped
    network_mode: "service:ts-%[1]s"
    depends_on: [ts-%[1]s]
    environment: { %[7]s }
    volumes: ["%[3]s/%[1]s:/data"]
`, w.Name, tailscaleImage, hostData, w.Port, net, bedrockImage, strings.Join(envs, ", "))
	}
	if discovery {
		fmt.Fprintf(&b, "networks:\n  %s: { external: true }\n", macvlanNetwork)
	}
	if err := os.WriteFile(composeFile, []byte(b.String()), 0o644); err != nil {
		log.Printf("write compose: %v", err)
	}
}

func compose(args ...string) {
	cmd := exec.Command("docker", append([]string{"compose", "-p", project, "-f", composeFile}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s := string(out)
		if len(s) > 500 {
			s = s[len(s)-500:]
		}
		log.Printf("compose %v failed: %s", args, s)
	}
}

func docker(args ...string) {
	if err := exec.Command("docker", args...).Run(); err != nil {
		log.Printf("docker %v: %v", args, err)
	}
}

// alloc picks the next free LAN port and (if discovery) LAN IP.
func alloc(ws []world) (port int, ip string, ok bool) {
	port = basePort - 1
	for _, w := range ws {
		if w.Port > port {
			port = w.Port
		}
	}
	port++
	if discovery {
		octet := worldIPFirst - 1
		for _, w := range ws {
			if w.IP == "" {
				continue
			}
			if i := strings.LastIndex(w.IP, "."); i >= 0 {
				if n, err := strconv.Atoi(w.IP[i+1:]); err == nil && n > octet {
					octet = n
				}
			}
		}
		octet++
		if octet > worldIPLast {
			return 0, "", false
		}
		ip = worldIPPrefix + strconv.Itoa(octet)
	}
	return port, ip, true
}

func own(path string) {
	exec.Command("chown", "-R", puid+":"+pgid, path).Run()
	exec.Command("chmod", "-R", "u+rwX,g+rwX", path).Run()
}

var playerRe = regexp.MustCompile(`Player connected: ([^,]+), xuid: (\d+)`)

// seenPlayers merges players from container logs into a persistent per-world roster.
func seenPlayers(name string) map[string]string {
	rosterFile := filepath.Join(dataDir, name, "players_seen.json")
	roster := map[string]string{}
	if b, err := os.ReadFile(rosterFile); err == nil {
		json.Unmarshal(b, &roster)
	}
	logs, _ := exec.Command("docker", "logs", "mc-"+name).Output()
	for _, m := range playerRe.FindAllStringSubmatch(string(logs), -1) {
		roster[m[2]] = strings.TrimSpace(m[1])
	}
	if b, err := json.Marshal(roster); err == nil {
		os.WriteFile(rosterFile, b, 0o644)
	}
	return roster
}

func permissions(name string) map[string]string {
	var entries []struct{ Permission, Xuid string }
	b, err := os.ReadFile(filepath.Join(dataDir, name, "permissions.json"))
	if err != nil || json.Unmarshal(b, &entries) != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, e := range entries {
		out[e.Xuid] = e.Permission
	}
	return out
}

func setPermission(name, xuid, level string) {
	ps := permissions(name)
	if level == "member" {
		delete(ps, xuid)
	} else {
		ps[xuid] = level
	}
	type entry struct {
		Permission string `json:"permission"`
		Xuid       string `json:"xuid"`
	}
	var entries []entry
	for k, v := range ps {
		entries = append(entries, entry{v, k})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Xuid < entries[j].Xuid })
	if entries == nil {
		entries = []entry{}
	}
	b, _ := json.MarshalIndent(entries, "", " ")
	os.WriteFile(filepath.Join(dataDir, name, "permissions.json"), b, 0o644)
}

// ---------- HTML ----------

//go:embed base.tmpl list.tmpl world.tmpl
var tmplFS embed.FS

var tmplFuncs = template.FuncMap{
	"opts": func(values []string, current string) template.HTML {
		var b strings.Builder
		for _, v := range values {
			sel := ""
			if v == current {
				sel = " selected"
			}
			fmt.Fprintf(&b, "<option%s>%s</option>", sel, template.HTMLEscapeString(v))
		}
		return template.HTML(b.String())
	},
	"initial": func(s string) string {
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
}

var (
	listPage  = template.Must(template.New("base.tmpl").Funcs(tmplFuncs).ParseFS(tmplFS, "base.tmpl", "list.tmpl"))
	worldPage = template.Must(template.New("base.tmpl").Funcs(tmplFuncs).ParseFS(tmplFS, "base.tmpl", "world.tmpl"))
)

type playerView struct{ Xuid, Tag, Level string }

type worldView struct {
	world
	Status             string
	CheatsStr          string
	Tailnet            string
	ModeIcon, DiffIcon string
	Strip              template.CSS // per-world terrain banner gradient
	HomeAuto           bool         // discovery: appears under LAN Games
	Players            []playerView
	CanManage          bool
}

type section struct {
	Title  string
	Worlds []worldView
}

type pageView struct {
	Sections               []section
	HasWorlds              bool
	Presets                []preset
	DiscoveryNote, Version string
	Identity, Role         string
	CanCreate, IsReader    bool
}

// worldPageView backs the per-world detail page (/world/<name>).
type worldPageView struct {
	worldView
	Modes, Diffs, Perms, Bools []string
	Version, Identity, Role    string
	Back                       string
}

func buildWorldView(wd world, login string, rl role) worldView {
	return worldView{
		world: wd, Status: worldStatus(wd.Name),
		CheatsStr: strconv.FormatBool(wd.Cheats),
		Tailnet:   "mc-" + wd.Name,
		ModeIcon:  modeIcon(wd.Mode), DiffIcon: diffIcon(wd.Difficulty),
		Strip:     terrain(wd.Name),
		HomeAuto:  discovery && wd.IP != "",
		CanManage: canManage(login, rl, &wd)}
}

// terrain renders a deterministic strip of "blocks" from the world's name, so
// every card gets a recognizable banner without any image assets.
func terrain(name string) template.CSS {
	pal := []string{"#4c9a2e", "#57b13a", "#3a7a21", "#8a5a32", "#75492a",
		"#8e8e85", "#a5a59b", "#d9c98e", "#5aa9d6"}
	h := fnv.New32a()
	h.Write([]byte(name))
	r := h.Sum32()
	const n = 14
	stops := make([]string, 0, n)
	for i := 0; i < n; i++ {
		r = r*1664525 + 1013904223 // LCG walk from the name's hash
		c := pal[r%uint32(len(pal))]
		stops = append(stops, fmt.Sprintf("%s %.2f%% %.2f%%", c,
			float64(i)*100/n, float64(i+1)*100/n))
	}
	return template.CSS("linear-gradient(90deg," + strings.Join(stops, ",") + ")")
}

var localClient *local.Client // set once tsnet is up; nil until then

// ---------- auth ----------
// Three tiers, resolved per request from Tailscale identity (WhoIs):
//
//	admin  — logins in PANEL_ADMINS (or any tailnet user if PANEL_ADMINS is
//	         empty): can do anything
//	user   — any other tailnet user: create worlds, manage/delete their own,
//	         read the rest
//	reader — everyone else (LAN listener, tagged nodes, unknown): read only
type role int

const (
	roleReader role = iota
	roleUser
	roleAdmin
)

func (r role) String() string {
	switch r {
	case roleAdmin:
		return "admin"
	case roleUser:
		return "user"
	}
	return "read-only"
}

var admins = parseAdmins(os.Getenv("PANEL_ADMINS"))

func parseAdmins(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// requester resolves who is asking and what they may do. A var so tests can
// stub identities without a live tailnet.
var requester = func(r *http.Request) (login string, rl role) {
	if localClient == nil {
		return "", roleReader
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	who, err := localClient.WhoIs(ctx, r.RemoteAddr)
	if err != nil || who.UserProfile == nil ||
		(who.Node != nil && len(who.Node.Tags) > 0) {
		return "", roleReader // LAN visitor, tagged node, or lookup failed
	}
	login = who.UserProfile.LoginName
	if len(admins) == 0 || contains(admins, strings.ToLower(login)) {
		return login, roleAdmin
	}
	return login, roleUser
}

// canManage: admins manage everything; users manage the worlds they own.
// Ownerless worlds (created before ownership existed) are admin-only.
func canManage(login string, rl role, w *world) bool {
	return rl == roleAdmin ||
		(rl == roleUser && w.Owner != "" && strings.EqualFold(w.Owner, login))
}

// authWorld guards a mutating route on one world: 404 if it doesn't exist,
// 403 if the requester may not manage it. Returns nil on failure (response
// already written).
func authWorld(w http.ResponseWriter, r *http.Request, ws []world, name string) *world {
	wd := findWorld(ws, name)
	if wd == nil {
		http.Error(w, "no such world", 404)
		return nil
	}
	if login, rl := requester(r); !canManage(login, rl, wd) {
		http.Error(w, "forbidden — not your world (owner: "+orDash(wd.Owner)+")", 403)
		return nil
	}
	return wd
}

func orDash(s string) string {
	if s == "" {
		return "unowned"
	}
	return s
}

func playersOf(name string) []playerView {
	roster, ps := seenPlayers(name), permissions(name)
	var out []playerView
	for xuid, tag := range roster {
		level := ps[xuid]
		if level == "" {
			level = "member"
		}
		out = append(out, playerView{Xuid: xuid, Tag: tag, Level: level})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Tag) < strings.ToLower(out[j].Tag)
	})
	return out
}

func index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	login, rl := requester(r)
	view := pageView{Presets: presets, Version: appVersion,
		Identity: login, Role: rl.String(),
		CanCreate: rl >= roleUser, IsReader: rl == roleReader}
	var mine, others []worldView
	for _, wd := range load() {
		wv := buildWorldView(wd, login, rl)
		if login != "" && strings.EqualFold(wd.Owner, login) {
			mine = append(mine, wv)
		} else {
			others = append(others, wv)
		}
	}
	view.HasWorlds = len(mine)+len(others) > 0
	if len(mine) > 0 && len(others) > 0 {
		view.Sections = []section{{"Your worlds", mine}, {"Everyone else’s", others}}
	} else if view.HasWorlds {
		view.Sections = []section{{"", append(mine, others...)}}
	}
	if discovery {
		view.DiscoveryNote = "Running worlds appear automatically under Friends → LAN Games on devices in the same network."
	} else {
		view.DiscoveryNote = "On Tailscale devices, add worlds via Servers → Add Server with address mc-<name>, port 19132."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := listPage.ExecuteTemplate(w, "base.tmpl", view); err != nil {
		log.Printf("render: %v", err)
	}
}

// worldDetail renders the per-world management page. Reads are open to
// everyone who can reach the panel; the template hides controls the
// requester can't use and every action stays server-enforced.
func worldDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	wd := findWorld(load(), name)
	if wd == nil {
		http.NotFound(w, r)
		return
	}
	login, rl := requester(r)
	wv := buildWorldView(*wd, login, rl)
	wv.Players = playersOf(name)
	view := worldPageView{worldView: wv,
		Modes: modes, Diffs: diffs, Perms: perms, Bools: []string{"false", "true"},
		Version: appVersion, Identity: login, Role: rl.String(),
		Back: "/world/" + name}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := worldPage.ExecuteTemplate(w, "base.tmpl", view); err != nil {
		log.Printf("render: %v", err)
	}
}

// ---------- JSON API (used by the CLI) ----------
func apiWorlds(w http.ResponseWriter, _ *http.Request) {
	type wireWorld struct {
		world
		Status  string `json:"status"`
		Tailnet string `json:"tailnet"`
	}
	out := []wireWorld{}
	for _, wd := range load() {
		out = append(out, wireWorld{wd, worldStatus(wd.Name), "mc-" + wd.Name})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func apiVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": appVersion})
}

// ---------- actions ----------
func findWorld(ws []world, name string) *world {
	for i := range ws {
		if ws[i].Name == name {
			return &ws[i]
		}
	}
	return nil
}

var backRe = regexp.MustCompile(`^/world/[a-z0-9-]{1,20}$`)

// back redirects with 302 (the CLI greps for it) — to the detail page the
// form came from when it names one (validated, so it can't be an open
// redirect), otherwise to the list.
func back(w http.ResponseWriter, r *http.Request) {
	to := r.FormValue("back")
	if !backRe.MatchString(to) {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func newWorld(w http.ResponseWriter, r *http.Request) {
	login, rl := requester(r)
	if rl < roleUser {
		http.Error(w, "forbidden — sign in via Tailscale to create worlds", 403)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ws := load()
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	var pre *preset
	for i := range presets {
		if presets[i].Key == r.FormValue("preset") {
			pre = &presets[i]
		}
	}
	mode := r.FormValue("mode")
	if pre != nil {
		mode = pre.Mode
	} else if mode == "" {
		mode = "survival"
	}
	if !nameRe.MatchString(name) || !contains(modes, mode) {
		http.Error(w, "bad name/mode", 400)
		return
	}
	if findWorld(ws, name) != nil {
		http.Error(w, "world exists", 409)
		return
	}
	port, ip, ok := alloc(ws)
	if !ok {
		http.Error(w, "no free world IPs — raise WORLD_IP_LAST", 507)
		return
	}
	wd := defaultWorld(name, port, ip, mode)
	wd.Owner = login
	wd.Seed = strings.TrimSpace(r.FormValue("seed"))
	if r.FormValue("flat") != "" {
		wd.LevelType = "FLAT"
	}
	if pre != nil {
		wd.Difficulty, wd.Cheats = pre.Difficulty, pre.Cheats
	} else if mode == "creative" {
		wd.Difficulty, wd.Cheats = "peaceful", true
	}
	d := filepath.Join(dataDir, name)
	os.MkdirAll(d, 0o775)
	own(d)
	os.MkdirAll(filepath.Join(tsStateRoot, name), 0o700)
	save(append(ws, wd))
	regen()
	compose("up", "-d", "mc-"+name)
	back(w, r)
}

func settings(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	ws := load()
	name := r.PathValue("name")
	wd := authWorld(w, r, ws, name)
	if wd == nil {
		return
	}
	mode := formOr(r, "mode", wd.Mode)
	diff := formOr(r, "difficulty", wd.Difficulty)
	perm := formOr(r, "default_permission", wd.DefaultPermission)
	if !contains(modes, mode) || !contains(diffs, diff) || !contains(perms, perm) {
		http.Error(w, "bad value", 400)
		return
	}
	wd.Mode, wd.Difficulty, wd.DefaultPermission = mode, diff, perm
	wd.Cheats = r.FormValue("cheats") == "true"
	wd.MaxPlayers = clamp(formInt(r, "max_players", 10), 1, 30)
	wd.ViewDistance = clamp(formInt(r, "view_distance", 10), 5, 32)
	save(ws)
	regen()
	compose("up", "-d", "mc-"+name)
	back(w, r)
}

func permission(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	name := r.PathValue("name")
	if authWorld(w, r, load(), name) == nil {
		return
	}
	xuid, level := r.FormValue("xuid"), r.FormValue("level")
	if !xuidRe.MatchString(xuid) || !contains(perms, level) {
		http.Error(w, "bad value", 400)
		return
	}
	setPermission(name, xuid, level)
	docker("restart", "mc-"+name)
	back(w, r)
}

func clone(w http.ResponseWriter, r *http.Request) {
	login, rl := requester(r)
	mu.Lock()
	defer mu.Unlock()
	ws := load()
	name := r.PathValue("name")
	src := findWorld(ws, name)
	if src == nil {
		http.Error(w, "no such world", 404)
		return
	}
	// Cloning cold-stops the source for the copy, so it takes manage rights.
	if !canManage(login, rl, src) {
		http.Error(w, "forbidden — not your world (owner: "+orDash(src.Owner)+")", 403)
		return
	}
	newname := strings.ToLower(strings.TrimSpace(r.FormValue("newname")))
	if !nameRe.MatchString(newname) {
		http.Error(w, "bad name", 400)
		return
	}
	if findWorld(ws, newname) != nil {
		http.Error(w, "world exists", 409)
		return
	}
	port, ip, ok := alloc(ws)
	if !ok {
		http.Error(w, "no free world IPs — raise WORLD_IP_LAST", 507)
		return
	}
	wasRunning := worldStatus(name) == "running"
	if wasRunning { // cold copy: leveldb hates live copies
		docker("stop", "mc-"+name)
	}
	if err := os.CopyFS(filepath.Join(dataDir, newname), os.DirFS(filepath.Join(dataDir, name))); err != nil {
		log.Printf("clone copy: %v", err)
	}
	oldWorld := filepath.Join(dataDir, newname, "worlds", name)
	if _, err := os.Stat(oldWorld); err == nil {
		os.Rename(oldWorld, filepath.Join(dataDir, newname, "worlds", newname))
	}
	own(filepath.Join(dataDir, newname))
	if wasRunning {
		docker("start", "mc-"+name)
	}
	wd := *src
	wd.Name, wd.Port, wd.IP = newname, port, ip
	wd.Owner = login
	os.MkdirAll(filepath.Join(tsStateRoot, newname), 0o700) // fresh tailnet identity
	save(append(ws, wd))
	regen()
	compose("up", "-d", "mc-"+newname)
	back(w, r)
}

func deleteWorld(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	ws := load()
	name := r.PathValue("name")
	if authWorld(w, r, ws, name) == nil {
		return
	}
	var keep []world
	for _, wd := range ws {
		if wd.Name != name {
			keep = append(keep, wd)
		}
	}
	compose("rm", "-sf", "mc-"+name, "ts-"+name)
	arch := filepath.Join(dataDir, "_archive")
	os.MkdirAll(arch, 0o775)
	stamp := time.Now().Format("20060102-1504")
	if err := os.Rename(filepath.Join(dataDir, name), filepath.Join(arch, name+"-"+stamp)); err != nil {
		log.Printf("archive %s: %v", name, err)
	}
	// Drop the world's tailnet identity; the node goes offline and can be
	// removed from the admin console (a restored world re-auths as new).
	os.RemoveAll(filepath.Join(tsStateRoot, name))
	if keep == nil {
		keep = []world{}
	}
	save(keep)
	regen()
	back(w, r)
}

func start(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if authWorld(w, r, load(), name) == nil {
		return
	}
	docker("start", "ts-"+name, "mc-"+name)
	back(w, r)
}

func stop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if authWorld(w, r, load(), name) == nil {
		return
	}
	docker("stop", "mc-"+name) // sidecar stays up; node stays reachable
	back(w, r)
}

func restart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if authWorld(w, r, load(), name) == nil {
		return
	}
	docker("restart", "mc-"+name)
	back(w, r)
}

// ---------- helpers ----------
func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func getenvBool(k string, def bool) bool {
	switch strings.ToLower(os.Getenv(k)) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return def
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func formOr(r *http.Request, key, def string) string {
	if v := r.FormValue(key); v != "" {
		return v
	}
	return def
}

func formInt(r *http.Request, key string, def int) int {
	if n, err := strconv.Atoi(r.FormValue(key)); err == nil {
		return n
	}
	return def
}

func clamp(n, lo, hi int) int {
	return max(lo, min(hi, n))
}

// ---------- app icon + manifest (Add to Home Screen) ----------
// The icon is drawn in code — a 16×16 pixel-art mountain scene scaled up —
// so the binary ships zero image assets.
func genIcon(px int) []byte {
	const n = 16
	sky := color.NRGBA{0x8c, 0xc6, 0xe8, 0xff}
	snow := color.NRGBA{0xf4, 0xf6, 0xf2, 0xff}
	stone := color.NRGBA{0x8e, 0x8e, 0x85, 0xff}
	grass := color.NRGBA{0x4c, 0x9a, 0x2e, 0xff}
	grassD := color.NRGBA{0x3a, 0x7a, 0x21, 0xff}
	dirt := color.NRGBA{0x8a, 0x5a, 0x32, 0xff}
	dirtD := color.NRGBA{0x75, 0x49, 0x2a, 0xff}
	img := image.NewNRGBA(image.Rect(0, 0, px, px))
	s := px / n
	for gy := 0; gy < n; gy++ {
		for gx := 0; gx < n; gx++ {
			c := sky
			dx := gx - 8
			if dx < 0 {
				dx = -dx
			}
			switch {
			case gy >= 13:
				c = dirtD
				if (gx+gy)%2 == 0 {
					c = dirt
				}
			case gy == 12:
				c = dirt
			case gy == 11:
				c = grassD
			case gy == 10:
				c = grass
			case gy >= 3 && dx <= gy-3: // the mountain, snow-capped
				c = stone
				if gy <= 5 {
					c = snow
				}
			}
			for y := gy * s; y < (gy+1)*s; y++ {
				for x := gx * s; x < (gx+1)*s; x++ {
					img.SetNRGBA(x, y, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

var icon192, icon512 = genIcon(192), genIcon(512)

func servePNG(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	}
}

func manifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	json.NewEncoder(w).Encode(map[string]any{
		"name": "homerealm", "short_name": "homerealm",
		"start_url": "/", "display": "standalone",
		"background_color": "#eff0e2", "theme_color": "#4c9a2e",
		"icons": []map[string]string{
			{"src": "/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/icon-512.png", "sizes": "512x512", "type": "image/png"},
		},
	})
}

// ---------- main ----------
func routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", index)
	mux.HandleFunc("GET /world/{name}", worldDetail)
	mux.HandleFunc("GET /api/worlds", apiWorlds)
	mux.HandleFunc("GET /api/version", apiVersion)
	mux.HandleFunc("GET /icon-192.png", servePNG(icon192))
	mux.HandleFunc("GET /icon-512.png", servePNG(icon512))
	mux.HandleFunc("GET /apple-touch-icon.png", servePNG(icon192))
	mux.HandleFunc("GET /manifest.webmanifest", manifest)
	mux.HandleFunc("POST /new", newWorld)
	mux.HandleFunc("POST /settings/{name}", settings)
	mux.HandleFunc("POST /permission/{name}", permission)
	mux.HandleFunc("POST /clone/{name}", clone)
	mux.HandleFunc("POST /delete/{name}", deleteWorld)
	mux.HandleFunc("POST /start/{name}", start)
	mux.HandleFunc("POST /stop/{name}", stop)
	mux.HandleFunc("POST /restart/{name}", restart)
	return mux
}

func main() {
	log.SetFlags(log.LstdFlags)
	for _, d := range []string{dataDir, tsStateRoot, filepath.Join(tsStateRoot, "panel")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}
	mux := routes()

	// Optional plain-HTTP LAN listener (for the CLI and non-Tailscale devices
	// at home). The tailnet is the primary, authenticated front door.
	if panelLAN {
		go func() {
			log.Printf("panel (LAN) listening on :%d", panelPort)
			if err := http.ListenAndServe(fmt.Sprintf(":%d", panelPort), mux); err != nil {
				log.Printf("LAN listener: %v", err)
			}
		}()
	}

	ts := &tsnet.Server{
		Hostname: tsHostname,
		Dir:      filepath.Join(tsStateRoot, "panel"),
		AuthKey:  tsAuthKey,
		UserLogf: log.Printf,
		Logf:     func(string, ...any) {}, // quiet the debug firehose
	}
	defer ts.Close()

	var err error
	localClient, err = ts.LocalClient()
	if err != nil {
		log.Fatalf("tsnet local client: %v", err)
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		st, err := ts.Up(ctx)
		if err != nil {
			log.Printf("tailnet not up yet: %v (if there's a login URL above, open it)", err)
			return
		}
		log.Printf("panel on your tailnet: https://%s", strings.TrimSuffix(st.Self.DNSName, "."))
	}()

	// HTTPS with automatic Tailscale certs, HTTP alongside for tailnets
	// without HTTPS enabled.
	go func() {
		ln, err := ts.ListenTLS("tcp", ":443")
		if err != nil {
			log.Printf("tailnet HTTPS unavailable: %v", err)
			return
		}
		if err := http.Serve(ln, mux); err != nil {
			log.Printf("tailnet HTTPS listener: %v", err)
		}
	}()
	ln, err := ts.Listen("tcp", ":80")
	if err != nil {
		log.Fatalf("tsnet listen: %v", err)
	}
	if err := http.Serve(ln, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("tailnet listener: %v", err)
	}
}
