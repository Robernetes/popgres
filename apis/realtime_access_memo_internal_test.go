package apis

import (
	"testing"

	"github.com/Robernetes/popgres/core"
)

func TestMemoizeRealtimeGuestAccessCheck(t *testing.T) {
	t.Parallel()

	collection := core.NewBaseCollection("test")
	collection.Id = "test_id"

	record1 := core.NewRecord(collection)
	record1.Id = "r1"
	record2 := core.NewRecord(collection)
	record2.Id = "r2"

	auth := core.NewRecord(core.NewAuthCollection("users"))
	auth.Id = "u1"

	ruleA := "status = 'public'"
	ruleB := "status = 'draft'"

	calls := 0
	canAccess := memoizeRealtimeGuestAccessCheck(func(record *core.Record, requestInfo *core.RequestInfo, rule *string) bool {
		calls++
		return record.Id == "r1"
	})

	guest := func(filter string) *core.RequestInfo {
		query := map[string]string{}
		if filter != "" {
			query["filter"] = filter
		}
		return &core.RequestInfo{Method: "GET", Query: query, Headers: map[string]string{}}
	}

	scenarios := []struct {
		name          string
		record        *core.Record
		requestInfo   *core.RequestInfo
		rule          *string
		expected      bool
		expectedCalls int
	}{
		{"first guest", record1, guest(""), &ruleA, true, 1},
		{"same guest options", record1, guest(""), &ruleA, true, 1},
		{"same rule value", record1, guest(""), new("status = 'public'"), true, 1},
		{"different rule", record1, guest(""), &ruleB, true, 2},
		{"nil rule", record1, guest(""), nil, true, 3},
		{"different filter", record1, guest("a > 1"), &ruleA, true, 4},
		{"same filter", record1, guest("a > 1"), &ruleA, true, 4},
		{"different record", record2, guest(""), &ruleA, false, 5},
		{"same different record", record2, guest(""), &ruleA, false, 5},
		{"auth client", record1, &core.RequestInfo{Method: "GET", Auth: auth}, &ruleA, true, 6},
		{"same auth client", record1, &core.RequestInfo{Method: "GET", Auth: auth}, &ruleA, true, 7},
	}

	for _, s := range scenarios {
		if result := canAccess(s.record, s.requestInfo, s.rule); result != s.expected {
			t.Fatalf("[%s] Expected %v, got %v", s.name, s.expected, result)
		}
		if calls != s.expectedCalls {
			t.Fatalf("[%s] Expected %d calls, got %d", s.name, s.expectedCalls, calls)
		}
	}
}
