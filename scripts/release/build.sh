#!/usr/bin/env bash
# Cross-compile Sweef for every release target with a pure-Go toolchain.
#
#   scripts/release/build.sh [--targets "linux/amd64 ..."] [--check-repro] [--skip-launchers]
#
# Env: VERSION, COMMIT, SOURCE_DATE_EPOCH, DIST (default ./dist),
#      SWEEF_MAIN_PKG (default ./cmd/sweef), SWEEF_MAX_BINARY_BYTES (default 41943040),
#      SWEEF_MIN_HEADROOM_BYTES (default 2097152: warn when less budget is left), SWEEF_SIZE_TABLE=1 (always print
#      the per-package size table for linux targets; it is printed anyway when a binary is over budget),
#      STRICT_VERSION=1 (fail if the built binary does not report $VERSION)
#
# Output layout:
#   dist/bin/<os>_<arch>/sweef[.exe]        the browser/CLI
#   dist/bin/<os>_<arch>/SweefLauncher[.exe]      GUI launcher (darwin, windows): runs `sweef gui`
#   dist/bin/SHA256SUMS                     checksums of everything above
set -euo pipefail
. "$(dirname "$0")/lib.sh"

TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
CHECK_REPRO=0
LAUNCHERS=1
while [ $# -gt 0 ]; do
  case "$1" in
    --targets) TARGETS="$2"; shift 2 ;;
    --check-repro) CHECK_REPRO=1; shift ;;
    --skip-launchers) LAUNCHERS=0; shift ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

need go
resolve_all
MAIN_PKG="${SWEEF_MAIN_PKG:-./cmd/sweef}"
# 40 MiB: the binary carries WebRTC (pion, about +4.5 MiB), WebAssembly (wazero, about +3.7 MiB), the media codecs and
# the web platform layer; it was 32 MiB before those. Raise it deliberately, with a line in CHANGELOG.md.
# cmd/sweef/binsize_linux_test.go reads this default, so there is one place to change. How to see what is big and how
# to add data without breaking the budget: docs/development.md, "Binary size budget".
MAX_BYTES="${SWEEF_MAX_BINARY_BYTES:-41943040}"
MIN_HEADROOM="${SWEEF_MIN_HEADROOM_BYTES:-2097152}"
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:-} -mod=readonly"
export SOURCE_DATE_EPOCH

log "version=$VERSION commit=$COMMIT epoch=$SOURCE_DATE_EPOCH go=$(go env GOVERSION)"

# -funcalign=16: the linker aligns functions to 32 bytes on amd64 (arm64 already uses 16). 16 is enough for the CPU's
# fetch and the linker still honours any larger alignment a function asks for (PCALIGN in assembly, aligned loops),
# so only the padding goes: about 260 KB. See docs/development.md, "Binary size budget", for the benchmarks.
LDFLAGS="-s -w -buildid= -funcalign=16 -X main.version=$VERSION -X main.commit=$COMMIT"

# Packages compiled without inlining, for size: scripts/release/noinline-packages.txt (read by cmd/sweef/binsize_linux_test.go
# too). Only the browser binary gets them. bash 3.2 (macOS runners) has no mapfile, hence the loop.
GCFLAGS=()
while IFS= read -r pat; do
  case "$pat" in ''|'#'*) ;; *) GCFLAGS+=("-gcflags=$pat=-l") ;; esac
done < "$ROOT/scripts/release/noinline-packages.txt"

# Windows resources (manifest: DPI v2, long paths, UTF-8; version info; icon) are linked in
# via .syso files that live next to the main package only for the duration of the build.
SYSO_FILES=()
cleanup() { for f in ${SYSO_FILES[@]+"${SYSO_FILES[@]}"}; do rm -f "$f"; done; }
trap cleanup EXIT

prepare_windows_resources() {
  "$ROOT/scripts/release/icons.sh" >&2
  local PT; PT="$(pkgtool)"
  local mf="$DIST/windows-resources/sweef.manifest"
  mkdir -p "$DIST/windows-resources"
  sed "s/@VERSION4@/$VERSION4/g" "$ROOT/packaging/windows/sweef.manifest" > "$mf"
  local pre=""; [ -n "$VERSION_PRE" ] && pre="-prerelease"
  for arch in amd64 arm64; do
    "$PT" syso -arch "$arch" -o "$DIST/windows-resources/rsrc_windows_$arch.syso" \
      -manifest "$mf" -ico "$DIST/icons/sweef.ico" -version "$VERSION4" -display-version "$VERSION" \
      -name Sweef -desc "Sweef Browser" -company "Sweef" -original sweef.exe $pre
    local dst="$ROOT/${MAIN_PKG#./}/sweef_windows_$arch.syso"
    cp "$DIST/windows-resources/rsrc_windows_$arch.syso" "$dst"
    SYSO_FILES+=("$dst")
    # the GUI launcher (SweefLauncher.exe) gets the same icon/manifest and its own version strings
    "$PT" syso -arch "$arch" -o "$DIST/windows-resources/launcher_windows_$arch.syso" \
      -manifest "$mf" -ico "$DIST/icons/sweef.ico" -version "$VERSION4" -display-version "$VERSION" \
      -name Sweef -desc "Sweef Browser launcher" -company "Sweef" -original SweefLauncher.exe $pre
    dst="$ROOT/packaging/tools/launcher/sweef_windows_$arch.syso"
    cp "$DIST/windows-resources/launcher_windows_$arch.syso" "$dst"
    SYSO_FILES+=("$dst")
  done
}

build_one() { # os arch outdir
  local os="$1" arch="$2" out="$3" ext=""
  [ "$os" = windows ] && ext=".exe"
  mkdir -p "$out"
  ( cd "$ROOT" && GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false ${GCFLAGS[@]+"${GCFLAGS[@]}"} -ldflags "$LDFLAGS" -o "$out/sweef$ext" "$MAIN_PKG" ) \
    || die "build failed for $os/$arch"
  if [ "$LAUNCHERS" = 1 ] && { [ "$os" = darwin ] || [ "$os" = windows ]; }; then
    local lf="-s -w -buildid="
    [ "$os" = windows ] && lf="$lf -H=windowsgui"
    ( cd "$ROOT" && GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "$lf" -o "$out/SweefLauncher$ext" ./packaging/tools/launcher ) \
      || die "launcher build failed for $os/$arch"
  fi
  # Sweef is one static binary: no libc, no dynamic loader (it must run on Alpine/musl and in scratch containers).
  # A dependency that loads libraries with dlopen (purego's Linux shim) would add NEEDED entries and an INTERP.
  if [ "$os" = linux ] && command -v readelf >/dev/null 2>&1; then
    if readelf -lW "$out/sweef$ext" 2>/dev/null | grep -q 'INTERP' || readelf -dW "$out/sweef$ext" 2>/dev/null | grep -q 'NEEDED'; then
      die "$os/$arch binary is dynamically linked (INTERP/NEEDED present); Linux builds must be static"
    fi
  fi
  # sweef-cli: the session commands without the engine linked in, so each command starts in half the time
  # (cmd/sweef-cli); it runs the sweef binary next to it as the daemon
  ( cd "$ROOT" && GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" -o "$out/sweef-cli$ext" ./cmd/sweef-cli ) \
    || die "sweef-cli build failed for $os/$arch"
  local size; size="$(wc -c < "$out/sweef$ext" | tr -d ' ')"
  printf '    %-14s %10s bytes (%s MiB), headroom %s bytes (%s MiB) of the %s byte budget\n' "$os/$arch" "$size" \
    "$(awk -v n="$size" 'BEGIN{printf "%.2f", n/1048576}')" "$((MAX_BYTES - size))" \
    "$(awk -v n="$((MAX_BYTES - size))" 'BEGIN{printf "%.2f", n/1048576}')" "$MAX_BYTES" >&2
  if [ "$os" = linux ] && { [ "$size" -gt "$MAX_BYTES" ] || [ "${SWEEF_SIZE_TABLE:-0}" = 1 ]; }; then
    size_table "$os" "$arch" "$out/sweef$ext"
  fi
  if [ "$size" -gt "$MAX_BYTES" ]; then
    die "$os/$arch binary is $size bytes, over the budget of $MAX_BYTES (SWEEF_MAX_BINARY_BYTES). Investigate before shipping: docs/development.md, \"Binary size budget\"."
  fi
  if [ $((MAX_BYTES - size)) -lt "$MIN_HEADROOM" ]; then
    warn "$os/$arch has only $((MAX_BYTES - size)) bytes of size budget left (warning threshold $MIN_HEADROOM). New features should shrink something first."
  fi
}

# size_table prints where the bytes of a linux binary are: builds an unstripped copy (symbols are needed) and runs
# tools/sizeprof on it. Best effort: it never fails the build.
size_table() { # os arch strippedbinary
  local full; full="$(mktemp "${TMPDIR:-/tmp}/sweef-full.XXXXXX")"
  if ( cd "$ROOT" && GOOS="$1" GOARCH="$2" go build -trimpath -buildvcs=false ${GCFLAGS[@]+"${GCFLAGS[@]}"} -ldflags "-buildid= -funcalign=16" -o "$full" "$MAIN_PKG" ) \
     && ( cd "$ROOT" && go run ./tools/sizeprof -bin "$full" -stripped "$3" -top 40 ) >&2; then :; else
    warn "could not produce the size table for $1/$2"
  fi
  rm -f "$full"
}

build_all() { # outroot
  local t
  for t in $TARGETS; do
    build_one "${t%/*}" "${t#*/}" "$1/${t%/*}_${t#*/}"
  done
}

case " $TARGETS " in *" windows/"*) prepare_windows_resources ;; esac

if [ "$CHECK_REPRO" = 1 ]; then
  log "reproducibility check: building twice (second with a cold, separate GOCACHE)"
  A="$DIST/repro/a"; B="$DIST/repro/b"
  rm -rf "$DIST/repro"; build_all "$A"
  cold="$(mktemp -d)"
  GOCACHE="$cold" build_all "$B" || { rm -rf "$cold"; exit 1; }
  chmod -R u+w "$cold" 2>/dev/null; rm -rf "$cold"
  ( cd "$A" && find . -type f | LC_ALL=C sort ) | while read -r f; do
    if ! cmp -s "$A/$f" "$B/$f"; then echo "NOT REPRODUCIBLE: $f" >&2; echo bad > "$DIST/repro/.bad"; fi
  done
  [ ! -e "$DIST/repro/.bad" ] || die "builds differ"
  log "reproducible: all binaries byte-identical across two builds"
  rm -rf "$DIST/repro"
fi

rm -rf "${DIST:?}/bin"
build_all "$DIST/bin"

# Does the binary report the version we injected? (-X silently ignores a const.)
host_os="$(go env GOHOSTOS)"; host_arch="$(go env GOHOSTARCH)"
hb="$DIST/bin/${host_os}_${host_arch}/sweef"
if [ -x "$hb" ]; then
  reported="$("$hb" version 2>&1 | head -n1 || true)"
  case "$reported" in
    *"$VERSION"*) log "binary reports: $reported" ;;
    *)
      msg="built binary reports '$reported', expected '$VERSION'. cmd/sweef/main.go declares version as a const, so -X main.version cannot take effect: change it to 'var version = \"dev\"' (and add 'var commit = \"unknown\"')."
      if [ "${STRICT_VERSION:-0}" = 1 ]; then die "$msg"; else warn "$msg"; fi ;;
  esac
fi

( cd "$DIST/bin" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | while read -r f; do
    printf '%s  %s\n' "$(sha256_of "$f")" "$f"; done > SHA256SUMS )
log "wrote $DIST/bin/SHA256SUMS"
