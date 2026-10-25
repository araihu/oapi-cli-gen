package cliruntime

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestResponseOutputFormats(t *testing.T) {
	for _, tc := range []struct {
		name, body, table string
		csv               [][]string
	}{
		{"object", `{"name":"demo","id":9007199254740993,"enabled":true,"labels":{"env":"prod"}}`,
			"enabled  id                labels          name\ntrue     9007199254740993  {\"env\":\"prod\"}  demo\n",
			[][]string{{"enabled", "id", "labels", "name"}, {"true", "9007199254740993", `{"env":"prod"}`, "demo"}}},
		{"heterogeneous list", `[{"name":"a","id":1},{"id":2,"extra":null}]`,
			"extra  id  name\n       1   a\nnull   2   \n",
			[][]string{{"extra", "id", "name"}, {"", "1", "a"}, {"null", "2", ""}}},
		{"page", `{"items":[{"name":"a","id":1}],"page":{"hasMore":true,"nextCursor":"cursor"}}`,
			"id  name\n1   a\n", [][]string{{"id", "name"}, {"1", "a"}}},
		{"empty page", `{"items":[],"page":{"hasMore":false}}`, "", nil},
		{"empty array", `[]`, "", nil},
		{"empty object", `{}`, "value\n{}\n", [][]string{{"value"}, {"{}"}}},
		{"null", `null`, "value\nnull\n", [][]string{{"value"}, {"null"}}},
		{"scalar list", `[1.234567890123456789,"x",false]`, "value\n1.234567890123456789\nx\nfalse\n",
			[][]string{{"value"}, {"1.234567890123456789"}, {"x"}, {"false"}}},
		{"escaping", `{"name":"a,\"b\"\nline\t\u001b[31m","nested":[1,"x"]}`,
			"name                   nested\na,\"b\"\\nline\\t\\x1b[31m  [1,\"x\"]\n",
			[][]string{{"name", "nested"}, {"a,\"b\"\nline\t\x1b[31m", `[1,"x"]`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			for _, format := range []string{"", "table", "json", "csv"} {
				args := []string{"public"}
				if format != "" {
					args = append(args, "--output", format)
				}
				out, stderr, err := runCommand(t, document(t), server.URL, "", args...)
				if err != nil || stderr != "" {
					t.Fatalf("format %q: %v stderr=%s", format, err, stderr)
				}
				switch format {
				case "", "table":
					if out != tc.table {
						t.Fatalf("table: got %q want %q", out, tc.table)
					}
				case "csv":
					records, err := csv.NewReader(strings.NewReader(out)).ReadAll()
					if err != nil || !reflect.DeepEqual(records, tc.csv) {
						t.Fatalf("CSV: got %#v want %#v error=%v", records, tc.csv, err)
					}
				case "json":
					var original, formatted any
					// Preserve number precision in the comparison too.
					originalDecoder := json.NewDecoder(strings.NewReader(tc.body))
					originalDecoder.UseNumber()
					formattedDecoder := json.NewDecoder(strings.NewReader(out))
					formattedDecoder.UseNumber()
					if originalDecoder.Decode(&original) != nil || formattedDecoder.Decode(&formatted) != nil || !reflect.DeepEqual(original, formatted) {
						t.Fatalf("JSON response changed: %s", out)
					}
				}
			}
		})
	}
}

func TestFormattedResponseFailuresPublishNoPartialData(t *testing.T) {
	for _, format := range []string{"table", "json", "csv"} {
		for _, body := range []string{`{"name":"private-marker"} {}`, `not-json-private-marker`, `[{"a":1},{"b":2},{"c":3},{"d":4},{"e":5},{"f":6},{"g":7},{"h":8},{"i":9},{"j":0}]`} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, body)
			}))
			args := []string{"public", "--output", format}
			if strings.HasPrefix(body, "[") {
				// The body fits, but table/CSV headers and JSON indentation exceed it.
				args = append(args, "--max-output-bytes", fmt.Sprint(len(body)))
			}
			out, stderr, err := runCommand(t, document(t), server.URL, "", args...)
			server.Close()
			if err == nil || ExitCode(err) != 3 || out != "" || stderr != "" || strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("format %s: err=%v out=%q stderr=%q", format, err, out, stderr)
			}
		}
	}
}
