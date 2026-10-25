package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/types"
	"github.com/pocketbase/dbx"
)

type rlsFixture struct {
	app     *tests.TestApp
	user1   *core.Record
	user2   *core.Record
	token1  string
	token2  string
	posts   *core.Collection
	related *core.Collection
}

// newRLSFixture creates the test collections:
//   - rls_posts with the "owner = @request.auth.id" rules (inline policies)
//   - rls_related with a relation rule that needs a join (function policies)
//   - rls_headers with an untranslatable list rule
func newRLSFixture(t testing.TB) *rlsFixture {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	f := &rlsFixture{app: app}

	users, err := app.FindAllRecords("users", dbx.NewExp("1=1"))
	if err != nil || len(users) < 2 {
		t.Fatalf("Expected at least 2 users, got %d (%v)", len(users), err)
	}
	f.user1 = users[0]
	f.user2 = users[1]

	f.user1.SetVerified(true)
	if err := app.Save(f.user1); err != nil {
		t.Fatal(err)
	}
	f.user2.SetVerified(false)
	if err := app.Save(f.user2); err != nil {
		t.Fatal(err)
	}

	f.token1, err = f.user1.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	f.token2, err = f.user2.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	ownerRule := "owner = @request.auth.id"

	f.posts = core.NewBaseCollection("rls_posts")
	f.posts.Fields.Add(
		&core.TextField{Name: "owner"},
		&core.TextField{Name: "title"},
		&core.TextField{Name: "secret", Hidden: true},
	)
	f.posts.ListRule = &ownerRule
	f.posts.ViewRule = &ownerRule
	f.posts.CreateRule = &ownerRule
	f.posts.UpdateRule = &ownerRule
	f.posts.DeleteRule = &ownerRule
	if err := app.Save(f.posts); err != nil {
		t.Fatal(err)
	}

	relatedRule := "owner = @request.auth.id && owner.verified = true"
	f.related = core.NewBaseCollection("rls_related")
	f.related.Fields.Add(
		&core.RelationField{Name: "owner", CollectionId: f.user1.Collection().Id, MaxSelect: 1},
		&core.TextField{Name: "title"},
	)
	f.related.ListRule = &relatedRule
	if err := app.Save(f.related); err != nil {
		t.Fatal(err)
	}

	headersRule := "@request.headers.x_token = 'abc'"
	headers := core.NewBaseCollection("rls_headers")
	headers.Fields.Add(&core.TextField{Name: "title"})
	headers.ListRule = &headersRule
	if err := app.Save(headers); err != nil {
		t.Fatal(err)
	}

	for i, owner := range []*core.Record{f.user1, f.user1, f.user2} {
		post := core.NewRecord(f.posts)
		post.Set("owner", owner.Id)
		post.Set("title", "post"+string(rune('a'+i)))
		post.Set("secret", "secret")
		if err := app.Save(post); err != nil {
			t.Fatal(err)
		}

		related := core.NewRecord(f.related)
		related.Set("owner", owner.Id)
		related.Set("title", "related"+string(rune('a'+i)))
		if err := app.Save(related); err != nil {
			t.Fatal(err)
		}

		h := core.NewRecord(headers)
		h.Set("title", "h")
		if err := app.Save(h); err != nil {
			t.Fatal(err)
		}
	}

	return f
}

func countRows(t *testing.T, txApp core.App, table string) int {
	t.Helper()

	var total int
	if err := txApp.DB().Select("count(*)").From(table).Row(&total); err != nil {
		t.Fatal(err)
	}

	return total
}

func TestRLSRoles(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	for _, role := range []string{core.RLSAnonRole, core.RLSAuthenticatedRole, core.RLSAPIRole} {
		var isSuperuser, canBypass, canLogin bool
		err := app.DB().NewQuery("SELECT rolsuper, rolbypassrls, rolcanlogin FROM pg_roles WHERE rolname = {:role}").
			Bind(dbx.Params{"role": role}).
			Row(&isSuperuser, &canBypass, &canLogin)
		if err != nil {
			t.Fatalf("[%s] %v", role, err)
		}
		if isSuperuser || canBypass || canLogin {
			t.Fatalf("[%s] Expected a NOLOGIN role without SUPERUSER and BYPASSRLS, got super %v, bypass %v, login %v", role, isSuperuser, canBypass, canLogin)
		}
	}
}

func TestRLSOwnerRule(t *testing.T) {
	f := newRLSFixture(t)

	// guests
	err := tests.RunWithRLSRole(f.app, core.RLSAnonRole, "", func(txApp core.App) error {
		if total := countRows(t, txApp, "rls_posts"); total != 0 {
			t.Fatalf("Expected no visible posts for the guests, got %d", total)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// authenticated without claims
	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, "", func(txApp core.App) error {
		if total := countRows(t, txApp, "rls_posts"); total != 0 {
			t.Fatalf("Expected no visible posts without claims, got %d", total)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// own records only
	for _, s := range []struct {
		token    string
		owner    string
		expected int
	}{
		{f.token1, f.user1.Id, 2},
		{f.token2, f.user2.Id, 1},
	} {
		err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, s.token, func(txApp core.App) error {
			var owners []string
			if err := txApp.DB().Select("owner").From("rls_posts").Column(&owners); err != nil {
				t.Fatal(err)
			}
			if len(owners) != s.expected {
				t.Fatalf("Expected %d visible posts, got %d", s.expected, len(owners))
			}
			for _, owner := range owners {
				if owner != s.owner {
					t.Fatalf("Expected only the posts of %q, got a post of %q", s.owner, owner)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRLSRejectedTokens(t *testing.T) {
	f := newRLSFixture(t)

	// invalid signature
	invalid := f.token1[:len(f.token1)-2] + "xx"
	if invalid == f.token1 {
		invalid = f.token1[:len(f.token1)-2] + "yy"
	}

	err := tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, invalid, func(txApp core.App) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("Expected the invalid signature token to be rejected, got %v", err)
	}

	// the token key of the record has changed
	f.user1.RefreshTokenKey()
	if err := f.app.Save(f.user1); err != nil {
		t.Fatal(err)
	}

	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token1, func(txApp core.App) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("Expected the token with an old tokenKey to be rejected, got %v", err)
	}

	// a forged setting without set_claims is ignored
	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, "", func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("SELECT set_config('popgres.token', {:token}, true)").
			Bind(dbx.Params{"token": invalid}).
			Execute()
		if err != nil {
			t.Fatal(err)
		}
		if total := countRows(t, txApp, "rls_posts"); total != 0 {
			t.Fatalf("Expected no visible posts with a forged token, got %d", total)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRLSRelationRule(t *testing.T) {
	f := newRLSFixture(t)

	compiled, err := core.CompileRLS(f.app, f.related)
	if err != nil {
		t.Fatal(err)
	}
	if p := compiled.Policies[0]; p.Status != core.RLSStatusTranslated || p.Inline {
		t.Fatalf("Expected a translated function policy, got %q (inline %v, %s)", p.Status, p.Inline, p.Reason)
	}

	// verified user
	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token1, func(txApp core.App) error {
		if total := countRows(t, txApp, "rls_related"); total != 2 {
			t.Fatalf("Expected 2 visible records, got %d", total)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// unverified user
	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token2, func(txApp core.App) error {
		if total := countRows(t, txApp, "rls_related"); total != 0 {
			t.Fatalf("Expected no visible records for the unverified user, got %d", total)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRLSUntranslatableRuleDenies(t *testing.T) {
	f := newRLSFixture(t)

	collection, err := f.app.FindCollectionByNameOrId("rls_headers")
	if err != nil {
		t.Fatal(err)
	}

	compiled, err := core.CompileRLS(f.app, collection)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Status != core.RLSCollectionDenied {
		t.Fatalf("Expected the collection status %q, got %q", core.RLSCollectionDenied, compiled.Status)
	}
	if p := compiled.Policies[0]; p.Status != core.RLSStatusDenied || !strings.Contains(p.Reason, "@request.headers.x_token") {
		t.Fatalf("Expected a denied list policy with a reason, got %q (%s)", p.Status, p.Reason)
	}

	for _, token := range []string{"", f.token1} {
		role := core.RLSAnonRole
		if token != "" {
			role = core.RLSAuthenticatedRole
		}
		err = tests.RunWithRLSRole(f.app, role, token, func(txApp core.App) error {
			if total := countRows(t, txApp, "rls_headers"); total != 0 {
				t.Fatalf("Expected no visible records, got %d", total)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRLSPrivileges(t *testing.T) {
	f := newRLSFixture(t)

	scenarios := []struct {
		name  string
		query string
	}{
		{"hidden column", "SELECT [[secret]] FROM {{rls_posts}}"},
		{"system table _params", "SELECT 1 FROM {{_params}}"},
		{"system table _collections", "SELECT 1 FROM {{_collections}}"},
		{"system table _realtime_events", "SELECT 1 FROM {{_realtime_events}}"},
		{"system collection _superusers", "SELECT 1 FROM {{_superusers}}"},
		{"auth collection email", "SELECT [[email]] FROM {{users}}"},
		{"auth collection password", "SELECT [[password]] FROM {{users}}"},
		{"auth collection write", "UPDATE {{users}} SET [[verified]] = TRUE"},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			err := tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token1, func(txApp core.App) error {
				_, err := txApp.DB().NewQuery(s.query).Execute()
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("Expected permission denied, got %v", err)
			}
		})
	}
}

func TestRLSWrites(t *testing.T) {
	f := newRLSFixture(t)

	// create
	err := tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token1, func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("INSERT INTO {{rls_posts}} ([[id]], [[owner]], [[title]]) VALUES ('rlsown00000001', {:owner}, 'own')").
			Bind(dbx.Params{"owner": f.user1.Id}).
			Execute()
		return err
	})
	if err != nil {
		t.Fatalf("Expected the own record insert to succeed, got %v", err)
	}

	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token1, func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("INSERT INTO {{rls_posts}} ([[id]], [[owner]], [[title]]) VALUES ('rlsother000001', {:owner}, 'other')").
			Bind(dbx.Params{"owner": f.user2.Id}).
			Execute()
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("Expected the insert for another owner to be rejected, got %v", err)
	}

	// update and delete affect only the own records
	err = tests.RunWithRLSRole(f.app, core.RLSAuthenticatedRole, f.token1, func(txApp core.App) error {
		result, err := txApp.DB().NewQuery("UPDATE {{rls_posts}} SET [[title]] = 'changed'").Execute()
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 2 {
			t.Fatalf("Expected 2 updated records, got %d", n)
		}

		result, err = txApp.DB().NewQuery("DELETE FROM {{rls_posts}}").Execute()
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 2 {
			t.Fatalf("Expected 2 deleted records, got %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRLSStrictMode checks that in strict mode the API list and view requests
// are filtered by the RLS policies (replaced here with a policy that differs from the rule).
func TestRLSStrictMode(t *testing.T) {
	// the env variable overrides the disabled config value
	t.Setenv(core.RLSStrictEnv, "")

	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
				EncryptionEnv: "pb_test_env",
				RLSStrict:     strict,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Cleanup()

			collection := core.NewBaseCollection("rls_strict")
			collection.Fields.Add(&core.TextField{Name: "title"})
			collection.ListRule = types.Pointer("")
			collection.ViewRule = types.Pointer("")
			if err := app.Save(collection); err != nil {
				t.Fatal(err)
			}

			visible := core.NewRecord(collection)
			visible.Set("title", "visible")
			hidden := core.NewRecord(collection)
			hidden.Set("title", "hidden")
			if err := app.Save(visible); err != nil {
				t.Fatal(err)
			}
			if err := app.Save(hidden); err != nil {
				t.Fatal(err)
			}

			// a policy that differs from the API rule
			_, err = app.DB().NewQuery(`
				DROP POLICY "popgres_list" ON "rls_strict";
				DROP POLICY "popgres_view" ON "rls_strict";
				CREATE POLICY "popgres_list" ON "rls_strict" FOR SELECT TO popgres_api USING ("title" = 'visible');
				CREATE POLICY "popgres_view" ON "rls_strict" FOR SELECT TO popgres_api USING ("title" = 'visible');
			`).Execute()
			if err != nil {
				t.Fatal(err)
			}

			client := newAPIClient(t, app)

			list := client.mustDo(200, http.MethodGet, "/api/collections/rls_strict/records", nil)
			expectedTotal := 2.0
			if strict {
				expectedTotal = 1
			}
			if total := list["totalItems"]; total != expectedTotal {
				t.Fatalf("Expected %v listed records, got %v", expectedTotal, total)
			}

			expectedStatus := 200
			if strict {
				expectedStatus = 404
			}
			if status, body := client.do(http.MethodGet, "/api/collections/rls_strict/records/"+hidden.Id, nil); status != expectedStatus {
				t.Fatalf("Expected view status %d, got %d\n%s", expectedStatus, status, body)
			}
			client.mustDo(200, http.MethodGet, "/api/collections/rls_strict/records/"+visible.Id, nil)

			// the superusers bypass the strict mode
			superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
			if err != nil {
				t.Fatal(err)
			}
			client.token, err = superuser.NewAuthToken()
			if err != nil {
				t.Fatal(err)
			}
			list = client.mustDo(200, http.MethodGet, "/api/collections/rls_strict/records", nil)
			if total := list["totalItems"]; total != 2.0 {
				t.Fatalf("Expected 2 listed records for the superusers, got %v", total)
			}
		})
	}
}

// BenchmarkRLSQuery measures the cost of the policies (and of the auth token
// verification) of a single SELECT of the records of a user.
//
//	go test ./tests/e2e -run '^$' -bench RLSQuery
func BenchmarkRLSQuery(b *testing.B) {
	f := newRLSFixture(b)

	scenarios := []struct {
		name  string
		table string
		role  string
	}{
		{"owner", "rls_posts", ""},
		{"inline policy", "rls_posts", core.RLSAuthenticatedRole},
		{"function policy", "rls_related", core.RLSAuthenticatedRole},
	}

	for _, s := range scenarios {
		b.Run(s.name, func(b *testing.B) {
			exec := func(txApp core.App) error {
				for b.Loop() {
					var total int
					if err := txApp.DB().Select("count(*)").From(s.table).Row(&total); err != nil {
						return err
					}
				}
				return nil
			}

			var err error
			if s.role == "" {
				err = exec(f.app)
			} else {
				err = tests.RunWithRLSRole(f.app, s.role, f.token1, exec)
			}
			if err != nil {
				b.Fatal(err)
			}
		})
	}
}
