package cliruntime

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

const runtimeSpec = `{
 "openapi":"3.0.3","info":{"title":"Runtime tests","version":"1"},
 "security":[{"bearerAuth":[]}],
 "paths":{
  "/items/{id}":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"integer","format":"int64","minimum":1}}],
   "get":{"operationId":"read","tags":["Items"],"parameters":[{"name":"kind","in":"query","schema":{"type":"string","enum":["cat","dog"]}}],"responses":{"200":{"description":"ok"}}}},
  "/items":{"post":{"operationId":"create","tags":["Items"],"requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string","minLength":1}}}}}},"responses":{"201":{"description":"created"}}}},
  "/public":{"get":{"operationId":"public","security":[],"responses":{"200":{"description":"ok"}}}}
 },"components":{"securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer"}}}
}`

func document(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(runtimeSpec))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func runCommand(t *testing.T, spec *openapi3.T, server, body string, args ...string) (string, string, error) {
	t.Helper()
	build := func(path string) Builder {
		return func(server string, input Input) (*http.Request, error) {
			method := "GET"
			if input.Body != nil {
				method = "POST"
			}
			return http.NewRequest(method, strings.TrimRight(server, "/")+path, bytes.NewReader(input.Body))
		}
	}
	root, err := New("demo", spec, map[string]Builder{"read": build("/items/1"), "create": build("/items"), "public": build("/public")})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(body))
	if server != "" {
		args = append([]string{"--server", server}, args...)
	}
	root.SetArgs(args)
	err = root.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

func TestHelpAndValidationNeverSendRequests(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "test-bearer")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }))
	defer server.Close()
	for _, tc := range []struct {
		name, body string
		args       []string
		success    bool
	}{
		{"help", "", []string{"--help"}, true},
		{"output format", "", []string{"public", "--output", "invalid"}, false},
		{"missing path", "", []string{"items", "read"}, false},
		{"invalid integer", "", []string{"items", "read", "--id", "private-marker"}, false},
		{"integer constraint", "", []string{"items", "read", "--id", "0"}, false},
		{"enum", "", []string{"items", "read", "--id", "1", "--kind", "private-marker"}, false},
		{"missing body", "", []string{"items", "create"}, false},
		{"body type", `{"name":42}`, []string{"items", "create", "--body-stdin"}, false},
		{"body required", `{}`, []string{"items", "create", "--body-stdin"}, false},
		{"body unknown", `{"name":"ok","unexpected":"private-marker"}`, []string{"items", "create", "--body-stdin"}, false},
		{"body JSON", `private-marker`, []string{"items", "create", "--body-stdin"}, false},
		{"body trailing", `{"name":"ok"} {}`, []string{"items", "create", "--body-stdin"}, false},
		{"body limit", `{"name":"private-marker"}`, []string{"items", "create", "--body-stdin", "--max-input-bytes", "3"}, false},
		{"body on GET", `{}`, []string{"items", "read", "--id", "1", "--body-stdin"}, false},
		{"content type", `{}`, []string{"items", "create", "--body-stdin", "--content-type", "text/plain"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := runCommand(t, document(t), server.URL, tc.body, tc.args...)
			if (err == nil) != tc.success {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && (ExitCode(err) != 2 || strings.Contains(err.Error(), "private-marker")) {
				t.Fatalf("unsafe validation error: %v", err)
			}
			if strings.Contains(stderr, "private-marker") {
				t.Fatal("input leaked to stderr")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("validation/help sent a request")
	}
}

func TestBearerPublicTLSAndBody(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "private-bearer")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public" {
			if r.Header.Get("Authorization") != "" {
				t.Error("credentials sent to anonymous operation")
			}
		} else if r.Header.Get("Authorization") != "Bearer private-bearer" {
			t.Error("bearer missing")
		}
		if r.Method == "POST" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "accepted" {
				t.Error("body changed")
			}
			w.WriteHeader(201)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCommand(t, document(t), server.URL, "", "items", "read", "--id", "1"); err == nil || ExitCode(err) != 3 {
		t.Fatalf("untrusted certificate accepted: %v", err)
	}
	for _, args := range [][]string{{"items", "read", "--id", "1"}, {"public"}, {"items", "create", "--body-stdin"}} {
		out, _, err := runCommand(t, document(t), server.URL, `{"name":"accepted"}`, append(args, "--ca-file", ca, "--output", "json")...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("TLS request: %v: %s", err, out)
		}
	}
}

func TestResponseErrorsRedirectsAndLimits(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "private-bearer")
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalls.Add(1); fmt.Fprint(w, `{}`) }))
	defer target.Close()
	for _, tc := range []struct {
		name, body   string
		status, code int
		args         []string
		want         string
	}{
		{"HTTP failure", `private-bearer echoed-error`, 403, 4, nil, ""},
		{"redirect", `private-bearer`, 302, 4, nil, ""},
		{"not modified", "", 304, 0, nil, ""},
		{"no content", "", 204, 0, nil, ""},
		{"JSON limit", `{"large":"private-bearer"}`, 200, 3, []string{"--max-output-bytes", "3"}, ""},
		{"not JSON", "raw-bytes", 200, 3, nil, ""},
		{"raw", "raw-bytes", 200, 0, []string{"--output", "raw"}, "raw-bytes"},
		{"raw limit", "abcdef", 200, 3, []string{"--output", "raw", "--max-output-bytes", "3"}, "abc"},
		{"ETag", `{}`, 200, 0, []string{"--response-header", "ETag"}, "\"etag\"\n"},
		{"ETag CSV", `{}`, 200, 0, []string{"--response-header", "ETag", "--output", "csv"}, "\"etag\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"etag"`)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			out, stderr, err := runCommand(t, document(t), server.URL, "", append([]string{"items", "read", "--id", "1"}, tc.args...)...)
			code := 0
			if err != nil {
				code = ExitCode(err)
			}
			if code != tc.code || out != tc.want {
				t.Fatalf("got code %d output %q error %v", code, out, err)
			}
			if strings.Contains(stderr, "private-bearer") || (err != nil && strings.Contains(err.Error(), "private-bearer")) {
				t.Fatal("credential leaked")
			}
		})
	}
	if targetCalls.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestAuthenticationAlternativesAndAND(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "bearer")
	t.Setenv("SERVICE_KEY", "api-key")
	t.Setenv("SERVICE_BASIC", "user:password")
	spec := document(t)
	spec.Components.SecuritySchemes["apiKey"] = &openapi3.SecuritySchemeRef{Value: &openapi3.SecurityScheme{Type: "apiKey", In: "header", Name: "X-API-Key"}}
	spec.Components.SecuritySchemes["basic"] = &openapi3.SecuritySchemeRef{Value: &openapi3.SecurityScheme{Type: "http", Scheme: "basic"}}
	spec.Security = openapi3.SecurityRequirements{{"apiKey": {}, "bearerAuth": {}}}
	req, _ := http.NewRequest("GET", "https://example.invalid", nil)
	if err := authenticate(req, spec, spec.Paths.Value("/items/{id}").Get, "DEMO", []string{"apiKey=SERVICE_KEY"}, false); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer bearer" || req.Header.Get("X-API-Key") != "api-key" {
		t.Fatal("AND requirements not applied")
	}
	spec.Security = openapi3.SecurityRequirements{{"basic": {}}}
	req.Header = http.Header{}
	if err := authenticate(req, spec, spec.Paths.Value("/items/{id}").Get, "DEMO", []string{"basic=SERVICE_BASIC"}, false); err != nil {
		t.Fatal(err)
	}
	user, password, ok := req.BasicAuth()
	if !ok || user != "user" || password != "password" {
		t.Fatal("basic authentication mismatch")
	}
	spec.Security = openapi3.SecurityRequirements{{"apiKey": {}}, {"bearerAuth": {}}}
	req.Header = http.Header{}
	if err := authenticate(req, spec, spec.Paths.Value("/items/{id}").Get, "DEMO", nil, false); err != nil || req.Header.Get("Authorization") != "Bearer bearer" {
		t.Fatal("OR fallback failed")
	}
	if req.Header.Get("X-API-Key") != "" {
		t.Fatal("partial OR credential leaked")
	}
}

func TestTimeoutAndHTTPPolicy(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "private-bearer")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	started := time.Now()
	_, _, err := runCommand(t, document(t), server.URL, "", "items", "read", "--id", "1", "--timeout", "40ms")
	if err == nil || ExitCode(err) != 3 || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout failed: %v", err)
	}
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?token=secret", "file:///tmp/foo"} {
		if _, err := serverURL(raw, false); err == nil {
			t.Fatalf("unsafe server accepted: %s", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "https://example.com/prefix"} {
		if _, err := serverURL(raw, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := serverURL("http://example.com", true); err != nil {
		t.Fatal(err)
	}
}

func TestIntegerPrecision(t *testing.T) {
	spec := document(t)
	p := spec.Paths.Value("/items/{id}").Parameters[0].Value
	raw, err := parameterJSON(p, &parameterValue{kind: "integer", values: []string{"9007199254740993"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Values: map[string]json.RawMessage{"path:id": raw}}
	var id int64
	if err := input.Decode("path", "id", &id); err != nil || id != 9007199254740993 {
		t.Fatalf("integer precision lost: %d %v", id, err)
	}
}
