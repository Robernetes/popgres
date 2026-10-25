package apis_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

func TestServeClusterModeRequirements(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{ClusterMode: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	// S3 is required
	err = apis.Serve(app, apis.ServeConfig{HttpAddr: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "S3") {
		t.Fatalf("Expected S3 required error, got %v", err)
	}

	// HTTPS (autocert) is not supported
	app.Settings().S3.Enabled = true
	err = apis.Serve(app, apis.ServeConfig{HttpsAddr: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("Expected HTTPS not supported error, got %v", err)
	}
}

func TestSettingsSetClusterModeS3(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{ClusterMode: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"s3":{"enabled":false}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "validation_cluster_s3_required") {
		t.Fatalf("Expected S3 required validation error, got %d: %s", rec.Code, rec.Body.String())
	}
}
