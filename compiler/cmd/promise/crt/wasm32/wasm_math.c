// wasm_math.c — WASM math runtime providing libm functions that LLVM lowers
// the corresponding intrinsics to (sin/cos/tan, exp/log, pow, etc.).
//
// WASM MVP has only sqrt, abs, ceil, floor, trunc, nearest as native f64 ops;
// trigonometric and exponential functions must be provided by the runtime.
// LLVM lowers `llvm.sin.f64` and friends to extern `sin` libcalls; without a
// WASI libm, the link would fail with "unknown import: env::sin". This file
// supplies those symbols.
//
// Implementations are minimal but accurate enough for typical use:
//   - sin/cos: argument reduction to [-pi, pi] + 7-term Taylor series
//   - tan: sin/cos
//   - exp: integer-part repeated-multiply + 8-term Taylor for fractional
//   - log: ln via the atanh identity log(x) = 2·atanh((x-1)/(x+1)), 6 terms
//   - pow(x, y): exp(y·log(x)); special-cased for integer y
//
// Rebuild: clang --target=wasm32-unknown-wasi -O2 -nostdlib -c wasm_math.c -o wasm_math.o

static const double PI       = 3.141592653589793;
static const double TWO_PI   = 6.283185307179586;
static const double HALF_PI  = 1.5707963267948966;
static const double E_CONST  = 2.718281828459045;

double sin(double x) {
    while (x >  PI) x -= TWO_PI;
    while (x < -PI) x += TWO_PI;
    if (x >  HALF_PI) x =  PI - x;
    if (x < -HALF_PI) x = -PI - x;
    double x2 = x * x;
    double t  = x;
    double s  = t;
    t *= -x2 / (2.0 * 3.0);     s += t;
    t *= -x2 / (4.0 * 5.0);     s += t;
    t *= -x2 / (6.0 * 7.0);     s += t;
    t *= -x2 / (8.0 * 9.0);     s += t;
    t *= -x2 / (10.0 * 11.0);   s += t;
    t *= -x2 / (12.0 * 13.0);   s += t;
    return s;
}

double cos(double x) { return sin(x + HALF_PI); }
double tan(double x) { return sin(x) / cos(x); }

double exp(double x) {
    if (x == 0.0) return 1.0;
    int neg = 0;
    if (x < 0) { neg = 1; x = -x; }
    int n = (int)x;
    double r = x - (double)n;
    if (r > 0.5) { r -= 1.0; n += 1; }
    double en = 1.0;
    for (int i = 0; i < n; i++) en *= E_CONST;
    double t = 1.0, s = 1.0;
    t *= r / 1.0;  s += t;
    t *= r / 2.0;  s += t;
    t *= r / 3.0;  s += t;
    t *= r / 4.0;  s += t;
    t *= r / 5.0;  s += t;
    t *= r / 6.0;  s += t;
    t *= r / 7.0;  s += t;
    t *= r / 8.0;  s += t;
    double result = en * s;
    return neg ? 1.0 / result : result;
}

double log(double x) {
    if (x <= 0.0) return -1e308;
    if (x == 1.0) return 0.0;
    int k = 0;
    while (x >= 2.0) { x *= 0.5; k++; }
    while (x <  1.0) { x *= 2.0; k--; }
    double t  = (x - 1.0) / (x + 1.0);
    double t2 = t * t;
    double s  = t;
    double n  = t;
    n *= t2; s += n / 3.0;
    n *= t2; s += n / 5.0;
    n *= t2; s += n / 7.0;
    n *= t2; s += n / 9.0;
    n *= t2; s += n / 11.0;
    return 2.0 * s + (double)k * 0.6931471805599453;
}

double pow(double x, double y) {
    if (y == 0.0) return 1.0;
    if (x == 0.0) return 0.0;
    int yi = (int)y;
    if ((double)yi == y) {
        double r = 1.0;
        int n = yi < 0 ? -yi : yi;
        for (int i = 0; i < n; i++) r *= x;
        return yi < 0 ? 1.0 / r : r;
    }
    return exp(y * log(x));
}

double sqrt(double x)  { return __builtin_sqrt(x); }
double fabs(double x)  { return __builtin_fabs(x); }
double floor(double x) { return __builtin_floor(x); }
double ceil(double x)  { return __builtin_ceil(x); }
double round(double x) { return x >= 0.0 ? __builtin_floor(x + 0.5) : __builtin_ceil(x - 0.5); }

float sinf(float x)         { return (float)sin((double)x); }
float cosf(float x)         { return (float)cos((double)x); }
float tanf(float x)         { return (float)tan((double)x); }
float expf(float x)         { return (float)exp((double)x); }
float logf(float x)         { return (float)log((double)x); }
float powf(float x, float y){ return (float)pow((double)x, (double)y); }
float sqrtf(float x)        { return __builtin_sqrtf(x); }
float fabsf(float x)        { return __builtin_fabsf(x); }
float floorf(float x)       { return __builtin_floorf(x); }
float ceilf(float x)        { return __builtin_ceilf(x); }
float roundf(float x)       { return x >= 0.0f ? __builtin_floorf(x + 0.5f) : __builtin_ceilf(x - 0.5f); }
