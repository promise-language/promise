package module

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

func TestHashModuleSources(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.pr"), []byte("func a() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "b.pr"), []byte("func b() {}"), 0644)

	h1, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	// Same content should produce same hash
	h2, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("same source should produce same hash")
	}

	// Changing a file should change the hash
	os.WriteFile(filepath.Join(dir, "b.pr"), []byte("func b_changed() {}"), 0644)
	h3, err := HashModuleSources(dir, false)
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

	h1, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	// Adding a non-.pr file should not change the hash
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hello"), 0644)
	h2, err := HashModuleSources(dir, false)
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

	h1, _ := HashModuleSources(dir1, false)
	h2, _ := HashModuleSources(dir2, false)
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

func TestHashModuleInterfaceNamedType(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "test")

	// Create a Named type with exported fields and methods
	tn := types.NewTypeName(types.Pos{}, "Dog", nil)
	named := types.NewNamed(tn, nil)
	named.SetExported(true)

	field := types.NewField(types.Pos{}, "name", types.TypString, types.PlaceInstance, false, false)
	field.SetExported(true)
	named.AddField(field)

	sig := types.NewSignature(nil, nil, types.TypString, false)
	method := types.NewMethod(types.Pos{}, "speak", sig, types.PlaceInstance, false, false)
	method.SetExported(true)
	named.AddMethod(method)

	scope.Insert(tn)
	h1 := HashModuleInterface(scope)

	// Adding a second exported field should change the hash
	field2 := types.NewField(types.Pos{}, "age", types.TypInt, types.PlaceInstance, false, false)
	field2.SetExported(true)
	named.AddField(field2)
	h2 := HashModuleInterface(scope)
	if h1 == h2 {
		t.Error("adding an exported field should change the hash")
	}

	// Adding a non-exported method should NOT change the hash
	privMethod := types.NewMethod(types.Pos{}, "internal", sig, types.PlaceInstance, false, false)
	// Not calling SetExported — stays private
	named.AddMethod(privMethod)
	h3 := HashModuleInterface(scope)
	if h2 != h3 {
		t.Error("adding a non-exported method should not change the hash")
	}
}

func TestHashModuleInterfaceNamedWithTypeParams(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "test")

	// Create a generic Named type: Box[T]
	tpName := types.NewTypeName(types.Pos{}, "T", nil)
	tp := types.NewTypeParam(tpName, nil, 0)

	tn := types.NewTypeName(types.Pos{}, "Box", nil)
	named := types.NewNamed(tn, []*types.TypeParam{tp})
	named.SetExported(true)
	scope.Insert(tn)

	h1 := HashModuleInterface(scope)

	// Different type param count should change hash: Box[T, U]
	scope2 := types.NewScope(nil, types.Pos{}, types.Pos{}, "test2")
	tp2Name := types.NewTypeName(types.Pos{}, "T", nil)
	tp2 := types.NewTypeParam(tp2Name, nil, 0)
	up2Name := types.NewTypeName(types.Pos{}, "U", nil)
	up2 := types.NewTypeParam(up2Name, nil, 1)

	tn2 := types.NewTypeName(types.Pos{}, "Box", nil)
	named2 := types.NewNamed(tn2, []*types.TypeParam{tp2, up2})
	named2.SetExported(true)
	scope2.Insert(tn2)

	h2 := HashModuleInterface(scope2)
	if h1 == h2 {
		t.Error("different type param count should produce different hash")
	}
}

func TestHashModuleInterfaceEnumType(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "test")

	// Create an Enum with variants
	tn := types.NewTypeName(types.Pos{}, "Color", nil)
	enum := types.NewEnum(tn, nil)
	enum.SetExported(true)

	enum.AddVariant(types.NewVariant("Red", nil))
	enum.AddVariant(types.NewVariant("Green", nil))
	scope.Insert(tn)

	h1 := HashModuleInterface(scope)
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	// Adding a variant should change the hash
	enum.AddVariant(types.NewVariant("Blue", nil))
	h2 := HashModuleInterface(scope)
	if h1 == h2 {
		t.Error("adding an enum variant should change the hash")
	}
}

func TestHashModuleInterfaceEnumWithFields(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "test")

	tn := types.NewTypeName(types.Pos{}, "Shape", nil)
	enum := types.NewEnum(tn, nil)
	enum.SetExported(true)

	// Variant with named field
	enum.AddVariant(types.NewVariant("Circle", []*types.VarField{
		types.NewVarField("radius", types.TypF64),
	}))
	// Variant with positional field (empty name)
	enum.AddVariant(types.NewVariant("Square", []*types.VarField{
		types.NewVarField("", types.TypF64),
	}))

	scope.Insert(tn)
	h1 := HashModuleInterface(scope)
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	// Adding a method should change the hash
	sig := types.NewSignature(nil, nil, types.TypF64, false)
	method := types.NewMethod(types.Pos{}, "area", sig, types.PlaceInstance, false, false)
	method.SetExported(true)
	enum.AddMethod(method)

	h2 := HashModuleInterface(scope)
	if h1 == h2 {
		t.Error("adding enum method should change the hash")
	}
}

func TestHashModuleInterfaceEnumWithTypeParams(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "test")

	tpName := types.NewTypeName(types.Pos{}, "T", nil)
	tp := types.NewTypeParam(tpName, nil, 0)

	tn := types.NewTypeName(types.Pos{}, "Option", nil)
	enum := types.NewEnum(tn, []*types.TypeParam{tp})
	enum.SetExported(true)
	enum.AddVariant(types.NewVariant("Some", nil))
	enum.AddVariant(types.NewVariant("None", nil))

	scope.Insert(tn)
	h := HashModuleInterface(scope)
	if h == "" {
		t.Fatal("expected non-empty hash for generic enum")
	}
}

func TestBuildCacheKey(t *testing.T) {
	key1 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", nil)
	if key1 == "" {
		t.Fatal("expected non-empty cache key")
	}

	// Same inputs should produce same key
	key2 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", nil)
	if key1 != key2 {
		t.Error("same inputs should produce same cache key")
	}

	// Different impl hash should change key
	key3 := BuildCacheKey("impl2", "compiler1", "x86_64", "debug", nil)
	if key1 == key3 {
		t.Error("different impl hash should produce different cache key")
	}

	// Different compiler should change key
	key4 := BuildCacheKey("impl1", "compiler2", "x86_64", "debug", nil)
	if key1 == key4 {
		t.Error("different compiler hash should produce different cache key")
	}

	// Different target should change key
	key5 := BuildCacheKey("impl1", "compiler1", "wasm32", "debug", nil)
	if key1 == key5 {
		t.Error("different target should produce different cache key")
	}

	// T0181: Dep hashes affect cache key
	key6 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", []string{"./dep1:hash1"})
	if key1 == key6 {
		t.Error("adding dep hashes should produce different cache key")
	}

	// Different dep hash should change key
	key7 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", []string{"./dep1:hash2"})
	if key6 == key7 {
		t.Error("different dep hash should produce different cache key")
	}

	// Dep hash order should not matter (sorted internally)
	key8 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", []string{"./dep2:h2", "./dep1:h1"})
	key9 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", []string{"./dep1:h1", "./dep2:h2"})
	if key8 != key9 {
		t.Error("dep hash order should not affect cache key")
	}

	// Nil vs empty dep hashes should produce same key
	key10 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", nil)
	key11 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", []string{})
	if key10 != key11 {
		t.Error("nil and empty dep hashes should produce same cache key")
	}

	// T0205: Different build mode should change key
	key12 := BuildCacheKey("impl1", "compiler1", "x86_64", "debug", nil)
	key13 := BuildCacheKey("impl1", "compiler1", "x86_64", "release", nil)
	if key12 == key13 {
		t.Error("different build mode should produce different cache key")
	}
}

func TestBuildCacheRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	meta := &CacheMeta{
		Kind:          CacheKindLLVMModule,
		Name:          "mymod",
		CacheKey:      cacheKey,
		InterfaceHash: "iface_hash_123",
	}

	// Initially no cache
	if got := LookupBuildCache(cacheDir, cacheKey); got != "" {
		t.Errorf("expected no cached obj, got %q", got)
	}

	// Create a fake .o file and save it
	objFile := filepath.Join(t.TempDir(), "test.o")
	os.WriteFile(objFile, []byte("fake object data"), 0644)

	err := SaveBuildCache(cacheDir, cacheKey, meta, objFile)
	if err != nil {
		t.Fatal(err)
	}

	// Now lookup should succeed
	cached := LookupBuildCache(cacheDir, cacheKey)
	if cached == "" {
		t.Fatal("expected cached obj after save")
	}

	// Read back the object data
	data, err := os.ReadFile(cached)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake object data" {
		t.Errorf("cached data = %q, want %q", string(data), "fake object data")
	}

	// Sidecar metadata should be readable
	gotMeta := ReadBuildCacheMeta(cacheDir, cacheKey)
	if gotMeta == nil {
		t.Fatal("expected non-nil meta after save")
	}
	if gotMeta.Kind != CacheKindLLVMModule {
		t.Errorf("meta.Kind = %q, want %q", gotMeta.Kind, CacheKindLLVMModule)
	}
	if gotMeta.Name != "mymod" {
		t.Errorf("meta.Name = %q, want %q", gotMeta.Name, "mymod")
	}
	if gotMeta.InterfaceHash != "iface_hash_123" {
		t.Errorf("meta.InterfaceHash = %q, want %q", gotMeta.InterfaceHash, "iface_hash_123")
	}
	if gotMeta.CreatedAt == "" {
		t.Error("meta.CreatedAt should be set")
	}

	// Two-level directory structure should exist
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if _, err := os.Stat(subdir); err != nil {
		t.Errorf("expected two-level subdir %s to exist", subdir)
	}
}

func TestFormatCacheKeyInputs(t *testing.T) {
	inputs := []CacheKeyInput{
		{Label: "impl", Value: "abcdef1234567890abcdef1234567890"},
		{Label: "compiler", Value: "1111222233334444"},
		{Label: "target", Value: "x86_64"},
		{Label: "dep ./mymod", Value: "aaaabbbbccccddddeeee"},
	}
	out := FormatCacheKeyInputs("test-name", "finalkey123", inputs)
	if !strings.Contains(out, "[cache DEBUG] test-name") {
		t.Error("expected header with name")
	}
	if !strings.Contains(out, "impl: abcdef1234567890") {
		t.Error("expected truncated impl hash")
	}
	if !strings.Contains(out, "target: x86_64") {
		t.Error("expected target (not truncated, shorter than 16)")
	}
	if !strings.Contains(out, "dep ./mymod: aaaabbbbccccdddd") {
		t.Error("expected truncated dep hash")
	}
	if !strings.Contains(out, "key: finalkey123") {
		t.Error("expected final key")
	}
}

func TestCleanAll(t *testing.T) {
	dir := t.TempDir()

	// Create files and subdirectories
	os.MkdirAll(filepath.Join(dir, "ab"), 0755)
	os.WriteFile(filepath.Join(dir, "ab", "abc123.o"), []byte("obj"), 0644)
	os.WriteFile(filepath.Join(dir, "ab", "abc123.o.meta"), []byte(`{"kind":"module"}`), 0644)
	os.MkdirAll(filepath.Join(dir, "cd"), 0755)
	os.WriteFile(filepath.Join(dir, "cd", "cde456.o"), []byte("obj2"), 0644)
	os.WriteFile(filepath.Join(dir, "loose.txt"), []byte("data"), 0644)

	if err := CleanAll(dir); err != nil {
		t.Fatal(err)
	}

	// All contents should be removed, but the dir itself still exists
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty dir after CleanAll, got %d entries", len(entries))
	}
}

func TestCleanAllPreservesLock(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, ".lock"), []byte{}, 0644)
	os.WriteFile(filepath.Join(dir, "data.o"), []byte("obj"), 0644)

	if err := CleanAll(dir); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".lock" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected only .lock to survive, got %v", names)
	}
}

func TestCleanAllNonexistent(t *testing.T) {
	// Should not error on nonexistent directory
	if err := CleanAll("/tmp/nonexistent-promise-test-dir-12345"); err != nil {
		t.Errorf("CleanAll on nonexistent dir should not error, got: %v", err)
	}
}

func TestCleanBuildCache(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("PROMISE_HOME", tmpHome)

	// Create some cached files
	buildDir := filepath.Join(tmpHome, "cache", "build", "ab")
	os.MkdirAll(buildDir, 0755)
	os.WriteFile(filepath.Join(buildDir, "abc.o"), []byte("data"), 0644)

	if err := CleanBuildCache(); err != nil {
		t.Fatal(err)
	}

	// The build cache contents should be cleaned
	entries, _ := os.ReadDir(filepath.Join(tmpHome, "cache", "build"))
	if len(entries) != 0 {
		t.Errorf("expected empty build cache after clean, got %d entries", len(entries))
	}
}

func TestBuildCacheDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("PROMISE_HOME", tmpHome)

	dir, err := BuildCacheDir()
	if err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(tmpHome, "cache", "build")
	if dir != expected {
		t.Errorf("BuildCacheDir() = %q, want %q", dir, expected)
	}

	// Directory should be created
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("BuildCacheDir should create the directory: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestSaveBuildCacheBadObjFile(t *testing.T) {
	cacheDir := t.TempDir()
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	err := SaveBuildCache(cacheDir, cacheKey, &CacheMeta{Kind: CacheKindLLVMModule, Name: "test"}, "/nonexistent/file.o")
	if err == nil {
		t.Fatal("expected error for nonexistent object file")
	}
}

func TestCompilerHash(t *testing.T) {
	h1 := CompilerHash()
	if h1 == "" {
		t.Fatal("expected non-empty compiler hash")
	}
	// Should be stable
	h2 := CompilerHash()
	if h1 != h2 {
		t.Error("compiler hash should be stable across calls")
	}
}

func TestReadBuildCacheMetaMissing(t *testing.T) {
	dir := t.TempDir()
	got := ReadBuildCacheMeta(dir, "nonexistent_key_000000000000000000000000000000000000000000000000000000000000")
	if got != nil {
		t.Errorf("expected nil for missing meta, got %+v", got)
	}
}

func TestHashModuleSourcesBadDir(t *testing.T) {
	_, err := HashModuleSources("/nonexistent/dir/12345", false)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestHashModuleSourcesExcludesTestFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "impl.pr"), []byte("func impl() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "impl_test.pr"), []byte("test_impl() `test {}"), 0644)

	// Without tests: test file should not affect hash
	h1, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	// With tests: hash should differ
	h2, err := HashModuleSources(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("hash with tests should differ from hash without tests")
	}

	// Changing test file should not affect non-test hash
	os.WriteFile(filepath.Join(dir, "impl_test.pr"), []byte("test_impl_v2() `test {}"), 0644)
	h3, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h3 {
		t.Error("changing test file should not affect non-test hash")
	}

	// But should affect test hash
	h4, err := HashModuleSources(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h4 {
		t.Error("changing test file should affect test hash")
	}
}

func TestCollectModuleSourcesSubdirs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "root.pr"), []byte("func root() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "root_test.pr"), []byte("test() `test {}"), 0644)

	// Subdir without promise.toml — included
	sub := filepath.Join(dir, "helpers")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "helper.pr"), []byte("func helper() {}"), 0644)
	os.WriteFile(filepath.Join(sub, "helper_test.pr"), []byte("helper_test() `test {}"), 0644)

	// Nested module subdir with promise.toml — excluded
	nested := filepath.Join(dir, "nested")
	os.MkdirAll(nested, 0755)
	os.WriteFile(filepath.Join(nested, "promise.toml"), []byte("name = \"nested\"\n"), 0644)
	os.WriteFile(filepath.Join(nested, "nested.pr"), []byte("func nested() {}"), 0644)

	// Without tests
	files, err := CollectModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Should include root.pr and helpers/helper.pr, exclude tests and nested
	if len(files) != 2 {
		t.Errorf("CollectModuleSources(false) = %d files, want 2: %v", len(files), files)
	}

	// With tests
	files, err = CollectModuleSources(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	// Should include root.pr, root_test.pr, helpers/helper.pr, helpers/helper_test.pr
	if len(files) != 4 {
		t.Errorf("CollectModuleSources(true) = %d files, want 4: %v", len(files), files)
	}

	// Nested module files should never be included
	for _, f := range files {
		if filepath.Base(f) == "nested.pr" {
			t.Errorf("nested module file should not be included: %s", f)
		}
	}
}

func TestHashModuleSourcesSubdirs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "root.pr"), []byte("func root() {}"), 0644)

	h1, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	// Adding a file in a subdirectory should change the hash
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "sub.pr"), []byte("func sub() {}"), 0644)

	h2, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("adding subdir file should change the hash")
	}

	// Adding a nested module should NOT change the hash
	nested := filepath.Join(dir, "nested")
	os.MkdirAll(nested, 0755)
	os.WriteFile(filepath.Join(nested, "promise.toml"), []byte("name = \"nested\"\n"), 0644)
	os.WriteFile(filepath.Join(nested, "nested.pr"), []byte("func nested() {}"), 0644)

	h3, err := HashModuleSources(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if h2 != h3 {
		t.Error("nested module files should not affect parent hash")
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.pr")
	os.WriteFile(f, []byte("func hello() {}"), 0644)

	h1, err := HashFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	// Same content → same hash
	h2, err := HashFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("same file should produce same hash")
	}

	// Different content → different hash
	os.WriteFile(f, []byte("func goodbye() {}"), 0644)
	h3, err := HashFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h3 {
		t.Error("different content should produce different hash")
	}

	// Nonexistent file → error
	_, err = HashFile(filepath.Join(dir, "nonexistent.pr"))
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestHashDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.pr"), []byte("func a() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "b.pr"), []byte("func b() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hello"), 0644)

	h1, err := HashDir(dir, ".pr")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	// Non-.pr files should not affect hash
	os.WriteFile(filepath.Join(dir, "extra.md"), []byte("more"), 0644)
	h2, err := HashDir(dir, ".pr")
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("non-.pr files should not affect hash")
	}

	// Changing a .pr file should change hash
	os.WriteFile(filepath.Join(dir, "a.pr"), []byte("func a_v2() {}"), 0644)
	h3, err := HashDir(dir, ".pr")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h3 {
		t.Error("changed .pr file should change hash")
	}

	// Subdirectories should be ignored (flat only).
	sub := filepath.Join(dir, "subdir")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "nested.pr"), []byte("func nested() {}"), 0644)
	h4, err := HashDir(dir, ".pr")
	if err != nil {
		t.Fatal(err)
	}
	if h3 != h4 {
		t.Error("subdirectory files should not affect hash (flat only)")
	}

	// Nonexistent dir → error
	_, err = HashDir("/nonexistent/dir/12345", ".pr")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestTestBinaryCacheRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Initially no cached binary.
	if got := LookupTestBinaryCache(cacheDir, cacheKey); got != "" {
		t.Errorf("expected no cached binary, got %q", got)
	}

	// Create a fake binary and save it.
	binFile := filepath.Join(t.TempDir(), "test-bin")
	os.WriteFile(binFile, []byte("fake binary data"), 0755)

	if err := SaveTestBinaryCache(cacheDir, cacheKey, binFile); err != nil {
		t.Fatal(err)
	}

	// Lookup should succeed.
	cached := LookupTestBinaryCache(cacheDir, cacheKey)
	if cached == "" {
		t.Fatal("expected cached binary after save")
	}

	// Read back and verify content.
	data, err := os.ReadFile(cached)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake binary data" {
		t.Errorf("cached data = %q, want %q", string(data), "fake binary data")
	}

	// Verify executable permissions (POSIX only — Windows determines executability by extension).
	if runtime.GOOS != "windows" {
		info, err := os.Stat(cached)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0111 == 0 {
			t.Errorf("cached binary should be executable, mode = %v", info.Mode())
		}
	}

	// Two-level directory structure should exist.
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if _, err := os.Stat(subdir); err != nil {
		t.Errorf("expected two-level subdir %s to exist", subdir)
	}
}

func TestSaveTestBinaryCacheBadFile(t *testing.T) {
	cacheDir := t.TempDir()
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	err := SaveTestBinaryCache(cacheDir, cacheKey, "/nonexistent/binary")
	if err == nil {
		t.Fatal("expected error for nonexistent binary file")
	}
}

func TestTestBinaryMetaRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Initially no metadata
	if got := LoadTestBinaryMeta(cacheDir, cacheKey); got != nil {
		t.Errorf("expected nil meta, got %+v", got)
	}

	// Save unit test metadata
	unitMeta := &CacheMeta{
		Kind:         CacheKindBinary,
		Name:         "tests/unit_test.pr",
		CacheKey:     cacheKey,
		Tests:        []string{"test_add", "test_sub"},
		TestExcludes: map[string][]string{"test_sub": {"wasm32-wasi"}},
	}
	if err := SaveTestBinaryMeta(cacheDir, cacheKey, unitMeta); err != nil {
		t.Fatal(err)
	}

	got := LoadTestBinaryMeta(cacheDir, cacheKey)
	if got == nil {
		t.Fatal("expected non-nil meta after save")
	}
	if got.E2E {
		t.Error("expected E2E=false for unit test meta")
	}
	if len(got.Tests) != 2 || got.Tests[0] != "test_add" || got.Tests[1] != "test_sub" {
		t.Errorf("unexpected tests: %v", got.Tests)
	}
	if excludes, ok := got.TestExcludes["test_sub"]; !ok || len(excludes) != 1 || excludes[0] != "wasm32-wasi" {
		t.Errorf("unexpected test excludes: %v", got.TestExcludes)
	}
	if got.Kind != CacheKindBinary {
		t.Errorf("meta.Kind = %q, want %q", got.Kind, CacheKindBinary)
	}
	if got.CacheKey != cacheKey {
		t.Errorf("meta.CacheKey = %q, want %q", got.CacheKey, cacheKey)
	}
	if got.CreatedAt == "" {
		t.Error("meta.CreatedAt should be set")
	}

	// Save E2E metadata (overwrites)
	e2eMeta := &CacheMeta{
		Kind:           CacheKindBinary,
		Name:           "tests/e2e_test.pr",
		CacheKey:       cacheKey,
		E2E:            true,
		ExpectedOutput: "hello\nworld",
		ExcludeTargets: []string{"wasm32-wasi"},
	}
	if err := SaveTestBinaryMeta(cacheDir, cacheKey, e2eMeta); err != nil {
		t.Fatal(err)
	}

	got = LoadTestBinaryMeta(cacheDir, cacheKey)
	if got == nil {
		t.Fatal("expected non-nil meta after E2E save")
	}
	if !got.E2E {
		t.Error("expected E2E=true")
	}
	if got.ExpectedOutput != "hello\nworld" {
		t.Errorf("expected output = %q, want %q", got.ExpectedOutput, "hello\nworld")
	}
	if len(got.ExcludeTargets) != 1 || got.ExcludeTargets[0] != "wasm32-wasi" {
		t.Errorf("unexpected exclude targets: %v", got.ExcludeTargets)
	}
}

func TestLockBuildDirShared(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("PROMISE_HOME", tmpHome)

	// First shared lock should succeed immediately.
	unlock1 := LockBuildDirShared()

	// Lock file should exist.
	lockPath := filepath.Join(tmpHome, "cache", "build", ".lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}

	// Second shared lock should also succeed (shared locks are concurrent).
	unlock2 := LockBuildDirShared()

	// Release both.
	unlock2()
	unlock1()

	// Re-acquire exclusive should succeed (no stale lock).
	unlock3 := LockBuildDirExclusive()
	unlock3()
}

func TestPromiseHome(t *testing.T) {
	// Default should use home dir
	home, err := PromiseHome()
	if err != nil {
		t.Fatal(err)
	}
	if home == "" {
		t.Fatal("expected non-empty home")
	}

	// PROMISE_HOME override
	t.Setenv("PROMISE_HOME", "/tmp/test-promise-home")
	home, err = PromiseHome()
	if err != nil {
		t.Fatal(err)
	}
	if home != "/tmp/test-promise-home" {
		t.Errorf("PromiseHome() = %q, want /tmp/test-promise-home", home)
	}
}

// --- InstanceCacheKey tests ---

func TestInstanceCacheKeyNonEmpty(t *testing.T) {
	key := InstanceCacheKey("", "Vector[int]", "decl123", "compiler456", "x86_64-linux-musl", "debug", nil)
	if key == "" {
		t.Error("expected non-empty key")
	}
	// FNV-128a → 16 bytes → 32 hex chars
	if len(key) != 32 {
		t.Errorf("expected 32 hex chars, got %d: %q", len(key), key)
	}
}

func TestInstanceCacheKeyDeterminism(t *testing.T) {
	k1 := InstanceCacheKey("", "Vector[int]", "decl", "cmp", "linux", "debug", nil)
	k2 := InstanceCacheKey("", "Vector[int]", "decl", "cmp", "linux", "debug", nil)
	if k1 != k2 {
		t.Errorf("non-deterministic: %q != %q", k1, k2)
	}
}

func TestInstanceCacheKeyDifferentIRPrefix(t *testing.T) {
	k1 := InstanceCacheKey("", "Vector[int]", "decl", "cmp", "linux", "debug", nil)
	k2 := InstanceCacheKey("mymod", "Vector[int]", "decl", "cmp", "linux", "debug", nil)
	if k1 == k2 {
		t.Error("different irPrefix should produce different key")
	}
}

func TestInstanceCacheKeyDifferentMonoName(t *testing.T) {
	k1 := InstanceCacheKey("", "Vector[int]", "decl", "cmp", "linux", "debug", nil)
	k2 := InstanceCacheKey("", "Vector[string]", "decl", "cmp", "linux", "debug", nil)
	if k1 == k2 {
		t.Error("different monoName should produce different key")
	}
}

func TestInstanceCacheKeyDifferentDeclHash(t *testing.T) {
	k1 := InstanceCacheKey("", "Vector[int]", "hash1", "cmp", "linux", "debug", nil)
	k2 := InstanceCacheKey("", "Vector[int]", "hash2", "cmp", "linux", "debug", nil)
	if k1 == k2 {
		t.Error("different typeDeclHash should produce different key")
	}
}

func TestInstanceCacheKeyDifferentCompilerHash(t *testing.T) {
	k1 := InstanceCacheKey("", "Vector[int]", "decl", "compiler1", "linux", "debug", nil)
	k2 := InstanceCacheKey("", "Vector[int]", "decl", "compiler2", "linux", "debug", nil)
	if k1 == k2 {
		t.Error("different compilerHash should produce different key")
	}
}

func TestInstanceCacheKeyDifferentTarget(t *testing.T) {
	k1 := InstanceCacheKey("", "Vector[int]", "decl", "cmp", "x86_64-linux-musl", "debug", nil)
	k2 := InstanceCacheKey("", "Vector[int]", "decl", "cmp", "aarch64-apple-macos14", "debug", nil)
	if k1 == k2 {
		t.Error("different target should produce different key")
	}
}

func TestInstanceCacheKeyPrefixCollisionFree(t *testing.T) {
	// Distinct (irPrefix, monoName) pairs that share a naive concatenation must not collide.
	// e.g., ("ab", "cdef") vs ("abc", "def") should differ.
	k1 := InstanceCacheKey("ab", "cdef", "d", "c", "t", "debug", nil)
	k2 := InstanceCacheKey("abc", "def", "d", "c", "t", "debug", nil)
	if k1 == k2 {
		t.Error("prefix/name boundary collision: different (irPrefix, monoName) pairs produced same key")
	}
}

func TestInstanceCacheKeyAllDistinct(t *testing.T) {
	// All seven parameters independently affect the key.
	base := InstanceCacheKey("pfx", "Box[int]", "decl", "cmp", "linux", "debug", nil)
	variants := []string{
		InstanceCacheKey("pfx2", "Box[int]", "decl", "cmp", "linux", "debug", nil),      // irPrefix differs
		InstanceCacheKey("pfx", "Box[string]", "decl", "cmp", "linux", "debug", nil),    // monoName differs
		InstanceCacheKey("pfx", "Box[int]", "decl2", "cmp", "linux", "debug", nil),      // typeDeclHash differs
		InstanceCacheKey("pfx", "Box[int]", "decl", "cmp2", "linux", "debug", nil),      // compilerHash differs
		InstanceCacheKey("pfx", "Box[int]", "decl", "cmp", "wasm32-wasi", "debug", nil), // target differs
		InstanceCacheKey("pfx", "Box[int]", "decl", "cmp", "linux", "release", nil),     // buildMode differs (T0205)
	}
	for i, k := range variants {
		if k == base {
			t.Errorf("variant %d produced same key as base: %q", i, base)
		}
	}
}

// TestInstanceCacheKeyCoverageMode locks the T0574 isolation contract: a
// coverage instance .bc (externally-linked counter globals) must never share a
// cache entry with a non-coverage build of the same instance. compileAndLinkSeparate
// passes "debug+cov" as the build mode when coverage is enabled; this asserts
// that flows through to a distinct key.
func TestInstanceCacheKeyCoverageMode(t *testing.T) {
	mc := []string{"std"}
	plain := InstanceCacheKey("pfx", "Box[int]", "decl", "cmp", "linux", "debug", mc)
	cov := InstanceCacheKey("pfx", "Box[int]", "decl", "cmp", "linux", "debug+cov", mc)
	if plain == cov {
		t.Errorf("coverage and non-coverage instance cache keys must differ, both = %q", plain)
	}
}

// === Embed hash functions (T0032) ===

func TestIsGlobPattern(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"templates/*.html", true},
		{"data/[abc].txt", true},
		{"logs/app?.log", true},
		{"plain/file.txt", false},
		{"dir/sub/file.pr", false},
		{"", false},
	}
	for _, tc := range tests {
		got := IsGlobPattern(tc.path)
		if got != tc.want {
			t.Errorf("IsGlobPattern(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestHashEmbedFilesSingleFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "data.json"), []byte(`{"key":"val"}`), 0644)

	content := []byte("get config string `embed(\"data.json\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0], "data.json:") {
		t.Errorf("expected entry to start with 'data.json:', got %q", entries[0])
	}
}

func TestHashEmbedFilesNoAnnotations(t *testing.T) {
	content := []byte("get name string { return \"hello\"; }")
	entries, ok := HashEmbedFiles(content, t.TempDir())
	if !ok {
		t.Fatal("expected ok")
	}
	if entries != nil {
		t.Errorf("expected nil entries for no annotations, got %v", entries)
	}
}

func TestHashEmbedFilesMissingFile(t *testing.T) {
	content := []byte("get config string `embed(\"missing.txt\");")
	_, ok := HashEmbedFiles(content, t.TempDir())
	if ok {
		t.Error("expected ok=false for missing file")
	}
}

func TestHashEmbedFilesDeterministic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0644)

	content := []byte("get a string `embed(\"a.txt\");")
	e1, _ := HashEmbedFiles(content, dir)
	e2, _ := HashEmbedFiles(content, dir)
	if len(e1) != len(e2) {
		t.Fatal("different lengths")
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Errorf("entry %d differs: %q vs %q", i, e1[i], e2[i])
		}
	}
}

func TestHashEmbedFilesContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	os.WriteFile(path, []byte("v1"), 0644)

	content := []byte("get d string `embed(\"data.txt\");")
	e1, _ := HashEmbedFiles(content, dir)

	os.WriteFile(path, []byte("v2"), 0644)
	e2, _ := HashEmbedFiles(content, dir)

	if len(e1) != 1 || len(e2) != 1 {
		t.Fatal("expected 1 entry each")
	}
	if e1[0] == e2[0] {
		t.Error("hash should change when file content changes")
	}
}

func TestHashEmbedFilesMultipleAnnotations(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0644)

	content := []byte("get a string `embed(\"a.txt\");\nget b string `embed(\"b.txt\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Sorted order
	if !strings.HasPrefix(entries[0], "a.txt:") {
		t.Errorf("first entry should be a.txt, got %q", entries[0])
	}
	if !strings.HasPrefix(entries[1], "b.txt:") {
		t.Errorf("second entry should be b.txt, got %q", entries[1])
	}
}

func TestHashEmbedFilesGlob(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "data"), 0755)
	os.WriteFile(filepath.Join(dir, "data", "a.txt"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(dir, "data", "b.txt"), []byte("bbb"), 0644)
	os.WriteFile(filepath.Join(dir, "data", "c.csv"), []byte("ccc"), 0644)

	content := []byte("get files EmbeddedFiles `embed(\"data/*.txt\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (only .txt), got %d", len(entries))
	}
}

func TestHashEmbedFilesDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "x.txt"), []byte("xxx"), 0644)
	os.WriteFile(filepath.Join(sub, "y.txt"), []byte("yyy"), 0644)

	content := []byte("get assets EmbeddedFiles `embed(\"assets/...\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestHashEmbedFilesForInputsSingleFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "data.json"), []byte(`{"k":"v"}`), 0644)

	content := []byte("get config string `embed(\"data.json\");")
	inputs := HashEmbedFilesForInputs(content, dir)
	if len(inputs) != 1 {
		t.Fatalf("expected 1 input, got %d", len(inputs))
	}
	if inputs[0].Label != "embed data.json" {
		t.Errorf("expected label 'embed data.json', got %q", inputs[0].Label)
	}
	if inputs[0].Value == "" {
		t.Error("expected non-empty hash value")
	}
}

func TestHashEmbedFilesForInputsNoAnnotations(t *testing.T) {
	content := []byte("get name string { return \"hello\"; }")
	inputs := HashEmbedFilesForInputs(content, t.TempDir())
	if inputs != nil {
		t.Errorf("expected nil for no annotations, got %v", inputs)
	}
}

func TestHashEmbedFilesForInputsMissing(t *testing.T) {
	content := []byte("get config string `embed(\"missing.txt\");")
	inputs := HashEmbedFilesForInputs(content, t.TempDir())
	if len(inputs) != 0 {
		t.Errorf("expected 0 inputs for missing file (silently skipped), got %d", len(inputs))
	}
}

func TestHashModuleEmbedsNoEmbeds(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.pr"), []byte("main() { println(\"hi\"); }"), 0644)

	h := HashModuleEmbeds(dir, false)
	if h != "" {
		t.Errorf("expected empty hash for no embeds, got %q", h)
	}
}

func TestHashModuleEmbedsWithEmbed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mod.pr"), []byte("get data string `embed(\"data.txt\");"), 0644)
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello"), 0644)

	h := HashModuleEmbeds(dir, false)
	if h == "" {
		t.Fatal("expected non-empty hash")
	}

	// Changing embedded file changes hash
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte("world"), 0644)
	h2 := HashModuleEmbeds(dir, false)
	if h2 == "" {
		t.Fatal("expected non-empty hash")
	}
	if h == h2 {
		t.Error("hash should change when embedded file content changes")
	}
}

func TestHashModuleEmbedsExcludesTestFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mod.pr"), []byte("main() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "mod_test.pr"), []byte("get data string `embed(\"data.txt\");"), 0644)
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte("test data"), 0644)

	h := HashModuleEmbeds(dir, false)
	if h != "" {
		t.Errorf("expected empty hash (test file embeds excluded), got %q", h)
	}

	// With includeTests=true, test file embeds are included
	h2 := HashModuleEmbeds(dir, true)
	if h2 == "" {
		t.Error("expected non-empty hash with includeTests=true")
	}
}

func TestHashModuleEmbedsBadDir(t *testing.T) {
	h := HashModuleEmbeds("/nonexistent/dir", false)
	if h != "" {
		t.Errorf("expected empty hash for bad dir, got %q", h)
	}
}

func TestHashEmbedDirSkipsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "visible.txt"), []byte("ok"), 0644)
	os.WriteFile(filepath.Join(sub, ".hidden.txt"), []byte("no"), 0644)

	content := []byte("get a EmbeddedFiles `embed(\"assets/...\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (hidden skipped), got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0], "assets/visible.txt:") {
		t.Errorf("expected visible.txt entry, got %q", entries[0])
	}
}

func TestHashEmbedDirSkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets")
	hidden := filepath.Join(sub, ".git")
	os.MkdirAll(hidden, 0755)
	os.WriteFile(filepath.Join(sub, "ok.txt"), []byte("ok"), 0644)
	os.WriteFile(filepath.Join(hidden, "secret"), []byte("no"), 0644)

	content := []byte("get a EmbeddedFiles `embed(\"assets/...\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (hidden dir skipped), got %d", len(entries))
	}
}

func TestHashEmbedGlobSkipsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, ".hidden.txt"), []byte("h"), 0644)

	content := []byte("get a EmbeddedFiles `embed(\"*.txt\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (hidden skipped), got %d", len(entries))
	}
}

func TestHashEmbedGlobSkipsDirs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("f"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir.txt"), 0755) // dir named like a file

	content := []byte("get a EmbeddedFiles `embed(\"*.txt\");")
	entries, ok := HashEmbedFiles(content, dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (dir skipped), got %d", len(entries))
	}
}
