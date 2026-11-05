# The x86-64 instruction set whisper_stage's whisper.cpp (ggml) is built
# for: AVX, AVX2, FMA and F16C, and nothing newer (no AVX-512, no AMX).
#
# Without it the set depends on the machine that builds: ggml's GGML_NATIVE
# defaults ON in a native build, which is -march=native under gcc and clang
# and a probe of the build machine's CPU under MSVC, so a binary built on a
# machine with AVX-512 dies with an illegal instruction on one without it.
# A cross build defaults the other way, to SSE only. Each flag is stated
# here, so a native build and a cross build come out the same.
#
# whisper-rs-sys passes cmake only the environment variables named WHISPER_*
# or CMAKE_*, so GGML_* cannot be set from the environment. This file goes
# in through one of those instead, included at every project() call:
#
#   CMAKE_PROJECT_INCLUDE=/abs/path/to/x86-64-baseline.cmake cargo build ...
#
# Two caveats. whisper-rs-sys's build script does not watch that variable,
# so a whisper-rs-sys cargo has already built is not rebuilt when it is set
# or changed: `cargo clean -p whisper-rs-sys` first. And it is for an
# x86-64 target only; set it nowhere else.
#
# The effective ggml compile options are written to
# bk-ggml-arch-flags.txt in the cmake build folder, for a build script or a
# person to read: cargo hides cmake's own output.

set(GGML_NATIVE OFF CACHE BOOL "ggml: enable -march=native flag" FORCE)
foreach(flag GGML_AVX GGML_AVX2 GGML_FMA GGML_F16C)
    set(${flag} ON CACHE BOOL "x86-64 baseline (x86-64-baseline.cmake)" FORCE)
endforeach()
foreach(flag GGML_AVX512 GGML_AVX512_VBMI GGML_AVX512_VNNI GGML_AVX512_BF16
             GGML_AVX_VNNI GGML_AMX GGML_AMX_TILE GGML_AMX_INT8 GGML_AMX_BF16)
    set(${flag} OFF CACHE BOOL "x86-64 baseline (x86-64-baseline.cmake)" FORCE)
endforeach()

# Once, at the end of the top-level configure: the compile options ggml's
# sources get (ARCH_FLAGS lands in ggml/src's COMPILE_OPTIONS).
if(NOT BK_X86_BASELINE_REPORT_DEFERRED
   AND CMAKE_CURRENT_SOURCE_DIR STREQUAL CMAKE_SOURCE_DIR
   AND NOT CMAKE_VERSION VERSION_LESS 3.19)
    set(BK_X86_BASELINE_REPORT_DEFERRED ON)
    function(bk_x86_baseline_report)
        set(options "")
        if(EXISTS "${CMAKE_SOURCE_DIR}/ggml/src/CMakeLists.txt")
            get_directory_property(options DIRECTORY "${CMAKE_SOURCE_DIR}/ggml/src" COMPILE_OPTIONS)
        endif()
        file(WRITE "${CMAKE_BINARY_DIR}/bk-ggml-arch-flags.txt" "${options}\n")
    endfunction()
    cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL bk_x86_baseline_report)
endif()
