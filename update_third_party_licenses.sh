#!/usr/bin/env bash
# Regenerates THIRD_PARTY_LICENSES, the licence notices shipped in the googet
# package (see googet.goospec), from the dependencies linked into googet.exe.
#
# Usage: ./update_third_party_licenses.sh
#
# go-licenses keeps one licence file per library. Some modules also carry extra
# notices for code they bundle (for example modernc.org/sqlite's LICENSE-SQLITE),
# so every module-root LICENSE-* and NOTICE* file of a linked module is copied
# as well. Not copied: AUTHORS lists, golang.org/x PATENTS grants (not
# licences), and LICENSE-LOGO files, which cover logo artwork that is not part
# of the binary.
set -euo pipefail

# googet ships only as googet.exe (x86_64 in googet.goospec), and Windows build
# tags pull in extra modules (for example go-ole and wmi), so both dependency
# walks below target windows/amd64. GOARCH also matters: some modules keep
# per-architecture licence copies.
target_goos=windows
target_goarch=amd64
go_licenses_version=v2.0.1

cd "$(dirname "$0")"
out=THIRD_PARTY_LICENSES
tmp="$(mktemp -d)"
# go-licenses output and files copied from the Go module cache are read-only;
# make them writable so the temporary directory can be removed.
trap 'chmod -R u+w "$tmp"; rm -rf "$tmp"' EXIT

# Build go-licenses for this host, whatever GOOS/GOARCH the caller's
# environment or go env file sets. The target only applies when it runs, where
# it selects the dependency graph to analyse.
GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" GOBIN="$tmp/bin" \
  go install "github.com/google/go-licenses/v2@$go_licenses_version"

# On success go-licenses only warns about non-Go (assembly or C) files it
# cannot inspect, and it fails on missing or unclassifiable licences, so its
# log is shown only on failure, or always when VERBOSE is set.
if ! GOOS="$target_goos" GOARCH="$target_goarch" "$tmp/bin/go-licenses" save . \
  --save_path="$tmp/licenses" 2>"$tmp/go-licenses.log"; then
  cat "$tmp/go-licenses.log" >&2
  exit 1
fi
if [[ -n "${VERBOSE:-}" ]]; then
  cat "$tmp/go-licenses.log" >&2
fi

GOOS="$target_goos" GOARCH="$target_goarch" go list -deps \
  -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' . |
  sort -u |
  while read -r mod dir; do
    for f in "$dir"/LICENSE-* "$dir"/NOTICE*; do
      [[ -f "$f" ]] || continue
      [[ "$(basename "$f")" == LICENSE-LOGO ]] && continue
      dst="$tmp/licenses/$mod/$(basename "$f")"
      # Keep go-licenses' copy if it already saved this file.
      [[ -e "$dst" ]] && continue
      mkdir -p "$(dirname "$dst")"
      cp "$f" "$dst"
    done
  done

# Use the same 0644 mode for every file, whatever the module cache used.
chmod -R u+w "$tmp/licenses"
find "$tmp/licenses" -type f -exec chmod 644 {} +
rm -rf "$out"
mv "$tmp/licenses" "$out"
