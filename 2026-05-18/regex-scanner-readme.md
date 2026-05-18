# regex-scanner

Single-file Go CLI that scans a file or directory against one or more regular
expressions (regexp2 dialect) and reports every match with file path, line,
column, matched text, and which source pattern produced the match.

## Setup

```bash
mkdir regex-scanner && cd regex-scanner
# place regex-scanner.go as main.go and regex-scanner-go.mod as go.mod
go mod tidy
go build -o regex-scanner .
```

## Usage

```bash
# Single pattern, single file
./regex-scanner -pattern '"gp2"' ./main.tf

# Multiple patterns, recursive directory scan
./regex-scanner \
  -pattern '\btype\s*=\s*"gp2"' \
  -pattern '\bvolume_type\s*=\s*"gp2"' \
  -pattern '\bdefault\s*=\s*"gp2"' \
  ./infra

# Patterns from a file (one per line, blank lines and # comments ignored)
./regex-scanner -patterns-file patterns.txt ./infra

# Restrict to certain extensions, JSON output (NDJSON)
./regex-scanner -patterns-file patterns.txt -include '*.tf,*.tfvars' -json ./infra
```

## Output format

Default human-readable:

```
testdata/main.tf:2:3   [\btype\s*=\s*"gp2"]         type = "gp2"
testdata/main.tf:8:5   [\bvolume_type\s*=\s*"gp2"]  volume_type = "gp2"
testdata/main.tf:13:3  [\bdefault\s*=\s*"gp2"]      default = "gp2"
```

`-json` emits one JSON object per match.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-pattern` | (none) | Regex pattern; repeatable |
| `-patterns-file` | (none) | File with one regex per line, `#` comments allowed |
| `-include` | (none) | Comma-separated globs to include (e.g. `*.tf,*.tfvars`) |
| `-exclude` | (none) | Comma-separated globs to exclude |
| `-max-size` | 10 MiB | Skip files larger than this; 0 disables |
| `-match-timeout` | 2s | Per-regex match timeout (catches catastrophic backtracking) |
| `-workers` | NumCPU | Concurrent file scanners |
| `-json` | false | Emit NDJSON instead of human format |
| `-include-binary` | false | Scan files that look binary |
| `-no-skip-defaults` | false | Don't auto-skip `.git`, `node_modules`, `.terraform`, etc. |
| `-i` | false | Case-insensitive matching |
| `-multiline` | false | `^` and `$` match line boundaries |

## Design notes

- Uses `regexp2.FindStringMatch` + `FindNextMatch` (no `FindAllString` in regexp2).
- `MatchTimeout` per-pattern guards against catastrophic backtracking — important
  with regexp2 since it's NFA-based and lacks RE2's linear-time guarantee.
- Worker pool over file list; scanning is mostly IO-bound but lots of small
  files benefit from parallelism.
- Binary detection by NUL-byte sniff in first 8 KiB (cheap heuristic).
- `bufio.Scanner` buffer raised to 1 MiB max for long-line JSON / minified files.
- Per-file and per-pattern errors print to stderr and continue rather than aborting
  the whole scan; compile errors fail fast.

## Tradeoff: regexp2 vs stdlib regexp

regexp2 gets you lookarounds, backreferences, and named groups — at the cost of
RE2's linear-time guarantee. If your patterns don't need those features, swapping
to `regexp` from the stdlib gets you protection against runaway regexes for free.
The structure of this program would be nearly identical either way.
