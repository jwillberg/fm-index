#!/usr/bin/env bash
set -euo pipefail

usage() {
	echo "usage: $(basename "$0") [--dry-run]" >&2
	return 2
}

dry_run=0
if [[ ${1:-} == "--dry-run" ]]; then
	dry_run=1
	shift
fi

if [[ $# -ne 0 ]]; then
	usage
fi

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

shopt -s nullglob

public_paths=(
	README.md
	LICENSE
	build.sh
	go.mod
	scripts/publish-github.sh
	deploy/fmindex.conf
	deploy/fmindex.service
	deploy/install_remote.sh
	docs/benchmarks.md
	cmd/fmindex/*.go
	fmindex/*.go
	internal/sais/*.go
)

echo "Public paths to stage:"
staged_paths=()
for path in "${public_paths[@]}"; do
	if [[ -e $path ]]; then
		echo "  $path"
		staged_paths+=("$path")
	else
		echo "  $path (missing)" >&2
	fi
done

if [[ $dry_run -eq 1 ]]; then
	exit 0
fi

if [[ ${#staged_paths[@]} -eq 0 ]]; then
	echo "No public files found to stage." >&2
	exit 1
fi

git add -- "${staged_paths[@]}"

echo
echo "Staged public files only."
echo "Review with: git status --short"
echo "Then commit and push manually."