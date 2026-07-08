// FM-Index Signature Cache CLI
//
// Author: Jani Willberg <jani@willberg.me>
// Created: 25.06.2026
// Version: v1.0.0
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jwillberg/fm-index/fmindex"
)

var (
	version = "v1.0.0"
	commit  = "unknown"
	builtAt = "unknown"
)

const (
	defaultIndexPath = "cache.fm"
	defaultBind      = "127.0.0.1"
	defaultPort      = 8080
)

type daemonConfig struct {
	IndexPath      string
	Bind           string
	Port           int
	RebuildCommand string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "build":
		err = build(os.Args[2:])
	case "query":
		err = query(os.Args[2:])
	case "query-batch":
		err = queryBatch(os.Args[2:])
	case "daemon":
		err = daemon(os.Args[2:])
	case "version":
		printVersion()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func build(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	out := fs.String("o", "cache.fm", "output index path")
	recursive := fs.Bool("r", false, "read directories recursively")
	memoryLimitValue := fs.String("memory-limit", "512M", "approximate builder memory budget")
	maxQueryBytesValue := fs.String("max-query-bytes", "1M", "maximum query length guaranteed for chunked files")
	jobs := fs.Int("jobs", 0, "parallel shard build jobs; 0 uses CPU count")
	silent := fs.Bool("silent", false, "hide build progress")
	verbose := fs.Bool("verbose", false, "show detailed build progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: fmindex build [-r] [-o cache.fm] [-memory-limit bytes] [-max-query-bytes bytes] <file-or-dir> [...]")
	}

	memoryLimit, err := parseSize(*memoryLimitValue)
	if err != nil {
		return err
	}
	maxQueryBytes, err := parseSize(*maxQueryBytesValue)
	if err != nil {
		return err
	}

	var progress fmindex.ProgressFunc
	if !*silent {
		progress = newProgressPrinter(os.Stderr, *verbose)
	}

	meta, err := fmindex.BuildShardedToFile(fs.Args(), *out, fmindex.ShardedBuildOptions{
		Recursive:     *recursive,
		MemoryLimit:   memoryLimit,
		MaxQueryBytes: maxQueryBytes,
		Jobs:          *jobs,
		Progress:      progress,
	})
	if err != nil {
		return err
	}

	fmt.Printf("indexed_files=%d\n", meta.FileCount)
	fmt.Printf("total_bytes=%d\n", meta.TotalBytes)
	fmt.Printf("sha256=%s\n", meta.SHA256)
	fmt.Printf("shards=%d\n", meta.ShardCount)
	fmt.Printf("memory_limit=%d\n", meta.MemoryLimit)
	fmt.Printf("max_query_bytes=%d\n", meta.MaxQueryBytes)
	fmt.Printf("jobs=%d\n", meta.Jobs)
	fmt.Printf("shard_size=%d\n", meta.ShardSize)
	fmt.Printf("index=%s\n", *out)
	fmt.Printf("meta=%s.meta\n", *out)
	return nil
}

func parseSize(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty size")
	}

	multiplier := uint64(1)
	last := value[len(value)-1]
	switch last {
	case 'k', 'K':
		multiplier = 1024
		value = value[:len(value)-1]
	case 'm', 'M':
		multiplier = 1024 * 1024
		value = value[:len(value)-1]
	case 'g', 'G':
		multiplier = 1024 * 1024 * 1024
		value = value[:len(value)-1]
	}

	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", value, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("size must be greater than zero")
	}
	return n * multiplier, nil
}

func newProgressPrinter(writer io.Writer, verbose bool) fmindex.ProgressFunc {
	startedAt := time.Now()
	lastLineLength := 0
	var mutex sync.Mutex

	return func(update fmindex.BuildProgress) {
		mutex.Lock()
		defer mutex.Unlock()

		line := progressLine(update, verbose, time.Since(startedAt))
		if line == "" {
			return
		}
		if lastLineLength > len(line) {
			line += strings.Repeat(" ", lastLineLength-len(line))
		}
		lastLineLength = len(line)
		fmt.Fprintf(writer, "\r%s", line)
		if update.Phase == fmindex.BuildPhaseDone {
			fmt.Fprintln(writer)
		}
	}
}

func progressLine(update fmindex.BuildProgress, verbose bool, elapsed time.Duration) string {
	label := string(update.Phase)
	if update.Message != "" {
		label = update.Message
	}

	var line string
	switch {
	case update.Total > 0:
		line = fmt.Sprintf("%s %d/%d", label, update.Current, update.Total)
	case update.TotalBytes > 0:
		line = fmt.Sprintf("%s %s/%s", label, formatBytes(update.CurrentBytes), formatBytes(update.TotalBytes))
	case update.CurrentBytes > 0:
		line = fmt.Sprintf("%s %s", label, formatBytes(update.CurrentBytes))
	default:
		line = label
	}

	if verbose && update.Path != "" {
		line += " " + update.Path
	}
	if elapsed > 0 {
		line += " elapsed=" + elapsed.Truncate(time.Second).String()
	}
	return line
}

func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}

	value := float64(bytes)
	for _, suffix := range []string{"K", "M", "G", "T"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1fP", value/unit)
}

func query(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	in := fs.String("i", "cache.fm", "input index path")
	fromStdin := fs.Bool("stdin", false, "read query bytes from stdin")
	queryFile := fs.String("file", "", "read query bytes from file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	idx, err := fmindex.OpenSearch(*in)
	if err != nil {
		return err
	}

	needle, err := queryBytes(fs.Args(), *fromStdin, *queryFile)
	if err != nil {
		return err
	}
	if maxQueryBytes := idx.MaxQueryBytes(); maxQueryBytes > 0 && uint64(len(needle)) > maxQueryBytes {
		return fmt.Errorf("query is %d bytes, index supports queries up to %d bytes", len(needle), maxQueryBytes)
	}
	if idx.ContainsBytes(needle) {
		fmt.Println("TRUE")
		return nil
	}
	fmt.Println("FALSE")
	return nil
}

func queryBatch(args []string) error {
	fs := flag.NewFlagSet("query-batch", flag.ExitOnError)
	in := fs.String("i", "cache.fm", "input index path")
	queryFile := fs.String("file", "", "read newline-delimited query bytes from file; default is stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: fmindex query-batch [-i cache.fm] [--file queries.txt]")
	}

	idx, err := fmindex.OpenSearch(*in)
	if err != nil {
		return err
	}

	reader := io.Reader(os.Stdin)
	if *queryFile != "" {
		file, err := os.Open(*queryFile)
		if err != nil {
			return fmt.Errorf("open query batch file %q: %w", *queryFile, err)
		}
		defer file.Close()
		reader = file
	}

	return runBatchQueries(idx, reader, os.Stdout)
}

func daemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "", "daemon config file path")
	in := fs.String("i", "", "input index path")
	bind := fs.String("bind", "", "HTTP bind address")
	port := fs.Int("port", 0, "HTTP listen port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: fmindex daemon [--config /opt/fmindex/etc/fmindex.conf] [-i cache.fm] [--bind 127.0.0.1] [--port 8080]")
	}

	cfg, err := loadDaemonConfig(*configPath)
	if err != nil {
		return err
	}
	applyDaemonFlagOverrides(&cfg, fs, *in, *bind, *port)
	if err := validateDaemonConfig(cfg); err != nil {
		return err
	}

	idx, err := openDaemonIndex(cfg.IndexPath)
	if err != nil {
		return err
	}
	searcher := newReloadableSearcher(cfg.IndexPath, idx)
	setupIndexReloadSignals(searcher, cfg.IndexPath)

	var rebuildRunner *rebuildRunner
	if cfg.RebuildCommand != "" {
		rebuildRunner = newRebuildRunner(cfg.RebuildCommand)
	}

	address := fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)
	server := &http.Server{
		Addr:              address,
		Handler:           newHTTPHandler(searcher, rebuildRunner),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "fmindex daemon listening on http://%s index=%s\n", address, cfg.IndexPath)
	return server.ListenAndServe()
}

func loadDaemonConfig(path string) (daemonConfig, error) {
	cfg := daemonConfig{
		IndexPath: defaultIndexPath,
		Bind:      defaultBind,
		Port:      defaultPort,
	}
	if path == "" {
		return cfg, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return daemonConfig{}, fmt.Errorf("open daemon config %q: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return daemonConfig{}, fmt.Errorf("parse daemon config %q line %d: expected key=value", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if err := applyDaemonConfigValue(&cfg, key, value); err != nil {
			return daemonConfig{}, fmt.Errorf("parse daemon config %q line %d: %w", path, lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return daemonConfig{}, fmt.Errorf("read daemon config %q: %w", path, err)
	}
	return cfg, nil
}

func applyDaemonConfigValue(cfg *daemonConfig, key string, value string) error {
	switch key {
	case "index_path", "index":
		if value == "" {
			return fmt.Errorf("%s must not be empty", key)
		}
		cfg.IndexPath = value
	case "bind":
		if value == "" {
			return fmt.Errorf("bind must not be empty")
		}
		cfg.Bind = value
	case "port":
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid port %q: %w", value, err)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d is outside valid range 1-65535", port)
		}
		cfg.Port = port
	case "rebuild_command":
		cfg.RebuildCommand = value
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

func applyDaemonFlagOverrides(cfg *daemonConfig, fs *flag.FlagSet, indexPath string, bind string, port int) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "i":
			cfg.IndexPath = indexPath
		case "bind":
			cfg.Bind = bind
		case "port":
			cfg.Port = port
		}
	})
}

func validateDaemonConfig(cfg daemonConfig) error {
	if cfg.IndexPath == "" {
		return fmt.Errorf("daemon index path must not be empty")
	}
	if cfg.Bind == "" {
		return fmt.Errorf("daemon bind address must not be empty")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("daemon port %d is outside valid range 1-65535", cfg.Port)
	}
	return nil
}

func openDaemonIndex(path string) (fmindex.Searcher, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("daemon index %q does not exist; build the index before starting the daemon", path)
		}
		return nil, fmt.Errorf("stat daemon index %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("daemon index %q is a directory; expected an index file or shard manifest", path)
	}

	idx, err := fmindex.OpenSearch(path)
	if err != nil {
		return nil, fmt.Errorf("open daemon index %q: %w", path, err)
	}
	return idx, nil
}

type reloadableSearcher struct {
	mutex     sync.RWMutex
	indexPath string
	loadedAt  time.Time
	searcher  fmindex.Searcher
}

func newReloadableSearcher(indexPath string, searcher fmindex.Searcher) *reloadableSearcher {
	return &reloadableSearcher{
		indexPath: indexPath,
		loadedAt:  time.Now().UTC(),
		searcher:  searcher,
	}
}

func (searcher *reloadableSearcher) Reload() error {
	next, err := openDaemonIndex(searcher.indexPath)
	if err != nil {
		return err
	}

	searcher.mutex.Lock()
	defer searcher.mutex.Unlock()
	searcher.searcher = next
	searcher.loadedAt = time.Now().UTC()
	return nil
}

func (searcher *reloadableSearcher) Contains(q string) bool {
	searcher.mutex.RLock()
	defer searcher.mutex.RUnlock()
	return searcher.searcher.Contains(q)
}

func (searcher *reloadableSearcher) ContainsBytes(q []byte) bool {
	searcher.mutex.RLock()
	defer searcher.mutex.RUnlock()
	return searcher.searcher.ContainsBytes(q)
}

func (searcher *reloadableSearcher) MaxQueryBytes() uint64 {
	searcher.mutex.RLock()
	defer searcher.mutex.RUnlock()
	return searcher.searcher.MaxQueryBytes()
}

type rebuildRunner struct {
	mutex        sync.Mutex
	command      string
	running      bool
	lastStarted  time.Time
	lastFinished time.Time
	lastError    string
}

// newRebuildRunner prepares a single-process guard around the configured
// rebuild command. The update script should still keep its own filesystem lock,
// because multiple daemon processes or manual runs can exist outside this guard.
func newRebuildRunner(command string) *rebuildRunner {
	return &rebuildRunner{
		command: command,
	}
}

// Start launches the configured rebuild command in the background. The HTTP
// request returns immediately while command output is written to the daemon log.
func (runner *rebuildRunner) Start() (bool, string) {
	runner.mutex.Lock()
	if runner.running {
		runner.mutex.Unlock()
		return false, "rebuild already running"
	}
	runner.running = true
	runner.lastStarted = time.Now().UTC()
	runner.lastFinished = time.Time{}
	runner.lastError = ""
	command := runner.command
	runner.mutex.Unlock()

	go runner.run(command)
	return true, "rebuild started"
}

// run executes through the shell so operators can configure either a direct
// script path or a systemd-run wrapper without adding more daemon options.
func (runner *rebuildRunner) run(command string) {
	fmt.Fprintf(os.Stderr, "starting rebuild command: %s\n", command)
	cmd := exec.Command("/bin/sh", "-c", command)
	output, err := cmd.CombinedOutput()

	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	runner.running = false
	runner.lastFinished = time.Now().UTC()
	if err != nil {
		runner.lastError = err.Error()
		fmt.Fprintf(os.Stderr, "rebuild command failed: %v\n%s", err, output)
		return
	}
	fmt.Fprintf(os.Stderr, "rebuild command completed\n%s", output)
}

// Status returns a text format matching the rest of the CLI-oriented HTTP API.
func (runner *rebuildRunner) Status() string {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()

	status := "idle"
	if runner.running {
		status = "running"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "status=%s\n", status)
	if !runner.lastStarted.IsZero() {
		fmt.Fprintf(&builder, "last_started=%s\n", runner.lastStarted.Format(time.RFC3339))
	}
	if !runner.lastFinished.IsZero() {
		fmt.Fprintf(&builder, "last_finished=%s\n", runner.lastFinished.Format(time.RFC3339))
	}
	if runner.lastError != "" {
		fmt.Fprintf(&builder, "last_error=%s\n", runner.lastError)
	}
	return builder.String()
}

func newHTTPHandler(idx fmindex.Searcher, rebuildRunners ...*rebuildRunner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(writer, "OK")
	})
	mux.HandleFunc("/query", func(writer http.ResponseWriter, request *http.Request) {
		handleHTTPQuery(idx, writer, request)
	})
	mux.HandleFunc("/query-batch", func(writer http.ResponseWriter, request *http.Request) {
		handleHTTPBatchQuery(idx, writer, request)
	})
	var rebuildRunner *rebuildRunner
	if len(rebuildRunners) > 0 {
		rebuildRunner = rebuildRunners[0]
	}
	if rebuildRunner != nil {
		mux.HandleFunc("/rebuild", func(writer http.ResponseWriter, request *http.Request) {
			handleHTTPRebuild(rebuildRunner, writer, request)
		})
	}
	return mux
}

func handleHTTPRebuild(runner *rebuildRunner, writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(writer, runner.Status())
	case http.MethodPost:
		started, message := runner.Start()
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !started {
			http.Error(writer, message, http.StatusConflict)
			return
		}
		writer.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(writer, message)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleHTTPQuery(idx fmindex.Searcher, writer http.ResponseWriter, request *http.Request) {
	var needle []byte
	switch request.Method {
	case http.MethodGet:
		needle = []byte(request.URL.Query().Get("q"))
	case http.MethodPost:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, fmt.Sprintf("read request body: %v", err), http.StatusBadRequest)
			return
		}
		needle = body
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if len(needle) == 0 {
		http.Error(writer, "query is empty", http.StatusBadRequest)
		return
	}
	if maxQueryBytes := idx.MaxQueryBytes(); maxQueryBytes > 0 && uint64(len(needle)) > maxQueryBytes {
		http.Error(writer, fmt.Sprintf("query is %d bytes, index supports queries up to %d bytes", len(needle), maxQueryBytes), http.StatusRequestEntityTooLarge)
		return
	}

	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if idx.ContainsBytes(needle) {
		fmt.Fprintln(writer, "TRUE")
		return
	}
	fmt.Fprintln(writer, "FALSE")
}

func handleHTTPBatchQuery(idx fmindex.Searcher, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var output bytes.Buffer
	if err := runBatchQueries(idx, request.Body, &output); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := writer.Write(output.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "write HTTP batch response: %v\n", err)
	}
}

func runBatchQueries(idx fmindex.Searcher, reader io.Reader, writer io.Writer) error {
	lineReader := bufio.NewReader(reader)
	lineNumber := 0
	for {
		needle, err := readBatchQueryLine(lineReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		lineNumber++
		if maxQueryBytes := idx.MaxQueryBytes(); maxQueryBytes > 0 && uint64(len(needle)) > maxQueryBytes {
			return fmt.Errorf("query line %d is %d bytes, index supports queries up to %d bytes", lineNumber, len(needle), maxQueryBytes)
		}
		result := "FALSE"
		if idx.ContainsBytes(needle) {
			result = "TRUE"
		}
		if _, err := fmt.Fprintln(writer, result); err != nil {
			return fmt.Errorf("write batch result: %w", err)
		}
	}
}

func readBatchQueryLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read batch query line: %w", err)
	}
	line = trimLineEnding(line)
	return line, nil
}

func trimLineEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line
}

func queryBytes(args []string, fromStdin bool, queryFile string) ([]byte, error) {
	sources := 0
	if len(args) > 0 {
		sources++
	}
	if fromStdin {
		sources++
	}
	if queryFile != "" {
		sources++
	}
	if sources != 1 {
		return nil, fmt.Errorf("usage: fmindex query [-i cache.fm] (<substring> | --stdin | --file path)")
	}

	switch {
	case fromStdin:
		return io.ReadAll(os.Stdin)
	case queryFile != "":
		return os.ReadFile(queryFile)
	default:
		return []byte(strings.Join(args, " ")), nil
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  fmindex build [-r] [-o cache.fm] [-memory-limit bytes] [-max-query-bytes bytes] [-jobs n] [--silent] [--verbose] <file-or-dir> [...]")
	fmt.Fprintln(os.Stderr, "  fmindex query [-i cache.fm] (<substring> | --stdin | --file path)")
	fmt.Fprintln(os.Stderr, "  fmindex query-batch [-i cache.fm] [--file queries.txt]")
	fmt.Fprintln(os.Stderr, "  fmindex daemon [--config /opt/fmindex/etc/fmindex.conf] [-i cache.fm] [--bind 127.0.0.1] [--port 8080]")
	fmt.Fprintln(os.Stderr, "  fmindex version")
}

func printVersion() {
	fmt.Printf("version=%s\n", version)
	fmt.Printf("commit=%s\n", commit)
	fmt.Printf("built_at=%s\n", builtAt)
}
