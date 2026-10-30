# Full text search

popgres has a `search` field type for the full text search of the text fields of a collection, with the Postgres [text search](https://www.postgresql.org/docs/current/textsearch.html): word stemming per language, relevance ranking and a GIN index.

It doesn't need any extension and it works with the embedded Postgres of the dev mode.

## The search field

| Option | Description |
|---|---|
| `fields` | The names of the fields to search in (1-8): `text`, `editor`, `email`, `url` or `select` fields of the same collection. Their order sets the relevance weight: the first field has the highest weight (A), then B, C and D for the rest. |
| `language` | The Postgres text search configuration used for the word stemming and the stop words, eg. `english` or `spanish` (see `SELECT cfgname FROM pg_ts_config`). The default `simple` only lowercases the words. |
| `hidden` | Allows the search only for the superusers. It is required if any of the fields to search in is hidden. |

```js
await pb.collections.create({
    name: "posts",
    type: "base",
    listRule: "",
    fields: [
        { name: "title", type: "text" },
        { name: "body", type: "editor" },
        { name: "content", type: "search", fields: ["title", "body"], language: "english" },
    ],
});
```

The field is a [generated column](https://www.postgresql.org/docs/current/ddl-generated-columns.html) (`tsvector GENERATED ALWAYS AS (...) STORED`) with a GIN index:

- It is always in sync with the fields to search in, also for the records changed with SQL (eg. the [direct SQL clients](RLS.md)).
- It has no record value: it is never returned in the records (also not to the superusers) and the values sent to it are ignored.
- The HTML tags of the `editor` fields are not indexed as words.
- A renamed field is renamed in `fields` too. A field used by a search field can't be deleted until it is removed from `fields`.
- Changing `fields` or `language` (or the type of a field to search in, eg. a single to multiple `select`) recreates the column, which recomputes it for all the records of the collection.

The auth collections `email` field can't be searched (its visibility is checked per record).

## Filter

Use the `~` (match) and `!~` (no match) operators with the search field in any filter (the records list, the API rules, the realtime subscriptions, etc.):

```js
const result = await pb.collection("posts").getList(1, 20, {
    filter: pb.filter("content ~ {:q}", { q: "red shoes" }),
});
```

The query uses the [web search syntax](https://www.postgresql.org/docs/current/textsearch-controls.html#TEXTSEARCH-PARSING-QUERIES) of Postgres (`websearch_to_tsquery`):

| Query | Matches the records with |
|---|---|
| `red shoes` | both words (in any order and any of the fields) |
| `"red shoes"` | the phrase |
| `red or blue` | any of the words |
| `shoes -red` | "shoes" but not "red" |

With the `english` language the words are stemmed (eg. `running` matches "run" and "runs"). The empty query matches all the records (the same as `title ~ ''`) and a query with only stop words matches none. The query can have at most 1000 characters.

The `?~` operator is the same as `~`: through a multiple relation (eg. `posts_via_author.content ~ 'shoes'`) the filter matches if at least one of the related records matches.

The search fields support only the `~` and `!~` operators, without modifiers, and the query must be a text value (not another field).

## Sort by relevance

The `@textRank(field, 'query')` sort field sorts by the relevance of the search field for the query (`ts_rank_cd`, higher is more relevant, so sort it descending):

```js
const q = "red shoes";

const result = await pb.collection("posts").getList(1, 20, {
    filter: pb.filter("content ~ {:q}", { q }),
    sort: `-@textRank(content, '${q.replaceAll("'", "")}'),-created`,
});
```

The sort expressions have no params: the query must be single or double quoted without the same quote inside it (a query with a single quote can be double quoted). It is sent to Postgres hex encoded, so it can't change the SQL.
