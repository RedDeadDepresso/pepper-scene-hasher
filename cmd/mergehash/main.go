// mergehash merges the per-folder JSON files (found recursively) produced by pnghash into a single
// JSON file mapping xxhash -> {"author": <name>, "year": <year or null>}.
//
// The author is the name of the JSON file (without ".json"). With -names, a
// JSON file mapping that name -> string is used to replace it. Names missing
// from it, or mapped to an empty string, fall back to the plain file name.
//
// The year is extracted from the file path: a trailing year (optionally
// followed by -MM-DD) in the file name, e.g. "Bazett_2026.json" or
// "Krib_2024-05-01.json", or failing that in a parent folder name. If no year
// is found it is null.
//
// If the same hash appears in several files, the first file in sorted name
// order wins and a warning is printed.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type entry struct {
	XXH string `json:"xxh"`
}

// info is the value stored for each hash in the merged output.
type info struct {
	Author string `json:"author"`
	Year   *int   `json:"year"` // null when no year could be found in the path
}

// yearRe matches a year at the end of a name, either on its own or followed by
// a -MM-DD date, and preceded by the start of the name or a separator. Anchoring
// to the end avoids picking up unrelated numbers such as "Anon_2232_2026-08-01".
var yearRe = regexp.MustCompile(`(?:^|[_\-\s])((?:19|20)\d{2})(?:-\d{2}-\d{2})?$`)

// yearFromPath returns the year found in the file name (without extension),
// falling back to each parent folder name from nearest to farthest.
func yearFromPath(path string) *int {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if m := yearRe.FindStringSubmatch(name); m != nil {
			y, _ := strconv.Atoi(m[1])
			return &y
		}
		parent := filepath.Dir(dir)
		if parent == dir || dir == "." || dir == string(filepath.Separator) {
			return nil
		}
		name = filepath.Base(dir)
	}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func same(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}

func main() {
	input := flag.String("input", "", "folder containing the JSON files to merge, searched recursively (required)")
	output := flag.String("output", "merged.json", "path of the merged JSON file")
	namesPath := flag.String("names", "", "optional JSON file mapping json file name (no .json) -> value")
	flag.Parse()

	if *input == "" {
		fmt.Fprintln(os.Stderr, "error: -input is required")
		flag.Usage()
		os.Exit(2)
	}

	names := map[string]string{}
	if *namesPath != "" {
		if err := readJSON(*namesPath, &names); err != nil {
			fmt.Fprintf(os.Stderr, "error: reading names file: %v\n", err)
			os.Exit(1)
		}
	}

	var paths []string
	err := filepath.WalkDir(*input, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".json") {
			return nil
		}
		// Don't merge our own output or the names file if they live in the input folder.
		if same(p, *output) || (*namesPath != "" && same(p, *namesPath)) {
			return nil
		}
		paths = append(paths, p) // WalkDir visits in lexical order
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot read input folder:", err)
		os.Exit(1)
	}

	merged := map[string]info{}
	owner := map[string]string{} // hash -> file name that claimed it first
	files := 0
	for _, full := range paths {
		base := filepath.Base(full)
		name := strings.TrimSuffix(base, filepath.Ext(base))
		var data map[string]entry
		if err := readJSON(full, &data); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", full, err)
			os.Exit(1)
		}
		author := name
		if v, ok := names[name]; ok && v != "" {
			author = v
		}
		value := info{Author: author, Year: yearFromPath(full)}
		files++
		for _, e := range data {
			if e.XXH == "" {
				continue
			}
			if prev, ok := owner[e.XXH]; ok {
				if prev != name {
					fmt.Fprintf(os.Stderr, "warning: hash %s is in both %q and %q; keeping %q\n", e.XXH, prev, name, prev)
				}
				continue
			}
			owner[e.XXH] = name
			merged[e.XXH] = value
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)                   // keep "&" as "&" instead of \u0026
	if err := enc.Encode(merged); err != nil { // compact; Encode appends a newline
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	b := buf.Bytes()
	if dir := filepath.Dir(*output); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(*output, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("Merged %d file(s), %d unique hash(es) -> %s\n", files, len(merged), *output)
}