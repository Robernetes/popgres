# MCP server

popgres has a built-in [Model Context Protocol](https://modelcontextprotocol.io) server, so that AI agents and assistants can work with the collections.

It is disabled by default. Enable it in the dashboard (Settings > Application > MCP server) or with the settings API (`{"mcp": {"enabled": true}}`). When disabled, `/mcp` responds with 404.

## Connecting

- Endpoint: `https://<your-app>/mcp` (Streamable HTTP transport).
- Authentication: a popgres auth token of a superuser or of an auth record, in the `Authorization` header (`Bearer <token>` or the raw token). Without a token the endpoint responds with 401.
- The server is stateless (no sessions), so it works with multiple app instances behind a load balancer.

Example for a client configuration:

```json
{
  "mcpServers": {
    "popgres": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": { "Authorization": "Bearer <auth token>" }
    }
  }
}
```

To try it, run the [MCP Inspector](https://github.com/modelcontextprotocol/inspector) (`npx @modelcontextprotocol/inspector`), select the "Streamable HTTP" transport, enter the `/mcp` URL and add the `Authorization` header.

## Tools

The tools are generated for the non-system collections and the operations allowed to the authenticated record (a `null` rule means superusers only):

| Tool | Arguments | REST API request |
|---|---|---|
| `list_<collection>` | `filter`, `sort`, `page`, `perPage`, `skipTotal`, `expand`, `fields` | `GET /api/collections/<collection>/records` |
| `get_<collection>` | `id`, `expand`, `fields` | `GET /api/collections/<collection>/records/<id>` |
| `create_<collection>` | `data`, `expand`, `fields` | `POST /api/collections/<collection>/records` |
| `update_<collection>` | `id`, `data`, `expand`, `fields` | `PATCH /api/collections/<collection>/records/<id>` |
| `delete_<collection>` | `id` | `DELETE /api/collections/<collection>/records/<id>` |
| `vector_search_<collection>` | `field`, `vector`, `k`, `filter`, `expand`, `fields` | `POST /api/collections/<collection>/vector-search` |

Each tool executes the REST API request internally with the same auth token, so the same handlers, API rules, hooks, validations and rate limits apply. The result is the JSON response of the API; the error responses are returned as tool errors. The views only have the list and get tools. File uploads are not supported.

## Resources

`popgres://schema` describes the collections, their fields (with their options, the hidden fields only for the superusers) and the operations allowed to the authenticated record.
