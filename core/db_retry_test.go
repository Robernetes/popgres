package core

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestGetDefaultRetryInterval(t *testing.T) {
	t.Parallel()

	if i := getDefaultRetryInterval(-1); i.Milliseconds() != 1000 {
		t.Fatalf("Expected 1000ms, got %v", i)
	}

	if i := getDefaultRetryInterval(999); i.Milliseconds() != 1000 {
		t.Fatalf("Expected 1000ms, got %v", i)
	}

	if i := getDefaultRetryInterval(3); i.Milliseconds() != 200 {
		t.Fatalf("Expected 500ms, got %v", i)
	}
}

func TestBaseLockRetry(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		err              error
		failUntilAttempt int
		expectedAttempts int
	}{
		{nil, 3, 1},
		{errors.New("test"), 3, 1},
		{errors.New("database is locked"), 3, 1},
		{&pgconn.PgError{Code: "23505"}, 3, 1},
		{&pgconn.PgError{Code: "40001"}, 3, 3},
		{&pgconn.PgError{Code: "40P01"}, 3, 3},
		{fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "55P03"}), 3, 3},
		{errors.New("ERROR: could not serialize access (SQLSTATE 40001)"), 3, 3},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v", i, s.err), func(t *testing.T) {
			lastAttempt := 0

			err := baseLockRetry(func(attempt int) error {
				lastAttempt = attempt

				if attempt < s.failUntilAttempt {
					return s.err
				}

				return nil
			}, s.failUntilAttempt+2)

			if lastAttempt != s.expectedAttempts {
				t.Errorf("Expected lastAttempt to be %d, got %d", s.expectedAttempts, lastAttempt)
			}

			if s.failUntilAttempt == s.expectedAttempts && err != nil {
				t.Fatalf("Expected nil, got err %v", err)
			}

			if s.failUntilAttempt != s.expectedAttempts && s.err != nil && err == nil {
				t.Fatalf("Expected error %q, got nil", s.err)
			}
		})
	}
}

func TestBaseLockRetryInFailedTransaction(t *testing.T) {
	t.Parallel()

	original := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}

	var attempts int
	err := baseLockRetry(func(attempt int) error {
		attempts = attempt
		if attempt == 1 {
			return original
		}
		return &pgconn.PgError{Code: "25P02", Message: "current transaction is aborted"}
	}, 5)

	if attempts != 2 {
		t.Fatalf("Expected 2 attempts, got %d", attempts)
	}

	if err != original {
		t.Fatalf("Expected the original error, got %v", err)
	}
}

func TestSQLState(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		err      error
		expected string
	}{
		{nil, ""},
		{errors.New("test"), ""},
		{&pgconn.PgError{Code: "23505"}, "23505"},
		{fmt.Errorf("a: %w", &pgconn.PgError{Code: "40P01"}), "40P01"},
		{errors.New("ERROR: x (SQLSTATE 22007); failed query: SELECT 1"), "22007"},
	}

	for i, s := range scenarios {
		if v := sqlState(s.err); v != s.expected {
			t.Errorf("[%d] Expected %q, got %q", i, s.expected, v)
		}
	}
}
