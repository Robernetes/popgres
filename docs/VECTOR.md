# Vector search

popgres has a `vector` field type for storing embeddings with [pgvector](https://github.com/pgvector/pgvector), and a similarity search that applies the collection API rules.

popgres doesn't generate the embeddings: send them from the client or compute them in a hook.

## Requirements

The field requires the pgvector extension on the Postgres server.

- The first vector field creates it (`CREATE EXTENSION IF NOT EXISTS vector`). pgvector is not a trusted extension, so only a superuser can create it. If the app database role can't, the field validation shows the SQL that a database administrator must execute (`CREATE EXTENSION vector;`).
- The `pgvector/pgvector` Docker images include it.
- The embedded Postgres of the dev mode (`serve --dev` without a `DATABASE_URL`) doesn't include it: the field validation explains it. Use a Postgres server with pgvector instead.

## The vector field

| Option | Description |
|---|---|
| `dimensions` | The number of the vector items (1-16000). It can't be changed after the field is created. |
| `metric` | The distance metric of the search and the index: `cosine` (default), `l2` or `ip` (inner product). |
| `index` | Creates an HNSW index for the metric (up to 2000 dimensions). |
| `required` | Requires a non-empty vector. |

The column type is `vector(<dimensions>)`. The value is a JSON array of numbers (float32); an empty vector is stored as `NULL` and returned as `null`.

```js
await pb.collections.create({
    name: "docs",
    type: "base",
    fields: [
        { name: "content", type: "text" },
        { name: "embedding", type: "vector", dimensions: 1536, metric: "cosine", index: true },
    ],
});

await pb.collection("docs").create({ content: "...", embedding: [0.012, -0.03, ...] });
```

## Vector search endpoint

```http
POST /api/collections/{collection}/vector-search?expand=...&fields=...
Content-Type: application/json

{"field": "embedding", "vector": [0.01, -0.02, ...], "k": 10, "filter": "status = 'published'"}
```

| Body | Description |
|---|---|
| `field` | The vector field (required). The hidden fields can be searched only by the superusers. |
| `vector` | The query vector, with the field dimensions (required). |
| `k` | The number of records (1-1000, default 10). |
| `filter` | Optional records filter (the same syntax as the list API, the `@collection.*` and `@request.*` fields only for the superusers). |

- The collection list rule is applied (only the superusers can search a collection with a `null` list rule).
- The records without a vector are excluded.
- The response has the format of a records list with `skipTotal` (`page` 1, `perPage` = `k`, `totalItems` and `totalPages` -1), ordered by distance.
- Each record has an extra `score` property, higher is closer: the cosine similarity (`1 - cosine distance`), the inner product, or `1 / (1 + l2 distance)`.
- `expand` and `fields` work as in the list API.

```js
const result = await pb.send("/api/collections/docs/vector-search", {
    method: "POST",
    body: { field: "embedding", vector: queryEmbedding, k: 5 },
});
```

## Sorting by distance

The records list can be sorted by the distance to a vector with the special `@vectorDistance(field, [numbers...], [metric])` sort field (cosine by default):

```
GET /api/collections/docs/records?sort=@vectorDistance(embedding,[0.01,-0.02,...]),-created
```

## MCP

With the MCP server enabled, the collections with vector fields have a `vector_search_<collection>` tool (see [MCP.md](MCP.md)).
