package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

func BenchmarkReadPath(b *testing.B) {
	os.Setenv("MAKEFUN_SHOP_TOKEN", "test-token-1234")
	f := NewForge()
	repo := f.CreateRepo("bench", "", false, "main")
	_ = f.CreateIssue(repo, "hello", "body", nil)
	reqText := "GET /api/v3/repos/adaiversen/bench/issues/100001 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer test-token-1234\r\nAccept: application/json\r\n\r\n"
	gen := &RequestIDGen{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		method, path, query, headers, body, err := readRequest(bufio.NewReader(strings.NewReader(reqText)))
		if err != nil {
			b.Fatal(err)
		}
		_ = dispatch(f, gen, method, path, query, headers, body)
	}
}

func BenchmarkWritePath(b *testing.B) {
	os.Setenv("MAKEFUN_SHOP_TOKEN", "test-token-1234")
	f := NewForge()
	_ = f.CreateRepo("bench", "", false, "main")
	body := `{"title":"t","body":"b"}`
	reqText := "POST /api/v3/repos/adaiversen/bench/issues HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer test-token-1234\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	gen := &RequestIDGen{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		method, path, query, headers, body, err := readRequest(bufio.NewReader(strings.NewReader(reqText)))
		if err != nil {
			b.Fatal(err)
		}
		_ = dispatch(f, gen, method, path, query, headers, body)
	}
}
