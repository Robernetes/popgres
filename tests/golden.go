package tests

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// GoldenEnvVar is the env variable that controls the capture of the API golden
// files used to verify the response compatibility with PocketBase (ADR-0010, ADR-0013).
//
// The response of every passing [ApiScenario] is saved in
// tests/golden/api/<package>/<test>/<subtest>.json when it is set to:
//   - "write" - overwrites any existing golden file;
//   - "merge" - compares with the existing golden file (if any) and replaces
//     every value that differs with [GoldenDynamic].
//
// The expected workflow is a "write" run followed by a "merge" run so that
// the values that change between executions (generated ids, random ports, etc.) are masked.
//
// Object keys are sorted because PocketBase doesn't guarantee the order of the
// keys of map values (encoding/json/v2 without json.Deterministic).
const GoldenEnvVar = "POPGRES_GOLDEN"

const (
	GoldenDynamic = "<dynamic>"
	GoldenJWT     = "<jwt>"
	GoldenNow     = "<now>"
)

const goldenMaxTextBody = 1 << 20

const goldenDateTimeLayout = "2006-01-02 15:04:05.000Z"

// goldenStart is used to detect the datetime values generated during the test run.
var goldenStart = time.Now().UTC()

var (
	goldenJWTRegex      = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	goldenJWTInlineRe   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	goldenDateTimeRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}Z`)
	goldenUnsafeChars   = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

	// keeps distinct the scenario names that differ only by an operator (eg. "<=" and ">")
	goldenOperatorNames = strings.NewReplacer(
		"<=", "lte",
		">=", "gte",
		"!=", "ne",
		"<", "lt",
		">", "gt",
		"=", "eq",
	)
)

var (
	goldenRootOnce sync.Once
	goldenRootDir  string
	goldenRootErr  error

	goldenWrittenMu sync.Mutex
	goldenWritten   = map[string]string{} // file -> test name
)

// captureGolden registers a cleanup that saves the scenario response
// as golden file if the test passed and the golden capture is enabled.
func (scenario *ApiScenario) captureGolden(t testing.TB, recorder *httptest.ResponseRecorder) {
	mode := os.Getenv(GoldenEnvVar)
	if mode != "write" && mode != "merge" {
		return
	}

	// benchmarks are not captured
	if _, ok := t.(*testing.T); !ok {
		return
	}

	status := recorder.Code
	contentType := recorder.Header().Get("Content-Type")
	body := bytes.Clone(recorder.Body.Bytes())

	t.Cleanup(func() {
		if t.Failed() {
			return
		}

		if err := writeGolden(t.Name(), mode == "merge", status, contentType, body); err != nil {
			t.Errorf("Failed to write golden file: %v", err)
		}
	})
}

func writeGolden(testName string, merge bool, status int, contentType string, body []byte) error {
	file, err := goldenFilePath(testName)
	if err != nil {
		return err
	}

	goldenWrittenMu.Lock()
	prev, exists := goldenWritten[file]
	goldenWritten[file] = testName
	goldenWrittenMu.Unlock()
	if exists && prev != testName {
		return fmt.Errorf("golden file name collision between %q and %q", prev, testName)
	}

	node, err := newGoldenNode(status, contentType, body)
	if err != nil {
		return err
	}

	if merge {
		existing, err := os.ReadFile(file)
		if err == nil {
			old, err := parseGoldenJSON(existing)
			if err != nil {
				return fmt.Errorf("failed to parse existing %s: %w", file, err)
			}
			node = mergeGoldenNodes(old, node)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	buf := new(bytes.Buffer)
	enc := jsontext.NewEncoder(buf, jsontext.Multiline(true), jsontext.WithIndent("  "))
	if err := node.write(enc); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
		return err
	}

	return os.WriteFile(file, buf.Bytes(), 0o644)
}

// goldenFilePath returns tests/golden/api/<package dir>/<test name parts>.json
// resolved from the module root.
func goldenFilePath(testName string) (string, error) {
	root, err := goldenModuleRoot()
	if err != nil {
		return "", err
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	pkgDir, err := filepath.Rel(root, wd)
	if err != nil {
		return "", err
	}

	parts := []string{root, "tests", "golden", "api", filepath.ToSlash(pkgDir)}
	for _, p := range strings.Split(testName, "/") {
		parts = append(parts, goldenSafeName(p))
	}

	return filepath.Join(parts...) + ".json", nil
}

// goldenSafeName normalizes a test name part into a portable file name.
func goldenSafeName(name string) string {
	safe := goldenOperatorNames.Replace(name)
	safe = strings.Trim(goldenUnsafeChars.ReplaceAllString(safe, "_"), "._")
	if safe == "" {
		safe = "_"
	}

	// keep the paths short enough for Windows checkouts
	if len(safe) > 80 {
		sum := sha256.Sum256([]byte(name))
		safe = safe[:70] + "-" + hex.EncodeToString(sum[:4])
	}

	return safe
}

func goldenModuleRoot() (string, error) {
	goldenRootOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			goldenRootErr = err
			return
		}

		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				goldenRootDir = dir
				return
			}

			parent := filepath.Dir(dir)
			if parent == dir {
				goldenRootErr = errors.New("go.mod not found")
				return
			}
			dir = parent
		}
	})

	return goldenRootDir, goldenRootErr
}

// -------------------------------------------------------------------

// goldenNode is a minimal JSON tree that keeps the original number literals.
type goldenNode struct {
	kind    jsontext.Kind // '{', '[', '"' or the kind of the raw literal
	value   string        // the unescaped string or the raw literal
	members []goldenMember
	items   []*goldenNode
}

type goldenMember struct {
	name string
	node *goldenNode
}

func goldenString(v string) *goldenNode {
	return &goldenNode{kind: '"', value: v}
}

// newGoldenNode builds the golden file tree of a single response.
func newGoldenNode(status int, contentType string, body []byte) (*goldenNode, error) {
	root := &goldenNode{kind: '{'}

	root.members = append(root.members,
		goldenMember{"status", &goldenNode{kind: '0', value: fmt.Sprint(status)}},
		goldenMember{"contentType", goldenString(contentType)},
	)

	if len(body) > 0 {
		if jsontext.Value(body).IsValid() {
			node, err := parseGoldenJSON(body)
			if err != nil {
				return nil, err
			}
			node.mask()
			root.members = append(root.members, goldenMember{"body", node})
		} else if utf8.Valid(body) && len(body) <= goldenMaxTextBody {
			root.members = append(root.members, goldenMember{"bodyText", goldenString(maskGoldenText(string(body)))})
		} else {
			sum := sha256.Sum256(body)
			root.members = append(root.members, goldenMember{"bodySha256", goldenString(hex.EncodeToString(sum[:]))})
		}
	}

	root.sortMembers()

	return root, nil
}

func parseGoldenJSON(data []byte) (*goldenNode, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))

	return readGoldenNode(dec)
}

func readGoldenNode(dec *jsontext.Decoder) (*goldenNode, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}

	switch tok.Kind() {
	case '{':
		node := &goldenNode{kind: '{'}
		for dec.PeekKind() != '}' {
			nameTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			// the token is voided by the next decoder call
			name := nameTok.String()
			child, err := readGoldenNode(dec)
			if err != nil {
				return nil, err
			}
			node.members = append(node.members, goldenMember{name, child})
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		node.sortMembers()
		return node, nil
	case '[':
		node := &goldenNode{kind: '['}
		for dec.PeekKind() != ']' {
			child, err := readGoldenNode(dec)
			if err != nil {
				return nil, err
			}
			node.items = append(node.items, child)
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return node, nil
	default:
		// for non-string tokens String() returns the raw literal
		return &goldenNode{kind: tok.Kind(), value: tok.String()}, nil
	}
}

func (n *goldenNode) sortMembers() {
	slices.SortStableFunc(n.members, func(a, b goldenMember) int {
		return strings.Compare(a.name, b.name)
	})
}

// mask replaces the values generated during the test run (JWT tokens and recent datetimes).
func (n *goldenNode) mask() {
	switch n.kind {
	case '{':
		for _, m := range n.members {
			m.node.mask()
		}
	case '[':
		for _, item := range n.items {
			item.mask()
		}
	case '"':
		if goldenJWTRegex.MatchString(n.value) {
			n.value = GoldenJWT
		} else if len(n.value) == len(goldenDateTimeLayout) && isGoldenRecent(n.value) {
			n.value = GoldenNow
		}
	}
}

func (n *goldenNode) write(enc *jsontext.Encoder) error {
	switch n.kind {
	case '{':
		if err := enc.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		for _, m := range n.members {
			if err := enc.WriteToken(jsontext.String(m.name)); err != nil {
				return err
			}
			if err := m.node.write(enc); err != nil {
				return err
			}
		}
		return enc.WriteToken(jsontext.EndObject)
	case '[':
		if err := enc.WriteToken(jsontext.BeginArray); err != nil {
			return err
		}
		for _, item := range n.items {
			if err := item.write(enc); err != nil {
				return err
			}
		}
		return enc.WriteToken(jsontext.EndArray)
	case '"':
		return enc.WriteToken(jsontext.String(n.value))
	default:
		return enc.WriteValue(jsontext.Value(n.value))
	}
}

// mergeGoldenNodes returns a copy of b where every value that differs from a is
// replaced with [GoldenDynamic].
func mergeGoldenNodes(a, b *goldenNode) *goldenNode {
	if a.kind != b.kind {
		return goldenString(GoldenDynamic)
	}

	switch b.kind {
	case '{':
		if len(a.members) != len(b.members) {
			return goldenString(GoldenDynamic)
		}
		merged := &goldenNode{kind: '{', members: make([]goldenMember, len(b.members))}
		for i, m := range b.members {
			if a.members[i].name != m.name {
				return goldenString(GoldenDynamic)
			}
			merged.members[i] = goldenMember{m.name, mergeGoldenNodes(a.members[i].node, m.node)}
		}
		return merged
	case '[':
		if len(a.items) != len(b.items) {
			return goldenString(GoldenDynamic)
		}
		merged := &goldenNode{kind: '[', items: make([]*goldenNode, len(b.items))}
		for i, item := range b.items {
			merged.items[i] = mergeGoldenNodes(a.items[i], item)
		}
		return merged
	default:
		if a.value != b.value {
			return goldenString(GoldenDynamic)
		}
		return b
	}
}

func maskGoldenText(v string) string {
	v = goldenJWTInlineRe.ReplaceAllString(v, GoldenJWT)

	return goldenDateTimeRegex.ReplaceAllStringFunc(v, func(match string) string {
		if isGoldenRecent(match) {
			return GoldenNow
		}
		return match
	})
}

// isGoldenRecent reports whether the datetime was likely generated during the test run
// (the fixtures and the hardcoded test values are much older).
func isGoldenRecent(v string) bool {
	dt, err := time.Parse(goldenDateTimeLayout, v)
	if err != nil {
		return false
	}

	return !dt.Before(goldenStart.Add(-24 * time.Hour))
}
