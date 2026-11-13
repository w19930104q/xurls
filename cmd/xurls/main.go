// Copyright (c) 2015, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"mvdan.cc/xurls/v2"
)

var (
	matching    = flag.String("m", "", "")
	relaxed     = flag.Bool("r", false, "")
	fix         boolString
	fixManifest = flag.String("fix-manifest", "", "")
	versionFlag = flag.Bool("version", false, "")
)

type boolString string

func (s *boolString) Set(val string) error {
	*s = boolString(val)
	return nil
}
func (s *boolString) Get() any       { return string(*s) }
func (s *boolString) String() string { return string(*s) }
func (*boolString) IsBoolFlag() bool { return true }

func init() {
	flag.Var(&fix, "fix", "")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `
Usage: xurls [flags] [files]

xurls extracts urls from text using regular expressions.
If no files are given, it reads from standard input.

   -m <regexp>   only match urls whose scheme matches a regexp
                    example: 'https?://|mailto:'
   -r            also match urls without a scheme (relaxed)
   -version      print version and exit

When the -fix or -fix=auto flag is used, xurls instead attempts to replace
any urls which result in a permanent redirect (301 or 308).
It also fails if any urls fail to load, so that they may be removed or replaced.
To replace urls which result in temporary redirect as well, use -fix=all.

Fixing files is transactional: all urls in all files are checked first,
and no file is modified unless every check succeeds and every file can be
written. Fixed files are listed on standard output, one per line.
With -fix-manifest=<file>, a JSON manifest is written listing every url
with its file, line, and either its final url or its error.

Exit codes: 0 on success, 1 on broken urls or I/O errors, 2 on usage errors.
`[1:])
	}
}

// httpTimeout bounds each redirect check.
const httpTimeout = 10 * time.Second

// maxWeight bounds how many redirect checks run in parallel.
const maxWeight = 32

// urlRecord describes the outcome of checking a single url while fixing.
type urlRecord struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	URL   string `json:"url"`
	Final string `json:"final,omitempty"`
	Error string `json:"error,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

// checkError is a url check failure, categorized by kind.
type checkError struct {
	kind string // parse, timeout, dns, connect, redirect, or http_status
	msg  string
}

func (e *checkError) Error() string { return e.msg }

// categorize classifies a client.Do error into a stable kind.
func categorize(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.Contains(urlErr.Err.Error(), "stopped after") {
		return "redirect"
	}
	return "connect"
}

// checkURL normalizes match and, for http and https urls, follows any
// redirects which should be fixed, returning the final url. The fragment is
// preserved across redirects when the redirect target does not set one.
// HEAD is used for the checks, falling back to GET only on 405 Method Not
// Allowed. Any failure is returned as a categorized checkError.
func checkURL(match, userAgent string, fixAll bool, timeout time.Duration) (string, *checkError) {
	origURL, err := url.Parse(match)
	if err != nil {
		return "", &checkError{"parse", err.Error()}
	}
	fixed := origURL.String()
	switch origURL.Scheme {
	case "http", "https":
	default:
		// Not a scheme we can check over HTTP; normalization only.
		return fixed, nil
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			switch req.Response.StatusCode {
			case http.StatusMovedPermanently, http.StatusPermanentRedirect:
				// "auto" and "all" fix permanent redirects.
			case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
				// Only "all" fixes temporary redirects.
				if !fixAll {
					return http.ErrUseLastResponse
				}
			default:
				// Any other redirects are ignored.
				return http.ErrUseLastResponse
			}
			// Inherit the fragment if empty.
			if req.URL.Fragment == "" {
				req.URL.Fragment = origURL.Fragment
			}
			fixed = req.URL.String()
			return nil
		},
	}
	method := http.MethodHead
retry:
	req, err := http.NewRequest(method, fixed, nil)
	if err != nil {
		return "", &checkError{"parse", err.Error()}
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", &checkError{categorize(err), err.Error()}
	}
	if code := resp.StatusCode; code >= 400 {
		resp.Body.Close()
		if code == http.StatusMethodNotAllowed && method == http.MethodHead {
			// HEAD is not allowed; retry once with GET.
			method = http.MethodGet
			goto retry
		}
		return "", &checkError{"http_status", fmt.Sprintf("%d %s", code, http.StatusText(code))}
	}
	resp.Body.Close()
	return fixed, nil
}

// fixLine checks every url match in line, applies any fixes, and returns the
// resulting line. Every match is recorded, and broken urls are reported, via
// the reporter.
func fixLine(r *reporter, path string, lineNo int, line string, matches [][]int, userAgent string, fixAll bool) string {
	offsetWithinLine := 0
	for _, pair := range matches {
		// The indexes are based on the original line.
		pair[0] += offsetWithinLine
		pair[1] += offsetWithinLine
		match := line[pair[0]:pair[1]]
		rec := urlRecord{File: path, Line: lineNo, URL: match}
		final, cerr := checkURL(match, userAgent, fixAll, httpTimeout)
		if cerr != nil {
			rec.Error = cerr.msg
			rec.Kind = cerr.kind
			r.appendBroken(match, cerr.msg)
		} else {
			rec.Final = final
			if final != match {
				// Replace the url, and update offsetWithinLine.
				newLine := line[:pair[0]] + final + line[pair[1]:]
				offsetWithinLine += len(newLine) - len(line)
				line = newLine
			}
		}
		r.appendRecord(rec)
	}
	return line
}

// extractPath prints the urls found in path, one per line, in order.
func extractPath(re *regexp.Regexp, path string) error {
	in := os.Stdin
	if path != "-" {
		var err error
		in, err = os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
	}
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		for _, pair := range re.FindAllStringIndex(line, -1) {
			fmt.Printf("%s\n", line[pair[0]:pair[1]])
		}
	}
	return scanner.Err()
}

// fixStream fixes urls read from standard input, writing the result to
// standard output. It returns the url records, and an error if any url is
// broken.
func fixStream(re *regexp.Regexp, userAgent string, fixAll bool) ([]urlRecord, error) {
	seq := newSequencer(maxWeight, os.Stdout, os.Stderr)
	scanner := bufio.NewScanner(os.Stdin)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text() + "\n"
		matches := re.FindAllStringIndex(line, -1)
		ln := lineNo
		weight := min(int64(len(matches)), maxWeight)
		seq.Add(weight, func(r *reporter) error {
			io.WriteString(r, fixLine(r, "-", ln, line, matches, userAgent, fixAll))
			return nil
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	state := seq.finalState()
	if len(state.brokenURLs) > 0 {
		return state.records, brokenError("-", state.brokenURLs)
	}
	return state.records, nil
}

// fileOutcome is the planned result of fixing a single file.
type fileOutcome struct {
	path    string
	mode    os.FileMode
	content []byte
	changed bool
	broken  []brokenURL
	records []urlRecord
}

// fixFile checks every url in path and computes the fixed content, without
// modifying the file.
func fixFile(re *regexp.Regexp, path, userAgent string, fixAll bool) (*fileOutcome, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	out := &fileOutcome{path: path, mode: info.Mode().Perm()}
	buf := new(bytes.Buffer)
	seq := newSequencer(maxWeight, buf, io.Discard)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text() + "\n"
		matches := re.FindAllStringIndex(line, -1)
		ln := lineNo
		weight := min(int64(len(matches)), maxWeight)
		seq.Add(weight, func(r *reporter) error {
			io.WriteString(r, fixLine(r, path, ln, line, matches, userAgent, fixAll))
			return nil
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	state := seq.finalState()
	out.content = buf.Bytes()
	out.broken = state.brokenURLs
	out.records = state.records
	for _, rec := range state.records {
		if rec.Error == "" && rec.Final != rec.URL {
			out.changed = true
			break
		}
	}
	return out, nil
}

// publish atomically writes the fixed content of every changed file, via a
// temporary file and rename per file. If any temporary file cannot be
// written, no file is modified at all.
func publish(outcomes []*fileOutcome) error {
	type staged struct{ tmp, path string }
	var temps []staged
	removeTemps := func() {
		for _, t := range temps {
			os.Remove(t.tmp)
		}
	}
	for _, o := range outcomes {
		if !o.changed {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(o.path), ".xurls-fix-*")
		if err != nil {
			removeTemps()
			return fmt.Errorf("cannot publish fixes: %w", err)
		}
		_, werr := tmp.Write(o.content)
		cerr := tmp.Chmod(o.mode)
		if err := tmp.Close(); werr == nil && cerr == nil {
			werr = err
		}
		if werr != nil || cerr != nil {
			os.Remove(tmp.Name())
			removeTemps()
			return fmt.Errorf("cannot publish fixes to %s: %w", o.path, errors.Join(werr, cerr))
		}
		temps = append(temps, staged{tmp.Name(), o.path})
	}
	// All temporary files are written; rename them into place.
	var renamed int
	for _, t := range temps {
		if err := os.Rename(t.tmp, t.path); err != nil {
			removeTemps()
			return fmt.Errorf("cannot publish fixes: renamed %d of %d files: %w", renamed, len(temps), err)
		}
		renamed++
	}
	return nil
}

func brokenError(path string, broken []brokenURL) error {
	var s strings.Builder
	fmt.Fprintf(&s, "found %d broken urls in %q:", len(broken), path)
	for _, b := range broken {
		fmt.Fprintf(&s, "\n  * %s - %s", b.url, b.reason)
	}
	return errors.New(s.String())
}

// writeManifest writes the JSON manifest of url records, if a manifest path
// was given.
func writeManifest(path string, records []urlRecord) error {
	if path == "" {
		return nil
	}
	if records == nil {
		records = []urlRecord{}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot write manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o666); err != nil {
		return fmt.Errorf("cannot write manifest: %w", err)
	}
	return nil
}

func main() {
	flag.Parse()
	if *versionFlag {
		fmt.Println(readVersion())
		return
	}
	if *relaxed && *matching != "" {
		fmt.Fprintln(os.Stderr, "-r and -m at the same time don't make much sense")
		os.Exit(1)
	}
	switch fix {
	case "": // disabled by default
	case "false": // disabled via -fix=false; normalize
		fix = ""
	case "auto", "all": // enabled via -fix=auto, -fix=all, etc
	case "true": // enabled via -fix; normalize
		fix = "auto"
	default:
		flag.Usage()
		os.Exit(2)
	}
	if *fixManifest != "" && fix == "" {
		fmt.Fprintln(os.Stderr, "-fix-manifest requires -fix")
		os.Exit(2)
	}
	var re *regexp.Regexp
	if *relaxed {
		re = xurls.Relaxed()
	} else if *matching != "" {
		var err error
		if re, err = xurls.StrictMatchingScheme(*matching); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	} else {
		re = xurls.Strict()
	}
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"-"}
	}
	userAgent := fmt.Sprintf("mvdan.cc/xurls %s", readVersion())

	if fix == "" {
		for _, path := range args {
			if err := extractPath(re, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		return
	}
	if len(args) == 1 && args[0] == "-" {
		// Fixing standard input streams the fixed text to standard output.
		records, err := fixStream(re, userAgent, fix == "all")
		if merr := writeManifest(*fixManifest, records); merr != nil {
			fmt.Fprintln(os.Stderr, merr)
			os.Exit(1)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// Fixing one or more files is transactional: check every url in every
	// file first, and only then publish all the fixed files at once.
	seen := make(map[string]bool)
	for _, path := range args {
		if path == "-" {
			fmt.Fprintln(os.Stderr, "cannot mix standard input and files with -fix")
			os.Exit(2)
		}
		if seen[path] {
			fmt.Fprintf(os.Stderr, "file listed twice: %s\n", path)
			os.Exit(2)
		}
		seen[path] = true
	}
	outcomes := make([]*fileOutcome, 0, len(args))
	for _, path := range args {
		outcome, err := fixFile(re, path, userAgent, fix == "all")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		outcomes = append(outcomes, outcome)
	}
	var records []urlRecord
	broken := false
	for _, o := range outcomes {
		records = append(records, o.records...)
		if len(o.broken) > 0 {
			broken = true
			fmt.Fprintln(os.Stderr, brokenError(o.path, o.broken))
		}
	}
	if broken {
		// Roll back: no file is modified.
		if err := writeManifest(*fixManifest, records); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	if err := publish(outcomes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if err := writeManifest(*fixManifest, records); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	for _, o := range outcomes {
		if o.changed {
			fmt.Println(o.path)
		}
	}
	if err := writeManifest(*fixManifest, records); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	mod := &info.Main
	if mod.Replace != nil {
		mod = mod.Replace
	}
	return mod.Version
}
