/* features.h — minimal shim.
 *
 * The musl exp_data.h / log_data.h / pow_data.h headers include <features.h>
 * to pick up its definition of `hidden` and `weak`. We make both no-ops since
 * we link statically into the final wasm binary and don't need symbol
 * visibility control.
 */
#ifndef FEATURES_H
#define FEATURES_H

#define hidden
#define weak

#endif
