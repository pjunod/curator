package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type badBody struct{ readErr, closeErr error }

func (b badBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b badBody) Close() error             { return b.closeErr }

func TestLoopbackOnlyClientPreservesStatusBodyAndRejectsRedirect(t *testing.T) {
	for _, code := range []int{200, 202, 204, 302, 409, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			calls := 0
			transport := transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "127.0.0.1:7676" || r.URL.Path != "/api/v1/queue" || r.Method != "POST" {
					t.Fatalf("wrong namespace: %s", r.URL)
				}
				raw, _ := io.ReadAll(r.Body)
				if string(raw) != "{}" {
					t.Fatal(string(raw))
				}
				payload := `{"state":"pending"}`
				if code == 204 {
					payload = ""
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Location": []string{"http://remote.invalid"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			var out bytes.Buffer
			if err := run([]string{"POST", "/api/v1/queue"}, strings.NewReader("{}"), &out, transport); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Status  int
				Seconds float64
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Status != code || calls != 1 {
				t.Fatalf("%s calls=%d %v", out.String(), calls, err)
			}
		})
	}
}
func TestLoopbackClientReturnsIOAndProtocolFailures(t *testing.T) {
	for _, args := range [][]string{nil, {"GET", "https://wrong.invalid"}, {"bad method", "/api/v1/status"}} {
		if err := run(args, strings.NewReader("{}"), io.Discard, nil); err == nil {
			t.Fatal("bad arguments accepted")
		}
	}
	cause := errors.New("IO failure")
	for _, kind := range []string{"transport", "read", "close", "json"} {
		t.Run(kind, func(t *testing.T) {
			transport := transportFunc(func(*http.Request) (*http.Response, error) {
				if kind == "transport" {
					return nil, cause
				}
				body := io.ReadCloser(io.NopCloser(strings.NewReader("not json")))
				if kind == "read" {
					body = badBody{readErr: cause}
				}
				if kind == "close" {
					body = badBody{readErr: io.EOF, closeErr: cause}
				}
				return &http.Response{StatusCode: 200, Body: body}, nil
			})
			if err := run([]string{"GET", "/api/v1/status"}, strings.NewReader("{}"), io.Discard, transport); err == nil {
				t.Fatal("failure accepted")
			}
		})
	}
	if err := run([]string{"POST", "/api/v1/status"}, badBody{readErr: cause}, io.Discard, nil); !errors.Is(err, cause) {
		t.Fatal(err)
	}
}
