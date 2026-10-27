package core

import (
	"testing"
	"time"
)

func TestNewBaseAppPoolEnv(t *testing.T) {
	// note: not parallel because of t.Setenv

	scenarios := []struct {
		name             string
		config           BaseAppConfig
		maxConnsEnv      string
		lifetimeEnv      string
		expectedMaxConns int
		expectedIdle     int
		expectedLifetime time.Duration
	}{
		{"defaults", BaseAppConfig{}, "", "", DefaultDataMaxOpenConns, DefaultDataMaxOpenConns, DefaultConnMaxLifetime},
		{"env", BaseAppConfig{}, "7", "10m", 7, 7, 10 * time.Minute},
		{"env disabled lifetime", BaseAppConfig{}, "", "0", DefaultDataMaxOpenConns, DefaultDataMaxOpenConns, 0},
		{"invalid env", BaseAppConfig{}, "abc", "abc", DefaultDataMaxOpenConns, DefaultDataMaxOpenConns, DefaultConnMaxLifetime},
		{"config over env", BaseAppConfig{DataMaxOpenConns: 3, DataMaxIdleConns: 2, ConnMaxLifetime: time.Hour}, "7", "10m", 3, 2, time.Hour},
		{"negative config lifetime", BaseAppConfig{ConnMaxLifetime: -1}, "", "10m", DefaultDataMaxOpenConns, DefaultDataMaxOpenConns, -1},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			t.Setenv(DBMaxConnsEnv, s.maxConnsEnv)
			t.Setenv(DBConnMaxLifetimeEnv, s.lifetimeEnv)

			app := NewBaseApp(s.config)

			if app.config.DataMaxOpenConns != s.expectedMaxConns {
				t.Fatalf("Expected DataMaxOpenConns %d, got %d", s.expectedMaxConns, app.config.DataMaxOpenConns)
			}
			if app.config.DataMaxIdleConns != s.expectedIdle {
				t.Fatalf("Expected DataMaxIdleConns %d, got %d", s.expectedIdle, app.config.DataMaxIdleConns)
			}
			if app.config.ConnMaxLifetime != s.expectedLifetime {
				t.Fatalf("Expected ConnMaxLifetime %s, got %s", s.expectedLifetime, app.config.ConnMaxLifetime)
			}
		})
	}
}
