// The forge HTTP server: one Forge store, dispatching to git smart HTTP,
// REST v3, GraphQL, and MCP — over a raw TCP listener with a minimal HTTP
// parser (no net/http, no IncomingMessage/ServerResponse objects).
// Ported from app.ts — same routes, same gates, same replies.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// appendResponseHeader appends the HTTP status line + headers (no body)
// for resp onto h and returns the extended slice. Shared by the bufio path
// (writeResponse) and the epoll path (which queues bytes in conn buffers).
func appendResponseHeader(h []byte, resp Response, reqID string) []byte {
	h = append(h, "HTTP/1.1 "...)
	h = strconv.AppendInt(h, int64(resp.Status), 10)
	h = append(h, ' ')
	h = append(h, httpStatusText(resp.Status)...)
	h = append(h, "\r\n"...)
	if resp.ContentType != "" {
		h = append(h, "Content-Type: "...)
		h = append(h, resp.ContentType...)
		h = append(h, "\r\n"...)
	}
	if resp.FixedReqID != "" {
		h = append(h, "x-github-request-id: "...)
		h = append(h, resp.FixedReqID...)
		h = append(h, "\r\n"...)
	} else if !resp.NoRequestID {
		h = append(h, "x-github-request-id: "...)
		h = append(h, reqID...)
		h = append(h, "\r\n"...)
	}
	for k, v := range resp.Extra {
		h = append(h, k...)
		h = append(h, ": "...)
		h = append(h, v...)
		h = append(h, "\r\n"...)
	}
	h = append(h, "Content-Length: "...)
	h = strconv.AppendInt(h, int64(len(resp.Body)), 10)
	h = append(h, "\r\n\r\n"...)
	return h
}

// writeResponse serializes one handler Response onto the connection.
// The header is built in a stack array (no strings.Builder allocation);
// bufio coalesces it with the body into a single write syscall.
func writeResponse(w *bufio.Writer, resp Response, reqID string) error {
	var hb [512]byte
	h := appendResponseHeader(hb[:0], resp, reqID)
	if _, err := w.Write(h); err != nil {
		return err
	}
	if len(resp.Body) > 0 {
		if _, err := w.Write(resp.Body); err != nil {
			return err
		}
	}
	return w.Flush()
}

func httpStatusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	}
	return "Status"
}

// requestIsAuthed mirrors app.ts: no token configured means everything is
// accepted; otherwise the Bearer token must match.
func requestIsAuthed(authHeader string) bool {
	expected := shopToken()
	if expected == "" {
		return true
	}
	return bearerToken(authHeader) == expected
}

// dispatch routes one parsed request to the right handler.
func dispatch(f *Forge, reqIDGen *RequestIDGen, method, path string, query ParsedURL, headers map[string]string, body []byte) Response {
	segments := query.Segments
	host := headers["host"]
	if host == "" {
		host = "localhost"
	}

	// The env's boot check calls this twice and wants 200 {"ok":true}. No auth.
	if method == "GET" && path == "/healthz" {
		return jsonResp(200, map[string]any{"ok": true}, nil)
	}

	// ---- Git smart HTTP: /{owner}/{repo}.git/* ----
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		expected := shopToken()
		token := bearerToken(headers["authorization"])
		if expected != "" && token != "" && token != expected {
			return errResp(401, "Requires authentication")
		}
		owner := segments[0]
		repoName := segments[1][:len(segments[1])-4]
		tail := segments[2:]
		gitProtocol := headers["git-protocol"]
		return handleGit(f, owner, repoName, tail, query, method, gitProtocol, headers["authorization"], body)
	}

	// ---- REST v3: /api/v3/* ----
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		restSegments := segments[2:]
		authed := requestIsAuthed(headers["authorization"])
		var parsedBody any
		bodyMalformed := false
		if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
			if len(body) > 0 {
				var v any
				if err := json.Unmarshal(body, &v); err != nil {
					bodyMalformed = true
				} else {
					parsedBody = v
				}
			}
		}
		ctx := &RestCtx{
			Method: method, Segments: restSegments, Query: query,
			Body: parsedBody, BodyMalformed: bodyMalformed, Authed: authed,
			Host: host, Forge: f, ReqIDGen: reqIDGen,
		}
		if handled, resp := issuePullRoutes(ctx); handled {
			return resp
		}
		if handled, resp := repoRoutes(ctx); handled {
			return resp
		}
		if handled, resp := contentRoutes(ctx); handled {
			return resp
		}
		if !authed {
			return errResp(401, "Requires authentication")
		}
		return errResp(404, "Not Found")
	}

	// ---- MCP: POST /mcp ----
	if path == "/mcp" {
		return handleMCP(f, body, requestIsAuthed(headers["authorization"]))
	}

	// ---- GraphQL: POST /api/graphql ----
	if path == "/api/graphql" {
		authed := requestIsAuthed(headers["authorization"])
		var parsedBody any
		bodyMalformed := false
		if method == "POST" || method == "PUT" || method == "PATCH" {
			if len(body) > 0 {
				var v any
				if err := json.Unmarshal(body, &v); err != nil {
					bodyMalformed = true
				} else {
					parsedBody = v
				}
			}
		}
		return handleGraphQL(f, parsedBody, bodyMalformed, authed)
	}

	// Anything else, any method: GitHub's plain 404 body.
	return jsonResp(404, map[string]any{"message": "Not Found"}, nil)
}

// readRequest parses one HTTP/1.1 request from the buffered reader.
// Returns method, path, query, headers (lower-cased keys), body.
func readRequest(r *bufio.Reader) (method, path string, query ParsedURL, headers map[string]string, body []byte, err error) {
	headers = map[string]string{}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", "", ParsedURL{}, headers, nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return "", "", ParsedURL{}, headers, nil, fmt.Errorf("bad request line")
	}
	method = parts[0]
	rawURL := parts[1]
	query = parseURL(rawURL)
	path = query.Path
	for {
		hline, err := r.ReadString('\n')
		if err != nil {
			return "", "", ParsedURL{}, headers, nil, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		colon := strings.Index(hline, ":")
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(hline[:colon]))
		value := strings.TrimSpace(hline[colon+1:])
		headers[key] = value
	}
	// Body: Content-Length only (the suite and git clients use it; no chunked
	// bodies in this protocol). A GET carries no body.
	contentLength := 0
	if cl, ok := headers["content-length"]; ok {
		contentLength, _ = strconv.Atoi(cl)
	}
	if contentLength > 0 {
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(r, body); err != nil {
			return "", "", ParsedURL{}, headers, nil, err
		}
	}
	return method, path, query, headers, body, nil
}

// tuneConn applies low-level socket options to an accepted connection:
// TCP_QUICKACK (answer immediately instead of waiting for the delayed-ACK
// timer) and larger socket buffers.
func tuneConn(conn net.Conn) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	raw, err := tcpConn.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 262144)
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 262144)
	})
}

// serveConn handles one keep-alive connection: read requests in a loop and
// answer each in turn. A single global mutex serializes request handling,
// matching the TS server's single-threaded semantics (and guaranteeing the
// logical clock stays deterministic for replay).
func serveConn(conn net.Conn, f *Forge, reqIDGen *RequestIDGen, mu *sync.Mutex) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 65536)
	writer := bufio.NewWriterSize(conn, 65536)
	for {
		method, path, query, headers, body, err := readRequest(reader)
		if err != nil {
			return
		}
		mu.Lock()
		var resp Response
		func() {
			defer func() {
				if r := recover(); r != nil {
					resp = errResp(500, "Internal Server Error")
				}
			}()
			resp = dispatch(f, reqIDGen, method, path, query, headers, body)
		}()
		reqID := reqIDGen.Next()
		mu.Unlock()
		if err := writeResponse(writer, resp, reqID); err != nil {
			return
		}
	}
}

// serve listens and serves until the process exits. It prints "PORT=<n>" on
// stdout once listening, which is how the launcher learns the port. When the
// PORT env var names a fixed port (local testing), it binds that instead.
func serve() {
	addr := "127.0.0.1:0"
	if fixed := os.Getenv("PORT"); fixed != "" {
		if n, err := strconv.Atoi(fixed); err == nil && n > 0 && n < 65536 {
			addr = "127.0.0.1:" + fixed
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("PORT=%d\n", port)
	os.Stdout.Sync()
	f := NewForge()
	reqIDGen := &RequestIDGen{}
	var mu sync.Mutex
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetNoDelay(true)
		}
		tuneConn(conn)
		go serveConn(conn, f, reqIDGen, &mu)
	}
}
