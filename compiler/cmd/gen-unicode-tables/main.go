// gen-unicode-tables generates Promise source files containing Unicode
// normalization data tables (CCC, decomposition, composition, quick-check).
//
// Usage:
//
//	go run ./cmd/gen-unicode-tables/
//
// Output files are written to modules/std/ in the repo root.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const unicodeVersion = "16.0.0"

const (
	unicodeDataURL           = "https://www.unicode.org/Public/" + unicodeVersion + "/ucd/UnicodeData.txt"
	compositionExclusionsURL = "https://www.unicode.org/Public/" + unicodeVersion + "/ucd/CompositionExclusions.txt"
	derivedNormPropsURL      = "https://www.unicode.org/Public/" + unicodeVersion + "/ucd/DerivedNormalizationProps.txt"
)

// Hangul constants
const (
	SBase  = 0xAC00
	LBase  = 0x1100
	VBase  = 0x1161
	TBase  = 0x11A7
	LCount = 19
	VCount = 21
	TCount = 28
	NCount = VCount * TCount // 588
	SCount = LCount * NCount // 11172
)

type decomp struct {
	cp       int
	isCompat bool
	mapping  []int
}

type cpRange struct {
	start, end int
}

func main() {
	repoRoot := findRepoRoot()
	outDir := filepath.Join(repoRoot, "modules", "std")

	fmt.Println("Downloading UCD files...")
	unicodeData := fetch(unicodeDataURL)
	compositionExcl := fetch(compositionExclusionsURL)
	derivedNormProps := fetch(derivedNormPropsURL)

	fmt.Println("Parsing UCD data...")
	ccc := parseCCC(unicodeData)
	decomps := parseDecompositions(unicodeData)
	exclusions := parseCompositionExclusions(compositionExcl)
	nfdQCRanges, nfcQCNoRanges, nfcQCMaybeRanges := parseQuickCheckRanges(derivedNormProps)

	var canonDecomps []decomp
	for _, d := range decomps {
		if !d.isCompat {
			canonDecomps = append(canonDecomps, d)
		}
	}

	compositionPairs := buildCompositionPairs(canonDecomps, exclusions)

	fmt.Printf("CCC entries: %d\n", len(ccc))
	fmt.Printf("Canonical decompositions: %d\n", len(canonDecomps))
	fmt.Printf("Composition pairs: %d\n", len(compositionPairs))
	fmt.Printf("NFD_QC=No ranges: %d, NFC_QC=No ranges: %d, NFC_QC=Maybe ranges: %d\n",
		len(nfdQCRanges), len(nfcQCNoRanges), len(nfcQCMaybeRanges))

	fmt.Println("Generating Promise source files...")
	generateCCCFile(outDir, ccc)
	generateDecompFile(outDir, canonDecomps)
	generateCompFile(outDir, compositionPairs)
	generateQCFile(outDir, nfdQCRanges, nfcQCNoRanges, nfcQCMaybeRanges)

	fmt.Println("Done!")
}

// --- Parsing ---

func parseCCC(data []byte) [][2]int {
	var result [][2]int
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ";")
		if len(fields) < 6 {
			continue
		}
		cp := parseHex(fields[0])
		cccVal, _ := strconv.Atoi(fields[3])
		if cccVal > 0 {
			result = append(result, [2]int{cp, cccVal})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i][0] < result[j][0] })
	return result
}

func parseDecompositions(data []byte) []decomp {
	var result []decomp
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ";")
		if len(fields) < 6 {
			continue
		}
		cp := parseHex(fields[0])
		decompField := strings.TrimSpace(fields[5])
		if decompField == "" {
			continue
		}
		if cp >= SBase && cp < SBase+SCount {
			continue
		}

		isCompat := false
		if decompField[0] == '<' {
			isCompat = true
			if _, after, ok := strings.Cut(decompField, ">"); ok {
				decompField = strings.TrimSpace(after)
			}
		}

		parts := strings.Fields(decompField)
		if len(parts) == 0 {
			continue
		}
		var mapping []int
		for _, p := range parts {
			mapping = append(mapping, parseHex(p))
		}
		result = append(result, decomp{cp: cp, isCompat: isCompat, mapping: mapping})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].cp < result[j].cp })
	return result
}

func parseCompositionExclusions(data []byte) map[int]bool {
	result := make(map[int]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cp := parseHex(line)
		result[cp] = true
	}
	return result
}

func parseQuickCheckRanges(data []byte) (nfdNo, nfcNo, nfcMaybe []cpRange) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ";", 2)
		if len(fields) < 2 {
			continue
		}
		cpRangeStr := strings.TrimSpace(fields[0])
		prop := strings.TrimSpace(fields[1])

		var start, end int
		if before, after, ok := strings.Cut(cpRangeStr, ".."); ok {
			start = parseHex(before)
			end = parseHex(after)
		} else {
			start = parseHex(cpRangeStr)
			end = start
		}

		r := cpRange{start, end}
		switch prop {
		case "NFD_QC; N":
			nfdNo = append(nfdNo, r)
		case "NFC_QC; N":
			nfcNo = append(nfcNo, r)
		case "NFC_QC; M":
			nfcMaybe = append(nfcMaybe, r)
		}
	}
	sortRanges := func(rs []cpRange) {
		sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	}
	sortRanges(nfdNo)
	sortRanges(nfcNo)
	sortRanges(nfcMaybe)
	return
}

func buildCompositionPairs(canonDecomps []decomp, exclusions map[int]bool) [][3]int {
	var pairs [][3]int
	for _, d := range canonDecomps {
		if len(d.mapping) != 2 {
			continue
		}
		if exclusions[d.cp] {
			continue
		}
		pairs = append(pairs, [3]int{d.mapping[0], d.mapping[1], d.cp})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	return pairs
}

// --- Code Generation ---

func generateCCCFile(outDir string, ccc [][2]int) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Generated by gen-unicode-tables. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Unicode %s — Canonical Combining Class data.\n", unicodeVersion)
	fmt.Fprintf(&buf, "// %d entries (codepoints with CCC > 0).\n\n", len(ccc))

	writeI32VectorLiteral(&buf, "_ccc_codepoints", extractColumn(ccc, 0))
	writeI32VectorLiteral(&buf, "_ccc_values", extractColumn(ccc, 1))

	writePrFile(outDir, "unicode_ccc.pr", buf.Bytes())
}

func generateDecompFile(outDir string, canonDecomps []decomp) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Generated by gen-unicode-tables. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Unicode %s — Canonical decomposition mapping data.\n", unicodeVersion)
	fmt.Fprintf(&buf, "// %d entries.\n\n", len(canonDecomps))

	cps := make([]int, len(canonDecomps))
	for i, d := range canonDecomps {
		cps[i] = d.cp
	}
	writeI32VectorLiteral(&buf, "_canon_decomp_codepoints", cps)

	offsets := make([]int, 0, len(canonDecomps)+1)
	offset := 0
	for _, d := range canonDecomps {
		offsets = append(offsets, offset)
		offset += len(d.mapping)
	}
	offsets = append(offsets, offset)
	writeI32VectorLiteral(&buf, "_canon_decomp_offsets", offsets)

	var data []int
	for _, d := range canonDecomps {
		data = append(data, d.mapping...)
	}
	writeI32VectorLiteral(&buf, "_canon_decomp_data", data)

	writePrFile(outDir, "unicode_decomp.pr", buf.Bytes())
}

func generateCompFile(outDir string, pairs [][3]int) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Generated by gen-unicode-tables. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Unicode %s — Canonical composition pairs.\n", unicodeVersion)
	fmt.Fprintf(&buf, "// %d pairs.\n\n", len(pairs))

	writeI32VectorLiteral(&buf, "_comp_first", extractTripleColumn(pairs, 0))
	writeI32VectorLiteral(&buf, "_comp_second", extractTripleColumn(pairs, 1))
	writeI32VectorLiteral(&buf, "_comp_result", extractTripleColumn(pairs, 2))

	writePrFile(outDir, "unicode_comp.pr", buf.Bytes())
}

func generateQCFile(outDir string, nfdNo, nfcNo, nfcMaybe []cpRange) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Generated by gen-unicode-tables. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "// Unicode %s — Quick Check property data (ranges).\n", unicodeVersion)
	fmt.Fprintf(&buf, "// NFD_QC=No: %d ranges, NFC_QC=No: %d ranges, NFC_QC=Maybe: %d ranges.\n\n",
		len(nfdNo), len(nfcNo), len(nfcMaybe))

	writeRangeArrays(&buf, "_nfd_qc_no", nfdNo)
	writeRangeArrays(&buf, "_nfc_qc_no", nfcNo)
	writeRangeArrays(&buf, "_nfc_qc_maybe", nfcMaybe)

	writePrFile(outDir, "unicode_qc.pr", buf.Bytes())
}

// writeI32VectorLiteral writes a function returning an i32[] vector literal.
func writeI32VectorLiteral(buf *bytes.Buffer, name string, values []int) {
	fmt.Fprintf(buf, "%s() i32[] {\n  return [\n", name)
	for i := 0; i < len(values); i += 20 {
		buf.WriteString("    ")
		end := min(i+20, len(values))
		for j := i; j < end; j++ {
			if j > i {
				buf.WriteString(", ")
			}
			fmt.Fprintf(buf, "%di32", values[j])
		}
		if end < len(values) {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("  ];\n}\n\n")
}

func writeRangeArrays(buf *bytes.Buffer, name string, ranges []cpRange) {
	starts := make([]int, len(ranges))
	ends := make([]int, len(ranges))
	for i, r := range ranges {
		starts[i] = r.start
		ends[i] = r.end
	}
	writeI32VectorLiteral(buf, name+"_start", starts)
	writeI32VectorLiteral(buf, name+"_end", ends)
}

func extractColumn(pairs [][2]int, idx int) []int {
	result := make([]int, len(pairs))
	for i, p := range pairs {
		result[i] = p[idx]
	}
	return result
}

func extractTripleColumn(pairs [][3]int, idx int) []int {
	result := make([]int, len(pairs))
	for i, p := range pairs {
		result[i] = p[idx]
	}
	return result
}

func writePrFile(outDir, name string, data []byte) {
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("  %s (%d bytes)\n", path, len(data))
}

// --- Helpers ---

func parseHex(s string) int {
	s = strings.TrimSpace(s)
	val, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing hex %q: %v\n", s, err)
		os.Exit(1)
	}
	return int(val)
}

func fetch(url string) []byte {
	cacheDir := filepath.Join(os.TempDir(), "gen-unicode-tables-cache")
	os.MkdirAll(cacheDir, 0755)
	cachePath := filepath.Join(cacheDir, filepath.Base(url))
	if data, err := os.ReadFile(cachePath); err == nil {
		fmt.Printf("  %s (cached)\n", filepath.Base(url))
		return data
	}

	fmt.Printf("  Downloading %s...\n", filepath.Base(url))
	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching %s: %v\n", url, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "HTTP %d for %s\n", resp.StatusCode, url)
		os.Exit(1)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", url, err)
		os.Exit(1)
	}
	os.WriteFile(cachePath, data, 0644)
	return data
}

func findRepoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			wd, _ := os.Getwd()
			return wd
		}
		dir = parent
	}
}
