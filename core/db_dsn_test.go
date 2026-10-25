package core

import "testing"

func TestDSNWithRuntimeParam(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		dsn      string
		expected string
	}{
		{
			"postgres://user:pass@localhost:5432/db?sslmode=disable",
			"postgres://user:pass@localhost:5432/db?search_path=_aux&sslmode=disable",
		},
		{
			"postgresql://localhost/db",
			"postgresql://localhost/db?search_path=_aux",
		},
		{
			"postgres://localhost/db?search_path=public",
			"postgres://localhost/db?search_path=_aux",
		},
		{
			"host=localhost dbname=db ",
			"host=localhost dbname=db search_path='_aux'",
		},
	}

	for _, s := range scenarios {
		t.Run(s.dsn, func(t *testing.T) {
			result, err := dsnWithRuntimeParam(s.dsn, "search_path", "_aux")
			if err != nil {
				t.Fatal(err)
			}

			if result != s.expected {
				t.Fatalf("Expected\n%s\ngot\n%s", s.expected, result)
			}
		})
	}
}
