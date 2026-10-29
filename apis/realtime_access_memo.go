package apis

import (
	"encoding/json/v2"
	"sync"

	"github.com/Robernetes/popgres/core"
)

// popgres: the guests realtime access checks of a record broadcast are executed once
// for the same inputs (instead of one query per subscribed guest with a non-public rule).

type realtimeAccessCheck func(record *core.Record, requestInfo *core.RequestInfo, rule *string) bool

// memoizeRealtimeGuestAccessCheck wraps canAccess so that its result is reused for
// the guests with the same subscription query and headers, the same rule and record.
//
// The checks of the authenticated clients are not memoized because their
// result could depend on the client auth record state.
func memoizeRealtimeGuestAccessCheck(canAccess realtimeAccessCheck) realtimeAccessCheck {
	var mu sync.Mutex
	results := map[string]bool{}

	return func(record *core.Record, requestInfo *core.RequestInfo, rule *string) bool {
		if requestInfo.Auth != nil {
			return canAccess(record, requestInfo, rule)
		}

		key, ok := realtimeGuestAccessCheckKey(record, requestInfo, rule)
		if !ok {
			return canAccess(record, requestInfo, rule)
		}

		mu.Lock()
		result, ok := results[key]
		mu.Unlock()
		if ok {
			return result
		}

		// note: a concurrent check with the same key could be executed twice
		// (it has the same result, so it is not worth blocking the other checks)
		result = canAccess(record, requestInfo, rule)

		mu.Lock()
		results[key] = result
		mu.Unlock()

		return result
	}
}

func realtimeGuestAccessCheckKey(record *core.Record, requestInfo *core.RequestInfo, rule *string) (string, bool) {
	collection := record.Collection()
	if collection == nil {
		return "", false
	}

	key := struct {
		Collection string            `json:"c"`
		Record     string            `json:"r"`
		Rule       *string           `json:"rule"`
		Method     string            `json:"m"`
		Query      map[string]string `json:"q"`
		Headers    map[string]string `json:"h"`
	}{
		Collection: collection.Id,
		Record:     record.Id,
		Rule:       rule,
		Method:     requestInfo.Method,
		Query:      requestInfo.Query,
		Headers:    requestInfo.Headers,
	}

	raw, err := json.Marshal(key, json.Deterministic(true))
	if err != nil {
		return "", false
	}

	return string(raw), true
}
