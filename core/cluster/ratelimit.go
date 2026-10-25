package cluster

import (
	"time"

	"github.com/pocketbase/dbx"
)

// RateLimitsTable is the UNLOGGED table of the shared rate limit counters
// (created by the system migrations).
const RateLimitsTable = "_rate_limits"

// RateLimitHit registers a hit of the specified rate limit key and reports
// whether it is allowed (aka. the total hits in the current window are <= maxAllowed).
//
// It implements the same fixed window strategy as the in-memory app rate
// limiter: the window starts with the first hit and resets after interval.
func (c *Cluster) RateLimitHit(key string, maxAllowed int, interval time.Duration) (bool, error) {
	db, err := c.kv.builder()
	if err != nil {
		return false, err
	}

	var hits int

	err = db.NewQuery(`
		INSERT INTO {{_rate_limits}} ([[key]], [[window_start]], [[hits]], [[expires_at]])
		VALUES ({:key}, now(), 1, now() + make_interval(secs => {:interval}))
		ON CONFLICT ([[key]]) DO UPDATE SET
			[[hits]] = CASE WHEN {{_rate_limits}}.[[expires_at]] <= now() THEN 1 ELSE {{_rate_limits}}.[[hits]] + 1 END,
			[[window_start]] = CASE WHEN {{_rate_limits}}.[[expires_at]] <= now() THEN now() ELSE {{_rate_limits}}.[[window_start]] END,
			[[expires_at]] = CASE WHEN {{_rate_limits}}.[[expires_at]] <= now() THEN EXCLUDED.[[expires_at]] ELSE {{_rate_limits}}.[[expires_at]] END
		RETURNING [[hits]]
	`).Bind(dbx.Params{
		"key":      key,
		"interval": interval.Seconds(),
	}).Row(&hits)
	if err != nil {
		return false, err
	}

	return hits <= maxAllowed, nil
}
