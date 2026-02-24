package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadJSONLInfersFieldsAndRows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	content := strings.Join([]string{
		`{"ts":"2026-02-23T18:00:00Z","severity":"INFO","msg":"hello","user":"gp"}`,
		`{"ts":"2026-02-23T18:01:00Z","severity":"WARN","msg":"bye"}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}

	page, err := loadJSONL(path)
	if err != nil {
		t.Fatalf("loadJSONL returned error: %v", err)
	}

	if page.Fields.Timestamp != "ts" || page.Fields.Level != "severity" || page.Fields.Message != "msg" {
		t.Fatalf("unexpected inferred fields: %+v", page.Fields)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(page.Rows))
	}
	if got := page.Rows[0].Timestamp; got != "2026-02-23T18:00:00Z" {
		t.Fatalf("unexpected timestamp: %q", got)
	}
	if got := page.Rows[1].Level; got != "WARN" {
		t.Fatalf("unexpected level: %q", got)
	}
	if got := page.Rows[0].Message; got != "hello" {
		t.Fatalf("unexpected message: %q", got)
	}
	if got := page.Rows[0].PrettyJSON; !strings.Contains(got, "\n  \"msg\": \"hello\"") {
		t.Fatalf("expected pretty JSON formatting, got %q", got)
	}
	if got := page.Rows[0].SearchText; !strings.Contains(got, "1 2026-02-23t18:00:00z info hello") {
		t.Fatalf("expected normalized search text, got %q", got)
	}
	if got := page.Rows[0].SearchText; !strings.Contains(got, `"user":"gp"`) {
		t.Fatalf("expected raw json included in search text, got %q", got)
	}
	if page.Metrics.Events != 2 || page.Metrics.ParseErrors != 0 || page.Metrics.DroppedEvents != 0 {
		t.Fatalf("unexpected metrics: %+v", page.Metrics)
	}
	if page.Metrics.EventsPerSec <= 0 {
		t.Fatalf("expected positive events/sec, got %f", page.Metrics.EventsPerSec)
	}
	if len(page.Malformed) != 0 {
		t.Fatalf("expected no malformed lines, got %+v", page.Malformed)
	}
}

func TestLoadJSONLSurfacesMalformedLinesAndContinues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	content := strings.Join([]string{
		`{"msg":"ok-1"}`,
		`{"msg":"broken"`,
		`{"msg":"ok-2"}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}

	page, err := loadJSONL(path)
	if err != nil {
		t.Fatalf("loadJSONL returned error: %v", err)
	}

	if len(page.Rows) != 2 {
		t.Fatalf("expected 2 parsed rows, got %d", len(page.Rows))
	}
	if got := []int{page.Rows[0].Line, page.Rows[1].Line}; got[0] != 1 || got[1] != 3 {
		t.Fatalf("expected source line numbers preserved, got %#v", got)
	}
	if len(page.Malformed) != 1 {
		t.Fatalf("expected 1 malformed line, got %d", len(page.Malformed))
	}
	if page.Malformed[0].Line != 2 || page.Malformed[0].Raw != `{"msg":"broken"` {
		t.Fatalf("unexpected malformed line: %+v", page.Malformed[0])
	}
	if !strings.Contains(page.Malformed[0].Error, "parse line 2") {
		t.Fatalf("expected parse error context, got %q", page.Malformed[0].Error)
	}
	if page.Metrics.Events != 2 || page.Metrics.ParseErrors != 1 {
		t.Fatalf("unexpected metrics: %+v", page.Metrics)
	}
}

func TestLoadJSONLSupportsJSONArrayImport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "logs.json")
	content := `[{"timestamp":"2026-02-23T18:00:00Z","level":"INFO","message":"from-array","service":"api"}]`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}

	page, err := loadJSONL(path)
	if err != nil {
		t.Fatalf("loadJSONL returned error: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(page.Rows))
	}
	if page.Rows[0].Message != "from-array" || page.Rows[0].Source != "api" {
		t.Fatalf("unexpected row: %+v", page.Rows[0])
	}
	if page.Fields.Source != "service" {
		t.Fatalf("expected inferred source field 'service', got %+v", page.Fields)
	}
}

func TestNewHandlerRendersReadableRows(t *testing.T) {
	t.Parallel()

	h := newHandler(pageData{
		Path:   "/tmp/logs.jsonl",
		Fields: inferredFields{Timestamp: "timestamp", Level: "level", Message: "message"},
		Rows: []logRow{{
			Line:       1,
			Timestamp:  "2026-02-23T18:00:00Z",
			Level:      "INFO",
			Message:    "hello <world>",
			RawJSON:    `{"message":"hello <world>","nested":{"ok":true}}`,
			PrettyJSON: "{\n  \"message\": \"hello <world>\",\n  \"nested\": {\n    \"ok\": true\n  }\n}",
			SearchText: `1 2026-02-23t18:00:00z info hello <world> {"message":"hello <world>","nested":{"ok":true}}`,
		}},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Sift", "Local JSONL viewer", "INFO", "2026-02-23T18:00:00Z"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	if !strings.Contains(body, "hello &lt;world&gt;") {
		t.Fatalf("expected escaped message in body: %s", body)
	}
	for _, want := range []string{
		`id="search-query"`,
		`id="filter-query"`,
		`<input id="query" class="query-input" type="hidden"`,
		`Search terms match all text.`,
		`field=value`,
		`field!=value`,
		`field~=value`,
		`field:*`,
		`field=low..high`,
		`id="filter-chip-list"`,
		`Column visibility`,
		`data-column="source"`,
		`data-column="context"`,
		`id="match-count"`,
		`id="events-per-sec"`,
		`id="parse-error-count"`,
		`id="dropped-event-count"`,
		`Malformed JSON lines (`,
		`id="malformed-count"`,
		`id="malformed-body"`,
		`class="log-row severity-info"`,
		`data-search="1 2026-02-23t18:00:00z info hello`,
		`data-raw-json="{&#34;message&#34;:&#34;hello &lt;world&gt;&#34;,&#34;nested&#34;:{&#34;ok&#34;:true}}"`,
		`data-line="1"`,
		`data-timestamp="2026-02-23T18:00:00Z"`,
		`data-level="INFO"`,
		`data-source=""`,
		`data-context=""`,
		`data-message="hello &lt;world&gt;"`,
		`Full-text search (`,
		`field=value field!=value`,
		`function applySearch()`,
		`function parseQueryClauses(value)`,
		`function tokenizeQuery(value)`,
		`function matchesFieldClause(row, clause)`,
		`function matchesRange(value, low, high)`,
		`parseQueryClauses(queryInput ? queryInput.value : "")`,
		`parsed.fieldClauses.every(function (clause)`,
		`searchMatch && fieldMatch`,
		`searchInput.addEventListener("input", queueSearchApply)`,
		`filterInput.addEventListener("input", queueSearchApply)`,
		`function queueSearchApply()`,
		`renderFilterChips();`,
		`event.key === "/"`,
		`event.key === "Escape"`,
		`Details &gt;`,
		`id="detail-drawer"`,
		`id="drawer-copy-fields"`,
		`Copy selected fields`,
		`class="copy-json"`,
		`data-raw-json="{&#34;message&#34;:&#34;hello &lt;world&gt;&#34;,&#34;nested&#34;:{&#34;ok&#34;:true}}"`,
		`class="payload-json"`,
		`navigator.clipboard`,
		`function updateIngestMetrics(metrics)`,
		`function appendMalformedLine(item)`,
		`hydrateInitialRowsFromDOM();`,
		`function renderVirtualRows()`,
		`MAX_CLIENT_ROWS = 500000`,
		`MAX_CLIENT_MALFORMED_ROWS = 5000`,
		`MAX_PENDING_EVENTS = 5000`,
		`rowsViewport.addEventListener("scroll", queueVirtualRender`,
		`virtual-top-spacer`,
		`virtual-bottom-spacer`,
		`severity-info`,
		`function applyColumnVisibility()`,
		`function rowSeverityClass(level)`,
		`selectNextMatch(1);`,
		`streamToggle.click();`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestNewHandlerFieldFilterScriptSupportsOperatorSyntax(t *testing.T) {
	t.Parallel()

	h := newHandler(pageData{
		Path: "/tmp/logs.jsonl",
		Rows: []logRow{{
			Line:       1,
			RawJSON:    `{"duration_ms":123,"user":"gp","nested":{"status":"ok"}}`,
			PrettyJSON: "{}",
			SearchText: `1 {"duration_ms":123,"user":"gp","nested":{"status":"ok"}}`,
		}},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`token.slice(-2) === ":*"`,
		`["!=", "~=", "="].some`,
		`rawValue.indexOf("..")`,
		`type: "exists"`,
		`type: "eq"`,
		`type: "neq"`,
		`type: "contains"`,
		`type: "range"`,
		`rowFieldLookup(row, clause.field)`,
		`return found.ok && found.value !== null;`,
		`metaKey === "line" || metaKey === "timestamp" || metaKey === "level" || metaKey === "source" || metaKey === "context" || metaKey === "message"`,
		`getObjectValueCaseInsensitive(cur, seg)`,
		`tr.setAttribute("data-raw-json", row.rawJSON || "")`,
		`tr.setAttribute("data-line", row.line == null ? "" : String(row.line))`,
		`tr.setAttribute("data-source", row.source || "")`,
		`tr.setAttribute("data-context", row.context || "")`,
		`tr.setAttribute("data-message", row.message || "")`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestNewHandlerPersistsBookmarksAndLastQueryState(t *testing.T) {
	t.Parallel()

	h := newHandler(pageData{
		Path: "/tmp/logs.jsonl",
		Rows: []logRow{{
			Line:       1,
			RawJSON:    `{"msg":"hello"}`,
			PrettyJSON: "{}",
			SearchText: `1 {"msg":"hello"}`,
		}},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="bookmark-save"`,
		`id="bookmark-clear"`,
		`id="bookmark-list"`,
		`Saved query bookmarks`,
		`var UI_STATE_KEY = "sift-ui-state-v2"`,
		`function loadUIState()`,
		`window.localStorage.getItem(UI_STATE_KEY)`,
		`function saveUIState()`,
		`window.localStorage.setItem(UI_STATE_KEY`,
		`search: uiState.search || ""`,
		`filters: uiState.filters || ""`,
		`bookmarks: Array.isArray(uiState.bookmarks)`,
		`columns: uiState.columns || {}`,
		`function syncQueryState()`,
		`function syncQueryInputs()`,
		`function addBookmark()`,
		`function clearBookmarks()`,
		`function renderBookmarks()`,
		`if (uiState.search) searchInput.value = uiState.search;`,
		`if (uiState.filters) filterInput.value = uiState.filters;`,
		`bookmarkSave.addEventListener("click", addBookmark)`,
		`bookmarkClear.addEventListener("click", clearBookmarks)`,
		`syncQueryState();`,
		`syncQueryInputs();`,
		`renderBookmarks();`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestLivePageAppendPreservesOrderAndPublishesEvents(t *testing.T) {
	t.Parallel()

	live := newLivePage(pageData{Path: "stdin"})
	sub := live.subscribe()
	defer live.unsubscribe(sub)

	live.append(1, map[string]any{
		"ts":  "2026-02-23T19:00:00Z",
		"msg": "first",
	})
	live.append(2, map[string]any{
		"ts":  "2026-02-23T19:00:01Z",
		"msg": "second",
	})

	page := live.snapshot()
	if len(page.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(page.Rows))
	}
	if page.Rows[0].Line != 1 || page.Rows[1].Line != 2 {
		t.Fatalf("unexpected line ordering: %#v", []int{page.Rows[0].Line, page.Rows[1].Line})
	}
	if page.Rows[0].Message != "first" || page.Rows[1].Message != "second" {
		t.Fatalf("unexpected messages: %q %q", page.Rows[0].Message, page.Rows[1].Message)
	}
	if page.Fields.Timestamp != "ts" || page.Fields.Message != "msg" {
		t.Fatalf("expected inferred fields from stream, got %+v", page.Fields)
	}
	if page.Metrics.Events != 2 || page.Metrics.ParseErrors != 0 {
		t.Fatalf("unexpected metrics: %+v", page.Metrics)
	}

	ev1 := <-sub
	ev2 := <-sub
	if ev1.Row.Line != 1 || ev2.Row.Line != 2 {
		t.Fatalf("unexpected event ordering: %d then %d", ev1.Row.Line, ev2.Row.Line)
	}
	if ev2.TotalRows != 2 {
		t.Fatalf("expected total rows in second event = 2, got %d", ev2.TotalRows)
	}
	if ev2.Metrics.Events != 2 {
		t.Fatalf("expected metrics in event payload, got %+v", ev2.Metrics)
	}
}

func TestLivePageTracksDroppedEventsForSlowSubscribers(t *testing.T) {
	t.Parallel()

	live := newLivePage(pageData{Path: "stdin"})
	sub := live.subscribe()
	defer live.unsubscribe(sub)

	for i := 0; i < cap(sub); i++ {
		sub <- streamEvent{Type: "append"}
	}

	live.append(1, map[string]any{"msg": "hello"})

	page := live.snapshot()
	if page.Metrics.DroppedEvents != 1 {
		t.Fatalf("expected 1 dropped event, got %+v", page.Metrics)
	}
}

func TestLivePageEvictsOldRowsBeyondRetentionLimit(t *testing.T) {
	t.Parallel()

	live := newLivePage(pageData{Path: "stdin"})
	total := maxRetainedLiveRows + 25
	for i := 1; i <= total; i++ {
		live.append(i, map[string]any{"msg": "row"})
	}

	page := live.snapshot()
	if got := len(page.Rows); got != maxRetainedLiveRows {
		t.Fatalf("expected %d retained rows, got %d", maxRetainedLiveRows, got)
	}
	if got := page.Rows[0].Line; got != 26 {
		t.Fatalf("expected oldest retained line to be 26, got %d", got)
	}
	if got := page.Rows[len(page.Rows)-1].Line; got != total {
		t.Fatalf("expected newest retained line %d, got %d", total, got)
	}
	if got := page.Metrics.Events; got != total {
		t.Fatalf("expected total ingested events %d, got %d", total, got)
	}
}

func TestNewLiveHandlerIncludesPauseResumeAndEventSource(t *testing.T) {
	t.Parallel()

	live := newLivePage(pageData{Path: "stdin"})
	live.append(1, map[string]any{"msg": "hello"})

	h := newLiveHandler(live)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="stream-toggle"`,
		`Pause stream`,
		`Resume stream`,
		`new EventSource("/events")`,
		`pendingEvents.push(payload)`,
		`flushPendingEvents()`,
		`id="stream-pending"`,
		`id="row-count"`,
		`id="total-count"`,
		`id="events-per-sec"`,
		`id="parse-error-count"`,
		`id="dropped-event-count"`,
		`id="malformed-count"`,
		`id="malformed-body"`,
		`Malformed JSON lines (`,
		`payload.type === "malformed"`,
		`updateIngestMetrics(payload.metrics)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestLiveHandlerStreamsAppendEvents(t *testing.T) {
	t.Parallel()

	live := newLivePage(pageData{Path: "stdin"})
	h := newLiveHandler(live)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(rr, req)
	}()

	time.Sleep(10 * time.Millisecond)
	live.append(1, map[string]any{"msg": "hello"})
	time.Sleep(10 * time.Millisecond)
	cancel()
	wg.Wait()

	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("unexpected content type: %q", ct)
	}
	chunk := rr.Body.String()
	if !strings.Contains(chunk, "data: ") {
		t.Fatalf("expected SSE data frame, got %q", chunk)
	}
	payloadLine := ""
	for _, line := range strings.Split(chunk, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payloadLine = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if payloadLine == "" {
		t.Fatalf("expected SSE payload line, got %q", chunk)
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(payloadLine), &ev); err != nil {
		t.Fatalf("unmarshal SSE payload: %v (payload=%q)", err, payloadLine)
	}
	if ev.Type != "append" || ev.Row.Line != 1 || ev.Row.Message != "hello" {
		t.Fatalf("unexpected event payload: %+v", ev)
	}
	if ev.Metrics.Events != 1 {
		t.Fatalf("expected metrics in SSE payload, got %+v", ev.Metrics)
	}
}

func TestStreamJSONLContinuesAfterMalformedAndPublishesMalformedState(t *testing.T) {
	t.Parallel()

	live := newLivePage(pageData{Path: "stdin"})

	err := streamJSONL(strings.NewReader(strings.Join([]string{
		`{"msg":"ok-1"}`,
		`{"msg":"broken"`,
		`{"msg":"ok-2"}`,
	}, "\n")), live)
	if err == nil || err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}

	page := live.snapshot()
	if len(page.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(page.Rows))
	}
	if page.Rows[0].Line != 1 || page.Rows[1].Line != 3 {
		t.Fatalf("expected source line numbers 1 and 3, got %d and %d", page.Rows[0].Line, page.Rows[1].Line)
	}
	if len(page.Malformed) != 1 {
		t.Fatalf("expected 1 malformed line, got %d", len(page.Malformed))
	}
	if page.Metrics.Events != 2 || page.Metrics.ParseErrors != 1 {
		t.Fatalf("unexpected metrics: %+v", page.Metrics)
	}
}

func TestTailFilePublishesAppendedRows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "tail.jsonl")
	if err := os.WriteFile(path, []byte("{\"msg\":\"existing\"}\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}

	live := newLivePage(pageData{Path: path})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tailFile(ctx, path, 1, live)
	}()

	time.Sleep(50 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open append file: %v", err)
	}
	if _, err := f.WriteString("{\"msg\":\"appended\"}\n"); err != nil {
		_ = f.Close()
		t.Fatalf("append row: %v", err)
	}
	_ = f.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page := live.snapshot()
		if len(page.Rows) == 1 && page.Rows[0].Message == "appended" && page.Rows[0].Line == 2 {
			cancel()
			if err := <-done; err != nil && err != context.Canceled {
				t.Fatalf("tailFile returned error: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("timed out waiting for appended row")
}

func TestParseIngestJSONSupportsObjectAndArray(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "object", body: `{"msg":"ok"}`, want: 1},
		{name: "array", body: `[{"msg":"a"},{"msg":"b"}]`, want: 2},
		{name: "scalar", body: `123`, want: 1},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseIngestJSON([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseIngestJSON returned error: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("expected %d records, got %d", tt.want, len(got))
			}
		})
	}
}

func TestFrontendHandlerIngestAppendsRows(t *testing.T) {
	t.Parallel()

	base := pageData{Path: "ingest", RetentionCap: 100}
	live := newLivePage(base)
	h := newFrontendHandler(base, live)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(`[{"message":"hello","service":"api"},{"message":"bye","service":"worker"}]`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := int(resp["ingested"].(float64)); got != 2 {
		t.Fatalf("expected ingested=2, got %d", got)
	}

	page := live.snapshot()
	if len(page.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(page.Rows))
	}
	if page.Rows[0].Line != 1 || page.Rows[1].Line != 2 {
		t.Fatalf("expected auto line numbering 1,2 got %d,%d", page.Rows[0].Line, page.Rows[1].Line)
	}
	if page.Rows[0].Message != "hello" || page.Rows[1].Message != "bye" {
		t.Fatalf("unexpected messages: %+v", page.Rows)
	}
	if page.Metrics.Events != 2 {
		t.Fatalf("expected metrics.events=2, got %+v", page.Metrics)
	}
}
