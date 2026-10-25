// Package importer copies the collections and the data of a PocketBase
// SQLite pb_data directory into a popgres (Postgres) app.
//
// It is the only popgres package allowed to depend on a SQLite driver.
package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tools/dbutils"
	"github.com/Robernetes/popgres/tools/types"
	"github.com/pocketbase/dbx"
	_ "modernc.org/sqlite"
)

// Options defines the import options.
type Options struct {
	// SkipLogs skips the import of the auxiliary.db request logs.
	SkipLogs bool

	// Force allows importing into a database that already has non-system collections.
	Force bool
}

// Report describes the result of an import.
type Report struct {
	// Collections lists the names of the imported collections.
	Collections []string

	// Records holds the number of imported records per collection name.
	Records map[string]int

	// FailedViews holds the view collections that couldn't be created
	// (usually because their query is not valid Postgres SQL) and their errors.
	FailedViews map[string]error

	// Params is the number of imported _params rows.
	Params int

	// Logs is the number of imported _logs rows.
	Logs int
}

// String returns a human readable summary of the report.
func (r *Report) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "collections: %d\n", len(r.Collections))
	for _, name := range r.Collections {
		if n, ok := r.Records[name]; ok {
			fmt.Fprintf(&b, "  %-24s %d records\n", name, n)
		} else {
			fmt.Fprintf(&b, "  %-24s view\n", name)
		}
	}
	fmt.Fprintf(&b, "params: %d\n", r.Params)
	fmt.Fprintf(&b, "logs: %d\n", r.Logs)
	if len(r.FailedViews) > 0 {
		fmt.Fprintf(&b, "views that need to be adapted manually: %d\n", len(r.FailedViews))
		for name, err := range r.FailedViews {
			fmt.Fprintf(&b, "  %s: %v\n", name, err)
		}
	}

	return b.String()
}

// Import copies the collections, records, settings and (optionally) the logs
// of the PocketBase pb_data directory into the provided bootstrapped popgres app.
//
// The storage files are not copied (see [CopyStorage]).
func Import(app core.App, pbDataDir string, opts Options) (*Report, error) {
	report := &Report{
		Records:     map[string]int{},
		FailedViews: map[string]error{},
	}

	src, err := openSQLite(filepath.Join(pbDataDir, "data.db"))
	if err != nil {
		return nil, err
	}
	defer src.Close()

	if !opts.Force {
		existing, err := app.FindAllCollections()
		if err != nil {
			return nil, err
		}
		for _, c := range existing {
			if !c.System && c.Name != "users" {
				return nil, fmt.Errorf("the target database already has the non-system collection %q (use the force option to import anyway)", c.Name)
			}
		}
	}

	collections, err := readCollections(src)
	if err != nil {
		return nil, fmt.Errorf("failed to read the source collections: %w", err)
	}

	if err := importCollections(app, collections, report); err != nil {
		return nil, err
	}

	err = app.RunInTransaction(func(txApp core.App) error {
		for _, c := range collections {
			if c.isView() {
				continue
			}

			target, err := txApp.FindCollectionByNameOrId(c.id)
			if err != nil {
				return err
			}

			total, err := importRecords(txApp, src, target)
			if err != nil {
				return fmt.Errorf("failed to import the %q records: %w", target.Name, err)
			}
			report.Records[target.Name] = total
		}

		report.Params, err = importParams(txApp, src)

		return err
	})
	if err != nil {
		return nil, err
	}

	// reload with the imported settings
	if err := app.ReloadSettings(); err != nil {
		return nil, err
	}

	if !opts.SkipLogs {
		auxPath := filepath.Join(pbDataDir, "auxiliary.db")
		if _, err := os.Stat(auxPath); err == nil {
			report.Logs, err = importLogs(app, auxPath)
			if err != nil {
				return nil, err
			}
		}
	}

	return report, nil
}

func openSQLite(path string) (*dbx.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("missing SQLite database %s: %w", path, err)
	}

	return dbx.Open("sqlite", path+"?mode=ro")
}

// sourceCollection is a raw collection row of the PocketBase _collections table.
type sourceCollection struct {
	id      string
	name    string
	created string
	updated string
	indexes string // the raw source indexes json

	// data is the collection in the import (export) format with the
	// options flattened (including the secrets, eg. the OAuth2 client secrets,
	// that are omitted when a Collection model is serialized to json).
	data map[string]any
}

func (c *sourceCollection) isView() bool {
	return c.data["type"] == core.CollectionTypeView
}

func readCollections(src *dbx.DB) ([]*sourceCollection, error) {
	rows := []dbx.NullStringMap{}
	if err := src.Select("*").From("_collections").OrderBy("rowid ASC").All(&rows); err != nil {
		return nil, err
	}

	result := make([]*sourceCollection, 0, len(rows))

	for _, row := range rows {
		data := map[string]any{}

		// the options are flattened in the export format
		if raw := row["options"].String; raw != "" {
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				return nil, fmt.Errorf("invalid options of collection %q: %w", row["name"].String, err)
			}
		}

		for _, col := range []string{"fields", "indexes"} {
			var v any
			if err := json.Unmarshal([]byte(row[col].String), &v); err != nil {
				return nil, fmt.Errorf("invalid %s of collection %q: %w", col, row["name"].String, err)
			}
			data[col] = v
		}

		for _, col := range []string{"listRule", "viewRule", "createRule", "updateRule", "deleteRule"} {
			if row[col].Valid {
				data[col] = row[col].String
			} else {
				data[col] = nil
			}
		}

		system, _ := strconv.ParseBool(row["system"].String)

		data["id"] = row["id"].String
		data["name"] = row["name"].String
		data["type"] = row["type"].String
		data["system"] = system

		result = append(result, &sourceCollection{
			id:      row["id"].String,
			name:    row["name"].String,
			created: row["created"].String,
			updated: row["updated"].String,
			indexes: row["indexes"].String,
			data:    data,
		})
	}

	return result, nil
}

func importCollections(app core.App, collections []*sourceCollection, report *Report) error {
	// replace the matching non-system collections of the target
	// (eg. the default "users" created by the system migrations) so that the
	// source fields ids and options are preserved
	// (ImportCollections would otherwise keep the existing fields with the same name)
	for _, c := range collections {
		existing, err := app.FindCollectionByNameOrId(c.id)
		if err != nil {
			existing, err = app.FindCollectionByNameOrId(c.name)
		}
		if err != nil || existing.System {
			continue
		}

		existing.IntegrityChecks(false)
		if err := app.Delete(existing); err != nil {
			return fmt.Errorf("failed to replace the existing collection %q: %w", existing.Name, err)
		}
	}

	regular := []map[string]any{}
	views := []*sourceCollection{}

	for _, c := range collections {
		if c.isView() {
			views = append(views, c)
			continue
		}

		regular = append(regular, c.data)
	}

	if len(regular) > 0 {
		if err := app.ImportCollections(regular, false); err != nil {
			return fmt.Errorf("failed to import the collections: %w", err)
		}
	}

	for _, c := range collections {
		if !c.isView() {
			report.Collections = append(report.Collections, c.name)
		}
	}

	// the views are imported one by one because their query may need manual changes
	for _, view := range views {
		if query, ok := view.data["viewQuery"].(string); ok {
			view.data["viewQuery"] = dbutils.NormalizeIdentifierQuotes(query)
		}

		if err := app.ImportCollections([]map[string]any{view.data}, false); err != nil {
			report.FailedViews[view.name] = err
			continue
		}

		report.Collections = append(report.Collections, view.name)
	}

	// restore the source creation and update dates and the indexes definitions
	// (ImportCollections saves the collections as new ones and normalizes the indexes)
	for _, c := range collections {
		params := dbx.Params{}
		if c.indexes != "" && json.Valid([]byte(c.indexes)) {
			params["indexes"] = c.indexes
		}
		for col, value := range map[string]string{"created": c.created, "updated": c.updated} {
			if dt, _ := types.ParseDateTime(value); !dt.IsZero() {
				params[col] = dt.String()
			}
		}
		if c.isView() && report.FailedViews[c.name] == nil {
			fields, err := viewFieldsWithSourceIds(app, c)
			if err != nil {
				return err
			}
			if fields != "" {
				params["fields"] = fields
			}
		}
		if len(params) == 0 {
			continue
		}

		_, err := app.DB().Update("_collections", params, dbx.HashExp{"id": c.id}).Execute()
		if err != nil {
			return fmt.Errorf("failed to restore the %q collection dates: %w", c.name, err)
		}
	}

	if err := reorderCollections(app, collections); err != nil {
		return fmt.Errorf("failed to reorder the collections: %w", err)
	}

	return app.ReloadCachedCollections()
}

// viewFieldsWithSourceIds returns the serialized fields of the imported view
// with the ids of the source fields with the same name
// (the view fields are regenerated from the view query with new random ids).
func viewFieldsWithSourceIds(app core.App, view *sourceCollection) (string, error) {
	target, err := app.FindCollectionByNameOrId(view.id)
	if err != nil {
		return "", fmt.Errorf("failed to load the imported view %q: %w", view.name, err)
	}

	sourceIds := map[string]string{}
	sourceFields, _ := view.data["fields"].([]any)
	for _, raw := range sourceFields {
		f, _ := raw.(map[string]any)
		name, _ := f["name"].(string)
		id, _ := f["id"].(string)
		if name != "" && id != "" {
			sourceIds[name] = id
		}
	}

	for _, f := range target.Fields {
		if id, ok := sourceIds[f.GetName()]; ok {
			f.SetId(id)
		}
	}

	raw, err := json.Marshal(target.Fields)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// reorderCollections rewrites the _collections rows in the source rowid order.
//
// The rows of a query without ORDER BY are returned in the SQLite rowid order
// while Postgres returns them in their physical order, which is changed by the
// replaced and updated collections during the import.
func reorderCollections(app core.App, collections []*sourceCollection) error {
	ids := make([]string, len(collections))
	for i, c := range collections {
		ids[i] = c.id
	}

	rawIds, err := json.Marshal(ids)
	if err != nil {
		return err
	}

	return app.RunInTransaction(func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("CREATE TEMP TABLE {{_popgres_reorder}} ON COMMIT DROP AS SELECT * FROM {{_collections}}").Execute()
		if err != nil {
			return err
		}

		_, err = txApp.DB().NewQuery("DELETE FROM {{_collections}}").Execute()
		if err != nil {
			return err
		}

		_, err = txApp.DB().NewQuery(`
			INSERT INTO {{_collections}}
			SELECT r.* FROM {{_popgres_reorder}} [[r]]
			LEFT JOIN jsonb_array_elements_text({:ids}::jsonb) WITH ORDINALITY [[o]]([[id]], [[n]]) ON [[o.id]] = [[r.id]]
			ORDER BY [[o.n]] NULLS LAST, [[r.id]]
		`).Bind(dbx.Params{"ids": string(rawIds)}).Execute()

		return err
	})
}

func importRecords(txApp core.App, src *dbx.DB, collection *core.Collection) (int, error) {
	rows := []dbx.NullStringMap{}
	if err := src.Select("*").From(collection.Name).OrderBy("rowid ASC").All(&rows); err != nil {
		return 0, err
	}

	for _, row := range rows {
		params := make(dbx.Params, len(collection.Fields))

		for _, field := range collection.Fields {
			raw, ok := row[field.GetName()]
			if !ok {
				continue // the column doesn't exist in the source table
			}

			params[field.GetName()] = convertValue(field, raw.String, raw.Valid)
		}

		if _, err := txApp.DB().Insert(collection.Name, params).Execute(); err != nil {
			return 0, err
		}
	}

	return len(rows), nil
}

// convertValue converts a SQLite column value to the equivalent Postgres one
// based on the field type.
func convertValue(field core.Field, value string, valid bool) any {
	if mv, ok := field.(core.MultiValuer); ok && mv.IsMultiple() {
		return toJSONArray(value, valid)
	}

	switch field.Type() {
	case core.FieldTypeBool:
		b, _ := strconv.ParseBool(value)
		return valid && b
	case core.FieldTypeNumber:
		f, err := strconv.ParseFloat(value, 64)
		if !valid || err != nil {
			return float64(0)
		}
		return f
	case core.FieldTypeDate, core.FieldTypeAutodate:
		dt, _ := types.ParseDateTime(value)
		if !valid || dt.IsZero() {
			return nil
		}
		return dt.String()
	case core.FieldTypeJSON:
		if !valid || value == "" {
			return nil
		}
		if json.Valid([]byte(value)) {
			return value
		}
		quoted, _ := json.Marshal(value)
		return string(quoted)
	case core.FieldTypeGeoPoint:
		if valid && json.Valid([]byte(value)) {
			return value
		}
		return `{"lon":0,"lat":0}`
	default:
		if !valid {
			return ""
		}
		return value
	}
}

// toJSONArray normalizes a single or multiple value column to a json array.
func toJSONArray(value string, valid bool) string {
	if !valid || value == "" {
		return "[]"
	}

	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		if _, isArray := decoded.([]any); isArray {
			return value
		}
		wrapped, _ := json.Marshal([]any{decoded})
		return string(wrapped)
	}

	wrapped, _ := json.Marshal([]string{value})
	return string(wrapped)
}

func importParams(txApp core.App, src *dbx.DB) (int, error) {
	rows := []dbx.NullStringMap{}
	if err := src.Select("*").From("_params").All(&rows); err != nil {
		return 0, err
	}

	for _, row := range rows {
		id := row["id"].String

		if _, err := txApp.DB().Delete("_params", dbx.HashExp{"id": id}).Execute(); err != nil {
			return 0, err
		}

		params := dbx.Params{"id": id}
		if row["value"].Valid {
			params["value"] = row["value"].String
		} else {
			params["value"] = nil
		}
		for _, col := range []string{"created", "updated"} {
			dt, _ := types.ParseDateTime(row[col].String)
			if dt.IsZero() {
				dt = types.NowDateTime()
			}
			params[col] = dt.String()
		}

		if _, err := txApp.DB().Insert("_params", params).Execute(); err != nil {
			return 0, err
		}
	}

	return len(rows), nil
}

func importLogs(app core.App, auxPath string) (int, error) {
	src, err := openSQLite(auxPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	rows := []dbx.NullStringMap{}
	if err := src.Select("*").From("_logs").OrderBy("rowid ASC").All(&rows); err != nil {
		return 0, err
	}

	for _, row := range rows {
		data := row["data"].String
		if !json.Valid([]byte(data)) {
			data = "{}"
		}

		created, _ := types.ParseDateTime(row["created"].String)
		if created.IsZero() {
			return 0, errors.New("invalid log created date " + row["created"].String)
		}

		level, _ := strconv.Atoi(row["level"].String)

		_, err := app.AuxDB().Insert("_logs", dbx.Params{
			"id":      row["id"].String,
			"level":   level,
			"message": row["message"].String,
			"data":    data,
			"created": created.String(),
		}).Execute()
		if err != nil {
			return 0, err
		}
	}

	return len(rows), nil
}

// CopyStorage copies the local storage files of the PocketBase pb_data
// directory into the popgres data directory.
func CopyStorage(pbDataDir string, popgresDataDir string) error {
	srcStorage := filepath.Join(pbDataDir, core.LocalStorageDirName)
	if _, err := os.Stat(srcStorage); err != nil {
		return nil // nothing to copy
	}

	return os.CopyFS(filepath.Join(popgresDataDir, core.LocalStorageDirName), os.DirFS(srcStorage))
}
