# Row level security

popgres compiles the collection API rules to Postgres row level security (RLS) policies, so that the same rules apply to the clients connected directly to the database (BI tools, other services, ETL).

The app connection role owns the tables (or is a superuser) and bypasses the policies, so the app itself behaves the same as PocketBase.

## Roles

The system migration creates three roles if they don't exist (they are server-wide and shared by all popgres databases of the server):

| Role | Used by | Privileges |
|---|---|---|
| `popgres_anon` | external clients without an auth record | the collection policies, the non-hidden columns |
| `popgres_authenticated` | external clients with an auth record | the collection policies, the non-hidden columns |
| `popgres_api` | the API in strict mode (see below) | the collection policies, all columns |

The roles are `NOLOGIN`, without `SUPERUSER` or `BYPASSRLS`. The app connection role is granted `SET` on them (without inheriting their privileges).

Creating the roles requires the `CREATEROLE` attribute. Without it, the migration fails and prints the SQL that a database administrator must execute:

```sql
CREATE ROLE popgres_anon NOLOGIN NOINHERIT;
CREATE ROLE popgres_authenticated NOLOGIN NOINHERIT;
CREATE ROLE popgres_api NOLOGIN NOINHERIT;
GRANT popgres_anon, popgres_authenticated, popgres_api TO <app role> WITH INHERIT FALSE, SET TRUE;
```

## Connecting as an external client

Create a login role for the client and grant it one of the external roles:

```sql
CREATE ROLE reporting LOGIN PASSWORD '...';
GRANT popgres_authenticated TO reporting;
```

Limit the connections and the timeouts of the external login roles (see [Untrusted database roles](CLUSTER.md#untrusted-database-roles)).

Then get an RLS token of the auth record from the API (with its auth token, eg. obtained with `authWithPassword`):

```sh
curl -X POST https://example.com/api/rls/token \
  -H "Authorization: <auth token>" \
  -H "Content-Type: application/json" \
  -d '{"duration": 3600}'   # optional, in seconds (default 900, from 60 to 86400)
# {"token":"<RLS token>"}
```

and set it in the client session (preferably as a query parameter):

```sql
SET ROLE popgres_authenticated;
SELECT popgres_rls.set_claims($1);       -- session; set_claims($1, true) for the current transaction only
SELECT id, title FROM posts;             -- only the records allowed by the list rule
```

- The RLS token is accepted only by `popgres_rls.set_claims` and the API auth tokens are rejected there. The API rejects the RLS tokens, so a token that leaks from a database session (eg. `pg_stat_activity` or the server logs) can't be used against the API.
- `popgres_rls.set_claims` raises an error for an invalid or expired token. The token is stored in the `popgres.token` setting and it is verified again on every query by `popgres_rls.auth()` (HS256 signature with the record token key and the collection secret, token type and expiration with the clock time, so it expires also inside a long transaction). Changing the setting directly with `SET` doesn't bypass the verification. A token is rejected after its record token key changes (eg. after a password change), the same as in the API.
- With a pooler in transaction mode (eg. PgBouncer), set the token for each transaction (`set_claims($1, true)`): a session setting could be used by the next client of the server connection.
- The roles are shared by all popgres databases of the server: revoke the `CONNECT` privilege of the other databases from the client login roles (see [Multiple popgres databases on one server](OPERATIONS.md#multiple-popgres-databases-on-one-server)).

## Rules to policies

| Rule | Policy |
|---|---|
| list | `popgres_list`, `FOR SELECT` |
| view | `popgres_view`, `FOR SELECT`, only for the strict mode role `popgres_api` |
| create | `popgres_create`, `FOR INSERT WITH CHECK` |
| update | `popgres_update`, `FOR UPDATE USING (...) WITH CHECK (true)` (the API checks the rule against the current record) |
| delete | `popgres_delete`, `FOR DELETE USING` |
| `null` (superusers only) | no policy (denied) |
| `""` (public) | `true` |

An external `SELECT` returns only the records allowed by the **list** rule. The view rule allows a single record by its id in the API (eg. a record shared by a secret link), and a `SELECT` could read all records allowed by it, so its policy applies only to the strict mode API role. A collection with a view rule but without a list rule can't be read by the external roles.

The rules without relations are compiled to inline policy expressions (they use the table indexes). The rules with relations, back relations, `@collection` or auth record fields other than `id`, `collectionId` and `collectionName` are evaluated by a `SECURITY DEFINER` function with the tables owner privileges, the same as the API rules (the related collections are not filtered by their own rules). These functions are called for each row, so they are slower for large tables.

The policies are regenerated when any collection is created, updated or deleted. The dashboard shows the generated SQL and the status of each rule in the collection "RLS" tab.

### Rules that can't be translated

The request data that exists only in the API can't be used outside of it:

- `@request.body.*`, `@request.query.*`, `@request.headers.*`, `@request.method` and `@request.context`;
- the `:isset` and `:changed` modifiers.

The policy of a rule that uses them denies all records (`false`) and the dashboard shows a warning.

## Privileges

- `SELECT` of the non-hidden columns if the list rule is not `null`. For the auth collections the `email` column is excluded too (the API checks the email visibility per record).
- `INSERT`, `UPDATE` (non-hidden columns, except `id`) and `DELETE` only for the base collections with a non-`null` rule. The auth records are managed only through the API (password hashes, token keys and verification).
- No privileges on the views, the system collections (`_superusers`, `_externalAuths`, `_mfas`, `_otps`, `_authOrigins`) and the other system tables (`_collections`, `_params`, `_realtime_events`, etc.).

The external writes don't execute the app hooks and the fields validations.

## Strict mode

With `POPGRES_RLS_STRICT=true` the main record query of the API list and view requests of the non-superusers is executed as the `popgres_api` role, with a short-lived RLS token of the auth record and without the rule in the query: the rule is enforced by the RLS policy (the `popgres.rule` setting selects the list or the view policy). It is an extra safeguard against a bug in the API rules filtering.

- It applies only to the rules that can be translated (the others are enforced only by the API, as without strict mode).
- Create, update and delete don't change: the create rule check doesn't read the table, and an RLS `UPDATE`/`DELETE` also requires the `SELECT` policy, which would change the API behavior.
- The expand, the hooks and the realtime events are executed as the app connection role.

## Limitations

- The policies use the collection rules of the database where they were generated. A rule that references another collection is regenerated when that collection changes.
- The policies don't apply to the app connection role (the tables owner) and the database superusers.
