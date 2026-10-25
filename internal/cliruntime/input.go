// Package cliruntime implements the shared behavior of generated Cobra clients.
package cliruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Input contains validated values. HTTP serialization remains in oapi-codegen.
type Input struct {
	Values      map[string]json.RawMessage
	Body        []byte
	ContentType string
}

// Decode populates a generated, typed SDK parameter without losing integer precision.
func (v Input) Decode(location, name string, target any) error {
	raw, exists := v.Values[location+":"+name]
	if !exists {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid %s parameter %s", location, name)
	}
	return nil
}

type parameterValue struct {
	values []string
	kind   string
}

func (v *parameterValue) Set(s string) error { v.values = append(v.values, s); return nil }
func (v *parameterValue) String() string     { return "" }
func (v *parameterValue) Type() string       { return v.kind }

func schemaKind(s *openapi3.Schema) string {
	if s == nil {
		return "json"
	}
	if s.Type != nil {
		for _, kind := range s.Type.Slice() {
			if kind != "null" {
				return kind
			}
		}
	}
	for _, ref := range s.AllOf {
		if kind := schemaKind(ref.Value); kind != "json" {
			return kind
		}
	}
	return "json"
}

func scalar(kind, text string) (any, error) {
	switch kind {
	case "string":
		return text, nil
	case "integer":
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return nil, errors.New("expected a 64-bit integer")
		}
		return json.Number(text), nil
	case "number":
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return nil, errors.New("expected a number")
		}
		return json.Number(text), nil
	case "boolean":
		return strconv.ParseBool(text)
	default:
		var value any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("expected JSON")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, errors.New("expected one JSON value")
		}
		return value, nil
	}
}

func parameterJSON(p *openapi3.Parameter, v *parameterValue, changed bool) (json.RawMessage, error) {
	if p.Schema == nil || p.Schema.Value == nil {
		return nil, errors.New("parameters using content are not supported")
	}
	schema := p.Schema.Value
	var value any
	if !changed {
		if schema.Default != nil {
			value = schema.Default
		} else if p.Required {
			return nil, errors.New("required")
		} else {
			return nil, nil
		}
	} else if v.kind == "array" {
		items := schema.Items
		if items == nil {
			return nil, errors.New("array items schema is missing")
		}
		values := make([]any, 0, len(v.values))
		for _, raw := range v.values {
			item, err := scalar(schemaKind(items.Value), raw)
			if err != nil {
				return nil, err
			}
			values = append(values, item)
		}
		value = values
	} else {
		if len(v.values) != 1 {
			return nil, errors.New("may only be supplied once")
		}
		var err error
		value, err = scalar(v.kind, v.values[0])
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("invalid JSON value")
	}
	if err := schema.VisitJSON(value); err != nil {
		return nil, errors.New("violates the OpenAPI parameter schema")
	}
	return raw, nil
}

func boundedRead(r io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maximum+1))
	if err != nil {
		return nil, errors.New("cannot read input")
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("input exceeds the configured byte limit")
	}
	return data, nil
}

func readBody(in io.Reader, op *openapi3.Operation, file string, stdin bool, contentType string, limit int64) ([]byte, string, error) {
	if file != "" && stdin {
		return nil, "", errors.New("use either --body-file or --body-stdin")
	}
	provided := file != "" || stdin
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		if provided || contentType != "" {
			return nil, "", errors.New("this operation does not accept a request body")
		}
		return nil, "", nil
	}
	body := op.RequestBody.Value
	if !provided {
		if body.Required {
			return nil, "", errors.New("provide --body-file or --body-stdin")
		}
		return nil, "", nil
	}
	if contentType == "" {
		if body.Content.Get("application/json") != nil {
			contentType = "application/json"
		} else if len(body.Content) == 1 {
			for name := range body.Content {
				contentType = name
			}
		} else {
			return nil, "", errors.New("select --content-type from the contract's request body media types")
		}
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", errors.New("invalid --content-type")
	}
	media := body.Content.Get(contentType)
	if media == nil {
		return nil, "", errors.New("--content-type is not declared by this operation")
	}
	reader := in
	if file != "" && file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return nil, "", errors.New("cannot open --body-file")
		}
		defer f.Close()
		reader = f
	}
	data, err := boundedRead(reader, limit)
	if err != nil {
		return nil, "", err
	}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, "", errors.New("request body must be valid JSON")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, "", errors.New("request body must contain exactly one JSON value")
		}
		if media.Schema != nil && media.Schema.Value != nil {
			if err := media.Schema.Value.VisitJSON(value, openapi3.VisitAsRequest()); err != nil {
				return nil, "", errors.New("request body violates the OpenAPI schema")
			}
		}
	}
	return data, contentType, nil
}
