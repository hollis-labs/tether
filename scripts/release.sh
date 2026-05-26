#!/usr/bin/env bash

set -euo pipefail

if [[ -z "${VERSION:-}" ]]; then
  echo "VERSION is required (example: VERSION=0.1.0)" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DIST_DIR="${DIST_DIR:-${REPO_ROOT}/dist}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
COMMIT="${COMMIT:-$(cd "${REPO_ROOT}" && git rev-parse --short HEAD 2>/dev/null || echo unknown)}"

TARGETS=(
  "darwin/arm64"
  "darwin/amd64"
  "linux/arm64"
  "linux/amd64"
)

mkdir -p "${DIST_DIR}"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/tether-release.XXXXXX")"
cleanup() { rm -rf "${tmp_dir}"; }
trap cleanup EXIT

echo "Building Tether release artifacts"
echo "  version: ${VERSION}"
echo "  commit: ${COMMIT}"
echo "  build date: ${BUILD_DATE}"

ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}"

declare -a archives

for target in "${TARGETS[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  work_dir="${tmp_dir}/${os}_${arch}"
  mkdir -p "${work_dir}"

  archive_name="tether_${VERSION}_${os}_${arch}.tar.gz"
  archive_path="${DIST_DIR}/${archive_name}"
  checksum_path="${archive_path}.sha256"

  echo
  echo "==> ${os}/${arch}"

  (
    cd "${REPO_ROOT}"
    CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build \
      -trimpath \
      -ldflags "${ldflags}" \
      -o "${work_dir}/mux" ./cmd/mux
    CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build \
      -trimpath \
      -ldflags "${ldflags}" \
      -o "${work_dir}/mux-apikey-helper" ./cmd/mux-apikey-helper
  )

  cp "${REPO_ROOT}/README.md" "${REPO_ROOT}/LICENSE" "${REPO_ROOT}/docs/install.md" "${work_dir}/"

  tar -C "${work_dir}" -czf "${archive_path}" mux mux-apikey-helper README.md LICENSE install.md
  shasum -a 256 "${archive_path}" > "${checksum_path}"

  echo "  wrote: ${archive_path}"
  archives+=("${archive_name}")
done

combined_checksums="${DIST_DIR}/checksums.txt"
(
  cd "${DIST_DIR}"
  shasum -a 256 "${archives[@]}" > "${combined_checksums}"
)

echo
echo "Done."
echo "  combined checksums: ${combined_checksums}"
