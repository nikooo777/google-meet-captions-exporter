package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "meet-transcript"
	serverVersion = "0.1.0"

	defaultAddr = "127.0.0.1:6338"
	ingestPath  = "/ingest"
	mcpPath     = "/mcp"

	tokenEnvVar = "MEET_BRIDGE_TOKEN"
	addrEnvVar  = "MEET_BRIDGE_ADDR"
	dirEnvVar   = "MEET_BRIDGE_DIR"

	maxBodyBytes       = 16 << 20
	settlingWindowMs   = 2000
	activeWindowMs     = 120000
	defaultRecentCount = 12

	timeLayout         = "15:04:05"
	filenameTimeLayout = "2006-01-02_15-04-05"
	headerTimeLayout   = "2006-01-02 15:04:05"
)

type Entry struct {
	Speaker   string `json:"speaker"`
	Timestamp int64  `json:"timestamp"`
	Text      string `json:"text"`
	UpdatedAt int64  `json:"updated_at"`
}

type Snapshot struct {
	MeetingID  string  `json:"meeting_id"`
	CapturedAt int64   `json:"captured_at"`
	Entries    []Entry `json:"entries"`
}

type persistRecord struct {
	filename string
	captured int64
	hash     [32]byte
}

type bridge struct {
	dir string

	mu        sync.RWMutex
	snapshots map[string]Snapshot

	persistMu sync.Mutex
	persisted map[string]persistRecord
}

func newBridge(dir string) *bridge {
	return &bridge{
		dir:       dir,
		snapshots: make(map[string]Snapshot),
		persisted: make(map[string]persistRecord),
	}
}

// put stores the latest snapshot and prunes meetings that have gone quiet past
// the active window (their final transcript is already on disk). Out-of-order
// snapshots for a meeting (older CapturedAt) are rejected so a late retry can't
// regress the live transcript; the bool reports whether the snapshot was stored.
func (b *bridge) put(snap Snapshot) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	existing, ok := b.snapshots[snap.MeetingID]
	if ok && snap.CapturedAt < existing.CapturedAt {
		return false
	}
	b.snapshots[snap.MeetingID] = snap
	for id, s := range b.snapshots {
		if id != snap.MeetingID && snap.CapturedAt-s.CapturedAt > activeWindowMs {
			delete(b.snapshots, id)
		}
	}
	return true
}

// resolve returns the snapshot for meetingID, or — when meetingID is empty and
// exactly one meeting is currently active — that sole active meeting. An explicit
// meetingID is returned even if it has gone quiet.
func (b *bridge) resolve(meetingID string) (Snapshot, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if meetingID != "" {
		snap, ok := b.snapshots[meetingID]
		return snap, ok
	}
	now := time.Now().UnixMilli()
	var found Snapshot
	active := 0
	for _, s := range b.snapshots {
		if now-s.CapturedAt <= activeWindowMs {
			found = s
			active++
		}
	}
	if active == 1 {
		return found, true
	}
	return Snapshot{}, false
}

func (b *bridge) activeCount(now int64) int {
	n := 0
	for _, s := range b.snapshots {
		if now-s.CapturedAt <= activeWindowMs {
			n++
		}
	}
	return n
}

func (b *bridge) noMeetingMessage(meetingID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if meetingID != "" {
		return fmt.Sprintf("No transcript for meeting %q. Call get_status to list active meetings.", meetingID)
	}
	if b.activeCount(time.Now().UnixMilli()) == 0 {
		return "No active meeting. Start a Google Meet call with captions on, then enable the bridge in the extension."
	}
	return "Multiple meetings are active — pass meeting_id. Call get_status to list them."
}

// --- persistence ---

func earliestTimestamp(entries []Entry) int64 {
	start := entries[0].Timestamp
	for _, e := range entries {
		if e.Timestamp < start {
			start = e.Timestamp
		}
	}
	return start
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

func transcriptFilename(snap Snapshot) string {
	stamp := time.UnixMilli(earliestTimestamp(snap.Entries)).Format(filenameTimeLayout)
	return fmt.Sprintf("%s_%s.txt", stamp, sanitize(snap.MeetingID))
}

func hashContent(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

func renderForFile(snap Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Google Meet transcript — %s\n# Started %s\n\n",
		snap.MeetingID, time.UnixMilli(earliestTimestamp(snap.Entries)).Format(headerTimeLayout))
	for _, e := range snap.Entries {
		text := strings.TrimSpace(e.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", time.UnixMilli(e.Timestamp).Format(timeLayout), e.Speaker, text)
	}
	return b.String()
}

// persist atomically writes a meeting's full transcript to disk, skipping writes
// that would not change the file. The whole body holds persistMu — the only mutex
// touching the persisted map — so concurrent ingests for one meeting can't rename
// an older snapshot over a newer one (an out-of-order snapshot is dropped by
// CapturedAt). A snapshot read lock keeps pruning from racing a delayed write.
// Failures are logged but don't fail ingest, since the snapshot stays in memory
// and is retried.
func (b *bridge) persist(snap Snapshot) {
	if b.dir == "" || countNonEmpty(snap.Entries) == 0 {
		return
	}
	content := renderForFile(snap)
	hash := hashContent(content)

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	current, ok := b.snapshots[snap.MeetingID]
	if !ok || current.CapturedAt != snap.CapturedAt {
		return
	}

	for id, r := range b.persisted {
		if id != snap.MeetingID && snap.CapturedAt-r.captured > activeWindowMs {
			delete(b.persisted, id)
		}
	}

	rec, known := b.persisted[snap.MeetingID]
	if known && (rec.captured > snap.CapturedAt || rec.hash == hash) {
		return
	}
	if rec.filename == "" {
		rec.filename = transcriptFilename(snap)
	}

	path := filepath.Join(b.dir, rec.filename)
	tmp, err := os.CreateTemp(b.dir, ".meet-bridge-*.tmp")
	if err != nil {
		log.Printf("persist: create temp: %v", err)
		return
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.WriteString(content)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		log.Printf("persist: write %s: %v / close: %v", path, writeErr, closeErr)
		return
	}
	err = os.Rename(tmpPath, path)
	if err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("persist: rename to %s: %v", path, err)
		return
	}

	b.persisted[snap.MeetingID] = persistRecord{filename: rec.filename, captured: snap.CapturedAt, hash: hash}
}

// --- HTTP ---

func (b *bridge) ingestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var snap Snapshot
	err := json.NewDecoder(r.Body).Decode(&snap)
	if err != nil {
		http.Error(w, "malformed body", http.StatusBadRequest)
		return
	}
	if snap.MeetingID == "" {
		http.Error(w, "meeting_id required", http.StatusBadRequest)
		return
	}

	if b.put(snap) {
		b.persist(snap)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// withAuth gates a handler behind the shared bearer token and caps the request body.
func withAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// authorized requires a non-empty configured token and a matching bearer token.
func authorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// --- transcript rendering (MCP tools) ---

func renderEntries(entries []Entry, capturedAt int64) string {
	var b strings.Builder
	wrote := false
	for i := range entries {
		text := strings.TrimSpace(entries[i].Text)
		if text == "" {
			continue
		}
		if wrote {
			b.WriteByte('\n')
		}
		stamp := time.UnixMilli(entries[i].Timestamp).Format(timeLayout)
		fmt.Fprintf(&b, "[%s] %s: %s", stamp, entries[i].Speaker, text)
		if i == len(entries)-1 && capturedAt-entries[i].UpdatedAt < settlingWindowMs {
			b.WriteString(" …")
		}
		wrote = true
	}
	return b.String()
}

func tailByCount(entries []Entry, count int) []Entry {
	if count <= 0 || count >= len(entries) {
		return entries
	}
	return entries[len(entries)-count:]
}

func filterByMinutes(entries []Entry, minutes int, nowMs int64) []Entry {
	cutoff := nowMs - int64(minutes)*60_000
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.UpdatedAt >= cutoff {
			out = append(out, e)
		}
	}
	return out
}

func countNonEmpty(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if strings.TrimSpace(e.Text) != "" {
			n++
		}
	}
	return n
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// --- MCP tools ---

type transcriptArgs struct {
	MeetingID string `json:"meeting_id,omitempty" jsonschema:"meeting id; omit when only one meeting is active"`
}

type recentArgs struct {
	MeetingID string `json:"meeting_id,omitempty" jsonschema:"meeting id; omit when only one meeting is active"`
	Minutes   int    `json:"minutes,omitempty" jsonschema:"include entries updated within the last N minutes"`
	Count     int    `json:"count,omitempty" jsonschema:"return the last N entries; takes precedence over minutes; defaults to 12"`
}

type statusArgs struct{}

type sinceArgs struct {
	MeetingID string `json:"meeting_id,omitempty" jsonschema:"meeting id; omit when only one meeting is active"`
	Since     int64  `json:"since,omitempty" jsonschema:"Unix-ms cursor from a previous call; omit on the first call to receive the full transcript"`
}

func (b *bridge) handleGetTranscript(_ context.Context, _ *mcp.CallToolRequest, args transcriptArgs) (*mcp.CallToolResult, any, error) {
	snap, ok := b.resolve(args.MeetingID)
	if !ok {
		return textResult(b.noMeetingMessage(args.MeetingID)), nil, nil
	}
	text := renderEntries(snap.Entries, snap.CapturedAt)
	if text == "" {
		text = "(transcript is empty)"
	}
	return textResult(text), nil, nil
}

func (b *bridge) handleGetRecent(_ context.Context, _ *mcp.CallToolRequest, args recentArgs) (*mcp.CallToolResult, any, error) {
	snap, ok := b.resolve(args.MeetingID)
	if !ok {
		return textResult(b.noMeetingMessage(args.MeetingID)), nil, nil
	}

	entries := snap.Entries
	switch {
	case args.Count > 0:
		entries = tailByCount(entries, args.Count)
	case args.Minutes > 0:
		entries = filterByMinutes(entries, args.Minutes, time.Now().UnixMilli())
	default:
		entries = tailByCount(entries, defaultRecentCount)
	}

	text := renderEntries(entries, snap.CapturedAt)
	if text == "" {
		text = "(no matching entries yet)"
	}
	return textResult(text), nil, nil
}

func (b *bridge) handleGetSince(_ context.Context, _ *mcp.CallToolRequest, args sinceArgs) (*mcp.CallToolResult, any, error) {
	snap, ok := b.resolve(args.MeetingID)
	if !ok {
		return textResult(b.noMeetingMessage(args.MeetingID)), nil, nil
	}

	cursor := args.Since
	picked := make([]Entry, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		if e.UpdatedAt > cursor {
			cursor = e.UpdatedAt
		}
		if e.UpdatedAt > args.Since {
			picked = append(picked, e)
		}
	}

	body := renderEntries(picked, snap.CapturedAt)
	if body == "" {
		body = fmt.Sprintf("(no new or updated lines since cursor %d)", args.Since)
	}
	return textResult(fmt.Sprintf("cursor: %d\n%s", cursor, body)), nil, nil
}

func (b *bridge) handleGetStatus(_ context.Context, _ *mcp.CallToolRequest, _ statusArgs) (*mcp.CallToolResult, any, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	now := time.Now().UnixMilli()
	active := make([]string, 0, len(b.snapshots))
	for id, s := range b.snapshots {
		if now-s.CapturedAt <= activeWindowMs {
			active = append(active, id)
		}
	}
	sort.Strings(active)

	var sb strings.Builder
	fmt.Fprintf(&sb, "active_meetings: %d", len(active))
	if len(active) == 0 {
		sb.WriteString("\n(no active meeting right now)")
	}
	for _, id := range active {
		s := b.snapshots[id]
		last := time.UnixMilli(s.CapturedAt).Format(timeLayout)
		ageSec := (now - s.CapturedAt) / 1000
		fmt.Fprintf(&sb, "\n- %s: %d entries, last update %s (%ds ago)",
			id, countNonEmpty(s.Entries), last, ageSec)
	}
	fmt.Fprintf(&sb, "\nsaving transcripts to: %s", b.dir)
	return textResult(sb.String()), nil, nil
}

func newMCPServer(b *bridge) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_transcript",
		Description: "Return the full transcript of the live Google Meet call as timestamped lines. Call this when the user asks about the whole conversation or an earlier part of it.",
	}, b.handleGetTranscript)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_recent",
		Description: "Return the most recent part of the live Google Meet transcript (last N entries, or entries updated within the last N minutes). Call this first when the user asks what someone just said or wants help replying to something said moments ago — it keeps context small. A trailing ' …' marks a line that may still be changing.",
	}, b.handleGetRecent)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_since",
		Description: "Incrementally pull only the transcript lines added or changed since a cursor — the efficient way to keep up with a live call. First call: omit `since` to get the full transcript; the result's first line is `cursor: <ms>`. On each later call, pass that value back as `since` to receive only new or revised lines plus a fresh cursor. A line still being spoken is revised in place, so it reappears with updated text (and a trailing ' …') until it settles.",
	}, b.handleGetSince)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_status",
		Description: "Report which meetings are active, how many entries each has, when each was last updated, and where transcripts are saved on disk. Call this when the transcript tools report no active meeting, or to check the bridge is receiving data.",
	}, b.handleGetStatus)
	return server
}

// --- wiring ---

func envOr(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func transcriptDir() (string, error) {
	dir := os.Getenv(dirEnvVar)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home dir: %w", err)
		}
		dir = filepath.Join(home, ".local", "share", "meet-bridge", "transcripts")
	}
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return "", fmt.Errorf("cannot create transcript dir %s: %w", dir, err)
	}
	chmodErr := os.Chmod(dir, 0o700)
	if chmodErr != nil {
		log.Printf("transcriptDir: chmod %s 0700: %v", dir, chmodErr)
	}
	return dir, nil
}

func ensureLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("addr host %q is not loopback; refusing to bind a non-local interface", host)
	}
	return nil
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	addr := flag.String("addr", envOr(addrEnvVar, defaultAddr), "loopback address to listen on")
	flag.Parse()

	err := ensureLoopback(*addr)
	if err != nil {
		log.Fatalf("meet-bridge: %v", err)
	}

	token := os.Getenv(tokenEnvVar)
	if token == "" {
		log.Fatalf("meet-bridge: %s must be set to a non-empty value", tokenEnvVar)
	}

	dir, err := transcriptDir()
	if err != nil {
		log.Fatalf("meet-bridge: %v", err)
	}

	b := newBridge(dir)
	server := newMCPServer(b)

	// Stateless + JSON responses: each /mcp request is self-contained, which keeps
	// the server simple, lets multiple clients (Claude, Codex) attach concurrently,
	// and is easy to probe with curl. The tools never push server→client requests.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)

	mux := http.NewServeMux()
	mux.Handle(ingestPath, withAuth(token, http.HandlerFunc(b.ingestHandler)))
	mux.Handle(mcpPath, withAuth(token, mcpHandler))

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(os.Stderr, "http: ", log.LstdFlags),
	}
	log.Printf("meet-bridge listening on http://%s (ingest %s, mcp %s); transcripts -> %s",
		*addr, ingestPath, mcpPath, dir)

	err = httpServer.ListenAndServe()
	if err != nil {
		log.Fatalf("meet-bridge: %v", err)
	}
}
