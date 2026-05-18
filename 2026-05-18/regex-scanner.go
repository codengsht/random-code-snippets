// regex-scanner is a small CLI that scans a file or directory tree against
// one or more regular expressions (regexp2 dialect) and reports every match
// with file path, line, column, the matched text, and which pattern caught it.
//
// Dependency: github.com/dlclark/regexp2 v1.11.5
//
// Output modes:
//   - default  : "file:line:col [pattern] match" lines on stdout
//   - -table   : ASCII table with summary on stdout
//   - -json    : NDJSON, one JSON object per match on stdout
//   - -html F  : composes with any of the above; writes self-contained HTML report to F
//
// Usage examples:
//
//	# Single pattern, single file
//	regex-scanner -pattern '"gp2"' ./main.tf
//
//	# Multiple patterns, recursive directory scan, table output
//	regex-scanner -table \
//	              -pattern 'volume_type\s*=\s*"gp2"' \
//	              -pattern 'storage_type\s*=\s*"gp2"' ./infra
//
//	# Patterns from a file plus an HTML report
//	regex-scanner -table -html report.html -patterns-file patterns.txt ./infra
//
//	# Restrict to certain extensions, JSON output
//	regex-scanner -patterns-file patterns.txt -include '*.tf,*.tfvars' -json ./infra
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// Default directories that almost never contain useful matches and tend to
// blow up scan time. Override with -no-skip-defaults if needed.
var defaultSkipDirs = map[string]struct{}{
	".git":              {},
	".svn":              {},
	".hg":               {},
	".terraform":        {},
	".terragrunt-cache": {},
	"node_modules":      {},
	"vendor":            {},
	"dist":              {},
	"build":             {},
	".idea":             {},
	".vscode":           {},
}

// stringSliceFlag lets a flag be specified multiple times on the command line.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ", ") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

// compiledPattern keeps the original pattern text alongside the compiled regex
// so we can report which source pattern produced a match.
type compiledPattern struct {
	source string
	re     *regexp2.Regexp
}

// Match describes a single regex hit in a file.
type Match struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Pattern string `json:"pattern"`
	Text    string `json:"match"`
}

type config struct {
	patterns        []string
	patternsFile    string
	includeGlobs    []string
	excludeGlobs    []string
	maxFileSize     int64
	matchTimeout    time.Duration
	workers         int
	jsonOutput      bool
	tableOutput     bool
	htmlPath        string
	includeBinary   bool
	noSkipDefaults  bool
	caseInsensitive bool
	multiline       bool
	target          string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	patternSources, err := collectPatternSources(cfg)
	if err != nil {
		return err
	}
	if len(patternSources) == 0 {
		return errors.New("no patterns provided; use -pattern or -patterns-file")
	}

	patterns, err := compilePatterns(patternSources, cfg)
	if err != nil {
		return err
	}

	files, err := collectFiles(cfg)
	if err != nil {
		return err
	}

	if cfg.jsonOutput && cfg.tableOutput {
		return errors.New("-json and -table are mutually exclusive")
	}

	matches := scanFiles(files, patterns, cfg)

	if cfg.htmlPath != "" {
		if err := writeHTMLReport(cfg.htmlPath, cfg.target, matches); err != nil {
			return fmt.Errorf("writing HTML report: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote HTML report to %s\n", cfg.htmlPath)
	}

	return reportMatches(matches, cfg)
}

func parseFlags() (*config, error) {
	cfg := &config{}
	var patterns stringSliceFlag
	var includes, excludes stringSliceFlag

	flag.Var(&patterns, "pattern", "regex pattern (regexp2 dialect); may be repeated")
	flag.StringVar(&cfg.patternsFile, "patterns-file", "", "file containing one regex per line (# comments allowed)")
	flag.Var(&includes, "include", "comma-separated glob(s) to include (e.g. '*.tf,*.tfvars'); may be repeated")
	flag.Var(&excludes, "exclude", "comma-separated glob(s) to exclude; may be repeated")
	flag.Int64Var(&cfg.maxFileSize, "max-size", 10*1024*1024, "skip files larger than this many bytes (0 disables)")
	flag.DurationVar(&cfg.matchTimeout, "match-timeout", 2*time.Second, "max time per regex match attempt")
	flag.IntVar(&cfg.workers, "workers", runtime.NumCPU(), "number of concurrent file scanners")
	flag.BoolVar(&cfg.jsonOutput, "json", false, "emit one JSON object per match (NDJSON)")
	flag.BoolVar(&cfg.tableOutput, "table", false, "render results as a human-readable table with summary")
	flag.StringVar(&cfg.htmlPath, "html", "", "also write a self-contained HTML report to this path")
	flag.BoolVar(&cfg.includeBinary, "include-binary", false, "scan files that look binary (default skips them)")
	flag.BoolVar(&cfg.noSkipDefaults, "no-skip-defaults", false, "do not skip default ignore dirs (.git, node_modules, etc.)")
	flag.BoolVar(&cfg.caseInsensitive, "i", false, "case-insensitive matching")
	flag.BoolVar(&cfg.multiline, "multiline", false, "enable multiline mode (^ and $ match line boundaries)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <file-or-directory>\n\nFlags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		return nil, errors.New("exactly one target path is required")
	}

	cfg.patterns = patterns
	cfg.includeGlobs = splitCSV(includes)
	cfg.excludeGlobs = splitCSV(excludes)
	cfg.target = flag.Arg(0)
	if cfg.workers < 1 {
		cfg.workers = 1
	}
	return cfg, nil
}

// splitCSV flattens repeated flags that may also contain comma-separated values.
func splitCSV(in stringSliceFlag) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func collectPatternSources(cfg *config) ([]string, error) {
	sources := append([]string{}, cfg.patterns...)
	if cfg.patternsFile == "" {
		return sources, nil
	}
	f, err := os.Open(cfg.patternsFile)
	if err != nil {
		return nil, fmt.Errorf("opening patterns file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sources = append(sources, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading patterns file (line %d): %w", lineNum, err)
	}
	return sources, nil
}

func compilePatterns(sources []string, cfg *config) ([]compiledPattern, error) {
	var opts regexp2.RegexOptions
	if cfg.caseInsensitive {
		opts |= regexp2.IgnoreCase
	}
	if cfg.multiline {
		opts |= regexp2.Multiline
	}

	out := make([]compiledPattern, 0, len(sources))
	for i, src := range sources {
		re, err := regexp2.Compile(src, opts)
		if err != nil {
			return nil, fmt.Errorf("compiling pattern #%d (%q): %w", i+1, src, err)
		}
		re.MatchTimeout = cfg.matchTimeout
		out = append(out, compiledPattern{source: src, re: re})
	}
	return out, nil
}

func collectFiles(cfg *config) ([]string, error) {
	info, err := os.Stat(cfg.target)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", cfg.target, err)
	}
	if !info.IsDir() {
		return []string{cfg.target}, nil
	}

	var files []string
	walkErr := filepath.WalkDir(cfg.target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !cfg.noSkipDefaults {
				if _, skip := defaultSkipDirs[d.Name()]; skip {
					return fs.SkipDir
				}
			}
			return nil
		}
		// Skip non-regular files (symlinks, sockets, devices).
		if !d.Type().IsRegular() {
			return nil
		}
		if !shouldInclude(path, cfg) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walking %s: %w", cfg.target, walkErr)
	}
	return files, nil
}

func shouldInclude(path string, cfg *config) bool {
	base := filepath.Base(path)
	for _, g := range cfg.excludeGlobs {
		if matched, _ := filepath.Match(g, base); matched {
			return false
		}
	}
	if len(cfg.includeGlobs) == 0 {
		return true
	}
	for _, g := range cfg.includeGlobs {
		if matched, _ := filepath.Match(g, base); matched {
			return true
		}
	}
	return false
}

func scanFiles(files []string, patterns []compiledPattern, cfg *config) []Match {
	jobs := make(chan string)
	results := make(chan []Match)

	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				m, err := scanOne(path, patterns, cfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warn: %s: %v\n", path, err)
					continue
				}
				if len(m) > 0 {
					results <- m
				}
			}
		}()
	}

	go func() {
		for _, f := range files {
			jobs <- f
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var all []Match
	for m := range results {
		all = append(all, m...)
	}
	return all
}

func scanOne(path string, patterns []compiledPattern, cfg *config) ([]Match, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if cfg.maxFileSize > 0 && info.Size() > cfg.maxFileSize {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if !cfg.includeBinary {
		binary, err := looksBinary(f)
		if err != nil {
			return nil, err
		}
		if binary {
			return nil, nil
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}

	var matches []Match
	scanner := bufio.NewScanner(f)
	// Allow lines up to 1 MiB; default 64 KiB is too small for some Terraform/JSON.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		for _, p := range patterns {
			lineMatches, err := findAll(p, line)
			if err != nil {
				// Per-pattern errors (e.g. timeout) shouldn't kill the whole file.
				fmt.Fprintf(os.Stderr, "warn: %s:%d pattern %q: %v\n", path, lineNum, p.source, err)
				continue
			}
			for _, m := range lineMatches {
				matches = append(matches, Match{
					File:    path,
					Line:    lineNum,
					Column:  m.index + 1, // 1-based for display
					Pattern: p.source,
					Text:    m.text,
				})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return matches, fmt.Errorf("reading file: %w", err)
	}
	return matches, nil
}

type rangeMatch struct {
	index int
	text  string
}

// findAll iterates all non-overlapping matches of p against s.
func findAll(p compiledPattern, s string) ([]rangeMatch, error) {
	var out []rangeMatch
	m, err := p.re.FindStringMatch(s)
	if err != nil {
		return nil, err
	}
	for m != nil {
		out = append(out, rangeMatch{index: m.Index, text: m.String()})
		m, err = p.re.FindNextMatch(m)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// looksBinary inspects the first 8 KiB for NUL bytes — a cheap and decent
// heuristic for "do I want to scan this as text".
func looksBinary(r io.Reader) (bool, error) {
	buf := make([]byte, 8*1024)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true, nil
		}
	}
	return false, nil
}

func reportMatches(matches []Match, cfg *config) error {
	if len(matches) == 0 {
		fmt.Println("no matches")
		return nil
	}

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	switch {
	case cfg.jsonOutput:
		enc := json.NewEncoder(w)
		for _, m := range matches {
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		return nil

	case cfg.tableOutput:
		return renderTable(w, matches)

	default:
		for _, m := range matches {
			fmt.Fprintf(w, "%s:%d:%d  [%s]  %s\n", m.File, m.Line, m.Column, m.Pattern, m.Text)
		}
		return nil
	}
}

// Maximum cell widths for the table to keep rows readable. Long values are
// truncated with an ellipsis. Tweak if you regularly scan paths or patterns
// longer than these.
const (
	maxFileCol    = 50
	maxPatternCol = 40
	maxMatchCol   = 60
)

// renderTable prints two ASCII tables: a per-match table grouped by file, and
// a summary table with hit counts per pattern.
func renderTable(w io.Writer, matches []Match) error {
	headers := []string{"File", "Line:Col", "Pattern", "Match"}
	rows := make([][]string, 0, len(matches))
	for _, m := range matches {
		rows = append(rows, []string{
			truncate(m.File, maxFileCol),
			fmt.Sprintf("%d:%d", m.Line, m.Column),
			truncate(m.Pattern, maxPatternCol),
			truncate(m.Text, maxMatchCol),
		})
	}
	if err := writeASCIITable(w, headers, rows); err != nil {
		return err
	}

	// Summary: hits per pattern, sorted by count desc then pattern asc for stability.
	type patternCount struct {
		pattern string
		count   int
	}
	counts := map[string]int{}
	files := map[string]struct{}{}
	for _, m := range matches {
		counts[m.Pattern]++
		files[m.File] = struct{}{}
	}
	summary := make([]patternCount, 0, len(counts))
	for p, c := range counts {
		summary = append(summary, patternCount{p, c})
	}
	sort.Slice(summary, func(i, j int) bool {
		if summary[i].count != summary[j].count {
			return summary[i].count > summary[j].count
		}
		return summary[i].pattern < summary[j].pattern
	})

	summaryRows := make([][]string, 0, len(summary))
	for _, s := range summary {
		summaryRows = append(summaryRows, []string{
			truncate(s.pattern, maxPatternCol),
			fmt.Sprintf("%d", s.count),
		})
	}
	fmt.Fprintln(w, "\nSummary:")
	if err := writeASCIITable(w, []string{"Pattern", "Hits"}, summaryRows); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%d match(es) across %d file(s)\n", len(matches), len(files))
	return nil
}

// writeASCIITable renders a basic +---+ style table. Column widths are sized
// to the widest cell (header or row) by display width (rune count).
func writeASCIITable(w io.Writer, headers []string, rows [][]string) error {
	if len(headers) == 0 {
		return errors.New("table requires at least one header")
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if i >= len(widths) {
				break
			}
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	separator := buildSeparator(widths)
	if _, err := fmt.Fprintln(w, separator); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, buildRow(headers, widths)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, separator); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintln(w, buildRow(r, widths)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, separator); err != nil {
		return err
	}
	return nil
}

func buildSeparator(widths []int) string {
	var b strings.Builder
	b.WriteByte('+')
	for _, w := range widths {
		b.WriteString(strings.Repeat("-", w+2))
		b.WriteByte('+')
	}
	return b.String()
}

func buildRow(cells []string, widths []int) string {
	var b strings.Builder
	b.WriteByte('|')
	for i, w := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		pad := w - utf8.RuneCountInString(cell)
		if pad < 0 {
			pad = 0
		}
		b.WriteByte(' ')
		b.WriteString(cell)
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteByte(' ')
		b.WriteByte('|')
	}
	return b.String()
}

// truncate shortens s to at most max display columns, appending an ellipsis
// when content was dropped. Operates on runes to avoid splitting multi-byte
// characters.
func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// htmlReport is the data shape passed to the HTML template.
type htmlReport struct {
	GeneratedAt    string
	Target         string
	TotalMatches   int
	FileCount      int
	PatternCount   int
	PatternSummary []htmlPatternRow
	FileGroups     []htmlFileGroup
}

type htmlPatternRow struct {
	Pattern string
	Count   int
}

type htmlFileGroup struct {
	File    string
	Matches []Match
}

// writeHTMLReport renders the matches into a self-contained HTML page (no
// external assets, no JS dependencies). Safe to open from disk or share as a
// single file. Uses html/template so all dynamic content is auto-escaped.
func writeHTMLReport(path, target string, matches []Match) error {
	report := buildHTMLReport(target, matches)

	// Write to a temp file in the same dir, then rename — gives an atomic
	// replace on POSIX so a crash mid-write doesn't leave a half-baked report.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".regex-scanner-report-*.html")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}

	tpl, err := template.New("report").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}).Parse(htmlTemplate)
	if err != nil {
		cleanup()
		return err
	}

	bw := bufio.NewWriter(tmp)
	if err := tpl.Execute(bw, report); err != nil {
		cleanup()
		return err
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func buildHTMLReport(target string, matches []Match) htmlReport {
	// Group matches by file, preserving stable ordering: file A-Z, then by line/col.
	byFile := map[string][]Match{}
	for _, m := range matches {
		byFile[m.File] = append(byFile[m.File], m)
	}
	fileNames := make([]string, 0, len(byFile))
	for f := range byFile {
		fileNames = append(fileNames, f)
	}
	sort.Strings(fileNames)

	groups := make([]htmlFileGroup, 0, len(fileNames))
	for _, f := range fileNames {
		group := byFile[f]
		sort.Slice(group, func(i, j int) bool {
			if group[i].Line != group[j].Line {
				return group[i].Line < group[j].Line
			}
			return group[i].Column < group[j].Column
		})
		groups = append(groups, htmlFileGroup{File: f, Matches: group})
	}

	// Pattern summary, sorted by count desc then pattern asc.
	counts := map[string]int{}
	for _, m := range matches {
		counts[m.Pattern]++
	}
	summary := make([]htmlPatternRow, 0, len(counts))
	for p, c := range counts {
		summary = append(summary, htmlPatternRow{Pattern: p, Count: c})
	}
	sort.Slice(summary, func(i, j int) bool {
		if summary[i].Count != summary[j].Count {
			return summary[i].Count > summary[j].Count
		}
		return summary[i].Pattern < summary[j].Pattern
	})

	return htmlReport{
		GeneratedAt:    time.Now().Format(time.RFC1123),
		Target:         target,
		TotalMatches:   len(matches),
		FileCount:      len(byFile),
		PatternCount:   len(counts),
		PatternSummary: summary,
		FileGroups:     groups,
	}
}

// htmlTemplate is intentionally inline so the binary stays single-file. All
// values are run through html/template's auto-escaping. Styles use system fonts
// and a small palette so the page renders cleanly on light or dark OS themes.
const htmlTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>regex-scanner report</title>
<style>
  :root {
    --bg: #ffffff;
    --fg: #1f2328;
    --muted: #57606a;
    --border: #d0d7de;
    --accent: #0969da;
    --accent-soft: #ddf4ff;
    --code-bg: #f6f8fa;
    --row-alt: #f6f8fa;
    --hit: #fff8c5;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0d1117;
      --fg: #e6edf3;
      --muted: #8b949e;
      --border: #30363d;
      --accent: #2f81f7;
      --accent-soft: #0c2d6b;
      --code-bg: #161b22;
      --row-alt: #161b22;
      --hit: #5d4d0a;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 2rem;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
    color: var(--fg);
    background: var(--bg);
    line-height: 1.5;
  }
  h1 { margin: 0 0 0.25rem 0; font-size: 1.5rem; }
  h2 { margin-top: 2rem; font-size: 1.15rem; border-bottom: 1px solid var(--border); padding-bottom: 0.35rem; }
  .meta { color: var(--muted); font-size: 0.9rem; margin-bottom: 1.5rem; }
  .stats { display: flex; gap: 1rem; flex-wrap: wrap; margin: 1rem 0 0.5rem; }
  .stat {
    border: 1px solid var(--border);
    background: var(--code-bg);
    border-radius: 6px;
    padding: 0.6rem 0.9rem;
    min-width: 7rem;
  }
  .stat-num { font-size: 1.4rem; font-weight: 600; }
  .stat-label { color: var(--muted); font-size: 0.8rem; text-transform: uppercase; letter-spacing: 0.04em; }
  table { border-collapse: collapse; width: 100%; margin-top: 0.5rem; }
  th, td {
    text-align: left;
    padding: 0.45rem 0.7rem;
    border-bottom: 1px solid var(--border);
    vertical-align: top;
    font-size: 0.92rem;
  }
  th { background: var(--code-bg); font-weight: 600; }
  tbody tr:nth-child(even) td { background: var(--row-alt); }
  code, .mono {
    font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
    font-size: 0.88rem;
  }
  td.pattern code { background: var(--accent-soft); padding: 0.05rem 0.35rem; border-radius: 4px; }
  td.match code { background: var(--code-bg); padding: 0.05rem 0.35rem; border-radius: 4px; }
  .file-group { margin-top: 1.25rem; }
  .file-group h3 {
    font-size: 1rem;
    margin: 0 0 0.25rem 0;
    word-break: break-all;
  }
  .file-group h3 code { background: var(--code-bg); padding: 0.1rem 0.4rem; border-radius: 4px; }
  .loc { color: var(--muted); white-space: nowrap; }
  .empty { color: var(--muted); font-style: italic; }
  .filter {
    margin: 0.5rem 0 1rem;
    display: flex;
    gap: 0.5rem;
    align-items: center;
    flex-wrap: wrap;
  }
  .filter input {
    padding: 0.35rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--bg);
    color: var(--fg);
    min-width: 16rem;
  }
  .filter label { color: var(--muted); font-size: 0.85rem; }
  tr.hidden { display: none; }
</style>
</head>
<body>
  <h1>regex-scanner report</h1>
  <div class="meta">
    Target: <code>{{.Target}}</code> &middot; Generated {{.GeneratedAt}}
  </div>

  <div class="stats">
    <div class="stat"><div class="stat-num">{{.TotalMatches}}</div><div class="stat-label">Matches</div></div>
    <div class="stat"><div class="stat-num">{{.FileCount}}</div><div class="stat-label">Files</div></div>
    <div class="stat"><div class="stat-num">{{.PatternCount}}</div><div class="stat-label">Patterns</div></div>
  </div>

  {{if eq .TotalMatches 0}}
    <p class="empty">No matches.</p>
  {{else}}
    <h2>Pattern summary</h2>
    <table>
      <thead><tr><th>Pattern</th><th>Hits</th></tr></thead>
      <tbody>
        {{range .PatternSummary}}
          <tr><td class="pattern"><code>{{.Pattern}}</code></td><td class="mono">{{.Count}}</td></tr>
        {{end}}
      </tbody>
    </table>

    <h2>All matches</h2>
    <div class="filter">
      <label for="q">Filter:</label>
      <input id="q" type="text" placeholder="substring filter (file, pattern, or match)" autocomplete="off">
      <span id="counter" class="loc"></span>
    </div>
    <table id="matches">
      <thead>
        <tr><th>File</th><th>Loc</th><th>Pattern</th><th>Match</th></tr>
      </thead>
      <tbody>
        {{range .FileGroups}}
          {{$file := .File}}
          {{range .Matches}}
            <tr>
              <td><code>{{$file}}</code></td>
              <td class="loc mono">{{.Line}}:{{.Column}}</td>
              <td class="pattern"><code>{{.Pattern}}</code></td>
              <td class="match"><code>{{.Text}}</code></td>
            </tr>
          {{end}}
        {{end}}
      </tbody>
    </table>

    <h2>By file</h2>
    {{range .FileGroups}}
      <div class="file-group">
        <h3><code>{{.File}}</code> <span class="loc">({{len .Matches}} match{{if ne (len .Matches) 1}}es{{end}})</span></h3>
        <table>
          <thead><tr><th>Loc</th><th>Pattern</th><th>Match</th></tr></thead>
          <tbody>
            {{range .Matches}}
              <tr>
                <td class="loc mono">{{.Line}}:{{.Column}}</td>
                <td class="pattern"><code>{{.Pattern}}</code></td>
                <td class="match"><code>{{.Text}}</code></td>
              </tr>
            {{end}}
          </tbody>
        </table>
      </div>
    {{end}}
  {{end}}

  <script>
    // Tiny client-side filter for the "All matches" table. No deps.
    (function () {
      var input = document.getElementById('q');
      var counter = document.getElementById('counter');
      var table = document.getElementById('matches');
      if (!input || !table) return;
      var rows = Array.prototype.slice.call(table.querySelectorAll('tbody tr'));
      var total = rows.length;

      function update() {
        var q = input.value.trim().toLowerCase();
        var visible = 0;
        for (var i = 0; i < rows.length; i++) {
          var r = rows[i];
          if (q === '' || r.textContent.toLowerCase().indexOf(q) !== -1) {
            r.classList.remove('hidden');
            visible++;
          } else {
            r.classList.add('hidden');
          }
        }
        counter.textContent = visible + ' / ' + total + ' shown';
      }
      input.addEventListener('input', update);
      update();
    })();
  </script>
</body>
</html>
`
