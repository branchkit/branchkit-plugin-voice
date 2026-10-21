#!/usr/bin/env bash
# Build the BranchKit "grammar-provider" patched sherpa-onnx static libs.
#
# Patch-overlay model (Direction 2, Phase 1): we do NOT fork sherpa-onnx. We
# carry a small diff against a PINNED upstream tag, build the static libs from
# source once, cache them, and link them into the sherpa-commands stage via
# SHERPA_ONNX_LIB_DIR (honored by the sherpa-onnx-sys crate's build.rs — no -sys
# fork). The diff adds an in-process word-list -> HL grammar builder + a runtime
# graph swap (SetGraphFromWords) so the CTC engine can add words live.
#
# Idempotent: re-running is a no-op unless the pinned version or the patch
# changes (tracked by a stamp file). Safe to call from `just setup`.
set -euo pipefail

SHERPA_VERSION="v1.13.2"
# The voice stages workspace (this script lives in its sherpa-commands/).
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Two overlays, applied in sequence:
# grammar-core = the generic lazy-grammar capability (upstream PR material);
# branchkit-g  = BranchKit's GrammarDag wire + BuildFromDagLazy, applies on top.
PATCH_CORE="$REPO_ROOT/sherpa-commands/patches/sherpa-onnx-${SHERPA_VERSION}-grammar-core.patch"
PATCH_G="$REPO_ROOT/sherpa-commands/patches/sherpa-onnx-${SHERPA_VERSION}-branchkit-g.patch"
BUILD_ROOT="$REPO_ROOT/target/sherpa-patched-build"        # scratch build tree

# The cache is OS+arch-specific but the repo tree is shared across machines
# (the Linux VM rsyncs this checkout from the Mac), so the install dir is
# namespaced by target triple — a flat dir WILL cross-contaminate.
host_arch="$(uname -m)"
case "$host_arch" in arm64) host_arch=aarch64 ;; esac
case "$(uname -s)" in
    Darwin) TRIPLE="${host_arch}-apple-darwin" ;;
    Linux)  TRIPLE="${host_arch}-unknown-linux-gnu" ;;
    MINGW*|MSYS*|CYGWIN*)
        # Windows is MSVC-native (VS generator, .lib archives, /MD CRT). The
        # build is PowerShell, not bash — delegate to the sibling .ps1 so both
        # `just build-windows` (this bash path) and a direct PowerShell invoke
        # share one implementation. See build-patched-sherpa.ps1.
        echo "build-sherpa-patched: Windows host — delegating to build-patched-sherpa.ps1"
        exec powershell.exe -NoProfile -ExecutionPolicy Bypass \
            -File "$(cygpath -w "$REPO_ROOT/sherpa-commands/build-patched-sherpa.ps1")" ;;
    *) echo "build-sherpa-patched: unsupported host $(uname -s)" >&2
       exit 1 ;;
esac
CACHE_ROOT="$REPO_ROOT/sherpa-commands/.sherpa-libs" # gitignored install cache
LIB_DIR="$CACHE_ROOT/$TRIPLE"
STAMP="$LIB_DIR/.stamp"

for p in "$PATCH_CORE" "$PATCH_G"; do
    if [ ! -f "$p" ]; then
        echo "build-sherpa-patched: patch not found: $p" >&2
        exit 1
    fi
done

sha256_stdin() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'
    else shasum -a 256 | awk '{print $1}'; fi
}

jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
# Stamp covers BOTH overlays, each prefixed by its basename so the stamp
# encodes identity AND apply order, not just the concatenated bytes.
patch_sha="$({ for p in "$PATCH_CORE" "$PATCH_G"; do basename "$p"; cat "$p"; done; } | sha256_stdin)"
# NOTE: sherpa-onnx-sys 1.13.2 hardcodes the static-lib link list including the
# TTS-adjacent archives (piper_phonemize, espeak-ng, ucd, ssentencepiece_core),
# so TTS must stay ON — building without it drops archives the -sys crate
# unconditionally links (fails on every OS). Revisit if the -sys crate gains a
# feature flag.
CMAKE_ARGS=(
    -DBUILD_SHARED_LIBS=OFF
    -DCMAKE_BUILD_TYPE=Release
    -DSHERPA_ONNX_ENABLE_BINARY=OFF
    -DSHERPA_ONNX_ENABLE_C_API=ON
    -DSHERPA_ONNX_ENABLE_PORTAUDIO=OFF
    -DSHERPA_ONNX_ENABLE_PYTHON=OFF
    -DSHERPA_ONNX_ENABLE_TTS=ON
    -DSHERPA_ONNX_ENABLE_WEBSOCKET=OFF
)
case "$(uname -s)" in
    Darwin) CMAKE_ARGS+=(-DCMAKE_OSX_ARCHITECTURES="$(uname -m)") ;;
esac
cfg_sha="$(printf '%s\n' "${CMAKE_ARGS[@]}" | sha256_stdin)"
want="${SHERPA_VERSION} ${patch_sha} ${cfg_sha}"

if [ -f "$STAMP" ] && [ "$(cat "$STAMP")" = "$want" ] \
        && [ -f "$LIB_DIR/libsherpa-onnx-c-api.a" ]; then
    echo "patched sherpa libs up to date ($SHERPA_VERSION, patch ${patch_sha:0:12}, $TRIPLE)"
    exit 0
fi

echo "Building patched sherpa-onnx $SHERPA_VERSION from source (one-time, ~minutes)..."
rm -rf "$BUILD_ROOT"
mkdir -p "$BUILD_ROOT"
git clone --depth 1 --branch "$SHERPA_VERSION" \
    https://github.com/k2-fsa/sherpa-onnx "$BUILD_ROOT/src"
cd "$BUILD_ROOT/src"

# Apply the overlays in sequence; fail loudly if either does not apply cleanly.
for p in "$PATCH_CORE" "$PATCH_G"; do
    git apply --check "$p" || { echo "patch does not apply cleanly: $p" >&2; exit 1; }
    git apply "$p"
done

cmake -B build "${CMAKE_ARGS[@]}" .
# c-api transitively pulls in every static lib the -sys crate links.
cmake --build build --target sherpa-onnx-core sherpa-onnx-c-api -j"$jobs"

mkdir -p "$LIB_DIR"
cp build/lib/*.a "$LIB_DIR/"
# onnxruntime is fetched prebuilt by cmake; the -sys crate expects it beside
# the rest in SHERPA_ONNX_LIB_DIR.
find build/_deps -name 'libonnxruntime.a' -exec cp {} "$LIB_DIR/" \;

echo "$want" > "$STAMP"
echo "Installed patched sherpa libs -> $LIB_DIR"
echo "  ($(ls "$LIB_DIR"/*.a | wc -l | tr -d ' ') archives)"

# The sherpa-onnx-sys rlib bundles these static-archive objects at ITS build
# time, and its build.rs has no rerun-if-changed on the lib FILES (only on the
# SHERPA_ONNX_LIB_DIR env value). So when the patch changes and we install new
# archives into the SAME dir, a previously-built sys rlib stays cached with the
# OLD objects → the next `cargo build` links a stale c-api (undefined-symbol on
# any newly-patched function, OR silently-old behavior for a CHANGED function).
# Evict the sys crate so it re-ingests the fresh archives. No-op on a clean tree.
#
# The `sherpa-libs` just recipe runs this script WITHOUT sourcing the cargo env,
# so cargo may not be on PATH here — source it first, else the eviction is
# silently skipped and the stale link survives. (This footgun cost real debugging
# time: the stage ran old patched-C++ grammar-builder behavior even though
# .sherpa-libs was current, looking like an FST bug that did not exist.)
#
# `cargo clean -p` is PROFILE- and ENV-specific: it only removes artifacts that
# match the build's fingerprint. The stage is built `--release` with
# SHERPA_ONNX_LIB_DIR set, so a bare `cargo clean -p sherpa-onnx-sys` targets the
# (empty) debug unit under a different fingerprint and removes NOTHING — an
# under-specified clean is no clean at all, and the stale release rlib survives.
# The eviction MUST mirror the build: `--release` + the same SHERPA_ONNX_LIB_DIR.
# (This second footgun also cost real debugging time: a freshly-built libs dir,
# stamp current, yet the test linked the previous patch's c-api objects.)
if ! command -v cargo >/dev/null 2>&1 && [ -f "$HOME/.cargo/env" ]; then
    . "$HOME/.cargo/env"
fi
if command -v cargo >/dev/null 2>&1; then
    # Release (the profile every build recipe uses) + debug, both with the env.
    (cd "$REPO_ROOT" && SHERPA_ONNX_LIB_DIR="$LIB_DIR" cargo clean -p sherpa-onnx-sys --release 2>/dev/null) || true
    (cd "$REPO_ROOT" && SHERPA_ONNX_LIB_DIR="$LIB_DIR" cargo clean -p sherpa-onnx-sys 2>/dev/null) || true
else
    echo "WARN: cargo not on PATH; skipped sherpa-onnx-sys eviction. Run" \
         "'SHERPA_ONNX_LIB_DIR=\"$LIB_DIR\" cargo clean -p sherpa-onnx-sys --release'" \
         "before rebuilding the stage." >&2
fi
