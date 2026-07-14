package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

//go:embed web/index.html
var webFS embed.FS

type Config struct {
	Bind         string   `json:"bind"`
	ServerScript string   `json:"server_script"`
	Services     []string `json:"services"`
	DBContainer  string   `json:"db_container"`
	DB           struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		User string `json:"user"`
		Pass string `json:"pass"`
	} `json:"db"`
	SOAP struct {
		URL  string `json:"url"`
		User string `json:"user"`
		Pass string `json:"pass"`
	} `json:"soap"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Bind == "" {
		c.Bind = "127.0.0.1:8080"
	}
	return &c, nil
}

var (
	cfg      *Config
	db       *sql.DB
	numCores int

	cpuMu    sync.Mutex
	cpuLast  = map[string]cpuSample{} // per-unit CPU delta tracking
)

type cpuSample struct {
	nsec uint64
	when time.Time
}

type Status struct {
	Time     string    `json:"time"`
	Host     Host      `json:"host"`
	DB       DBStatus  `json:"db"`
	Services []Service `json:"services"`
	Game     Game      `json:"game"`
	SOAP     SOAP      `json:"soap"`
}

type Host struct {
	Load1    float64 `json:"load1"`
	Load5    float64 `json:"load5"`
	Load15   float64 `json:"load15"`
	Cores    int     `json:"cores"`
	MemTotal uint64  `json:"memTotal"`
	MemUsed  uint64  `json:"memUsed"`
	MemPct   float64 `json:"memPct"`
}

type DBStatus struct {
	Container string  `json:"container"`
	State     string  `json:"state"`
	Health    string  `json:"health"`
	Up        bool    `json:"up"`
	PingOK    bool    `json:"pingOk"`
	PingMs    float64 `json:"pingMs"`
	Error     string  `json:"error"`
}

type Service struct {
	Unit      string  `json:"unit"`
	Active    string  `json:"active"`
	Sub       string  `json:"sub"`
	Up        bool    `json:"up"`
	PID       int     `json:"pid"`
	MemBytes  uint64  `json:"memBytes"`
	CPUPct    float64 `json:"cpuPct"`
	UptimeSec int64   `json:"uptimeSec"`
}

type Game struct {
	OK         bool   `json:"ok"`
	Online     int    `json:"online"`
	Bots       int    `json:"bots"`
	Players    int    `json:"players"`
	Accounts   int    `json:"accounts"`
	Characters int    `json:"characters"`
	Error      string `json:"error"`
}

type SOAP struct {
	OK    bool   `json:"ok"`
	Info  string `json:"info"`
	Error string `json:"error"`
}

func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func collectStatus() Status {
	s := Status{Time: time.Now().Format(time.RFC3339)}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); s.Host = collectHost() }()
	go func() { defer wg.Done(); s.DB = collectDB() }()
	go func() { defer wg.Done(); s.Services = collectServices() }()
	go func() { defer wg.Done(); s.Game, s.SOAP = collectGame(), collectSOAP() }()
	wg.Wait()
	return s
}

func collectHost() Host {
	var h Host
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			h.Load1, _ = strconv.ParseFloat(f[0], 64)
			h.Load5, _ = strconv.ParseFloat(f[1], 64)
			h.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	h.Cores = countCores()
	mt, ma := memInfo()
	h.MemTotal = mt
	if mt > 0 {
		h.MemUsed = mt - ma
		h.MemPct = float64(h.MemUsed) / float64(mt) * 100
	}
	return h
}

func countCores() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	return n
}

func memInfo() (total, avail uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		v *= 1024 // kB -> bytes
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return total, avail
}

func collectDB() DBStatus {
	d := DBStatus{Container: cfg.DBContainer}
	out, err := runCmd(4*time.Second, "docker", "inspect", "-f",
		"{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", cfg.DBContainer)
	if err != nil {
		d.State = "not found"
		d.Error = strings.TrimSpace(out)
	} else {
		parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
		d.State = parts[0]
		if len(parts) > 1 {
			d.Health = parts[1]
		}
		d.Up = d.State == "running"
	}

	// SQL connectivity ping regardless of container introspection.
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		if err := db.PingContext(ctx); err != nil {
			if d.Error == "" {
				d.Error = err.Error()
			}
		} else {
			d.PingOK = true
			d.PingMs = float64(time.Since(start).Microseconds()) / 1000
		}
	}
	return d
}

func collectServices() []Service {
	svcs := make([]Service, len(cfg.Services))
	var wg sync.WaitGroup
	for i, unit := range cfg.Services {
		wg.Add(1)
		go func(i int, unit string) {
			defer wg.Done()
			svcs[i] = serviceStatus(unit)
		}(i, unit)
	}
	wg.Wait()
	return svcs
}

func serviceStatus(unit string) Service {
	s := Service{Unit: unit, Active: "unknown"}
	out, _ := runCmd(4*time.Second, "systemctl", "--user", "show", unit,
		"--property=ActiveState,SubState,MainPID,MemoryCurrent,CPUUsageNSec,ActiveEnterTimestampMonotonic")
	m := parseKV(out)
	s.Active = m["ActiveState"]
	s.Sub = m["SubState"]
	s.Up = s.Active == "active"
	s.PID, _ = strconv.Atoi(m["MainPID"])

	if v, err := strconv.ParseUint(m["MemoryCurrent"], 10, 64); err == nil && v < (1<<63) {
		s.MemBytes = v
	}
	// Uptime from monotonic start vs current /proc/uptime.
	if mono, err := strconv.ParseUint(m["ActiveEnterTimestampMonotonic"], 10, 64); err == nil && mono > 0 {
		if up := procUptime(); up > 0 {
			sec := int64(up) - int64(mono/1_000_000)
			if sec > 0 {
				s.UptimeSec = sec
			}
		}
	}
	// CPU% from cumulative CPUUsageNSec delta between polls.
	if nsec, err := strconv.ParseUint(m["CPUUsageNSec"], 10, 64); err == nil {
		s.CPUPct = cpuPercent(unit, nsec)
	}
	return s
}

func parseKV(s string) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '='); i > 0 {
			m[line[:i]] = line[i+1:]
		}
	}
	return m
}

func procUptime() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func cpuPercent(unit string, nsec uint64) float64 {
	cpuMu.Lock()
	defer cpuMu.Unlock()
	now := time.Now()
	prev, ok := cpuLast[unit]
	cpuLast[unit] = cpuSample{nsec: nsec, when: now}
	if !ok || nsec < prev.nsec {
		return 0
	}
	dt := now.Sub(prev.when).Seconds()
	if dt <= 0 {
		return 0
	}
	// Normalize across all cores: 100% == every core fully saturated.
	pct := (float64(nsec-prev.nsec) / 1e9) / dt * 100
	if numCores > 0 {
		pct /= float64(numCores)
	}
	if pct < 0 {
		pct = 0
	}
	return pct
}

func collectGame() Game {
	g := Game{}
	if db == nil {
		g.Error = "no db connection"
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	q := func(dst *int, query string) error {
		return db.QueryRowContext(ctx, query).Scan(dst)
	}
	if err := q(&g.Online, "SELECT COUNT(*) FROM acore_characters.characters WHERE online=1"); err != nil {
		g.Error = err.Error()
		return g
	}
	if err := q(&g.Bots, "SELECT COUNT(*) FROM acore_characters.characters c "+
		"JOIN acore_auth.account a ON c.account=a.id "+
		"WHERE c.online=1 AND a.username LIKE 'RNDBOT%'"); err != nil {
		g.Error = err.Error()
		return g
	}
	g.Players = g.Online - g.Bots
	if g.Players < 0 {
		g.Players = 0
	}
	_ = q(&g.Accounts, "SELECT COUNT(*) FROM acore_auth.account")
	_ = q(&g.Characters, "SELECT COUNT(*) FROM acore_characters.characters")
	g.OK = true
	return g
}

func collectSOAP() SOAP {
	s := SOAP{}
	if cfg.SOAP.User == "" || cfg.SOAP.Pass == "" {
		s.Error = "not configured (set soap.user/soap.pass in config.json)"
		return s
	}
	info, err := soapCommand("server info")
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.OK = true
	s.Info = strings.TrimSpace(info)
	return s
}

func soapCommand(command string) (string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="urn:AC">` +
		`<SOAP-ENV:Body><ns1:executeCommand><command>` +
		xmlEscape(command) +
		`</command></ns1:executeCommand></SOAP-ENV:Body></SOAP-ENV:Envelope>`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.SOAP.URL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.SetBasicAuth(cfg.SOAP.User, cfg.SOAP.Pass)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("SOAP auth failed (account needs GM level 3)")
	}
	// Pull the first <result>...</result> text out of the envelope.
	if r := extractTag(buf.String(), "result"); r != "" {
		return r, nil
	}
	if f := extractTag(buf.String(), "faultstring"); f != "" {
		return "", fmt.Errorf("SOAP fault: %s", f)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SOAP HTTP %d", resp.StatusCode)
	}
	return strings.TrimSpace(buf.String()), nil
}

func extractTag(s, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return xmlUnescape(s[i+len(open) : i+len(open)+j])
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlUnescape(s string) string {
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&apos;", "'", "&#xD;", "", "&#10;", "\n")
	return r.Replace(s)
}

type controlReq struct {
	Action string `json:"action"` // start | stop | restart
	Target string `json:"target"` // all | <unit name>
}

func handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req controlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Action != "start" && req.Action != "stop" && req.Action != "restart" {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}

	var out string
	var err error
	if req.Target == "all" || req.Target == "" {
		log.Printf("control: server.sh %s", req.Action)
		out, err = runCmd(90*time.Second, cfg.ServerScript, req.Action)
	} else {
		if !validUnit(req.Target) {
			http.Error(w, "unknown target", http.StatusBadRequest)
			return
		}
		log.Printf("control: systemctl --user %s %s", req.Action, req.Target)
		out, err = runCmd(90*time.Second, "systemctl", "--user", req.Action, req.Target)
	}

	resp := map[string]any{"ok": err == nil, "output": out}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}

func validUnit(u string) bool {
	for _, s := range cfg.Services {
		if s == u {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, collectStatus())
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func main() {
	confPath := os.Getenv("ACMONITOR_CONFIG")
	if confPath == "" {
		confPath = "config.json"
	}
	var err error
	cfg, err = loadConfig(confPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	numCores = countCores()

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?timeout=3s&readTimeout=3s&writeTimeout=3s",
		cfg.DB.User, cfg.DB.Pass, cfg.DB.Host, cfg.DB.Port)
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		log.Printf("db open: %v", err)
	} else {
		db.SetMaxOpenConns(4)
		db.SetConnMaxLifetime(5 * time.Minute)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/control", handleControl)

	srv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("acmonitor listening on http://%s", cfg.Bind)
	log.Fatal(srv.ListenAndServe())
}
