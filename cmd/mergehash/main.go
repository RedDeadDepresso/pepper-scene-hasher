// mergehash merges the per-folder JSON files (found recursively) produced by pnghash into a single
// JSON file mapping xxhash -> {"author": <name>, "year": <year or null>}.
//
// The author is the name of the JSON file (without ".json"). With -names, a
// JSON file mapping that name -> string is used to replace it. Names missing
// from it, or mapped to an empty string, fall back to the plain file name.
//
// The year is extracted from the image path (the key inside the JSON file), not
// from the JSON file's own name: the deepest folder named exactly like a year
// ("Artist/2023/Series/img.png" -> 2023) wins, otherwise a date (year+month+day) in
// the image file name ("2021_0909_1200.png", "autosave_2023-12-26.png") is
// used. If neither is found the year is null.
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
	"sort"
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

var (
	// yearDirRe matches a folder named exactly like a year.
	yearDirRe = regexp.MustCompile(`^(?:19|20)\d{2}$`)
	// yearFileRe matches a year followed by a month and day in an image file
	// name (without extension), e.g. "2021_0909", "2023-12-26" or
	// "autosave_2023_1109_...". Plain IDs such as "118903695_p1" don't match.
	yearFileRe = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})[_-]?(?:0[1-9]|1[0-2])[_-]?(?:0[1-9]|[12]\d|3[01])(?:[^0-9]|$)`)
)

// yearFromImagePath returns the year found in an image path (slash separated,
// as stored in the hash JSON files), or nil if there is none. A folder named
// exactly like a year wins (the deepest one if there are several); failing
// that, a date in the file name is used.
func yearFromImagePath(p string) *int {
	parts := strings.Split(p, "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if yearDirRe.MatchString(parts[i]) {
			y, _ := strconv.Atoi(parts[i])
			return &y
		}
	}
	base := parts[len(parts)-1]
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if m := yearFileRe.FindStringSubmatch(base); m != nil {
		y, _ := strconv.Atoi(m[1])
		return &y
	}
	return nil
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
		files++
		// Sorted keys keep the result deterministic if a hash repeats within a file.
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e := data[k]
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
			merged[e.XXH] = info{Author: author, Year: yearFromImagePath(k)}
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