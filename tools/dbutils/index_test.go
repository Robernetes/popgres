package dbutils_test

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/tools/dbutils"
)

func TestParseIndex(t *testing.T) {
	scenarios := []struct {
		name     string
		index    string
		expected dbutils.Index
	}{
		{
			"invalid",
			`invalid`,
			dbutils.Index{},
		},
		{
			"no names",
			`create index on ()`,
			dbutils.Index{},
		},
		{
			"invalid index name",
			`create index a.b.c on ()`,
			dbutils.Index{},
		},
		{
			"simple (multiple spaces as prefix, suffix, between the table and columns list)",
			`  create index indexname on tablename   (col1)  `,
			dbutils.Index{
				IndexName: "indexname",
				TableName: "tablename",
				Columns: []dbutils.IndexColumn{
					{Name: "col1"},
				},
			},
		},
		{
			"simple (no space between the table and the columns list)",
			`create index indexname on tablename(col1)`,
			dbutils.Index{
				IndexName: "indexname",
				TableName: "tablename",
				Columns: []dbutils.IndexColumn{
					{Name: "col1"},
				},
			},
		},
		{
			"all fields",
			`CREATE UNIQUE INDEX IF NOT EXISTS "schemaname".[indexname] on 'tablename' (
				col0,
				` + "`" + `col1` + "`" + `,
				json_extract("col2_multiline",` + "\n" + ` "$.a") asc,
				"col3" collate NOCASE,
				"col4" collate RTRIM desc
			) where cast(test1 as ` + "\n" + `int) = 1 and test2 != ''`,
			dbutils.Index{
				Unique:     true,
				Optional:   true,
				SchemaName: "schemaname",
				IndexName:  "indexname",
				TableName:  "tablename",
				Columns: []dbutils.IndexColumn{
					{Name: "col0"},
					{Name: "col1"},
					{Name: `json_extract("col2_multiline",` + "\n" + ` "$.a")`, Sort: "ASC"},
					{Name: `col3`, Collate: "NOCASE"},
					{Name: `col4`, Collate: "RTRIM", Sort: "DESC"},
				},
				Where: "cast(test1 as \nint) = 1 and test2 != ''",
			},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := dbutils.ParseIndex(s.index)

			resultRaw, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("Failed to marshalize parse result: %v", err)
			}

			expectedRaw, err := json.Marshal(s.expected)
			if err != nil {
				t.Fatalf("Failed to marshalize expected index: %v", err)
			}

			if !bytes.Equal(resultRaw, expectedRaw) {
				t.Errorf("Expected \n%s \ngot \n%s", expectedRaw, resultRaw)
			}
		})
	}
}

func TestIndexIsValid(t *testing.T) {
	scenarios := []struct {
		name     string
		index    dbutils.Index
		expected bool
	}{
		{
			"empty",
			dbutils.Index{},
			false,
		},
		{
			"no index name",
			dbutils.Index{
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			false,
		},
		{
			"no table name",
			dbutils.Index{
				IndexName: "index",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			false,
		},
		{
			"no columns",
			dbutils.Index{
				IndexName: "index",
				TableName: "table",
			},
			false,
		},
		{
			"min valid",
			dbutils.Index{
				IndexName: "index",
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			true,
		},
		{
			"all fields",
			dbutils.Index{
				Optional:   true,
				Unique:     true,
				SchemaName: "schema",
				IndexName:  "index",
				TableName:  "table",
				Columns:    []dbutils.IndexColumn{{Name: "col"}},
				Where:      "test = 1 OR test = 2",
			},
			true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.index.IsValid()
			if result != s.expected {
				t.Fatalf("Expected %v, got %v", s.expected, result)
			}
		})
	}
}

func TestIndexBuild(t *testing.T) {
	scenarios := []struct {
		name     string
		index    dbutils.Index
		expected string
	}{
		{
			"empty",
			dbutils.Index{},
			"",
		},
		{
			"no index name",
			dbutils.Index{
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			"",
		},
		{
			"no table name",
			dbutils.Index{
				IndexName: "index",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			"",
		},
		{
			"no columns",
			dbutils.Index{
				IndexName: "index",
				TableName: "table",
			},
			"",
		},
		{
			"min valid",
			dbutils.Index{
				IndexName: "index",
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			"CREATE INDEX `index` ON `table` (`col`)",
		},
		{
			"all fields",
			dbutils.Index{
				Optional:   true,
				Unique:     true,
				SchemaName: "schema",
				IndexName:  "index",
				TableName:  "table",
				Columns: []dbutils.IndexColumn{
					{Name: "col1", Collate: "NOCASE", Sort: "asc"},
					{Name: "col2", Sort: "desc"},
					{Name: `json_extract("col3", "$.a")`, Collate: "NOCASE"},
				},
				Where: "test = 1 OR test = 2",
			},
			"CREATE UNIQUE INDEX IF NOT EXISTS `schema`.`index` ON `table` (\n  `col1` COLLATE NOCASE ASC,\n  `col2` DESC,\n  " + `json_extract("col3", "$.a")` + " COLLATE NOCASE\n) WHERE test = 1 OR test = 2",
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.index.Build()
			if result != s.expected {
				t.Fatalf("Expected \n%v \ngot \n%v", s.expected, result)
			}
		})
	}
}

func TestHasSingleColumnUniqueIndex(t *testing.T) {
	scenarios := []struct {
		name     string
		column   string
		indexes  []string
		expected bool
	}{
		{
			"empty indexes",
			"test",
			nil,
			false,
		},
		{
			"empty column",
			"",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`)",
			},
			false,
		},
		{
			"mismatched column",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test2`)",
			},
			false,
		},
		{
			"non unique index",
			"test",
			[]string{
				"CREATE INDEX `index1` ON `example` (`test`)",
			},
			false,
		},
		{
			"matching columnd and unique index",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`)",
			},
			true,
		},
		{
			"multiple columns",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`, `test2`)",
			},
			false,
		},
		{
			"multiple indexes",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`, `test2`)",
				"CREATE UNIQUE INDEX `index2` ON `example` (`test`)",
			},
			true,
		},
		{
			"partial unique index",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index` ON `example` (`test`) where test != ''",
			},
			true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := dbutils.HasSingleColumnUniqueIndex(s.column, s.indexes)
			if result != s.expected {
				t.Fatalf("Expected %v got %v", s.expected, result)
			}
		})
	}
}

func TestFindSingleColumnUniqueIndex(t *testing.T) {
	scenarios := []struct {
		name     string
		column   string
		indexes  []string
		expected bool
	}{
		{
			"empty indexes",
			"test",
			nil,
			false,
		},
		{
			"empty column",
			"",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`)",
			},
			false,
		},
		{
			"mismatched column",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test2`)",
			},
			false,
		},
		{
			"non unique index",
			"test",
			[]string{
				"CREATE INDEX `index1` ON `example` (`test`)",
			},
			false,
		},
		{
			"matching columnd and unique index",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`)",
			},
			true,
		},
		{
			"multiple columns",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`, `test2`)",
			},
			false,
		},
		{
			"multiple indexes",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index1` ON `example` (`test`, `test2`)",
				"CREATE UNIQUE INDEX `index2` ON `example` (`test`)",
			},
			true,
		},
		{
			"partial unique index",
			"test",
			[]string{
				"CREATE UNIQUE INDEX `index` ON `example` (`test`) where test != ''",
			},
			true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			index, exists := dbutils.FindSingleColumnUniqueIndex(s.indexes, s.column)
			if exists != s.expected {
				t.Fatalf("Expected exists %v got %v", s.expected, exists)
			}

			if !exists && len(index.Columns) > 0 {
				t.Fatal("Expected index.Columns to be empty")
			}

			if exists && !strings.EqualFold(index.Columns[0].Name, s.column) {
				t.Fatalf("Expected to find column %q in %v", s.column, index)
			}
		})
	}
}

func TestIndexBuildPostgres(t *testing.T) {
	scenarios := []struct {
		name     string
		index    dbutils.Index
		expected string
	}{
		{
			"empty",
			dbutils.Index{},
			"",
		},
		{
			"no index name",
			dbutils.Index{
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			"",
		},
		{
			"no table name",
			dbutils.Index{
				IndexName: "index",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			"",
		},
		{
			"no columns",
			dbutils.Index{
				IndexName: "index",
				TableName: "table",
			},
			"",
		},
		{
			"min valid",
			dbutils.Index{
				IndexName: "index",
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: "col"}},
			},
			`CREATE INDEX "index" ON "table" ("col")`,
		},
		{
			"all fields",
			dbutils.Index{
				Optional:   true,
				Unique:     true,
				SchemaName: "schema",
				IndexName:  "index",
				TableName:  "table",
				Columns: []dbutils.IndexColumn{
					{Name: "col1", Collate: "NOCASE", Sort: "asc"},
					{Name: "col2", Sort: "desc"},
					{Name: "(`col3` ->> 'a')", Collate: "NOCASE"},
					{Name: "col4", Collate: "BINARY"},
					{Name: "col5", Collate: "C"},
				},
				Where: "test = 1 OR test = 2",
			},
			"CREATE UNIQUE INDEX IF NOT EXISTS \"index\" ON \"schema\".\"table\" (\n  lower(\"col1\") ASC,\n  \"col2\" DESC,\n  (lower((\"col3\" ->> 'a'))),\n  \"col4\",\n  \"col5\" COLLATE \"C\"\n)" +
				" WHERE test = 1 OR test = 2",
		},
		{
			"PocketBase system index format",
			dbutils.ParseIndex("CREATE UNIQUE INDEX `idx_email__pb_users_auth_` ON `users` (`email`) WHERE `email` != ''"),
			`CREATE UNIQUE INDEX "idx_email__pb_users_auth_" ON "users" ("email") WHERE "email" != ''`,
		},
		{
			"identifier with double quote",
			dbutils.Index{
				IndexName: `idx"1`,
				TableName: "table",
				Columns:   []dbutils.IndexColumn{{Name: `col"a`}},
			},
			`CREATE INDEX "idx""1" ON "table" ("col""a")`,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := s.index.BuildPostgres()
			if result != s.expected {
				t.Fatalf("Expected \n%v \ngot \n%v", s.expected, result)
			}
		})
	}
}

func TestNormalizeIdentifierQuotes(t *testing.T) {
	scenarios := []struct {
		expr     string
		expected string
	}{
		{"", ""},
		{"a = 1", "a = 1"},
		{"`a` = 1", `"a" = 1`},
		{"`a`.`b` != ''", `"a"."b" != ''`},
		{"`a` = 'it''s `not` quoted'", `"a" = 'it''s ` + "`not`" + ` quoted'`},
		{"\"a`b\" = `c`", "\"a`b\" = \"c\""},
		{"`a\"b`", `"a""b"`},
	}

	for _, s := range scenarios {
		t.Run(s.expr, func(t *testing.T) {
			result := dbutils.NormalizeIdentifierQuotes(s.expr)
			if result != s.expected {
				t.Fatalf("Expected \n%v \ngot \n%v", s.expected, result)
			}
		})
	}
}
