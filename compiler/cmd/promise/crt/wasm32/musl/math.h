/* math.h — Promise WASM shim providing the subset of <math.h> the vendored
 * musl source files use. Built against -nostdlibinc, so we can't pull in
 * a real libc math.h.
 *
 * Required:
 *   - INFINITY, NAN macros (used by exp.c, log.c, pow.c, expf.c, logf.c, powf.c)
 *   - M_PI_2 (used by sin.c, cos.c, tan.c, sinf.c, cosf.c, tanf.c for the
 *     small-argument constants s1pio2..s4pio2 / c1pio2..c4pio2 / t1pio2..t4pio2)
 *   - fabs(), floor(), scalbn() declarations (used by pow.c, __rem_pio2_large.c)
 */
#ifndef MATH_H
#define MATH_H

/* INFINITY and NAN are defined by clang's <float.h> as builtins, so we
 * include them only as a fallback (e.g. on toolchains that don't pre-define
 * them). The vendored musl files include this header transitively via libm.h
 * which already pulls in <float.h>, so the clang versions are normally seen
 * first and these guards keep us out of the redefinition warning path. */
#ifndef INFINITY
#define INFINITY __builtin_inff()
#endif
#ifndef NAN
#define NAN      __builtin_nanf("")
#endif

#define M_PI_2 1.57079632679489661923

/* float_t / double_t — would normally come from <bits/alltypes.h>.
 * No excess precision on WASM, so they alias the base types. */
typedef float  float_t;
typedef double double_t;

double fabs(double);
float  fabsf(float);
double floor(double);
double scalbn(double, int);

#endif
