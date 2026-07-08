#!/usr/bin/env bash
set -euo pipefail

APP_NAME="fmindex"
PKG="./cmd/fmindex"
DIST_DIR="${DIST_DIR:-dist}"
VERSION="${VERSION:-v1.0.0}"
GOMODCACHE="${GOMODCACHE:-$(pwd)/.gomodcache}"
export GOMODCACHE

if command -v git >/dev/null 2>&1; then
	COMMIT="$(git rev-parse --short HEAD 2>/dev/null || printf 'unknown')"
else
	COMMIT="unknown"
fi

BUILT_AT="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.builtAt=${BUILT_AT}"

TARGETS=(
	"darwin/amd64"
	"darwin/arm64"
	"linux/amd64"
	"linux/arm64"
	"linux/arm/7"
	"windows/amd64"
	"windows/arm64"
)

mkdir -p "${DIST_DIR}"
mkdir -p "${GOMODCACHE}"

echo "running tests"
go test ./...

for target in "${TARGETS[@]}"; do
	IFS="/" read -r goos goarch goarm <<<"${target}"
	label="${goos}"
	if [[ "${goos}" == "darwin" ]]; then
		label="macos"
	fi

	output="${DIST_DIR}/${APP_NAME}_${label}_${goarch}"
	if [[ -n "${goarm:-}" ]]; then
		output="${output}v${goarm}"
	fi
	if [[ "${goos}" == "windows" ]]; then
		output="${output}.exe"
	fi

	echo "building ${output}"
	if [[ -n "${goarm:-}" ]]; then
		CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" GOARM="${goarm}" go build -trimpath -ldflags "${LDFLAGS}" -o "${output}" "${PKG}"
	else
		CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -ldflags "${LDFLAGS}" -o "${output}" "${PKG}"
	fi
done

echo "done: ${DIST_DIR}"
