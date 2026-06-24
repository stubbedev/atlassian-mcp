package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Streamable HTTP transport (MCP spec): one /mcp endpoint. Clients POST
// JSON-RPC requests and optionally open a GET SSE stream to receive
// server→client requests (roots/list, elicitation). Server→client responses
// are POSTed back and correlated by id.

type backResp struct {
	result json.RawMessage
	rerr   *rpcError
}

type httpSession struct {
	id string
	*sessionState

	mu      sync.Mutex
	sse     chan []byte
	pending map[string]chan backResp
	hasSSE  bool
}

func newHTTPSession(id string) *httpSession {
	hs := &httpSession{id: id, sse: make(chan []byte, 16), pending: map[string]chan backResp{}}
	hs.sessionState = &sessionState{stdio: false, send: hs.send}
	return hs
}

func (hs *httpSession) send(method string, params any) (json.RawMessage, error) {
	hs.mu.Lock()
	if !hs.hasSSE {
		hs.mu.Unlock()
		return nil, &rpcError{Code: codeInternalError, Message: "client has no open event stream for server→client requests"}
	}
	id := nextRequestID()
	ch := make(chan backResp, 1)
	hs.pending[id] = ch
	hs.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	frame, _ := json.Marshal(req)

	select {
	case hs.sse <- frame:
	case <-time.After(5 * time.Second):
		hs.dropPending(id)
		return nil, &rpcError{Code: codeInternalError, Message: "event stream send timeout"}
	}
	select {
	case r := <-ch:
		if r.rerr != nil {
			return nil, r.rerr
		}
		return r.result, nil
	case <-time.After(120 * time.Second):
		hs.dropPending(id)
		return nil, &rpcError{Code: codeInternalError, Message: "timeout waiting for client response to " + method}
	}
}

func (hs *httpSession) dropPending(id string) {
	hs.mu.Lock()
	delete(hs.pending, id)
	hs.mu.Unlock()
}

func (hs *httpSession) routeResponse(id string, result json.RawMessage, rerr *rpcError) {
	hs.mu.Lock()
	ch := hs.pending[id]
	delete(hs.pending, id)
	hs.mu.Unlock()
	if ch != nil {
		ch <- backResp{result, rerr}
	}
}

var (
	httpSessions   = map[string]*httpSession{}
	httpSessionsMu sync.Mutex
)

func getSession(id string) *httpSession {
	httpSessionsMu.Lock()
	defer httpSessionsMu.Unlock()
	return httpSessions[id]
}

func putSession(hs *httpSession) {
	httpSessionsMu.Lock()
	httpSessions[hs.id] = hs
	httpSessionsMu.Unlock()
}

func dropSession(id string) {
	httpSessionsMu.Lock()
	delete(httpSessions, id)
	httpSessionsMu.Unlock()
}

const sessionHeader = "Mcp-Session-Id"

func runHTTP(addr, instructions string) {
	loopback := isLoopbackAddr(addr)
	token := os.Getenv("ATLASSIAN_MCP_HTTP_TOKEN")
	if !loopback && token == "" {
		logf("Refusing to bind a non-loopback address (%s) without ATLASSIAN_MCP_HTTP_TOKEN set.", addr)
		os.Exit(1)
	}

	authOK := func(r *http.Request) bool {
		if token == "" {
			return true // loopback, no token configured
		}
		h := r.Header.Get("Authorization")
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) == token
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			handleHTTPPost(w, r, instructions)
		case http.MethodGet:
			handleHTTPGet(w, r)
		case http.MethodDelete:
			if id := r.Header.Get(sessionHeader); id != "" {
				dropSession(id)
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	logf("Listening on http://%s/mcp (loopback=%v, auth=%v)", addr, loopback, token != "")
	srv := &http.Server{Addr: addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		logf("http server error: %v", err)
		os.Exit(1)
	}
}

func handleHTTPPost(w http.ResponseWriter, r *http.Request, instructions string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// A server→client response posted back by the client (has id, result/error,
	// no method) is routed to the waiting back-channel rather than dispatched.
	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(trimmed, &probe) == nil && probe.Method == "" && len(probe.ID) > 0 &&
		(len(probe.Result) > 0 || probe.Error != nil) {
		if hs := getSession(r.Header.Get(sessionHeader)); hs != nil {
			hs.routeResponse(trimQuotes(string(probe.ID)), probe.Result, probe.Error)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		http.Error(w, "parse error", http.StatusBadRequest)
		return
	}

	// Resolve or create the session. initialize mints a new session id.
	sessID := r.Header.Get(sessionHeader)
	hs := getSession(sessID)
	if req.Method == "initialize" {
		hs = newHTTPSession(nextRequestID() + "-sess")
		putSession(hs)
		w.Header().Set(sessionHeader, hs.id)
	} else if hs == nil {
		// Stateless fallback: a one-off session with no back-channel.
		hs = newHTTPSession("")
	}

	isNotification := len(req.ID) == 0
	result, rerr := dispatch(hs.sessionState, &req, instructions)
	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := rpcResponse{Jsonrpc: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	w.Header().Set("Content-Type", "application/json")
	out, _ := json.Marshal(resp)
	w.Write(out)
}

func handleHTTPGet(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionHeader)
	hs := getSession(id)
	if hs == nil {
		http.Error(w, "unknown session", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	hs.mu.Lock()
	hs.hasSSE = true
	hs.mu.Unlock()
	defer func() {
		hs.mu.Lock()
		hs.hasSSE = false
		hs.mu.Unlock()
	}()

	ctx := r.Context()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-hs.sse:
			w.Write([]byte("data: "))
			w.Write(frame)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-keepalive.C:
			w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
