package core

import (
	"log/slog"
	"time"
)

// clusterCleanupJobId is the id of the system cron job that deletes
// the expired rows of the cluster tables.
const clusterCleanupJobId = "__popgresClusterCleanup__"

// registerClusterHooks registers the cron and cleanup hooks of the
// app instances (nodes) that share the same database.
func (app *BaseApp) registerClusterHooks() {
	// run each scheduled cron job only once per schedule slot across all nodes
	app.cron.SetClaimFunc(func(jobId string, slot time.Time) bool {
		claimed, err := app.cluster.ClaimCronRun(jobId, slot)
		if err != nil {
			app.Logger().Warn(
				"Failed to claim the cron job execution",
				slog.String("jobId", jobId),
				slog.String("error", err.Error()),
			)
			return false
		}

		return claimed
	})

	app.cron.Add(clusterCleanupJobId, "*/5 * * * *", func() {
		if err := app.cluster.DeleteExpired(); err != nil {
			app.Logger().Warn("Failed to delete the expired cluster rows", slog.String("error", err.Error()))
		}
	})
}
