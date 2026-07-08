package fmindex

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestContainsExactSubstrings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	data := []byte("<?php\nbase64_encode(str_replace(\n\x00\xffEND")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	idx, meta, err := Build([]string{path}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if meta.FileCount != 1 || meta.TotalBytes != uint64(len(data)) {
		t.Fatalf("unexpected metadata: %+v", meta)
	}

	for _, q := range [][]byte{
		[]byte("base64_encode(str_replace("),
		[]byte("\x00\xffEND"),
		[]byte("<?php\nbase64"),
	} {
		if !idx.ContainsBytes(q) {
			t.Fatalf("expected %q to exist", q)
		}
	}
	if idx.Contains("Base64_encode") {
		t.Fatal("search should be case-sensitive")
	}
	if idx.Contains("missing") {
		t.Fatal("unexpected match")
	}
}

func TestFileBoundaryDoesNotMatch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("def"), 0o644); err != nil {
		t.Fatal(err)
	}

	idx, _, err := Build([]string{dir}, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("abc") || !idx.Contains("def") {
		t.Fatal("expected per-file matches")
	}
	if idx.Contains("cde") {
		t.Fatal("query matched across a file boundary")
	}
}

func TestSaveOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	out := filepath.Join(dir, "cache.fm")
	if err := os.WriteFile(source, []byte("eval(base64_decode($x));"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := BuildToFile([]string{source}, out, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	idx, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("base64_decode") {
		t.Fatal("round-trip index did not match")
	}
	if idx.Contains("str_replace") {
		t.Fatal("unexpected round-trip match")
	}
	if _, err := os.Stat(out + ".meta"); err != nil {
		t.Fatal(err)
	}
}

func TestRecursiveBuild(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "payload.txt"), []byte("public function sort_users($arGn510VL9Ju075o)"), 0o644); err != nil {
		t.Fatal(err)
	}

	flat, _, err := Build([]string{dir}, BuildOptions{})
	if !errors.Is(err, ErrEmptyIndex) {
		t.Fatal(err)
	}
	if flat != nil && flat.Contains("sort_users") {
		t.Fatal("non-recursive directory build should not include nested files")
	}

	idx, _, err := Build([]string{dir}, BuildOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("sort_users($arGn510VL9Ju075o)") {
		t.Fatal("recursive build missed nested file")
	}
}

func TestShardedBuildAndOpenSearch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	out := filepath.Join(dir, "cache.fm")
	if err := os.WriteFile(a, []byte("first-signature"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("second-signature"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := BuildShardedToFile([]string{dir}, out, ShardedBuildOptions{ShardSize: 20, MaxQueryBytes: 19})
	if err != nil {
		t.Fatal(err)
	}
	if meta.ShardCount != 2 {
		t.Fatalf("expected 2 shards, got %d", meta.ShardCount)
	}

	idx, err := OpenSearch(out)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("first-signature") || !idx.Contains("second-signature") {
		t.Fatal("sharded index missed expected match")
	}
	if idx.Contains("missing-signature") {
		t.Fatal("unexpected sharded match")
	}
}

func TestShardedBuildUsesMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	out := filepath.Join(dir, "cache.fm")
	if err := os.WriteFile(a, []byte("alpha-signature"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("beta-signature"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := BuildShardedToFile([]string{dir}, out, ShardedBuildOptions{MemoryLimit: 1280, MaxQueryBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if meta.MemoryLimit != 1280 {
		t.Fatalf("memory limit = %d, want 1280", meta.MemoryLimit)
	}
	if meta.ShardSize != 20 {
		t.Fatalf("shard size = %d, want 20", meta.ShardSize)
	}
	if meta.ShardCount != 2 {
		t.Fatalf("expected 2 shards, got %d", meta.ShardCount)
	}
}

func TestLargeFileIsChunkedWithOverlap(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "large.txt")
	out := filepath.Join(dir, "cache.fm")
	data := []byte("0123456789ABCDEFGHIJ")
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := BuildShardedToFile([]string{source}, out, ShardedBuildOptions{ShardSize: 10, MaxQueryBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if meta.ShardCount != 3 {
		t.Fatalf("expected 3 shards, got %d", meta.ShardCount)
	}
	if meta.MaxQueryBytes != 5 {
		t.Fatalf("max query bytes = %d, want 5", meta.MaxQueryBytes)
	}

	idx, err := OpenSearch(out)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("89ABC") {
		t.Fatal("chunk-overlap match was not found")
	}
	if idx.Contains("missing") {
		t.Fatal("unexpected chunked match")
	}
}

func TestShardedBuildUsesParallelJobs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cache.fm")
	for index, content := range []string{"one-signature", "two-signature", "three-signature", "four-signature"} {
		path := filepath.Join(dir, string(rune('a'+index))+".txt")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	meta, err := BuildShardedToFile([]string{dir}, out, ShardedBuildOptions{ShardSize: 20, MaxQueryBytes: 10, Jobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Jobs != 2 {
		t.Fatalf("jobs = %d, want 2", meta.Jobs)
	}

	idx, err := OpenSearch(out)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("three") {
		t.Fatal("parallel sharded index missed expected match")
	}
}

func TestTrigramPrefilterRejectsAbsentNGram(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sample.txt")
	out := filepath.Join(dir, "cache.fm")
	if err := os.WriteFile(source, []byte("base64_decode($payload)"), 0o644); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(other, []byte("str_replace($payload)"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := BuildShardedToFile([]string{dir}, out, ShardedBuildOptions{ShardSize: 32, MaxQueryBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Shards) != 2 {
		t.Fatalf("expected two shards, got %d", len(meta.Shards))
	}
	idx, err := OpenSharded(out)
	if err != nil {
		t.Fatal(err)
	}
	candidates, ok := idx.trigramCandidates([]byte("base64_decode"))
	if !ok {
		t.Fatal("trigram index was not available")
	}
	if len(candidates) == 0 {
		t.Fatal("trigram index rejected existing query")
	}
	candidates, ok = idx.trigramCandidates([]byte("definitely_missing"))
	if !ok {
		t.Fatal("trigram index was not available")
	}
	if len(candidates) != 0 {
		t.Fatalf("expected no trigram candidates, got %v", candidates)
	}
}

func TestNgramPrefilterRejectsAbsentLongQuery(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	out := filepath.Join(dir, "cache.fm")
	if err := os.WriteFile(a, []byte("base64_decode_payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("eval_payload_without_joined_prefix"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := BuildShardedToFile([]string{dir}, out, ShardedBuildOptions{ShardSize: 48, MaxQueryBytes: 32}); err != nil {
		t.Fatal(err)
	}
	idx, err := OpenSharded(out)
	if err != nil {
		t.Fatal(err)
	}
	candidates, ok := idx.ngramCandidates([]byte("base64_decode(eval"))
	if !ok {
		t.Fatal("ngram filter was not available")
	}
	if len(candidates) != 0 {
		t.Fatalf("expected no ngram candidates, got %v", candidates)
	}
}

func TestRawShardVerificationFindsCandidate(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	out := filepath.Join(dir, "cache.fm")
	if err := os.WriteFile(a, []byte("base64_decode_payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("json_encode_payload_without_joined_prefix"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := BuildShardedToFile([]string{dir}, out, ShardedBuildOptions{ShardSize: 48, MaxQueryBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Shards) != 2 {
		t.Fatalf("expected two shards, got %d", len(meta.Shards))
	}
	for _, shard := range meta.Shards {
		if shard.RawPath == "" {
			t.Fatalf("raw shard path was not recorded: %+v", shard)
		}
	}

	idx, err := OpenSharded(out)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := idx.rawShardContainsBytes(meta.Shards[0], []byte("base64_decode"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("raw shard verification missed expected match")
	}
	if !idx.Contains("base64_decode") || !idx.Contains("json_encode") {
		t.Fatal("sharded index missed expected raw-cache-backed match")
	}
	if idx.Contains("payloadjson") {
		t.Fatal("raw shard verification matched across a segment boundary")
	}
}
