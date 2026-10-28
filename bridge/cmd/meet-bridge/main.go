package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "meet-transcript"
	serverVersion = "0.2.0"

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
	persistRetryDelay  = 2 * time.Second

	timeLayout         = "15:04:05"
	filenameTimeLayout = "2006-01-02_15-04-05"
	headerTimeLayout   = "2006-01-02 15:04:05"
)

type Entry struct {
	Speaker   string `json:"speaker"`
	Timestamp int64  `json:"timestamp"`
	Text      string `json:"text"`
	UpdatedAt int64  `json:"updated_at"`
	Revision  int64  `json:"-"`
}

type Snapshot struct {
	MeetingID     string  `json:"meeting_id"`
	SessionID     string  `json:"session_id,omitempty"`
	StartedAt     int64   `json:"started_at,omitempty"`
	CapturedAt    int64   `json:"captured_at"`
	Sequence      uint64  `json:"sequence,omitempty"`
	Active        *bool   `json:"active,omitempty"`
	Entries       []Entry `json:"entries"`
	Revision      int64   `json:"-"`
	ResetRevision int64   `json:"-"`
}

type persistRecord struct {
	filename string
	captured int64
	sequence uint64
	hash     [32]byte
	pending  *Snapshot
	err      string
	seenAt   time.Time
}

type persistenceStatus struct {
	Revision int64             `json:"persistence_revision"`
	Errors   map[string]string `json:"persistence_errors"`
}

type bridge struct {
	dir string

	mu        sync.RWMutex
	snapshots map[string]Snapshot
	selected  map[string]string
	revision  int64

	persistMu       sync.Mutex
	persisted       map[string]*persistRecord
	persistRevision int64
}

func newBridge(dir string) *bridge {
	return &bridge{
		dir:       dir,
		snapshots: make(map[string]Snapshot),
		selected:  make(map[string]string),
		persisted: make(map[string]*persistRecord),
	}
}

func captureID(snap Snapshot) string {
	if snap.SessionID != "" {
		return snap.SessionID
	}
	return "legacy-" + strconv.FormatInt(captureStartedAt(snap), 10)
}

func captureKey(snap Snapshot) string {
	return snap.MeetingID + "/" + captureID(snap)
}

func olderSnapshot(snap, existing Snapshot) bool {
	if snap.Sequence != 0 && existing.Sequence != 0 {
		return snap.Sequence < existing.Sequence
	}
	return snap.CapturedAt < existing.CapturedAt
}

func (b *bridge) put(snap Snapshot) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := captureKey(snap)
	existing, ok := b.snapshots[key]
	if ok && olderSnapshot(snap, existing) {
		return false
	}
	b.revision = max(b.revision+1, time.Now().UnixMilli())
	snap.Revision = b.revision
	snap.ResetRevision = existing.ResetRevision
	if !ok || len(snap.Entries) < len(existing.Entries) {
		snap.ResetRevision = snap.Revision
	}
	snap.Entries = append([]Entry(nil), snap.Entries...)
	for i := range snap.Entries {
		entry := &snap.Entries[i]
		entry.Revision = snap.Revision
		if i < len(existing.Entries) {
			previous := existing.Entries[i]
			if entry.Speaker == previous.Speaker && entry.Timestamp == previous.Timestamp && entry.Text == previous.Text {
				entry.Revision = previous.Revision
			}
		}
	}
	b.snapshots[key] = snap
	for id, s := range b.snapshots {
		if id != key && snap.CapturedAt-s.CapturedAt > activeWindowMs {
			delete(b.snapshots, id)
		}
	}
	b.updateSelections(time.Now().UnixMilli())
	return true
}

func activeSnapshot(snap Snapshot, now int64) bool {
	return (snap.Active == nil || *snap.Active) && now-snap.CapturedAt <= activeWindowMs
}

func newerCapture(snap, previous Snapshot) bool {
	if snap.CapturedAt != previous.CapturedAt {
		return snap.CapturedAt > previous.CapturedAt
	}
	if captureStartedAt(snap) != captureStartedAt(previous) {
		return captureStartedAt(snap) > captureStartedAt(previous)
	}
	return snap.Revision > previous.Revision
}

func (b *bridge) currentSnapshots(now int64) map[string]Snapshot {
	current := make(map[string]Snapshot)
	for _, snap := range b.snapshots {
		previous, ok := current[snap.MeetingID]
		active := activeSnapshot(snap, now)
		previousActive := activeSnapshot(previous, now)
		if !ok || (active && !previousActive) || (active == previousActive && newerCapture(snap, previous)) {
			current[snap.MeetingID] = snap
		}
	}
	return current
}

func (b *bridge) updateSelections(now int64) {
	current := b.currentSnapshots(now)
	for meetingID := range b.selected {
		if _, ok := current[meetingID]; !ok {
			delete(b.selected, meetingID)
		}
	}
	for meetingID, snap := range current {
		key := captureKey(snap)
		if b.selected[meetingID] == key {
			continue
		}
		b.revision++
		snap.Revision = b.revision
		snap.ResetRevision = b.revision
		b.snapshots[key] = snap
		b.selected[meetingID] = key
	}
}

func (b *bridge) resolve(meetingID string) (Snapshot, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := time.Now().UnixMilli()
	if meetingID != "" {
		snap, ok := b.currentSnapshots(now)[meetingID]
		return snap, ok
	}
	active := b.activeSnapshots(now)
	if len(active) == 1 {
		for _, snap := range active {
			return snap, true
		}
	}
	return Snapshot{}, false
}

func (b *bridge) activeSnapshots(now int64) map[string]Snapshot {
	active := make(map[string]Snapshot)
	for meetingID, snap := range b.currentSnapshots(now) {
		if activeSnapshot(snap, now) {
			active[meetingID] = snap
		}
	}
	return active
}

func (b *bridge) noMeetingMessage(meetingID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if meetingID != "" {
		return fmt.Sprintf("No transcript for meeting %q. Call get_status to list active meetings.", meetingID)
	}
	if len(b.activeSnapshots(time.Now().UnixMilli())) == 0 {
		return "No active meeting. Start a Google Meet call with captions on, then enable the bridge in the extension."
	}
	return "Multiple meetings are active — pass meeting_id. Call get_status to list them."
}

// --- persistence ---

func earliestTimestamp(entries []Entry) int64 {
	if len(entries) == 0 {
		return 0
	}
	start := entries[0].Timestamp
	for _, e := range entries {
		if e.Timestamp < start {
			start = e.Timestamp
		}
	}
	return start
}

func captureStartedAt(snap Snapshot) int64 {
	if snap.StartedAt != 0 {
		return snap.StartedAt
	}
	return earliestTimestamp(snap.Entries)
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
	stamp := time.UnixMilli(captureStartedAt(snap)).Format(filenameTimeLayout)
	return fmt.Sprintf("%s_%s_%s.txt", stamp, sanitize(snap.MeetingID), sanitize(captureID(snap)))
}

func hashContent(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

func renderForFile(snap Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Google Meet transcript — %s\n# Started %s\n\n",
		snap.MeetingID, time.UnixMilli(captureStartedAt(snap)).Format(headerTimeLayout))
	for _, e := range snap.Entries {
		text := strings.TrimSpace(e.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", time.UnixMilli(e.Timestamp).Format(timeLayout), e.Speaker, text)
	}
	return b.String()
}

func (b *bridge) persist(snap Snapshot) error {
	if b.dir == "" || countNonEmpty(snap.Entries) == 0 {
		return nil
	}
	b.persistMu.Lock()
	defer b.persistMu.Unlock()
	return b.persistLocked(snap)
}

func (b *bridge) persistLocked(snap Snapshot) error {
	key := captureKey(snap)
	b.mu.RLock()
	current, currentKnown := b.snapshots[key]
	b.mu.RUnlock()
	if currentKnown && olderSnapshot(snap, current) {
		return nil
	}
	now := time.Now()
	for id, r := range b.persisted {
		if id != key && r.pending == nil && now.Sub(r.seenAt) > time.Duration(activeWindowMs)*time.Millisecond {
			delete(b.persisted, id)
		}
	}
	rec := b.persisted[key]
	if rec == nil {
		if !currentKnown {
			_, statErr := os.Stat(filepath.Join(b.dir, transcriptFilename(snap)))
			if statErr == nil {
				return nil
			}
		}
		rec = &persistRecord{filename: transcriptFilename(snap)}
		b.persisted[key] = rec
	}
	rec.seenAt = now
	previous := Snapshot{CapturedAt: rec.captured, Sequence: rec.sequence}
	if rec.pending != nil {
		previous = *rec.pending
	}
	if olderSnapshot(snap, previous) {
		if rec.err != "" {
			return fmt.Errorf("%s", rec.err)
		}
		return nil
	}
	content := renderForFile(snap)
	hash := hashContent(content)
	if rec.pending == nil && rec.captured != 0 && rec.hash == hash {
		rec.captured = snap.CapturedAt
		rec.sequence = snap.Sequence
		return nil
	}
	rec.pending = &snap
	path := filepath.Join(b.dir, rec.filename)
	err := writeTranscript(path, content)
	if err != nil {
		rec.err = err.Error()
		return err
	}
	rec.captured = snap.CapturedAt
	rec.sequence = snap.Sequence
	rec.hash = hash
	rec.pending = nil
	rec.err = ""
	return nil
}

func writeTranscript(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meet-bridge-*.tmp")
	if err != nil {
		return fmt.Errorf("save transcript: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_, writeErr := tmp.WriteString(content)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		return fmt.Errorf("save transcript: write: %v; close: %v", writeErr, closeErr)
	}
	err = os.Rename(tmpPath, path)
	if err != nil {
		return fmt.Errorf("save transcript: %w", err)
	}
	return nil
}

func (b *bridge) retryPending() {
	b.persistMu.Lock()
	defer b.persistMu.Unlock()
	var pending []Snapshot
	for _, rec := range b.persisted {
		if rec.pending != nil {
			pending = append(pending, *rec.pending)
		}
	}
	for _, snap := range pending {
		err := b.persistLocked(snap)
		if err != nil {
			log.Printf("persist retry: %v", err)
		}
	}
}

func (b *bridge) retryPersistence(ctx context.Context) {
	ticker := time.NewTicker(persistRetryDelay)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.retryPending()
		}
	}
}

func (b *bridge) persistenceStatus() persistenceStatus {
	b.persistMu.Lock()
	defer b.persistMu.Unlock()
	b.persistRevision = max(b.persistRevision+1, time.Now().UnixMilli())
	status := persistenceStatus{Revision: b.persistRevision, Errors: make(map[string]string)}
	for key, rec := range b.persisted {
		if rec.err != "" {
			status.Errors[key] = rec.err
		}
	}
	return status
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

	var saveError string
	if b.put(snap) {
		err = b.persist(snap)
		if err != nil {
			saveError = err.Error()
			log.Printf("persist: %v", err)
		}
	}
	status := b.persistenceStatus()
	if saveError == "" {
		for _, pendingError := range status.Errors {
			saveError = pendingError
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		persistenceStatus
		OK               bool   `json:"ok"`
		PersistenceError string `json:"persistence_error,omitempty"`
	}{persistenceStatus: status, OK: true, PersistenceError: saveError})
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allowedHost(host string) bool {
	parsed, err := url.Parse("http://" + host)
	return err == nil && parsed.Host == host && parsed.User == nil &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" &&
		loopbackHost(parsed.Hostname())
}

func allowedOrigin(origin, host string) bool {
	if origin == "http://"+host {
		return true
	}
	id, ok := strings.CutPrefix(origin, "chrome-extension://")
	if !ok || len(id) != 32 {
		return false
	}
	for _, c := range id {
		if c < 'a' || c > 'p' {
			return false
		}
	}
	return true
}

func withLocalAccess(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host) {
			http.Error(w, "loopback Host required", http.StatusForbidden)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && !allowedOrigin(origins[0], r.Host)) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
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
	Since     int64  `json:"since,omitempty" jsonschema:"opaque revision cursor from a previous call; omit on the first call to receive the full transcript"`
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

	reset := args.Since != 0 && (args.Since > snap.Revision || args.Since < snap.ResetRevision)
	picked := make([]Entry, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		if reset || e.Revision > args.Since {
			picked = append(picked, e)
		}
	}

	body := renderEntries(picked, snap.CapturedAt)
	if body == "" {
		if reset {
			body = "(transcript is empty)"
		} else {
			body = fmt.Sprintf("(no new or updated lines since cursor %d)", args.Since)
		}
	}
	if reset {
		body = "reset: true\n" + body
	}
	return textResult(fmt.Sprintf("cursor: %d\n%s", snap.Revision, body)), nil, nil
}

func (b *bridge) handleGetStatus(_ context.Context, _ *mcp.CallToolRequest, _ statusArgs) (*mcp.CallToolResult, any, error) {
	b.mu.RLock()
	now := time.Now().UnixMilli()
	snapshots := b.activeSnapshots(now)
	b.mu.RUnlock()
	active := make([]string, 0, len(snapshots))
	for id := range snapshots {
		active = append(active, id)
	}
	sort.Strings(active)

	var sb strings.Builder
	fmt.Fprintf(&sb, "active_meetings: %d", len(active))
	if len(active) == 0 {
		sb.WriteString("\n(no active meeting right now)")
	}
	for _, id := range active {
		s := snapshots[id]
		last := time.UnixMilli(s.CapturedAt).Format(timeLayout)
		ageSec := (now - s.CapturedAt) / 1000
		fmt.Fprintf(&sb, "\n- %s: %d entries, last update %s (%ds ago)",
			id, countNonEmpty(s.Entries), last, ageSec)
	}
	fmt.Fprintf(&sb, "\nsaving transcripts to: %s", b.dir)
	status := b.persistenceStatus()
	keys := make([]string, 0, len(status.Errors))
	for key := range status.Errors {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&sb, "\nsave error (%s; retry pending): %s", key, status.Errors[key])
	}
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
		Description: "Incrementally pull transcript lines added or changed since an opaque revision cursor. First call: omit `since` for the full transcript; the first line is `cursor: <revision>`. Pass that value back as `since` on later calls. A `reset: true` line means replace the previous baseline with the returned transcript. Revisions to a line reappear with updated text (and a trailing ' …') until it settles.",
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
	if !loopbackHost(host) {
		return fmt.Errorf("addr host %q is not loopback; refusing to bind a non-local interface", host)
	}
	return nil
}

func newHTTPHandler(b *bridge, token string) http.Handler {
	server := newMCPServer(b)
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	mux := http.NewServeMux()
	mux.Handle(ingestPath, http.HandlerFunc(b.ingestHandler))
	mux.Handle(mcpPath, mcpHandler)
	return withLocalAccess(token, mux)
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

	token := strings.TrimSpace(os.Getenv(tokenEnvVar))

	dir, err := transcriptDir()
	if err != nil {
		log.Fatalf("meet-bridge: %v", err)
	}

	b := newBridge(dir)
	go b.retryPersistence(context.Background())

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           newHTTPHandler(b, token),
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
