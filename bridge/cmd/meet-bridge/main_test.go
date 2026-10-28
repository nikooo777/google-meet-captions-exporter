package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testSnapshot(session string, sequence uint64, text string) Snapshot {
	now := time.Now().UnixMilli()
	return Snapshot{
		MeetingID: "abc-defg-hij", SessionID: session, StartedAt: now,
		CapturedAt: now, Sequence: sequence,
		Entries: []Entry{{Speaker: "Alice", Timestamp: now, UpdatedAt: now, Text: text}},
	}
}

func saveSnapshot(t *testing.T, b *bridge, snap Snapshot) {
	t.Helper()
	if !b.put(snap) {
		t.Fatal("snapshot rejected")
	}
	err := b.persist(snap)
	if err != nil {
		t.Fatal(err)
	}
}

func savedText(t *testing.T, b *bridge, snap Snapshot) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(b.dir, transcriptFilename(snap)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCaptureResetPreservesPreviousFile(t *testing.T) {
	b := newBridge(t.TempDir())
	first := testSnapshot("before-reload", 1, "Earlier conversation")
	saveSnapshot(t, b, first)
	second := testSnapshot("after-reload", 1, "New conversation")
	second.StartedAt = first.StartedAt
	saveSnapshot(t, b, second)
	if !strings.Contains(savedText(t, b, first), "Earlier conversation") {
		t.Fatal("reload erased earlier history")
	}
	if !strings.Contains(savedText(t, b, second), "New conversation") {
		t.Fatal("new capture was not saved separately")
	}
	files, err := os.ReadDir(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
}

func TestLegacyCaptureResetPreservesPreviousFile(t *testing.T) {
	b := newBridge(t.TempDir())
	first := testSnapshot("", 0, "Legacy earlier history")
	first.StartedAt = 0
	saveSnapshot(t, b, first)
	second := first
	second.CapturedAt++
	second.Entries = []Entry{{Speaker: "Alice", Timestamp: first.Entries[0].Timestamp + 1, Text: "Legacy new capture"}}
	saveSnapshot(t, b, second)
	if !strings.Contains(savedText(t, b, first), "Legacy earlier history") {
		t.Fatal("legacy reset erased earlier history")
	}
}

func TestPersistenceFailureIsReportedAndRetried(t *testing.T) {
	b := newBridge(filepath.Join(t.TempDir(), "not-created-yet"))
	snap := testSnapshot("retry", 1, "First draft")
	for sequence := uint64(1); sequence <= 2; sequence++ {
		snap.Sequence = sequence
		if sequence == 2 {
			snap.Entries[0].Text = "Latest correction"
		}
		body, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, ingestPath, bytes.NewReader(body))
		b.ingestHandler(recorder, request)
		var response struct {
			OK               bool   `json:"ok"`
			PersistenceError string `json:"persistence_error"`
		}
		err = json.Unmarshal(recorder.Body.Bytes(), &response)
		if err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || !response.OK || response.PersistenceError == "" {
			t.Fatalf("save failure not reported: %s", recorder.Body.String())
		}
	}
	status, _, err := b.handleGetStatus(context.Background(), nil, statusArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toolText(status), "retry pending") {
		t.Fatal("MCP status hides the pending save")
	}
	err = os.MkdirAll(b.dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	b.retryPending()
	if !strings.Contains(savedText(t, b, snap), "Latest correction") {
		t.Fatal("retry did not preserve the latest failed snapshot")
	}
	status, _, err = b.handleGetStatus(context.Background(), nil, statusArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(toolText(status), "save error") {
		t.Fatal("save error persisted after recovery")
	}
}

func toolText(result *mcp.CallToolResult) string {
	return result.Content[0].(*mcp.TextContent).Text
}

func delta(t *testing.T, b *bridge, since int64) (int64, string) {
	t.Helper()
	result, _, err := b.handleGetSince(context.Background(), nil, sinceArgs{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	header, body, ok := strings.Cut(toolText(result), "\n")
	if !ok {
		t.Fatalf("missing cursor: %s", toolText(result))
	}
	cursor, err := strconv.ParseInt(strings.TrimPrefix(header, "cursor: "), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return cursor, body
}

func TestDeltaCapturesSameMillisecondCorrection(t *testing.T) {
	b := newBridge("")
	snap := testSnapshot("delta", 1, "Original wording")
	b.put(snap)
	cursor, _ := delta(t, b, 0)
	snap.Sequence++
	snap.Entries[0].Text = "Corrected wording"
	b.put(snap)
	next, text := delta(t, b, cursor)
	if next <= cursor || !strings.Contains(text, "Corrected wording") {
		t.Fatalf("correction missing: cursor=%d text=%s", next, text)
	}
	snap.Sequence++
	b.put(snap)
	_, text = delta(t, b, next)
	if strings.Contains(text, "Corrected wording") {
		t.Fatal("unchanged heartbeat resent the caption")
	}
}

func TestDeltaResetsAcrossCaptureSessions(t *testing.T) {
	b := newBridge("")
	first := testSnapshot("old-session", 1, "Old session")
	b.put(first)
	cursor, _ := delta(t, b, 0)
	b.put(testSnapshot("new-session", 1, "New session"))
	_, text := delta(t, b, cursor)
	if !strings.Contains(text, "reset: true") || !strings.Contains(text, "New session") {
		t.Fatalf("session reset missing: %s", text)
	}
}

func TestDelayedCaptureCannotReplaceNewerLiveSession(t *testing.T) {
	b := newBridge(t.TempDir())
	old := testSnapshot("before-reload", 1, "Before reload")
	old.CapturedAt -= 100
	old.StartedAt -= 200
	saveSnapshot(t, b, old)
	current := testSnapshot("after-reload", 1, "After reload")
	saveSnapshot(t, b, current)
	cursor, _ := delta(t, b, 0)
	delayed := old
	delayed.Sequence++
	delayed.CapturedAt = current.CapturedAt - 1
	saveSnapshot(t, b, delayed)
	snap, ok := b.resolve("")
	if !ok || snap.SessionID != current.SessionID {
		t.Fatal("delayed packet replaced the current live capture")
	}
	_, text := delta(t, b, cursor)
	if strings.Contains(text, "Before reload") || strings.Contains(text, "reset: true") {
		t.Fatalf("delayed packet changed the delta baseline: %s", text)
	}
	delayed.Sequence++
	delayed.Entries = []Entry{{Speaker: "Alice", Timestamp: old.StartedAt, Text: "Late archived correction"}}
	saveSnapshot(t, b, delayed)
	if !strings.Contains(savedText(t, b, old), "Late archived correction") {
		t.Fatal("old-session archive was discarded along with live selection")
	}
	ended := false
	current.Active = &ended
	current.Sequence++
	current.CapturedAt++
	b.put(current)
	delayed.Active = &ended
	delayed.Sequence++
	b.put(delayed)
	snap, ok = b.resolve(current.MeetingID)
	if !ok || snap.SessionID != current.SessionID {
		t.Fatal("delayed ended capture replaced the latest archived selection")
	}
}

func TestDeltaResetsWhenSelectionReturnsToExistingCapture(t *testing.T) {
	b := newBridge("")
	old := testSnapshot("existing-session", 1, "Existing session")
	old.CapturedAt -= 100
	b.put(old)
	current := testSnapshot("new-session", 1, "New session")
	b.put(current)
	cursor, _ := delta(t, b, 0)
	old.Sequence++
	old.CapturedAt = current.CapturedAt - 1
	b.put(old)
	ended := false
	current.Sequence++
	current.CapturedAt++
	current.Active = &ended
	b.put(current)
	next, text := delta(t, b, cursor)
	if next <= cursor || !strings.Contains(text, "reset: true") || !strings.Contains(text, "Existing session") {
		t.Fatalf("return to an existing capture did not reset the baseline: %s", text)
	}
	_, text = delta(t, b, next)
	if strings.Contains(text, "reset: true") || strings.Contains(text, "Existing session") {
		t.Fatalf("acknowledged baseline was resent: %s", text)
	}
}

func TestClearKeepsPendingPersistenceErrorsVisible(t *testing.T) {
	b := newBridge(filepath.Join(t.TempDir(), "unavailable"))
	old := testSnapshot("before-clear", 1, "Words before clear")
	body, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	b.ingestHandler(recorder, httptest.NewRequest(http.MethodPost, ingestPath, bytes.NewReader(body)))
	var first struct {
		Revision int64 `json:"persistence_revision"`
	}
	err = json.Unmarshal(recorder.Body.Bytes(), &first)
	if err != nil {
		t.Fatal(err)
	}
	current := testSnapshot("after-clear", 1, "")
	current.Entries = nil
	body, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	b.ingestHandler(recorder, httptest.NewRequest(http.MethodPost, ingestPath, bytes.NewReader(body)))
	var response struct {
		Revision int64             `json:"persistence_revision"`
		Errors   map[string]string `json:"persistence_errors"`
		Error    string            `json:"persistence_error"`
	}
	err = json.Unmarshal(recorder.Body.Bytes(), &response)
	if err != nil {
		t.Fatal(err)
	}
	if response.Revision <= first.Revision || response.Errors[captureKey(old)] == "" || response.Error == "" {
		t.Fatalf("empty replacement session hid a failed save: %s", recorder.Body.String())
	}
	err = os.MkdirAll(b.dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	b.retryPending()
	status := b.persistenceStatus()
	if status.Revision <= response.Revision || len(status.Errors) != 0 {
		t.Fatal("recovery was not confirmed by a newer persistence status")
	}
	if !strings.Contains(savedText(t, b, old), "Words before clear") {
		t.Fatal("previous capture did not survive recovery")
	}
}

func TestDeltaResetsWhenLinesAreRemoved(t *testing.T) {
	b := newBridge("")
	snap := testSnapshot("removed", 1, "Caption to remove")
	b.put(snap)
	cursor, _ := delta(t, b, 0)
	snap.Sequence++
	snap.Entries = nil
	b.put(snap)
	_, text := delta(t, b, cursor)
	if !strings.Contains(text, "reset: true") || !strings.Contains(text, "transcript is empty") {
		t.Fatalf("removal not communicated: %s", text)
	}
}

func TestStaleSaveAfterMetadataEvictionDoesNotOverwrite(t *testing.T) {
	b := newBridge(t.TempDir())
	old := testSnapshot("eviction", 1, "Old content")
	b.put(old)
	latest := old
	latest.Sequence = 2
	latest.Entries = []Entry{{Speaker: "Alice", Timestamp: old.StartedAt, Text: "Latest content"}}
	saveSnapshot(t, b, latest)
	b.persistMu.Lock()
	delete(b.persisted, captureKey(old))
	b.persistMu.Unlock()
	err := b.persist(old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(savedText(t, b, latest), "Latest content") {
		t.Fatal("metadata eviction allowed an older write to erase the file")
	}
	b.mu.Lock()
	delete(b.snapshots, captureKey(old))
	b.mu.Unlock()
	err = b.persist(old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(savedText(t, b, latest), "Latest content") {
		t.Fatal("pruned snapshot allowed an older write to erase the file")
	}
}

func TestEndedMeetingDoesNotBlockActiveSelection(t *testing.T) {
	b := newBridge("")
	ended := testSnapshot("ended", 1, "Finished meeting")
	ended.MeetingID = "old-call-aaa"
	active := false
	ended.Active = &active
	b.put(ended)
	current := testSnapshot("current", 1, "Current meeting")
	b.put(current)
	snap, ok := b.resolve("")
	if !ok || snap.SessionID != current.SessionID {
		t.Fatal("ended meeting interfered with active selection")
	}
}

func TestLateSnapshotCannotReactivateEndedCapture(t *testing.T) {
	b := newBridge("")
	snap := testSnapshot("left", 2, "Final words")
	active := false
	snap.Active = &active
	b.put(snap)
	snap.Sequence--
	snap.Active = nil
	if b.put(snap) {
		t.Fatal("stale snapshot accepted after leaving")
	}
	_, ok := b.resolve("")
	if ok {
		t.Fatal("stale snapshot reactivated the meeting")
	}
}

func ingestConcurrent(b *bridge, snap Snapshot, wg *sync.WaitGroup, failures chan<- error) {
	defer wg.Done()
	if b.put(snap) {
		err := b.persist(snap)
		if err != nil {
			failures <- err
		}
	}
}

func TestConcurrentPersistenceKeepsNewestSnapshot(t *testing.T) {
	b := newBridge(t.TempDir())
	base := testSnapshot("concurrent", 1, "")
	const count = 24
	failures := make(chan error, count)
	var wg sync.WaitGroup
	for i := 1; i <= count; i++ {
		snap := base
		snap.Sequence = uint64(i)
		snap.Entries = []Entry{{Speaker: "Alice", Timestamp: base.StartedAt, Text: fmt.Sprintf("Revision %d", i)}}
		wg.Add(1)
		go ingestConcurrent(b, snap, &wg, failures)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if !strings.Contains(savedText(t, b, base), "Revision 24") {
		t.Fatal("older concurrent snapshot overwrote the latest correction")
	}
}

func TestLocalHTTPAccessPolicy(t *testing.T) {
	b := newBridge("")
	open := newHTTPHandler(b, "")
	protected := newHTTPHandler(b, "secret")
	snapshotBody, err := json.Marshal(testSnapshot("access", 1, "Local transcript"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name          string
		host          string
		origin        string
		mediaType     string
		authorization string
		requireToken  bool
		want          int
	}{
		{name: "native client", mediaType: "application/json", want: http.StatusOK},
		{name: "localhost", host: "localhost:6338", mediaType: "application/json", want: http.StatusOK},
		{name: "IPv6", host: "[::1]:6338", mediaType: "application/json", want: http.StatusOK},
		{name: "extension", origin: "chrome-extension://" + strings.Repeat("a", 32), mediaType: "application/json", want: http.StatusOK},
		{name: "same origin", origin: "http://127.0.0.1:6338", mediaType: "application/json", want: http.StatusOK},
		{name: "JSON charset", mediaType: "application/json; charset=utf-8", want: http.StatusOK},
		{name: "foreign website", origin: "https://example.com", mediaType: "application/json", want: http.StatusForbidden},
		{name: "opaque origin", origin: "null", mediaType: "application/json", want: http.StatusForbidden},
		{name: "other local website", origin: "http://127.0.0.1:3000", mediaType: "application/json", want: http.StatusForbidden},
		{name: "fake extension", origin: "chrome-extension://" + strings.Repeat("z", 32), mediaType: "application/json", want: http.StatusForbidden},
		{name: "extension path", origin: "chrome-extension://" + strings.Repeat("a", 32) + "/page", mediaType: "application/json", want: http.StatusForbidden},
		{name: "DNS rebinding", host: "attacker.example:6338", mediaType: "application/json", want: http.StatusForbidden},
		{name: "external host", host: "192.168.1.1:6338", mediaType: "application/json", want: http.StatusForbidden},
		{name: "host credentials", host: "attacker@127.0.0.1:6338", mediaType: "application/json", want: http.StatusForbidden},
		{name: "simple text POST", mediaType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "form POST", mediaType: "application/x-www-form-urlencoded", want: http.StatusUnsupportedMediaType},
		{name: "missing content type", want: http.StatusUnsupportedMediaType},
		{name: "missing required token", requireToken: true, mediaType: "application/json", want: http.StatusUnauthorized},
		{name: "wrong token", requireToken: true, authorization: "Bearer wrong", mediaType: "application/json", want: http.StatusUnauthorized},
		{name: "valid token", requireToken: true, authorization: "Bearer secret", mediaType: "application/json", want: http.StatusOK},
		{name: "website with token", requireToken: true, authorization: "Bearer secret", origin: "https://example.com", mediaType: "application/json", want: http.StatusForbidden},
		{name: "old token in open mode", authorization: "Bearer old-token", mediaType: "application/json", want: http.StatusOK},
	}
	for _, endpoint := range []string{ingestPath, mcpPath} {
		body := string(snapshotBody)
		if endpoint == mcpPath {
			body = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
		}
		for _, tc := range cases {
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:6338"+endpoint, strings.NewReader(body))
			if tc.host != "" {
				request.Host = tc.host
			}
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			request.Header.Set("Content-Type", tc.mediaType)
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("Authorization", tc.authorization)
			recorder := httptest.NewRecorder()
			handler := open
			if tc.requireToken {
				handler = protected
			}
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Errorf("%s %s: got %d, want %d: %s", endpoint, tc.name, recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.want == http.StatusOK && endpoint == mcpPath && !strings.Contains(recorder.Body.String(), "get_transcript") {
				t.Errorf("%s: MCP tool discovery failed: %s", tc.name, recorder.Body.String())
			}
		}
	}
}

func TestLocalHTTPRejectsAmbiguousOrigins(t *testing.T) {
	handler := newHTTPHandler(newBridge(""), "")
	for _, origins := range [][]string{{""}, {"http://127.0.0.1:6338", "https://example.com"}} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:6338/ingest", strings.NewReader("{}"))
		request.Header["Origin"] = origins
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("origins %q: got %d, want 403", origins, recorder.Code)
		}
	}
}
