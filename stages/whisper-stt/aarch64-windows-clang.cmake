# The Arm instruction set whisper_stage's whisper.cpp (ggml) is built for on
# Windows ARM64, compiled by clang-cl: ARMv8.2-A with FP16 arithmetic and
# the dot product, what every Windows 11 Arm PC has (Snapdragon 8cx and
# later). Not the i8mm matrix multiply: the earliest of those lack it.
#
# Why clang-cl and these flags: under MSVC, ggml never uses the NEON FP16
# vector arithmetic its F16 matrix multiplies need (MSVC has no flag that
# defines __ARM_FEATURE_FP16_VECTOR_ARITHMETIC), and whisper's weights are
# F16, so every multiply took the scalar path. clang defines the feature
# macros itself from -march.
#
# ggml's MSVC branch (ggml/src/CMakeLists.txt) also probes for the dot
# product, int8 matrix multiply and FP16 arithmetic and adds a define for
# each probe that compiles. Its int8 probe compiles vmlaq_f32, which every
# Arm compiler has, so it would define __ARM_FEATURE_MATMUL_INT8 and ggml
# would use i8mm instructions the target lacks. The probe results are cache
# variables, so they are set here first and the probes never run; the
# features come from -march alone.
#
# Set through CMAKE_PROJECT_INCLUDE, for an aarch64-pc-windows-msvc build
# with clang-cl only (scripts/recipes/build-whisper-stage.py). The same
# caveat as x86-64-baseline.cmake: whisper-rs-sys's build script does not
# watch the variable, so `cargo clean -p whisper-rs-sys` after setting it.
#
# The compile options ggml's sources get are written to
# bk-ggml-arch-flags.txt in the cmake build folder, as the x86 file does.

if(NOT (MSVC AND CMAKE_C_COMPILER_ID STREQUAL "Clang"))
    message(FATAL_ERROR "aarch64-windows-clang.cmake is for clang-cl; the C compiler is ${CMAKE_C_COMPILER_ID}")
endif()

foreach(probe GGML_COMPILER_SUPPORT_DOTPROD GGML_COMPILER_SUPPORT_MATMUL_INT8
              GGML_COMPILER_SUPPORT_FP16_VECTOR_ARITHMETIC)
    set(${probe} "" CACHE INTERNAL "set by aarch64-windows-clang.cmake: features come from -march")
endforeach()

add_compile_options("/clang:-march=armv8.2-a+fp16+dotprod")
# ggml's own thread pool, not OpenMP. whisper-stt asks for OpenMP on Windows
# because ggml's pool spins on locked compare-exchanges under MSVC; under
# clang it has real atomics and a pause, and OpenMP here would be LLVM's
# libomp, which whisper-rs-sys does not link (it relies on MSVC's /openmp
# pulling in vcomp) and the stage would have to ship as libomp.dll.
set(GGML_OPENMP OFF CACHE BOOL "aarch64-windows-clang.cmake: ggml's own pool" FORCE)
# ggml.c's FP16 vector paths load through `__fp16 *`, but ggml-cpu-impl.h
# gives ggml_fp16_internal_t a uint16_t on any compiler defining _MSC_VER,
# clang-cl included. Both hold the same IEEE half bits, so the assignments
# are only a type-name mismatch; clang makes it an error by default.
add_compile_options("-Wno-error=incompatible-pointer-types")

if(NOT BK_ARM_REPORT_DEFERRED
   AND CMAKE_CURRENT_SOURCE_DIR STREQUAL CMAKE_SOURCE_DIR
   AND NOT CMAKE_VERSION VERSION_LESS 3.19)
    set(BK_ARM_REPORT_DEFERRED ON)
    function(bk_arm_report)
        set(options "")
        if(EXISTS "${CMAKE_SOURCE_DIR}/ggml/src/CMakeLists.txt")
            get_directory_property(options DIRECTORY "${CMAKE_SOURCE_DIR}/ggml/src" COMPILE_OPTIONS)
        endif()
        file(WRITE "${CMAKE_BINARY_DIR}/bk-ggml-arch-flags.txt" "${options}\n")
    endfunction()
    cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL bk_arm_report)
endif()
