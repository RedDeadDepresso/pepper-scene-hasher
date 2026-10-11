# pepper-scene-hasher

Tools for building a hash index of scene PNGs (cards with embedded data after the PNG `IEND` chunk). It has two steps:

1. **`pnghash`** (root `main.go`) hashes every PNG and writes one JSON file per top-level folder into `data/`.
2. **`mergehash`** (`cmd/mergehash`) merges all those JSON files into a single index, `pepper-scene-index.json`, which is published to GitHub Pages by a workflow.

## Requirements

- Go 1.22+

## 1. pnghash

Scans every top-level subfolder of an input directory, hashes all PNG files found recursively, and writes `<subfolder>.json` for each one.

```sh
go run . -input /path/to/scenes
```

| Flag | Default | Description |
| --- | --- | --- |
| `-input` | *(required)* | Folder whose top-level subfolders are scanned |
| `-output` | `<program folder>/data/<input folder name>` | Folder where the `<subfolder>.json` files are written |
| `-names` | `<program folder>/config/names.json` | Names file (see below) |
| `-workers` | `min(32, 2 × CPUs)` | Number of parallel hashing workers |

Note: with `go run` the "program folder" is a temporary directory, so pass `-output` and `-names` explicitly, or build a binary first (`go build -o pnghash .`) and run it from the repo folder.

### Hash

The hash is the XXH3-128 hex digest of the data after the PNG `IEND` chunk (the embedded card data), or of the whole file if there is none. It matches the Python `xxhash.xxh3_128_hexdigest` output.

### Per-folder JSON

```json
{
  "relative/path.png": { "xxh": "<32 hex chars>", "mtime": 1754000000000000000, "size": 123456 }
}
```

An existing JSON file is used as a cache: entries whose `mtime` and `size` still match are reused, everything else is re-hashed. Entries for files that no longer exist are kept. Files are written atomically.

### Names file

`config/names.json` maps a JSON file name (without `.json`) to the author name shown in the index. After each run, pnghash adds any new file names to it and fills empty values with the first non-numeric subfolder found in that file's paths. Existing non-empty values are never changed, so you can edit them by hand.

```json
{
  "AmiRX_2024-05-04": "amirx",
  "Bazett_2026": ""
}
```

## 2. mergehash

Recursively merges the per-folder JSON files in a folder into one file mapping each hash to its author and year.

```sh
go run ./cmd/mergehash -input data -output merged.json -names config/names.json
```

| Flag | Default | Description |
| --- | --- | --- |
| `-input` | *(required)* | Folder containing the JSON files, searched recursively |
| `-output` | `merged.json` | Path of the merged JSON file |
| `-names` | *(none)* | Optional names file, as above |

### Output format

```json
{
  "0001eeaad4e62808190fb06dc3484e4b": { "author": "Erena", "year": 2026 },
  "f08442e920f9dc144efc495426c5ac8b": { "author": "momosu", "year": null }
}
```

- **`author`**: the value from the names file. If the file name is missing from it, or mapped to an empty string, the plain file name (without `.json`) is used.
- **`year`**: taken from the image path (the key inside the JSON file), not from the JSON file's name. A folder named exactly like a year (1900-2099) wins, and if there are several the deepest one is used, e.g. `Artist/2023/Series/img.png` gives `2023`. Otherwise a date in the image file name is used, e.g. `2021_0909_1200.png` or `autosave_2023-12-26.png`. If neither is found, `year` is `null`, e.g. `118903695_p1.png`.
- If the same hash appears in several files, the first file in sorted path order wins and a warning is printed to stderr.

The output file and the names file are skipped if they live inside the input folder.

## Repository layout

```
main.go                      pnghash
cmd/mergehash/main.go        mergehash
config/names.json            file name -> author
data/                        per-folder hash JSON files
.github/workflows/           merge-hashes.yml
```

## Deployment

`.github/workflows/merge-hashes.yml` runs on every push to `main` that changes `data/**/*.json`, `config/names.json` or `cmd/mergehash/**` (it can also be run manually). It runs mergehash on `data/` with `config/names.json` and deploys the result to GitHub Pages as `pepper-scene-index.json`.

## License

GPL-3.0. See [LICENSE](LICENSE).