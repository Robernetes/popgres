# Security policy

## Supported versions

| Version | Supported |
|---|---|
| 1.0.x | Yes |
| < 1.0 | No |

The security fixes are released as patch versions of the latest minor version.

## Reporting a vulnerability

**Please don't open a public issue, discussion or pull request for a security problem.**

Report it privately through GitHub: [**Report a vulnerability**](https://github.com/Robernetes/popgres/security/advisories/new) (the "Security" tab of the repository). Only the maintainers can see the report.

A useful report includes:

- the popgres version, the Postgres version and the relevant settings (eg. `POPGRES_CLUSTER`, `POPGRES_REALTIME_SOURCE`, `POPGRES_RLS_STRICT`);
- the steps to reproduce the problem (requests, collection rules, SQL);
- what an attacker can do with it.

A simple description is enough, there is no need for a formal write up.

## What to expect

- An acknowledgement within a week.
- If the issue is confirmed, a fix is prepared privately and released as a new version, followed by a GitHub security advisory with the affected versions and the remediation steps.
- Credit in the advisory, unless you prefer to stay anonymous.

Please give the users some time to update before publishing the details.

## Scope

popgres is a fork of [PocketBase](https://github.com/pocketbase/pocketbase). Report here the problems that affect popgres, including the code inherited from PocketBase; if a problem also affects PocketBase, we'll coordinate with the PocketBase maintainers. Don't report the popgres specific problems to the PocketBase project.

Especially relevant for popgres:

- the API rules compiled to the Postgres row level security policies (eg. a direct SQL client that reads rows the API rule doesn't allow);
- the realtime events of the logical replication source and the events between the cluster instances;
- the MCP server and its tools;
- the `popgres-import` tool.

The following are not considered vulnerabilities, the same as in PocketBase:

- SQL injection through the low level Go methods that take raw SQL, table or column names (eg. `app.DeleteTable(dangerousName)`): they must not be called with untrusted input.
- The JS hooks (`pb_hooks`) are not a sandbox: they run with the same permissions as the Go code.
- Stored XSS through the record values: the clients must escape or sanitize the values they render.
- The `superuser` accounts and the database credentials have full access by design.

## Verifying a release

Each release (from v1.1.0) has an SPDX SBOM per archive (`*.zip.sbom.json`) and the `checksums.txt` file is signed with [cosign](https://docs.sigstore.dev/) keyless, with the identity of the release workflow of this repository. Verify the signature with cosign v3 or later (the older versions can't read the Sigstore bundle format of the signature) and then the archive checksum:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/Robernetes/popgres/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

sha256sum --ignore-missing -c checksums.txt
```

A release is published only for a tagged commit with a successful CI run.
