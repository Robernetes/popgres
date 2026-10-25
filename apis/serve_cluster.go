package apis

import (
	"errors"

	"github.com/Robernetes/popgres/core"
)

// checkClusterServeConfig checks the cluster mode (POPGRES_CLUSTER) requirements.
func checkClusterServeConfig(app core.App, config ServeConfig) error {
	if !app.Cluster().Enabled() {
		return nil
	}

	if config.HttpsAddr != "" {
		return errors.New("the HTTPS server (autocert) is not supported in cluster mode: terminate TLS at the load balancer and use only the HTTP address")
	}

	if !app.Settings().S3.Enabled {
		return errors.New("the S3 file storage is required in cluster mode: enable it in the settings (eg. from a single instance without POPGRES_CLUSTER) and start again")
	}

	if !app.Settings().Backups.S3.Enabled {
		app.Logger().Warn("Cluster mode: the backups are stored on the local disk of the node that created them (enable the backups S3 storage to share them)")
	}

	return nil
}
