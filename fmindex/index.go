package fmindex

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jwillberg/fm-index/internal/sais"
)

const (
	fileMagic    = "FMIDX001"
	metaMagic    = "FMMETA001"
	version      = 2
	checkpointN  = 4096
	terminalSym  = uint16(0)
	separator    = uint16(257)
	alphabetSize = 258
)

var (
	// ErrEmptyIndex is returned when a build has no regular files to index.
	ErrEmptyIndex = errors.New("fmindex: empty index")
	// ErrBadFormat is returned when an index or manifest has an unsupported format.
	ErrBadFormat = errors.New("fmindex: bad index format")
)

// Index is an in-memory FM-index loaded from one serialized index file.
type Index struct {
	length     uint64
	c          [alphabetSize + 1]uint64
	bwt        []uint16
	checkEvery uint32
	checks     [][alphabetSize]uint32
}

// Metadata describes a single physical FM-index build.
type Metadata struct {
	Version      int       `json:"version"`
	FileCount    int       `json:"file_count"`
	TotalBytes   uint64    `json:"total_bytes"`
	SHA256       string    `json:"sha256"`
	CreatedAt    time.Time `json:"created_at"`
	Recursive    bool      `json:"recursive"`
	InputPaths   []string  `json:"input_paths"`
	IndexedFiles []string  `json:"indexed_files"`
}

// BuildOptions controls single-index file collection.
type BuildOptions struct {
	Recursive bool
}

// Build creates one FM-index from the selected files.
func Build(paths []string, opts BuildOptions) (*Index, Metadata, error) {
	files, err := collectFiles(paths, opts.Recursive)
	if err != nil {
		return nil, Metadata{}, err
	}
	if len(files) == 0 {
		return nil, Metadata{}, ErrEmptyIndex
	}

	h := sha256.New()
	text, total, err := readSymbols(files, h)
	if err != nil {
		return nil, Metadata{}, err
	}
	text = append(text, terminalSym)

	idx, err := buildFromSymbols(text)
	if err != nil {
		return nil, Metadata{}, err
	}

	meta := Metadata{
		Version:      version,
		FileCount:    len(files),
		TotalBytes:   total,
		SHA256:       fmt.Sprintf("%x", h.Sum(nil)),
		CreatedAt:    time.Now().UTC(),
		Recursive:    opts.Recursive,
		InputPaths:   append([]string(nil), paths...),
		IndexedFiles: files,
	}
	return idx, meta, nil
}

// BuildToFile builds one FM-index and writes it to out.
func BuildToFile(paths []string, out string, opts BuildOptions) (Metadata, error) {
	idx, meta, err := Build(paths, opts)
	if err != nil {
		return Metadata{}, err
	}
	if err := idx.Save(out); err != nil {
		return Metadata{}, err
	}
	if err := writeMeta(out+".meta", meta); err != nil {
		return Metadata{}, err
	}
	return meta, nil
}

// Open loads one serialized FM-index file.
func Open(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	magic := make([]byte, len(fileMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, err
	}
	if string(magic) != fileMagic {
		return nil, ErrBadFormat
	}

	var ver uint32
	if err := binary.Read(r, binary.LittleEndian, &ver); err != nil {
		return nil, err
	}
	if ver != version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrBadFormat, ver)
	}

	idx := &Index{}
	if err := binary.Read(r, binary.LittleEndian, &idx.length); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &idx.checkEvery); err != nil {
		return nil, err
	}
	for i := range idx.c {
		if err := binary.Read(r, binary.LittleEndian, &idx.c[i]); err != nil {
			return nil, err
		}
	}

	idx.bwt = make([]uint16, idx.length)
	for i := range idx.bwt {
		if err := binary.Read(r, binary.LittleEndian, &idx.bwt[i]); err != nil {
			return nil, err
		}
	}

	var checkCount uint64
	if err := binary.Read(r, binary.LittleEndian, &checkCount); err != nil {
		return nil, err
	}
	idx.checks = make([][alphabetSize]uint32, checkCount)
	for i := range idx.checks {
		for j := range idx.checks[i] {
			if err := binary.Read(r, binary.LittleEndian, &idx.checks[i][j]); err != nil {
				return nil, err
			}
		}
	}

	return idx, nil
}

// Save writes the index to path.
func (idx *Index) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if _, err := w.WriteString(fileMagic); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(version)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, idx.length); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, idx.checkEvery); err != nil {
		return err
	}
	for _, v := range idx.c {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	for _, v := range idx.bwt {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	if err := binary.Write(w, binary.LittleEndian, uint64(len(idx.checks))); err != nil {
		return err
	}
	for _, row := range idx.checks {
		for _, v := range row {
			if err := binary.Write(w, binary.LittleEndian, v); err != nil {
				return err
			}
		}
	}
	return w.Flush()
}

// Contains reports whether q exists in this index.
func (idx *Index) Contains(q string) bool {
	return idx.ContainsBytes([]byte(q))
}

// ContainsBytes reports whether q exists in this index.
func (idx *Index) ContainsBytes(q []byte) bool {
	if idx == nil || len(q) == 0 || len(idx.bwt) == 0 {
		return false
	}

	left, right := uint64(0), idx.length
	for i := len(q) - 1; i >= 0; i-- {
		sym := byteToSym(q[i])
		left = idx.c[sym] + idx.occ(sym, left)
		right = idx.c[sym] + idx.occ(sym, right)
		if left >= right {
			return false
		}
	}
	return true
}

// MaxQueryBytes returns zero for a single index, meaning no index-level limit.
func (idx *Index) MaxQueryBytes() uint64 {
	return 0
}

func (idx *Index) occ(sym uint16, pos uint64) uint64 {
	if pos > idx.length {
		pos = idx.length
	}
	block := pos / uint64(idx.checkEvery)
	count := uint64(idx.checks[block][sym])
	start := block * uint64(idx.checkEvery)
	for i := start; i < pos; i++ {
		if idx.bwt[i] == sym {
			count++
		}
	}
	return count
}

func buildFromSymbols(text []uint16) (*Index, error) {
	if len(text) == 0 {
		return nil, ErrEmptyIndex
	}

	counts := [alphabetSize]uint64{}
	for _, sym := range text {
		if sym >= alphabetSize {
			return nil, fmt.Errorf("fmindex: symbol %d outside alphabet", sym)
		}
		counts[sym]++
	}
	sa, err := suffixArray(text)
	if err != nil {
		return nil, err
	}

	idx := &Index{
		length:     uint64(len(text)),
		bwt:        make([]uint16, len(text)),
		checkEvery: checkpointN,
	}

	var sum uint64
	for i := 0; i < alphabetSize; i++ {
		idx.c[i] = sum
		sum += counts[i]
	}
	idx.c[alphabetSize] = sum

	for i, pos := range sa {
		if pos == 0 {
			idx.bwt[i] = text[len(text)-1]
			continue
		}
		idx.bwt[i] = text[pos-1]
	}
	if err := idx.buildCheckpoints(); err != nil {
		return nil, err
	}
	return idx, nil
}

func (idx *Index) buildCheckpoints() error {
	checkCount := int(idx.length/uint64(idx.checkEvery)) + 1
	idx.checks = make([][alphabetSize]uint32, checkCount)

	var counts [alphabetSize]uint64
	for i, sym := range idx.bwt {
		if i%int(idx.checkEvery) == 0 {
			row, err := compactCheckpoint(counts)
			if err != nil {
				return err
			}
			idx.checks[i/int(idx.checkEvery)] = row
		}
		counts[sym]++
	}
	if idx.length%uint64(idx.checkEvery) == 0 {
		row, err := compactCheckpoint(counts)
		if err != nil {
			return err
		}
		idx.checks[checkCount-1] = row
	}
	return nil
}

func compactCheckpoint(counts [alphabetSize]uint64) ([alphabetSize]uint32, error) {
	var row [alphabetSize]uint32
	for symbol, count := range counts {
		if count > math.MaxUint32 {
			return row, fmt.Errorf("fmindex: checkpoint count %d for symbol %d exceeds uint32", count, symbol)
		}
		row[symbol] = uint32(count)
	}
	return row, nil
}

func suffixArray(text []uint16) ([]int, error) {
	symbols := make([]int32, len(text))
	for index, symbol := range text {
		symbols[index] = int32(symbol)
	}

	suffixArray32, err := sais.Int32(symbols, alphabetSize)
	if err != nil {
		return nil, fmt.Errorf("build suffix array with SA-IS: %w", err)
	}

	suffixArray := make([]int, len(suffixArray32))
	for index, suffixStart := range suffixArray32 {
		suffixArray[index] = int(suffixStart)
	}
	return suffixArray, nil
}

func suffixArrayPrefixDoubling(text []uint16) []int {
	n := len(text)
	sa := make([]int, n)
	rank := make([]int, n)
	tmp := make([]int, n)
	work := make([]int, n)
	counts := make([]int, max(n, alphabetSize)+1)
	for i, sym := range text {
		sa[i] = i
		rank[i] = int(sym)
	}
	maxRank := alphabetSize - 1

	for k := 1; k < n; k *= 2 {
		radixSortSuffixRanks(sa, work, counts, rank, k, maxRank)

		tmp[sa[0]] = 0
		for i := 1; i < n; i++ {
			prev, cur := sa[i-1], sa[i]
			tmp[cur] = tmp[prev]
			if rank[prev] != rank[cur] || nextRank(rank, prev, k) != nextRank(rank, cur, k) {
				tmp[cur]++
			}
		}
		copy(rank, tmp)
		maxRank = rank[sa[n-1]]
		if maxRank == n-1 {
			break
		}
	}
	return sa
}

// radixSortSuffixRanks sorts suffixes by the pair (rank[i], rank[i+k]).
// Prefix-doubling calls this repeatedly, so replacing comparison sort here
// removes most of the builder's CPU overhead on multi-megabyte shards.
func radixSortSuffixRanks(sa []int, work []int, counts []int, rank []int, k int, maxRank int) {
	countingSortSuffixRanks(work, sa, counts, rank, k, maxRank)
	countingSortSuffixRanks(sa, work, counts, rank, 0, maxRank)
}

func countingSortSuffixRanks(out []int, in []int, counts []int, rank []int, offset int, maxRank int) {
	counts = counts[:maxRank+2]
	clear(counts)

	for _, suffixStart := range in {
		counts[suffixRankKey(rank, suffixStart, offset)]++
	}

	total := 0
	for key, count := range counts {
		counts[key] = total
		total += count
	}

	for _, suffixStart := range in {
		key := suffixRankKey(rank, suffixStart, offset)
		out[counts[key]] = suffixStart
		counts[key]++
	}
}

func suffixRankKey(rank []int, suffixStart int, offset int) int {
	pos := suffixStart + offset
	if pos >= len(rank) {
		return 0
	}
	return rank[pos] + 1
}

func nextRank(rank []int, pos, k int) int {
	if pos+k >= len(rank) {
		return -1
	}
	return rank[pos+k]
}

func collectFiles(paths []string, recursive bool) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("fmindex: no input paths")
	}

	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if info.Mode().IsRegular() {
				files = append(files, p)
			}
			continue
		}

		if recursive {
			err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.Type().IsRegular() {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}

		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				files = append(files, filepath.Join(p, entry.Name()))
			}
		}
	}

	sort.Strings(files)
	return files, nil
}

func readSymbols(files []string, h hash.Hash) ([]uint16, uint64, error) {
	var text []uint16
	var total uint64
	buf := make([]byte, 1024*1024)

	for fileIndex, path := range files {
		if fileIndex > 0 {
			text = append(text, separator)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		for {
			n, readErr := f.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if _, err := h.Write(chunk); err != nil {
					_ = f.Close()
					return nil, 0, err
				}
				for _, b := range chunk {
					text = append(text, byteToSym(b))
				}
				total += uint64(n)
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				_ = f.Close()
				return nil, 0, readErr
			}
		}
		if err := f.Close(); err != nil {
			return nil, 0, err
		}
	}
	return text, total, nil
}

func byteToSym(b byte) uint16 {
	return uint16(b) + 1
}

func writeMeta(path string, meta Metadata) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if _, err := w.WriteString(metaMagic + "\n"); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return err
	}
	return w.Flush()
}
