package core

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/security"
	"github.com/pocketbase/dbx"
)

// rlsStateParamKey is the _params key with the hashes of the applied RLS definitions.
const rlsStateParamKey = "popgresRLS"

// rlsStateErrorPrefix marks the collections whose RLS definition failed to apply
// (they are retried on the next sync).
const rlsStateErrorPrefix = "error:"

// ErrRLSRolesPermission is returned when the external RLS roles can't be created
// or granted by the app connection role.
var ErrRLSRolesPermission = errors.New("popgres: the app database role can't create or use the RLS roles")

// rlsRolesAdminSQL is the SQL that a database administrator must execute
// when the app role doesn't have the CREATEROLE attribute.
const rlsRolesAdminSQL = `CREATE ROLE popgres_anon NOLOGIN NOINHERIT;
CREATE ROLE popgres_authenticated NOLOGIN NOINHERIT;
CREATE ROLE popgres_api NOLOGIN NOINHERIT;
GRANT popgres_anon, popgres_authenticated, popgres_api TO <app role> WITH INHERIT FALSE, SET TRUE;`

// rlsRolesSQL creates the external RLS roles (if missing) and grants them to the current role.
const rlsRolesSQL = `
	DO $popgres$
	DECLARE
		r text;
	BEGIN
		FOREACH r IN ARRAY ARRAY['` + RLSAnonRole + `', '` + RLSAuthenticatedRole + `', '` + RLSAPIRole + `'] LOOP
			-- the roles are global to the server and could be created by another popgres database
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
				BEGIN
					EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOINHERIT', r);
				EXCEPTION WHEN duplicate_object OR unique_violation THEN
					NULL;
				END;
			END IF;

			-- the non-superuser app role needs the SET option to switch to the
			-- external roles (eg. in the strict mode); since PG16 it is not
			-- implicitly granted to the role creator
			IF NOT (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) AND NOT pg_has_role(current_user, r, 'SET') THEN
				BEGIN
					EXECUTE format('GRANT %I TO %I WITH INHERIT FALSE, SET TRUE', r, current_user);
				EXCEPTION WHEN unique_violation THEN
					NULL;
				END;
			END IF;
		END LOOP;
	END
	$popgres$;
`

// rlsRolesError wraps the error of rlsRolesSQL with the SQL that an administrator must execute.
func rlsRolesError(err error) error {
	return fmt.Errorf(
		"%w (%v).\nAsk a database administrator to execute:\n%s",
		ErrRLSRolesPermission, err, rlsRolesAdminSQL,
	)
}

// InstallRLS creates the external RLS roles (if missing), the popgres_rls schema
// with the auth functions and the policies of all collections.
//
// It is executed by the popgres RLS system migration (and again after a backup
// restore, see [BaseApp.repairRLS]). It is idempotent.
func InstallRLS(txApp App) error {
	_, err := txApp.DB().NewQuery(rlsRolesSQL).Execute()
	if err != nil {
		return rlsRolesError(err)
	}

	schema, err := currentSchema(txApp)
	if err != nil {
		return err
	}

	var cryptoSchema string
	err = txApp.DB().NewQuery("SELECT extnamespace::regnamespace::text FROM pg_extension WHERE extname = 'pgcrypto'").Row(&cryptoSchema)
	if err != nil {
		return fmt.Errorf("the pgcrypto extension is required: %w", err)
	}

	searchPath := "pg_catalog, " + quoteRLSIdent(schema)
	if cryptoSchema != schema && cryptoSchema != "pg_catalog" {
		searchPath += ", " + cryptoSchema
	}
	searchPath += ", pg_temp"

	roles := rlsRoles

	_, err = txApp.DB().NewQuery(`
		CREATE SCHEMA IF NOT EXISTS ` + RLSSchema + `;
		REVOKE ALL ON SCHEMA ` + RLSSchema + ` FROM PUBLIC;
		GRANT USAGE ON SCHEMA ` + RLSSchema + ` TO ` + roles + `;

		CREATE OR REPLACE FUNCTION ` + RLSSchema + `.base64url_decode(input text) RETURNS bytea
		LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $popgres$
			SELECT decode(rpad(translate(input, '-_', '+/'), length(input) + (4 - length(input) % 4) % 4, '='), 'base64')
		$popgres$;

		CREATE OR REPLACE FUNCTION ` + RLSSchema + `.base64url_encode(input bytea) RETURNS text
		LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $popgres$
			SELECT rtrim(translate(encode(input, 'base64'), E'+/\n', '-_'), '=')
		$popgres$;

		-- auth returns the auth record of the current popgres.token setting
		-- ({id, collectionId, collectionName}) or NULL if missing or invalid.
		--
		-- The token is verified on every call because any role can change
		-- the setting (the same checks as the app: HS256 signature with the
		-- record tokenKey + collection secret, auth type and expiration).
		CREATE OR REPLACE FUNCTION ` + RLSSchema + `.auth() RETURNS jsonb
		LANGUAGE plpgsql STABLE SECURITY DEFINER
		SET search_path = ` + searchPath + ` AS $popgres$
		DECLARE
			token text := current_setting('` + RLSTokenSetting + `', true);
			parts text[];
			header jsonb;
			payload jsonb;
			collection_id text;
			collection_name text;
			secret text;
			token_key text;
		BEGIN
			IF token IS NULL OR token = '' THEN
				RETURN NULL;
			END IF;

			parts := string_to_array(token, '.');
			IF array_length(parts, 1) IS DISTINCT FROM 3 THEN
				RETURN NULL;
			END IF;

			BEGIN
				header := convert_from(` + RLSSchema + `.base64url_decode(parts[1]), 'UTF8')::jsonb;
				payload := convert_from(` + RLSSchema + `.base64url_decode(parts[2]), 'UTF8')::jsonb;
			EXCEPTION WHEN others THEN
				RETURN NULL;
			END;

			IF jsonb_typeof(header) IS DISTINCT FROM 'object' OR jsonb_typeof(payload) IS DISTINCT FROM 'object' THEN
				RETURN NULL;
			END IF;

			IF header ->> 'alg' IS DISTINCT FROM 'HS256' OR payload ->> 'type' IS DISTINCT FROM 'auth' THEN
				RETURN NULL;
			END IF;

			IF jsonb_typeof(payload -> 'exp') IS DISTINCT FROM 'number' OR (payload ->> 'exp')::numeric <= extract(epoch FROM now()) THEN
				RETURN NULL;
			END IF;

			SELECT c.id, c.name, c.options -> 'authToken' ->> 'secret'
			INTO collection_id, collection_name, secret
			FROM ` + quoteRLSIdent(schema) + `._collections c
			WHERE c.id = payload ->> 'collectionId' AND c.type = 'auth';

			IF collection_id IS NULL OR coalesce(secret, '') = '' THEN
				RETURN NULL;
			END IF;

			EXECUTE format('SELECT %I FROM %I.%I WHERE %I = $1', 'tokenKey', ` + quoteRLSLiteral(schema) + `, collection_name, 'id')
			INTO token_key
			USING payload ->> 'id';

			IF token_key IS NULL THEN
				RETURN NULL;
			END IF;

			IF ` + RLSSchema + `.base64url_encode(hmac(
				convert_to(parts[1] || '.' || parts[2], 'UTF8'),
				convert_to(token_key || secret, 'UTF8'),
				'sha256'
			)) IS DISTINCT FROM parts[3] THEN
				RETURN NULL;
			END IF;

			RETURN jsonb_build_object(
				'id', payload ->> 'id',
				'collectionId', collection_id,
				'collectionName', collection_name
			);
		END
		$popgres$;

		-- set_claims verifies the provided auth token and stores it in the
		-- popgres.token setting (of the session or, with is_local, of the transaction).
		CREATE OR REPLACE FUNCTION ` + RLSSchema + `.set_claims(token text, is_local boolean DEFAULT false) RETURNS jsonb
		LANGUAGE plpgsql VOLATILE
		SET search_path = pg_catalog, pg_temp AS $popgres$
		DECLARE
			claims jsonb;
		BEGIN
			PERFORM set_config('` + RLSTokenSetting + `', coalesce(token, ''), is_local);

			IF coalesce(token, '') = '' THEN
				RETURN NULL;
			END IF;

			claims := ` + RLSSchema + `.auth();
			IF claims IS NULL THEN
				PERFORM set_config('` + RLSTokenSetting + `', '', is_local);
				RAISE EXCEPTION 'invalid or expired popgres auth token' USING ERRCODE = '28000';
			END IF;

			RETURN claims;
		END
		$popgres$;

		REVOKE ALL ON ALL FUNCTIONS IN SCHEMA ` + RLSSchema + ` FROM PUBLIC;
		GRANT EXECUTE ON FUNCTION ` + RLSSchema + `.auth(), ` + RLSSchema + `.set_claims(text, boolean) TO ` + roles + `;
	`).Execute()
	if err != nil {
		return fmt.Errorf("failed to create the popgres RLS functions: %w", err)
	}

	return syncRLSPolicies(txApp, true)
}

// repairRLS reinstalls the RLS functions, privileges and policies if RLS is
// installed but its state is missing.
//
// The backup dumps don't include the privileges (they depend on the server roles),
// so the restore removes the RLS state and the next app start reinstalls them.
func (app *BaseApp) repairRLS() error {
	needsRepair := func(db dbx.Builder) (bool, error) {
		var needed bool

		// note: the catalogs are used instead of to_regprocedure because it fails
		// for a role without the schema USAGE privilege
		err := db.NewQuery(`
			SELECT EXISTS (
				SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = {:schema} AND p.proname = 'auth'
			)
			AND NOT EXISTS (SELECT 1 FROM {{_params}} WHERE [[id]] = {:id})
		`).Bind(dbx.Params{"schema": RLSSchema, "id": rlsStateParamKey}).Row(&needed)

		return needed, err
	}

	needed, err := needsRepair(app.DB())
	if err != nil || !needed {
		return err
	}

	return app.RunInTransaction(func(txApp App) error {
		// serialize the nodes started at the same time
		if err := cluster.XactLock(txApp.DB(), "rls_repair"); err != nil {
			return err
		}

		needed, err := needsRepair(txApp.DB())
		if err != nil || !needed {
			return err
		}

		txApp.Logger().Info("Reinstalling the RLS policies and privileges (missing RLS state, eg. after a backup restore)")

		return InstallRLS(txApp)
	})
}

// UninstallRLS drops the policies, the popgres_rls schema and the external roles privileges
// (the roles are kept because they could be used by other databases).
func UninstallRLS(txApp App) error {
	collections, err := txApp.FindAllCollections()
	if err != nil {
		return err
	}

	var b strings.Builder
	for _, c := range collections {
		table := quoteRLSIdent(c.Name)
		if !c.IsView() {
			for _, rule := range []string{RLSRuleList, RLSRuleView, RLSRuleCreate, RLSRuleUpdate, RLSRuleDelete} {
				b.WriteString("DROP POLICY IF EXISTS " + quoteRLSIdent(rlsPolicyName(rule)) + " ON " + table + ";\n")
			}
			b.WriteString("ALTER TABLE " + table + " DISABLE ROW LEVEL SECURITY;\n")
		}
		b.WriteString("REVOKE ALL ON TABLE " + table + " FROM " + rlsRoles + ";\n")
	}
	b.WriteString("DROP SCHEMA IF EXISTS " + RLSSchema + " CASCADE;\n")

	if _, err := txApp.DB().NewQuery(b.String()).Execute(); err != nil {
		return err
	}

	_, err = txApp.DB().Delete("_params", dbx.HashExp{"id": rlsStateParamKey}).Execute()
	return err
}

// SyncRLSPolicies regenerates the RLS policies (and the external roles privileges)
// of the collections whose compiled definition has changed.
//
// It is invoked automatically on collection save and delete.
func SyncRLSPolicies(app App) error {
	return syncRLSPolicies(app, false)
}

func syncRLSPolicies(app App, force bool) error {
	return app.RunInTransaction(func(txApp App) error {
		// not installed yet (eg. a system migration before the RLS one)
		var installed bool
		err := txApp.DB().NewQuery("SELECT to_regprocedure('" + RLSSchema + ".auth()') IS NOT NULL").Row(&installed)
		if err != nil {
			return err
		}
		if !installed {
			return nil
		}

		// serialize the concurrent collection changes (eg. of other nodes)
		_, err = txApp.DB().NewQuery("SELECT pg_advisory_xact_lock(hashtext('popgres_rls'))").Execute()
		if err != nil {
			return err
		}

		collections, err := txApp.FindAllCollections()
		if err != nil {
			return err
		}

		schema, err := currentSchema(txApp)
		if err != nil {
			return err
		}

		oldState, err := loadRLSState(txApp)
		if err != nil {
			return err
		}

		functions, err := listRLSFunctions(txApp)
		if err != nil {
			return err
		}

		compiler := newRLSCompiler(txApp, collections, schema)

		newState := make(map[string]string, len(collections))
		ids := make(map[string]struct{}, len(collections))

		for _, collection := range collections {
			ids[collection.Id] = struct{}{}

			compiled := compiler.compile(collection)
			hash := security.SHA256(compiled.SQL)

			if !force && oldState[collection.Id] == hash {
				newState[collection.Id] = hash
				continue
			}

			err := applyRLSDefinition(txApp, collection, functions[collection.Id], compiled.SQL)
			if err != nil {
				// fail closed and retry on the next sync
				txApp.Logger().Error(
					"Failed to apply the collection RLS policies (the external roles are denied)",
					"collection", collection.Name,
					"error", err.Error(),
				)
				newState[collection.Id] = rlsStateErrorPrefix + hash
				continue
			}

			newState[collection.Id] = hash
		}

		// the functions of the deleted collections
		var orphans strings.Builder
		for id, fns := range functions {
			if _, ok := ids[id]; ok {
				continue
			}
			for _, fn := range fns {
				orphans.WriteString("DROP FUNCTION IF EXISTS " + fn + ";\n")
			}
		}
		if orphans.Len() > 0 {
			if _, err := txApp.DB().NewQuery(orphans.String()).Execute(); err != nil {
				return err
			}
		}

		if !force && rlsStatesEqual(oldState, newState) {
			return nil
		}

		return saveRLSState(txApp, newState)
	})
}

// applyRLSDefinition replaces the policies, functions and privileges of a single collection.
//
// On failure the changes are rolled back to a savepoint and the external roles
// lose all privileges of the collection table.
func applyRLSDefinition(txApp App, collection *Collection, functions []string, definition string) error {
	table := quoteRLSIdent(collection.Name)

	var drop strings.Builder
	if !collection.IsView() {
		for _, rule := range []string{RLSRuleList, RLSRuleView, RLSRuleCreate, RLSRuleUpdate, RLSRuleDelete} {
			drop.WriteString("DROP POLICY IF EXISTS " + quoteRLSIdent(rlsPolicyName(rule)) + " ON " + table + ";\n")
		}
	}
	for _, fn := range functions {
		drop.WriteString("DROP FUNCTION IF EXISTS " + fn + ";\n")
	}

	if _, err := txApp.DB().NewQuery("SAVEPOINT popgres_rls").Execute(); err != nil {
		return err
	}

	_, applyErr := txApp.DB().NewQuery(drop.String() + definition).Execute()
	if applyErr == nil {
		_, err := txApp.DB().NewQuery("RELEASE SAVEPOINT popgres_rls").Execute()
		return err
	}

	_, err := txApp.DB().NewQuery("ROLLBACK TO SAVEPOINT popgres_rls").Execute()
	if err != nil {
		return errors.Join(applyErr, err)
	}

	// deny all
	deny := drop.String() + "REVOKE ALL ON TABLE " + table + " FROM " + rlsRoles + ";\n"
	if !collection.IsView() {
		deny += "ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY;\n"
	}
	_, err = txApp.DB().NewQuery("RELEASE SAVEPOINT popgres_rls;\n" + deny).Execute()

	return errors.Join(applyErr, err)
}

// listRLSFunctions returns the policy functions grouped by their collection id
// (as "schema"."name"(args) signatures).
func listRLSFunctions(txApp App) (map[string][]string, error) {
	rows := []struct {
		Name string `db:"name"`
		Args string `db:"args"`
	}{}

	err := txApp.DB().NewQuery(`
		SELECT p.proname AS name, pg_get_function_identity_arguments(p.oid) AS args
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = {:schema} AND starts_with(p.proname, 'rls_')
	`).Bind(dbx.Params{"schema": RLSSchema}).All(&rows)
	if err != nil {
		return nil, err
	}

	result := map[string][]string{}
	for _, row := range rows {
		id := strings.TrimPrefix(row.Name, "rls_")
		if i := strings.LastIndex(id, "_"); i > 0 {
			id = id[:i]
		}
		result[id] = append(result[id], quoteRLSIdent(RLSSchema)+"."+quoteRLSIdent(row.Name)+"("+row.Args+")")
	}

	return result, nil
}

func loadRLSState(txApp App) (map[string]string, error) {
	var raw string
	err := txApp.DB().Select("value").From("_params").Where(dbx.HashExp{"id": rlsStateParamKey}).Row(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]string{}, nil
		}
		return nil, err
	}

	state := map[string]string{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			// regenerate everything
			return map[string]string{}, nil
		}
	}

	return state, nil
}

func saveRLSState(txApp App, state map[string]string) error {
	raw, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return err
	}

	_, err = txApp.DB().NewQuery(`
		INSERT INTO {{_params}} ([[id]], [[value]]) VALUES ({:id}, {:value})
		ON CONFLICT ([[id]]) DO UPDATE SET [[value]] = EXCLUDED.[[value]], [[updated]] = now()
	`).Bind(dbx.Params{"id": rlsStateParamKey, "value": string(raw)}).Execute()

	return err
}

func rlsStatesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// The application statuses of a compiled RLS definition (see RLSApplyStatus).
const (
	RLSApplied      = "applied"
	RLSApplyFailed  = "failed"
	RLSApplyPending = "pending"
)

// RLSApplyStatus reports whether the provided compiled definition is the
// one applied in the database (RLSApplied), if it failed to apply
// (RLSApplyFailed, the external roles are denied) or if it is not applied yet (RLSApplyPending).
func RLSApplyStatus(app App, compiled *RLSCollection) (string, error) {
	state, err := loadRLSState(app)
	if err != nil {
		return "", err
	}

	hash := security.SHA256(compiled.SQL)

	switch state[compiled.CollectionId] {
	case hash:
		return RLSApplied, nil
	case rlsStateErrorPrefix + hash:
		return RLSApplyFailed, nil
	default:
		return RLSApplyPending, nil
	}
}
