package apis

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/router"
	"github.com/Robernetes/popgres/tools/security"
)

// Requests forwarding between the app instances (nodes) in cluster mode.
//
// The realtime clients (the SSE connections) are kept in the memory of the
// node that accepted the connection. A request that needs the client
// (eg. the realtime subscriptions or the OAuth2 redirect) could be received by
// another node behind the load balancer, so it is forwarded to the client node:
//  1. the receiving node stores the request in the shared key-value store and
//     publishes the request id to the client node;
//  2. the client node executes the request with its own router (with all
//     middlewares and hooks), stores the response and publishes the response id;
//  3. the receiving node writes the stored response.
//
// The realtime client ids start with the id of their node (see realtimeClientIdPrefix).

const (
	clusterTopicForwardRequest  = "fwd_req"
	clusterTopicForwardResponse = "fwd_res"

	forwardKVPrefix = "@fwd_"
	forwardTTL      = 30 * time.Second

	// forwardTimeout is the max time to wait for the response of the client node.
	forwardTimeout = 5 * time.Second
)

var errForwardTimeout = errors.New("the forwarded request timed out")

// forwardedContextKey marks the requests executed on behalf of another node
// (they are not forwarded again and they are not rate limited twice).
type forwardedContextKey struct{}

func isForwardedRequest(r *http.Request) bool {
	forwarded, _ := r.Context().Value(forwardedContextKey{}).(bool)
	return forwarded
}

type forwardedRequest struct {
	Method     string              `json:"method"`
	URL        string              `json:"url"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
	RemoteAddr string              `json:"remoteAddr"`
}

type forwardedResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// realtimeClientIdPrefix returns the realtime client id prefix of the current node.
func realtimeClientIdPrefix(app core.App) string {
	return app.Cluster().NodeId() + "_"
}

// realtimeClientNodeId returns the node id from a realtime client id
// (or an empty string if the client id doesn't have one).
func realtimeClientNodeId(clientId string) string {
	nodeId, _, ok := strings.Cut(clientId, "_")
	if !ok {
		return ""
	}

	return nodeId
}

// shouldForwardToClientNode reports whether the request related to the
// specified missing local realtime client should be forwarded to another node.
func shouldForwardToClientNode(e *core.RequestEvent, clientId string) (string, bool) {
	if !e.App.Cluster().Enabled() || isForwardedRequest(e.Request) {
		return "", false
	}

	nodeId := realtimeClientNodeId(clientId)
	if nodeId == "" || nodeId == e.App.Cluster().NodeId() {
		return "", false
	}

	return nodeId, true
}

// forwardRequest forwards the current request (with the provided body)
// to the specified node and writes its response.
func forwardRequest(e *core.RequestEvent, nodeId string, body []byte, contentType string) error {
	headers := e.Request.Header.Clone()
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	headers.Del("Content-Length")

	req := forwardedRequest{
		Method:     e.Request.Method,
		URL:        e.Request.URL.RequestURI(),
		Headers:    headers,
		Body:       body,
		RemoteAddr: e.Request.RemoteAddr,
	}

	rawReq, err := json.Marshal(req)
	if err != nil {
		return err
	}

	reqId := security.RandomStringWithAlphabet(20, "abcdefghijklmnopqrstuvwxyz0123456789")

	wait := forwardWaiters.add(reqId)
	defer forwardWaiters.remove(reqId)

	if err := e.App.Cluster().KV().Set(forwardKVPrefix+"req_"+reqId, string(rawReq), forwardTTL); err != nil {
		return err
	}

	// payload: targetNodeId|replyNodeId|reqId
	payload := nodeId + "|" + e.App.Cluster().NodeId() + "|" + reqId
	if err := e.App.Cluster().Notify(clusterTopicForwardRequest, payload); err != nil {
		return err
	}

	select {
	case <-wait:
	case <-time.After(forwardTimeout):
		_ = e.App.Cluster().KV().Delete(forwardKVPrefix + "req_" + reqId)
		return errForwardTimeout
	case <-e.Request.Context().Done():
		return e.Request.Context().Err()
	}

	rawRes, ok, err := e.App.Cluster().KV().Pop(forwardKVPrefix + "res_" + reqId)
	if err != nil {
		return err
	}
	if !ok {
		return errForwardTimeout
	}

	res := forwardedResponse{}
	if err := json.Unmarshal([]byte(rawRes), &res); err != nil {
		return err
	}

	for k, v := range res.Headers {
		e.Response.Header()[k] = v
	}
	e.Response.WriteHeader(res.Status)
	_, err = e.Response.Write(res.Body)

	return err
}

// forwardWaiters tracks the forwarded requests that wait for a response.
var forwardWaiters = &waiters{items: map[string]chan struct{}{}}

type waiters struct {
	items map[string]chan struct{}
	mu    sync.Mutex
}

func (w *waiters) add(id string) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()

	ch := make(chan struct{})
	w.items[id] = ch

	return ch
}

func (w *waiters) remove(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.items, id)
}

func (w *waiters) notify(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if ch, ok := w.items[id]; ok {
		close(ch)
		delete(w.items, id)
	}
}

// bindClusterForwarding registers the handlers of the requests forwarded by the other nodes.
func bindClusterForwarding(app core.App, pbRouter *router.Router[*core.RequestEvent]) {
	var muxOnce sync.Once
	var mux http.Handler
	var muxErr error

	// the mux is built on the first forwarded request so that it includes
	// the routes and middlewares registered after the router creation (eg. OnServe)
	getMux := func() (http.Handler, error) {
		muxOnce.Do(func() {
			mux, muxErr = pbRouter.BuildMux()
		})
		return mux, muxErr
	}

	app.Cluster().Subscribe(clusterTopicForwardRequest, func(msg cluster.Message) {
		parts := strings.Split(msg.Payload, "|")
		if len(parts) != 3 || parts[0] != app.Cluster().NodeId() {
			return
		}
		replyNodeId, reqId := parts[1], parts[2]

		// note: the request is popped so that it is executed only once
		// (eg. if there are multiple routers of the same app)
		rawReq, ok, err := app.Cluster().KV().Pop(forwardKVPrefix + "req_" + reqId)
		if err != nil || !ok {
			return
		}

		// execute in the background to not block the other cluster messages
		go func() {
			defer func() {
				if r := recover(); r != nil {
					app.Logger().Error("Forwarded request panic", slog.Any("error", r))
				}
			}()

			res, err := executeForwardedRequest(getMux, rawReq)
			if err != nil {
				app.Logger().Warn("Failed to execute a forwarded request", slog.String("error", err.Error()))
				res = &forwardedResponse{Status: http.StatusInternalServerError}
			}

			rawRes, err := json.Marshal(res)
			if err != nil {
				app.Logger().Warn("Failed to serialize a forwarded request response", slog.String("error", err.Error()))
				return
			}

			if err := app.Cluster().KV().Set(forwardKVPrefix+"res_"+reqId, string(rawRes), forwardTTL); err != nil {
				app.Logger().Warn("Failed to store a forwarded request response", slog.String("error", err.Error()))
				return
			}

			if err := app.Cluster().Notify(clusterTopicForwardResponse, replyNodeId+"|"+reqId); err != nil {
				app.Logger().Warn("Failed to publish a forwarded request response", slog.String("error", err.Error()))
			}
		}()
	})

	app.Cluster().Subscribe(clusterTopicForwardResponse, func(msg cluster.Message) {
		nodeId, reqId, ok := strings.Cut(msg.Payload, "|")
		if ok && nodeId == app.Cluster().NodeId() {
			forwardWaiters.notify(reqId)
		}
	})
}

func executeForwardedRequest(getMux func() (http.Handler, error), rawReq string) (*forwardedResponse, error) {
	req := forwardedRequest{}
	if err := json.Unmarshal([]byte(rawReq), &req); err != nil {
		return nil, err
	}

	mux, err := getMux()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()

	ctx = context.WithValue(ctx, forwardedContextKey{}, true)

	r, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	r.Header = req.Headers
	r.RemoteAddr = req.RemoteAddr

	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, r)

	return &forwardedResponse{
		Status:  rec.Code,
		Headers: rec.Header(),
		Body:    rec.Body.Bytes(),
	}, nil
}
