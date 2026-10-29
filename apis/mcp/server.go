// Package mcp implements the popgres MCP (Model Context Protocol) server.
//
// The server tools are generated from the collections and they execute the
// same REST API requests (handlers, rules, hooks and rate limits) with the
// auth token of the MCP request.
package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Robernetes/popgres/core"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SchemaResourceURI is the URI of the resource that describes the collections.
const SchemaResourceURI = "popgres://schema"

// Dispatcher executes an internal REST API request with the auth of the
// MCP request and returns the response status code and body.
type Dispatcher func(ctx context.Context, method string, path string, body any) (int, []byte, error)

// NewServer creates a new MCP server with the tools of the collections
// that the provided auth record can access.
func NewServer(app core.App, auth *core.Record, version string, dispatch Dispatcher) (*sdk.Server, error) {
	server := sdk.NewServer(
		&sdk.Implementation{Name: "popgres", Version: version},
		&sdk.ServerOptions{
			Instructions: "popgres (PocketBase compatible backend on Postgres) collections. " +
				"Read the " + SchemaResourceURI + " resource for the collections and their fields. " +
				"The tools follow the collection API rules of the authenticated record. " +
				"The filter and sort arguments use the PocketBase filter syntax (eg. title ~ 'abc' && created > '2026-01-01').",
		},
	)

	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}

	isSuperuser := auth != nil && auth.IsSuperuser()

	visible := make([]*core.Collection, 0, len(collections))
	for _, c := range collections {
		if c.System || strings.HasPrefix(c.Name, "_") {
			continue
		}
		visible = append(visible, c)
	}

	for _, c := range visible {
		registerCollectionTools(server, c, isSuperuser, dispatch)
	}

	server.AddResource(
		&sdk.Resource{
			URI:         SchemaResourceURI,
			Name:        "schema",
			Description: "The collections, their fields and the available operations.",
			MIMEType:    "application/json",
		},
		func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			raw, err := json.Marshal(schemaInfo(visible, isSuperuser), json.Deterministic(true))
			if err != nil {
				return nil, err
			}

			return &sdk.ReadResourceResult{
				Contents: []*sdk.ResourceContents{{
					URI:      SchemaResourceURI,
					MIMEType: "application/json",
					Text:     string(raw),
				}},
			}, nil
		},
	)

	return server, nil
}

func allowed(rule *string, isSuperuser bool) bool {
	return isSuperuser || rule != nil
}

func vectorFields(c *core.Collection) []string {
	var names []string
	for _, f := range c.Fields {
		if f.Type() == core.FieldTypeVector {
			names = append(names, f.GetName())
		}
	}
	return names
}

func registerCollectionTools(server *sdk.Server, c *core.Collection, isSuperuser bool, dispatch Dispatcher) {
	name := c.Name
	recordsPath := "/api/collections/" + url.PathEscape(name) + "/records"

	enrichProps := map[string]any{
		"expand": map[string]any{"type": "string", "description": "Comma separated relation fields to expand (eg. author,tags)."},
		"fields": map[string]any{"type": "string", "description": "Comma separated fields to return (eg. id,title,expand.author.name)."},
	}

	withEnrich := func(props map[string]any) map[string]any {
		for k, v := range enrichProps {
			props[k] = v
		}
		return props
	}

	idProp := map[string]any{"type": "string", "description": "The record id."}
	dataProp := map[string]any{
		"type":                 "object",
		"description":          "The record fields data (see the " + SchemaResourceURI + " resource). File uploads are not supported.",
		"additionalProperties": true,
	}

	if allowed(c.ListRule, isSuperuser) {
		server.AddTool(&sdk.Tool{
			Name:        "list_" + name,
			Description: "Lists the records of the " + name + " collection (paginated).",
			InputSchema: objectSchema(withEnrich(map[string]any{
				"filter":    map[string]any{"type": "string", "description": "Records filter (eg. status = 'active' && created > '2026-01-01')."},
				"sort":      map[string]any{"type": "string", "description": "Sort fields (eg. -created,title)."},
				"page":      map[string]any{"type": "integer", "minimum": 1},
				"perPage":   map[string]any{"type": "integer", "minimum": 1, "maximum": 1000},
				"skipTotal": map[string]any{"type": "boolean", "description": "Skip the total count (faster)."},
			})),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args, err := parseArgs(req)
			if err != nil {
				return errorResult(err), nil
			}
			query := queryFromArgs(args, "filter", "sort", "page", "perPage", "skipTotal", "expand", "fields")
			return dispatchResult(ctx, dispatch, http.MethodGet, recordsPath+query, nil)
		})
	}

	if allowed(c.ViewRule, isSuperuser) {
		server.AddTool(&sdk.Tool{
			Name:        "get_" + name,
			Description: "Returns a single record of the " + name + " collection by its id.",
			InputSchema: objectSchema(withEnrich(map[string]any{"id": idProp}), "id"),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args, err := parseArgs(req)
			if err != nil {
				return errorResult(err), nil
			}
			id, err := requiredString(args, "id")
			if err != nil {
				return errorResult(err), nil
			}
			query := queryFromArgs(args, "expand", "fields")
			return dispatchResult(ctx, dispatch, http.MethodGet, recordsPath+"/"+url.PathEscape(id)+query, nil)
		})
	}

	if c.IsView() {
		return // read only
	}

	if allowed(c.CreateRule, isSuperuser) {
		server.AddTool(&sdk.Tool{
			Name:        "create_" + name,
			Description: "Creates a new record in the " + name + " collection.",
			InputSchema: objectSchema(withEnrich(map[string]any{"data": dataProp}), "data"),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args, err := parseArgs(req)
			if err != nil {
				return errorResult(err), nil
			}
			query := queryFromArgs(args, "expand", "fields")
			return dispatchResult(ctx, dispatch, http.MethodPost, recordsPath+query, args["data"])
		})
	}

	if allowed(c.UpdateRule, isSuperuser) {
		server.AddTool(&sdk.Tool{
			Name:        "update_" + name,
			Description: "Updates a record of the " + name + " collection (only the provided fields are changed).",
			InputSchema: objectSchema(withEnrich(map[string]any{"id": idProp, "data": dataProp}), "id", "data"),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args, err := parseArgs(req)
			if err != nil {
				return errorResult(err), nil
			}
			id, err := requiredString(args, "id")
			if err != nil {
				return errorResult(err), nil
			}
			query := queryFromArgs(args, "expand", "fields")
			return dispatchResult(ctx, dispatch, http.MethodPatch, recordsPath+"/"+url.PathEscape(id)+query, args["data"])
		})
	}

	if allowed(c.DeleteRule, isSuperuser) {
		server.AddTool(&sdk.Tool{
			Name:        "delete_" + name,
			Description: "Deletes a record of the " + name + " collection.",
			InputSchema: objectSchema(map[string]any{"id": idProp}, "id"),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args, err := parseArgs(req)
			if err != nil {
				return errorResult(err), nil
			}
			id, err := requiredString(args, "id")
			if err != nil {
				return errorResult(err), nil
			}
			return dispatchResult(ctx, dispatch, http.MethodDelete, recordsPath+"/"+url.PathEscape(id), nil)
		})
	}

	if fields := vectorFields(c); len(fields) > 0 && allowed(c.ListRule, isSuperuser) {
		server.AddTool(&sdk.Tool{
			Name:        "vector_search_" + name,
			Description: "Returns the records of the " + name + " collection with the vector field closest to the provided vector (the embeddings are not generated by popgres).",
			InputSchema: objectSchema(withEnrich(map[string]any{
				"field":  map[string]any{"type": "string", "enum": fields},
				"vector": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
				"k":      map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "description": "The number of records (default 10)."},
				"filter": map[string]any{"type": "string", "description": "Optional records filter."},
			}), "field", "vector"),
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			args, err := parseArgs(req)
			if err != nil {
				return errorResult(err), nil
			}
			query := queryFromArgs(args, "expand", "fields")
			body := map[string]any{"field": args["field"], "vector": args["vector"], "k": args["k"], "filter": args["filter"]}
			return dispatchResult(ctx, dispatch, http.MethodPost, "/api/collections/"+url.PathEscape(name)+"/vector-search"+query, body)
		})
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func parseArgs(req *sdk.CallToolRequest) (map[string]any, error) {
	args := map[string]any{}

	if req.Params == nil || len(req.Params.Arguments) == 0 {
		return args, nil
	}

	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	return args, nil
}

func requiredString(args map[string]any, key string) (string, error) {
	v, _ := args[key].(string)
	if v == "" {
		return "", fmt.Errorf("missing required %q argument", key)
	}
	return v, nil
}

// queryFromArgs returns the url query string of the provided args.
func queryFromArgs(args map[string]any, keys ...string) string {
	query := url.Values{}

	for _, key := range keys {
		switch v := args[key].(type) {
		case nil:
			continue
		case string:
			if v != "" {
				query.Set(key, v)
			}
		case bool:
			query.Set(key, strconv.FormatBool(v))
		case float64:
			query.Set(key, strconv.FormatFloat(v, 'f', -1, 64))
		default:
			query.Set(key, fmt.Sprint(v))
		}
	}

	if len(query) == 0 {
		return ""
	}

	return "?" + query.Encode()
}

func dispatchResult(ctx context.Context, dispatch Dispatcher, method string, path string, body any) (*sdk.CallToolResult, error) {
	status, raw, err := dispatch(ctx, method, path, body)
	if err != nil {
		return errorResult(err), nil
	}

	text := string(raw)
	if status == http.StatusNoContent {
		text = `{"status":204}`
	}

	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: text}},
		IsError: status >= 400,
	}, nil
}

func errorResult(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}},
		IsError: true,
	}
}

type schemaField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Options  any    `json:"options,omitempty"`
}

type schemaCollection struct {
	Name       string        `json:"name"`
	Type       string        `json:"type"`
	Fields     []schemaField `json:"fields"`
	Operations []string      `json:"operations"`
}

// schemaInfo describes the collections for the schema resource
// (the hidden fields and the collections without any allowed operation
// are listed only for the superusers).
func schemaInfo(collections []*core.Collection, isSuperuser bool) []schemaCollection {
	result := make([]schemaCollection, 0, len(collections))

	for _, c := range collections {
		item := schemaCollection{Name: c.Name, Type: c.Type, Fields: []schemaField{}, Operations: []string{}}

		for _, f := range c.Fields {
			if f.GetHidden() && !isSuperuser {
				continue
			}

			field := schemaField{Name: f.GetName(), Type: f.Type()}

			// the field options without the common properties
			raw, err := json.Marshal(f)
			if err == nil {
				options := map[string]any{}
				if json.Unmarshal(raw, &options) == nil {
					if required, ok := options["required"].(bool); ok {
						field.Required = required
					}
					for _, key := range []string{"id", "name", "type", "system", "hidden", "presentable", "required", "help"} {
						delete(options, key)
					}
					if len(options) > 0 {
						field.Options = options
					}
				}
			}

			item.Fields = append(item.Fields, field)
		}

		ops := map[string]*string{"list": c.ListRule, "view": c.ViewRule}
		if !c.IsView() {
			ops["create"] = c.CreateRule
			ops["update"] = c.UpdateRule
			ops["delete"] = c.DeleteRule
		}
		for _, op := range []string{"list", "view", "create", "update", "delete"} {
			if rule, ok := ops[op]; ok && allowed(rule, isSuperuser) {
				item.Operations = append(item.Operations, op)
			}
		}
		if len(vectorFields(c)) > 0 && slices.Contains(item.Operations, "list") {
			item.Operations = append(item.Operations, "vectorSearch")
		}

		if len(item.Operations) == 0 {
			continue // superusers only collection
		}

		result = append(result, item)
	}

	return result
}
