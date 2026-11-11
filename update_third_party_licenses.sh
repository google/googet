#!/usr/bin/env bash
# Regenerates THIRD_PARTY_LICENSES, the licence notices shipped in the googet
# package (see googet.goospec), from the dependencies linked into googet.exe.
#
# Usage: ./update_third_party_licenses.sh [output_dir]
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

if [[ $# -gt 1 || "${1:-}" == -* ]]; then
  echo "Usage: $0 [output_dir]" >&2
  exit 2
fi
repo_root="$(cd "$(dirname "$0")" && pwd)"
out="${1:-"$repo_root/THIRD_PARTY_LICENSES"}"
if [[ "$out" != /* ]]; then
  out="$PWD/$out"
fi
cd "$repo_root"
tmp="$(mktemp -d)"
# go-licenses output and files copied from the Go module cache are read-only;
# make them writable so the temporary directory can be removed.
trap 'chmod -R u+w "$tmp"; rm -rf "$tmp"' EXIT

# Keep the caller's workspace or flags from affecting any go command below.
export GOWORK=off GOFLAGS=

# Build go-licenses for this host, whatever GOOS/GOARCH the caller's
# environment or go env file sets. The target only applies when it runs, where
# it selects the dependency graph to analyse.
GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" GOBIN="$tmp/bin" \
  go install "github.com/google/go-licenses/v2@$go_licenses_version"


# On success go-licenses only warns about non-Go (assembly or C) files it
# cannot inspect, and it fails on missing or unclassifiable licences, so its
# log is shown only on failure, or always when VERBOSE is set. googet's own
# licence is left out because the package already ships it as LICENSE.txt.
if ! GOOS="$target_goos" GOARCH="$target_goarch" "$tmp/bin/go-licenses" save . \
  --ignore github.com/google/googet/v2 \
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
    mod_dst="$tmp/licenses/$mod"
    # Check before creating directories whether go-licenses put a regular
    # file at $mod itself (rather than only under a subpackage path).
    has_go_license=false
    for existing in "$mod_dst"/*; do
      if [[ -f "$existing" ]]; then
        has_go_license=true
        break
      fi
    done
    for f in "$dir"/LICENSE-* "$dir"/NOTICE*; do
      [[ -f "$f" ]] || continue
      [[ "$(basename "$f")" == LICENSE-LOGO ]] && continue
      dst="$mod_dst/$(basename "$f")"
      # Keep go-licenses' copy if it already saved this file.
      [[ -e "$dst" ]] && continue
      # go-licenses names folders by the import path that holds the licence,
      # which is not always the module path. Flag a notice that would land in
      # a folder where go-licenses placed no file so someone checks placement.
      if [[ "$has_go_license" == false ]]; then
        echo "warning: $mod/$(basename "$f") has no go-licenses file in $mod; check its placement" >&2
      fi
      mkdir -p "$mod_dst"
      cp "$f" "$dst"
    done
  done

# Use the same 0644 mode for every file, whatever the module cache used.
chmod -R u+w "$tmp/licenses"
find "$tmp/licenses" -type f -exec chmod 644 {} +
rm -rf "$out"
mkdir -p "$(dirname "$out")"
mv "$tmp/licenses" "$out"
