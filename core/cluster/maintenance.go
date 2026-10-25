package cluster

import (
	"errors"
	"time"

	"github.com/pocketbase/dbx"
)

// CronRunsTable is the UNLOGGED table of the claimed cron job executions
// (created by the system migrations).
const CronRunsTable = "_cron_runs"

const (
	// cronRunsMaxAge is the max age of the claimed cron job executions.
	cronRunsMaxAge = 24 * time.Hour

	// RealtimeEventsMaxAge is the max age of the published realtime events.
	RealtimeEventsMaxAge = 5 * time.Minute
)

// ClaimCronRun claims the execution of a cron job for the specified
// scheduled slot and reports whether the current node should run it.
//
// Only one node can claim the same job and slot.
func (c *Cluster) ClaimCronRun(jobId string, slot time.Time) (bool, error) {
	db, err := c.kv.builder()
	if err != nil {
		return false, err
	}

	result, err := db.NewQuery(`
		INSERT INTO {{_cron_runs}} ([[job_id]], [[slot]], [[node_id]])
		VALUES ({:jobId}, {:slot}, {:nodeId})
		ON CONFLICT DO NOTHING
	`).Bind(dbx.Params{
		"jobId":  jobId,
		"slot":   slot.UTC(),
		"nodeId": c.nodeId,
	}).Execute()
	if err != nil {
		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected == 1, nil
}

// DeleteExpired deletes the expired rows of the cluster tables
// (the old cron runs, the expired keys and rate limits, and the old realtime events).
func (c *Cluster) DeleteExpired() error {
	db, err := c.kv.builder()
	if err != nil {
		return err
	}

	queries := []*dbx.Query{
		db.NewQuery("DELETE FROM {{_cron_runs}} WHERE [[created]] < now() - make_interval(secs => {:maxAge})").
			Bind(dbx.Params{"maxAge": cronRunsMaxAge.Seconds()}),
		db.NewQuery("DELETE FROM {{_kv_ttl}} WHERE [[expires_at]] <= now()"),
		db.NewQuery("DELETE FROM {{_rate_limits}} WHERE [[expires_at]] <= now()"),
		db.NewQuery("DELETE FROM {{_realtime_events}} WHERE [[created]] < now() - make_interval(secs => {:maxAge})").
			Bind(dbx.Params{"maxAge": RealtimeEventsMaxAge.Seconds()}),
	}

	var errs []error
	for _, q := range queries {
		if _, err := q.Execute(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
