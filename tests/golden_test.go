package tests

import (
	"bytes"
	"encoding/json/jsontext"
	"testing"
)

func encodeGoldenNodeCompact(t *testing.T, n *goldenNode) string {
	buf := new(bytes.Buffer)
	if err := n.write(jsontext.NewEncoder(buf)); err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(buf.Bytes()))
}

func TestNewGoldenNode(t *testing.T) {
	t.Parallel()

	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6IjEyMyJ9.abc_DEF-123"
	now := goldenStart.Format(goldenDateTimeLayout)

	scenarios := []struct {
		name     string
		body     string
		expected string
	}{
		{
			"empty body",
			``,
			`{"contentType":"text/plain","status":200}`,
		},
		{
			"sorted keys, kept number literals and masked values",
			`{"z":1.50,"a":[{"token":"` + jwt + `","created":"` + now + `","old":"2022-10-11 09:42:15.113Z"}],"m":null}`,
			`{"body":{"a":[{"created":"<now>","old":"2022-10-11 09:42:15.113Z","token":"<jwt>"}],"m":null,"z":1.50},"contentType":"text/plain","status":200}`,
		},
		{
			"plain text body",
			`token ` + jwt + ` at ` + now,
			`{"bodyText":"token <jwt> at <now>","contentType":"text/plain","status":200}`,
		},
		{
			"binary body",
			"\xff\xfe",
			"", // checked only by its key
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			node, err := newGoldenNode(200, "text/plain", []byte(s.body))
			if err != nil {
				t.Fatal(err)
			}

			result := encodeGoldenNodeCompact(t, node)

			// the binary checksum is checked only by its key
			if s.name == "binary body" {
				if !bytes.HasPrefix([]byte(result), []byte(`{"bodySha256":"`)) {
					t.Fatalf("Expected bodySha256, got %s", result)
				}
				return
			}

			if result != s.expected {
				t.Fatalf("Expected\n%s\ngot\n%s", s.expected, result)
			}
		})
	}
}

func TestMergeGoldenNodes(t *testing.T) {
	t.Parallel()

	a, err := parseGoldenJSON([]byte(`{"id":"abc","same":{"x":[1,2]},"list":[1],"kind":1,"obj":{"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}

	b, err := parseGoldenJSON([]byte(`{"obj":{"b":1},"kind":"1","list":[1,2],"same":{"x":[1,2]},"id":"def"}`))
	if err != nil {
		t.Fatal(err)
	}

	result := encodeGoldenNodeCompact(t, mergeGoldenNodes(a, b))

	expected := `{"id":"<dynamic>","kind":"<dynamic>","list":"<dynamic>","obj":"<dynamic>","same":{"x":[1,2]}}`
	if result != expected {
		t.Fatalf("Expected\n%s\ngot\n%s", expected, result)
	}
}

func TestGoldenSafeName(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name     string
		expected string
	}{
		{"", "_"},
		{"GET:/api/health", "GET_api_health"},
		{"missing_collection#01", "missing_collection_01"},
		{"..", "_"},
		{"body <= limit", "body_lte_limit"},
		{"body > limit", "body_gt_limit"},
		{"with < 9", "with_lt_9"},
		{"a != b", "a_ne_b"},
	}

	for _, s := range scenarios {
		if v := goldenSafeName(s.name); v != s.expected {
			t.Errorf("[%q] Expected %q, got %q", s.name, s.expected, v)
		}
	}

	long := goldenSafeName(string(bytes.Repeat([]byte("a"), 200)))
	if len(long) != 79 {
		t.Errorf("Expected truncated name with hash suffix, got %q", long)
	}
}
