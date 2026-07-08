package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jwillberg/fm-index/fmindex"
)

func TestQueryBytesFromArgs(t *testing.T) {
	got, err := queryBytes([]string{`"haku"tähän"`}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `"haku"tähän"` {
		t.Fatalf("unexpected query: %q", got)
	}
}

func TestParseSize(t *testing.T) {
	tests := map[string]uint64{
		"10": 10,
		"8K": 8 * 1024,
		"8M": 8 * 1024 * 1024,
		"1G": 1024 * 1024 * 1024,
	}
	for input, want := range tests {
		got, err := parseSize(input)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("parseSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestParseSizeRejectsZero(t *testing.T) {
	if _, err := parseSize("0"); err == nil {
		t.Fatal("expected zero size error")
	}
}

func TestProgressLine(t *testing.T) {
	line := progressLine(fmindex.BuildProgress{
		Phase:   fmindex.BuildPhaseBuilding,
		Message: "building shard",
		Current: 2,
		Total:   5,
		Path:    "cache.fm.shards/shard-000002.fm",
	}, true, 3*time.Second)

	for _, want := range []string{"building shard 2/5", "cache.fm.shards/shard-000002.fm", "elapsed=3s"} {
		if !strings.Contains(line, want) {
			t.Fatalf("progress line %q does not contain %q", line, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	if got := formatBytes(1536); got != "1.5K" {
		t.Fatalf("formatBytes() = %q, want 1.5K", got)
	}
}

func TestLoadDaemonConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fmindex.conf")
	config := strings.Join([]string{
		"# fmindex daemon",
		"index_path = /data/fmindex/current/cache.fm",
		"bind = 0.0.0.0",
		"port = 9090",
		"rebuild_command = /opt/fmindex/bin/build_fmindex.sh",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadDaemonConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IndexPath != "/data/fmindex/current/cache.fm" {
		t.Fatalf("IndexPath = %q", cfg.IndexPath)
	}
	if cfg.Bind != "0.0.0.0" {
		t.Fatalf("Bind = %q", cfg.Bind)
	}
	if cfg.Port != 9090 {
		t.Fatalf("Port = %d", cfg.Port)
	}
	if cfg.RebuildCommand != "/opt/fmindex/bin/build_fmindex.sh" {
		t.Fatalf("RebuildCommand = %q", cfg.RebuildCommand)
	}
}

func TestLoadDaemonConfigRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fmindex.conf")
	if err := os.WriteFile(path, []byte("unknown = value\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadDaemonConfig(path)
	if err == nil {
		t.Fatal("expected unknown key error")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenDaemonIndexReportsMissingIndex(t *testing.T) {
	_, err := openDaemonIndex(filepath.Join(t.TempDir(), "missing.fm"))
	if err == nil {
		t.Fatal("expected missing index error")
	}
	if !strings.Contains(err.Error(), "build the index before starting the daemon") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryBytesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signature.bin")
	want := []byte{'"', 'h', 'a', 'k', 'u', '"', 0x00, '\'', 0xff}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := queryBytes(nil, false, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected file query: %q", got)
	}
}

func TestQueryBytesFromStdin(t *testing.T) {
	oldStdin := os.Stdin
	t.Cleanup(func() {
		os.Stdin = oldStdin
	})

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r

	want := []byte("haku\"tähän'\n")
	go func() {
		_, _ = w.Write(want)
		_ = w.Close()
	}()

	got, err := queryBytes(nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected stdin query: %q", got)
	}
}

func TestQueryBytesRejectsMultipleSources(t *testing.T) {
	if _, err := queryBytes([]string{"abc"}, true, ""); err == nil {
		t.Fatal("expected multiple source error")
	}
}

func TestQueryBytesRejectsNoSource(t *testing.T) {
	if _, err := queryBytes(nil, false, ""); err == nil {
		t.Fatal("expected missing source error")
	}
}

func TestTrimLineEnding(t *testing.T) {
	tests := map[string]string{
		"alpha\n":   "alpha",
		"alpha\r\n": "alpha",
		"alpha":     "alpha",
	}
	for input, want := range tests {
		got := trimLineEnding([]byte(input))
		if string(got) != want {
			t.Fatalf("trimLineEnding(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRunBatchQueries(t *testing.T) {
	idx := fakeSearcher{
		matches: map[string]bool{
			"base64_decode(": true,
			"json(":          true,
		},
		maxQueryBytes: 1024,
	}
	input := strings.NewReader("base64_decode(\nmissing\r\njson(\n")
	var output bytes.Buffer

	if err := runBatchQueries(idx, input, &output); err != nil {
		t.Fatal(err)
	}
	want := "TRUE\nFALSE\nTRUE\n"
	if output.String() != want {
		t.Fatalf("batch output = %q, want %q", output.String(), want)
	}
}

func TestRunBatchQueriesRejectsTooLongQuery(t *testing.T) {
	idx := fakeSearcher{maxQueryBytes: 3}
	err := runBatchQueries(idx, strings.NewReader("abcd\n"), io.Discard)
	if err == nil {
		t.Fatal("expected too-long query error")
	}
	if !strings.Contains(err.Error(), "query line 1 is 4 bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPQuery(t *testing.T) {
	handler := newHTTPHandler(fakeSearcher{
		matches: map[string]bool{
			"base64_decode(": true,
		},
		maxQueryBytes: 1024,
	})

	request := httptest.NewRequest(http.MethodGet, "/query?q=base64_decode%28", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.String() != "TRUE\n" {
		t.Fatalf("body = %q, want TRUE", response.Body.String())
	}
}

func TestHTTPQueryPostBody(t *testing.T) {
	handler := newHTTPHandler(fakeSearcher{
		matches: map[string]bool{
			`"haku"tähän"`: true,
		},
		maxQueryBytes: 1024,
	})

	request := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`"haku"tähän"`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.String() != "TRUE\n" {
		t.Fatalf("body = %q, want TRUE", response.Body.String())
	}
}

func TestHTTPBatchQuery(t *testing.T) {
	handler := newHTTPHandler(fakeSearcher{
		matches: map[string]bool{
			"base64_decode(": true,
			"json(":          true,
		},
		maxQueryBytes: 1024,
	})

	request := httptest.NewRequest(http.MethodPost, "/query-batch", strings.NewReader("base64_decode(\nmissing\njson(\n"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	want := "TRUE\nFALSE\nTRUE\n"
	if response.Body.String() != want {
		t.Fatalf("body = %q, want %q", response.Body.String(), want)
	}
}

func TestHTTPQueryRejectsTooLongQuery(t *testing.T) {
	handler := newHTTPHandler(fakeSearcher{maxQueryBytes: 3})

	request := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader("abcd"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestHTTPRebuildStartsConfiguredCommand(t *testing.T) {
	handler := newHTTPHandler(fakeSearcher{}, newRebuildRunner("exit 0"))

	request := httptest.NewRequest(http.MethodPost, "/rebuild", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	if !strings.Contains(response.Body.String(), "rebuild started") {
		t.Fatalf("body = %q", response.Body.String())
	}
}

func TestHTTPRebuildStatus(t *testing.T) {
	handler := newHTTPHandler(fakeSearcher{}, newRebuildRunner("exit 0"))

	request := httptest.NewRequest(http.MethodGet, "/rebuild", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "status=idle") {
		t.Fatalf("body = %q", response.Body.String())
	}
}

type fakeSearcher struct {
	matches       map[string]bool
	maxQueryBytes uint64
}

func (searcher fakeSearcher) Contains(q string) bool {
	return searcher.matches[q]
}

func (searcher fakeSearcher) ContainsBytes(q []byte) bool {
	return searcher.matches[string(q)]
}

func (searcher fakeSearcher) MaxQueryBytes() uint64 {
	return searcher.maxQueryBytes
}
