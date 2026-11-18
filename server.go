// Raw TCP HTTP/1.1 server: request parsing, keep-alive, routing, auth.
// No net/http: the binary stays small and the hot paths stay allocation-light.
package main

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

type request struct {
	method  string
	path    string
	query   map[string][]string
	headers map[string]string // lower-cased keys
	body    []byte
}

type response struct {
	status      int
	contentType string
	headers     map[string]string
	body        []byte
}

func jsonResp(status int, body []byte) response {
	return response{status: status, contentType: "application/json", body: body}
}

func errJSON(status int, message string) response {
	return jsonResp(status, []byte(`{"message":`+jsonString(message)+`}`))
}

// jsonString encodes a string as a JSON string literal (no HTML escaping).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[c>>4])
				b.WriteByte("0123456789abcdef"[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

type server struct {
	st    *store
	token string
	reqID uint64
}

func (s *server) nextReqID() string {
	return strconv.FormatUint(atomic.AddUint64(&s.reqID, 1), 10)
}

func (s *server) authed(req *request) bool {
	if s.token == "" {
		return true
	}
	return req.headers["authorization"] == "Bearer "+s.token
}

var statusText = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func (s *server) dispatch(req *request) response {
	if req.path == "/healthz" {
		if req.method != "GET" {
			return errJSON(405, "method not allowed")
		}
		return jsonResp(200, []byte(`{"ok":true}`))
	}
	if strings.HasPrefix(req.path, "/api/v3/") || req.path == "/api/v3" {
		if !s.authed(req) {
			return errJSON(401, "Bad credentials")
		}
		return s.dispatchREST(req)
	}
	if req.path == "/api/graphql" {
		if req.method != "POST" {
			return errJSON(405, "method not allowed")
		}
		if !s.authed(req) {
			return errJSON(401, "Bad credentials")
		}
		return s.dispatchGraphQL(req)
	}
	if req.path == "/mcp" {
		if req.method != "POST" {
			return errJSON(405, "method not allowed")
		}
		if !s.authed(req) {
			return errJSON(401, "Bad credentials")
		}
		return s.dispatchMCP(req)
	}
	if owner, repoName, suffix, ok := splitGitPath(req.path); ok {
		return s.dispatchGit(req, owner, repoName, suffix)
	}
	return errJSON(404, "Not Found")
}

// splitGitPath matches /{owner}/{repo}.git/{suffix}.
func splitGitPath(path string) (owner, repoName, suffix string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", "", false
	}
	rest := path[1:]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", "", false
	}
	owner = rest[:i]
	rest = rest[i+1:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return "", "", "", false
	}
	repoSeg := rest[:j]
	suffix = rest[j+1:]
	if !strings.HasSuffix(repoSeg, ".git") {
		return "", "", "", false
	}
	repoName = strings.TrimSuffix(repoSeg, ".git")
	if owner == "" || repoName == "" {
		return "", "", "", false
	}
	return owner, repoName, suffix, true
}

// serveConn handles one TCP connection, sequentially, with keep-alive.
func (s *server) serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	for {
		req, keepAlive, err := readRequest(br)
		if err != nil {
			return
		}
		resp := s.dispatch(req)
		if !writeResponse(conn, resp, s.nextReqID(), keepAlive) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

func readRequest(br *bufio.Reader) (*request, bool, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, false, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, simpleError("bad request line")
	}
	req := &request{
		method:  parts[0],
		headers: map[string]string{},
		query:   map[string][]string{},
	}
	rawTarget := parts[1]
	if idx := strings.IndexByte(rawTarget, '?'); idx >= 0 {
		req.path = rawTarget[:idx]
		parseQuery(rawTarget[idx+1:], req.query)
	} else {
		req.path = rawTarget
	}
	// Headers.
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, false, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		if idx := strings.IndexByte(hline, ':'); idx >= 0 {
			key := strings.ToLower(strings.TrimSpace(hline[:idx]))
			val := strings.TrimSpace(hline[idx+1:])
			req.headers[key] = val
		}
	}
	keepAlive := true
	if strings.EqualFold(req.headers["connection"], "close") {
		keepAlive = false
	} else if strings.HasPrefix(parts[2], "HTTP/1.0") && !strings.EqualFold(req.headers["connection"], "keep-alive") {
		keepAlive = false
	}
	// Body.
	if te := req.headers["transfer-encoding"]; strings.Contains(strings.ToLower(te), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, false, err
		}
		req.body = body
	} else if cl := req.headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, false, simpleError("bad content-length")
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := readFull(br, body); err != nil {
				return nil, false, err
			}
			req.body = body
		}
	}
	return req, keepAlive, nil
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if idx := strings.IndexByte(line, ';'); idx >= 0 {
			line = line[:idx]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || n < 0 {
			return nil, simpleError("bad chunk size")
		}
		if n == 0 {
			// Trailer headers until an empty line.
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(t, "\r\n") == "" {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := readFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// Trailing CRLF after the chunk.
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

func parseQuery(raw string, out map[string][]string) {
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		k = queryUnescape(k)
		v = queryUnescape(v)
		out[k] = append(out[k], v)
	}
}

func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			b.WriteByte(' ')
		} else if c == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
			} else {
				b.WriteByte(c)
			}
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

func writeResponse(conn net.Conn, resp response, reqID string, keepAlive bool) bool {
	text, ok := statusText[resp.status]
	if !ok {
		text = "Status"
	}
	var b strings.Builder
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(resp.status))
	b.WriteString(" ")
	b.WriteString(text)
	b.WriteString("\r\n")
	ct := resp.contentType
	if ct == "" {
		ct = "application/json"
	}
	b.WriteString("Content-Type: " + ct + "\r\n")
	b.WriteString("Content-Length: " + strconv.Itoa(len(resp.body)) + "\r\n")
	b.WriteString("X-GitHub-Request-Id: " + reqID + "\r\n")
	if keepAlive {
		b.WriteString("Connection: keep-alive\r\n")
	} else {
		b.WriteString("Connection: close\r\n")
	}
	for k, v := range resp.headers {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("\r\n")
	head := []byte(b.String())
	if _, err := conn.Write(head); err != nil {
		return false
	}
	if len(resp.body) > 0 {
		if _, err := conn.Write(resp.body); err != nil {
			return false
		}
	}
	return true
}

func listenAndServe(addr string, s *server) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	serveListener(ln, s)
	return nil
}

func serveListener(ln net.Listener, s *server) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go s.serveConn(conn)
	}
}
