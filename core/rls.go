package core

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/Robernetes/popgres/tools/inflector"
	"github.com/Robernetes/popgres/tools/search"
	"github.com/pocketbase/dbx"
	"github.com/spf13/cast"
)

// popgres: the collection API rules compiled to Postgres row level security policies.
//
// The policies apply to the external roles (RLSAnonRole and RLSAuthenticatedRole),
// eg. a BI tool or another service connected directly to the database.
// The app connection role is the tables owner (or a superuser) and bypasses them.
//
// The auth record is identified with a popgres auth token stored in the
// "popgres.token" setting (see popgres_rls.set_claims()). The policies never trust
// the setting value directly because any role can change it: popgres_rls.auth()
// verifies the token signature on every query.

const (
	// RLSAnonRole is the external role of the guests.
	RLSAnonRole = "popgres_anon"

	// RLSAuthenticatedRole is the external role of the auth records.
	RLSAuthenticatedRole = "popgres_authenticated"

	// RLSAPIRole is the role of the strict mode API queries. It has the same
	// policies as the external roles but it can read all columns (the API hides
	// the non-public fields itself). Only the app connection role can use it.
	RLSAPIRole = "popgres_api"

	// RLSSchema is the schema of the popgres RLS functions.
	RLSSchema = "popgres_rls"

	// RLSStrictEnv is the env variable used as fallback for BaseAppConfig.RLSStrict.
	RLSStrictEnv = "POPGRES_RLS_STRICT"

	// RLSTokenSetting is the setting with the auth token of the current session or transaction.
	RLSTokenSetting = "popgres.token"

	// RLSRuleSetting optionally restricts the SELECT policies to the "list" or the "view" rule.
	RLSRuleSetting = "popgres.rule"
)

// The collection rules compiled to RLS policies.
const (
	RLSRuleList   = "list"
	RLSRuleView   = "view"
	RLSRuleCreate = "create"
	RLSRuleUpdate = "update"
	RLSRuleDelete = "delete"
)

// The compilation statuses of a single collection rule.
const (
	// RLSStatusTranslated is a rule compiled to an equivalent policy.
	RLSStatusTranslated = "translated"

	// RLSStatusSuperusersOnly is a null (superusers only) rule without a policy.
	RLSStatusSuperusersOnly = "superusersOnly"

	// RLSStatusDenied is a rule that cannot be translated and its policy denies all records.
	RLSStatusDenied = "denied"

	// RLSStatusNotApplicable is a rule without external access (eg. of a view or a system collection).
	RLSStatusNotApplicable = "notApplicable"
)

// The collection RLS statuses.
const (
	// RLSCollectionTranslated is a collection with all of its rules translated (or superusers only).
	RLSCollectionTranslated = "translated"

	// RLSCollectionPartial is a collection with some of its rules denied.
	RLSCollectionPartial = "partial"

	// RLSCollectionDenied is a collection with all of its non-null rules denied.
	RLSCollectionDenied = "denied"

	// RLSCollectionExcluded is a view or a system collection without external access.
	RLSCollectionExcluded = "excluded"
)

// RLSPolicy is the compiled policy of a single collection rule.
type RLSPolicy struct {
	Rule   string  `json:"rule"`
	Filter *string `json:"filter"`
	Status string  `json:"status"`

	// Inline reports whether the policy expression is inlined
	// (otherwise it calls a SECURITY DEFINER function, eg. for the rules with relations).
	Inline bool `json:"inline"`

	// Reason describes why the rule was denied.
	Reason string `json:"reason,omitempty"`

	// SQL is the policy (and its function) definition.
	SQL string `json:"sql"`
}

// RLSCollection is the compiled RLS definition of a single collection.
type RLSCollection struct {
	CollectionId   string       `json:"collectionId"`
	CollectionName string       `json:"collectionName"`
	Status         string       `json:"status"`
	Policies       []*RLSPolicy `json:"policies"`

	// Grants are the privileges of the external roles.
	Grants string `json:"grants"`

	// SQL is the full definition (policies, functions and privileges).
	SQL string `json:"sql"`
}

// rlsAuthIdPlaceholder is the auth record id used during the compilation
// (it is replaced with the auth id expression in the generated SQL).
const rlsAuthIdPlaceholder = "\x01popgres_rls_auth_id\x01"

// rlsAuthAlias is the alias of the auth claims in the policy functions.
const rlsAuthAlias = "__rls_auth"

// rlsNewRecordName is the name of the new record CTE of the create rule functions.
const rlsNewRecordName = "_rls_new"

var (
	rlsInlineAuthIdExpr         = "(SELECT " + RLSSchema + ".auth() ->> 'id')"
	rlsInlineAuthCollectionExpr = "(SELECT " + RLSSchema + ".auth() ->> 'collectionId')"
	rlsFuncAuthIdExpr           = "(" + rlsAuthAlias + ".a ->> 'id')"
	rlsFuncAuthCollectionExpr   = rlsAuthAlias + ".a ->> 'collectionId'"
)

// rlsRoles are all roles of the RLS policies.
const rlsRoles = RLSAnonRole + ", " + RLSAuthenticatedRole + ", " + RLSAPIRole

// rlsDB is a connectionless db instance used only to build the policies SQL.
var rlsDB = dbx.NewFromDB(nil, "pgx")

// rlsStaticAuthFields are the auth fields resolved statically in the RLS policies
// (the other auth fields are resolved with a join to the auth collection).
var rlsStaticAuthFields = map[string]struct{}{
	"@request.auth." + FieldNameId:             {},
	"@request.auth." + FieldNameCollectionId:   {},
	"@request.auth." + FieldNameCollectionName: {},
}

func isRLSStaticAuthField(fieldName string) bool {
	_, ok := rlsStaticAuthFields[fieldName]
	return ok
}

// rlsResolverState is the RecordFieldResolver state of a RLS rule compilation.
type rlsResolverState struct {
	collections    []*Collection
	untranslatable string
}

func (s *rlsResolverState) findCollection(nameOrId string) (*Collection, error) {
	for _, c := range s.collections {
		if c.Id == nameOrId || strings.EqualFold(c.Name, nameOrId) {
			return c, nil
		}
	}

	return nil, fmt.Errorf("missing collection %q", nameOrId)
}

// checkTranslatable reports whether the field can be used in a RLS policy.
//
// The request body, query, headers, method and context are not available
// outside of the API (as well as the request specific modifiers).
func (s *rlsResolverState) checkTranslatable(fieldName string) bool {
	ok := true

	if strings.HasPrefix(fieldName, "@request.") && !strings.HasPrefix(fieldName, "@request.auth.") {
		ok = false
	} else if strings.Contains(fieldName, ":"+issetModifier) || strings.Contains(fieldName, ":"+changedModifier) {
		ok = false
	}

	if !ok && s.untranslatable == "" {
		s.untranslatable = fieldName
	}

	return ok
}

// rlsCompiler compiles the rules of the collections of a single state.
type rlsCompiler struct {
	app             App
	schema          string // the schema of the collection tables
	collections     []*Collection
	authCollections []*Collection
}

func newRLSCompiler(app App, collections []*Collection, schema string) *rlsCompiler {
	if schema == "" {
		schema = "public"
	}

	c := &rlsCompiler{app: app, collections: collections, schema: schema}

	for _, collection := range collections {
		if collection.IsAuth() {
			c.authCollections = append(c.authCollections, collection)
		}
	}

	// deterministic output
	slices.SortFunc(c.authCollections, func(a, b *Collection) int {
		return strings.Compare(a.Id, b.Id)
	})

	return c
}

// CompileRLS compiles the RLS policies of the provided collection
// with the current collections of the app.
func CompileRLS(app App, collection *Collection) (*RLSCollection, error) {
	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}

	// use the provided instance (it could have unsaved changes)
	for i, c := range collections {
		if c.Id == collection.Id {
			collections[i] = collection
		}
	}
	if !slices.Contains(collections, collection) {
		collections = append(collections, collection)
	}

	schema, err := currentSchema(app)
	if err != nil {
		return nil, err
	}

	return newRLSCompiler(app, collections, schema).compile(collection), nil
}

// currentSchema returns the schema of the collection tables.
func currentSchema(app App) (string, error) {
	var schema string
	err := app.DB().NewQuery("SELECT current_schema()").Row(&schema)
	return schema, err
}

// isRLSExcluded reports whether the collection doesn't have external access.
func isRLSExcluded(collection *Collection) bool {
	return collection.IsView() || collection.System || strings.HasPrefix(collection.Name, "_")
}

func (c *rlsCompiler) compile(collection *Collection) *RLSCollection {
	result := &RLSCollection{
		CollectionId:   collection.Id,
		CollectionName: collection.Name,
	}

	rules := []struct {
		name string
		rule *string
	}{
		{RLSRuleList, collection.ListRule},
		{RLSRuleView, collection.ViewRule},
		{RLSRuleCreate, collection.CreateRule},
		{RLSRuleUpdate, collection.UpdateRule},
		{RLSRuleDelete, collection.DeleteRule},
	}

	table := quoteRLSIdent(collection.Name)

	var sqlBuilder strings.Builder

	if collection.IsView() {
		result.Status = RLSCollectionExcluded
		for _, r := range rules {
			result.Policies = append(result.Policies, &RLSPolicy{Rule: r.name, Filter: r.rule, Status: RLSStatusNotApplicable})
		}
		// the views are executed with the owner privileges, so only the
		// strict mode role (the client filters of the API) can read them
		result.Grants = "REVOKE ALL ON TABLE " + table + " FROM " + rlsRoles + ";\n" +
			"GRANT SELECT ON TABLE " + table + " TO " + RLSAPIRole + ";\n"
		result.SQL = result.Grants
		return result
	}

	sqlBuilder.WriteString("ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY;\n")

	if isRLSExcluded(collection) {
		result.Status = RLSCollectionExcluded
		for _, r := range rules {
			result.Policies = append(result.Policies, &RLSPolicy{Rule: r.name, Filter: r.rule, Status: RLSStatusNotApplicable})
		}
		result.Grants = "REVOKE ALL ON TABLE " + table + " FROM " + rlsRoles + ";\n"
		sqlBuilder.WriteString(result.Grants)
		result.SQL = sqlBuilder.String()
		return result
	}

	var translated, denied int
	for _, r := range rules {
		policy := c.compilePolicy(collection, r.name, r.rule)
		result.Policies = append(result.Policies, policy)

		switch policy.Status {
		case RLSStatusTranslated:
			translated++
		case RLSStatusDenied:
			denied++
		}

		if policy.SQL != "" {
			sqlBuilder.WriteString(policy.SQL)
		}
	}

	switch {
	case denied == 0:
		result.Status = RLSCollectionTranslated
	case translated == 0:
		result.Status = RLSCollectionDenied
	default:
		result.Status = RLSCollectionPartial
	}

	result.Grants = c.grants(collection)
	sqlBuilder.WriteString(result.Grants)

	result.SQL = sqlBuilder.String()

	return result
}

// grants returns the privileges of the RLS roles for the provided collection.
func (c *rlsCompiler) grants(collection *Collection) string {
	table := quoteRLSIdent(collection.Name)
	roles := RLSAnonRole + ", " + RLSAuthenticatedRole

	var b strings.Builder
	b.WriteString("REVOKE ALL ON TABLE " + table + " FROM " + rlsRoles + ";\n")

	// the strict mode role reads all columns because the API needs the full
	// records (it hides the non-public fields itself); the rows are still
	// limited by the same policies
	b.WriteString("GRANT SELECT ON TABLE " + table + " TO " + RLSAPIRole + ";\n")

	var visibleCols []string
	for _, f := range collection.Fields {
		if f.GetHidden() {
			continue
		}
		// the email visibility is checked per record by the API
		if collection.IsAuth() && f.GetName() == FieldNameEmail {
			continue
		}
		visibleCols = append(visibleCols, quoteRLSIdent(f.GetName()))
	}

	if len(visibleCols) > 0 && (collection.ListRule != nil || collection.ViewRule != nil) {
		b.WriteString("GRANT SELECT (" + strings.Join(visibleCols, ", ") + ") ON TABLE " + table + " TO " + roles + ";\n")
	}

	// the auth records are managed only through the API (password hashes, token keys, verification, etc.)
	if !collection.IsBase() {
		return b.String()
	}

	if len(visibleCols) > 0 && collection.CreateRule != nil {
		b.WriteString("GRANT INSERT (" + strings.Join(visibleCols, ", ") + ") ON TABLE " + table + " TO " + roles + ";\n")
	}

	if collection.UpdateRule != nil {
		updateCols := slices.DeleteFunc(slices.Clone(visibleCols), func(col string) bool {
			return col == quoteRLSIdent(FieldNameId)
		})
		if len(updateCols) > 0 {
			b.WriteString("GRANT UPDATE (" + strings.Join(updateCols, ", ") + ") ON TABLE " + table + " TO " + roles + ";\n")
		}
	}

	if collection.DeleteRule != nil {
		b.WriteString("GRANT DELETE ON TABLE " + table + " TO " + roles + ";\n")
	}

	return b.String()
}

// rlsPolicyName returns the policy name of the provided rule.
func rlsPolicyName(rule string) string {
	return "popgres_" + rule
}

// rlsFunctionName returns the policy function name of the provided collection rule.
func rlsFunctionName(collectionId string, rule string) string {
	return "rls_" + collectionId + "_" + rule
}

func (c *rlsCompiler) compilePolicy(collection *Collection, ruleName string, rule *string) *RLSPolicy {
	policy := &RLSPolicy{Rule: ruleName, Filter: rule}

	if rule == nil {
		policy.Status = RLSStatusSuperusersOnly
		return policy
	}

	table := quoteRLSIdent(collection.Name)

	var expr string
	var functionSQL string

	if strings.TrimSpace(*rule) == "" {
		policy.Status = RLSStatusTranslated
		policy.Inline = true
		expr = "TRUE"
	} else {
		var reason string
		var err error
		expr, functionSQL, reason, err = c.compileRule(collection, ruleName, *rule)
		switch {
		case err != nil:
			policy.Status = RLSStatusDenied
			policy.Reason = err.Error()
			expr = "FALSE"
			functionSQL = ""
		case reason != "":
			policy.Status = RLSStatusDenied
			policy.Reason = reason
			expr = "FALSE"
			functionSQL = ""
		default:
			policy.Status = RLSStatusTranslated
			policy.Inline = functionSQL == ""
		}
	}

	roles := rlsRoles
	name := quoteRLSIdent(rlsPolicyName(ruleName))

	var policySQL string
	switch ruleName {
	case RLSRuleList:
		policySQL = fmt.Sprintf(
			"CREATE POLICY %s ON %s AS PERMISSIVE FOR SELECT TO %s USING (COALESCE(current_setting('%s', true), '') <> '%s' AND (%s));\n",
			name, table, roles, RLSRuleSetting, RLSRuleView, expr,
		)
	case RLSRuleView:
		policySQL = fmt.Sprintf(
			"CREATE POLICY %s ON %s AS PERMISSIVE FOR SELECT TO %s USING (COALESCE(current_setting('%s', true), '') <> '%s' AND (%s));\n",
			name, table, roles, RLSRuleSetting, RLSRuleList, expr,
		)
	case RLSRuleCreate:
		policySQL = fmt.Sprintf("CREATE POLICY %s ON %s AS PERMISSIVE FOR INSERT TO %s WITH CHECK (%s);\n", name, table, roles, expr)
	case RLSRuleUpdate:
		// the API checks the update rule only against the current record
		policySQL = fmt.Sprintf("CREATE POLICY %s ON %s AS PERMISSIVE FOR UPDATE TO %s USING (%s) WITH CHECK (TRUE);\n", name, table, roles, expr)
	case RLSRuleDelete:
		policySQL = fmt.Sprintf("CREATE POLICY %s ON %s AS PERMISSIVE FOR DELETE TO %s USING (%s);\n", name, table, roles, expr)
	}

	policy.SQL = functionSQL + policySQL

	return policy
}

// compileRule compiles a single non-empty rule and returns its policy expression
// and (for the rules with joins) the definition of the function used by the expression.
//
// A non-empty reason is returned for the rules that cannot be translated.
func (c *rlsCompiler) compileRule(collection *Collection, ruleName string, rule string) (expr string, functionSQL string, reason string, err error) {
	// one branch for the guests and one per auth collection if the rule references the auth record
	authBranches := []*Collection{nil}
	if strings.Contains(rule, "@request.auth") {
		authBranches = append(authBranches, c.authCollections...)
	}

	isCreate := ruleName == RLSRuleCreate

	type branch struct {
		auth  *Collection
		where string
		query string
		joins bool
	}

	compileBranches := func(base *Collection) ([]*branch, bool, string, error) {
		branches := make([]*branch, 0, len(authBranches))
		hasJoins := false

		for _, auth := range authBranches {
			where, query, joins, untranslatable, err := c.compileBranch(base, auth, rule)
			if err != nil {
				return nil, false, "", err
			}
			if untranslatable != "" {
				return nil, false, fmt.Sprintf("%q is not available outside of the API", untranslatable), nil
			}

			branches = append(branches, &branch{auth: auth, where: where, query: query, joins: joins})
			if joins {
				hasJoins = true
			}
		}

		return branches, hasJoins, "", nil
	}

	branches, hasJoins, reason, err := compileBranches(collection)
	if err != nil || reason != "" {
		return "", "", reason, err
	}

	if hasJoins && isCreate {
		// the new record is evaluated as a CTE with a different name
		// so that the rule references to the same collection use the actual table
		// (the same as the API create rule check)
		clone := *collection
		clone.Id = collection.Id + rlsNewRecordName
		clone.Name = rlsNewRecordName

		branches, _, reason, err = compileBranches(&clone)
		if err != nil || reason != "" {
			return "", "", reason, err
		}
	}

	// inline policy expression
	// ---
	if !hasJoins {
		if len(branches) == 1 {
			expr, err = renderRLSSQL(branches[0].where, rlsInlineAuthIdExpr)
			return expr, "", "", err
		}

		var b strings.Builder
		b.WriteString("CASE " + rlsInlineAuthCollectionExpr)
		var anon string
		for _, br := range branches {
			rendered, err := renderRLSSQL(br.where, rlsInlineAuthIdExpr)
			if err != nil {
				return "", "", "", err
			}
			if br.auth == nil {
				anon = rendered
				continue
			}
			b.WriteString(" WHEN " + quoteRLSLiteral(br.auth.Id) + " THEN (" + rendered + ")")
		}
		b.WriteString(" ELSE (" + anon + ") END")

		return b.String(), "", "", nil
	}

	// policy function (evaluated as the tables owner)
	// ---
	var body strings.Builder
	if isCreate {
		body.WriteString("WITH " + quoteRLSIdent(rlsNewRecordName) + " AS (SELECT (jsonb_populate_record(NULL::" + quoteRLSIdent(collection.Name) + ", $1)).*)\n")
	}

	if len(branches) == 1 {
		rendered, err := renderRLSSQL(branches[0].query, rlsFuncAuthIdExpr)
		if err != nil {
			return "", "", "", err
		}
		body.WriteString("SELECT EXISTS (" + rendered + ")")
	} else {
		body.WriteString("SELECT CASE " + rlsFuncAuthCollectionExpr)
		var anon string
		for _, br := range branches {
			rendered, err := renderRLSSQL(br.query, rlsFuncAuthIdExpr)
			if err != nil {
				return "", "", "", err
			}
			if br.auth == nil {
				anon = rendered
				continue
			}
			body.WriteString("\n  WHEN " + quoteRLSLiteral(br.auth.Id) + " THEN EXISTS (" + rendered + ")")
		}
		body.WriteString("\n  ELSE EXISTS (" + anon + ") END")
		body.WriteString("\nFROM (SELECT " + RLSSchema + ".auth() AS a) AS " + rlsAuthAlias)
	}

	fn := quoteRLSIdent(RLSSchema) + "." + quoteRLSIdent(rlsFunctionName(collection.Id, ruleName))
	argType := "text"
	arg := quoteRLSIdent(collection.Name) + "." + quoteRLSIdent(FieldNameId)
	if isCreate {
		argType = "jsonb"
		arg = "to_jsonb(" + quoteRLSIdent(collection.Name) + ".*)"
	}

	functionSQL = fmt.Sprintf(
		// note: pg_temp is the last schema so that the caller temporary tables can't shadow the collection tables
		"CREATE FUNCTION %s(%s) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, %s, pg_temp AS %s;\n"+
			"REVOKE ALL ON FUNCTION %s(%s) FROM PUBLIC;\n"+
			"GRANT EXECUTE ON FUNCTION %s(%s) TO %s;\n",
		fn, argType, quoteRLSIdent(c.schema), dollarQuote(body.String()),
		fn, argType,
		fn, argType, rlsRoles,
	)

	return fn + "(" + arg + ")", functionSQL, "", nil
}

// compileBranch builds the rule expression for a single auth collection (or the guests).
//
// It returns the expression, the full rule query of the policy functions
// (the record id is the $1 argument) and whether the rule needs joins.
func (c *rlsCompiler) compileBranch(base *Collection, auth *Collection, rule string) (where string, query string, joins bool, untranslatable string, err error) {
	info := &RequestInfo{
		Context: RequestInfoContextDefault,
		Query:   map[string]string{},
		Headers: map[string]string{},
		Body:    map[string]any{},
	}

	if auth != nil {
		record := NewRecord(auth)
		record.Id = rlsAuthIdPlaceholder
		info.Auth = record
	}

	resolver := NewRecordFieldResolver(c.app, base, info, true)
	state := &rlsResolverState{collections: c.collections}
	resolver.rls = state

	expr, buildErr := search.FilterData(rule).BuildExpr(resolver)
	if state.untranslatable != "" {
		return "", "", false, state.untranslatable, nil
	}
	if buildErr != nil {
		return "", "", false, "", buildErr
	}

	params := dbx.Params{}
	where = expr.Build(rlsDB, params)
	where, err = inlineRLSParams(where, params)
	if err != nil {
		return "", "", false, "", err
	}

	baseAlias := inflector.Columnify(base.Name)

	q := rlsDB.Select("(1)").From(base.Name).AndWhere(expr)
	if base.Name != rlsNewRecordName {
		q.AndWhere(dbx.NewExp("[[" + baseAlias + "." + FieldNameId + "]] = $1"))
	}
	if err := resolver.UpdateQuery(q); err != nil {
		return "", "", false, "", err
	}

	built := q.Build()
	query, err = inlineRLSParams(built.SQL(), built.Params())
	if err != nil {
		return "", "", false, "", err
	}

	joins = len(resolver.joins) > 0 || len(resolver.listRuleJoins) > 0

	return where, query, joins, "", nil
}

var rlsParamRegex = regexp.MustCompile(`\{:\w+\}`)

// inlineRLSParams replaces the named params of the provided SQL with literals
// (the policies and their functions can't have bound params).
//
// The auth id placeholder is kept and replaced later by renderRLSSQL.
func inlineRLSParams(sql string, params dbx.Params) (string, error) {
	var inlineErr error

	result := rlsParamRegex.ReplaceAllStringFunc(sql, func(m string) string {
		name := m[2 : len(m)-1]

		v, ok := params[name]
		if !ok {
			inlineErr = errors.Join(inlineErr, fmt.Errorf("missing param %q", name))
			return m
		}

		literal, err := rlsLiteral(v)
		if err != nil {
			inlineErr = errors.Join(inlineErr, err)
			return m
		}

		return literal
	})

	return result, inlineErr
}

var rlsQuoteRegex = regexp.MustCompile(`(\{\{[\w\-\. ]+\}\}|\[\[[\w\-\. ]+\]\])`)

// rlsRandomAliasRegex matches the random aliases of the filter multi-match subqueries.
var rlsRandomAliasRegex = regexp.MustCompile(`\b__(?:sm|ml|mr)[0-9A-Za-z]{8}\b`)

// rlsAuthIdLiteral is the quoted auth id placeholder.
var rlsAuthIdLiteral = quoteRLSLiteral(rlsAuthIdPlaceholder)

// renderRLSSQL quotes the table and column names of the provided SQL and
// replaces the auth id placeholder with the provided expression.
func renderRLSSQL(sql string, authIdExpr string) (string, error) {
	sql = rlsQuoteRegex.ReplaceAllStringFunc(sql, func(m string) string {
		if m[0] == '{' {
			return rlsDB.QuoteTableName(m[2 : len(m)-2])
		}
		return rlsDB.QuoteColumnName(m[2 : len(m)-2])
	})

	sql = strings.ReplaceAll(sql, rlsAuthIdLiteral, authIdExpr)

	// replace the random multi-match subquery aliases with sequential ones
	// so that the same rule always produces the same SQL (and definition hash)
	aliases := map[string]string{}
	sql = rlsRandomAliasRegex.ReplaceAllStringFunc(sql, func(m string) string {
		if alias, ok := aliases[m]; ok {
			return alias
		}
		alias := fmt.Sprintf("%s%d", m[:4], len(aliases)+1)
		aliases[m] = alias
		return alias
	})

	// the placeholder is part of a bigger (and possibly escaped) value, eg. a LIKE pattern
	if strings.ContainsRune(sql, '\x01') {
		return "", errors.New("the auth record id can't be part of another value")
	}

	return sql, nil
}

// rlsLiteral returns the SQL literal of the provided param value.
//
// The values are rendered as untyped string literals so that Postgres
// infers their type from the context, the same as the untyped bound params.
func rlsLiteral(v any) (string, error) {
	if valuer, ok := v.(driver.Valuer); ok && valuer != nil {
		var err error
		v, err = valuer.Value()
		if err != nil {
			return "", err
		}
	}

	switch val := v.(type) {
	case nil:
		return "NULL", nil
	case []byte:
		return quoteRLSLiteral(string(val)), nil
	case bool:
		if val {
			return "'true'", nil
		}
		return "'false'", nil
	}

	str, err := cast.ToStringE(v)
	if err != nil {
		return "", fmt.Errorf("unsupported param value %T", v)
	}

	if strings.ContainsRune(str, 0) {
		return "", errors.New("unsupported NUL character")
	}

	return quoteRLSLiteral(str), nil
}

// quoteRLSLiteral quotes the provided string as a SQL string literal.
//
// The characters that dbx treats as placeholders or name quotes ({ and [)
// are escaped because the generated SQL is executed through dbx.
func quoteRLSLiteral(str string) string {
	if !strings.ContainsAny(str, `\{[`) {
		return "'" + strings.ReplaceAll(str, "'", "''") + "'"
	}

	r := strings.NewReplacer(`\`, `\\`, `'`, `''`, `{`, `\x7b`, `[`, `\x5b`)

	return "E'" + r.Replace(str) + "'"
}

// quoteRLSIdent quotes the provided identifier.
func quoteRLSIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// dollarQuote returns the provided function body in a dollar quoted string.
func dollarQuote(body string) string {
	tag := "$rls$"
	for i := 0; strings.Contains(body, tag); i++ {
		tag = fmt.Sprintf("$rls%d$", i)
	}

	return tag + "\n" + body + "\n" + tag
}

// -------------------------------------------------------------------

// rlsTranslatableCache caches the RLSRuleTranslatable results
// (the result depends only on the collection and the rule).
var rlsTranslatableCache sync.Map

// RLSRuleTranslatable reports whether the provided collection rule can be
// enforced by its RLS policy (it is not null and doesn't reference
// request data that is available only in the API).
func RLSRuleTranslatable(app App, collection *Collection, ruleName string) bool {
	var rule *string
	switch ruleName {
	case RLSRuleList:
		rule = collection.ListRule
	case RLSRuleView:
		rule = collection.ViewRule
	case RLSRuleCreate:
		rule = collection.CreateRule
	case RLSRuleUpdate:
		rule = collection.UpdateRule
	case RLSRuleDelete:
		rule = collection.DeleteRule
	}

	if rule == nil || isRLSExcluded(collection) {
		return false
	}

	key := collection.Id + "|" + ruleName + "|" + *rule
	if v, ok := rlsTranslatableCache.Load(key); ok {
		return v.(bool)
	}

	collections, _ := app.Store().Get(StoreKeyCachedCollections).([]*Collection)
	if collections == nil {
		var err error
		collections, err = app.FindAllCollections()
		if err != nil {
			return false
		}
	}

	compiler := newRLSCompiler(app, collections, "")
	policy := compiler.compilePolicy(collection, ruleName, rule)
	ok := policy.Status == RLSStatusTranslated

	rlsTranslatableCache.Store(key, ok)

	return ok
}
