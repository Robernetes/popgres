package cluster_test

import (
	"slices"
	"testing"

	"github.com/Robernetes/popgres/core/cluster"
)

func TestEventsDeduplicator(t *testing.T) {
	d := cluster.NewEventsDeduplicator(3)

	if got := d.Filter([]string{"b", "a", "", "a"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Expected [a b], got %v", got)
	}

	if got := d.Filter([]string{"a", "c"}); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("Expected [c], got %v", got)
	}

	// the oldest id ("b") is removed when the max is exceeded
	if got := d.Filter([]string{"d"}); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("Expected [d], got %v", got)
	}
	if got := d.Filter([]string{"b"}); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("Expected the oldest id to be forgotten, got %v", got)
	}

	// the forgotten ids are received again
	d.Forget([]string{"d", "missing"})
	if got := d.Filter([]string{"c", "d"}); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("Expected [d] after Forget, got %v", got)
	}
}
