# FM-Index Signature Cache

Fast exact substring lookup for byte datasets.

This project builds an FM-index from one or more files and answers one runtime
question:

```text
Does this byte substring exist anywhere in the indexed dataset?
```

The answer is only `TRUE` or `FALSE`. Runtime queries do not scan the original
files and do not return file names, offsets, or occurrence counts.

## Current Status

Implemented:

- Build an index from a file
- Build an index from a directory
- Build an index recursively with `-r`
- Save the index to disk
- Save metadata to disk
- Open an existing index
- Query with `Contains(string) bool`
- Query byte sequences with `ContainsBytes([]byte) bool`
- Query from command-line argument, stdin, or file

Not implemented in version 1:

- Returning offsets
- Returning file names
- Occurrence counts
- Incremental updates
- Memory mapping
- HTTP API

## Important Behavior

Input files are indexed as raw bytes exactly as they are.

The builder does not:

- Normalize text
- Decode content
- Lowercase content
- Trim whitespace
- Change line endings
- Parse PHP, JavaScript, HTML, or any other format

If a file contains bytes, those bytes are what the index sees.

When multiple files are indexed, the builder inserts an internal separator
symbol between files. This separator is not part of any file content. It exists
only to prevent false matches that would otherwise span from the end of one file
into the beginning of the next file.

## Build

Run tests:

```sh
go test ./...
```

Build the CLI:

```sh
go build -o fmindex ./cmd/fmindex
```

Build release binaries for common platforms:

```sh
./build.sh
```

Or run it directly with `go run`.

By default `build.sh` creates binaries in `dist/` for:

- macOS Intel: `fmindex_macos_amd64`
- macOS Apple Silicon: `fmindex_macos_arm64`
- Linux amd64
- Linux arm64
- Linux armv7
- Windows amd64
- Windows arm64

Set `VERSION` to embed a release version:

```sh
VERSION=0.1.0 ./build.sh
```

To stage only the public project files for a GitHub release, use:

```sh
bash scripts/publish-github.sh --dry-run
bash scripts/publish-github.sh
```

This helper stages the public project files only.

The `dist/` directory is build output. It is useful locally for packaging and
release binaries, but it should not be committed as source.

## CLI Usage

Build an index from one file:

```sh
go run ./cmd/fmindex build -o cache.fm sample.txt
```

Build an index from the regular files directly inside a directory:

```sh
go run ./cmd/fmindex build -o cache.fm /path/to/dataset
```

Build an index recursively:

```sh
go run ./cmd/fmindex build -r -o cache.fm /path/to/dataset
```

Build with an explicit memory budget:

```sh
go run ./cmd/fmindex build -r -o cache.fm -memory-limit 512M /path/to/dataset
```

Build with an explicit maximum query size:

```sh
go run ./cmd/fmindex build -r -o cache.fm -max-query-bytes 1M /path/to/dataset
```

Build without progress output:

```sh
go run ./cmd/fmindex build -r -o cache.fm --silent /path/to/dataset
```

Build with detailed progress output:

```sh
go run ./cmd/fmindex build -r -o cache.fm --verbose /path/to/dataset
```

Build with explicit parallelism:

```sh
go run ./cmd/fmindex build -r -o cache.fm -jobs 2 /path/to/dataset
```

On machines with enough RAM, a larger memory budget reduces the number of
shards and can improve total build time:

```sh
go run ./cmd/fmindex build -r -o cache.fm -memory-limit 1G -jobs 2 /path/to/dataset
```

Build from multiple inputs:

```sh
go run ./cmd/fmindex build -r -o cache.fm file1.txt /path/to/dir another-file.php
```

Query an existing index:

```sh
go run ./cmd/fmindex query -i cache.fm "base64_encode(str_replace("
```

Query from stdin:

```sh
printf '%s' '"haku"tähän"' | go run ./cmd/fmindex query -i cache.fm --stdin
```

Query from a file:

```sh
go run ./cmd/fmindex query -i cache.fm --file signature.txt
```

Query many newline-delimited signatures with one process:

```sh
printf '%s\n' 'base64_decode(' 'json(' 'base64_decode(eval' |
  go run ./cmd/fmindex query-batch -i cache.fm
```

Run a local HTTP daemon:

```sh
go run ./cmd/fmindex daemon -i cache.fm --bind 127.0.0.1 --port 8080
```

Query through HTTP:

```sh
curl 'http://127.0.0.1:8080/query?q=base64_decode%28'
```

or send exact bytes in the request body:

```sh
printf '%s' '"haku"tähän"' |
  curl -X POST --data-binary @- 'http://127.0.0.1:8080/query'
```

Output:

```text
TRUE
```

or:

```text
FALSE
```

## CLI Commands

### `build`

```sh
fmindex build [-r] [-o cache.fm] [-memory-limit 512M] [-max-query-bytes 1M] [-jobs n] [--silent] [--verbose] <file-or-dir> [...]
```

Options:

- `-o`: output index path. Default: `cache.fm`
- `-r`: recursively read directories
- `-memory-limit`: approximate builder memory budget. Default: `512M`
- `-max-query-bytes`: maximum query length guaranteed across chunked files. Default: `1M`
- `-jobs`: parallel shard build jobs. Default: CPU count
- `--silent`: hide build progress
- `--verbose`: show detailed build progress, including paths

Build progress is enabled by default and is written to stderr. The final summary
is written to stdout.

The build command creates:

- `cache.fm`: serialized FM-index
- `cache.fm.meta`: JSON metadata

For larger inputs, the builder derives a safe internal shard size from the
memory budget. `cache.fm` becomes a shard manifest and shard index files are
stored in `cache.fm.shards/`. Querying works the same way:

```sh
fmindex query -i cache.fm "needle"
```

The query command detects whether `cache.fm` is a single index or a shard
manifest.

Sharded builds also write prefilter sidecars:

- `cache.fm.ngr`: per-shard 8-byte n-gram filters
- `cache.fm.tri`: exact trigram-to-shard fallback index
- `cache.fm.raw/`: raw shard segment cache for fast candidate verification

For longer queries, the n-gram filter usually rejects non-candidate shards
before opening the heavier `.fm` shard files. This mainly improves negative
lookups. When a shard remains a possible match, query checks `cache.fm.raw/`
first with a direct byte search before falling back to the FM-index. The raw
cache stores indexed segment bytes with internal segment boundaries, so it does
not create cross-file matches and still does not read the original input files.

If one input file is larger than the internal shard size, the builder chunks it
automatically with overlap. The overlap is based on `-max-query-bytes`, so any
query at or below that size is guaranteed to be found even when it crosses a
chunk boundary.

Metadata includes:

- Index version
- File count
- Total indexed bytes
- SHA-256 of indexed file bytes
- Creation time
- Input paths
- Memory limit
- Maximum query bytes
- Indexed file list

### `query`

```sh
fmindex query [-i cache.fm] (<substring> | --stdin | --file path)
```

Options:

- `-i`: input index path. Default: `cache.fm`
- `--stdin`: read query bytes from stdin
- `--file`: read query bytes from a file

The query command opens only the index file. It does not read the original input
files.

Use `--stdin` or `--file` when the substring contains shell-sensitive
characters such as nested quotes, newlines, binary bytes, or long signatures.

### `query-batch`

```sh
fmindex query-batch [-i cache.fm] [--file queries.txt]
```

Options:

- `-i`: input index path. Default: `cache.fm`
- `--file`: read newline-delimited queries from a file. Default: stdin

Each input line is one query. The trailing line ending is removed; all other
bytes on the line are searched exactly. Output contains one `TRUE` or `FALSE`
line for each input query, in the same order.

Batch mode opens the index once and reuses it for all queries. Use it when
checking hundreds or thousands of signatures.

### `daemon`

```sh
fmindex daemon [--config /opt/fmindex/etc/fmindex.conf] [-i cache.fm] [--bind 127.0.0.1] [--port 8080]
```

Options:

- `--config`: daemon config file path
- `-i`: input index path. Default: `cache.fm`
- `--bind`: HTTP bind address. Default: `127.0.0.1`
- `--port`: HTTP listen port. Default: `8080`

The daemon opens the index before it starts listening. If the index file does
not exist, startup fails with an explicit error. This is intentional: serving
without an index would make health checks pass while all real queries fail.

Config files use simple `key=value` lines:

```conf
index_path=/opt/fmindex/indexes/current/cache.fm
bind=127.0.0.1
port=8080
# Optional: enables POST /rebuild and GET /rebuild.
# rebuild_command=/opt/fmindex/bin/build_fmindex.sh
```

Command-line flags override values from the config file.

The daemon keeps the currently opened index in memory. On Unix systems it
handles `SIGHUP` by opening `index_path` again and switching to the new index
only after the open succeeds. If reload fails, the old index remains active.

Endpoints:

- `GET /health`: returns `OK`
- `GET /query?q=substring`: returns `TRUE` or `FALSE`
- `POST /query`: reads exact query bytes from the request body
- `POST /query-batch`: reads newline-delimited query bytes from the request body

Batch HTTP output is one `TRUE` or `FALSE` line per input line, in the same
order.

Bind to `127.0.0.1` for local-only use. Bind to `0.0.0.0` only when the service
is protected by firewalling or another trusted network boundary.

`rebuild_command` is intentionally opt-in. When it is configured, the daemon
also exposes:

- `POST /rebuild`: starts `rebuild_command` and returns `202 Accepted`
- `GET /rebuild`: returns the daemon's last rebuild command status

Protect this endpoint with local-only binding, firewalling, or a trusted reverse
proxy, because `/rebuild` intentionally starts a local command.

If the rebuild script restarts `fmindex`, run it through systemd so the rebuild
does not live inside the daemon's own process tree:

```conf
rebuild_command=systemd-run --wait --collect --unit=fmindex-rebuild /opt/fmindex/bin/build_fmindex.sh
```

## Service Layout

A practical Linux layout is:

```text
/opt/fmindex/bin/fmindex
/opt/fmindex/indexes/current -> /opt/fmindex/indexes/20260626-112004
/opt/fmindex/indexes/20260626-112004/cache.fm
/opt/fmindex/etc/fmindex.conf
/etc/systemd/system/fmindex.service
```

Example files are included in `deploy/fmindex.conf` and
`deploy/fmindex.service`.

The service can be installed manually:

```sh
sudo useradd --system --home /opt/fmindex --shell /usr/sbin/nologin fmindex
sudo install -d -o root -g root -m 0755 /opt/fmindex/bin
sudo install -d -o fmindex -g fmindex -m 0755 /opt/fmindex/indexes
sudo install -m 0755 fmindex_linux_amd64 /opt/fmindex/bin/fmindex
sudo install -d -m 0755 /opt/fmindex/etc
sudo install -m 0644 deploy/fmindex.conf /opt/fmindex/etc/fmindex.conf
sudo install -m 0644 deploy/fmindex.service /etc/systemd/system/fmindex.service
sudo systemctl daemon-reload
sudo systemctl enable fmindex
```

Create the initial index before starting the service:

```sh
sudo -u fmindex install -d /opt/fmindex/indexes/20260626-112004
sudo -u fmindex /opt/fmindex/bin/fmindex build -r \
  -o /opt/fmindex/indexes/20260626-112004/cache.fm \
  /data/clamav-raw
sudo -u fmindex ln -sfn /opt/fmindex/indexes/20260626-112004 /opt/fmindex/indexes/current
sudo systemctl start fmindex
```

For a remote server, build locally and install the Linux binary and systemd
files over SSH:

```sh
./build.sh
deploy/install_remote.sh root@your-server.example
```

The remote installer copies:

- `dist/fmindex_linux_amd64` to `/opt/fmindex/bin/fmindex`
- `deploy/fmindex.conf` to `/opt/fmindex/etc/fmindex.conf` if no config exists yet
- `deploy/fmindex.service` to `/etc/systemd/system/fmindex.service`

It enables the service but does not start it. Build the initial index on the
server first:

```sh
ssh root@your-server.example

version="$(date -u +%Y%m%d-%H%M%S)"
install -d -o fmindex -g fmindex "/opt/fmindex/indexes/$version"
runuser -u fmindex -- /opt/fmindex/bin/fmindex build -r \
  -o "/opt/fmindex/indexes/$version/cache.fm" \
  /data/clamav-raw
runuser -u fmindex -- ln -sfn "/opt/fmindex/indexes/$version" /opt/fmindex/indexes/current
systemctl start fmindex
systemctl status fmindex
```

### `version`

```sh
fmindex version
```

Prints the embedded version, git commit, and build time.

## Go API

```go
package main

import (
	"fmt"

	"github.com/jwillberg/fm-index/fmindex"
)

func main() {
	idx, err := fmindex.OpenSearch("cache.fm")
	if err != nil {
		panic(err)
	}

	if idx.Contains("base64_encode(str_replace(") {
		fmt.Println("TRUE")
	} else {
		fmt.Println("FALSE")
	}
}
```

For non-text queries or byte sequences containing zero bytes:

```go
ok := idx.ContainsBytes([]byte{0x00, 0xff, 0x41})
```

## Updating an Index

The safest production update model is immutable index versions plus an atomic
`current` symlink switch:

```sh
version="$(date -u +%Y%m%d-%H%M%S)"
new_index="/opt/fmindex/indexes/$version"

sudo -u fmindex install -d "$new_index"
sudo -u fmindex /opt/fmindex/bin/fmindex build -r \
  -o "$new_index/cache.fm" \
  /data/clamav-raw

sudo -u fmindex /opt/fmindex/bin/fmindex query \
  -i "$new_index/cache.fm" "base64_decode("

sudo -u fmindex ln -sfn "$new_index" /opt/fmindex/indexes/current.next
sudo -u fmindex mv -Tf /opt/fmindex/indexes/current.next /opt/fmindex/indexes/current
sudo systemctl reload fmindex
```

This avoids writing over the index the daemon is using. If the build fails, the
`current` symlink is not changed and the daemon keeps serving the old index. If
reload fails, the daemon also keeps the old in-memory index and logs the error.

Restarting the daemon also works after switching `current`, but `reload` avoids
dropping the listening socket for a normal index refresh.

The project still rebuilds the full index when inputs change.

Appending a single file into an existing FM-index is not implemented. Dynamic
FM-index updates are possible in theory, but they are substantially more complex
than rebuilding.

A practical future design is sharding:

- Keep the existing index as one shard
- Build a new small index for newly added files
- Query all shards
- Periodically compact shards into a fresh full index

## Performance Notes

Runtime lookup uses the FM-index and depends mainly on query length, not on the
number of indexed files.

The current implementation builds the suffix array in memory for one shard at a
time before producing the FM-index. The primary user-facing control is
`-memory-limit`, not shard size. The builder derives a conservative internal
shard size from that memory budget, which keeps memory far lower than building
one monolithic index for the entire dataset.

The on-disk index uses sparse occurrence checkpoints. Version 2 stores
checkpoints every 4096 BWT symbols with `uint32` counters. This is a deliberate
space/speed tradeoff: queries scan a little farther inside each checkpoint
block, but shard files are much smaller than with dense `uint64` checkpoints.

Negative sharded queries use prefilters before loading shard indexes. For
queries at least 8 bytes long, `cache.fm.ngr` checks 8-byte n-grams first. For
shorter queries, `cache.fm.tri` can still narrow candidates by trigram. False
positives are possible, so candidate shards are verified before returning
`TRUE`.

Positive sharded queries first verify candidate shards from `cache.fm.raw/`.
This adds roughly the indexed input size to the cache on disk, but avoids
opening large `.fm` shard files for normal one-shot CLI lookups.

Shard builds can run in parallel with `-jobs`. More jobs can reduce build time,
but each active shard builder needs memory. On small servers, use `-jobs 1` or
`-jobs 2` if the kernel starts killing the build for memory pressure.

Increasing `-memory-limit` also increases the internal shard size, so the
builder creates fewer shards. This often speeds up full builds, but each active
job will need more memory.

Large individual files are split automatically into overlapping chunks. This
requires a correctness limit for query length, controlled by
`-max-query-bytes`. The default is `1M`, which is far larger than typical
signatures. Query returns a clear error if the requested query is longer than
the index guarantees.

The runtime index is separate from the original input files. For sharded
indexes, query opens shard indexes one at a time instead of loading every shard
into memory at once.

For large query sets, prefer `query-batch` over launching `query` once per
signature. Reusing the same process avoids repeated startup, manifest parsing,
and sidecar opening overhead.

For applications that need repeated online lookups, use `daemon` so the index
stays open behind a small HTTP API.

Build-time benchmark notes are documented in [docs/benchmarks.md](docs/benchmarks.md).
