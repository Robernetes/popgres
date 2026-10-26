package core_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/types"
)

// rlsGoldenEnvVar regenerates the golden file of the compiled fixtures policies when set to "write".
const rlsGoldenEnvVar = "POPGRES_RLS_GOLDEN"

func rlsGoldenFile() string {
	_, currentFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(currentFile), "..", "tests", "golden", "rls-fixtures.sql")
}

// TestRLSCompileFixtures checks the policies compiled from the rules of all
// fixtures collections against the golden file.
func TestRLSCompileFixtures(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	for _, collection := range collections {
		compiled, err := core.CompileRLS(app, collection)
		if err != nil {
			t.Fatalf("[%s] %v", collection.Name, err)
		}

		b.WriteString("-- " + collection.Name + " (" + compiled.Status + ")\n")
		for _, p := range compiled.Policies {
			filter := "null"
			if p.Filter != nil {
				filter = *p.Filter
			}
			b.WriteString("-- " + p.Rule + ": " + p.Status)
			if p.Status == core.RLSStatusTranslated && !p.Inline {
				b.WriteString(" (function)")
			}
			b.WriteString(" | " + strings.ReplaceAll(filter, "\n", " "))
			if p.Reason != "" {
				b.WriteString(" | " + p.Reason)
			}
			b.WriteString("\n")
		}
		b.WriteString(compiled.SQL)
		b.WriteString("\n")
	}

	result := b.String()

	if os.Getenv(rlsGoldenEnvVar) == "write" {
		if err := os.WriteFile(rlsGoldenFile(), []byte(result), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	expected, err := os.ReadFile(rlsGoldenFile())
	if err != nil {
		t.Fatalf("Failed to read the golden file (regenerate it with %s=write): %v", rlsGoldenEnvVar, err)
	}

	if strings.ReplaceAll(string(expected), "\r\n", "\n") != result {
		t.Fatalf("The compiled policies don't match %s (regenerate it with %s=write if the change is expected):\n%s", rlsGoldenFile(), rlsGoldenEnvVar, result)
	}
}

// TestRLSFixturesApplied checks that the policies of all fixtures collections
// were applied without errors (the failed definitions are marked in the state).
func TestRLSFixturesApplied(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if err := core.SyncRLSPolicies(app); err != nil {
		t.Fatal(err)
	}

	var raw string
	if err := app.DB().NewQuery("SELECT [[value]] FROM {{_params}} WHERE [[id]] = 'popgresRLS'").Row(&raw); err != nil {
		t.Fatal(err)
	}

	state := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}

	collections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range collections {
		hash, ok := state[c.Id]
		if !ok {
			t.Errorf("[%s] Missing RLS state", c.Name)
			continue
		}
		if strings.HasPrefix(hash, "error:") {
			t.Errorf("[%s] The RLS definition failed to apply (see the app logs)", c.Name)
		}
	}

	// the policies of every non-system table collection
	var total int
	err = app.DB().NewQuery("SELECT count(*) FROM pg_policies WHERE starts_with(policyname, 'popgres_')").Row(&total)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("Expected the popgres policies to be created")
	}
}

func TestRLSRuleStatuses(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		rule     *string
		status   string
		inline   bool
		contains string
	}{
		{nil, core.RLSStatusSuperusersOnly, false, ""},
		{types.Pointer(""), core.RLSStatusTranslated, true, "TRUE"},
		{types.Pointer("text = 'x'"), core.RLSStatusTranslated, true, `"demo1"."text"`},
		{types.Pointer("text = \"it's {{a}} [[b]] {:c}\""), core.RLSStatusTranslated, true, `E'it''s \x7b\x7ba}} \x5b\x5bb]] \x7b:c}'`},
		{types.Pointer("@request.auth.id != ''"), core.RLSStatusTranslated, true, "popgres_rls.auth() ->> 'id'"},
		{types.Pointer("@request.auth.verified = true"), core.RLSStatusTranslated, false, "popgres_rls.auth() AS a"},
		{types.Pointer("rel_one.text != ''"), core.RLSStatusTranslated, false, "rls_"},
		{types.Pointer("@collection.demo2.title ?= text"), core.RLSStatusTranslated, false, "rls_"},
		{types.Pointer("@request.body.text = 'x'"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("@request.query.a = 'x'"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("@request.headers.a = 'x'"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("@request.method = 'GET'"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("@request.context = 'default'"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("@request.body.text:isset = true"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("@request.body.text:changed = true"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("'abc' ~ @request.auth.id"), core.RLSStatusDenied, false, "FALSE"},
		{types.Pointer("missing_field = 1"), core.RLSStatusDenied, false, "FALSE"},
	}

	for _, s := range scenarios {
		name := "null"
		if s.rule != nil {
			name = *s.rule
		}

		t.Run(name, func(t *testing.T) {
			collection, err := app.FindCollectionByNameOrId("demo1")
			if err != nil {
				t.Fatal(err)
			}
			collection.ListRule = s.rule

			compiled, err := core.CompileRLS(app, collection)
			if err != nil {
				t.Fatal(err)
			}

			policy := compiled.Policies[0]
			if policy.Status != s.status {
				t.Fatalf("Expected status %q, got %q (%s)", s.status, policy.Status, policy.Reason)
			}
			if policy.Inline != s.inline {
				t.Fatalf("Expected inline %v, got %v", s.inline, policy.Inline)
			}
			if !strings.Contains(policy.SQL, s.contains) {
				t.Fatalf("Expected the policy SQL to contain %q, got\n%s", s.contains, policy.SQL)
			}
			if s.status == core.RLSStatusDenied && policy.Reason == "" {
				t.Fatal("Expected a non-empty reason for the denied policy")
			}
		})
	}
}

func TestRLSRuleTranslatable(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection, err := app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}

	collection.ListRule = types.Pointer("text != ''")
	collection.ViewRule = types.Pointer("@request.headers.x = 'y'")
	collection.CreateRule = nil

	if !core.RLSRuleTranslatable(app, collection, core.RLSRuleList) {
		t.Fatal("Expected the list rule to be translatable")
	}
	if core.RLSRuleTranslatable(app, collection, core.RLSRuleView) {
		t.Fatal("Expected the view rule to be untranslatable")
	}
	if core.RLSRuleTranslatable(app, collection, core.RLSRuleCreate) {
		t.Fatal("Expected the null create rule to be untranslatable")
	}

	superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	if core.RLSRuleTranslatable(app, superusers, core.RLSRuleList) {
		t.Fatal("Expected the system collection rules to be untranslatable")
	}
}

func TestRLSSyncOnCollectionChanges(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	policies := func(table string) []string {
		var names []string
		err := app.DB().NewQuery("SELECT policyname FROM pg_policies WHERE tablename = {:table} ORDER BY policyname").
			Bind(map[string]any{"table": table}).
			Column(&names)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	functions := func(collectionId string) int {
		var total int
		err := app.DB().NewQuery("SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'popgres_rls' AND starts_with(p.proname, {:prefix})").
			Bind(map[string]any{"prefix": "rls_" + collectionId + "_"}).
			Row(&total)
		if err != nil {
			t.Fatal(err)
		}
		return total
	}

	collection := core.NewBaseCollection("rls_sync")
	collection.Fields.Add(&core.TextField{Name: "title"}, &core.RelationField{Name: "rel", CollectionId: "_pb_users_auth_"})
	collection.ListRule = types.Pointer("rel.verified = true")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	if names := policies("rls_sync"); len(names) != 1 {
		t.Fatalf("Expected 1 policy (the other rules are null), got %v", names)
	}
	if total := functions(collection.Id); total != 1 {
		t.Fatalf("Expected 1 policy function, got %d", total)
	}

	// rename and change the rule to an inline one
	collection.Name = "rls_sync_renamed"
	collection.ListRule = types.Pointer("title != ''")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	if names := policies("rls_sync_renamed"); len(names) != 1 {
		t.Fatalf("Expected 1 policy after the rename, got %v", names)
	}
	if total := functions(collection.Id); total != 0 {
		t.Fatalf("Expected the policy function to be dropped, got %d", total)
	}

	// delete
	collection.ListRule = types.Pointer("rel.verified = true")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(collection); err != nil {
		t.Fatal(err)
	}
	if total := functions(collection.Id); total != 0 {
		t.Fatalf("Expected the policy functions of the deleted collection to be dropped, got %d", total)
	}
}
