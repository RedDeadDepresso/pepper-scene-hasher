// mergehash merges the per-folder JSON files produced by pnghash into a single
// JSON file mapping xxhash -> name of the JSON file (without ".json").
//
// With -names, a JSON file mapping that name -> string is used to replace the
// value. Names missing from it fall back to the plain file name.
//
// If the same hash appears in several files, the first file in sorted name
// order wins and a warning is printed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type entry struct {
	XXH string `json:"xxh"`
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
	input := flag.String("input", "", "folder containing the JSON files to merge (required)")
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

	dirEntries, err := os.ReadDir(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot read input folder:", err)
		os.Exit(1)
	}
	sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name() < dirEntries[j].Name() })

	merged := map[string]string{}
	owner := map[string]string{} // hash -> file name that claimed it first
	files := 0
	for _, de := range dirEntries {
		if de.IsDir() || !strings.EqualFold(filepath.Ext(de.Name()), ".json") {
			continue
		}
		full := filepath.Join(*input, de.Name())
		// Don't merge our own output or the names file if they live in the input folder.
		if same(full, *output) || (*namesPath != "" && same(full, *namesPath)) {
			continue
		}
		name := strings.TrimSuffix(de.Name(), filepath.Ext(de.Name()))
		var data map[string]entry
		if err := readJSON(full, &data); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", de.Name(), err)
			os.Exit(1)
		}
		value := name
		if v, ok := names[name]; ok {
			value = v
		}
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

	b, err := json.Marshal(merged)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if dir := filepath.Dir(*output); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(*output, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("Merged %d file(s), %d unique hash(es) -> %s\n", files, len(merged), *output)
}