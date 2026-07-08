package fmindex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	shardMagic                     = "FMSHARD001"
	trigramMagic                   = "FMTRI001"
	ngramFilterMagic               = "FMNGR001"
	trigramCount                   = 1 << 24
	ngramFilterN                   = 8
	ngramFilterBytes               = 4 * 1024 * 1024
	ngramFilterHashes              = 2
	builderMemoryMultiplier uint64 = 64
	// DefaultBuildMemoryLimit is the default approximate RAM budget for builds.
	DefaultBuildMemoryLimit uint64 = 512 * 1024 * 1024
	// DefaultMaxQueryBytes is the default correctness limit for chunked files.
	DefaultMaxQueryBytes uint64 = 1024 * 1024
	// MinimumBuildMemoryLimit is the smallest accepted memory budget.
	MinimumBuildMemoryLimit uint64 = builderMemoryMultiplier
)

// BuildPhase identifies the current high-level build step.
type BuildPhase string

const (
	// BuildPhaseCollecting means input paths are being expanded into files.
	BuildPhaseCollecting BuildPhase = "collecting"
	// BuildPhasePlanning means files are being split into build shards.
	BuildPhasePlanning BuildPhase = "planning"
	// BuildPhaseBuilding means FM-index shards are being constructed.
	BuildPhaseBuilding BuildPhase = "building"
	// BuildPhaseHashing means original file bytes are being hashed for metadata.
	BuildPhaseHashing BuildPhase = "hashing"
	// BuildPhaseWriting means final metadata or manifests are being written.
	BuildPhaseWriting BuildPhase = "writing"
	// BuildPhaseDone means the build has completed successfully.
	BuildPhaseDone BuildPhase = "done"
)

// BuildProgress describes one build progress update.
type BuildProgress struct {
	Phase        BuildPhase
	Message      string
	Current      int
	Total        int
	CurrentBytes uint64
	TotalBytes   uint64
	Path         string
}

// ProgressFunc receives build progress updates.
type ProgressFunc func(BuildProgress)

// Searcher is the common read-only interface implemented by single-file and
// sharded indexes.
type Searcher interface {
	Contains(string) bool
	ContainsBytes([]byte) bool
	MaxQueryBytes() uint64
}

// ShardedBuildOptions controls file collection and builder memory budgeting.
type ShardedBuildOptions struct {
	Recursive     bool
	MemoryLimit   uint64
	MaxQueryBytes uint64
	Jobs          int
	Progress      ProgressFunc

	// ShardSize is an advanced override mainly for tests and embedded callers.
	// CLI users should prefer MemoryLimit so they do not need to know builder
	// internals.
	ShardSize uint64
}

// ShardedMetadata describes a completed build that may contain one or more
// physical index shards.
type ShardedMetadata struct {
	Version       int       `json:"version"`
	FileCount     int       `json:"file_count"`
	TotalBytes    uint64    `json:"total_bytes"`
	SHA256        string    `json:"sha256"`
	CreatedAt     time.Time `json:"created_at"`
	Recursive     bool      `json:"recursive"`
	InputPaths    []string  `json:"input_paths"`
	MemoryLimit   uint64    `json:"memory_limit"`
	MaxQueryBytes uint64    `json:"max_query_bytes"`
	Jobs          int       `json:"jobs"`
	ShardSize     uint64    `json:"shard_size"`
	ShardCount    int       `json:"shard_count"`
	TrigramIndex  string    `json:"trigram_index,omitempty"`
	NgramFilter   string    `json:"ngram_filter,omitempty"`
	Shards        []Shard   `json:"shards"`
	IndexedFiles  []string  `json:"indexed_files"`
}

// Shard describes one physical FM-index file inside a sharded build.
type Shard struct {
	Path       string   `json:"path"`
	RawPath    string   `json:"raw_path,omitempty"`
	FileCount  int      `json:"file_count"`
	TotalBytes uint64   `json:"total_bytes"`
	Files      []string `json:"files"`
}

// ShardedIndex searches a shard manifest by opening one shard at a time.
type ShardedIndex struct {
	baseDir string
	meta    ShardedMetadata
}

type sourceSegment struct {
	path   string
	offset int64
	length uint64
}

type shardBuildTask struct {
	index   int
	out     string
	path    string
	rawOut  string
	rawPath string
	group   []sourceSegment
	size    uint64
}

type shardBuildResult struct {
	index int
	shard Shard
	err   error
}

// BuildShardedToFile builds an index at out using memory-budgeted file shards.
func BuildShardedToFile(paths []string, out string, opts ShardedBuildOptions) (ShardedMetadata, error) {
	progress := opts.Progress
	if opts.MemoryLimit == 0 {
		opts.MemoryLimit = DefaultBuildMemoryLimit
	}
	if opts.MaxQueryBytes == 0 {
		opts.MaxQueryBytes = DefaultMaxQueryBytes
	}
	if opts.Jobs <= 0 {
		opts.Jobs = defaultBuildJobs()
	}
	if opts.MemoryLimit < MinimumBuildMemoryLimit {
		return ShardedMetadata{}, fmt.Errorf("memory limit %d is too small; minimum is %d", opts.MemoryLimit, MinimumBuildMemoryLimit)
	}
	if opts.ShardSize == 0 {
		opts.ShardSize = shardSizeForMemoryLimit(opts.MemoryLimit)
	}
	if opts.MaxQueryBytes >= opts.ShardSize {
		return ShardedMetadata{}, fmt.Errorf("max query bytes %d must be smaller than computed shard input size %d; increase -memory-limit or lower -max-query-bytes", opts.MaxQueryBytes, opts.ShardSize)
	}

	reportProgress(progress, BuildProgress{Phase: BuildPhaseCollecting, Message: "collecting input files"})
	files, err := collectFiles(paths, opts.Recursive)
	if err != nil {
		return ShardedMetadata{}, fmt.Errorf("collect input files: %w", err)
	}
	if len(files) == 0 {
		return ShardedMetadata{}, ErrEmptyIndex
	}
	reportProgress(progress, BuildProgress{Phase: BuildPhaseCollecting, Message: "collected input files", Current: len(files), Total: len(files)})

	reportProgress(progress, BuildProgress{Phase: BuildPhasePlanning, Message: "planning build shards", Total: len(files)})
	groups, sizes, err := planSourceSegments(files, opts.ShardSize, opts.MaxQueryBytes)
	if err != nil {
		return ShardedMetadata{}, fmt.Errorf("plan shards: %w", err)
	}
	reportProgress(progress, BuildProgress{Phase: BuildPhasePlanning, Message: "planned build shards", Current: len(groups), Total: len(groups)})

	shardDir := out + ".shards"
	rawDir := out + ".raw"
	if len(groups) > 1 {
		if err := os.MkdirAll(shardDir, 0o755); err != nil {
			return ShardedMetadata{}, fmt.Errorf("create shard directory %q: %w", shardDir, err)
		}
		if err := os.MkdirAll(rawDir, 0o755); err != nil {
			return ShardedMetadata{}, fmt.Errorf("create raw shard directory %q: %w", rawDir, err)
		}
	}

	h := sha256.New()
	meta := ShardedMetadata{
		Version:       version,
		FileCount:     len(files),
		CreatedAt:     time.Now().UTC(),
		Recursive:     opts.Recursive,
		InputPaths:    append([]string(nil), paths...),
		MemoryLimit:   opts.MemoryLimit,
		MaxQueryBytes: opts.MaxQueryBytes,
		Jobs:          opts.Jobs,
		ShardSize:     opts.ShardSize,
		ShardCount:    len(groups),
		IndexedFiles:  files,
	}

	shards, err := buildShardGroups(groups, sizes, out, shardDir, rawDir, opts.Jobs, progress)
	if err != nil {
		return ShardedMetadata{}, err
	}
	meta.Shards = shards
	if len(groups) > 1 {
		ngramPath := out + ".ngr"
		reportProgress(progress, BuildProgress{Phase: BuildPhaseWriting, Message: "writing ngram prefilter", Current: 1, Total: 2, Path: ngramPath})
		if err := writeNgramFilter(ngramPath, groups); err != nil {
			return ShardedMetadata{}, fmt.Errorf("write ngram filter: %w", err)
		}
		meta.NgramFilter = filepath.ToSlash(filepath.Base(ngramPath))

		trigramPath := out + ".tri"
		reportProgress(progress, BuildProgress{Phase: BuildPhaseWriting, Message: "writing trigram prefilter", Current: 2, Total: 2, Path: trigramPath})
		if err := writeTrigramIndex(trigramPath, groups, len(groups)); err != nil {
			return ShardedMetadata{}, fmt.Errorf("write trigram index: %w", err)
		}
		meta.TrigramIndex = filepath.ToSlash(filepath.Base(trigramPath))
	}

	var hashedBytes uint64
	for i, file := range files {
		size, err := hashFile(h, file)
		if err != nil {
			return ShardedMetadata{}, fmt.Errorf("hash indexed file %q: %w", file, err)
		}
		meta.TotalBytes += size
		hashedBytes += size
		reportProgress(progress, BuildProgress{
			Phase:        BuildPhaseHashing,
			Message:      "hashing indexed files",
			Current:      i + 1,
			Total:        len(files),
			CurrentBytes: hashedBytes,
			Path:         file,
		})
	}

	meta.SHA256 = fmt.Sprintf("%x", h.Sum(nil))

	if len(groups) == 1 {
		reportProgress(progress, BuildProgress{Phase: BuildPhaseWriting, Message: "writing metadata", Current: 1, Total: 1, Path: out + ".meta"})
		if err := writeMeta(out+".meta", Metadata{
			Version:      version,
			FileCount:    meta.FileCount,
			TotalBytes:   meta.TotalBytes,
			SHA256:       meta.SHA256,
			CreatedAt:    meta.CreatedAt,
			Recursive:    meta.Recursive,
			InputPaths:   meta.InputPaths,
			IndexedFiles: meta.IndexedFiles,
		}); err != nil {
			return ShardedMetadata{}, fmt.Errorf("write metadata: %w", err)
		}
		reportProgress(progress, BuildProgress{Phase: BuildPhaseDone, Message: "build complete", Current: meta.ShardCount, Total: meta.ShardCount, CurrentBytes: meta.TotalBytes, TotalBytes: meta.TotalBytes})
		return meta, nil
	}

	reportProgress(progress, BuildProgress{Phase: BuildPhaseWriting, Message: "writing shard manifest", Current: 1, Total: 2, Path: out})
	if err := writeShardManifest(out, meta); err != nil {
		return ShardedMetadata{}, fmt.Errorf("write shard manifest: %w", err)
	}
	reportProgress(progress, BuildProgress{Phase: BuildPhaseWriting, Message: "writing shard metadata", Current: 2, Total: 2, Path: out + ".meta"})
	if err := writeShardManifest(out+".meta", meta); err != nil {
		return ShardedMetadata{}, fmt.Errorf("write shard metadata: %w", err)
	}
	reportProgress(progress, BuildProgress{Phase: BuildPhaseDone, Message: "build complete", Current: meta.ShardCount, Total: meta.ShardCount, CurrentBytes: meta.TotalBytes, TotalBytes: meta.TotalBytes})
	return meta, nil
}

// OpenSearch opens either a single FM-index file or a sharded manifest.
func OpenSearch(path string) (Searcher, error) {
	if isShardManifest(path) {
		return OpenSharded(path)
	}
	return Open(path)
}

// OpenSharded opens a shard manifest without loading all shards into memory.
func OpenSharded(path string) (*ShardedIndex, error) {
	meta, err := readShardManifest(path)
	if err != nil {
		return nil, fmt.Errorf("read shard manifest %q: %w", path, err)
	}
	return &ShardedIndex{
		baseDir: filepath.Dir(path),
		meta:    meta,
	}, nil
}

// Contains reports whether q exists in any shard.
func (idx *ShardedIndex) Contains(q string) bool {
	return idx.ContainsBytes([]byte(q))
}

// ContainsBytes reports whether q exists in any shard.
func (idx *ShardedIndex) ContainsBytes(q []byte) bool {
	if idx == nil || len(q) == 0 {
		return false
	}
	if idx.meta.MaxQueryBytes > 0 && uint64(len(q)) > idx.meta.MaxQueryBytes {
		return false
	}
	return idx.containsBytesParallel(q)
}

// MaxQueryBytes returns the longest query guaranteed to be correct.
func (idx *ShardedIndex) MaxQueryBytes() uint64 {
	if idx == nil {
		return 0
	}
	return idx.meta.MaxQueryBytes
}

func (idx *ShardedIndex) containsBytesParallel(q []byte) bool {
	candidates, ok := idx.trigramCandidates(q)
	if ok && len(candidates) == 0 {
		return false
	}

	jobs := defaultBuildJobs()
	totalShards := len(idx.meta.Shards)
	if ok {
		totalShards = len(candidates)
	}
	if jobs > totalShards {
		jobs = totalShards
	}
	if jobs < 1 {
		return false
	}

	shards := make(chan Shard)
	found := make(chan struct{})
	done := make(chan struct{})
	var closeFound sync.Once
	var workers sync.WaitGroup

	for workerIndex := 0; workerIndex < jobs; workerIndex++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-done:
					return
				case shard, ok := <-shards:
					if !ok {
						return
					}
					if idx.shardContainsBytes(shard, q) {
						closeFound.Do(func() {
							close(found)
						})
						return
					}
				}
			}
		}()
	}

	go func() {
		defer close(shards)
		for position, shard := range idx.meta.Shards {
			if ok && !candidateContains(candidates, position) {
				continue
			}
			select {
			case <-found:
				return
			case shards <- shard:
			}
		}
	}()

	go func() {
		workers.Wait()
		close(done)
	}()

	select {
	case <-found:
		return true
	case <-done:
		return false
	}
}

func (idx *ShardedIndex) shardContainsBytes(shard Shard, q []byte) bool {
	if shard.RawPath != "" {
		ok, err := idx.rawShardContainsBytes(shard, q)
		if err == nil {
			return ok
		}
	}

	path := shard.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(idx.baseDir, filepath.FromSlash(path))
	}
	shardIndex, err := Open(path)
	if err != nil {
		return false
	}
	return shardIndex.ContainsBytes(q)
}

// rawShardContainsBytes verifies candidate shards from length-delimited raw
// segments. Scanning per segment preserves file and chunk boundaries while
// avoiding the heavier FM-index load on common one-shot CLI lookups.
func (idx *ShardedIndex) rawShardContainsBytes(shard Shard, q []byte) (bool, error) {
	path := shard.RawPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(idx.baseDir, filepath.FromSlash(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open raw shard %q: %w", path, err)
	}
	defer file.Close()

	var data []byte
	for {
		var length uint64
		if err := binary.Read(file, binary.LittleEndian, &length); err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("read raw shard segment length %q: %w", path, err)
		}
		if length == 0 {
			continue
		}
		if length > uint64(int(^uint(0)>>1)) {
			return false, fmt.Errorf("raw shard segment %q is too large to scan: %d bytes", path, length)
		}
		if cap(data) < int(length) {
			data = make([]byte, int(length))
		}
		data = data[:int(length)]
		if _, err := io.ReadFull(file, data); err != nil {
			return false, fmt.Errorf("read raw shard segment %q: %w", path, err)
		}
		if bytes.Contains(data, q) {
			return true, nil
		}
	}
}

func (idx *ShardedIndex) trigramCandidates(q []byte) ([]int, bool) {
	if candidates, ok := idx.ngramCandidates(q); ok {
		return candidates, true
	}
	if len(q) < 3 || idx.meta.TrigramIndex == "" {
		return nil, false
	}
	path := idx.meta.TrigramIndex
	if !filepath.IsAbs(path) {
		path = filepath.Join(idx.baseDir, filepath.FromSlash(path))
	}
	candidates, err := readTrigramCandidates(path, q)
	if err != nil {
		return nil, false
	}
	return candidates, true
}

func (idx *ShardedIndex) ngramCandidates(q []byte) ([]int, bool) {
	if len(q) < ngramFilterN || idx.meta.NgramFilter == "" {
		return nil, false
	}
	path := idx.meta.NgramFilter
	if !filepath.IsAbs(path) {
		path = filepath.Join(idx.baseDir, filepath.FromSlash(path))
	}
	candidates, err := readNgramCandidates(path, q)
	if err != nil {
		return nil, false
	}
	return candidates, true
}

func shardSizeForMemoryLimit(memoryLimit uint64) uint64 {
	return memoryLimit / builderMemoryMultiplier
}

func defaultBuildJobs() int {
	cpus := runtime.NumCPU()
	if cpus < 1 {
		return 1
	}
	return cpus
}

func reportProgress(progress ProgressFunc, update BuildProgress) {
	if progress != nil {
		progress(update)
	}
}

func buildShardGroups(groups [][]sourceSegment, sizes []uint64, out string, shardDir string, rawDir string, jobs int, progress ProgressFunc) ([]Shard, error) {
	if jobs < 1 {
		jobs = 1
	}
	if jobs > len(groups) {
		jobs = len(groups)
	}

	tasks := make(chan shardBuildTask)
	results := make(chan shardBuildResult, len(groups))

	var workers sync.WaitGroup
	for workerIndex := 0; workerIndex < jobs; workerIndex++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for task := range tasks {
				results <- buildOneShard(task, len(groups), progress)
			}
		}()
	}

	go func() {
		for index, group := range groups {
			shardOut := out
			shardPath := out
			rawOut := ""
			rawPath := ""
			if len(groups) > 1 {
				name := fmt.Sprintf("shard-%06d.fm", index+1)
				shardOut = filepath.Join(shardDir, name)
				shardPath = filepath.ToSlash(filepath.Join(filepath.Base(shardDir), name))
				rawName := fmt.Sprintf("shard-%06d.raw", index+1)
				rawOut = filepath.Join(rawDir, rawName)
				rawPath = filepath.ToSlash(filepath.Join(filepath.Base(rawDir), rawName))
			}
			tasks <- shardBuildTask{
				index:   index,
				out:     shardOut,
				path:    shardPath,
				rawOut:  rawOut,
				rawPath: rawPath,
				group:   group,
				size:    sizes[index],
			}
		}
		close(tasks)
		workers.Wait()
		close(results)
	}()

	shards := make([]Shard, len(groups))
	for result := range results {
		if result.err != nil {
			return nil, result.err
		}
		shards[result.index] = result.shard
	}
	return shards, nil
}

func buildOneShard(task shardBuildTask, total int, progress ProgressFunc) shardBuildResult {
	reportProgress(progress, BuildProgress{
		Phase:        BuildPhaseBuilding,
		Message:      "building shard",
		Current:      task.index + 1,
		Total:        total,
		CurrentBytes: task.size,
		Path:         task.path,
	})

	idx, indexedBytes, err := buildFromSegments(task.group)
	if err != nil {
		return shardBuildResult{index: task.index, err: fmt.Errorf("build shard %d: %w", task.index+1, err)}
	}
	if indexedBytes != task.size {
		return shardBuildResult{index: task.index, err: fmt.Errorf("build shard %d: indexed %d bytes, expected %d", task.index+1, indexedBytes, task.size)}
	}
	if err := idx.Save(task.out); err != nil {
		return shardBuildResult{index: task.index, err: fmt.Errorf("save shard %q: %w", task.out, err)}
	}
	if task.rawOut != "" {
		if err := writeRawShard(task.rawOut, task.group); err != nil {
			return shardBuildResult{index: task.index, err: fmt.Errorf("write raw shard %q: %w", task.rawOut, err)}
		}
	}
	return shardBuildResult{
		index: task.index,
		shard: Shard{
			Path:       task.path,
			RawPath:    task.rawPath,
			FileCount:  countUniqueSegmentFiles(task.group),
			TotalBytes: task.size,
			Files:      uniqueSegmentFiles(task.group),
		},
	}
}

func planSourceSegments(files []string, shardBytes uint64, maxQueryBytes uint64) ([][]sourceSegment, []uint64, error) {
	var groups [][]sourceSegment
	var sizes []uint64
	var current []sourceSegment
	var currentSize uint64

	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			return nil, nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		segments, err := splitFileIntoSegments(file, uint64(info.Size()), shardBytes, maxQueryBytes)
		if err != nil {
			return nil, nil, err
		}
		for _, segment := range segments {
			if len(current) > 0 && currentSize+segment.length > shardBytes {
				groups = append(groups, current)
				sizes = append(sizes, currentSize)
				current = nil
				currentSize = 0
			}
			current = append(current, segment)
			currentSize += segment.length
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
		sizes = append(sizes, currentSize)
	}
	if len(groups) == 0 {
		return nil, nil, ErrEmptyIndex
	}
	return groups, sizes, nil
}

// splitFileIntoSegments overlaps large file chunks so every query up to
// maxQueryBytes is fully contained in at least one segment.
func splitFileIntoSegments(path string, fileSize uint64, shardBytes uint64, maxQueryBytes uint64) ([]sourceSegment, error) {
	if fileSize <= shardBytes {
		return []sourceSegment{{path: path, length: fileSize}}, nil
	}

	overlap := maxQueryBytes - 1
	step := shardBytes - overlap
	if step == 0 {
		return nil, fmt.Errorf("computed chunk step is zero for file %q", path)
	}

	var segments []sourceSegment
	for start := uint64(0); start < fileSize; start += step {
		length := shardBytes
		if remaining := fileSize - start; remaining < length {
			length = remaining
		}
		segments = append(segments, sourceSegment{
			path:   path,
			offset: int64(start),
			length: length,
		})
		if start+length >= fileSize {
			break
		}
	}
	return segments, nil
}

func buildFromSegments(segments []sourceSegment) (*Index, uint64, error) {
	text, total, err := readSegmentSymbols(segments)
	if err != nil {
		return nil, 0, err
	}
	text = append(text, terminalSym)

	idx, err := buildFromSymbols(text)
	if err != nil {
		return nil, 0, err
	}
	return idx, total, nil
}

func readSegmentSymbols(segments []sourceSegment) ([]uint16, uint64, error) {
	var text []uint16
	var total uint64
	buf := make([]byte, 1024*1024)

	for segmentIndex, segment := range segments {
		if segmentIndex > 0 {
			text = append(text, separator)
		}
		readBytes, err := appendSegmentSymbols(&text, buf, segment)
		if err != nil {
			return nil, 0, err
		}
		total += readBytes
	}
	return text, total, nil
}

func appendSegmentSymbols(text *[]uint16, buf []byte, segment sourceSegment) (uint64, error) {
	file, err := os.Open(segment.path)
	if err != nil {
		return 0, fmt.Errorf("open segment source %q: %w", segment.path, err)
	}
	defer file.Close()

	if _, err := file.Seek(segment.offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek segment source %q: %w", segment.path, err)
	}

	var total uint64
	remaining := segment.length
	for remaining > 0 {
		want := uint64(len(buf))
		if remaining < want {
			want = remaining
		}
		n, readErr := file.Read(buf[:want])
		if n > 0 {
			for _, b := range buf[:n] {
				*text = append(*text, byteToSym(b))
			}
			total += uint64(n)
			remaining -= uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("read segment source %q: %w", segment.path, readErr)
		}
	}
	if total != segment.length {
		return 0, fmt.Errorf("read segment source %q: got %d bytes, expected %d", segment.path, total, segment.length)
	}
	return total, nil
}

// writeRawShard stores indexed bytes as length-delimited segments. The query
// path can then verify candidate shards without reading the original files or
// matching across internal separators.
func writeRawShard(path string, segments []sourceSegment) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	buf := make([]byte, 1024*1024)
	for _, segment := range segments {
		if err := binary.Write(file, binary.LittleEndian, segment.length); err != nil {
			return fmt.Errorf("write raw segment length: %w", err)
		}
		if _, err := copySegmentBytes(file, buf, segment); err != nil {
			return err
		}
	}
	return nil
}

func copySegmentBytes(dst io.Writer, buf []byte, segment sourceSegment) (uint64, error) {
	file, err := os.Open(segment.path)
	if err != nil {
		return 0, fmt.Errorf("open raw segment source %q: %w", segment.path, err)
	}
	defer file.Close()

	if _, err := file.Seek(segment.offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek raw segment source %q: %w", segment.path, err)
	}

	var total uint64
	remaining := segment.length
	for remaining > 0 {
		want := uint64(len(buf))
		if remaining < want {
			want = remaining
		}
		n, readErr := file.Read(buf[:want])
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return 0, fmt.Errorf("write raw segment source %q: %w", segment.path, writeErr)
			}
			if written != n {
				return 0, fmt.Errorf("write raw segment source %q: wrote %d bytes, expected %d", segment.path, written, n)
			}
			total += uint64(n)
			remaining -= uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("read raw segment source %q: %w", segment.path, readErr)
		}
	}
	if total != segment.length {
		return 0, fmt.Errorf("copy raw segment source %q: got %d bytes, expected %d", segment.path, total, segment.length)
	}
	return total, nil
}

func uniqueSegmentFiles(segments []sourceSegment) []string {
	seen := make(map[string]struct{})
	var files []string
	for _, segment := range segments {
		if _, ok := seen[segment.path]; ok {
			continue
		}
		seen[segment.path] = struct{}{}
		files = append(files, segment.path)
	}
	return files
}

func countUniqueSegmentFiles(segments []sourceSegment) int {
	return len(uniqueSegmentFiles(segments))
}

func writeNgramFilter(path string, groups [][]sourceSegment) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := file.Write([]byte(ngramFilterMagic)); err != nil {
		return err
	}
	for _, value := range []uint32{
		uint32(version),
		uint32(len(groups)),
		uint32(ngramFilterBytes),
		uint32(ngramFilterN),
		uint32(ngramFilterHashes),
		0,
	} {
		if err := binary.Write(file, binary.LittleEndian, value); err != nil {
			return err
		}
	}

	for _, group := range groups {
		filter := make([]byte, ngramFilterBytes)
		for _, segment := range group {
			if err := addSegmentNgrams(filter, segment); err != nil {
				return err
			}
		}
		if _, err := file.Write(filter); err != nil {
			return err
		}
	}
	return nil
}

func addSegmentNgrams(filter []byte, segment sourceSegment) error {
	return scanSegmentWindows(segment, ngramFilterN, func(window []byte) {
		addNgramToFilter(filter, window)
	})
}

func scanSegmentWindows(segment sourceSegment, windowSize int, visit func([]byte)) error {
	file, err := os.Open(segment.path)
	if err != nil {
		return fmt.Errorf("open ngram source %q: %w", segment.path, err)
	}
	defer file.Close()

	if _, err := file.Seek(segment.offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek ngram source %q: %w", segment.path, err)
	}

	window := make([]byte, 0, windowSize)
	buf := make([]byte, 1024*1024)
	remaining := segment.length
	for remaining > 0 {
		want := uint64(len(buf))
		if remaining < want {
			want = remaining
		}
		n, readErr := file.Read(buf[:want])
		if n > 0 {
			for _, value := range buf[:n] {
				if len(window) < windowSize {
					window = append(window, value)
				} else {
					copy(window, window[1:])
					window[windowSize-1] = value
				}
				if len(window) == windowSize {
					visit(window)
				}
			}
			remaining -= uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read ngram source %q: %w", segment.path, readErr)
		}
	}
	return nil
}

func addNgramToFilter(filter []byte, ngram []byte) {
	hash1, hash2 := ngramHashes(ngram)
	bits := uint64(len(filter) * 8)
	for i := 0; i < ngramFilterHashes; i++ {
		bit := (hash1 + uint64(i)*hash2) % bits
		filter[bit/8] |= 1 << (bit % 8)
	}
}

func readNgramCandidates(path string, q []byte) ([]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	header := make([]byte, len(ngramFilterMagic))
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, err
	}
	if string(header) != ngramFilterMagic {
		return nil, ErrBadFormat
	}

	var filterVersion, shardCount, filterBytes, ngramSize, hashCount, reserved uint32
	for _, value := range []*uint32{&filterVersion, &shardCount, &filterBytes, &ngramSize, &hashCount, &reserved} {
		if err := binary.Read(file, binary.LittleEndian, value); err != nil {
			return nil, err
		}
	}
	if filterVersion != version {
		return nil, fmt.Errorf("%w: unsupported ngram filter version %d", ErrBadFormat, filterVersion)
	}
	if int(ngramSize) != ngramFilterN || int(hashCount) != ngramFilterHashes {
		return nil, fmt.Errorf("%w: unsupported ngram filter parameters", ErrBadFormat)
	}

	var candidates []int
	dataOffset := int64(len(ngramFilterMagic) + 24)
	for shardIndex := 0; shardIndex < int(shardCount); shardIndex++ {
		if shardFilterMayContain(file, dataOffset, int(filterBytes), shardIndex, q) {
			candidates = append(candidates, shardIndex)
		}
	}
	return candidates, nil
}

func shardFilterMayContain(file *os.File, dataOffset int64, filterBytes int, shardIndex int, q []byte) bool {
	filterOffset := dataOffset + int64(shardIndex*filterBytes)
	var one [1]byte
	bits := uint64(filterBytes * 8)
	for start := 0; start <= len(q)-ngramFilterN; start++ {
		hash1, hash2 := ngramHashes(q[start : start+ngramFilterN])
		for i := 0; i < ngramFilterHashes; i++ {
			bit := (hash1 + uint64(i)*hash2) % bits
			if _, err := file.ReadAt(one[:], filterOffset+int64(bit/8)); err != nil {
				return true
			}
			if one[0]&(1<<(bit%8)) == 0 {
				return false
			}
		}
	}
	return true
}

func ngramHashes(data []byte) (uint64, uint64) {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	hash1 := uint64(offset64)
	hash2 := uint64(0x9e3779b97f4a7c15)
	for _, value := range data {
		hash1 ^= uint64(value)
		hash1 *= prime64
		hash2 ^= uint64(value) + 0x9e3779b97f4a7c15 + (hash2 << 6) + (hash2 >> 2)
	}
	hash2 |= 1
	return hash1, hash2
}

func writeTrigramIndex(path string, groups [][]sourceSegment, shardCount int) error {
	shardBytes := (shardCount + 7) / 8
	tableSize := trigramCount * shardBytes
	table := make([]byte, tableSize)

	for shardIndex, group := range groups {
		for _, segment := range group {
			if err := addSegmentTrigrams(table, shardBytes, shardIndex, segment); err != nil {
				return err
			}
		}
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := file.Write([]byte(trigramMagic)); err != nil {
		return err
	}
	for _, value := range []uint32{uint32(version), uint32(shardCount), uint32(shardBytes), 0} {
		if err := binary.Write(file, binary.LittleEndian, value); err != nil {
			return err
		}
	}
	if _, err := file.Write(table); err != nil {
		return err
	}
	return nil
}

func addSegmentTrigrams(table []byte, shardBytes int, shardIndex int, segment sourceSegment) error {
	file, err := os.Open(segment.path)
	if err != nil {
		return fmt.Errorf("open trigram source %q: %w", segment.path, err)
	}
	defer file.Close()

	if _, err := file.Seek(segment.offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek trigram source %q: %w", segment.path, err)
	}

	var window [3]byte
	windowLen := 0
	buf := make([]byte, 1024*1024)
	remaining := segment.length
	for remaining > 0 {
		want := uint64(len(buf))
		if remaining < want {
			want = remaining
		}
		n, readErr := file.Read(buf[:want])
		if n > 0 {
			for _, value := range buf[:n] {
				if windowLen < len(window) {
					window[windowLen] = value
					windowLen++
				} else {
					window[0], window[1], window[2] = window[1], window[2], value
				}
				if windowLen == len(window) {
					setTrigramShardBit(table, shardBytes, shardIndex, trigramKey(window[:]))
				}
			}
			remaining -= uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read trigram source %q: %w", segment.path, readErr)
		}
	}
	return nil
}

func readTrigramCandidates(path string, q []byte) ([]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	header := make([]byte, len(trigramMagic))
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, err
	}
	if string(header) != trigramMagic {
		return nil, ErrBadFormat
	}

	var triVersion, shardCount, shardBytes, reserved uint32
	for _, value := range []*uint32{&triVersion, &shardCount, &shardBytes, &reserved} {
		if err := binary.Read(file, binary.LittleEndian, value); err != nil {
			return nil, err
		}
	}
	if triVersion != version {
		return nil, fmt.Errorf("%w: unsupported trigram index version %d", ErrBadFormat, triVersion)
	}

	candidates := make([]byte, shardBytes)
	for i := range candidates {
		candidates[i] = 0xff
	}
	maskUnusedShardBits(candidates, int(shardCount))

	row := make([]byte, shardBytes)
	dataOffset := int64(len(trigramMagic) + 16)
	for start := 0; start <= len(q)-3; start++ {
		key := trigramKey(q[start : start+3])
		offset := dataOffset + int64(key)*int64(shardBytes)
		if _, err := file.ReadAt(row, offset); err != nil {
			return nil, err
		}
		for i := range candidates {
			candidates[i] &= row[i]
		}
		if bitsetIsZero(candidates) {
			return nil, nil
		}
	}

	return bitsetShardIndexes(candidates, int(shardCount)), nil
}

func trigramKey(data []byte) int {
	return int(data[0])<<16 | int(data[1])<<8 | int(data[2])
}

func setTrigramShardBit(table []byte, shardBytes int, shardIndex int, key int) {
	offset := key*shardBytes + shardIndex/8
	table[offset] |= 1 << (shardIndex % 8)
}

func maskUnusedShardBits(bits []byte, shardCount int) {
	if shardCount == 0 || len(bits) == 0 {
		return
	}
	usedBits := shardCount % 8
	if usedBits == 0 {
		return
	}
	bits[len(bits)-1] &= byte((1 << usedBits) - 1)
}

func bitsetIsZero(bits []byte) bool {
	for _, value := range bits {
		if value != 0 {
			return false
		}
	}
	return true
}

func bitsetShardIndexes(bits []byte, shardCount int) []int {
	var indexes []int
	for shardIndex := 0; shardIndex < shardCount; shardIndex++ {
		if bits[shardIndex/8]&(1<<(shardIndex%8)) != 0 {
			indexes = append(indexes, shardIndex)
		}
	}
	return indexes
}

func candidateContains(candidates []int, shardIndex int) bool {
	position := sort.SearchInts(candidates, shardIndex)
	return position < len(candidates) && candidates[position] == shardIndex
}

func writeShardManifest(path string, meta ShardedMetadata) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if _, err := w.WriteString(shardMagic + "\n"); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return err
	}
	return w.Flush()
}

func readShardManifest(path string) (ShardedMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ShardedMetadata{}, err
	}
	prefix := shardMagic + "\n"
	if !strings.HasPrefix(string(data), prefix) {
		return ShardedMetadata{}, ErrBadFormat
	}
	var meta ShardedMetadata
	if err := json.Unmarshal(data[len(prefix):], &meta); err != nil {
		return ShardedMetadata{}, err
	}
	if meta.Version != version {
		return ShardedMetadata{}, fmt.Errorf("%w: unsupported shard version %d", ErrBadFormat, meta.Version)
	}
	if len(meta.Shards) == 0 {
		return ShardedMetadata{}, errors.New("fmindex: shard manifest has no shards")
	}
	return meta, nil
}

func isShardManifest(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	header := make([]byte, len(shardMagic))
	if _, err := f.Read(header); err != nil {
		return false
	}
	return string(header) == shardMagic
}

func hashFile(h hash.Hash, path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, err
	}
	return uint64(n), nil
}
