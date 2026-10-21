<#
.SYNOPSIS
  Windows (MSVC) build of the BranchKit "grammar-provider" patched sherpa-onnx
  static libs. PowerShell sibling of scripts/build-sherpa-patched.sh — same
  patch-overlay model (Direction 2, Phase 1): do NOT fork sherpa-onnx; carry a
  small diff against a PINNED upstream tag, build the static libs from source
  once, cache them per-triple, and link them into the sherpa-commands stage via
  SHERPA_ONNX_LIB_DIR (honored by the sherpa-onnx-sys crate's build.rs).

.DESCRIPTION
  Notes on the MSVC path,
  learned building it on the Windows 11 ARM64 VM:

  * Generator MUST be the Visual Studio generator with -A ARM64. sherpa's
    cmake/onnxruntime.cmake selects the onnxruntime archive on
    `CMAKE_VS_PLATFORM_NAME STREQUAL ARM64`, a variable ONLY the VS generator
    sets. Ninja would silently fall through to the wrong (x64) branch.
  * CRT: we pass -DSHERPA_ONNX_USE_STATIC_CRT=OFF so sherpa compiles /MD AND
    fetches the matching `-MD-` onnxruntime archive. That matches rustc's
    aarch64-pc-windows-msvc default (dynamic CRT); building /MT here would need
    every linking crate to carry `-C target-feature=+crt-static`. (The upstream
    -sys win-x64 *prebuilt* is `-static-MT-`, but we build from source and pick
    the config that needs no global rustflag.)
  * The sys crate links a FIXED list of archives (see SHERPA_ONNX_STATIC_LIBS in
    its build.rs); on MSVC each resolves to `<name>.lib`. The VS generator emits
    them under build\lib\Release\ ; onnxruntime.lib lands under build\_deps\.

  Idempotent: re-running is a no-op unless the pinned version, the patch, or the
  cmake config changes (tracked by a stamp file).

.NOTES
  Deps in the guest: VS Build Tools 2022 (VCTools + ARM64 + Win11 SDK), CMake,
  Git, and rustup's aarch64-pc-windows-msvc toolchain (for the -sys eviction).
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$SHERPA_VERSION = 'v1.13.2'
$REPO_ROOT = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
# Two overlays, applied in sequence:
# grammar-core = the generic lazy-grammar capability; branchkit-g applies on top.
$PATCH_CORE = Join-Path $REPO_ROOT "sherpa-commands\patches\sherpa-onnx-$SHERPA_VERSION-grammar-core.patch"
$PATCH_G = Join-Path $REPO_ROOT "sherpa-commands\patches\sherpa-onnx-$SHERPA_VERSION-branchkit-g.patch"
$BUILD_ROOT = Join-Path $REPO_ROOT 'target\sherpa-patched-build'   # scratch build tree

# Triple mirrors the Justfile's `sherpa_triple` so `just build-windows` and this
# script agree on the cache dir. arch() in `just` is native; we derive the same
# from PROCESSOR_ARCHITECTURE (a Git-Bash `uname -m` can lie under emulation).
# The MACHINE's architecture, not this process's. Under x64 emulation on an
# ARM64 host — which is what a Git-Bash process tree is, even when bash.exe
# itself is an ARM64 binary — $env:PROCESSOR_ARCHITECTURE reads AMD64 and
# `uname -m` reads x86_64. Read from that here, the script chose `-A x64`, the
# ARM64 cl.exe still compiled the probe, and cmake failed with LNK1112
# "module machine type 'ARM64' conflicts with target machine type 'x64'"
# (found 2026-09-18 on the Win11 ARM64 VM, from `just build-windows` in
# Git-Bash — the shell the Windows `just` recipes require, 4A.2). The registry's
# system environment holds the native value regardless of the caller's
# emulation; PROCESSOR_ARCHITEW6432 is the documented fallback for a 32-bit
# process on a 64-bit OS and is empty under ARM64's x64 emulation.
$machineArch = (Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment' -ErrorAction SilentlyContinue).PROCESSOR_ARCHITECTURE
if (-not $machineArch) { $machineArch = $env:PROCESSOR_ARCHITEW6432 }
if (-not $machineArch) { $machineArch = $env:PROCESSOR_ARCHITECTURE }
switch ($machineArch) {
    'ARM64' { $hostArch = 'aarch64'; $vsPlatform = 'ARM64' }
    'AMD64' { $hostArch = 'x86_64'; $vsPlatform = 'x64' }
    default { throw "build-sherpa-patched: unsupported machine architecture '$machineArch' (process reports '$($env:PROCESSOR_ARCHITECTURE)')" }
}
if ($machineArch -ne $env:PROCESSOR_ARCHITECTURE) {
    Write-Host "build-sherpa-patched: machine is $machineArch, this process reports $($env:PROCESSOR_ARCHITECTURE) (emulated) - building for $machineArch"
}
$TRIPLE = "$hostArch-pc-windows-msvc"
$CACHE_ROOT = Join-Path $REPO_ROOT 'sherpa-commands\.sherpa-libs'   # gitignored install cache
$LIB_DIR = Join-Path $CACHE_ROOT $TRIPLE
$STAMP = Join-Path $LIB_DIR '.stamp'

foreach ($p in @($PATCH_CORE, $PATCH_G)) {
    if (-not (Test-Path -LiteralPath $p)) {
        throw "build-sherpa-patched: patch not found: $p"
    }
}

function Get-StringSha256([string]$Text) {
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($Text)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { -join ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString('x2') }) }
    finally { $sha.Dispose() }
}

$jobs = [Environment]::ProcessorCount
# Stamp covers BOTH overlays, each prefixed by its basename (identity + apply
# order, matching the .sh's construction — the stamps themselves stay
# per-triple and are never compared across OSes).
$patch_sha = Get-StringSha256 ((@($PATCH_CORE, $PATCH_G) | ForEach-Object {
    (Split-Path -Leaf $_) + "`n" + (Get-Content -Raw -LiteralPath $_)
}) -join '')

# NOTE: sherpa-onnx-sys 1.13.2 hardcodes the static-lib link list including the
# TTS-adjacent archives (piper_phonemize, espeak-ng, ucd, ssentencepiece_core),
# so TTS must stay ON — building without it drops archives the -sys crate
# unconditionally links.
$CMAKE_ARGS = @(
    '-DBUILD_SHARED_LIBS=OFF'
    '-DCMAKE_BUILD_TYPE=Release'
    '-DSHERPA_ONNX_ENABLE_BINARY=OFF'
    '-DSHERPA_ONNX_ENABLE_C_API=ON'
    '-DSHERPA_ONNX_ENABLE_PORTAUDIO=OFF'
    '-DSHERPA_ONNX_ENABLE_PYTHON=OFF'
    '-DSHERPA_ONNX_ENABLE_TTS=ON'
    '-DSHERPA_ONNX_ENABLE_WEBSOCKET=OFF'
    # Windows-only: /MD + `-MD-` onnxruntime archive (see .DESCRIPTION).
    '-DSHERPA_ONNX_USE_STATIC_CRT=OFF'
)
# The generator/-A also uniquely identify the produced archives, so fold them
# into the config hash — flipping ARM64<->x64 must invalidate the cache. -A
# follows the host arch (sherpa's onnxruntime.cmake keys the ort archive off
# CMAKE_VS_PLATFORM_NAME, so a hardcoded ARM64 on an x64 host builds
# wrong-arch libs into an x86_64-named cache dir).
#
# The generator names the installed Visual Studio, read from vswhere rather
# than hard-coded: a fixed 'Visual Studio 17 2022' failed CMake configure
# ("could not find any instance of Visual Studio") once the GitHub Windows
# image moved to a newer release (2026-10-05).
function Get-VsGenerator {
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
    if (Test-Path -LiteralPath $vswhere) {
        $ver = & $vswhere -latest -products * -property installationVersion
        if ($ver) {
            # CMake names each generator by version AND marketing year. The
            # year comes from this table, not vswhere: VS 2026 reports its
            # product line as "18", which made 'Visual Studio 18 18'.
            $major = ($ver -split '\.')[0]
            $years = @{ '16' = '2019'; '17' = '2022'; '18' = '2026' }
            if ($years.ContainsKey($major)) {
                return "Visual Studio $major $($years[$major])"
            }
            throw "build-patched-sherpa: Visual Studio $ver is newer than this script knows; add its CMake generator year"
        }
    }
    return 'Visual Studio 17 2022'
}
$vsGenerator = Get-VsGenerator
Write-Host "build-patched-sherpa: generator '$vsGenerator' -A $vsPlatform"
$GEN_ARGS = @('-G', $vsGenerator, '-A', $vsPlatform)
$cfg_sha = Get-StringSha256 (($CMAKE_ARGS + $GEN_ARGS) -join "`n")
$want = "$SHERPA_VERSION $patch_sha $cfg_sha"

if ((Test-Path -LiteralPath $STAMP) -and ((Get-Content -Raw -LiteralPath $STAMP).Trim() -eq $want) `
        -and (Test-Path -LiteralPath (Join-Path $LIB_DIR 'sherpa-onnx-c-api.lib'))) {
    Write-Host "patched sherpa libs up to date ($SHERPA_VERSION, patch $($patch_sha.Substring(0,12)), $TRIPLE)"
    exit 0
}

Write-Host "Building patched sherpa-onnx $SHERPA_VERSION from source (one-time, ~minutes)..."
if (Test-Path -LiteralPath $BUILD_ROOT) { Remove-Item -Recurse -Force -LiteralPath $BUILD_ROOT }
New-Item -ItemType Directory -Force -Path $BUILD_ROOT | Out-Null
$srcDir = Join-Path $BUILD_ROOT 'src'

git clone --depth 1 --branch $SHERPA_VERSION https://github.com/k2-fsa/sherpa-onnx $srcDir
if ($LASTEXITCODE -ne 0) { throw "git clone failed ($LASTEXITCODE)" }
Push-Location $srcDir
try {
    # Apply the overlays in sequence; fail loudly if either does not apply cleanly.
    foreach ($p in @($PATCH_CORE, $PATCH_G)) {
        git apply --check --whitespace=nowarn $p
        if ($LASTEXITCODE -ne 0) { throw "patch does not apply cleanly: $p" }
        git apply --whitespace=nowarn $p
        if ($LASTEXITCODE -ne 0) { throw "git apply failed ($LASTEXITCODE): $p" }
    }

    cmake -B build @GEN_ARGS @CMAKE_ARGS .
    if ($LASTEXITCODE -ne 0) { throw "cmake configure failed ($LASTEXITCODE)" }

    # c-api transitively pulls in every static lib the -sys crate links.
    cmake --build build --config Release --target sherpa-onnx-core sherpa-onnx-c-api -j $jobs
    if ($LASTEXITCODE -ne 0) { throw "cmake build failed ($LASTEXITCODE)" }
}
finally { Pop-Location }

New-Item -ItemType Directory -Force -Path $LIB_DIR | Out-Null
# VS generator emits per-config: build\lib\Release\*.lib
$libSrc = Join-Path $srcDir 'build\lib\Release'
$libs = @(Get-ChildItem -LiteralPath $libSrc -Filter '*.lib' -ErrorAction SilentlyContinue)
if ($libs.Count -eq 0) {
    # Some sherpa versions drop the per-config subdir; fall back to build\lib.
    $libSrc = Join-Path $srcDir 'build\lib'
    $libs = @(Get-ChildItem -LiteralPath $libSrc -Filter '*.lib' -ErrorAction SilentlyContinue)
}
if ($libs.Count -eq 0) { throw "no .lib archives produced under $srcDir\build\lib" }
foreach ($l in $libs) { Copy-Item -Force -LiteralPath $l.FullName -Destination $LIB_DIR }

# onnxruntime is fetched prebuilt by cmake (FetchContent) into build\_deps; the
# -sys crate expects onnxruntime.lib beside the rest in SHERPA_ONNX_LIB_DIR.
$ort = @(Get-ChildItem -Path (Join-Path $srcDir 'build\_deps') -Recurse -Filter 'onnxruntime.lib' -ErrorAction SilentlyContinue)
if ($ort.Count -eq 0) { throw "onnxruntime.lib not found under build\_deps (FetchContent may have failed)" }
Copy-Item -Force -LiteralPath $ort[0].FullName -Destination $LIB_DIR

Set-Content -LiteralPath $STAMP -Value $want -NoNewline
$count = @(Get-ChildItem -LiteralPath $LIB_DIR -Filter '*.lib').Count
Write-Host "Installed patched sherpa libs -> $LIB_DIR ($count archives)"

# Evict the sys crate so it re-ingests the fresh archives (its build.rs reruns
# only on the SHERPA_ONNX_LIB_DIR *value*, not the lib files). Mirror the build:
# --release + the same env, so the fingerprint matches what the stage builds.
# See the long comment in build-sherpa-patched.sh for why an under-specified
# clean is no clean at all.
$cargo = Get-Command cargo -ErrorAction SilentlyContinue
if ($cargo) {
    $env:SHERPA_ONNX_LIB_DIR = $LIB_DIR
    Push-Location $REPO_ROOT
    # `cargo clean` reports "Removed N files" on STDERR. Under this script's
    # $ErrorActionPreference = 'Stop', a native command's redirected stderr
    # (2>$null included) becomes an error record and TERMINATES the script --
    # after the libs were installed, so the build failed on its own success
    # message (2026-09-18, Win11 ARM64). Best-effort eviction: lenient
    # handling around just these two calls, both streams discarded.
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        cargo clean -p sherpa-onnx-sys --release 2>&1 | Out-Null
        cargo clean -p sherpa-onnx-sys 2>&1 | Out-Null
    } finally {
        $ErrorActionPreference = $prevEap
        Pop-Location
    }
} else {
    Write-Warning "cargo not on PATH; skipped sherpa-onnx-sys eviction. Run 'cargo clean -p sherpa-onnx-sys --release' with SHERPA_ONNX_LIB_DIR=$LIB_DIR before rebuilding the stage."
}

# The eviction is best-effort (mirrors the `|| true` in the .sh). Don't let a
# nonzero $LASTEXITCODE from `cargo clean` leak out — `just build-windows` runs
# the delegating .sh under `set -e`, so a nonzero exit here aborts the build.
exit 0
