// regex-scanner is a small CLI that scans a file or directory tree against
// one or more regular expressions (regexp2 dialect) and reports every match
// with file path, line, column, the matched text, and which pattern caught it.
//
// Dependency: github.com/dlclark/regexp2 v1.11.5
//
// Usage examples:
//
//	# Single pattern, single file
//	regex-scanner -pattern '"gp2"' ./main.tf
//
//	# Multiple patterns, recursive directory scan
//	regex-scanner -pattern 'volume_type\s*=\s*"gp2"' \
//	              -pattern 'storage_type\s*=\s*"gp2"' ./infra
//
//	# Patterns from a file (one per line, blank lines and # comments ignored)
//	regex-scanner -patterns-file patterns.txt ./infra
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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

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

	matches := scanFiles(files, patterns, cfg)

	return reportMatches(matches, cfg.jsonOutput)
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

func reportMatches(matches []Match, asJSON bool) error {
	if len(matches) == 0 {
		fmt.Println("no matches")
		return nil
	}

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	if asJSON {
		enc := json.NewEncoder(w)
		for _, m := range matches {
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		return nil
	}

	for _, m := range matches {
		fmt.Fprintf(w, "%s:%d:%d  [%s]  %s\n", m.File, m.Line, m.Column, m.Pattern, m.Text)
	}
	return nil
}
