package apis_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

// waitClusterReady waits for the first LISTEN connection of the test app cluster.
func waitClusterReady(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
	select {
	case <-app.Cluster().Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("the cluster LISTEN connection is not ready")
	}
}

func TestHealthAPI(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:           "GET health status (guest)",
			Method:         http.MethodGet, // automatically matches also HEAD as a side-effect of the Go std mux
			URL:            "/api/health",
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"code":200`,
				`"data":{}`,
			},
			NotExpectedContent: []string{
				"canBackup",
				"realIP",
				"possibleProxyHeader",
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "GET health status (regular user)",
			Method: http.MethodGet,
			URL:    "/api/health",
			Headers: map[string]string{
				"Authorization": "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6IjRxMXhsY2xtZmxva3UzMyIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoiX3BiX3VzZXJzX2F1dGhfIiwiZXhwIjoyNTI0NjA0NDYxLCJyZWZyZXNoYWJsZSI6dHJ1ZX0.ZT3F0Z3iM-xbGgSG3LEKiEzHrPHr8t8IuHLZGGNuxLo",
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"code":200`,
				`"data":{}`,
			},
			NotExpectedContent: []string{
				"canBackup",
				"realIP",
				"possibleProxyHeader",
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "GET health status (superuser)",
			Method: http.MethodGet,
			URL:    "/api/health",
			Headers: map[string]string{
				"Authorization": "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY",
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"code":200`,
				`"data":{`,
				`"canBackup":true`,
				`"realIP"`,
				`"possibleProxyHeader"`,
				`"cluster":{`,
				`"connected":`,
				`"droppedMessages":0`,
				`"dbPool":{`,
				`"maxOpenConns":`,
			},
			NotExpectedContent: []string{
				`"ready"`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:           "GET health readiness (guest)",
			Method:         http.MethodGet,
			URL:            "/api/health?ready",
			BeforeTestFunc: waitClusterReady,
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"code":200`,
				`"data":{"ready":{"database":true,"cluster":true}}`,
			},
			NotExpectedContent: []string{
				"canBackup",
				"dbPool",
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "GET health readiness (superuser)",
			Method: http.MethodGet,
			URL:    "/api/health?ready=1",
			Headers: map[string]string{
				"Authorization": "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY",
			},
			BeforeTestFunc: waitClusterReady,
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"code":200`,
				`"canBackup":true`,
				`"ready":{"database":true,"cluster":true}`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "GET health readiness with a disconnected cluster node",
			Method: http.MethodGet,
			URL:    "/api/health?ready",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				app.Cluster().Stop()
			},
			ExpectedStatus: 503,
			ExpectedContent: []string{
				`"code":503`,
				`"message":"API is not ready."`,
				`"data":{"ready":{"database":true,"cluster":false}}`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
		{
			Name:   "GET health without readiness and a disconnected cluster node",
			Method: http.MethodGet,
			URL:    "/api/health",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				app.Cluster().Stop()
			},
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"code":200`,
				`"data":{}`,
			},
			ExpectedEvents: map[string]int{"*": 0},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
