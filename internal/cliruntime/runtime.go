package cliruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/spf13/cobra"
)

// Builder calls an oapi-codegen request constructor using typed parameters.
type Builder func(server string, input Input) (*http.Request, error)

// ExitError distinguishes invalid input (2), transport (3) and HTTP failure (4).
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }
func ExitCode(err error) int {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	return 2
}

type settings struct {
	server, caFile, certFile, keyFile, format, bodyFile, contentType, responseHeader string
	allowHTTP, bodyStdin                                                             bool
	authEnv                                                                          []string
	timeout                                                                          time.Duration
	maxInput, maxOutput                                                              int64
}

// Kebab produces stable CLI names from operation IDs, tags and parameter names.
func Kebab(s string) string {
	runes := []rune(s)
	var out strings.Builder
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "-") {
				out.WriteByte('-')
			}
			continue
		}
		if unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(runes[i-1]))) {
			out.WriteByte('-')
		}
		out.WriteRune(unicode.ToLower(r))
	}
	return strings.Trim(out.String(), "-")
}

func extension(values map[string]any, key, fallback string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return fallback
}

func defaultServer(spec *openapi3.T) string {
	if len(spec.Servers) == 0 {
		return ""
	}
	server := spec.Servers[0]
	value := server.URL
	keys := make([]string, 0, len(server.Variables))
	for key := range server.Variables {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value = strings.ReplaceAll(value, "{"+key+"}", server.Variables[key].Default)
	}
	return value
}

// Parameters applies operation-level overrides to inherited path parameters.
func Parameters(item *openapi3.PathItem, op *openapi3.Operation) []*openapi3.Parameter {
	byKey := map[string]*openapi3.Parameter{}
	for _, refs := range []openapi3.Parameters{item.Parameters, op.Parameters} {
		for _, ref := range refs {
			if ref != nil && ref.Value != nil {
				byKey[ref.Value.In+":"+ref.Value.Name] = ref.Value
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]*openapi3.Parameter, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out
}

// New creates a Cobra tree without making network requests or reading credentials.
func New(name string, spec *openapi3.T, builders map[string]Builder) (*cobra.Command, error) {
	prefix := envName(name)
	options := &settings{server: defaultServer(spec), timeout: 60 * time.Second, format: "json", maxInput: 8 << 20, maxOutput: 64 << 20}
	if value, ok := os.LookupEnv(prefix + "_SERVER"); ok {
		options.server = value
	}
	root := &cobra.Command{Use: name, Short: spec.Info.Title, Version: spec.Info.Version, SilenceUsage: true, SilenceErrors: true}
	flags := root.PersistentFlags()
	flags.StringVar(&options.server, "server", options.server, "API base URL (or "+prefix+"_SERVER)")
	flags.StringArrayVar(&options.authEnv, "auth-env", nil, "Read credentials from securityScheme=ENV (repeatable; bearer defaults to "+prefix+"_TOKEN)")
	flags.StringVar(&options.caFile, "ca-file", "", "Additional PEM CA bundle")
	flags.StringVar(&options.certFile, "client-cert", "", "PEM client certificate for mutual TLS")
	flags.StringVar(&options.keyFile, "client-key", "", "PEM client private key for mutual TLS")
	flags.BoolVar(&options.allowHTTP, "allow-http", false, "Explicitly allow HTTP outside loopback")
	flags.DurationVar(&options.timeout, "timeout", options.timeout, "Complete request timeout")
	flags.StringVar(&options.format, "output", options.format, "Response format: json or raw")
	flags.StringVar(&options.bodyFile, "body-file", "", "Request body file; '-' reads stdin")
	flags.BoolVar(&options.bodyStdin, "body-stdin", false, "Read request body from stdin")
	flags.StringVar(&options.contentType, "content-type", "", "Request body media type")
	flags.StringVar(&options.responseHeader, "response-header", "", "Print one response header instead of the body, e.g. ETag")
	flags.Int64Var(&options.maxInput, "max-input-bytes", options.maxInput, "Maximum request body size")
	flags.Int64Var(&options.maxOutput, "max-output-bytes", options.maxOutput, "Maximum response body size")
	groups := map[string]*cobra.Command{"": root}
	paths := spec.Paths.Map()
	pathNames := make([]string, 0, len(paths))
	for path := range paths {
		pathNames = append(pathNames, path)
	}
	slices.Sort(pathNames)
	for _, path := range pathNames {
		item := paths[path]
		methods := make([]string, 0)
		for method := range item.Operations() {
			methods = append(methods, method)
		}
		slices.Sort(methods)
		for _, method := range methods {
			op := item.GetOperation(method)
			if ignored, _ := op.Extensions["x-cli-ignore"].(bool); ignored {
				continue
			}
			build := builders[op.OperationID]
			if build == nil {
				return nil, fmt.Errorf("missing generated request builder for %s", op.OperationID)
			}
			group := ""
			if len(op.Tags) > 0 {
				group = Kebab(op.Tags[0])
			}
			group = extension(op.Extensions, "x-cli-group", group)
			leaf := extension(op.Extensions, "x-cli-name", Kebab(op.OperationID))
			if leaf == "" || leaf != Kebab(leaf) || group != Kebab(group) {
				return nil, errors.New("CLI command names must be nonempty kebab-case names")
			}
			parent := groups[group]
			if parent == nil {
				if root.HasSubCommands() {
					for _, existing := range root.Commands() {
						if existing.Name() == group {
							return nil, fmt.Errorf("CLI group collision: %s", group)
						}
					}
				}
				parent = &cobra.Command{Use: group, Short: "Operations tagged " + group}
				root.AddCommand(parent)
				groups[group] = parent
			}
			if leaf == "help" || leaf == "completion" {
				return nil, fmt.Errorf("reserved command name: %s", leaf)
			}
			for _, existing := range parent.Commands() {
				if existing.Name() == leaf {
					return nil, fmt.Errorf("CLI command collision: %s %s", group, leaf)
				}
			}
			command := &cobra.Command{Use: leaf, Short: op.Summary, Long: op.Description, Args: cobra.NoArgs}
			if op.Deprecated {
				command.Deprecated = "deprecated by the OpenAPI contract"
			}
			parameters := Parameters(item, op)
			flagNames := map[string]int{}
			for _, p := range parameters {
				flagNames[Kebab(p.Name)]++
			}
			values := map[string]*parameterValue{}
			names := map[string]string{}
			for _, p := range parameters {
				if p.Schema == nil || p.Schema.Value == nil {
					return nil, fmt.Errorf("parameter %s must use schema", p.Name)
				}
				flagName := Kebab(p.Name)
				if flagNames[flagName] > 1 || flags.Lookup(flagName) != nil || flagName == "help" {
					flagName = p.In + "-" + flagName
				}
				flagName = extension(p.Extensions, "x-cli-flag", flagName)
				if flagName == "" || flagName != Kebab(flagName) || flags.Lookup(flagName) != nil || command.Flags().Lookup(flagName) != nil || flagName == "help" {
					return nil, fmt.Errorf("invalid or colliding CLI flag for %s", p.Name)
				}
				key := p.In + ":" + p.Name
				value := &parameterValue{kind: schemaKind(p.Schema.Value)}
				values[key] = value
				names[key] = flagName
				help := strings.ReplaceAll(p.Description, "`", "'")
				if p.Required {
					help += " (required)"
				}
				if value.kind == "array" {
					help += " (repeat flag for each item)"
				}
				command.Flags().Var(value, flagName, help)
				if value.kind == "boolean" {
					command.Flags().Lookup(flagName).NoOptDefVal = "true"
				}
				if p.Schema.Value.Default != nil {
					data, _ := json.Marshal(p.Schema.Value.Default)
					command.Flags().Lookup(flagName).DefValue = string(data)
				}
			}
			command.RunE = func(cmd *cobra.Command, _ []string) error {
				input := Input{Values: map[string]json.RawMessage{}}
				for _, p := range parameters {
					key := p.In + ":" + p.Name
					flagName := names[key]
					raw, err := parameterJSON(p, values[key], cmd.Flags().Changed(flagName))
					if err != nil {
						return &ExitError{2, fmt.Sprintf("invalid --%s: %s", flagName, err)}
					}
					if raw != nil {
						input.Values[key] = raw
					}
				}
				return execute(cmd, spec, op, prefix, options, input, build)
			}
			parent.AddCommand(command)
		}
	}
	return root, nil
}

func execute(cmd *cobra.Command, spec *openapi3.T, op *openapi3.Operation, prefix string, options *settings, input Input, build Builder) error {
	usage := func(message string) error { return &ExitError{2, message} }
	if options.timeout <= 0 || options.maxInput <= 0 || options.maxOutput <= 0 {
		return usage("timeout and byte limits must be positive")
	}
	if options.format != "json" && options.format != "raw" {
		return usage("--output must be json or raw")
	}
	u, err := serverURL(options.server, options.allowHTTP)
	if err != nil {
		return usage(err.Error())
	}
	input.Body, input.ContentType, err = readBody(cmd.InOrStdin(), op, options.bodyFile, options.bodyStdin, options.contentType, options.maxInput)
	if err != nil {
		return usage(err.Error())
	}
	// Match oapi-codegen's Client constructor: request builders resolve relative
	// paths, so the server must be a directory URL to preserve a base-path prefix.
	base := u.String()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	req, err := build(base, input)
	if err != nil {
		return usage("cannot encode request parameters for " + op.OperationID)
	}
	if req.URL.Scheme != u.Scheme || !strings.EqualFold(req.URL.Host, u.Host) {
		return usage("generated request changed the configured server origin")
	}
	transport, err := newTransport(options.caFile, options.certFile, options.keyFile)
	if err != nil {
		return usage(err.Error())
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(cmd.Context(), options.timeout)
	defer cancel()
	req = req.WithContext(ctx)
	if err := authenticate(req, spec, op, prefix, options.authEnv, options.certFile != ""); err != nil {
		return usage(err.Error())
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		message := "request transport failed"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			message = "request timed out"
		} else if errors.Is(ctx.Err(), context.Canceled) {
			message = "request cancelled"
		}
		return &ExitError{3, message}
	}
	defer response.Body.Close()
	if (response.StatusCode < 200 || response.StatusCode >= 300) && response.StatusCode != 304 {
		// An untrusted error response may echo credentials or secret request data.
		return &ExitError{4, fmt.Sprintf("HTTP %d %s", response.StatusCode, http.StatusText(response.StatusCode))}
	}
	if options.responseHeader != "" {
		value := response.Header.Get(options.responseHeader)
		if value == "" {
			return &ExitError{4, "response header is absent"}
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), value)
		return err
	}
	if options.format == "raw" {
		_, err := io.Copy(cmd.OutOrStdout(), io.LimitReader(response.Body, options.maxOutput))
		if err != nil {
			return &ExitError{3, "cannot stream response"}
		}
		var probe [1]byte
		if n, err := response.Body.Read(probe[:]); n > 0 || (err != nil && err != io.EOF) {
			return &ExitError{3, "response exceeds the configured byte limit"}
		}
		return nil
	}
	data, err := boundedRead(response.Body, options.maxOutput)
	if err != nil {
		return &ExitError{3, "cannot read response within the configured byte limit"}
	}
	if len(data) == 0 {
		return nil
	}
	if !json.Valid(data) {
		return &ExitError{3, "response is not JSON; use --output raw"}
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return &ExitError{3, "cannot format JSON response"}
	}
	pretty.WriteByte('\n')
	_, err = pretty.WriteTo(cmd.OutOrStdout())
	return err
}
