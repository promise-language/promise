/* wasm_math.c — WASM math runtime providing libm functions that LLVM lowers
 * the corresponding intrinsics to (sin/cos/tan, exp/log, pow, etc.).
 *
 * WASM MVP has only sqrt, abs, ceil, floor, trunc, nearest as native f64 ops;
 * trigonometric and exponential functions must be provided by the runtime.
 * LLVM lowers `llvm.sin.f64` and friends to extern `sin` libcalls; without a
 * WASI libm, the link would fail with "unknown import: env::sin".
 *
 * This file plus the vendored musl sources under crt/wasm32/musl/ supply
 * those symbols. Accuracy is libm-quality: ~1 ULP for the primary range with
 * full IEEE 754 special-case handling for NaN, ±Inf, ±0, and denormals.
 *
 * Functions provided here (thin __builtin_* wrappers, since WASM has these
 * as native f64 ops or as compiler intrinsics):
 *   - sqrt / sqrtf, fabs / fabsf
 *   - floor / floorf, ceil / ceilf, round / roundf
 *
 * Functions provided by the musl source files (full libm-quality):
 *   - sin / cos / tan, sinf / cosf / tanf
 *   - exp / log / pow, expf / logf / powf
 *   - scalbn (helper used by __rem_pio2_large)
 *
 * Rebuild (run from compiler/cmd/promise/crt/wasm32):
 *
 *   ./build_wasm_math.sh
 *
 * The script compiles each .c file separately (musl uses file-static helpers
 * with overlapping names like `pio4` and `T[]` that collide if amalgamated
 * into a single TU) and links the resulting .o files with `wasm-ld -r` into
 * a single relocatable wasm_math.o.
 *
 * The musl sources are MIT-licensed; see musl/LICENSE for full attribution.
 */

double sqrt(double x)  { return __builtin_sqrt(x); }
double fabs(double x)  { return __builtin_fabs(x); }
double floor(double x) { return __builtin_floor(x); }
double ceil(double x)  { return __builtin_ceil(x); }
double round(double x) { return x >= 0.0 ? __builtin_floor(x + 0.5) : __builtin_ceil(x - 0.5); }

float sqrtf(float x)   { return __builtin_sqrtf(x); }
float fabsf(float x)   { return __builtin_fabsf(x); }
float floorf(float x)  { return __builtin_floorf(x); }
float ceilf(float x)   { return __builtin_ceilf(x); }
float roundf(float x)  { return x >= 0.0f ? __builtin_floorf(x + 0.5f) : __builtin_ceilf(x - 0.5f); }
