#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST="${1:-${REMOTE_HOST:-root@your-server.example}}"
REMOTE_TMP_DIR="${REMOTE_TMP_DIR:-/tmp/fmindex-install}"
LOCAL_BINARY="${LOCAL_BINARY:-dist/fmindex_linux_amd64}"
REMOTE_BINARY="/opt/fmindex/bin/fmindex"

if [[ ! -x "${LOCAL_BINARY}" ]]; then
	echo "missing ${LOCAL_BINARY}; run ./build.sh first" >&2
	exit 1
fi

ssh "${REMOTE_HOST}" "mkdir -p ${REMOTE_TMP_DIR}"
scp "${LOCAL_BINARY}" "${REMOTE_HOST}:${REMOTE_TMP_DIR}/fmindex"
scp deploy/fmindex.conf "${REMOTE_HOST}:${REMOTE_TMP_DIR}/fmindex.conf"
scp deploy/fmindex.service "${REMOTE_HOST}:${REMOTE_TMP_DIR}/fmindex.service"

ssh "${REMOTE_HOST}" "REMOTE_TMP_DIR=${REMOTE_TMP_DIR} REMOTE_BINARY=${REMOTE_BINARY} bash -s" <<'REMOTE_SCRIPT'
set -euo pipefail

if ! id fmindex >/dev/null 2>&1; then
	useradd --system --home /opt/fmindex --shell /usr/sbin/nologin fmindex
fi

install -d -o root -g root -m 0755 /opt/fmindex
install -d -o root -g root -m 0755 /opt/fmindex/bin
install -d -o fmindex -g fmindex -m 0755 /opt/fmindex/indexes
install -m 0755 "${REMOTE_TMP_DIR}/fmindex" "${REMOTE_BINARY}"

install -d -o root -g root -m 0755 /opt/fmindex/etc
if [[ ! -f /opt/fmindex/etc/fmindex.conf ]]; then
	install -m 0644 "${REMOTE_TMP_DIR}/fmindex.conf" /opt/fmindex/etc/fmindex.conf
fi

install -m 0644 "${REMOTE_TMP_DIR}/fmindex.service" /etc/systemd/system/fmindex.service
systemctl daemon-reload
systemctl enable fmindex

echo "installed ${REMOTE_BINARY}"
echo "config: /opt/fmindex/etc/fmindex.conf"
echo "service enabled; create /opt/fmindex/indexes/current/cache.fm before starting"
REMOTE_SCRIPT
