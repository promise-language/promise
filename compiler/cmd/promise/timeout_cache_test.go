package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTimeoutConfigCacheString(t *testing.T) {
	cfg1 := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          1.0,
	}
	cfg2 := testTimeoutConfig{
		defaultTimeout: 10 * time.Second,
		scale:          1.0,
	}
	cfg3 := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          2.0,
	}
	cfg4 := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          1.0,
		min:            1 * time.Second,
	}
	cfg5 := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          1.0,
		max:            5 * time.Second,
	}

	// Same config should produce the same string.
	cfg1Copy := cfg1
	if cfg1.cacheString() != cfg1Copy.cacheString() {
		t.Fatal("same config produced different cache strings")
	}

	// Different configs should produce different strings.
	configs := []testTimeoutConfig{cfg1, cfg2, cfg3, cfg4, cfg5}
	seen := make(map[string]int)
	for i, cfg := range configs {
		s := cfg.cacheString()
		if prev, ok := seen[s]; ok {
			t.Fatalf("config %d and %d produced same cache string: %s", prev, i, s)
		}
		seen[s] = i
	}
}

func TestComputeTestFileCacheKeyIncludesTimeout(t *testing.T) {
	// Create a temp .pr file with a simple test.
	dir := t.TempDir()
	f := filepath.Join(dir, "test.pr")
	if err := os.WriteFile(f, []byte(`test_foo() `+"`test {\n  assert(1 == 1);\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	target := "x86_64-unknown-linux-gnu"

	cfg1 := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          1.0,
	}
	cfg2 := testTimeoutConfig{
		defaultTimeout: 10 * time.Second,
		scale:          1.0,
	}
	cfg3 := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          2.0,
	}

	key1, ok1 := computeTestFileCacheKey(f, target, cfg1)
	key2, ok2 := computeTestFileCacheKey(f, target, cfg2)
	key3, ok3 := computeTestFileCacheKey(f, target, cfg3)

	if !ok1 || !ok2 || !ok3 {
		t.Skipf("cache key computation not available (missing std hash)")
	}

	if key1 == key2 {
		t.Error("different defaultTimeout should produce different cache keys")
	}
	if key1 == key3 {
		t.Error("different scale should produce different cache keys")
	}

	// Same config should produce the same key.
	key1b, _ := computeTestFileCacheKey(f, target, cfg1)
	if key1 != key1b {
		t.Error("same config should produce the same cache key")
	}
}

func TestComputeParentTimeout(t *testing.T) {
	// Default compile timeout: 10 minutes for native, 15 minutes for WASM.
	defaultCfg := testTimeoutConfig{defaultTimeout: 60 * time.Second, compileTimeout: 10 * time.Minute}

	nativeTimeout := computeParentTimeout(defaultCfg, "x86_64-unknown-linux-gnu")
	if nativeTimeout != 10*time.Minute {
		t.Errorf("native backstop = %v, want 10m", nativeTimeout)
	}

	wasmTimeout := computeParentTimeout(defaultCfg, "wasm32-wasi")
	if wasmTimeout != 15*time.Minute {
		t.Errorf("wasm backstop = %v, want 15m", wasmTimeout)
	}

	// Custom compile timeout respected for native.
	customCfg := testTimeoutConfig{compileTimeout: 5 * time.Minute}
	customTimeout := computeParentTimeout(customCfg, "x86_64-unknown-linux-gnu")
	if customTimeout != 5*time.Minute {
		t.Errorf("custom backstop = %v, want 5m", customTimeout)
	}

	// WASM minimum (15m) overrides a smaller custom compile timeout.
	smallCfg := testTimeoutConfig{compileTimeout: 3 * time.Minute}
	wasmSmall := computeParentTimeout(smallCfg, "wasm32-wasi")
	if wasmSmall != 15*time.Minute {
		t.Errorf("wasm backstop with small custom = %v, want 15m", wasmSmall)
	}

	// WASM timeout is strictly greater than native for default config.
	if wasmTimeout <= nativeTimeout {
		t.Errorf("WASM parent timeout (%v) should exceed native (%v) for default config",
			wasmTimeout, nativeTimeout)
	}
}
