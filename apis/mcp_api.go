package apis

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/Robernetes/popgres/apis/mcp"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tools/router"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// popgres: the MCP server (Streamable HTTP transport).

// MCPPath is the path of the MCP server endpoint.
const MCPPath = "/mcp"

// MCPVersion is the version reported by the MCP server.
var MCPVersion = "dev"

type mcpRequestKey struct{}

// bindMCPApi registers the MCP server endpoint.
//
// The server is stateless (no sessions), so any app instance can handle any
// request. Its tools execute internal REST API requests through the same router
// (handlers, middlewares, rules and hooks) with the auth token of the MCP request.
func bindMCPApi(app core.App, pbRouter *router.Router[*core.RequestEvent]) {
	var muxOnce sync.Once
	var mux http.Handler
	var muxErr error

	getMux := func() (http.Handler, error) {
		// built on the first use so that it includes the routes registered later (eg. in OnServe)
		muxOnce.Do(func() {
			mux, muxErr = pbRouter.BuildMux()
		})
		return mux, muxErr
	}

	handler := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		e, _ := r.Context().Value(mcpRequestKey{}).(*core.RequestEvent)
		if e == nil {
			return nil
		}

		dispatch := func(ctx context.Context, method string, path string, body any) (int, []byte, error) {
			m, err := getMux()
			if err != nil {
				return 0, nil, err
			}

			// note: the server requests always have a body
			var reader io.Reader = http.NoBody
			if body != nil {
				raw, err := json.Marshal(body)
				if err != nil {
					return 0, nil, err
				}
				reader = bytes.NewReader(raw)
			}

			req, err := http.NewRequestWithContext(ctx, method, path, reader)
			if err != nil {
				return 0, nil, err
			}
			req.RemoteAddr = e.Request.RemoteAddr
			req.Header.Set("Authorization", e.Request.Header.Get("Authorization"))
			req.Header.Set("User-Agent", e.Request.Header.Get("User-Agent"))
			for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Cf-Connecting-Ip", "Fly-Client-Ip"} {
				if v := e.Request.Header.Get(h); v != "" {
					req.Header.Set(h, v)
				}
			}
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}

			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, req)

			return rec.Code, rec.Body.Bytes(), nil
		}

		server, err := mcp.NewServer(e.App, e.Auth, MCPVersion, dispatch)
		if err != nil {
			e.App.Logger().Error("Failed to initialize the MCP server", "error", err.Error())
			return nil
		}

		return server
	}, &sdk.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})

	action := func(e *core.RequestEvent) error {
		if !e.App.Settings().MCP.Enabled {
			return e.NotFoundError("", nil)
		}

		if e.Auth == nil {
			return e.UnauthorizedError("The MCP server requires a popgres auth token (Authorization: Bearer <token>).", nil)
		}

		ctx := context.WithValue(e.Request.Context(), mcpRequestKey{}, e)
		handler.ServeHTTP(e.Response, e.Request.WithContext(ctx))

		return nil
	}

	// note: explicit methods (instead of Any) because a method-less pattern
	// conflicts with the "GET /{path...}" static files route
	pbRouter.POST(MCPPath, action)
	pbRouter.GET(MCPPath, action)
	pbRouter.DELETE(MCPPath, action)
}
