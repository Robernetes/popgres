package apis_test

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/security"
	"github.com/spf13/cast"
)

// popgres
func TestRLSToken(t *testing.T) {
	t.Parallel()

	const userToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6IjRxMXhsY2xtZmxva3UzMyIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoiX3BiX3VzZXJzX2F1dGhfIiwiZXhwIjoyNTI0NjA0NDYxLCJyZWZyZXNoYWJsZSI6dHJ1ZX0.ZT3F0Z3iM-xbGgSG3LEKiEzHrPHr8t8IuHLZGGNuxLo"

	// checkToken checks the returned RLS token (type, duration, record)
	// and that it is not accepted as an API auth token
	checkToken := func(expectedDuration time.Duration) func(t testing.TB, app *tests.TestApp, res *http.Response) {
		return func(t testing.TB, app *tests.TestApp, res *http.Response) {
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}

			body := struct {
				Token string `json:"token"`
			}{}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}

			claims, err := security.ParseUnverifiedJWT(body.Token)
			if err != nil {
				t.Fatal(err)
			}
			if claims[core.TokenClaimType] != core.TokenTypeRLS || claims[core.TokenClaimId] != "4q1xlclmfloku33" {
				t.Fatalf("Unexpected RLS token claims %v", claims)
			}

			duration := time.Until(time.Unix(cast.ToInt64(claims["exp"]), 0))
			if math.Abs(duration.Seconds()-expectedDuration.Seconds()) > 10 {
				t.Fatalf("Expected a token duration of %v, got %v", expectedDuration, duration)
			}

			// the RLS token is valid but only for its type
			if _, err := app.FindAuthRecordByToken(body.Token, core.TokenTypeRLS); err != nil {
				t.Fatalf("Expected a valid RLS token, got %v", err)
			}
			if _, err := app.FindAuthRecordByToken(body.Token, core.TokenTypeAuth); err == nil {
				t.Fatal("Expected the RLS token to be rejected as an API auth token")
			}
		}
	}

	scenarios := []tests.ApiScenario{
		{
			Name:            "unauthorized",
			Method:          http.MethodPost,
			URL:             "/api/rls/token",
			ExpectedStatus:  401,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "regular user (default duration)",
			Method:          http.MethodPost,
			URL:             "/api/rls/token",
			Headers:         map[string]string{"Authorization": userToken},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"`},
			ExpectedEvents:  map[string]int{"*": 0},
			AfterTestFunc:   checkToken(core.DefaultRLSTokenDuration),
		},
		{
			Name:            "regular user (custom duration)",
			Method:          http.MethodPost,
			URL:             "/api/rls/token",
			Body:            strings.NewReader(`{"duration":3600}`),
			Headers:         map[string]string{"Authorization": userToken},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"`},
			ExpectedEvents:  map[string]int{"*": 0},
			AfterTestFunc:   checkToken(time.Hour),
		},
		{
			Name:            "invalid duration",
			Method:          http.MethodPost,
			URL:             "/api/rls/token",
			Body:            strings.NewReader(`{"duration":86401}`),
			Headers:         map[string]string{"Authorization": userToken},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"duration":{"code":"validation_max_less_equal_than_required"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "too short duration",
			Method:          http.MethodPost,
			URL:             "/api/rls/token",
			Body:            strings.NewReader(`{"duration":30}`),
			Headers:         map[string]string{"Authorization": userToken},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"duration":{"code":"validation_min_greater_equal_than_required"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
