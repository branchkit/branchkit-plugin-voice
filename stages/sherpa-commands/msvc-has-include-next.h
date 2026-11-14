/*
 * Force-included into every C and C++ file of the patched sherpa-onnx build
 * under MSVC (build-patched-sherpa.ps1 passes /FI through CFLAGS/CXXFLAGS).
 *
 * espeak-ng's compat headers (src/include/compat/unistd.h, endian.h) test
 * `#if __has_include_next(<header>)`, a GCC/Clang extension. MSVC 19.51 let
 * the test fail quietly; MSVC 19.52 (Visual Studio 2026 on GitHub's
 * windows-11-arm image, November 2026) rejects it: C7780 "function-like
 * macro '__has_include_next' is not defined". Windows has neither header,
 * so answering "not found" is what the earlier compiler did.
 */
#if defined(_MSC_VER) && !defined(__clang__) && !defined(__has_include_next)
#define __has_include_next(header) 0
#endif
