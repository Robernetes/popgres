package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Robernetes/popgres/tools/list"
	"github.com/Robernetes/popgres/tools/search"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

// popgres: the full text "search" field.

func init() {
	Fields[FieldTypeSearch] = func() Field {
		return &SearchField{}
	}
}

const FieldTypeSearch = "search"

// SearchFieldMaxSources is the max number of the source fields of a search field.
const SearchFieldMaxSources = 8

// searchFieldSourceTypes are the field types that could be indexed by a search field.
var searchFieldSourceTypes = []string{
	FieldTypeText,
	FieldTypeEditor,
	FieldTypeEmail,
	FieldTypeURL,
	FieldTypeSelect,
}

var (
	_ Field = (*SearchField)(nil)
)

// SearchField defines "search" type field for the full text search of
// other text fields of the same collection.
//
// The field is a Postgres generated tsvector column (with a GIN index)
// computed from the source fields, so it is always up to date, also for
// the records changed with SQL. It has no record value: it is never
// exported and the values set to it are ignored.
//
// It is used in the filters with the ~ and !~ operators and sorted with @textRank:
//
//	filter: content ~ 'red shoes'
//	sort:   -@textRank(content, 'red shoes')
type SearchField struct {
	// Name (required) is the unique name of the field.
	Name string `form:"name" json:"name"`

	// Id is the unique stable field identifier.
	//
	// It is automatically generated from the name when adding to a collection FieldsList.
	Id string `form:"id" json:"id"`

	// System prevents the renaming and removal of the field.
	System bool `form:"system" json:"system"`

	// Hidden allows the search (filter and sort) only for the superusers.
	//
	// It is required if any of the source fields is hidden.
	Hidden bool `form:"hidden" json:"hidden"`

	// Presentable is not supported (the field has no value)
	// and it exists only for consistency with the other fields.
	Presentable bool `form:"presentable" json:"presentable"`

	// ---

	// Help is an extra text explaining what the field is about.
	// It is usually shown in Dashboard UI under the field input.
	Help string `form:"help" json:"help"`

	// Fields (required) is the list of the names of the collection
	// text, editor, email, url or select fields to search in.
	//
	// The order sets their relevance weight for @textRank: the first
	// field has the highest weight (A), then B, C and D for the rest.
	Fields []string `form:"fields" json:"fields"`

	// Language is the Postgres text search configuration (eg. "english" or "spanish")
	// used for the word stemming and the stop words.
	//
	// The default "simple" configuration only lowercases the words.
	Language string `form:"language" json:"language"`
}

// Type implements [Field.Type] interface method.
func (f *SearchField) Type() string {
	return FieldTypeSearch
}

// GetId implements [Field.GetId] interface method.
func (f *SearchField) GetId() string {
	return f.Id
}

// SetId implements [Field.SetId] interface method.
func (f *SearchField) SetId(id string) {
	f.Id = id
}

// GetName implements [Field.GetName] interface method.
func (f *SearchField) GetName() string {
	return f.Name
}

// SetName implements [Field.SetName] interface method.
func (f *SearchField) SetName(name string) {
	f.Name = name
}

// GetSystem implements [Field.GetSystem] interface method.
func (f *SearchField) GetSystem() bool {
	return f.System
}

// SetSystem implements [Field.SetSystem] interface method.
func (f *SearchField) SetSystem(system bool) {
	f.System = system
}

// GetHidden implements [Field.GetHidden] interface method.
func (f *SearchField) GetHidden() bool {
	return f.Hidden
}

// SetHidden implements [Field.SetHidden] interface method.
func (f *SearchField) SetHidden(hidden bool) {
	f.Hidden = hidden
}

// LanguageOrDefault returns the field text search configuration ("simple" if not set).
func (f *SearchField) LanguageOrDefault() string {
	if f.Language == "" {
		return search.DefaultTextSearchConfig
	}
	return f.Language
}

// ColumnType implements [Field.ColumnType] interface method.
//
// The column is generated from the source fields, each with its relevance weight.
func (f *SearchField) ColumnType(app App) string {
	config := search.TextSearchConfigLiteral(f.LanguageOrDefault())

	parts := make([]string, 0, len(f.Fields))
	for i, name := range f.Fields {
		weight := "D"
		if i < 3 {
			weight = string("ABC"[i])
		}

		parts = append(parts, fmt.Sprintf(
			"setweight(to_tsvector(%s, COALESCE(%s::text, '')), '%s')",
			config,
			quoteSearchSourceColumn(name),
			weight,
		))
	}

	if len(parts) == 0 {
		return "tsvector GENERATED ALWAYS AS (''::tsvector) STORED"
	}

	return "tsvector GENERATED ALWAYS AS (" + strings.Join(parts, " || ") + ") STORED"
}

// quoteSearchSourceColumn returns the quoted column name of a search field source.
func quoteSearchSourceColumn(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// PrepareValue implements [Field.PrepareValue] interface method.
//
// The search field has no record value (the column is generated by Postgres).
func (f *SearchField) PrepareValue(record *Record, raw any) (any, error) {
	return nil, nil
}

// ValidateValue implements [Field.ValidateValue] interface method.
func (f *SearchField) ValidateValue(ctx context.Context, app App, record *Record) error {
	return nil
}

// ValidateSettings implements [Field.ValidateSettings] interface method.
func (f *SearchField) ValidateSettings(ctx context.Context, app App, collection *Collection) error {
	return validation.ValidateStruct(f,
		validation.Field(&f.Id, validation.By(DefaultFieldIdValidationRule)),
		validation.Field(&f.Name, validation.By(DefaultFieldNameValidationRule)),
		validation.Field(&f.Help, validation.By(DefaultFieldHelpValidationRule)),
		validation.Field(
			&f.Fields,
			validation.Required,
			validation.Length(1, SearchFieldMaxSources),
			validation.By(f.checkSources(collection)),
		),
		validation.Field(&f.Language, validation.By(f.checkLanguage(app))),
	)
}

// checkSources checks that the source fields exist, could be searched and are unique.
func (f *SearchField) checkSources(collection *Collection) validation.RuleFunc {
	return func(value any) error {
		names, _ := value.([]string)

		if len(list.ToUniqueStringSlice(names)) != len(names) {
			return validation.NewError("validation_search_duplicated_fields", "The search fields must be unique.")
		}

		for _, name := range names {
			source := collection.Fields.GetByName(name)
			if source == nil {
				return validation.NewError(
					"validation_search_missing_field",
					fmt.Sprintf("The collection doesn't have a field %q.", name),
				).SetParams(map[string]any{"name": name})
			}

			if !slices.Contains(searchFieldSourceTypes, source.Type()) {
				return validation.NewError(
					"validation_search_field_type",
					fmt.Sprintf("The field %q can't be searched (only the text, editor, email, url and select fields).", name),
				).SetParams(map[string]any{"name": name})
			}

			// the auth email is visible only to the record owner when emailVisibility is false
			if collection.IsAuth() && name == FieldNameEmail {
				return validation.NewError(
					"validation_search_auth_email",
					"The email field of the auth collections can't be searched.",
				)
			}

			// the search must not reveal the content of the hidden fields
			if source.GetHidden() && !f.Hidden {
				return validation.NewError(
					"validation_search_hidden_field",
					fmt.Sprintf("The field %q is hidden, so the search field must be hidden too.", name),
				).SetParams(map[string]any{"name": name})
			}
		}

		return nil
	}
}

// checkLanguage checks that the text search configuration exists on the Postgres server.
func (f *SearchField) checkLanguage(app App) validation.RuleFunc {
	return func(value any) error {
		name, _ := value.(string)
		if name == "" {
			return nil // default
		}

		invalidErr := validation.NewError(
			"validation_search_language",
			fmt.Sprintf("The Postgres server doesn't have the %q text search configuration.", name),
		).SetParams(map[string]any{"name": name})

		if !search.IsValidTextSearchConfig(name) {
			return invalidErr
		}

		var exists bool
		err := app.DB().NewQuery("SELECT EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = {:name})").
			Bind(map[string]any{"name": name}).
			Row(&exists)
		if err != nil || !exists {
			return invalidErr
		}

		return nil
	}
}

// signature returns the definition of the generated column that doesn't
// depend on the names of the source fields (a renamed source column
// is still used by the generated column).
func (f *SearchField) signature(app App, collection *Collection) string {
	var b strings.Builder

	b.WriteString(f.LanguageOrDefault())

	for _, name := range f.Fields {
		b.WriteString("|")
		source := collection.Fields.GetByName(name)
		if source == nil {
			b.WriteString("?" + name)
			continue
		}
		b.WriteString(source.GetId() + ":" + ColumnBaseType(source.ColumnType(app)))
	}

	return b.String()
}

// renameSearchFieldSources replaces the renamed source fields of the collection search fields.
func renameSearchFieldSources(collection *Collection, oldCollection *Collection) {
	for _, f := range collection.Fields {
		field, ok := f.(*SearchField)
		if !ok {
			continue
		}

		for i, name := range field.Fields {
			oldSource := oldCollection.Fields.GetByName(name)
			if oldSource == nil {
				continue
			}

			// the old name is not used by another field of the new collection
			if newSource := collection.Fields.GetById(oldSource.GetId()); newSource != nil && collection.Fields.GetByName(name) == nil {
				field.Fields[i] = newSource.GetName()
			}
		}
	}
}

// changedSearchFields returns the ids of the old collection search fields whose
// generated column must be dropped before the other column changes: the removed
// search fields and the ones with a changed definition or changed source columns
// (Postgres doesn't drop or alter the columns used by a generated column).
func changedSearchFields(app App, oldCollection *Collection, newCollection *Collection) map[string]bool {
	result := map[string]bool{}

	for _, f := range oldCollection.Fields {
		oldField, ok := f.(*SearchField)
		if !ok {
			continue
		}

		newField, ok := newCollection.Fields.GetById(oldField.Id).(*SearchField)
		if !ok || oldField.signature(app, oldCollection) != newField.signature(app, newCollection) {
			result[oldField.Id] = true
		}
	}

	return result
}

// searchIndexName returns the name of the GIN index of the provided field.
func searchIndexName(collection *Collection, field *SearchField) string {
	return "idx_fts_" + collection.Id + "_" + field.Id
}

// dropSearchIndexes drops the GIN indexes of the collection search fields.
func dropSearchIndexes(txApp App, collection *Collection) error {
	for _, f := range collection.Fields {
		field, ok := f.(*SearchField)
		if !ok {
			continue
		}

		_, err := txApp.DB().NewQuery("DROP INDEX IF EXISTS [[" + searchIndexName(collection, field) + "]]").Execute()
		if err != nil {
			return err
		}
	}

	return nil
}

// createSearchIndexes creates the GIN indexes of the collection search fields.
func createSearchIndexes(txApp App, collection *Collection) error {
	for _, f := range collection.Fields {
		field, ok := f.(*SearchField)
		if !ok {
			continue
		}

		_, err := txApp.DB().NewQuery(fmt.Sprintf(
			"CREATE INDEX IF NOT EXISTS [[%s]] ON {{%s}} USING gin ([[%s]])",
			searchIndexName(collection, field),
			collection.Name,
			field.Name,
		)).Execute()
		if err != nil {
			return fmt.Errorf("failed to create the %q search index: %w", field.Name, err)
		}
	}

	return nil
}
