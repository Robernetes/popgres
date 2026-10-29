package validators

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

// UniqueId checks whether a field string id already exists in the specified table.
//
// Example:
//
//	validation.Field(&form.RelId, validation.By(validators.UniqueId(form.app.DB(), "tbl_example"))
func UniqueId(db dbx.Builder, tableName string) validation.RuleFunc {
	return func(value any) error {
		v, _ := value.(string)
		if v == "" {
			return nil // nothing to check
		}

		var foundId string

		err := db.
			Select("id").
			From(tableName).
			Where(dbx.HashExp{"id": v}).
			Limit(1).
			Row(&foundId)

		if (err != nil && !errors.Is(err, sql.ErrNoRows)) || foundId != "" {
			return validation.NewError("validation_invalid_or_existing_id", "The model id is invalid or already exists.")
		}

		return nil
	}
}

// NormalizeUniqueIndexError attempts to convert a Postgres
// unique violation error (SQLSTATE 23505) into a validation.Errors.
//
// The violating fields are detected from the error detail
// (eg. "Key (email)=(...)" or "Key (lower(email))=(...)").
//
// The provided err is returned as it is without changes if:
// - err is nil
// - err is already validation.Errors
// - err is not an unique violation error of the specified table
// - none of the fieldNames is part of the violated unique index
//
// popgres: detects the Postgres unique violations (SQLSTATE 23505) instead of the SQLite error text
func NormalizeUniqueIndexError(err error, tableOrAlias string, fieldNames []string) error {
	if err == nil {
		return err
	}

	if _, ok := err.(validation.Errors); ok {
		return err
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}

	if pgErr.TableName != "" && !strings.EqualFold(pgErr.TableName, tableOrAlias) {
		return err
	}

	columns := uniqueViolationColumns(pgErr.Detail)

	normalizedErrs := validation.Errors{}

	for _, name := range fieldNames {
		if _, ok := columns[strings.ToLower(name)]; ok {
			normalizedErrs[name] = validation.NewError("validation_not_unique", "Value must be unique")
		}
	}

	if len(normalizedErrs) > 0 {
		return normalizedErrs
	}

	return err
}

// uniqueViolationIdentifierRegex matches the (optionally double quoted)
// identifiers in an unique violation detail key expression.
var uniqueViolationIdentifierRegex = regexp.MustCompile(`"((?:[^"]|"")+)"|([A-Za-z_][A-Za-z0-9_$]*)`)

// uniqueViolationColumns extracts the lowercased column names from the
// "Key (...)=(...)" part of a Postgres unique violation error detail.
func uniqueViolationColumns(detail string) map[string]struct{} {
	result := map[string]struct{}{}

	start := strings.Index(detail, "Key (")
	end := strings.Index(detail, ")=(")
	if start < 0 || end <= start {
		return result
	}

	keyExpr := detail[start+len("Key (") : end]

	for _, m := range uniqueViolationIdentifierRegex.FindAllStringSubmatch(keyExpr, -1) {
		name := m[2]
		if m[1] != "" {
			name = strings.ReplaceAll(m[1], `""`, `"`)
		}
		result[strings.ToLower(name)] = struct{}{}
	}

	return result
}
