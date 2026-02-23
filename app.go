package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"io/fs"
)

type inferredFields struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Source    string `json:"source"`
	Context   string `json:"context"`
}

type logRow struct {
	Line       int    `json:"line"`
	Timestamp  string `json:"timestamp"`
	Level      string `json:"level"`
	Message    string `json:"message"`
	Source     string `json:"source"`
	Context    string `json:"context"`
	RawJSON    string `json:"rawJSON"`
	PrettyJSON string `json:"prettyJSON"`
	SearchText string `json:"searchText"`
}

type malformedLine struct {
	Line  int    `json:"line"`
	Raw   string `json:"raw"`
	Error string `json:"error"`
}

type ingestMetrics struct {
	Events        int     `json:"events"`
	EventsPerSec  float64 `json:"eventsPerSec"`
	ParseErrors   int     `json:"parseErrors"`
	DroppedEvents int     `json:"droppedEvents"`
}

type pageData struct {
	Path         string
	Fields       inferredFields
	Rows         []logRow
	Malformed    []malformedLine
	Metrics      ingestMetrics
	Live         bool
	RetentionCap int
}

//go:embed frontend/dist/* frontend/dist/assets/*
var frontendAssets embed.FS

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sift", flag.ContinueOnError)
	fs.SetOutput(stderr)
	filePath := fs.String("f", "", "path to JSONL file")
	tailMode := fs.Bool("tail", false, "watch file for appended lines")
	retentionCap := fs.Int("cap", defaultRetainedLiveRows, "max retained events in memory/UI")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *retentionCap <= 0 {
		return errors.New("cap must be > 0")
	}
	readFromStdin := *filePath == "-"
	if *filePath == "" {
		stat, err := os.Stdin.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			readFromStdin = true
		}
	}
	if *filePath == "" && !readFromStdin {
		return errors.New("usage: sift -f <path>")
	}

	var handler http.Handler
	var page pageData
	if readFromStdin {
		page = pageData{Path: "stdin", Live: true, RetentionCap: *retentionCap}
		live := newLivePage(page)
		handler = newFrontendHandler(page, live)
		go func() {
			if err := streamJSONL(os.Stdin, live); err != nil && !errors.Is(err, io.EOF) {
				_, _ = fmt.Fprintf(stderr, "stream error: %v\n", err)
			}
		}()
	} else if *tailMode {
		var err error
		page, err = loadJSONL(*filePath)
		if err != nil {
			return err
		}
		page.Live = true
		page.RetentionCap = *retentionCap
		if len(page.Rows) > *retentionCap {
			page.Rows = retainLatestRows(page.Rows, *retentionCap)
		}
		live := newLivePage(page)
		handler = newFrontendHandler(page, live)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			if err := tailFile(ctx, *filePath, maxSeenLine(page), live); err != nil && !errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintf(stderr, "tail error: %v\n", err)
			}
		}()
	} else {
		var err error
		page, err = loadJSONL(*filePath)
		if err != nil {
			return err
		}
		page.RetentionCap = *retentionCap
		if len(page.Rows) > *retentionCap {
			page.Rows = retainLatestRows(page.Rows, *retentionCap)
			page.Metrics.DroppedEvents += page.Metrics.Events - len(page.Rows)
		}
			handler = newFrontendHandler(page, nil)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	srv := &http.Server{Handler: handler}
	go func() {
		_ = srv.Serve(ln)
	}()

	if page.Live {
		_, _ = fmt.Fprintf(stdout, "Streaming JSONL from %s at http://%s\n", page.Path, ln.Addr().String())
	} else {
		_, _ = fmt.Fprintf(stdout, "Serving %d rows from %s at http://%s\n", len(page.Rows), page.Path, ln.Addr().String())
	}
	_, _ = fmt.Fprintln(stdout, "Press Ctrl+C to stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

func loadJSONL(path string) (pageData, error) {
	started := time.Now()
	content, err := os.ReadFile(path)
	if err != nil {
		return pageData{}, fmt.Errorf("open %s: %w", path, err)
	}

	absPath := path
	if p, err := filepath.Abs(path); err == nil {
		absPath = p
	}

	parsed, rawRecords, malformed, metrics, err := parseLogContent(content)
	if err != nil {
		return pageData{}, fmt.Errorf("read %s: %w", path, err)
	}

	fields := inferFields(rawRecords)
	rows := make([]logRow, 0, len(parsed))
	for _, item := range parsed {
		rec := item.rec
		rawJSONBytes, _ := json.Marshal(rec)
		prettyJSONBytes, _ := json.MarshalIndent(rec, "", "  ")
		rows = append(rows, logRow{
			Line:       item.line,
			Timestamp:  fieldValue(rec, fields.Timestamp),
			Level:      fieldValue(rec, fields.Level),
			Message:    fieldValue(rec, fields.Message),
			Source:     fieldValue(rec, fields.Source),
			Context:    fieldValue(rec, fields.Context),
			RawJSON:    string(rawJSONBytes),
			PrettyJSON: string(prettyJSONBytes),
			SearchText: buildSearchText(item.line, rec, rawJSONBytes, fields),
		})
	}
	metrics.Events = len(rows)
	metrics.EventsPerSec = calcEventsPerSec(metrics.Events, started)

	return pageData{Path: absPath, Fields: fields, Rows: rows, Malformed: malformed, Metrics: metrics, RetentionCap: defaultRetainedLiveRows}, nil
}

type parsedRecord struct {
	line int
	rec  map[string]any
}

func parseLogContent(content []byte) ([]parsedRecord, []map[string]any, []malformedLine, ingestMetrics, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 {
		var v any
		if err := json.Unmarshal(trimmed, &v); err == nil {
			switch vv := v.(type) {
			case []any:
				return parseJSONArray(vv), collectRawRecords(parseJSONArray(vv)), nil, ingestMetrics{Events: len(vv)}, nil
			case map[string]any:
				parsed := []parsedRecord{{line: 1, rec: vv}}
				return parsed, []map[string]any{vv}, nil, ingestMetrics{Events: 1}, nil
			}
		}
	}
	return parseJSONLContent(content)
}

func parseJSONArray(items []any) []parsedRecord {
	parsed := make([]parsedRecord, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			obj = map[string]any{"value": item}
		}
		parsed = append(parsed, parsedRecord{line: i + 1, rec: obj})
	}
	return parsed
}

func collectRawRecords(parsed []parsedRecord) []map[string]any {
	out := make([]map[string]any, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, p.rec)
	}
	return out
}

func parseJSONLContent(content []byte) ([]parsedRecord, []map[string]any, []malformedLine, ingestMetrics, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var parsed []parsedRecord
	var rawRecords []map[string]any
	var malformed []malformedLine
	metrics := ingestMetrics{}
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		rec, err := parseJSONLine(lineNo, line)
		if err != nil {
			metrics.ParseErrors++
			malformed = append(malformed, malformedLine{Line: lineNo, Raw: line, Error: err.Error()})
			continue
		}
		parsed = append(parsed, parsedRecord{line: lineNo, rec: rec})
		rawRecords = append(rawRecords, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, nil, ingestMetrics{}, err
	}
	metrics.Events = len(parsed)
	return parsed, rawRecords, malformed, metrics, nil
}

func streamJSONL(r io.Reader, live *livePage) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		rec, err := parseJSONLine(lineNo, line)
		if err != nil {
			live.appendMalformed(lineNo, line, err.Error())
			continue
		}
		live.append(lineNo, rec)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func maxSeenLine(page pageData) int {
	maxLine := 0
	for _, row := range page.Rows {
		if row.Line > maxLine {
			maxLine = row.Line
		}
	}
	for _, item := range page.Malformed {
		if item.Line > maxLine {
			maxLine = item.Line
		}
	}
	return maxLine
}

func tailFile(ctx context.Context, path string, startLine int, live *livePage) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	reader := bufio.NewReader(f)
	lineNo := startLine
	var partial strings.Builder
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		part, err := reader.ReadString('\n')
		if err == nil {
			partial.WriteString(part)
			line := strings.TrimSpace(strings.TrimSuffix(partial.String(), "\n"))
			partial.Reset()
			lineNo++
			if line == "" {
				continue
			}
			rec, parseErr := parseJSONLine(lineNo, line)
			if parseErr != nil {
				live.appendMalformed(lineNo, line, parseErr.Error())
				continue
			}
			live.append(lineNo, rec)
			continue
		}
		if errors.Is(err, io.EOF) {
			if part != "" {
				partial.WriteString(part)
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		return err
	}
}

func calcEventsPerSec(events int, started time.Time) float64 {
	elapsed := time.Since(started).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(events) / elapsed
}

func parseJSONLine(lineNo int, line string) (map[string]any, error) {
	var v any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		return nil, fmt.Errorf("parse line %d: %w", lineNo, err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		obj = map[string]any{"value": v}
	}
	return obj, nil
}

func inferFields(records []map[string]any) inferredFields {
	keys := collectKeys(records)
	return inferredFields{
		Timestamp: chooseKey(keys,
			[]string{"timestamp", "@timestamp", "time", "ts", "datetime", "date", "created_at"},
			[]string{"time", "date", "stamp"}),
		Level: chooseKey(keys,
			[]string{"level", "log_level", "severity", "lvl"},
			[]string{"level", "severity", "sev"}),
		Message: chooseKey(keys,
			[]string{"message", "msg", "event", "text", "log"},
			[]string{"message", "msg", "text", "event"}),
		Source: chooseKey(keys,
			[]string{"source", "service", "logger", "component", "module"},
			[]string{"service", "source", "logger", "component"}),
		Context: chooseKey(keys,
			[]string{"context", "ctx", "request_id", "trace_id"},
			[]string{"context", "trace", "request", "session"}),
	}
}

func collectKeys(records []map[string]any) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, rec := range records {
		for k := range rec {
			lk := strings.ToLower(k)
			if _, ok := seen[lk]; ok {
				continue
			}
			seen[lk] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	return keys
}

func chooseKey(keys []string, exact []string, contains []string) string {
	for _, target := range exact {
		for _, k := range keys {
			if strings.EqualFold(k, target) {
				return k
			}
		}
	}
	for _, part := range contains {
		for _, k := range keys {
			if strings.Contains(strings.ToLower(k), part) {
				return k
			}
		}
	}
	return ""
}

func fieldValue(rec map[string]any, key string) string {
	if key == "" {
		return ""
	}
	for k, v := range rec {
		if strings.EqualFold(k, key) {
			switch vv := v.(type) {
			case nil:
				return ""
			case string:
				return vv
			case float64, bool, int, int64:
				return fmt.Sprint(vv)
			default:
				b, err := json.Marshal(vv)
				if err != nil {
					return fmt.Sprint(vv)
				}
				return string(b)
			}
		}
	}
	return ""
}

func buildSearchText(line int, rec map[string]any, rawJSON []byte, fields inferredFields) string {
	parts := []string{
		fmt.Sprint(line),
		fieldValue(rec, fields.Timestamp),
		fieldValue(rec, fields.Level),
		fieldValue(rec, fields.Message),
		fieldValue(rec, fields.Source),
		fieldValue(rec, fields.Context),
		string(rawJSON),
	}
	return strings.ToLower(strings.Join(strings.Fields(strings.Join(parts, " ")), " "))
}

func newHandler(data pageData) http.Handler {
	return newAppHandler(data, nil)
}

func newLiveHandler(live *livePage) http.Handler {
	return newAppHandler(live.snapshot(), live)
}

type bootstrapPayload struct {
	Path         string         `json:"path"`
	Fields       inferredFields `json:"fields"`
	Rows         []logRow       `json:"rows"`
	Malformed    []malformedLine `json:"malformed"`
	Metrics      ingestMetrics  `json:"metrics"`
	Live         bool           `json:"live"`
	RetentionCap int            `json:"retentionCap"`
}

func newFrontendHandler(data pageData, live *livePage) http.Handler {
	mux := http.NewServeMux()

	if live != nil {
		mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			ch := live.subscribe()
			defer live.unsubscribe(ch)
			for {
				select {
				case <-r.Context().Done():
					return
				case ev := <-ch:
					b, err := json.Marshal(ev)
					if err != nil {
						continue
					}
					if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
						return
					}
					flusher.Flush()
				}
			}
		})
	}
	mux.HandleFunc("/api/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		page := data
		if live != nil {
			page = live.snapshot()
		}
		resp := bootstrapPayload{
			Path:         page.Path,
			Fields:       page.Fields,
			Rows:         page.Rows,
			Malformed:    page.Malformed,
			Metrics:      page.Metrics,
			Live:         page.Live,
			RetentionCap: page.RetentionCap,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	publicFS, err := fs.Sub(frontendAssets, "frontend/dist")
	if err != nil {
		// Fallback to legacy inline UI if embedded assets are unavailable.
		return newAppHandler(data, live)
	}
	fileServer := http.FileServer(http.FS(publicFS))

	mux.Handle("/assets/", fileServer)
	mux.Handle("/favicon.ico", fileServer)
	mux.Handle("/robots.txt", fileServer)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		f, err := publicFS.Open("index.html")
		if err != nil {
			http.Error(w, "frontend index missing", http.StatusInternalServerError)
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, f)
	})

	return mux
}

type streamEvent struct {
	Type      string         `json:"type"`
	Row       logRow         `json:"row"`
	Fields    inferredFields `json:"fields"`
	TotalRows int            `json:"totalRows"`
	Malformed *malformedLine `json:"malformed,omitempty"`
	Metrics   ingestMetrics  `json:"metrics"`
}

const (
	defaultRetainedLiveRows  = 500000
	maxRetainedLiveRows      = defaultRetainedLiveRows
	maxRetainedMalformedRows = 5000
)

type livePage struct {
	mu          sync.RWMutex
	page        pageData
	subscribers map[chan streamEvent]struct{}
	startedAt   time.Time
	totalRows   int
	maxRows     int
}

func newLivePage(base pageData) *livePage {
	base.Live = true
	capRows := base.RetentionCap
	if capRows <= 0 {
		capRows = defaultRetainedLiveRows
	}
	base.RetentionCap = capRows
	return &livePage{
		page:        base,
		subscribers: make(map[chan streamEvent]struct{}),
		startedAt:   time.Now(),
		totalRows:   len(base.Rows),
		maxRows:     capRows,
	}
}

func (l *livePage) snapshot() pageData {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows := make([]logRow, len(l.page.Rows))
	copy(rows, l.page.Rows)
	p := l.page
	p.Rows = rows
	return p
}

func (l *livePage) append(line int, rec map[string]any) {
	l.mu.Lock()
	if l.page.Fields.Timestamp == "" || l.page.Fields.Level == "" || l.page.Fields.Message == "" {
		next := inferFields([]map[string]any{rec})
		if l.page.Fields.Timestamp == "" {
			l.page.Fields.Timestamp = next.Timestamp
		}
		if l.page.Fields.Level == "" {
			l.page.Fields.Level = next.Level
		}
		if l.page.Fields.Message == "" {
			l.page.Fields.Message = next.Message
		}
	}

	rawJSONBytes, _ := json.Marshal(rec)
	prettyJSONBytes, _ := json.MarshalIndent(rec, "", "  ")
	row := logRow{
		Line:       line,
		Timestamp:  fieldValue(rec, l.page.Fields.Timestamp),
		Level:      fieldValue(rec, l.page.Fields.Level),
		Message:    fieldValue(rec, l.page.Fields.Message),
		Source:     fieldValue(rec, l.page.Fields.Source),
		Context:    fieldValue(rec, l.page.Fields.Context),
		RawJSON:    string(rawJSONBytes),
		PrettyJSON: string(prettyJSONBytes),
		SearchText: buildSearchText(line, rec, rawJSONBytes, l.page.Fields),
	}
	l.totalRows++
	l.page.Rows = append(l.page.Rows, row)
	if len(l.page.Rows) > l.maxRows {
		l.page.Rows = retainLatestRows(l.page.Rows, l.maxRows)
	}
	l.page.Metrics.Events = l.totalRows
	l.page.Metrics.EventsPerSec = calcEventsPerSec(l.page.Metrics.Events, l.startedAt)
	event := streamEvent{
		Type:      "append",
		Row:       row,
		Fields:    l.page.Fields,
		TotalRows: l.totalRows,
		Metrics:   l.page.Metrics,
	}
	subs := make([]chan streamEvent, 0, len(l.subscribers))
	for ch := range l.subscribers {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	dropped := 0
	for _, ch := range subs {
		select {
		case ch <- event:
		default:
			dropped++
		}
	}
	if dropped == 0 {
		return
	}
	l.mu.Lock()
	l.page.Metrics.DroppedEvents += dropped
	l.page.Metrics.EventsPerSec = calcEventsPerSec(l.page.Metrics.Events, l.startedAt)
	l.mu.Unlock()
}

func (l *livePage) appendMalformed(lineNo int, raw, errMsg string) {
	l.mu.Lock()
	item := malformedLine{
		Line:  lineNo,
		Raw:   raw,
		Error: errMsg,
	}
	l.page.Malformed = append(l.page.Malformed, item)
	if len(l.page.Malformed) > maxRetainedMalformedRows {
		l.page.Malformed = retainLatestMalformed(l.page.Malformed, maxRetainedMalformedRows)
	}
	l.page.Metrics.ParseErrors++
	l.page.Metrics.EventsPerSec = calcEventsPerSec(l.page.Metrics.Events, l.startedAt)
	event := streamEvent{
		Type:      "malformed",
		Malformed: &item,
		Metrics:   l.page.Metrics,
	}
	subs := make([]chan streamEvent, 0, len(l.subscribers))
	for ch := range l.subscribers {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	dropped := 0
	for _, ch := range subs {
		select {
		case ch <- event:
		default:
			dropped++
		}
	}
	if dropped == 0 {
		return
	}
	l.mu.Lock()
	l.page.Metrics.DroppedEvents += dropped
	l.page.Metrics.EventsPerSec = calcEventsPerSec(l.page.Metrics.Events, l.startedAt)
	l.mu.Unlock()
}

func retainLatestRows(rows []logRow, max int) []logRow {
	if max <= 0 || len(rows) <= max {
		return rows
	}
	trimmed := make([]logRow, max)
	copy(trimmed, rows[len(rows)-max:])
	return trimmed
}

func retainLatestMalformed(rows []malformedLine, max int) []malformedLine {
	if max <= 0 || len(rows) <= max {
		return rows
	}
	trimmed := make([]malformedLine, max)
	copy(trimmed, rows[len(rows)-max:])
	return trimmed
}

func (l *livePage) subscribe() chan streamEvent {
	ch := make(chan streamEvent, 128)
	l.mu.Lock()
	l.subscribers[ch] = struct{}{}
	l.mu.Unlock()
	return ch
}

func (l *livePage) unsubscribe(ch chan streamEvent) {
	l.mu.Lock()
	delete(l.subscribers, ch)
	l.mu.Unlock()
	close(ch)
}

func newAppHandler(data pageData, live *livePage) http.Handler {
	tmpl := template.Must(template.New("page").Funcs(template.FuncMap{
		"levelClass": levelClass,
	}).Parse(pageTemplate))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page := data
		if live != nil {
			page = live.snapshot()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, page); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	if live != nil {
		mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			ch := live.subscribe()
			defer live.unsubscribe(ch)

			notify := r.Context().Done()
			for {
				select {
				case <-notify:
					return
				case ev := <-ch:
					b, err := json.Marshal(ev)
					if err != nil {
						continue
					}
					if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
						return
					}
					flusher.Flush()
				}
			}
		})
	}
	return mux
}

func levelClass(level string) string {
	v := strings.ToLower(strings.TrimSpace(level))
	switch {
	case strings.Contains(v, "error"), strings.Contains(v, "fatal"), strings.Contains(v, "panic"):
		return "severity-error"
	case strings.Contains(v, "warn"):
		return "severity-warn"
	case strings.Contains(v, "debug"), strings.Contains(v, "trace"):
		return "severity-debug"
	case v != "":
		return "severity-info"
	default:
		return ""
	}
}

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sift</title>
<style>
:root {
  color-scheme: dark;
  --bg: #070b14;
  --bg-2: #0b1320;
  --panel: rgba(14, 21, 36, 0.88);
  --panel-2: rgba(18, 27, 46, 0.95);
  --panel-border: rgba(148, 163, 184, 0.18);
  --panel-border-strong: rgba(148, 163, 184, 0.26);
  --text: #e6edf8;
  --muted: #9fb0ca;
  --muted-2: #7c8ca6;
  --accent: #66d9ff;
  --accent-2: #3b82f6;
  --good: #34d399;
  --warn: #fbbf24;
  --bad: #fb7185;
  --shadow: 0 20px 60px rgba(0,0,0,.35);
}
* { box-sizing: border-box; }
html, body { min-height: 100%; }
body {
  margin: 0;
  font: 14px/1.45 "JetBrains Mono", "IBM Plex Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
  color: var(--text);
  background:
    radial-gradient(900px 500px at 10% -10%, rgba(59,130,246,.22), transparent 65%),
    radial-gradient(700px 420px at 95% 0%, rgba(20,184,166,.16), transparent 65%),
    radial-gradient(900px 700px at 50% 120%, rgba(99,102,241,.08), transparent 60%),
    linear-gradient(180deg, var(--bg) 0%, #05070d 100%);
}
body::before {
  content: "";
  position: fixed;
  inset: 0;
  pointer-events: none;
  opacity: .22;
  background-image:
    linear-gradient(rgba(148,163,184,.10) 1px, transparent 1px),
    linear-gradient(90deg, rgba(148,163,184,.10) 1px, transparent 1px);
  background-size: 22px 22px;
  mask-image: radial-gradient(circle at 50% 20%, black 35%, transparent 90%);
}
header {
  position: sticky;
  top: 0;
  z-index: 20;
  padding: 16px 22px;
  background: linear-gradient(180deg, rgba(7,11,20,.88), rgba(7,11,20,.58));
  backdrop-filter: blur(14px);
  border-bottom: 1px solid rgba(148,163,184,.12);
  box-shadow: 0 8px 30px rgba(0,0,0,.18);
}
header strong {
  display: block;
  margin-bottom: 4px;
  font-size: 15px;
  letter-spacing: .04em;
  text-transform: uppercase;
}
header small { color: var(--muted); }
header .header-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
header .brand {
  display: flex;
  align-items: center;
  gap: 12px;
}
header .brand-mark {
  width: 32px;
  height: 32px;
  border-radius: 9px;
  display: grid;
  place-items: center;
  font-weight: 700;
  color: #d9efff;
  background:
    linear-gradient(180deg, rgba(59,130,246,.28), rgba(14,165,233,.18)),
    rgba(9,14,24,.95);
  border: 1px solid rgba(148,163,184,.18);
  box-shadow: inset 0 1px 0 rgba(255,255,255,.06);
}
header .header-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--muted);
  font-size: 12px;
}
header .header-pill {
  border: 1px solid rgba(148,163,184,.14);
  border-radius: 999px;
  padding: 5px 8px;
  background: rgba(11,17,29,.6);
}
body > .app-shell {
  display: grid;
  grid-template-columns: 52px 1fr;
  max-width: 1700px;
  margin: 0 auto;
  padding: 0 10px 10px;
  gap: 10px;
}
.side-rail {
  position: sticky;
  top: 76px;
  align-self: start;
  height: calc(100vh - 88px);
  border-radius: 14px;
  border: 1px solid var(--panel-border);
  background: linear-gradient(180deg, rgba(10,15,26,.9), rgba(8,12,20,.96));
  box-shadow: var(--shadow);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 10px 6px;
}
.rail-slot {
  width: 28px;
  height: 28px;
  border-radius: 9px;
  border: 1px solid rgba(148,163,184,.14);
  background: rgba(12,19,33,.78);
  color: #c3d5f2;
  display: grid;
  place-items: center;
  position: relative;
  cursor: pointer;
  padding: 0;
}
.rail-slot:hover { border-color: rgba(148,163,184,.24); background: rgba(16,24,41,.9); }
.rail-slot:focus-visible {
  outline: none;
  box-shadow: 0 0 0 3px rgba(59,130,246,.18);
  border-color: rgba(96,165,250,.32);
}
.rail-slot.active {
  background: linear-gradient(180deg, rgba(59,130,246,.22), rgba(14,165,233,.16));
  border-color: rgba(96,165,250,.30);
  color: #d9efff;
}
.rail-slot::before,
.rail-slot::after {
  content: "";
  position: absolute;
  border-radius: 999px;
  background: currentColor;
  opacity: .9;
}
.rail-slot.rail-grid::before { width: 12px; height: 2px; top: 9px; left: 8px; }
.rail-slot.rail-grid::after { width: 12px; height: 2px; top: 16px; left: 8px; }
.rail-slot.rail-filter::before { width: 12px; height: 2px; top: 8px; left: 8px; }
.rail-slot.rail-filter::after { width: 8px; height: 2px; top: 16px; left: 10px; }
.rail-slot.rail-live::before { width: 8px; height: 8px; top: 10px; left: 10px; border-radius: 50%; box-shadow: 0 0 0 3px rgba(96,165,250,.12); }
.rail-slot.rail-live::after { width: 14px; height: 14px; top: 7px; left: 7px; border: 1.5px solid currentColor; background: transparent; opacity: .4; }
.rail-slot.rail-bookmark::before { width: 10px; height: 12px; top: 7px; left: 9px; border-radius: 2px 2px 1px 1px; }
.rail-slot.rail-bookmark::after { width: 4px; height: 4px; top: 15px; left: 12px; background: rgba(12,19,33,.9); transform: rotate(45deg); border-radius: 0; }
.rail-slot.rail-error::before { width: 2px; height: 10px; top: 7px; left: 13px; }
.rail-slot.rail-error::after { width: 2px; height: 2px; top: 20px; left: 13px; }
.rail-slot.rail-detail::before { width: 12px; height: 9px; top: 8px; left: 8px; border-radius: 3px; border: 1.5px solid currentColor; background: transparent; }
.rail-slot.rail-detail::after { width: 6px; height: 1.5px; top: 20px; left: 11px; }
.rail-sep {
  width: 22px;
  height: 1px;
  background: rgba(148,163,184,.14);
  margin: 2px 0;
}
.rail-bottom {
  margin-top: auto;
  width: 100%;
  display: grid;
  place-items: center;
  padding-top: 6px;
}
.rail-badge {
  width: 28px;
  height: 20px;
  border-radius: 7px;
  border: 1px solid rgba(148,163,184,.16);
  background: rgba(11,17,29,.7);
  display: grid;
  place-items: center;
  color: var(--muted);
  font-size: 9px;
  font-weight: 700;
  letter-spacing: .04em;
}
main {
  padding: 18px;
  display: grid;
  gap: 14px;
  max-width: 1600px;
  margin: 0 auto;
}
.meta {
  display: grid;
  gap: 10px;
  grid-template-columns: repeat(4, minmax(220px, 1fr));
}
.meta > div {
  background: linear-gradient(180deg, rgba(17,24,39,.84), rgba(9,14,24,.9));
  border: 1px solid var(--panel-border);
  border-radius: 14px;
  padding: 12px 14px;
  box-shadow: inset 0 1px 0 rgba(255,255,255,.02);
}
.meta > div strong { color: #dbe8ff; }
.meta > div:nth-child(1) {
  grid-column: 1 / -1;
  position: relative;
  overflow: hidden;
  border-color: rgba(102,217,255,.18);
  background:
    linear-gradient(135deg, rgba(17,24,39,.95), rgba(12,20,34,.92)),
    radial-gradient(circle at 88% 18%, rgba(102,217,255,.16), transparent 55%);
}
.meta > div:nth-child(1)::after {
  content: "";
  position: absolute;
  right: 14px;
  top: 12px;
  width: 180px;
  height: 42px;
  border-radius: 999px;
  background: linear-gradient(90deg, rgba(59,130,246,.0), rgba(59,130,246,.18), rgba(102,217,255,.12));
  filter: blur(12px);
}
.toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  align-items: center;
  padding: 12px;
  border-radius: 16px;
  background: linear-gradient(180deg, rgba(12,18,31,.88), rgba(10,15,26,.94));
  border: 1px solid var(--panel-border);
  box-shadow: var(--shadow);
}
.toolbar::before {
  content: "Explore";
  display: inline-flex;
  align-items: center;
  justify-content: center;
  height: 34px;
  padding: 0 10px;
  border-radius: 10px;
  border: 1px solid rgba(148,163,184,.14);
  background: rgba(10,15,26,.8);
  color: #d5e6ff;
  font-size: 12px;
  letter-spacing: .04em;
  text-transform: uppercase;
}
.query-label {
  display: flex;
  align-items: center;
  gap: 8px;
  background: rgba(13, 20, 34, 0.94);
  border: 1px solid var(--panel-border-strong);
  border-radius: 12px;
  padding: 10px 12px;
  box-shadow: inset 0 1px 0 rgba(255,255,255,.02);
}
.query-label:focus-within {
  border-color: rgba(102,217,255,.45);
  box-shadow: 0 0 0 3px rgba(59,130,246,.14), inset 0 1px 0 rgba(255,255,255,.03);
}
.query-label span { color: var(--muted); font-weight: 600; letter-spacing: .02em; }
.query-input, .filter-input {
  min-width: 280px;
  border: 0;
  outline: none;
  background: transparent;
  font: inherit;
  color: var(--text);
}
.query-input { width: min(420px, 70vw); }
.filter-input { width: min(560px, 75vw); }
.query-input::placeholder, .filter-input::placeholder { color: var(--muted-2); }
.query-help {
  flex-basis: 100%;
  color: var(--muted);
  margin: 2px 2px 0;
  line-height: 1.5;
}
.query-help code {
  background: rgba(30,41,59,.7);
  border: 1px solid rgba(148,163,184,.14);
  border-radius: 6px;
  padding: 1px 5px;
  color: #dbe8ff;
}
.filter-chips { display: flex; flex-wrap: wrap; gap: 6px; flex-basis: 100%; }
.filter-chip {
  background: linear-gradient(180deg, rgba(30,64,175,.22), rgba(37,99,235,.12));
  color: #bfdbfe;
  border: 1px solid rgba(96,165,250,.28);
  border-radius: 999px;
  padding: 4px 9px;
  font-size: 12px;
}
.result-count {
  color: var(--muted);
  background: rgba(11,17,29,.72);
  border: 1px solid var(--panel-border);
  border-radius: 999px;
  padding: 7px 10px;
}
.result-count strong { color: #f8fbff; }
.bookmark-controls { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
.bookmark-list { display: flex; flex-wrap: wrap; gap: 6px; }
.bookmark-pill { max-width: 320px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.column-controls { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; flex-basis: 100%; }
.column-controls label {
  font-size: 12px;
  color: var(--muted);
  display: inline-flex;
  gap: 5px;
  align-items: center;
  background: rgba(13,20,34,.75);
  border: 1px solid var(--panel-border);
  padding: 5px 8px;
  border-radius: 999px;
}
.column-controls input { accent-color: #38bdf8; }
.table-wrap {
  background: linear-gradient(180deg, rgba(12,18,31,.9), rgba(9,13,23,.96));
  border: 1px solid var(--panel-border);
  border-radius: 16px;
  overflow: auto;
  box-shadow: var(--shadow);
}
.results-shell {
  display: grid;
  gap: 10px;
}
.results-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 8px 10px;
  border-radius: 12px;
  border: 1px solid var(--panel-border);
  background: linear-gradient(180deg, rgba(11,17,29,.86), rgba(9,14,24,.94));
  box-shadow: var(--shadow);
}
.results-toolbar-left, .results-toolbar-right {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.toolbar-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border-radius: 999px;
  border: 1px solid rgba(148,163,184,.14);
  padding: 5px 8px;
  background: rgba(10,15,26,.72);
  color: var(--muted);
  font-size: 12px;
}
.toolbar-chip strong { color: #e5eefb; }
.chip-dot {
  width: 7px;
  height: 7px;
  border-radius: 999px;
  background: #60a5fa;
  box-shadow: 0 0 0 4px rgba(59,130,246,.10);
}
.histogram-panel {
  border-radius: 14px;
  border: 1px solid var(--panel-border);
  background: linear-gradient(180deg, rgba(10,16,27,.9), rgba(8,12,21,.96));
  box-shadow: var(--shadow);
  padding: 10px 12px 8px;
}
.histogram-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 8px;
}
.histogram-title {
  color: #dbe8ff;
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: .05em;
}
.histogram-legend {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.legend-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--muted);
  font-size: 11px;
}
.legend-swatch {
  width: 8px;
  height: 8px;
  border-radius: 3px;
}
.histogram-bars {
  height: 88px;
  display: grid;
  grid-auto-flow: column;
  grid-auto-columns: minmax(6px, 1fr);
  align-items: end;
  gap: 2px;
  border: 1px solid rgba(148,163,184,.08);
  border-radius: 10px;
  padding: 8px 8px 6px;
  background:
    linear-gradient(rgba(148,163,184,.08) 1px, transparent 1px),
    linear-gradient(180deg, rgba(7,10,18,.72), rgba(8,12,20,.9));
  background-size: 100% 20px, auto;
  background-position: 0 0, 0 0;
  overflow: hidden;
}
.histogram-bar {
  height: 2px;
  min-height: 2px;
  border-radius: 999px 999px 2px 2px;
  position: relative;
  display: grid;
  align-items: end;
  gap: 1px;
}
.histogram-stack {
  width: 100%;
  border-radius: 2px;
  min-height: 1px;
}
.histogram-stack.info { background: rgba(96,165,250,.88); }
.histogram-stack.warn { background: rgba(251,191,36,.88); }
.histogram-stack.error { background: rgba(251,113,133,.9); }
.histogram-stack.other { background: rgba(167,139,250,.85); }
table { border-collapse: separate; border-spacing: 0; width: 100%; min-width: 1040px; }
th, td {
  padding: 4px 8px;
  border-bottom: 1px solid rgba(148,163,184,.08);
  vertical-align: top;
  text-align: left;
}
th {
  position: sticky;
  top: 0;
  background: rgba(10,15,26,.95);
  backdrop-filter: blur(8px);
  font-weight: 600;
  color: #c8d6ef;
  text-transform: uppercase;
  letter-spacing: .04em;
  font-size: 10px;
  border-bottom-color: rgba(148,163,184,.16);
}
td { color: #e4edf9; font-size: 12px; line-height: 1.25; }
td.msg { white-space: pre-wrap; word-break: break-word; max-width: 720px; line-height: 1.2; }
td.payload { min-width: 220px; }
.copy-json {
  border: 1px solid rgba(148,163,184,.18);
  border-radius: 9px;
  background: linear-gradient(180deg, rgba(24,33,51,.95), rgba(15,22,35,.95));
  color: #d7e3f7;
  padding: 4px 8px;
  font: inherit;
  cursor: pointer;
  transition: border-color .12s ease, background .12s ease, transform .12s ease;
}
.copy-json:hover {
  border-color: rgba(102,217,255,.32);
  background: linear-gradient(180deg, rgba(28,38,58,.98), rgba(17,24,39,.98));
}
.copy-json:active { transform: translateY(1px); }
.inline-action {
  border: 0;
  background: transparent;
  color: #9ec5ff;
  padding: 0;
  font-size: 11px;
  line-height: 1.1;
  text-decoration: none;
  border-radius: 4px;
}
.inline-action:hover {
  color: #cfe6ff;
  background: transparent;
  border: 0;
  text-decoration: underline;
}
.inline-action:focus-visible {
  outline: none;
  box-shadow: 0 0 0 2px rgba(59,130,246,.2);
}
.copy-state { color: var(--muted-2); font-size: 12px; margin-left: 6px; }
pre.payload-json {
  margin: 6px 0 0;
  padding: 10px;
  background: rgba(8,12,20,.72);
  border: 1px solid rgba(148,163,184,.12);
  border-radius: 8px;
  color: #a8bad6;
  white-space: pre-wrap;
  word-break: break-word;
}
.log-row {
  cursor: pointer;
  transition: background-color .08s ease, box-shadow .08s ease;
}
.log-row:hover td { background: rgba(18, 28, 46, 0.88); }
.log-row td:first-child {
  color: #9ab2d8;
  font-variant-numeric: tabular-nums;
  width: 64px;
}
.log-row td[data-col="timestamp"] { color: #cad8ee; white-space: nowrap; font-variant-numeric: tabular-nums; }
.log-row td[data-col="source"], .log-row td[data-col="context"] { color: #a9bad4; }
.log-row.is-selected td {
  background: rgba(29, 78, 216, 0.18);
  box-shadow: inset 0 1px 0 rgba(147,197,253,.18), inset 0 -1px 0 rgba(147,197,253,.16);
}
.log-row.severity-error td:first-child,
.log-row.severity-error td[data-col="level"] { color: #fecdd3; }
.log-row.severity-warn td:first-child,
.log-row.severity-warn td[data-col="level"] { color: #fde68a; }
.log-row.severity-info td:first-child,
.log-row.severity-info td[data-col="level"] { color: #bfdbfe; }
.log-row.severity-debug td:first-child,
.log-row.severity-debug td[data-col="level"] { color: #ddd6fe; }
td[data-col="level"] {
  font-weight: 700;
  letter-spacing: .03em;
}
.level-badge {
  display: inline-flex;
  align-items: center;
  border-radius: 999px;
  padding: 1px 6px;
  font-size: 10px;
  line-height: 1.1;
  border: 1px solid rgba(148,163,184,.16);
  background: rgba(15,22,35,.72);
  color: #dbe8ff;
}
.level-badge.error { color: #fecdd3; border-color: rgba(251,113,133,.25); background: rgba(127,29,29,.16); }
.level-badge.warn { color: #fde68a; border-color: rgba(251,191,36,.22); background: rgba(120,53,15,.16); }
.level-badge.info { color: #bfdbfe; border-color: rgba(96,165,250,.22); background: rgba(30,58,138,.14); }
.level-badge.debug { color: #ddd6fe; border-color: rgba(167,139,250,.22); background: rgba(76,29,149,.12); }
.cell-hidden { display: none; }
#malformed-panel {
  margin-top: 4px;
  border-radius: 14px;
  border: 1px solid rgba(251,113,133,.16);
  background: linear-gradient(180deg, rgba(20,11,16,.82), rgba(14,10,15,.94));
  box-shadow: var(--shadow);
  padding: 8px 10px 12px;
}
#malformed-panel > summary {
  cursor: pointer;
  color: #fecdd3;
  list-style: none;
  font-weight: 600;
}
#malformed-panel > summary::-webkit-details-marker { display: none; }
#malformed-panel code {
  color: #fca5a5;
  background: rgba(69,10,10,.3);
  border: 1px solid rgba(248,113,113,.14);
  border-radius: 6px;
  padding: 1px 4px;
}
.drawer-backdrop {
  position: fixed;
  inset: 0;
  background: rgba(3,6,11,0.62);
  backdrop-filter: blur(4px);
  opacity: 0;
  pointer-events: none;
  transition: opacity .14s ease;
}
.drawer-backdrop.open { opacity: 1; pointer-events: auto; }
.detail-drawer {
  position: fixed;
  top: 0;
  right: 0;
  height: 100vh;
  width: min(620px, 96vw);
  background: linear-gradient(180deg, rgba(11,17,29,.98), rgba(7,11,19,.99));
  border-left: 1px solid rgba(148,163,184,.16);
  box-shadow: -20px 0 60px rgba(0,0,0,.45);
  transform: translateX(100%);
  transition: transform .14s ease;
  display: flex;
  flex-direction: column;
}
.detail-drawer.open { transform: translateX(0); }
.drawer-header {
  padding: 14px 16px;
  border-bottom: 1px solid rgba(148,163,184,.10);
  display: flex;
  justify-content: space-between;
  gap: 8px;
  align-items: center;
  background: rgba(10,15,26,.72);
}
.drawer-header strong { letter-spacing: .03em; }
.drawer-meta {
  padding: 12px 16px;
  display: grid;
  grid-template-columns: 110px 1fr;
  gap: 7px 10px;
  border-bottom: 1px solid rgba(148,163,184,.08);
}
.drawer-meta dt { color: var(--muted); }
.drawer-meta dd { margin: 0; word-break: break-word; color: #e5eefb; }
.drawer-controls {
  padding: 12px 16px;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
  border-bottom: 1px solid rgba(148,163,184,.08);
}
.drawer-controls input {
  flex: 1 1 220px;
  border: 1px solid rgba(148,163,184,.18);
  border-radius: 9px;
  padding: 8px 10px;
  font: inherit;
  color: var(--text);
  background: rgba(11,17,29,.78);
  outline: none;
}
.drawer-controls input:focus {
  border-color: rgba(102,217,255,.34);
  box-shadow: 0 0 0 3px rgba(56,189,248,.10);
}
.drawer-json {
  flex: 1;
  overflow: auto;
  padding: 14px 16px;
  margin: 0;
  white-space: pre-wrap;
  word-break: break-word;
  background:
    linear-gradient(180deg, rgba(8,12,20,.86), rgba(7,10,18,.94)),
    linear-gradient(90deg, rgba(148,163,184,.03) 1px, transparent 1px);
  background-size: auto, 18px 18px;
  color: #c7d8f1;
}
small { color: var(--muted); }
@media (max-width: 980px) {
  body > .app-shell {
    grid-template-columns: 1fr;
    padding: 0;
    gap: 0;
  }
  .side-rail {
    display: none;
  }
  main { padding: 12px; }
  .meta { grid-template-columns: 1fr; }
  .meta > div:nth-child(1) { grid-column: auto; }
  .query-input, .filter-input { min-width: 180px; width: min(100%, 100vw); }
  .toolbar { padding: 10px; }
  .query-label { width: 100%; }
  .query-label input { width: 100%; min-width: 0; }
  .bookmark-controls, .column-controls { flex-basis: 100%; }
  .detail-drawer { width: 100vw; }
}
</style>
</head>
<body>
<header>
  <div class="header-row">
    <div class="brand">
      <div class="brand-mark">SF</div>
      <div>
        <strong>Sift</strong>
        <small>Local JSONL viewer</small>
      </div>
    </div>
    <div class="header-actions">
      <span class="header-pill">Local</span>
      <span class="header-pill">JSON / JSONL</span>
      <span class="header-pill">Keyboard-first</span>
    </div>
  </div>
</header>
<div class="app-shell">
  <aside class="side-rail" aria-label="Navigation">
    <button id="rail-explore" type="button" class="rail-slot rail-grid active" title="Explore" aria-label="Explore"></button>
    <button id="rail-filters" type="button" class="rail-slot rail-filter" title="Filters" aria-label="Filters"></button>
    <button id="rail-stream" type="button" class="rail-slot rail-live" title="Stream" aria-label="Stream"></button>
    <button id="rail-bookmarks" type="button" class="rail-slot rail-bookmark" title="Bookmarks" aria-label="Bookmarks"></button>
    <div class="rail-sep"></div>
    <button id="rail-errors" type="button" class="rail-slot rail-error" title="Errors" aria-label="Errors"></button>
    <button id="rail-details" type="button" class="rail-slot rail-detail" title="Details" aria-label="Details"></button>
    <div class="rail-bottom"><div class="rail-badge" aria-label="Local app">AU</div></div>
  </aside>
<main>
  <div class="meta">
    <div><strong>File:</strong> {{.Path}}</div>
    <div><strong>Inferred fields:</strong> timestamp={{if .Fields.Timestamp}}{{.Fields.Timestamp}}{{else}}-{{end}}, level={{if .Fields.Level}}{{.Fields.Level}}{{else}}-{{end}}, message={{if .Fields.Message}}{{.Fields.Message}}{{else}}-{{end}}, source={{if .Fields.Source}}{{.Fields.Source}}{{else}}-{{end}}, context={{if .Fields.Context}}{{.Fields.Context}}{{else}}-{{end}}</div>
    <div><strong>Rows:</strong> <span id="row-count">{{len .Rows}}</span></div>
    <div><strong>Ingest health:</strong> <span id="events-per-sec">{{printf "%.1f" .Metrics.EventsPerSec}}</span> events/sec, <span id="parse-error-count">{{.Metrics.ParseErrors}}</span> parse errors, <span id="dropped-event-count">{{.Metrics.DroppedEvents}}</span> dropped events</div>
  </div>
  <div class="toolbar">
    <label class="query-label" for="search-query">
      <span>Search</span>
      <input id="search-query" class="query-input" type="search" placeholder='Full-text search ("/" focus, Esc clear)' autocomplete="off" spellcheck="false" autofocus>
    </label>
    <label class="query-label" for="filter-query">
      <span>Filters</span>
      <input id="filter-query" class="filter-input" type="search" placeholder='field=value field!=value field~=value field:* field=low..high' autocomplete="off" spellcheck="false">
    </label>
    <input id="query" class="query-input" type="hidden" aria-hidden="true">
    <small class="query-help">Search terms match all text. Filters support <code>field=value</code>, <code>field!=value</code>, <code>field~=value</code>, <code>field:*</code>, <code>field=low..high</code>. Search and filters are ANDed.</small>
    <div id="filter-chip-list" class="filter-chips" aria-label="Active filter chips"></div>
    <div class="column-controls" aria-label="Column visibility">
      <label><input type="checkbox" data-column="timestamp" checked> Timestamp</label>
      <label><input type="checkbox" data-column="level" checked> Level</label>
      <label><input type="checkbox" data-column="source" checked> Source</label>
      <label><input type="checkbox" data-column="context" checked> Context</label>
      <label><input type="checkbox" data-column="message" checked> Message</label>
      <label><input type="checkbox" data-column="payload" checked> Payload</label>
    </div>
    <div class="bookmark-controls">
      <button id="bookmark-save" type="button" class="copy-json">Save bookmark</button>
      <button id="bookmark-clear" type="button" class="copy-json" hidden>Clear bookmarks</button>
      <div id="bookmark-list" class="bookmark-list" aria-label="Saved query bookmarks"></div>
    </div>
    <div class="result-count"><strong id="match-count">{{len .Rows}}</strong> / <span id="total-count">{{len .Rows}}</span> rows</div>
    {{if .Live}}
    <button id="stream-toggle" type="button" class="copy-json" aria-pressed="false">Pause stream</button>
    <div id="stream-pending" class="result-count" hidden>0 queued</div>
    {{end}}
  </div>
  <div class="results-shell">
    <div class="results-toolbar" aria-label="Results toolbar">
      <div class="results-toolbar-left">
        <span class="toolbar-chip"><span class="chip-dot"></span> <strong id="query-time-ms">0ms</strong> query time</span>
        <span class="toolbar-chip">Rows: <strong id="rows-total-chip">{{len .Rows}}</strong></span>
        <span class="toolbar-chip">Visible: <strong id="rows-visible-chip">{{len .Rows}}</strong></span>
      </div>
      <div class="results-toolbar-right">
        <span class="toolbar-chip">Columns <strong id="columns-visible-chip">6</strong></span>
        {{if .Live}}<span class="toolbar-chip">Mode <strong>live</strong></span>{{else}}<span class="toolbar-chip">Mode <strong>file</strong></span>{{end}}
      </div>
    </div>
    <div class="histogram-panel" aria-label="Event distribution">
      <div class="histogram-header">
        <div class="histogram-title">Event Distribution</div>
        <div class="histogram-legend">
          <span class="legend-item"><span class="legend-swatch" style="background:rgba(96,165,250,.88)"></span>Info</span>
          <span class="legend-item"><span class="legend-swatch" style="background:rgba(251,191,36,.88)"></span>Warn</span>
          <span class="legend-item"><span class="legend-swatch" style="background:rgba(251,113,133,.9)"></span>Error</span>
          <span class="legend-item"><span class="legend-swatch" style="background:rgba(167,139,250,.85)"></span>Other</span>
        </div>
      </div>
      <div id="histogram-bars" class="histogram-bars" role="img" aria-label="Histogram of visible events"></div>
    </div>
  <div class="table-wrap">
    <table>
      <thead>
        <tr><th>#</th><th data-col="timestamp">Timestamp</th><th data-col="level">Level</th><th data-col="source">Source</th><th data-col="context">Context</th><th data-col="message">Message</th><th data-col="payload">Payload</th></tr>
      </thead>
      <tbody id="rows-body">
        {{range .Rows}}
        <tr class="log-row {{levelClass .Level}}" data-search="{{.SearchText}}" data-raw-json="{{.RawJSON}}" data-line="{{.Line}}" data-timestamp="{{.Timestamp}}" data-level="{{.Level}}" data-source="{{.Source}}" data-context="{{.Context}}" data-message="{{.Message}}">
          <td>{{.Line}}</td>
          <td data-col="timestamp">{{if .Timestamp}}{{.Timestamp}}{{else}}-{{end}}</td>
          <td data-col="level"><span class="level-badge {{levelClass .Level}}">{{if .Level}}{{.Level}}{{else}}-{{end}}</span></td>
          <td data-col="source">{{if .Source}}{{.Source}}{{else}}-{{end}}</td>
          <td data-col="context">{{if .Context}}{{.Context}}{{else}}-{{end}}</td>
          <td class="msg" data-col="message">{{if .Message}}{{.Message}}{{else}}-{{end}}</td>
          <td class="payload" data-col="payload"><button type="button" class="copy-json inline-action open-drawer" data-raw-json="{{.RawJSON}}">Details &gt;</button><span class="copy-state" aria-live="polite"></span><pre class="payload-json" hidden>{{.PrettyJSON}}</pre></td>
        </tr>
        {{else}}
        <tr id="no-rows-row"><td colspan="7">No rows found.</td></tr>
        {{end}}
        <tr id="no-results-row" hidden><td colspan="7">No matching rows.</td></tr>
      </tbody>
    </table>
  </div>
  </div>
  <details id="malformed-panel" {{if .Malformed}}open{{end}} style="margin-top:10px;">
    <summary>Malformed JSON lines (<span id="malformed-count">{{len .Malformed}}</span>)</summary>
    <div class="table-wrap" style="margin-top:8px;">
      <table>
        <thead>
          <tr><th>Line</th><th>Error</th><th>Raw line</th></tr>
        </thead>
        <tbody id="malformed-body">
          {{range .Malformed}}
          <tr class="malformed-row">
            <td>{{.Line}}</td>
            <td>{{.Error}}</td>
            <td><code>{{.Raw}}</code></td>
          </tr>
          {{else}}
          <tr id="no-malformed-row"><td colspan="3">No malformed lines.</td></tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </details>
</main>
</div>
<div id="drawer-backdrop" class="drawer-backdrop" hidden></div>
<aside id="detail-drawer" class="detail-drawer" aria-hidden="true" aria-label="Event details">
  <div class="drawer-header">
    <strong>Event details</strong>
    <button id="drawer-close" type="button" class="copy-json">Close</button>
  </div>
  <dl class="drawer-meta">
    <dt>Line</dt><dd id="drawer-line">-</dd>
    <dt>Timestamp</dt><dd id="drawer-timestamp">-</dd>
    <dt>Level</dt><dd id="drawer-level">-</dd>
    <dt>Source</dt><dd id="drawer-source">-</dd>
    <dt>Context</dt><dd id="drawer-context">-</dd>
    <dt>Message</dt><dd id="drawer-message">-</dd>
  </dl>
  <div class="drawer-controls">
    <button id="drawer-copy-raw" type="button" class="copy-json">Copy raw JSON</button>
    <input id="drawer-field-input" type="text" placeholder="field paths to copy (comma-separated)">
    <button id="drawer-copy-fields" type="button" class="copy-json">Copy selected fields</button>
    <span id="drawer-copy-state" class="copy-state" aria-live="polite"></span>
  </div>
  <pre id="drawer-json" class="drawer-json">{}</pre>
</aside>
<script>
var queryInput = document.getElementById("query");
var searchInput = document.getElementById("search-query");
var filterInput = document.getElementById("filter-query");
var filterChipList = document.getElementById("filter-chip-list");
var queryTimeMsEl = document.getElementById("query-time-ms");
var rowsTotalChip = document.getElementById("rows-total-chip");
var rowsVisibleChip = document.getElementById("rows-visible-chip");
var columnsVisibleChip = document.getElementById("columns-visible-chip");
var histogramBars = document.getElementById("histogram-bars");
var matchCount = document.getElementById("match-count");
var rowCount = document.getElementById("row-count");
var totalCount = document.getElementById("total-count");
var eventsPerSec = document.getElementById("events-per-sec");
var parseErrorCount = document.getElementById("parse-error-count");
var droppedEventCount = document.getElementById("dropped-event-count");
var noResultsRow = document.getElementById("no-results-row");
var noRowsRow = document.getElementById("no-rows-row");
var rowsBody = document.getElementById("rows-body");
var malformedCount = document.getElementById("malformed-count");
var malformedBody = document.getElementById("malformed-body");
var noMalformedRow = document.getElementById("no-malformed-row");
var rowsViewport = rowsBody ? rowsBody.closest(".table-wrap") : null;
var logRows = [];
var streamToggle = document.getElementById("stream-toggle");
var streamPending = document.getElementById("stream-pending");
var bookmarkSave = document.getElementById("bookmark-save");
var bookmarkClear = document.getElementById("bookmark-clear");
var bookmarkList = document.getElementById("bookmark-list");
var columnToggles = Array.prototype.slice.call(document.querySelectorAll('.column-controls input[type="checkbox"][data-column]'));
var drawer = document.getElementById("detail-drawer");
var drawerBackdrop = document.getElementById("drawer-backdrop");
var drawerClose = document.getElementById("drawer-close");
var drawerLine = document.getElementById("drawer-line");
var drawerTimestamp = document.getElementById("drawer-timestamp");
var drawerLevel = document.getElementById("drawer-level");
var drawerSource = document.getElementById("drawer-source");
var drawerContext = document.getElementById("drawer-context");
var drawerMessage = document.getElementById("drawer-message");
var drawerJSON = document.getElementById("drawer-json");
var drawerCopyRaw = document.getElementById("drawer-copy-raw");
var drawerFieldInput = document.getElementById("drawer-field-input");
var drawerCopyFields = document.getElementById("drawer-copy-fields");
var drawerCopyState = document.getElementById("drawer-copy-state");
var malformedPanel = document.getElementById("malformed-panel");
var railExplore = document.getElementById("rail-explore");
var railFilters = document.getElementById("rail-filters");
var railStream = document.getElementById("rail-stream");
var railBookmarks = document.getElementById("rail-bookmarks");
var railErrors = document.getElementById("rail-errors");
var railDetails = document.getElementById("rail-details");
var livePaused = false;
var pendingEvents = [];
var allRows = [];
var filteredRows = [];
var currentQueryParsed = { terms: [], fieldClauses: [] };
var virtualTopSpacer = null;
var virtualBottomSpacer = null;
var virtualRenderQueued = false;
var lastRenderedRange = { start: -1, end: -1, total: -1 };
var selectedMatchIndex = -1;
var selectedRowLine = null;
var drawerRow = null;
var searchDebounceTimer = 0;
var UI_STATE_KEY = "sift-ui-state-v2";
var MAX_BOOKMARKS = 10;
var MAX_CLIENT_ROWS = {{if gt .RetentionCap 0}}{{.RetentionCap}}{{else}}500000{{end}};
var MAX_CLIENT_MALFORMED_ROWS = 5000;
var MAX_PENDING_EVENTS = 5000;
var VIRTUAL_ROW_HEIGHT = 30;
var VIRTUAL_OVERSCAN = 30;
var uiState = loadUIState();

function loadUIState() {
  var state = { search: "", filters: "", bookmarks: [], columns: {} };
  try {
    if (!window.localStorage) return state;
    var raw = window.localStorage.getItem(UI_STATE_KEY);
    if (!raw) return state;
    var parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return state;
    if (typeof parsed.search === "string") state.search = parsed.search;
    if (typeof parsed.filters === "string") state.filters = parsed.filters;
    if (Array.isArray(parsed.bookmarks)) {
      state.bookmarks = parsed.bookmarks.filter(function (item) {
        if (typeof item === "string" && item.trim() !== "") {
          return true;
        }
        return !!(item && typeof item === "object" && (typeof item.search === "string" || typeof item.filters === "string"));
      }).slice(0, MAX_BOOKMARKS);
    }
    if (parsed.columns && typeof parsed.columns === "object") state.columns = parsed.columns;
  } catch (_) {}
  return state;
}

function saveUIState() {
  try {
    if (!window.localStorage) return;
    window.localStorage.setItem(UI_STATE_KEY, JSON.stringify({
      search: uiState.search || "",
      filters: uiState.filters || "",
      bookmarks: Array.isArray(uiState.bookmarks) ? uiState.bookmarks.slice(0, MAX_BOOKMARKS) : [],
      columns: uiState.columns || {}
    }));
  } catch (_) {}
}

function syncQueryState() {
  if (searchInput) uiState.search = searchInput.value || "";
  if (filterInput) uiState.filters = filterInput.value || "";
  saveUIState();
}

function applyBookmark(bookmark) {
  if (!searchInput || !filterInput) return;
  var search = "";
  var filters = "";
  if (typeof bookmark === "string") {
    search = bookmark;
  } else if (bookmark && typeof bookmark === "object") {
    search = bookmark.search || "";
    filters = bookmark.filters || "";
  }
  searchInput.value = search;
  filterInput.value = filters;
  syncQueryInputs();
  syncQueryState();
  applySearch();
  renderFilterChips();
  searchInput.focus();
  searchInput.select();
}

function addBookmark() {
  if (!searchInput || !filterInput) return;
  var search = (searchInput.value || "").trim();
  var filters = (filterInput.value || "").trim();
  if (!search && !filters) return;
  var candidate = { search: search, filters: filters };
  var next = [candidate];
  (uiState.bookmarks || []).forEach(function (item) {
    var same = false;
    if (typeof item === "string") {
      same = item === candidate.search && candidate.filters === "";
      item = { search: item, filters: "" };
    } else if (item && typeof item === "object") {
      same = (item.search || "") === candidate.search && (item.filters || "") === candidate.filters;
    }
    if (!same && item && next.length < MAX_BOOKMARKS) next.push({ search: item.search || "", filters: item.filters || "" });
  });
  uiState.bookmarks = next;
  saveUIState();
  renderBookmarks();
}

function clearBookmarks() {
  uiState.bookmarks = [];
  saveUIState();
  renderBookmarks();
}

function renderBookmarks() {
  if (!bookmarkList) return;
  bookmarkList.innerHTML = "";
  var items = Array.isArray(uiState.bookmarks) ? uiState.bookmarks : [];
  if (bookmarkClear) bookmarkClear.hidden = items.length === 0;
  items.forEach(function (query) {
    if (typeof query === "string") query = { search: query, filters: "" };
    var label = ((query.search || "") + (query.filters ? " | " + query.filters : "")).trim() || "(empty)";
    var button = document.createElement("button");
    button.type = "button";
    button.className = "copy-json bookmark-pill";
    button.textContent = label;
    button.title = label;
    button.addEventListener("click", function () {
      applyBookmark(query);
    });
    bookmarkList.appendChild(button);
  });
}

function syncQueryInputs() {
  if (!queryInput) return;
  var search = searchInput ? (searchInput.value || "").trim() : "";
  var filters = filterInput ? (filterInput.value || "").trim() : "";
  queryInput.value = [search, filters].filter(function (part) { return part !== ""; }).join(" ");
}

function renderFilterChips() {
  if (!filterChipList) return;
  filterChipList.innerHTML = "";
  var parsed = parseQueryClauses(filterInput ? filterInput.value : "");
  parsed.fieldClauses.forEach(function (clause) {
    var chip = document.createElement("span");
    chip.className = "filter-chip";
    if (clause.type === "exists") chip.textContent = clause.field + ":*";
    else if (clause.type === "range") chip.textContent = clause.field + "=" + (clause.low || "") + ".." + (clause.high || "");
    else if (clause.type === "eq") chip.textContent = clause.field + "=" + (clause.value || "");
    else if (clause.type === "neq") chip.textContent = clause.field + "!=" + (clause.value || "");
    else chip.textContent = clause.field + "~=" + (clause.value || "");
    filterChipList.appendChild(chip);
  });
}

function tokenizeQuery(value) {
  var tokens = [];
  var token = "";
  var quote = "";
  for (var i = 0; i < value.length; i++) {
    var ch = value[i];
    if (quote) {
      if (ch === "\\" && i + 1 < value.length) {
        token += value[i + 1];
        i++;
        continue;
      }
      if (ch === quote) {
        quote = "";
        continue;
      }
      token += ch;
      continue;
    }
    if (ch === "\"" || ch === "'") {
      quote = ch;
      continue;
    }
    if (/\s/.test(ch)) {
      if (token) {
        tokens.push(token);
        token = "";
      }
      continue;
    }
    token += ch;
  }
  if (token) tokens.push(token);
  return tokens;
}

function parseQueryClauses(value) {
  var parsed = { terms: [], fieldClauses: [] };
  tokenizeQuery(value || "").forEach(function (token) {
    if (!token) return;
    if (token.length > 2 && token.slice(-2) === ":*" && token.indexOf(".") !== 0) {
      var existsField = token.slice(0, -2);
      if (existsField) {
        parsed.fieldClauses.push({ type: "exists", field: existsField });
        return;
      }
    }
    var op = "";
    var idx = -1;
    ["!=", "~=", "="].some(function (candidate) {
      var found = token.indexOf(candidate);
      if (found > 0) {
        op = candidate;
        idx = found;
        return true;
      }
      return false;
    });
    if (!op) {
      parsed.terms.push(token.toLowerCase());
      return;
    }
    var field = token.slice(0, idx);
    var rawValue = token.slice(idx + op.length);
    if (!field || rawValue === "") {
      parsed.terms.push(token.toLowerCase());
      return;
    }
    if (op === "=") {
      var rangeSep = rawValue.indexOf("..");
      if (rangeSep !== -1) {
        parsed.fieldClauses.push({
          type: "range",
          field: field,
          low: rawValue.slice(0, rangeSep),
          high: rawValue.slice(rangeSep + 2)
        });
        return;
      }
      parsed.fieldClauses.push({ type: "eq", field: field, value: rawValue });
      return;
    }
    if (op === "!=") {
      parsed.fieldClauses.push({ type: "neq", field: field, value: rawValue });
      return;
    }
    parsed.fieldClauses.push({ type: "contains", field: field, value: rawValue });
  });
  return parsed;
}

function rowAttr(row, name) {
  if (!row) return null;
  if (typeof row.getAttribute === "function") return row.getAttribute(name);
  if (row.attrs && Object.prototype.hasOwnProperty.call(row.attrs, name)) return row.attrs[name];
  return null;
}

function getRowPayload(row) {
  if (!row) return {};
  if (row._siftPayloadParsed) return row._siftPayload || {};
  row._siftPayloadParsed = true;
  try {
    row._siftPayload = JSON.parse(rowAttr(row, "data-raw-json") || "{}");
  } catch (_) {
    row._siftPayload = {};
  }
  return row._siftPayload;
}

function getObjectValueCaseInsensitive(obj, key) {
  if (!obj || typeof obj !== "object") return { ok: false, value: undefined };
  if (Object.prototype.hasOwnProperty.call(obj, key)) {
    return { ok: true, value: obj[key] };
  }
  var lower = String(key).toLowerCase();
  for (var k in obj) {
    if (Object.prototype.hasOwnProperty.call(obj, k) && String(k).toLowerCase() === lower) {
      return { ok: true, value: obj[k] };
    }
  }
  return { ok: false, value: undefined };
}

function rowFieldLookup(row, path) {
  var segments = String(path || "").split(".").filter(Boolean);
  if (segments.length === 0) return { ok: false, value: undefined };

  var metaKey = segments.length === 1 ? segments[0].toLowerCase() : "";
  if (metaKey === "line" || metaKey === "timestamp" || metaKey === "level" || metaKey === "source" || metaKey === "context" || metaKey === "message") {
    var attrValue = rowAttr(row, "data-" + metaKey);
    if (attrValue !== null) return { ok: true, value: attrValue };
  }

  var cur = getRowPayload(row);
  for (var i = 0; i < segments.length; i++) {
    var seg = segments[i];
    if (Array.isArray(cur)) {
      if (!/^\d+$/.test(seg)) return { ok: false, value: undefined };
      var index = Number(seg);
      if (index < 0 || index >= cur.length) return { ok: false, value: undefined };
      cur = cur[index];
      continue;
    }
    var found = getObjectValueCaseInsensitive(cur, seg);
    if (!found.ok) return { ok: false, value: undefined };
    cur = found.value;
  }
  return { ok: true, value: cur };
}

function normalizeFieldValue(value) {
  if (value === null || value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value);
  } catch (_) {
    return String(value);
  }
}

function parseNumberLike(value) {
  var n = Number(String(value).trim());
  return Number.isFinite(n) ? n : null;
}

function matchesRange(value, low, high) {
  var actual = normalizeFieldValue(value);
  var numActual = parseNumberLike(actual);
  var numLow = low === "" ? null : parseNumberLike(low);
  var numHigh = high === "" ? null : parseNumberLike(high);
  if (numActual !== null && (low === "" || numLow !== null) && (high === "" || numHigh !== null)) {
    if (numLow !== null && numActual < numLow) return false;
    if (numHigh !== null && numActual > numHigh) return false;
    return true;
  }
  var cmpActual = actual.toLowerCase();
  var cmpLow = String(low || "").toLowerCase();
  var cmpHigh = String(high || "").toLowerCase();
  if (cmpLow && cmpActual < cmpLow) return false;
  if (cmpHigh && cmpActual > cmpHigh) return false;
  return true;
}

function matchesFieldClause(row, clause) {
  var found = rowFieldLookup(row, clause.field);
  if (clause.type === "exists") {
    return found.ok && found.value !== null;
  }
  if (!found.ok) return false;

  var actual = normalizeFieldValue(found.value);
  var actualLower = actual.toLowerCase();
  var wanted = normalizeFieldValue(clause.value || "");
  var wantedLower = wanted.toLowerCase();

  if (clause.type === "eq") return actualLower === wantedLower;
  if (clause.type === "neq") return actualLower !== wantedLower;
  if (clause.type === "contains") return actualLower.indexOf(wantedLower) !== -1;
  if (clause.type === "range") return matchesRange(found.value, clause.low || "", clause.high || "");
  return true;
}

function normalizeRowData(input) {
  if (!input) return null;
  var rawLine = input.line;
  var attrs = {
    "data-search": String(input.searchText || ""),
    "data-raw-json": String(input.rawJSON || ""),
    "data-line": rawLine == null ? "" : String(rawLine),
    "data-timestamp": String(input.timestamp || ""),
    "data-level": String(input.level || ""),
    "data-source": String(input.source || ""),
    "data-context": String(input.context || ""),
    "data-message": String(input.message || "")
  };
  return {
    line: rawLine == null ? null : Number(rawLine),
    timestamp: input.timestamp || "",
    level: input.level || "",
    source: input.source || "",
    context: input.context || "",
    message: input.message || "",
    rawJSON: input.rawJSON || "",
    prettyJSON: input.prettyJSON || "",
    searchText: input.searchText || "",
    attrs: attrs,
    getAttribute: function (name) {
      return Object.prototype.hasOwnProperty.call(attrs, name) ? attrs[name] : null;
    }
  };
}

function hydrateInitialRowsFromDOM() {
  var initial = Array.prototype.slice.call(document.querySelectorAll("tr.log-row"));
  initial.forEach(function (tr) {
    var pre = tr.querySelector("pre.payload-json");
    var row = normalizeRowData({
      line: tr.getAttribute("data-line"),
      timestamp: tr.getAttribute("data-timestamp") || "",
      level: tr.getAttribute("data-level") || "",
      source: tr.getAttribute("data-source") || "",
      context: tr.getAttribute("data-context") || "",
      message: tr.getAttribute("data-message") || "",
      rawJSON: tr.getAttribute("data-raw-json") || "",
      prettyJSON: pre ? pre.textContent || "" : "",
      searchText: tr.getAttribute("data-search") || ""
    });
    if (row) allRows.push(row);
    if (tr.parentNode) tr.parentNode.removeChild(tr);
  });
}

function ensureVirtualSpacers() {
  if (!rowsBody || !noResultsRow) return;
  if (!virtualTopSpacer) {
    virtualTopSpacer = document.createElement("tr");
    virtualTopSpacer.id = "virtual-top-spacer";
    virtualTopSpacer.hidden = true;
    var topCell = document.createElement("td");
    topCell.colSpan = 7;
    topCell.style.padding = "0";
    topCell.style.border = "0";
    topCell.style.height = "0px";
    virtualTopSpacer.appendChild(topCell);
    rowsBody.insertBefore(virtualTopSpacer, noResultsRow);
  }
  if (!virtualBottomSpacer) {
    virtualBottomSpacer = document.createElement("tr");
    virtualBottomSpacer.id = "virtual-bottom-spacer";
    virtualBottomSpacer.hidden = true;
    var bottomCell = document.createElement("td");
    bottomCell.colSpan = 7;
    bottomCell.style.padding = "0";
    bottomCell.style.border = "0";
    bottomCell.style.height = "0px";
    virtualBottomSpacer.appendChild(bottomCell);
    rowsBody.insertBefore(virtualBottomSpacer, noResultsRow);
  }
}

function setSpacerHeight(spacer, px) {
  if (!spacer || !spacer.firstChild) return;
  var height = px > 0 ? px : 0;
  spacer.hidden = height === 0;
  spacer.firstChild.style.height = String(height) + "px";
}

function clearRenderedRows() {
  logRows.forEach(function (tr) {
    if (tr && tr.parentNode) tr.parentNode.removeChild(tr);
  });
  logRows = [];
}

function syncRowCounters() {
  if (rowCount) rowCount.textContent = String(allRows.length);
  if (totalCount) totalCount.textContent = String(allRows.length);
  if (rowsTotalChip) rowsTotalChip.textContent = String(allRows.length);
  if (noRowsRow) noRowsRow.hidden = allRows.length !== 0;
}

function rowMatchesParsed(row, parsed) {
  var haystack = rowAttr(row, "data-search") || "";
  var searchMatch = parsed.terms.every(function (term) {
    return haystack.indexOf(term) !== -1;
  });
  var fieldMatch = parsed.fieldClauses.every(function (clause) {
    return matchesFieldClause(row, clause);
  });
  return searchMatch && fieldMatch;
}

function queueVirtualRender() {
  if (virtualRenderQueued) return;
  virtualRenderQueued = true;
  window.requestAnimationFrame(function () {
    virtualRenderQueued = false;
    renderVirtualRows();
  });
}

function renderVirtualRows() {
  if (!rowsBody) return;
  ensureVirtualSpacers();
  var total = filteredRows.length;
  if (matchCount) matchCount.textContent = String(total);
  if (rowsVisibleChip) rowsVisibleChip.textContent = String(total);
  if (noResultsRow) noResultsRow.hidden = total !== 0 || allRows.length === 0;
  if (noRowsRow) noRowsRow.hidden = allRows.length !== 0;

  if (total === 0) {
    clearRenderedRows();
    setSpacerHeight(virtualTopSpacer, 0);
    setSpacerHeight(virtualBottomSpacer, 0);
    lastRenderedRange = { start: -1, end: -1, total: total };
    renderHistogram();
    return;
  }

  var viewport = rowsViewport || rowsBody.parentElement;
  var viewportHeight = viewport ? viewport.clientHeight : 0;
  var scrollTop = viewport ? viewport.scrollTop : 0;
  var start = Math.floor(scrollTop / VIRTUAL_ROW_HEIGHT) - VIRTUAL_OVERSCAN;
  if (start < 0) start = 0;
  var visibleCount = Math.ceil((viewportHeight || (VIRTUAL_ROW_HEIGHT * 20)) / VIRTUAL_ROW_HEIGHT) + (VIRTUAL_OVERSCAN * 2);
  if (visibleCount < 1) visibleCount = 1;
  var end = start + visibleCount;
  if (end > total) end = total;
  if (end <= start) end = Math.min(total, start + 1);

  if (lastRenderedRange.start === start && lastRenderedRange.end === end && lastRenderedRange.total === total) {
    setSpacerHeight(virtualTopSpacer, start * VIRTUAL_ROW_HEIGHT);
    setSpacerHeight(virtualBottomSpacer, (total - end) * VIRTUAL_ROW_HEIGHT);
    return;
  }

  clearRenderedRows();
  setSpacerHeight(virtualTopSpacer, start * VIRTUAL_ROW_HEIGHT);
  setSpacerHeight(virtualBottomSpacer, (total - end) * VIRTUAL_ROW_HEIGHT);

  var fragment = document.createDocumentFragment();
  for (var i = start; i < end; i++) {
    var tr = buildRowElement(filteredRows[i]);
    fragment.appendChild(tr);
    logRows.push(tr);
  }
  rowsBody.insertBefore(fragment, virtualBottomSpacer || noResultsRow);
  lastRenderedRange = { start: start, end: end, total: total };
  updateSelectedRowHighlight();
  renderHistogram();
}

function evictRowsIfNeeded() {
  if (allRows.length <= MAX_CLIENT_ROWS) return [];
  var overflow = allRows.length - MAX_CLIENT_ROWS;
  if (overflow <= 0) return [];
  return allRows.splice(0, overflow);
}

function applySearch() {
  var queryStarted = (window.performance && performance.now) ? performance.now() : Date.now();
  var parsed = parseQueryClauses(queryInput ? queryInput.value : "");
  currentQueryParsed = parsed;
  filteredRows = allRows.filter(function (row) {
    var searchMatch = parsed.terms.every(function (term) {
      return (rowAttr(row, "data-search") || "").indexOf(term) !== -1;
    });
    var fieldMatch = parsed.fieldClauses.every(function (clause) {
      return matchesFieldClause(row, clause);
    });
    return searchMatch && fieldMatch;
  });
  lastRenderedRange = { start: -1, end: -1, total: -1 };
  syncRowCounters();
  queueVirtualRender();
  var queryEnded = (window.performance && performance.now) ? performance.now() : Date.now();
  if (queryTimeMsEl) queryTimeMsEl.textContent = String(Math.max(0, Math.round(queryEnded - queryStarted))) + "ms";
  if (filteredRows.length === 0) {
    selectedMatchIndex = -1;
    selectedRowLine = null;
  } else if (selectedMatchIndex >= filteredRows.length) {
    selectedMatchIndex = filteredRows.length - 1;
    selectedRowLine = filteredRows[selectedMatchIndex] ? filteredRows[selectedMatchIndex].line : null;
  }
}

function updateSelectedRowHighlight() {
  Array.prototype.slice.call(document.querySelectorAll("tr.log-row.is-selected")).forEach(function (tr) {
    tr.classList.remove("is-selected");
  });
  if (selectedRowLine === null) return;
  var selector = 'tr.log-row[data-line="' + String(selectedRowLine).replace(/"/g, '\\"') + '"]';
  var tr = document.querySelector(selector);
  if (tr) tr.classList.add("is-selected");
}

function ensureSelectedVisible() {
  if (selectedRowLine === null || !rowsViewport) return;
  var tr = document.querySelector('tr.log-row[data-line="' + String(selectedRowLine).replace(/"/g, '\\"') + '"]');
  if (!tr) return;
  var trTop = tr.offsetTop;
  var trBottom = trTop + tr.offsetHeight;
  var vpTop = rowsViewport.scrollTop;
  var vpBottom = vpTop + rowsViewport.clientHeight;
  if (trTop < vpTop) rowsViewport.scrollTop = trTop;
  else if (trBottom > vpBottom) rowsViewport.scrollTop = trBottom - rowsViewport.clientHeight;
}

function selectNextMatch(delta) {
  if (!filteredRows.length) return;
  if (selectedMatchIndex < 0) selectedMatchIndex = 0;
  else {
    selectedMatchIndex = (selectedMatchIndex + delta + filteredRows.length) % filteredRows.length;
  }
  var row = filteredRows[selectedMatchIndex];
  selectedRowLine = row ? row.line : null;
  lastRenderedRange = { start: -1, end: -1, total: -1 };
  queueVirtualRender();
  window.requestAnimationFrame(function () {
    updateSelectedRowHighlight();
    ensureSelectedVisible();
  });
}

function histogramBucketType(level) {
  var v = String(level || "").toLowerCase();
  if (v.indexOf("error") !== -1 || v.indexOf("fatal") !== -1 || v.indexOf("panic") !== -1) return "error";
  if (v.indexOf("warn") !== -1) return "warn";
  if (v.indexOf("info") !== -1) return "info";
  if (v.indexOf("debug") !== -1 || v.indexOf("trace") !== -1) return "other";
  return "other";
}

function renderHistogram() {
  if (!histogramBars) return;
  histogramBars.innerHTML = "";
  var rows = filteredRows || [];
  if (!rows.length) return;

  var bucketCount = Math.min(64, Math.max(12, Math.ceil(Math.sqrt(rows.length))));
  var buckets = [];
  for (var i = 0; i < bucketCount; i++) {
    buckets.push({ info: 0, warn: 0, error: 0, other: 0, total: 0 });
  }
  for (var r = 0; r < rows.length; r++) {
    var idx = Math.floor((r / rows.length) * bucketCount);
    if (idx >= bucketCount) idx = bucketCount - 1;
    var bucket = buckets[idx];
    var kind = histogramBucketType(rows[r].level || rowAttr(rows[r], "data-level") || "");
    bucket[kind] = (bucket[kind] || 0) + 1;
    bucket.total++;
  }
  var maxTotal = 1;
  buckets.forEach(function (b) { if (b.total > maxTotal) maxTotal = b.total; });

  buckets.forEach(function (b) {
    var bar = document.createElement("div");
    bar.className = "histogram-bar";
    var totalPx = Math.max(2, Math.round((b.total / maxTotal) * 70));
    bar.style.height = String(totalPx) + "px";
    ["info", "warn", "error", "other"].forEach(function (key) {
      if (!b[key]) return;
      var seg = document.createElement("div");
      seg.className = "histogram-stack " + key;
      seg.style.height = String(Math.max(1, Math.round((b[key] / b.total) * totalPx))) + "px";
      bar.appendChild(seg);
    });
    bar.title = "total=" + b.total + " info=" + b.info + " warn=" + b.warn + " error=" + b.error + " other=" + b.other;
    histogramBars.appendChild(bar);
  });
}

function setDrawerText(el, value) {
  if (!el) return;
  el.textContent = (value === null || value === undefined || value === "") ? "-" : String(value);
}

function openDrawerForRow(row) {
  if (!row || !drawer) return;
  drawerRow = row;
  setDrawerText(drawerLine, row.line);
  setDrawerText(drawerTimestamp, row.timestamp);
  setDrawerText(drawerLevel, row.level);
  setDrawerText(drawerSource, row.source);
  setDrawerText(drawerContext, row.context);
  setDrawerText(drawerMessage, row.message);
  if (drawerJSON) drawerJSON.textContent = row.prettyJSON || "{}";
  if (drawerCopyState) drawerCopyState.textContent = "";
  drawer.classList.add("open");
  drawer.setAttribute("aria-hidden", "false");
  if (drawerBackdrop) {
    drawerBackdrop.hidden = false;
    drawerBackdrop.classList.add("open");
  }
}

function closeDrawer() {
  if (!drawer) return;
  drawer.classList.remove("open");
  drawer.setAttribute("aria-hidden", "true");
  if (drawerBackdrop) {
    drawerBackdrop.classList.remove("open");
    drawerBackdrop.hidden = true;
  }
}

function scrollIntoViewIfNeeded(el) {
  if (!el || typeof el.scrollIntoView !== "function") return;
  el.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

function copyTextToClipboard(raw, stateEl) {
  if (stateEl) stateEl.textContent = "";
  return (async function () {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(raw);
      } else {
        var area = document.createElement("textarea");
        area.value = raw;
        area.setAttribute("readonly", "");
        area.style.position = "absolute";
        area.style.left = "-9999px";
        document.body.appendChild(area);
        area.select();
        document.execCommand("copy");
        document.body.removeChild(area);
      }
      if (stateEl) stateEl.textContent = "Copied";
    } catch (_) {
      if (stateEl) stateEl.textContent = "Copy failed";
    }
  })();
}

function getPathValue(obj, path) {
  var cur = obj;
  var parts = String(path || "").split(".").filter(Boolean);
  for (var i = 0; i < parts.length; i++) {
    if (cur === null || cur === undefined) return { ok: false, value: null };
    var seg = parts[i];
    if (Array.isArray(cur)) {
      if (!/^\d+$/.test(seg)) return { ok: false, value: null };
      cur = cur[Number(seg)];
      continue;
    }
    if (typeof cur !== "object" || !Object.prototype.hasOwnProperty.call(cur, seg)) return { ok: false, value: null };
    cur = cur[seg];
  }
  return { ok: true, value: cur };
}

function selectedFieldsObject(row, fieldList) {
  var out = {};
  if (!row) return out;
  var payload = {};
  try { payload = JSON.parse(row.rawJSON || "{}"); } catch (_) {}
  fieldList.forEach(function (field) {
    var key = field.trim();
    if (!key) return;
    var meta = { line: row.line, timestamp: row.timestamp, level: row.level, source: row.source, context: row.context, message: row.message };
    if (Object.prototype.hasOwnProperty.call(meta, key)) {
      out[key] = meta[key];
      return;
    }
    var found = getPathValue(payload, key);
    if (found.ok) out[key] = found.value;
  });
  return out;
}

function queueSearchApply() {
  if (searchDebounceTimer) window.clearTimeout(searchDebounceTimer);
  searchDebounceTimer = window.setTimeout(function () {
    searchDebounceTimer = 0;
    syncQueryInputs();
    syncQueryState();
    renderFilterChips();
    applySearch();
  }, 120);
}

if (searchInput && filterInput) {
  if (uiState.search) searchInput.value = uiState.search;
  if (uiState.filters) filterInput.value = uiState.filters;
  syncQueryInputs();
  renderFilterChips();
  searchInput.addEventListener("input", queueSearchApply);
  filterInput.addEventListener("input", queueSearchApply);
}

if (bookmarkSave) {
  bookmarkSave.addEventListener("click", addBookmark);
}

if (bookmarkClear) {
  bookmarkClear.addEventListener("click", clearBookmarks);
}

columnToggles.forEach(function (toggle) {
  var key = toggle.getAttribute("data-column");
  if (Object.prototype.hasOwnProperty.call(uiState.columns || {}, key)) {
    toggle.checked = uiState.columns[key] !== false;
  }
  toggle.addEventListener("change", function () {
    if (!uiState.columns) uiState.columns = {};
    uiState.columns[key] = !!toggle.checked;
    saveUIState();
    applyColumnVisibility();
    lastRenderedRange = { start: -1, end: -1, total: -1 };
    queueVirtualRender();
  });
});

renderBookmarks();
hydrateInitialRowsFromDOM();
syncRowCounters();
ensureVirtualSpacers();
applyColumnVisibility();
updateVisibleColumnsChip();
if (rowsViewport) {
  rowsViewport.addEventListener("scroll", queueVirtualRender, { passive: true });
}
window.addEventListener("resize", queueVirtualRender);

document.addEventListener("keydown", function (event) {
  if (!searchInput) return;
  var target = event.target;
  var tag = target && target.tagName ? target.tagName.toLowerCase() : "";
  var isEditable = !!(target && (target.isContentEditable || tag === "input" || tag === "textarea" || tag === "select"));
  if (event.key === "/" && !event.metaKey && !event.ctrlKey && !event.altKey && !isEditable) {
    event.preventDefault();
    searchInput.focus();
    searchInput.select();
    return;
  }
  if (event.key.toLowerCase() === "f" && !event.metaKey && !event.ctrlKey && !event.altKey && !isEditable && filterInput) {
    event.preventDefault();
    filterInput.focus();
    filterInput.select();
    return;
  }
  if (event.key === "Escape" && target === searchInput) {
    if (searchInput.value !== "") {
      searchInput.value = "";
      queueSearchApply();
    }
    searchInput.select();
    event.preventDefault();
    return;
  }
  if (event.key === "Escape" && target === filterInput) {
    if (filterInput && filterInput.value !== "") {
      filterInput.value = "";
      queueSearchApply();
    }
    if (filterInput) filterInput.select();
    event.preventDefault();
    return;
  }
  if (!isEditable && (event.key === "n" || (event.shiftKey && event.key === "N"))) {
    event.preventDefault();
    selectNextMatch(event.shiftKey ? -1 : 1);
    return;
  }
  if (!isEditable && (event.key === "ArrowDown" || event.key === "j")) {
    event.preventDefault();
    selectNextMatch(1);
    return;
  }
  if (!isEditable && (event.key === "ArrowUp" || event.key === "k")) {
    event.preventDefault();
    selectNextMatch(-1);
    return;
  }
  if (!isEditable && (event.key === "p" || event.key === "P") && streamToggle) {
    event.preventDefault();
    streamToggle.click();
    return;
  }
  if (event.key === "Escape" && drawer && drawer.classList.contains("open")) {
    event.preventDefault();
    closeDrawer();
    return;
  }
  if (event.key === "Escape" && target === queryInput) {
    if (queryInput.value !== "") {
      queryInput.value = "";
      syncQueryState();
      applySearch();
    }
    queryInput.select();
    event.preventDefault();
  }
});

applySearch();

function updateFieldMeta(fields) {
  if (!fields) return;
  var meta = document.querySelector(".meta div:nth-child(2)");
  if (!meta) return;
  var timestamp = fields.timestamp || "-";
  var level = fields.level || "-";
  var message = fields.message || "-";
  var source = fields.source || "-";
  var context = fields.context || "-";
  meta.innerHTML = "<strong>Inferred fields:</strong> timestamp=" + escapeHTML(timestamp) + ", level=" + escapeHTML(level) + ", message=" + escapeHTML(message) + ", source=" + escapeHTML(source) + ", context=" + escapeHTML(context);
}

function escapeHTML(value) {
  return String(value)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/\"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function updateIngestMetrics(metrics) {
  if (!metrics) return;
  if (eventsPerSec) {
    var eps = Number(metrics.eventsPerSec || 0);
    eventsPerSec.textContent = Number.isFinite(eps) ? eps.toFixed(1) : "0.0";
  }
  if (parseErrorCount) parseErrorCount.textContent = String(metrics.parseErrors || 0);
  if (droppedEventCount) droppedEventCount.textContent = String(metrics.droppedEvents || 0);
}

function rowSeverityClass(level) {
  var v = String(level || "").toLowerCase();
  if (v.indexOf("error") !== -1 || v.indexOf("fatal") !== -1 || v.indexOf("panic") !== -1) return "severity-error";
  if (v.indexOf("warn") !== -1) return "severity-warn";
  if (v.indexOf("debug") !== -1 || v.indexOf("trace") !== -1) return "severity-debug";
  if (v) return "severity-info";
  return "";
}

function applyColumnVisibility() {
  var map = (uiState && uiState.columns) || {};
  Array.prototype.slice.call(document.querySelectorAll("[data-col]")).forEach(function (cell) {
    var col = cell.getAttribute("data-col");
    var visible = map[col] !== false;
    cell.classList.toggle("cell-hidden", !visible);
  });
  updateVisibleColumnsChip();
}

function applyColumnVisibilityToRow(tr) {
  if (!tr) return;
  var map = (uiState && uiState.columns) || {};
  Array.prototype.slice.call(tr.querySelectorAll("[data-col]")).forEach(function (cell) {
    var col = cell.getAttribute("data-col");
    var visible = map[col] !== false;
    cell.classList.toggle("cell-hidden", !visible);
  });
}

function updateVisibleColumnsChip() {
  if (!columnsVisibleChip) return;
  var count = 0;
  (columnToggles || []).forEach(function (toggle) {
    if (toggle.checked) count++;
  });
  columnsVisibleChip.textContent = String(count);
}

function createCell(text) {
  var td = document.createElement("td");
  td.textContent = text || "-";
  return td;
}

function buildRowElement(row) {
  var tr = document.createElement("tr");
  tr.className = "log-row " + rowSeverityClass(row.level || "");
  tr.setAttribute("data-search", row.searchText || "");
  tr.setAttribute("data-raw-json", row.rawJSON || "");
  tr.setAttribute("data-line", row.line == null ? "" : String(row.line));
  tr.setAttribute("data-timestamp", row.timestamp || "");
  tr.setAttribute("data-level", row.level || "");
  tr.setAttribute("data-source", row.source || "");
  tr.setAttribute("data-context", row.context || "");
  tr.setAttribute("data-message", row.message || "");

  var lineCell = document.createElement("td");
  lineCell.textContent = String(row.line);
  tr.appendChild(lineCell);
  var timestampCell = createCell(row.timestamp); timestampCell.setAttribute("data-col", "timestamp"); tr.appendChild(timestampCell);
  var levelCell = document.createElement("td");
  levelCell.setAttribute("data-col", "level");
  var levelBadge = document.createElement("span");
  levelBadge.className = "level-badge " + histogramBucketType(row.level || "");
  levelBadge.textContent = row.level || "-";
  levelCell.appendChild(levelBadge);
  tr.appendChild(levelCell);
  var sourceCell = createCell(row.source); sourceCell.setAttribute("data-col", "source"); tr.appendChild(sourceCell);
  var contextCell = createCell(row.context); contextCell.setAttribute("data-col", "context"); tr.appendChild(contextCell);

  var msgCell = createCell(row.message);
  msgCell.className = "msg";
  msgCell.setAttribute("data-col", "message");
  tr.appendChild(msgCell);

  var payloadCell = document.createElement("td");
  payloadCell.className = "payload";
  payloadCell.setAttribute("data-col", "payload");
  var actions = document.createElement("div");
  var button = document.createElement("button");
  button.type = "button";
  button.className = "copy-json inline-action open-drawer";
  button.textContent = "Details >";
  button.setAttribute("data-raw-json", row.rawJSON || "");
  actions.appendChild(button);
  var copyState = document.createElement("span");
  copyState.className = "copy-state";
  copyState.setAttribute("aria-live", "polite");
  actions.appendChild(copyState);
  var pre = document.createElement("pre");
  pre.className = "payload-json";
  pre.hidden = true;
  pre.textContent = row.prettyJSON || "";
  payloadCell.appendChild(actions);
  payloadCell.appendChild(pre);
  tr.appendChild(payloadCell);
  applyColumnVisibilityToRow(tr);
  if (selectedRowLine !== null && String(selectedRowLine) === String(row.line)) {
    tr.classList.add("is-selected");
  }
  return tr;
}

function appendRow(row) {
  if (!row) return;
  var normalized = normalizeRowData(row);
  if (!normalized) return;
  allRows.push(normalized);
  var evicted = evictRowsIfNeeded();
  if (evicted.length > 0 && filteredRows.length > 0) {
    filteredRows = filteredRows.filter(function (candidate) {
      return evicted.indexOf(candidate) === -1;
    });
  }
  if (rowMatchesParsed(normalized, currentQueryParsed)) {
    filteredRows.push(normalized);
  }
  syncRowCounters();
  lastRenderedRange = { start: -1, end: -1, total: -1 };
  queueVirtualRender();
}

function appendMalformedLine(item) {
  if (!malformedBody || !item) return;
  if (noMalformedRow && noMalformedRow.parentNode) {
    noMalformedRow.parentNode.removeChild(noMalformedRow);
    noMalformedRow = null;
  }
  var tr = document.createElement("tr");
  tr.className = "malformed-row";
  tr.appendChild(createCell(item.line == null ? "" : String(item.line)));
  tr.appendChild(createCell(item.error || ""));
  var rawCell = document.createElement("td");
  var code = document.createElement("code");
  code.textContent = item.raw || "";
  rawCell.appendChild(code);
  tr.appendChild(rawCell);
  malformedBody.appendChild(tr);
  var malformedRows = malformedBody.querySelectorAll("tr.malformed-row");
  if (malformedRows.length > MAX_CLIENT_MALFORMED_ROWS) {
    var removeCount = malformedRows.length - MAX_CLIENT_MALFORMED_ROWS;
    for (var i = 0; i < removeCount; i++) {
      if (malformedRows[i] && malformedRows[i].parentNode) malformedRows[i].parentNode.removeChild(malformedRows[i]);
    }
  }
  if (malformedCount) {
    var current = Number(malformedCount.textContent || "0");
    malformedCount.textContent = String(current + 1);
  }
}

function setPendingCount() {
  if (!streamPending) return;
  var count = pendingEvents.length;
  streamPending.hidden = count === 0;
  streamPending.textContent = String(count) + " queued";
}

function handleStreamEvent(payload) {
  if (!payload) return;
  updateIngestMetrics(payload.metrics);
  if (payload.type === "malformed") {
    appendMalformedLine(payload.malformed);
    return;
  }
  if (payload.type !== "append") return;
  updateFieldMeta(payload.fields);
  if (livePaused) {
    pendingEvents.push(payload);
    if (pendingEvents.length > MAX_PENDING_EVENTS) {
      pendingEvents.splice(0, pendingEvents.length - MAX_PENDING_EVENTS);
    }
    setPendingCount();
    return;
  }
  appendRow(payload.row);
}

function flushPendingEvents() {
  while (pendingEvents.length > 0) {
    var event = pendingEvents.shift();
    appendRow(event.row);
    updateFieldMeta(event.fields);
  }
  setPendingCount();
}

if (streamToggle) {
  streamToggle.addEventListener("click", function () {
    livePaused = !livePaused;
    streamToggle.setAttribute("aria-pressed", livePaused ? "true" : "false");
    streamToggle.textContent = livePaused ? "Resume stream" : "Pause stream";
    if (!livePaused) flushPendingEvents();
  });
}

if (window.EventSource && streamToggle) {
  var source = new EventSource("/events");
  source.onmessage = function (event) {
    try {
      handleStreamEvent(JSON.parse(event.data));
    } catch (_) {}
  };
}

if (drawerClose) drawerClose.addEventListener("click", closeDrawer);
if (drawerBackdrop) drawerBackdrop.addEventListener("click", closeDrawer);
if (drawerCopyRaw) {
  drawerCopyRaw.addEventListener("click", function () {
    if (!drawerRow) return;
    copyTextToClipboard(drawerRow.rawJSON || "", drawerCopyState);
  });
}
if (drawerCopyFields) {
  drawerCopyFields.addEventListener("click", function () {
    if (!drawerRow) return;
    var fields = (drawerFieldInput ? drawerFieldInput.value : "").split(",");
    var picked = selectedFieldsObject(drawerRow, fields);
    copyTextToClipboard(JSON.stringify(picked), drawerCopyState);
  });
}

if (railExplore) {
  railExplore.addEventListener("click", function () {
    if (searchInput) {
      searchInput.focus();
      searchInput.select();
      return;
    }
    scrollIntoViewIfNeeded(document.querySelector(".toolbar"));
  });
}
if (railFilters) {
  railFilters.addEventListener("click", function () {
    if (filterInput) {
      filterInput.focus();
      filterInput.select();
      return;
    }
    scrollIntoViewIfNeeded(filterChipList || document.querySelector(".column-controls"));
  });
}
if (railStream) {
  railStream.addEventListener("click", function () {
    if (streamToggle) {
      streamToggle.click();
      return;
    }
    scrollIntoViewIfNeeded(document.querySelector(".results-toolbar"));
  });
}
if (railBookmarks) {
  railBookmarks.addEventListener("click", function () {
    if (bookmarkSave) {
      scrollIntoViewIfNeeded(bookmarkSave);
      bookmarkSave.focus();
      return;
    }
    scrollIntoViewIfNeeded(bookmarkList);
  });
}
if (railErrors) {
  railErrors.addEventListener("click", function () {
    if (malformedPanel) {
      malformedPanel.open = true;
      scrollIntoViewIfNeeded(malformedPanel);
      return;
    }
  });
}
if (railDetails) {
  railDetails.addEventListener("click", function () {
    if (drawer && drawer.classList.contains("open")) {
      closeDrawer();
      return;
    }
    if (selectedRowLine !== null) {
      for (var i = 0; i < allRows.length; i++) {
        if (String(allRows[i].line) === String(selectedRowLine)) {
          openDrawerForRow(allRows[i]);
          return;
        }
      }
    }
    if (filteredRows.length > 0) {
      if (selectedMatchIndex < 0) {
        selectedMatchIndex = 0;
        selectedRowLine = filteredRows[0].line;
      }
      openDrawerForRow(filteredRows[Math.max(0, selectedMatchIndex)]);
      return;
    }
    scrollIntoViewIfNeeded(drawer);
  });
}

document.addEventListener("click", async function (event) {
  var openButton = event.target.closest(".open-drawer");
  if (openButton) {
    event.preventDefault();
    event.stopPropagation();
    var trForButton = openButton.closest("tr.log-row");
    if (trForButton) {
      var pre = trForButton.querySelector("pre.payload-json");
      openDrawerForRow(normalizeRowData({
        line: trForButton.getAttribute("data-line"),
        timestamp: trForButton.getAttribute("data-timestamp") || "",
        level: trForButton.getAttribute("data-level") || "",
        source: trForButton.getAttribute("data-source") || "",
        context: trForButton.getAttribute("data-context") || "",
        message: trForButton.getAttribute("data-message") || "",
        rawJSON: trForButton.getAttribute("data-raw-json") || "",
        prettyJSON: pre ? (pre.textContent || "") : "",
        searchText: trForButton.getAttribute("data-search") || ""
      }));
    }
    return;
  }
  var rowTarget = event.target.closest("tr.log-row");
  if (rowTarget && !event.target.closest("button, a, input, textarea, select")) {
    var line = rowTarget.getAttribute("data-line");
    var row = null;
    for (var i = 0; i < allRows.length; i++) {
      if (String(allRows[i].line) === String(line)) { row = allRows[i]; break; }
    }
    if (row) {
      selectedRowLine = row.line;
      updateSelectedRowHighlight();
      openDrawerForRow(row);
    }
    return;
  }
  var button = event.target.closest(".copy-json");
  if (!button) return;
  if (button.classList.contains("open-drawer")) return;

  var raw = button.getAttribute("data-raw-json") || "";
  var state = button.parentElement && button.parentElement.querySelector(".copy-state");
  await copyTextToClipboard(raw, state);
});
</script>
</body>
</html>`
