# Benchmarks

This document records build-time experiments that affect implementation
choices. Benchmarks are not promises for every dataset; they are evidence for
why the current builder uses a specific approach.

## Suffix Array Builder

FM-index construction needs BWT, and BWT construction needs suffix order:

```text
BWT[i] = T[SA[i] - 1]
```

The first implementation used a custom prefix-doubling suffix-array builder.
That implementation was simple and pure Go, but it became the dominant build
cost.

We benchmarked three builders:

- `PrefixDoubling`: the original in-project prefix-doubling builder
- `InternalSAIS`: Go standard-library SA-IS adapted into `internal/sais`
- `StdlibSAIS`: `index/suffixarray.New`, used only as a reference point

`StdlibSAIS` cannot be used directly for BWT construction because it does not
export the suffix array. `InternalSAIS` exposes the suffix array while keeping
the project pure Go.

Command:

```sh
go test ./fmindex -run '^$' -bench 'BenchmarkSuffixArray' -benchmem -benchtime=1x
```

Environment:

```text
goos: darwin
goarch: arm64
cpu: Apple M4
```

Results:

```text
BenchmarkSuffixArrayPrefixDoubling256K-10   90.45 ms   2.90 MB/s   10.5 MB alloc
BenchmarkSuffixArrayInternalSAIS256K-10      6.52 ms  40.22 MB/s    4.4 MB alloc
BenchmarkSuffixArrayStdlibSAIS256K-10        5.50 ms  47.68 MB/s    1.2 MB alloc

BenchmarkSuffixArrayPrefixDoubling1M-10    443.16 ms   2.37 MB/s   42.0 MB alloc
BenchmarkSuffixArrayInternalSAIS1M-10       25.86 ms  40.54 MB/s   17.5 MB alloc
BenchmarkSuffixArrayStdlibSAIS1M-10         22.23 ms  47.18 MB/s    4.9 MB alloc

BenchmarkSuffixArrayPrefixDoubling4M-10       5.74 s   0.73 MB/s  167.8 MB alloc
BenchmarkSuffixArrayInternalSAIS4M-10       154.68 ms  27.12 MB/s   69.9 MB alloc
BenchmarkSuffixArrayStdlibSAIS4M-10         112.03 ms  37.44 MB/s   19.6 MB alloc
```

Conclusion:

`InternalSAIS` is much faster than the prefix-doubling builder on this
code-like corpus. The 4 MiB benchmark improved from about 5.7 seconds to about
155 milliseconds.

The standard-library reference remains faster and allocates less because it
works directly on byte text and keeps its suffix array internal. Our FM-index
uses a larger alphabet with internal separator and terminal symbols, so
`InternalSAIS` uses an `int32` symbol path and converts the resulting suffix
array into the builder's native representation.

Current decision:

```text
Use InternalSAIS for FM-index construction.
Keep PrefixDoubling only as a benchmark reference.
```

## Live Dataset Observation

The synthetic benchmark above was followed by a real build test on the scanner
host.

Command:

```sh
./fmindex_linux_amd64 build -r -o cache.fm -memory-limit 1G -jobs 2 /samples/clean
```

Dataset:

```text
/samples/clean
```

Both runs were interrupted before completion, so the numbers below are
directional rather than full-build timings. They are still useful because the
progress counter reports the same build phase: shard construction.

Observed before switching to `InternalSAIS`:

```text
building shard 72/141 elapsed=21m45s
```

Observed after switching to `InternalSAIS`:

```text
building shard 60/141 elapsed=1m13s
```

Approximate shard-build rate:

```text
PrefixDoubling: 1305s / 72 shards = ~18.1s per shard
InternalSAIS:     73s / 60 shards = ~1.2s per shard
```

Approximate observed speedup:

```text
~15x per shard
```

Conclusion:

The SA-IS change produced a large practical build-time improvement on the live
dataset, not only on the synthetic benchmark corpus.

## Negative Query Observation

A live negative query exposed a separate runtime issue after build-time
optimization was complete.

Command:

```sh
time ./fmindex_linux_amd64 query -i cache.fm "base64_decode(eval"
```

Observed before query prefiltering:

```text
FALSE

real  0m49.018s
user  1m34.409s
sys   0m2.761s
```

Reason:

`TRUE` queries can return as soon as an early shard matches. `FALSE` queries
must prove absence across every shard. Before prefiltering, that meant opening
and reading every `.fm` shard file.

Current mitigation:

Sharded builds write an exact trigram-to-shard sidecar file, `cache.fm.tri`.
For queries at least 3 bytes long, the query path intersects the shard bitsets
for each query trigram before opening any `.fm` shard. If the intersection is
empty, the query returns `FALSE` without reading shard indexes.

Expected effect:

Negative lookups for strings with rare or absent trigrams should become much
faster. Queries shorter than 3 bytes still use the FM-index path directly.

## Positive Query Observation

After adding negative-query prefilters, live false lookups became fast:

```text
time ./fmindex_linux_amd64 query -i cache.fm "base64_decode(eval"

FALSE

real  0m0.335s
user  0m0.315s
sys   0m0.044s
```

Positive lookups still had a separate cost:

```text
time ./fmindex_linux_amd64 query -i cache.fm "json("

TRUE

real  0m2.731s
user  0m4.731s
sys   0m0.260s
```

Reason:

The prefilters can narrow candidate shards, but a positive query still needs to
verify a candidate. Verifying by opening a `.fm` shard is expensive for one-shot
CLI queries because it reads and decodes a large shard index before doing the
actual FM lookup.

Current mitigation:

Sharded builds write `cache.fm.raw/`, a raw segment cache. Query first verifies
candidate shards by scanning these cached segment bytes with exact byte search.
The cache is length-delimited per indexed segment, so it preserves file/chunk
boundaries and cannot create matches across input files.

Tradeoff:

`cache.fm.raw/` adds roughly the indexed input size to disk usage. It does not
replace the FM-index; it is a fast verification path for normal CLI lookups,
with the FM-index path kept as fallback.
