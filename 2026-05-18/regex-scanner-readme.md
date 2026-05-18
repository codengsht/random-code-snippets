# regex-scanner

Single-file Go CLI that scans a file or directory against one or more regular
expressions (regexp2 dialect) and reports every match. Three output modes plus
an optional self-contained HTML report.

## Setup

```bash
mkdir regex-scanner && cd regex-scanner
# place regex-scanner.go as main.go and regex-scanner-go.mod as go.mod
go mod tidy
go build -o regex-scanner .
```

## Output modes

| Mode | Flag | What you get |
|---|---|---|
| Default | (none) | `file:line:col [pattern] match` lines on stdout |
| Table | `-table` | ASCII table per match + pattern hit summary + footer |
| JSON | `-json` | NDJSON, one match per line |
| HTML | `-html FILE` | Composes with any of the above; writes a self-contained HTML report to FILE |

`-json` and `-table` are mutually exclusive. `-html` composes with anything.

## Usage

```bash
# Default: pipeline-friendly output
./regex-scanner -pattern '"gp2"' ./main.tf

# Human-readable table on terminal
./regex-scanner -table \
  -pattern '\btype\s*=\s*"gp2"' \
  -pattern '\bvolume_type\s*=\s*"gp2"' \
  -pattern '\bdefault\s*=\s*"gp2"' \
  ./infra

# Patterns from file (one per line, # comments allowed)
./regex-scanner -patterns-file patterns.txt ./infra

# Restrict by glob, NDJSON output
./regex-scanner -patterns-file patterns.txt -include '*.tf,*.tfvars' -json ./infra

# Table on stdout AND a self-contained HTML report on disk
./regex-scanner -table -html report.html -patterns-file patterns.txt ./infra
open report.html   # macOS
```

## Sample table output

```
+------------------+----------+---------------------------+---------------------+
| File             | Line:Col | Pattern                   | Match               |
+------------------+----------+---------------------------+---------------------+
| testdata/main.tf | 2:3      | \btype\s*=\s*"gp2"        | type = "gp2"        |
| testdata/main.tf | 8:5      | \bvolume_type\s*=\s*"gp2" | volume_type = "gp2" |
| testdata/main.tf | 13:3     | \bdefault\s*=\s*"gp2"     | default = "gp2"     |
+------------------+----------+---------------------------+---------------------+

Summary:
+---------------------------+------+
| Pattern                   | Hits |
+---------------------------+------+
| \bdefault\s*=\s*"gp2"     | 1    |
| \btype\s*=\s*"gp2"        | 1    |
| \bvolume_type\s*=\s*"gp2" | 1    |
+---------------------------+------+

3 match(es) across 1 file(s)
```

## HTML report

Single self-contained file (no external CSS, JS, or fonts). Includes:

- Header with target path and timestamp
- Stat cards: total matches / files / patterns
- Pattern summary table sorted by hit count
- All-matches table with a live client-side substring filter ("X / Y shown" counter)
- Per-file sections with their own match tables
- Light/dark mode via `prefers-color-scheme`

Safe to open from disk, attach to a ticket, or share as a single asset.

## All flags

| Flag | Default | Description |
|---|---|---|
| `-pattern` | (none) | Regex pattern; repeatable |
| `-patterns-file` | (none) | File with one regex per line, `#` comments allowed |
| `-include` | (none) | Comma-separated globs to include (e.g. `*.tf,*.tfvars`) |
| `-exclude` | (none) | Comma-separated globs to exclude |
| `-max-size` | 10 MiB | Skip files larger than this; 0 disables |
| `-match-timeout` | 2s | Per-regex match timeout (catches catastrophic backtracking) |
| `-workers` | NumCPU | Concurrent file scanners |
| `-json` | false | Emit NDJSON |
| `-table` | false | Emit table + summary |
| `-html` | "" | Write self-contained HTML report to this path |
| `-include-binary` | false | Scan files that look binary |
| `-no-skip-defaults` | false | Don't auto-skip `.git`, `node_modules`, `.terraform`, etc. |
| `-i` | false | Case-insensitive matching |
| `-multiline` | false | `^` and `$` match line boundaries |

## Design notes

- **regexp2 specifics**: uses `FindStringMatch` + `FindNextMatch` for non-overlapping
  iteration. `MatchTimeout` per-pattern guards against catastrophic backtracking
  (regexp2 is NFA-based and lacks RE2's linear-time guarantee).
- **Concurrency**: worker pool over file list (`-workers`, default NumCPU).
- **Binary detection**: NUL-byte sniff in first 8 KiB.
- **Long lines**: `bufio.Scanner` buffer raised to 1 MiB max for JSON / minified files.
- **Default skip dirs**: `.git`, `.terraform`, `.terragrunt-cache`, `node_modules`,
  `vendor`, `dist`, `build`, etc. Disable with `-no-skip-defaults`.
- **Atomic HTML write**: renders to temp file in target dir, then `os.Rename`.
  Crash mid-write won't leave a half-written report.
- **Auto-escaping**: HTML report uses `html/template`, so file/pattern/match content
  is safe even if files contain `<script>` or other HTML-looking text.
- **Error handling**: per-file and per-pattern errors print to stderr and continue;
  compile errors fail fast.

## regexp2 vs stdlib regexp

regexp2 gets you lookarounds, backreferences, and named groups — at the cost of
RE2's linear-time guarantee. If your patterns don't need those features, swapping
to stdlib `regexp` gets you protection against runaway regexes for free. Program
structure would be nearly identical either way.
