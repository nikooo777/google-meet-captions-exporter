package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "meet-transcript"
	serverVersion = "0.1.0"

	defaultAddr = "127.0.0.1:6338"
	ingestPath  = "/ingest"

	tokenEnvVar = "MEET_BRIDGE_TOKEN"
	addrEnvVar  = "MEET_BRIDGE_ADDR"

	maxBodyBytes       = 16 << 20
	settlingWindowMs   = 2000
	defaultRecentCount = 12
	timeLayout         = "15:04:05"
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

type bridge struct {
	mu          sync.RWMutex
	snapshots   map[string]Snapshot
	ingestBound atomic.Bool
}

func newBridge() *bridge {
	return &bridge{snapshots: make(map[string]Snapshot)}
}

func (b *bridge) put(snap Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snapshots[snap.MeetingID] = snap
}

// resolve returns the snapshot for meetingID, or — when meetingID is empty and
// exactly one meeting is active — that sole meeting.
func (b *bridge) resolve(meetingID string) (Snapshot, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if meetingID != "" {
		snap, ok := b.snapshots[meetingID]
		return snap, ok
	}
	if len(b.snapshots) == 1 {
		for _, snap := range b.snapshots {
			return snap, true
		}
	}
	return Snapshot{}, false
}

func (b *bridge) noMeetingMessage(meetingID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if meetingID != "" {
		return fmt.Sprintf("No transcript for meeting %q. Call get_status to list active meetings.", meetingID)
	}
	if len(b.snapshots) == 0 {
		return "No active meeting. Start a Google Meet call with captions on, then enable the bridge in the extension."
	}
	return "Multiple meetings are active — pass meeting_id. Call get_status to list them."
}

// --- HTTP ingest ---

func (b *bridge) runIngest(addr, token string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("ingest: cannot bind %s: %v (MCP still served; get_status reports ingest_bound=false)", addr, err)
		return
	}
	b.ingestBound.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc(ingestPath, b.ingestHandler(token))
	srv := &http.Server{
		Handler:  mux,
		ErrorLog: log.New(os.Stderr, "ingest-http: ", 0),
	}
	log.Printf("ingest: listening on http://%s%s", addr, ingestPath)

	err = srv.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		b.ingestBound.Store(false)
		log.Printf("ingest: server stopped: %v", err)
	}
}

func (b *bridge) ingestHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var snap Snapshot
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		err := decoder.Decode(&snap)
		if err != nil {
			http.Error(w, "malformed body", http.StatusBadRequest)
			return
		}
		if snap.MeetingID == "" {
			http.Error(w, "meeting_id required", http.StatusBadRequest)
			return
		}

		b.put(snap)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
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

// --- transcript rendering ---

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

	var sb strings.Builder
	fmt.Fprintf(&sb, "ingest_bound: %v\n", b.ingestBound.Load())
	fmt.Fprintf(&sb, "active_meetings: %d", len(b.snapshots))
	if len(b.snapshots) == 0 {
		sb.WriteString("\n(no transcript received yet)")
		return textResult(sb.String()), nil, nil
	}

	now := time.Now().UnixMilli()
	for _, id := range sortedKeys(b.snapshots) {
		snap := b.snapshots[id]
		last := time.UnixMilli(snap.CapturedAt).Format(timeLayout)
		ageSec := (now - snap.CapturedAt) / 1000
		fmt.Fprintf(&sb, "\n- %s: %d entries, last update %s (%ds ago)",
			id, countNonEmpty(snap.Entries), last, ageSec)
	}
	return textResult(sb.String()), nil, nil
}

func sortedKeys(m map[string]Snapshot) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- wiring ---

func envOr(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
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
	log.SetFlags(0)

	addr := flag.String("addr", envOr(addrEnvVar, defaultAddr), "loopback address for the ingest listener")
	flag.Parse()

	err := ensureLoopback(*addr)
	if err != nil {
		log.Fatalf("meet-bridge: %v", err)
	}

	token := os.Getenv(tokenEnvVar)
	if token == "" {
		log.Printf("warning: %s is empty — every %s request will be rejected with 401", tokenEnvVar, ingestPath)
	}

	b := newBridge()
	go b.runIngest(*addr, token)

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
		Description: "Report whether a live transcript is being received: which meetings are active, how many entries each has, when each was last updated, and whether the ingest listener is bound. Call this when the transcript tools report no active meeting, or to check the bridge is receiving data.",
	}, b.handleGetStatus)

	err = server.Run(context.Background(), &mcp.StdioTransport{})
	if err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}
