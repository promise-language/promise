package module

import (
	"os"
	"path/filepath"
	"testing"

	"djabi.dev/go/promise_lang/internal/types"
)

func TestHashModuleSources(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.pr"), []byte("func a() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "b.pr"), []byte("func b() {}"), 0644)

	h1, err := HashModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	// Same content should produce same hash
	h2, err := HashModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("same source should produce same hash")
	}

	// Changing a file should change the hash
	os.WriteFile(filepath.Join(dir, "b.pr"), []byte("func b_changed() {}"), 0644)
	h3, err := HashModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h3 {
		t.Error("changed source should produce different hash")
	}
}

func TestHashModuleSourcesIgnoresNonPr(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.pr"), []byte("func a() {}"), 0644)

	h1, err := HashModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Adding a non-.pr file should not change the hash
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hello"), 0644)
	h2, err := HashModuleSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("non-.pr files should not affect hash")
	}
}

func TestHashModuleSourcesDeterministic(t *testing.T) {
	// Create two directories with the same files in different creation order
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// dir1: write b first, then a
	os.WriteFile(filepath.Join(dir1, "b.pr"), []byte("func b() {}"), 0644)
	os.WriteFile(filepath.Join(dir1, "a.pr"), []byte("func a() {}"), 0644)

	// dir2: write a first, then b
	os.WriteFile(filepath.Join(dir2, "a.pr"), []byte("func a() {}"), 0644)
	os.WriteFile(filepath.Join(dir2, "b.pr"), []byte("func b() {}"), 0644)

	h1, _ := HashModuleSources(dir1)
	h2, _ := HashModuleSources(dir2)
	if h1 != h2 {
		t.Error("same files in different creation order should produce same hash")
	}
}

func TestHashModuleInterface(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "test")

	// Empty scope
	h1 := HashModuleInterface(scope)
	if h1 == "" {
		t.Fatal("expected non-empty hash for empty scope")
	}

	// Nil scope
	h2 := HashModuleInterface(nil)
	if h2 == "" {
		t.Fatal("expected non-empty hash for nil scope")
	}
	if h1 != h2 {
		t.Error("empty scope and nil scope should produce same hash")
	}

	// Add a public function — hash should change
	fn := types.NewFunc(types.Pos{}, "greet", types.NewSignature(nil, nil, nil, false))
	fn.SetExported(true)
	scope.Insert(fn)
	h3 := HashModuleInterface(scope)
	if h3 == h1 {
		t.Error("adding a function should change the hash")
	}
}

func TestHashModuleInterfaceDeterministic(t *testing.T) {
	// Two scopes with the same symbols should produce the same hash
	s1 := types.NewScope(nil, types.Pos{}, types.Pos{}, "test1")
	s2 := types.NewScope(nil, types.Pos{}, types.Pos{}, "test2")

	fn1 := types.NewFunc(types.Pos{}, "alpha", types.NewSignature(nil, nil, nil, false))
	fn1.SetExported(true)
	fn2 := types.NewFunc(types.Pos{}, "beta", types.NewSignature(nil, nil, nil, false))
	fn2.SetExported(true)

	// Insert in different order
	s1.Insert(fn1)
	s1.Insert(fn2)
	s2.Insert(fn2)
	s2.Insert(fn1)

	h1 := HashModuleInterface(s1)
	h2 := HashModuleInterface(s2)
	if h1 != h2 {
		t.Error("same symbols in different insertion order should produce same hash")
	}
}

func TestBuildCacheKey(t *testing.T) {
	key1 := BuildCacheKey("impl1", "compiler1", "x86_64", []string{"./a", "./b"})
	if key1 == "" {
		t.Fatal("expected non-empty cache key")
	}

	// Same inputs should produce same key
	key2 := BuildCacheKey("impl1", "compiler1", "x86_64", []string{"./a", "./b"})
	if key1 != key2 {
		t.Error("same inputs should produce same cache key")
	}

	// Module path order should not matter (sorted internally)
	key3 := BuildCacheKey("impl1", "compiler1", "x86_64", []string{"./b", "./a"})
	if key1 != key3 {
		t.Error("module path order should not affect cache key")
	}

	// Different impl hash should change key
	key4 := BuildCacheKey("impl2", "compiler1", "x86_64", []string{"./a", "./b"})
	if key1 == key4 {
		t.Error("different impl hash should produce different cache key")
	}

	// Different compiler should change key
	key5 := BuildCacheKey("impl1", "compiler2", "x86_64", []string{"./a", "./b"})
	if key1 == key5 {
		t.Error("different compiler hash should produce different cache key")
	}

	// Different target should change key
	key6 := BuildCacheKey("impl1", "compiler1", "wasm32", []string{"./a", "./b"})
	if key1 == key6 {
		t.Error("different target should produce different cache key")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	modName := "mymod"
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	ifaceHash := "iface_hash_123"

	// Initially no cache
	if got := LookupCachedObj(cacheDir, modName, cacheKey); got != "" {
		t.Errorf("expected no cached obj, got %q", got)
	}

	// Create a fake .o file and save it
	objFile := filepath.Join(t.TempDir(), "test.o")
	os.WriteFile(objFile, []byte("fake object data"), 0644)

	err := SaveCachedObj(cacheDir, modName, cacheKey, ifaceHash, objFile)
	if err != nil {
		t.Fatal(err)
	}

	// Now lookup should succeed
	cached := LookupCachedObj(cacheDir, modName, cacheKey)
	if cached == "" {
		t.Fatal("expected cached obj after save")
	}

	// Read back the data
	data, err := os.ReadFile(cached)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake object data" {
		t.Errorf("cached data = %q, want %q", string(data), "fake object data")
	}

	// Interface hash should be readable
	gotIface := ReadCachedInterfaceHash(cacheDir, modName, cacheKey)
	if gotIface != ifaceHash {
		t.Errorf("interface hash = %q, want %q", gotIface, ifaceHash)
	}
}

func TestCleanStaleCache(t *testing.T) {
	cacheDir := t.TempDir()
	modName := "mymod"
	oldKey := "oldkey1234567890oldkey1234567890oldkey1234567890oldkey1234567890"
	newKey := "newkey1234567890newkey1234567890newkey1234567890newkey1234567890"

	// Create old cached files
	os.WriteFile(filepath.Join(cacheDir, "mymod-oldkey1234567890.o"), []byte("old"), 0644)
	os.WriteFile(filepath.Join(cacheDir, "mymod-oldkey1234567890.interface"), []byte("old"), 0644)
	// Create new cached files
	os.WriteFile(filepath.Join(cacheDir, "mymod-newkey1234567890.o"), []byte("new"), 0644)
	os.WriteFile(filepath.Join(cacheDir, "mymod-newkey1234567890.interface"), []byte("new"), 0644)
	// Create unrelated file
	os.WriteFile(filepath.Join(cacheDir, "othermod-abc1234567890ab.o"), []byte("other"), 0644)

	CleanStaleCache(cacheDir, modName, newKey)

	// Old files should be removed
	if _, err := os.Stat(filepath.Join(cacheDir, "mymod-"+oldKey[:16]+".o")); err == nil {
		t.Error("expected old .o to be removed")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "mymod-"+oldKey[:16]+".interface")); err == nil {
		t.Error("expected old .interface to be removed")
	}

	// New files should remain
	if _, err := os.Stat(filepath.Join(cacheDir, "mymod-"+newKey[:16]+".o")); err != nil {
		t.Error("expected new .o to remain")
	}

	// Unrelated files should remain
	if _, err := os.Stat(filepath.Join(cacheDir, "othermod-abc1234567890ab.o")); err != nil {
		t.Error("expected unrelated .o to remain")
	}
}
