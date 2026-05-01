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

func TestBuildCacheRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()
	cacheKey := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	ifaceHash := "iface_hash_123"

	// Initially no cache
	if got := LookupBuildCache(cacheDir, cacheKey); got != "" {
		t.Errorf("expected no cached obj, got %q", got)
	}

	// Create a fake .o file and save it
	objFile := filepath.Join(t.TempDir(), "test.o")
	os.WriteFile(objFile, []byte("fake object data"), 0644)

	err := SaveBuildCache(cacheDir, cacheKey, ifaceHash, objFile)
	if err != nil {
		t.Fatal(err)
	}

	// Now lookup should succeed
	cached := LookupBuildCache(cacheDir, cacheKey)
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
	gotIface := ReadBuildCacheInterfaceHash(cacheDir, cacheKey)
	if gotIface != ifaceHash {
		t.Errorf("interface hash = %q, want %q", gotIface, ifaceHash)
	}

	// Two-level directory structure should exist
	subdir := filepath.Join(cacheDir, cacheKey[:2])
	if _, err := os.Stat(subdir); err != nil {
		t.Errorf("expected two-level subdir %s to exist", subdir)
	}
}

func TestCleanAll(t *testing.T) {
	dir := t.TempDir()

	// Create files and subdirectories
	os.MkdirAll(filepath.Join(dir, "ab"), 0755)
	os.WriteFile(filepath.Join(dir, "ab", "abc123.o"), []byte("obj"), 0644)
	os.WriteFile(filepath.Join(dir, "ab", "abc123.interface"), []byte("hash"), 0644)
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

	err := SaveBuildCache(cacheDir, cacheKey, "hash", "/nonexistent/file.o")
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

func TestReadBuildCacheInterfaceHashMissing(t *testing.T) {
	dir := t.TempDir()
	got := ReadBuildCacheInterfaceHash(dir, "nonexistent_key_000000000000000000000000000000000000000000000000000000000000")
	if got != "" {
		t.Errorf("expected empty string for missing interface hash, got %q", got)
	}
}

func TestHashModuleSourcesBadDir(t *testing.T) {
	_, err := HashModuleSources("/nonexistent/dir/12345")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
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
