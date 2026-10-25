package generator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/araihu/oapi-cli-gen/internal/cliruntime"
	"github.com/getkin/kin-openapi/openapi3"
)

func loadSpec(ctx context.Context, filename string) (*openapi3.T, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("open contract: %w", err)
	}
	base := filepath.Dir(real)
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = true
	remaining := int64(32 << 20)
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, uri *url.URL) ([]byte, error) {
		if uri.Scheme != "" && uri.Scheme != "file" {
			return nil, errors.New("remote OpenAPI references are disabled; vendor them beside the contract")
		}
		if uri.Host != "" {
			return nil, errors.New("reference authorities are not supported")
		}
		path := uri.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		relative, err := filepath.Rel(base, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("OpenAPI reference escapes the contract directory")
		}
		file, err := root.Open(relative)
		if err != nil {
			return nil, fmt.Errorf("read local reference: %w", err)
		}
		defer file.Close()
		if remaining <= 0 {
			return nil, errors.New("OpenAPI documents exceed 32 MiB")
		}
		data, err := io.ReadAll(io.LimitReader(file, remaining+1))
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return nil, errors.New("OpenAPI documents exceed 32 MiB")
		}
		return data, nil
	}
	doc, err := loader.LoadFromFile(real)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI: %w", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.0.") && !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		return nil, errors.New("OpenAPI 3.0 or 3.1 is required")
	}
	if doc.Paths == nil {
		return nil, errors.New("contract contains no paths")
	}
	ids := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "" {
				op.OperationID = cliruntime.Kebab(strings.ToLower(method) + "-" + path)
			}
			if ids[op.OperationID] {
				return nil, fmt.Errorf("duplicate operationId %s", op.OperationID)
			}
			ids[op.OperationID] = true
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("contract contains no operations")
	}
	if err := doc.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validate OpenAPI: %w", err)
	}
	doc.InternalizeRefs(ctx, nil)
	return doc, nil
}
