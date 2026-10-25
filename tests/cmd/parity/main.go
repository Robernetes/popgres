// Command parity compares the output of "go test -json ./..." with the list of
// the passing tests of PocketBase v0.40.6 (tests/golden/pocketbase-v0.40.6-tests.txt).
//
// Every reference test must pass or be skipped (itself or one of its parent tests)
// with a "popgres: <reason>" message documented in docs/COMPATIBILITY.md.
// The renamed and removed reference tests are listed with their reason in
// tests/golden/parity-renamed.txt.
// The missing tests are printed and the command exits with status 1 if there are any.
//
//	go test -json ./... | go run ./tests/cmd/parity
//	go run ./tests/cmd/parity -in test-output.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// the module path prefixes of the test packages (the reference was captured before the rename)
var modulePrefixes = []string{
	"github.com/Robernetes/popgres",
	"github.com/pocketbase/pocketbase",
}

// the same normalizations used when generating the reference list
// for the run dependent parts of the subtest names
var nameNormalizations = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`0x[0-9a-f]+`), "0x?"},
	{regexp.MustCompile(`time\.Date\([^)]*\)`), "time.Date(?)"},
	{regexp.MustCompile(`ext:-?\d+`), "ext:?"},
	{regexp.MustCompile(`boundary=[0-9a-f]+`), "boundary=?"},
	{regexp.MustCompile(`_[a-z0-9]{10}\.txt`), "_?.txt"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}_\d{2}:\d{2}:\d{2}\.\d{3}Z`), "<datetime>"},
}

const keySeparator = "\t"

type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

func main() {
	var (
		in        string
		reference string
		renamed   string
		verbose   bool
	)

	flag.StringVar(&in, "in", "", "the go test -json output file (default stdin)")
	flag.StringVar(&reference, "ref", "tests/golden/pocketbase-v0.40.6-tests.txt", "the reference tests list")
	flag.StringVar(&renamed, "renamed", "tests/golden/parity-renamed.txt", "the renamed and removed reference tests")
	flag.BoolVar(&verbose, "v", false, "also list the tests skipped, renamed or removed by popgres")
	flag.Parse()

	var input io.Reader = os.Stdin
	if in != "" {
		f, err := os.Open(in)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		input = f
	}

	ref, err := readReference(reference)
	if err != nil {
		fail(err)
	}

	renames, err := readRenamed(renamed)
	if err != nil {
		fail(err)
	}

	passed, skipped, err := readResults(input)
	if err != nil {
		fail(err)
	}

	missing := []string{}
	popgresSkips := []string{}
	for _, key := range ref {
		if _, ok := passed[key]; ok {
			continue
		}
		if r, ok := renames[key]; ok {
			if r.removed() {
				popgresSkips = append(popgresSkips, key+keySeparator+"removed: "+r.reason)
				continue
			}
			if _, ok := passed[r.newKey]; ok {
				popgresSkips = append(popgresSkips, key+keySeparator+"renamed: "+r.reason)
				continue
			}
		}
		if reason, ok := skipReason(skipped, key); ok {
			popgresSkips = append(popgresSkips, key+keySeparator+reason)
			continue
		}
		missing = append(missing, key)
	}

	fmt.Printf(
		"reference: %d, passed: %d, skipped, renamed or removed by popgres: %d, missing: %d\n",
		len(ref),
		len(ref)-len(missing)-len(popgresSkips),
		len(popgresSkips),
		len(missing),
	)

	if verbose {
		for _, s := range popgresSkips {
			fmt.Println("skipped" + keySeparator + s)
		}
	}

	for _, m := range missing {
		fmt.Println("missing" + keySeparator + m)
	}

	if len(missing) > 0 {
		os.Exit(1)
	}
}

type renamedTest struct {
	newKey string // empty for the removed tests
	reason string
}

func (r renamedTest) removed() bool {
	return r.newKey == ""
}

// readRenamed reads the renamed and removed reference tests file
// (tab separated "<package> <reference name> <new name or -> # <reason>" lines).
func readRenamed(path string) (map[string]renamedTest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]renamedTest{}, nil
		}
		return nil, err
	}

	result := map[string]renamedTest{}
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		var reason string
		if idx := strings.Index(line, "\t#"); idx >= 0 {
			reason = strings.TrimSpace(line[idx+2:])
			line = line[:idx]
		}

		parts := strings.Split(line, keySeparator)
		if len(parts) != 3 || reason == "" {
			return nil, fmt.Errorf("%s:%d: expected <package>, <reference name>, <new name or -> and a # reason", path, i+1)
		}

		r := renamedTest{reason: reason}
		if parts[2] != "-" {
			r.newKey = parts[0] + keySeparator + parts[2]
		}
		result[parts[0]+keySeparator+parts[1]] = r
	}

	return result, nil
}

func readReference(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	result := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		result = append(result, line)
	}

	return result, nil
}

// readResults returns the passed tests and the tests skipped with a "popgres:" reason.
func readResults(r io.Reader) (map[string]struct{}, map[string]string, error) {
	passed := map[string]struct{}{}
	skipped := map[string]string{}
	skipReasons := map[string]string{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	for scanner.Scan() {
		var e testEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || e.Test == "" {
			continue
		}

		key := normalizePackage(e.Package) + keySeparator + normalizeName(e.Test)

		switch e.Action {
		case "pass":
			passed[key] = struct{}{}
		case "output":
			if i := strings.Index(e.Output, "popgres:"); i >= 0 {
				skipReasons[key] = strings.TrimSpace(e.Output[i:])
			}
		case "skip":
			if reason, ok := skipReasons[key]; ok {
				skipped[key] = reason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}

	return passed, skipped, nil
}

// skipReason returns the popgres skip reason of the test or of one of its
// parent tests (the subtests of a skipped test don't run at all).
func skipReason(skipped map[string]string, key string) (string, bool) {
	pkgEnd := strings.Index(key, keySeparator)

	for {
		if reason, ok := skipped[key]; ok {
			return reason, true
		}

		i := strings.LastIndex(key, "/")
		if i <= pkgEnd {
			return "", false
		}
		key = key[:i]
	}
}

func normalizePackage(pkg string) string {
	for _, prefix := range modulePrefixes {
		if rest, ok := strings.CutPrefix(pkg, prefix); ok {
			return "." + rest
		}
	}
	return pkg
}

func normalizeName(name string) string {
	for _, n := range nameNormalizations {
		name = n.re.ReplaceAllString(name, n.repl)
	}
	return name
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
