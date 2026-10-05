// pnghash scans every top-level subfolder of an input directory, hashes all PNG
// files found recursively, and writes one JSON file per subfolder:
//
//	{ "relative/path.png": {"xxh": "<32 hex>", "mtime": <unix ns>, "size": <bytes>}, ... }
//
// The hash matches the Python tool: XXH3-128 (hex digest) of the data after the
// PNG IEND chunk (the embedded card data), or of the whole file if there is none.
// An existing JSON file is used as a cache: entries whose mtime and size still
// match are reused, everything else is re-hashed.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/zeebo/xxh3"
)

type Entry struct {
	XXH   string `json:"xxh"`
	MTime int64  `json:"mtime"` // Unix time in nanoseconds
	Size  int64  `json:"size"`
}

var (
	pngSig = []byte("\x89PNG\r\n\x1a\n")
	iend   = []byte("IEND")
)

// pngPayload returns the bytes after the IEND chunk, or nil if there are none.
func pngPayload(data []byte) []byte {
	if !bytes.HasPrefix(data, pngSig) {
		return nil
	}
	pos := 8
	for pos+12 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		chunkType := data[pos+4 : pos+8]
		pos += 12 + length
		if bytes.Equal(chunkType, iend) {
			if pos < len(data) {
				return data[pos:]
			}
			return nil
		}
	}
	return nil
}

// hashFile mirrors the Python logic: XXH3-128 of the payload, else the whole file.
// The hex format equals xxhash.xxh3_128_hexdigest (canonical: high64 then low64).
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if p := pngPayload(data); p != nil {
		data = p
	}
	h := xxh3.Hash128(data)
	return fmt.Sprintf("%016x%016x", h.Hi, h.Lo), nil
}

func loadCache(path string) map[string]Entry {
	cache := map[string]Entry{}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "  warning: cannot read %s: %v\n", path, err)
		}
		return cache
	}
	if err := json.Unmarshal(b, &cache); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: ignoring unreadable cache %s: %v\n", path, err)
		return map[string]Entry{}
	}
	return cache
}

// saveJSON writes atomically (temp file + rename) so an interruption can't
// leave a corrupt cache behind.
func saveJSON(path string, v any) error {
	// Map keys are sorted -> stable output. HTML escaping is off so "&" stays "&".
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil { // Encode appends a trailing newline
		return err
	}
	b := buf.Bytes()
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

type job struct {
	rel, abs string
	mtime    int64
	size     int64
}

func processFolder(folder, jsonPath string, workers int) error {
	cache := loadCache(jsonPath)

	// Collect PNGs recursively.
	var jobs []job
	err := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".png") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(folder, p)
		if err != nil {
			return nil
		}
		jobs = append(jobs, job{
			rel:   filepath.ToSlash(rel),
			abs:   p,
			mtime: info.ModTime().UnixNano(),
			size:  info.Size(),
		})
		return nil
	})
	if err != nil {
		return err
	}

	// Start from the existing cache so entries for files that no longer
	// exist are kept; entries for files found now are overwritten below.
	result := make(map[string]Entry, len(cache)+len(jobs))
	for k, v := range cache {
		result[k] = v
	}
	var (
		mu                sync.Mutex
		reused, refreshed int
		failed            int
		wg                sync.WaitGroup
	)
	ch := make(chan job)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if c, ok := cache[j.rel]; ok && c.XXH != "" && c.MTime == j.mtime && c.Size == j.size {
					mu.Lock()
					result[j.rel] = c
					reused++
					mu.Unlock()
					continue
				}
				digest, err := hashFile(j.abs)
				mu.Lock()
				if err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "  error: %s: %v\n", j.rel, err)
				} else {
					result[j.rel] = Entry{XXH: digest, MTime: j.mtime, Size: j.size}
					refreshed++
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()

	if err := saveJSON(jsonPath, result); err != nil {
		return fmt.Errorf("saving %s: %w", jsonPath, err)
	}
	fmt.Printf("  %d PNG(s): %d cached, %d hashed, %d failed -> %s\n",
		len(jobs), reused, refreshed, failed, jsonPath)
	return nil
}

func main() {
	exe, err := os.Executable()
	progDir := "."
	if err == nil {
		progDir = filepath.Dir(exe)
	}
	input := flag.String("input", "", "folder whose top-level subfolders are scanned (required)")
	output := flag.String("output", "", "folder where the <subfolder>.json files are written (default: <program folder>/data/<input folder name>)")
	namesPath := flag.String("names", filepath.Join(progDir, "config", "names.json"),
		"names JSON file (json file name without .json -> value); missing names are added and empty values filled in")
	workers := flag.Int("workers", min(32, runtime.NumCPU()*2), "number of parallel hashing workers")
	flag.Parse()

	if *input == "" {
		fmt.Fprintln(os.Stderr, "error: -input is required")
		flag.Usage()
		os.Exit(2)
	}
	if *output == "" {
		abs, err := filepath.Abs(*input)
		if err != nil {
			abs = *input
		}
		name := filepath.Base(abs)
		if name == "." || name == string(filepath.Separator) {
			name = "root"
		}
		*output = filepath.Join(progDir, "data", name)
	}

	if err := os.MkdirAll(*output, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create output folder:", err)
		os.Exit(1)
	}
	entries, err := os.ReadDir(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read input folder:", err)
		os.Exit(1)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	fmt.Printf("Input : %s\nOutput: %s\n", *input, *output)
	exit := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fmt.Println(e.Name())
		err := processFolder(
			filepath.Join(*input, e.Name()),
			filepath.Join(*output, e.Name()+".json"),
			*workers,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error: %v\n", err)
			exit = 1
		}
	}

	if err := updateNames(*output, *namesPath); err != nil {
		fmt.Fprintf(os.Stderr, "error updating names file: %v\n", err)
		exit = 1
	}
	os.Exit(exit)
}

// isNumeric reports whether s is non-empty and made only of digits ("2024").
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// firstNamedFolder returns the first folder in a relative path that is not
// purely numeric and does not match skip (compared case-insensitively; pass the
// JSON file name without extension): "Artist/sub/img.png" -> "Artist",
// "2024/Artist/img.png" -> "Artist". It returns "" if the path has no such
// folder (the last element is the file name and is never considered).
func firstNamedFolder(p, skip string) string {
	parts := strings.Split(p, "/")
	for _, d := range parts[:len(parts)-1] {
		if d != "" && !isNumeric(d) && !strings.EqualFold(d, skip) {
			return d
		}
	}
	return ""
}

// firstSubfolder reads a hash JSON file and returns the first folder of its
// relative paths that is neither numeric nor equal to skip (the JSON file name
// without extension; see firstNamedFolder). Paths are checked in
// sorted order and the first one that has such a folder wins; multiple reports
// whether other paths resolve to a different folder. Files directly in the root,
// or only inside numeric folders, are ignored.
func firstSubfolder(jsonPath, skip string) (sub string, multiple bool, err error) {
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", false, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return "", false, err
	}
	paths := make([]string, 0, len(data))
	for p := range data {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		first := firstNamedFolder(p, skip)
		if first == "" {
			continue
		}
		if sub == "" {
			sub = first
		} else if first != sub {
			multiple = true
		}
	}
	return sub, multiple, nil
}

// updateNames adds every JSON file name (without ".json") found in outputDir
// to the names file, and fills each empty value with the first subfolder found
// in that JSON file's paths. Existing non-empty values are never changed.
func updateNames(outputDir, namesPath string) error {
	names := map[string]string{}
	b, err := os.ReadFile(namesPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &names); err != nil {
			// Don't risk overwriting a file we couldn't understand.
			return fmt.Errorf("cannot parse %s: %w", namesPath, err)
		}
	case !os.IsNotExist(err):
		return err
	}

	added, set := 0, 0
	err = filepath.WalkDir(outputDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".json") {
			return nil
		}
		if isSamePath(p, namesPath) {
			return nil
		}
		name := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
		if _, ok := names[name]; !ok {
			names[name] = ""
			added++
		}
		if names[name] != "" {
			return nil
		}
		sub, multiple, err := firstSubfolder(p, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", p, err)
			return nil
		}
		if sub == "" {
			return nil
		}
		names[name] = sub
		set++
		fmt.Printf("  %s -> %q\n", name, sub)
		if multiple {
			fmt.Fprintf(os.Stderr, "warning: %s has paths in several folders; used %q\n", name, sub)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if added == 0 && set == 0 {
		if _, statErr := os.Stat(namesPath); statErr == nil {
			fmt.Printf("Names file %s is up to date\n", namesPath)
			return nil
		}
		if len(names) == 0 {
			return nil // nothing to write and no file to create
		}
	}
	if err := os.MkdirAll(filepath.Dir(namesPath), 0o755); err != nil {
		return err
	}
	if err := saveJSON(namesPath, names); err != nil {
		return err
	}
	fmt.Printf("Names file %s: added %d name(s), set %d value(s)\n", namesPath, added, set)
	return nil
}

func isSamePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}