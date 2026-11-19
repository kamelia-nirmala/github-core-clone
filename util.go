// Helpers shared by the HTTP handlers: JSON replies, base64, request ids,
// bearer auth, and URL parsing. Ported from util.ts.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// marshalJSON renders v as JSON exactly like Node's JSON.stringify for the
// values this forge produces (no HTML escaping), without a trailing newline.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder adds a trailing newline; strip it (sendJson adds its own).
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// marshalJSONLine is marshalJSON plus the "\n" Node's sendJson appends.
func marshalJSONLine(v any) []byte {
	b := marshalJSON(v)
	return append(b, '\n')
}

// jsonQuote renders s as a JSON string literal (no HTML escaping).
func jsonQuote(s string) string {
	return string(marshalJSON(s))
}

// jsonUnmarshal parses JSON into v (encoding/json).
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// shopToken is the bearer token the suite expects (MAKEFUN_SHOP_TOKEN env).
func shopToken() string {
	return os.Getenv("MAKEFUN_SHOP_TOKEN")
}
func b64encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// Base64 in, bytes out. Lenient like Node's Buffer.from(text, 'base64'):
// whitespace is stripped, characters outside the alphabet are ignored, and
// missing padding is tolerated. Never errors.
func b64decode(text string) []byte {
	var clean strings.Builder
	for _, r := range text {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' {
			clean.WriteRune(r)
		}
	}
	t := clean.String()
	for len(t)%4 != 0 {
		t += "="
	}
	b, err := base64.StdEncoding.DecodeString(t)
	if err != nil {
		return []byte{}
	}
	return b
}

// Request ids: a per-process counter, rendered as zero-padded hex.
type RequestIDGen struct {
	seq uint64
}

func (g *RequestIDGen) Next() string {
	g.seq++
	// Fast path: "req-" + 8 hex digits in one stack array, one allocation
	// for the returned string. Falls back to FormatUint for huge counters.
	if g.seq <= 0xffffffff {
		var b [12]byte
		copy(b[:4], "req-")
		n := g.seq
		for i := 11; i >= 4; i-- {
			b[i] = "0123456789abcdef"[n&0xf]
			n >>= 4
		}
		return string(b[:])
	}
	return "req-" + padHex(g.seq)
}

func padHex(n uint64) string {
	s := strconv.FormatUint(n, 16)
	for len(s) < 8 {
		s = "0" + s
	}
	return s
}

// Pull the token out of an `Authorization: Bearer <token>` header.
func bearerToken(authHeader string) string {
	h := strings.TrimSpace(authHeader)
	if !strings.HasPrefix(h, "Bearer") {
		return ""
	}
	rest := strings.TrimSpace(h[len("Bearer"):])
	if rest == "" {
		return ""
	}
	// "Bearer\s+(.+)" — require at least one space/tab after Bearer.
	if len(h) <= len("Bearer") || (h[len("Bearer")] != ' ' && h[len("Bearer")] != '\t') {
		return ""
	}
	return rest
}

// True for a plain JSON object (not null, not an array).
func isPlainObject(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// Parsed URL: path, decoded segments, query values.
type ParsedURL struct {
	Path     string
	Segments []string
	Query    map[string][]string
	// Ordered (key, value) pairs as received — needed to rebuild query strings
	// (e.g. pagination Link) in the same order URLSearchParams would.
	Pairs [][2]string
}

// Split a request URL into its path, its decoded path segments, and its query
// string. Mirrors util.ts parseUrl (manual split, no net/url validation).
func parseURL(raw string) ParsedURL {
	rest := raw
	if i := strings.Index(rest, "://"); i > 0 {
		if slash := strings.Index(rest[i+3:], "/"); slash >= 0 {
			rest = rest[i+3+slash:]
		} else {
			rest = "/"
		}
	}
	path := rest
	queryStr := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		path = rest[:q]
		queryStr = rest[q+1:]
	}
	// No query string: leave Query nil (reads from a nil map are safe in Go)
	// instead of allocating an empty map per request.
	var query map[string][]string
	var pairs [][2]string
	if queryStr != "" {
		query = map[string][]string{}
		for _, pair := range strings.Split(queryStr, "&") {
			if pair == "" {
				continue
			}
			k, v := pair, ""
			if eq := strings.Index(pair, "="); eq >= 0 {
				k, v = pair[:eq], pair[eq+1:]
			}
			k = queryUnescape(k)
			v = queryUnescape(v)
			query[k] = append(query[k], v)
			pairs = append(pairs, [2]string{k, v})
		}
	}
	segments := []string{}
	for _, piece := range strings.Split(path, "/") {
		if piece == "" {
			continue
		}
		if strings.Contains(piece, "%") {
			if dec, err := urlPathUnescape(piece); err == nil {
				piece = dec
			}
		}
		segments = append(segments, piece)
	}
	return ParsedURL{Path: path, Segments: segments, Query: query, Pairs: pairs}
}

func (u ParsedURL) Get(key string) string {
	if vs, ok := u.Query[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// SetPage returns the query string with page=<n>, mirroring
// URLSearchParams.set('page', ...).toString(): replace in place if present
// (dropping duplicates), else append at the end.
func (u ParsedURL) SetPage(n int) string {
	val := strconv.Itoa(n)
	seen := false
	var out [][2]string
	for _, p := range u.Pairs {
		if p[0] == "page" {
			if !seen {
				out = append(out, [2]string{"page", val})
				seen = true
			}
			continue
		}
		out = append(out, p)
	}
	if !seen {
		out = append(out, [2]string{"page", val})
	}
	var sb strings.Builder
	for i, p := range out {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(formEncode(p[0]))
		sb.WriteByte('=')
		sb.WriteString(formEncode(p[1]))
	}
	return sb.String()
}

// formEncode mirrors URLSearchParams.toString() percent-encoding
// (alphanumerics + *-._ kept, space -> '+', rest %XX uppercase).
func formEncode(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '*':
			sb.WriteByte(c)
		case c == ' ':
			sb.WriteByte('+')
		default:
			const hexd = "0123456789ABCDEF"
			sb.WriteByte('%')
			sb.WriteByte(hexd[c>>4])
			sb.WriteByte(hexd[c&0xf])
		}
	}
	return sb.String()
}

func queryUnescape(s string) string {
	// application/x-www-form-urlencoded: '+' means space.
	s = strings.ReplaceAll(s, "+", " ")
	if dec, err := urlPathUnescape(s); err == nil {
		return dec
	}
	return s
}

func urlPathUnescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi := hexVal(s[i+1])
			lo := hexVal(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
			return "", errBadEscape
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

type escapeError struct{}

func (escapeError) Error() string { return "bad escape" }

var errBadEscape = escapeError{}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
