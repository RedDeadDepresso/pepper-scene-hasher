// matchnames fills the empty values of names.json with the closest author name
// from authors.json.
//
// Each key of names.json (a JSON file name without ".json") is cleaned up (a
// trailing year such as "_2026" is dropped, and "--" or "&" split it into
// several names) and every part is compared with each author's "name" and
// "alias" entries. A long number in the key (such as a pixiv user id) that
// appears in an author's links also counts as a match. If exactly one author
// reaches -threshold, that author is written as the value, using the English
// (ASCII-letter) version of the name when there is one: the canonical name if it
// is English, else the alias that matched, else the first English alias. If
// several different authors match, the key is left empty for you to decide.
// Values that are already non-empty are never changed.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type author struct {
	Name  string   `json:"name"`
	Alias []string `json:"alias"`
	Links []string `json:"links"`
}

type candidate struct {
	author string // canonical name
	via    string // the name or alias that matched
	score  float64
}

// normalize lowercases and keeps only letters and digits, so that
// "Sam Cos Myson", "SamCosMyson" and "sam_cos_myson" compare equal.
func normalize(s string) []rune {
	var out []rune
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, r)
		}
	}
	return out
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// similarity returns a score in [0,1]: 1 means identical after normalisation.
func similarity(a, b []rune) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	longer, shorter := max(len(a), len(b)), min(len(a), len(b))
	score := 1 - float64(levenshtein(a, b))/float64(longer)
	// One contains the other ("alucid" in "alucid_pack"): strong signal,
	// scaled by how much of the longer string the shorter one covers.
	if shorter >= 3 && (strings.Contains(string(a), string(b)) || strings.Contains(string(b), string(a))) {
		score = max(score, 0.7+0.3*float64(shorter)/float64(longer))
	}
	return score
}

// isEnglish reports whether s contains at least one ASCII letter or digit and
// no letters outside ASCII (so "Kirikumori" and "Dr.Charlie" qualify, while
// "きりくもり / 霧久", "白5B" and "ᴺᵃᶤᵐᵘ-Micco" do not).
func isEnglish(s string) bool {
	ascii := false
	for _, r := range s {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			ascii = true
		case r >= 128 && unicode.IsLetter(r):
			return false
		}
	}
	return ascii
}

// englishName picks the value to save for a matched author. It returns the
// chosen name and whether it is an English one. Preference order: the canonical
// name, the alias that matched, then the first English alias. If the author has
// no English name at all, the canonical name is returned with ok=false.
func englishName(a author, via string) (name string, ok bool) {
	if isEnglish(a.Name) {
		return a.Name, true
	}
	for _, al := range a.Alias {
		if al == via && isEnglish(al) {
			return strings.TrimSpace(al), true
		}
	}
	for _, al := range a.Alias {
		if isEnglish(al) {
			return strings.TrimSpace(al), true
		}
	}
	return a.Name, false
}

func loadAuthors(path string) ([]author, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Authors []author `json:"authors"`
	}
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Authors != nil {
		return wrapped.Authors, nil
	}
	var list []author // also accept a bare top-level array
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("unrecognised authors format: %w", err)
	}
	return list, nil
}

var (
	yearSuffix = regexp.MustCompile(`[\s_-]*(19|20)\d{2}$`)
	longNumber = regexp.MustCompile(`\d{6,}`)
	separators = strings.NewReplacer("--", "\x00", "&", "\x00", "＆", "\x00")
)

// splitKey turns "BoiMeows--BoiTheMews_2026" into ["BoiMeows", "BoiTheMews"]:
// the trailing year is dropped and "--" / "&" separate several names.
func splitKey(key string) []string {
	key = yearSuffix.ReplaceAllString(key, "")
	var parts []string
	for _, p := range strings.Split(separators.Replace(key), "\x00") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// hasNumber reports whether link contains num as a whole number
// (e.g. "57786118" in "https://www.pixiv.net/users/57786118").
func hasNumber(link, num string) bool {
	for i := 0; ; {
		j := strings.Index(link[i:], num)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(num)
		before := start == 0 || link[start-1] < '0' || link[start-1] > '9'
		after := end == len(link) || link[end] < '0' || link[end] > '9'
		if before && after {
			return true
		}
		i = start + 1
	}
}

// matchPart finds the best author for one name part, comparing against every
// author name and alias. A long number in the part (e.g. a pixiv user id) that
// appears in an author's links is treated as an exact match.
func matchPart(part string, authors []author, norm map[string][]rune) candidate {
	k := normalize(part)
	nums := longNumber.FindAllString(part, -1)
	var top candidate
	consider := func(a author, via string, score float64) {
		if score > top.score {
			top = candidate{author: a.Name, via: via, score: score}
		}
	}
	for _, a := range authors {
		consider(a, a.Name, similarity(k, norm[a.Name]))
		for _, al := range a.Alias {
			consider(a, al, similarity(k, norm[al]))
		}
		for _, link := range a.Links {
			for _, n := range nums {
				if hasNumber(link, n) {
					consider(a, "link "+link, 1)
				}
			}
		}
	}
	return top
}

// matchKey matches every part of a key. It returns the distinct authors that
// reach the threshold (best score each) and the overall best guess.
func matchKey(key string, threshold float64, authors []author, norm map[string][]rune) (hits []candidate, guess candidate) {
	seen := map[string]int{}
	for _, part := range splitKey(key) {
		c := matchPart(part, authors, norm)
		if c.score > guess.score {
			guess = c
		}
		if c.score < threshold {
			continue
		}
		if i, ok := seen[c.author]; ok {
			if c.score > hits[i].score {
				hits[i] = c
			}
			continue
		}
		seen[c.author] = len(hits)
		hits = append(hits, c)
	}
	return hits, guess
}

func writeJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "&" as "&" instead of \u0026
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil { // Encode appends a trailing newline
		return err
	}
	b := buf.Bytes()
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func main() {
	progDir := "."
	if exe, err := os.Executable(); err == nil {
		progDir = filepath.Dir(exe)
	}
	authorsPath := flag.String("authors", filepath.Join(progDir, "config", "authors.json"), "authors JSON file")
	namesPath := flag.String("names", filepath.Join(progDir, "config", "names.json"), "names JSON file to fill in")
	threshold := flag.Float64("threshold", 0.8, "minimum similarity (0-1) required to assign a match")
	dryRun := flag.Bool("dry-run", false, "print the matches but don't modify the names file")
	flag.Parse()

	authors, err := loadAuthors(*authorsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: authors:", err)
		os.Exit(1)
	}
	var names map[string]string
	b, err := os.ReadFile(*namesPath)
	if err == nil {
		err = json.Unmarshal(b, &names)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: names:", err)
		os.Exit(1)
	}

	// Pre-normalise every author name/alias once.
	norm := map[string][]rune{}
	for _, a := range authors {
		norm[a.Name] = normalize(a.Name)
		for _, al := range a.Alias {
			norm[al] = normalize(al)
		}
	}

	byName := map[string]author{}
	for _, a := range authors {
		byName[a.Name] = a
	}

	keys := make([]string, 0, len(names))
	for k, v := range names {
		if v == "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	assigned, unmatched := 0, 0
	via := func(c candidate) string {
		if c.via == c.author {
			return ""
		}
		return fmt.Sprintf(" (via %q)", c.via)
	}
	for _, k := range keys {
		hits, guess := matchKey(k, *threshold, authors, norm)
		switch {
		case len(hits) == 1:
			c := hits[0]
			value, english := englishName(byName[c.author], c.via)
			names[k] = value
			assigned++
			note := ""
			switch {
			case !english:
				note = fmt.Sprintf(" (no English name; author %q)", c.author)
			case value != c.author:
				note = fmt.Sprintf(" (author %q)", c.author)
			}
			fmt.Printf("OK   %-34s -> %q  [%.2f]%s\n", k, value, c.score, note)
		case len(hits) > 1:
			unmatched++
			var list []string
			for _, c := range hits {
				list = append(list, fmt.Sprintf("%q [%.2f]", c.author, c.score))
			}
			fmt.Printf("??   %-34s    several authors, left empty: %s\n", k, strings.Join(list, ", "))
		case guess.author != "":
			unmatched++
			fmt.Printf("??   %-34s    best guess %q  [%.2f]%s\n", k, guess.author, guess.score, via(guess))
		default:
			unmatched++
			fmt.Printf("??   %-34s    no candidate\n", k)
		}
	}
	fmt.Printf("\n%d empty value(s): %d assigned, %d left empty (threshold %.2f)\n",
		len(keys), assigned, unmatched, *threshold)

	if *dryRun || assigned == 0 {
		if *dryRun {
			fmt.Println("dry run: names file not modified")
		}
		return
	}
	if err := writeJSON(*namesPath, names); err != nil {
		fmt.Fprintln(os.Stderr, "error: writing names:", err)
		os.Exit(1)
	}
	fmt.Println("updated", *namesPath)
}