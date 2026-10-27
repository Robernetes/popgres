package apis

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/router"
	"github.com/pocketbase/dbx"
)

// bindHealthApi registers the health api endpoint.
func bindHealthApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	subGroup := rg.Group("/health")
	subGroup.GET("", healthCheck)
}

// readyCheckTimeout is the max duration of the readiness database check.
const readyCheckTimeout = 3 * time.Second

// healthCheck returns a 200 OK response if the server is healthy.
//
// popgres: with the "ready" query parameter (eg. for a load balancer or a
// readiness probe) it returns a 503 response if the database is not reachable
// or if the cluster LISTEN connection of the node is not connected.
func healthCheck(e *core.RequestEvent) error {
	resp := struct {
		Message string         `json:"message"`
		Code    int            `json:"code"`
		Data    map[string]any `json:"data"`
	}{
		Code:    http.StatusOK,
		Message: "API is healthy.",
	}

	var ready *readiness
	if e.Request.URL.Query().Has("ready") {
		ready = readinessChecks(e)
		if !ready.Database || !ready.Cluster {
			resp.Code = http.StatusServiceUnavailable
			resp.Message = "API is not ready."
		}
	}

	// @todo evaluate whether it is worth removing the extra info from the health endpoint
	if e.HasSuperuserAuth() {
		resp.Data = make(map[string]any, 3)
		resp.Data["canBackup"] = !core.IsBackupActive(e.App)
		resp.Data["realIP"] = e.RealIP()

		// loosely check if behind a reverse proxy
		// (usually used in the dashboard to remind superusers in case deployed behind reverse-proxy)
		possibleProxyHeader := ""
		headersToCheck := append(
			slices.Clone(e.App.Settings().TrustedProxy.Headers),
			// common proxy headers
			"CF-Connecting-IP", "Fly-Client-IP", "X-Forwarded-For",
		)
		for _, header := range headersToCheck {
			if e.Request.Header.Get(header) != "" {
				possibleProxyHeader = header
				break
			}
		}
		resp.Data["possibleProxyHeader"] = possibleProxyHeader

		// popgres: the realtime source and the replication slot metrics (if any)
		realtime := map[string]any{
			"source": e.App.Cluster().RealtimeSource(),
			"slot":   nil,
		}
		if metrics, err := e.App.Cluster().SlotMetrics(e.Request.Context()); err == nil && metrics != nil {
			realtime["slot"] = metrics
			realtime["retainedWALWarning"] = metrics.RetainedBytes > cluster.RetainedWALWarningSize
		}
		resp.Data["realtime"] = realtime

		// popgres: the cluster node state and counters
		resp.Data["cluster"] = map[string]any{
			"nodeId":            e.App.Cluster().NodeId(),
			"enabled":           e.App.Cluster().Enabled(),
			"connected":         e.App.Cluster().Connected(),
			"replicationLeader": e.App.Cluster().IsReplicationLeader(),
			"droppedMessages":   e.App.Cluster().DroppedMessages(),
			"invalidMessages":   e.App.Cluster().InvalidMessages(),
			"realtimeFailures":  e.App.Cluster().RealtimeFailures(),
		}

		// popgres: the main database pool stats
		if db, ok := e.App.DB().(*dbx.DB); ok {
			stats := db.DB().Stats()
			resp.Data["dbPool"] = map[string]any{
				"maxOpenConns":   stats.MaxOpenConnections,
				"openConns":      stats.OpenConnections,
				"inUse":          stats.InUse,
				"idle":           stats.Idle,
				"waitCount":      stats.WaitCount,
				"waitDurationMs": stats.WaitDuration.Milliseconds(),
			}
		}
	} else {
		resp.Data = map[string]any{} // ensure that it is returned as object
	}

	if ready != nil {
		resp.Data["ready"] = ready
	}

	return e.JSON(resp.Code, resp)
}

// readiness is the state of the dependencies required to serve the requests.
type readiness struct {
	Database bool `json:"database"`

	// the node doesn't receive the collections and settings changes
	// of the other nodes while its LISTEN connection is down
	Cluster bool `json:"cluster"`
}

func readinessChecks(e *core.RequestEvent) *readiness {
	ctx, cancel := context.WithTimeout(e.Request.Context(), readyCheckTimeout)
	defer cancel()

	_, err := e.App.DB().NewQuery("SELECT 1").WithContext(ctx).Execute()

	return &readiness{
		Database: err == nil,
		Cluster:  e.App.Cluster().Connected(),
	}
}
