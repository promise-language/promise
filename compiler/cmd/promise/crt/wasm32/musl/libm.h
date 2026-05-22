/* libm.h — Promise WASM shim for musl's internal libm header.
 *
 * Replaces musl's src/internal/libm.h with the subset needed by the
 * sin/cos/tan/exp/log/pow files vendored under crt/wasm32/musl/.
 *
 * Differences from upstream musl:
 *   - No long double support (WASM has no native long double).
 *   - No <math.h> / <endian.h> includes — we don't need INFINITY here
 *     (it's pulled in by source files via <math.h>, see math.h shim).
 *   - `hidden` and `weak` are no-ops since we link statically into the
 *     final wasm binary and don't ship a shared library.
 *   - TOINT_INTRINSICS / WANT_SNAN are off; WANT_ROUNDING on (matches
 *     upstream defaults for arches without round-to-int instructions).
 */
#ifndef _LIBM_H
#define _LIBM_H

#include <stdint.h>
#include <float.h>
#include <math.h>     /* INFINITY, M_PI_2, fabs, floor, scalbn */

#define hidden
#define weak

/* Branch prediction hints (matches musl's predict_*) */
#ifdef __GNUC__
#define predict_true(x)  __builtin_expect(!!(x), 1)
#define predict_false(x) __builtin_expect(x, 0)
#else
#define predict_true(x)  (x)
#define predict_false(x) (x)
#endif

/* No excess precision on WASM, so float_t/double_t == float/double. */
typedef float  float_t;
typedef double double_t;

/* Configuration flags (match musl defaults for non-i386 / non-SNaN builds). */
#define WANT_ROUNDING 1
#define WANT_SNAN 0
#define issignaling_inline(x)  0
#define issignalingf_inline(x) 0

#ifndef TOINT_INTRINSICS
#define TOINT_INTRINSICS 0
#endif

/* Eval-as-type: cast through a local with the desired type. */
static inline float eval_as_float(float x)
{
	float y = x;
	return y;
}

static inline double eval_as_double(double x)
{
	double y = x;
	return y;
}

/* fp_barrier: prevent the compiler from collapsing common subexpressions
 * across this point — matches musl's volatile-store dance. */
static inline float fp_barrierf(float x)
{
	volatile float y = x;
	return y;
}

static inline double fp_barrier(double x)
{
	volatile double y = x;
	return y;
}

/* fp_force_eval: force evaluation for FP-exception side effects.
 * On WASM these flags are unobservable, so this is a no-op store
 * that prevents the compiler from removing the input expression. */
static inline void fp_force_evalf(float x)
{
	volatile float y;
	y = x;
}

static inline void fp_force_eval(double x)
{
	volatile double y;
	y = x;
}

#define FORCE_EVAL(x) do {                        \
	if (sizeof(x) == sizeof(float))           \
		fp_force_evalf(x);                \
	else                                      \
		fp_force_eval(x);                 \
} while(0)

/* Bit-pattern reinterpretation helpers (musl style: union initializer). */
#define asuint(f)   (((union { float    _f; uint32_t _i; }){ (f) })._i)
#define asfloat(i)  (((union { uint32_t _i; float    _f; }){ (i) })._f)
#define asuint64(f) (((union { double   _f; uint64_t _i; }){ (f) })._i)
#define asdouble(i) (((union { uint64_t _i; double   _f; }){ (i) })._f)

#define EXTRACT_WORDS(hi, lo, d) do {             \
	uint64_t __u = asuint64(d);               \
	(hi) = __u >> 32;                         \
	(lo) = (uint32_t)__u;                     \
} while (0)

#define GET_HIGH_WORD(hi, d) do {                 \
	(hi) = asuint64(d) >> 32;                 \
} while (0)

#define GET_LOW_WORD(lo, d) do {                  \
	(lo) = (uint32_t)asuint64(d);             \
} while (0)

#define INSERT_WORDS(d, hi, lo) do {              \
	(d) = asdouble(((uint64_t)(hi) << 32)     \
	             | (uint32_t)(lo));            \
} while (0)

#define SET_HIGH_WORD(d, hi) INSERT_WORDS(d, hi, (uint32_t)asuint64(d))
#define SET_LOW_WORD(d, lo)  INSERT_WORDS(d, asuint64(d) >> 32, lo)

#define GET_FLOAT_WORD(w, d) do {                 \
	(w) = asuint(d);                          \
} while (0)

#define SET_FLOAT_WORD(d, w) do {                 \
	(d) = asfloat(w);                         \
} while (0)

/* Forward declarations for the kernel routines we vendor. */
int    __rem_pio2_large(double*, double*, int, int, int);
int    __rem_pio2(double, double*);
double __sin(double, double, int);
double __cos(double, double);
double __tan(double, double, int);

int   __rem_pio2f(float, double*);
float __sindf(double);
float __cosdf(double);
float __tandf(double, int);

/* Math error helpers — musl signals overflow/underflow/invalid via
 * floating-point exceptions; on WASM these are unobservable so the
 * helpers just return the right IEEE 754 value. */
float  __math_xflowf(uint32_t, float);
float  __math_uflowf(uint32_t);
float  __math_oflowf(uint32_t);
float  __math_divzerof(uint32_t);
float  __math_invalidf(float);
double __math_xflow(uint32_t, double);
double __math_uflow(uint32_t);
double __math_oflow(uint32_t);
double __math_divzero(uint32_t);
double __math_invalid(double);

#endif
