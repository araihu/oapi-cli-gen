package cliruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
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

// Diagnostic formats local failures. HTTP failures already carry the selected
// representation, including Problem Details in JSON mode.
func Diagnostic(name string, documentation map[string]string, root *cobra.Command, args []string, err error) string {
	exitCode := ExitCode(err)
	var code, title string
	switch exitCode {
	case 2:
		code, title = "usage_error", "Invalid command usage"
	case 3:
		code, title = "transport_error", "Transport error"
	default:
		return err.Error()
	}
	if diagnosticOutput(root, args) != "json" {
		return err.Error()
	}
	diagnostic := struct {
		Type          string `json:"type"`
		Title         string `json:"title"`
		Detail        string `json:"detail"`
		Code          string `json:"code"`
		ExitCode      int    `json:"exitCode"`
		Documentation string `json:"documentation,omitempty"`
	}{
		Documentation: documentation[code],
		Type:          "urn:cli:" + name + ":problem:" + code,
		Title:         title, Detail: err.Error(), Code: code, ExitCode: exitCode,
	}
	data, _ := json.MarshalIndent(diagnostic, "", "  ")
	return string(data)
}

// Cobra can reject a command or flag before parsing --output. Recover the
// selected format while respecting known flag values and the option delimiter.
func diagnosticOutput(root *cobra.Command, args []string) string {
	var command *cobra.Command
	if root != nil {
		command, _, _ = root.Find(args)
	}
	var format string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name, value, assigned := strings.Cut(arg[2:], "=")
		if !assigned {
			takesValue := name == "output"
			if command != nil {
				if flag := command.Flag(name); flag != nil {
					takesValue = flag.NoOptDefVal == ""
				}
			}
			if takesValue && i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		if name == "output" {
			format = value
		}
	}
	return format
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

// commandGroups accepts the original single group and an ordered group path.
func commandGroups(op *openapi3.Operation) ([]string, error) {
	value, exists := op.Extensions["x-cli-group"]
	if !exists {
		if len(op.Tags) > 0 {
			if group := Kebab(op.Tags[0]); group != "" {
				return []string{group}, nil
			}
		}
		return nil, nil
	}

	switch value := value.(type) {
	case string:
		if value == "" {
			return nil, nil
		}
		return []string{value}, nil
	case []any:
		groups := make([]string, 0, len(value))
		for _, item := range value {
			group, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("invalid x-cli-group for %s: each path segment must be a string", op.OperationID)
			}
			groups = append(groups, group)
		}
		return groups, nil
	default:
		return nil, fmt.Errorf("invalid x-cli-group for %s: expected a string or a list of strings", op.OperationID)
	}
}

func childCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, child := range parent.Commands() {
		if child.Name() == name {
			return child
		}
	}
	return nil
}

func commandParent(root *cobra.Command, groups []string) (*cobra.Command, error) {
	parent := root
	for _, group := range groups {
		if group == "" || group != Kebab(group) {
			return nil, errors.New("CLI group names must be nonempty kebab-case names")
		}
		if group == "help" || group == "completion" {
			return nil, fmt.Errorf("reserved CLI group name: %s", group)
		}
		child := childCommand(parent, group)
		if child == nil {
			short := "Operations for " + group
			if parent == root {
				short = "Operations tagged " + group
			}
			child = &cobra.Command{
				Use: group, Short: short, Args: cobra.NoArgs,
				RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
			}
			parent.AddCommand(child)
		} else if !child.HasSubCommands() {
			return nil, fmt.Errorf("CLI group collision: %s %s", parent.CommandPath(), group)
		}
		parent = child
	}
	return parent, nil
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
	options := &settings{server: defaultServer(spec), timeout: 60 * time.Second, format: "table", maxInput: 8 << 20, maxOutput: 64 << 20}
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
	flags.StringVar(&options.format, "output", options.format, "Response format: table, json, csv or raw (event streams default to raw)")
	if err := root.RegisterFlagCompletionFunc("output", cobra.FixedCompletions([]string{"table", "json", "csv", "raw"}, cobra.ShellCompDirectiveNoFileComp)); err != nil {
		return nil, err
	}
	flags.StringVar(&options.bodyFile, "body-file", "", "Request body file; '-' reads stdin")
	flags.BoolVar(&options.bodyStdin, "body-stdin", false, "Read request body from stdin")
	flags.StringVar(&options.contentType, "content-type", "", "Request body media type")
	flags.StringVar(&options.responseHeader, "response-header", "", "Print one response header instead of the body, e.g. ETag")
	flags.Int64Var(&options.maxInput, "max-input-bytes", options.maxInput, "Maximum request body size")
	flags.Int64Var(&options.maxOutput, "max-output-bytes", options.maxOutput, "Maximum response body size")
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
			groups, err := commandGroups(op)
			if err != nil {
				return nil, err
			}
			parent, err := commandParent(root, groups)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", op.OperationID, err)
			}
			leaf := extension(op.Extensions, "x-cli-name", Kebab(op.OperationID))
			if leaf == "" || leaf != Kebab(leaf) {
				return nil, errors.New("CLI command names must be nonempty kebab-case names")
			}
			if leaf == "help" || leaf == "completion" {
				return nil, fmt.Errorf("reserved command name: %s", leaf)
			}
			if childCommand(parent, leaf) != nil {
				return nil, fmt.Errorf("CLI command collision: %s %s", parent.CommandPath(), leaf)
			}
			command := &cobra.Command{
				Use: leaf, Short: op.Summary, Args: cobra.NoArgs,
				Long:    extension(op.Extensions, "x-cli-description", op.Description),
				Example: extension(op.Extensions, "x-cli-example", ""),
			}
			if op.Deprecated {
				command.Hidden = true
				command.Long = strings.TrimSpace(command.Long + "\n\nDeprecated by the OpenAPI contract.")
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
				if op.Deprecated && options.format != "json" {
					cmd.PrintErrf("Command %q is deprecated by the OpenAPI contract.\n", cmd.Name())
				}
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
	if options.format != "table" && options.format != "json" && options.format != "csv" && options.format != "raw" {
		return usage("--output must be table, json, csv or raw")
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
	authenticated, err := authenticate(req, spec, op, prefix, options.authEnv, options.certFile != "")
	if err != nil {
		return usage(err.Error())
	}
	var proxyAuthenticated atomic.Bool
	if selectProxy := transport.Proxy; selectProxy != nil {
		transport.Proxy = func(request *http.Request) (*url.URL, error) {
			proxy, err := selectProxy(request)
			if proxy != nil && proxy.User != nil {
				proxyAuthenticated.Store(true)
			}
			return proxy, err
		}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return transportError(ctx, "request transport failed")
	}
	defer response.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if (response.StatusCode < 200 || response.StatusCode >= 300) && response.StatusCode != 304 {
		// An error can reflect credentials or other values from the request.
		serverPath := u.EscapedPath()
		privateInput := len(input.Body) != 0 || input.ContentType != "" || len(input.Values) != 0 ||
			authenticated || proxyAuthenticated.Load() || options.certFile != "" || (serverPath != "" && serverPath != "/")
		return httpFailure(ctx, op, response, mediaType, options.format, options.maxOutput, privateInput)
	}
	if options.responseHeader != "" {
		values := response.Header.Values(options.responseHeader)
		if len(values) == 0 {
			return &ExitError{3, "response header is absent"}
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), values[0]); err != nil {
			return &ExitError{3, "cannot write response header"}
		}
		return nil
	}
	format := options.format
	if mediaType == "text/event-stream" && !cmd.Flags().Changed("output") {
		if responseMedia(op, response.StatusCode, mediaType) == nil {
			return &ExitError{3, "operation does not declare an event stream; use --output raw"}
		}
		format = "raw"
	}
	if format == "raw" {
		_, err := io.Copy(cmd.OutOrStdout(), io.LimitReader(response.Body, options.maxOutput))
		if err != nil {
			return transportError(ctx, "cannot stream response")
		}
		var probe [1]byte
		if n, err := io.ReadFull(response.Body, probe[:]); n > 0 {
			return &ExitError{3, "response exceeds the configured byte limit"}
		} else if err != nil && err != io.EOF {
			return transportError(ctx, "cannot stream response")
		}
		return nil
	}
	data, err := boundedRead(response.Body, options.maxOutput)
	if err != nil {
		return transportError(ctx, "cannot read response within the configured byte limit")
	}
	if len(data) == 0 {
		return nil
	}
	return writeResponse(cmd.OutOrStdout(), data, format, options.maxOutput)
}

// transportError keeps transport details out of diagnostics and reports the
// request deadline even when it expires after the response headers arrive.
func transportError(ctx context.Context, message string) error {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		message = "request timed out"
	case context.Canceled:
		message = "request cancelled"
	}
	return &ExitError{3, message}
}

type httpProblem struct {
	Type          string `json:"type"`
	Title         string `json:"title,omitempty"`
	Status        int    `json:"status"`
	Code          string `json:"code,omitempty"`
	Documentation string `json:"documentation,omitempty"`
}

func httpFailure(ctx context.Context, op *openapi3.Operation, response *http.Response, mediaType, format string, maxOutput int64, privateInput bool) error {
	problem, err := readHTTPProblem(op, response, mediaType, maxOutput, privateInput)
	if err != nil && !errors.Is(err, errBodyTooLarge) {
		return transportError(ctx, "cannot read error response")
	}
	if format == "json" {
		data, _ := json.MarshalIndent(problem, "", "  ")
		return &ExitError{4, string(data)}
	}
	message := fmt.Sprintf("HTTP %d %s", problem.Status, http.StatusText(problem.Status))
	if problem.Code != "" {
		message += " (" + problem.Code + ")"
	}
	if problem.Documentation != "" {
		message += "\nDocumentation: " + problem.Documentation
	}
	return &ExitError{4, message}
}

func responseMedia(op *openapi3.Operation, status int, mediaType string) *openapi3.MediaType {
	if op.Responses == nil {
		return nil
	}
	contract := op.Responses.Status(status)
	if contract == nil {
		contract = op.Responses.Default()
	}
	if contract == nil || contract.Value == nil {
		return nil
	}
	return contract.Value.Content[mediaType]
}

func readHTTPProblem(op *openapi3.Operation, response *http.Response, mediaType string, maxOutput int64, privateInput bool) (httpProblem, error) {
	problem := httpProblem{Type: "about:blank", Title: http.StatusText(response.StatusCode), Status: response.StatusCode}
	if mediaType != "application/problem+json" {
		return problem, nil
	}
	media := responseMedia(op, response.StatusCode, mediaType)
	if media == nil {
		return problem, nil
	}
	data, err := boundedRead(response.Body, min(maxOutput, 1<<20))
	if err != nil {
		return problem, err
	}
	var body struct {
		Type          json.RawMessage `json:"type"`
		Title         json.RawMessage `json:"title"`
		Code          json.RawMessage `json:"code"`
		Documentation json.RawMessage `json:"documentation"`
	}
	if json.Unmarshal(data, &body) != nil {
		return problem, nil
	}
	if media.Schema != nil && media.Schema.Value != nil {
		schema := media.Schema.Value
		problem.Code = declaredProblemCode(schema.Properties["code"], body.Code)
		if catalog, ok := schema.Extensions["x-cli-problem-catalog"].(map[string]any); ok {
			// A configured catalog defines the public diagnostics for this schema.
			// The response body selects a declared code only.
			if titles, ok := catalog["titles"].(map[string]any); ok && problem.Code != "" {
				if title, ok := titles[problem.Code].(string); ok {
					if prefix, ok := catalog["type-prefix"].(string); ok {
						problem.Type = prefix + problem.Code
					}
					problem.Title = title
					if base, ok := catalog["documentation-base"].(string); ok {
						problem.Documentation = base + "#" + problem.Code
					}
				}
			}
			return problem, nil
		}
		if ref := schema.Properties["documentation"]; !privateInput && ref != nil && ref.Value != nil && ref.Value.Format == "uri" {
			problem.Documentation = problemDocumentation(body.Documentation)
		}
	}
	if privateInput {
		return problem, nil
	}
	if u, value := problemURI(body.Type); u != nil {
		if u.IsAbs() {
			problem.Type = value
		} else if response.Request != nil {
			// Resolve a relative problem identifier against the retrieval URI.
			base := *response.Request.URL
			base.RawQuery, base.ForceQuery = "", false
			problem.Type = base.ResolveReference(u).String()
		}
	}
	if problem.Type != "about:blank" {
		var title string
		if json.Unmarshal(body.Title, &title) == nil && title != "" && len(title) <= 160 && strings.IndexFunc(title, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
			problem.Title = title
		}
	}
	return problem, nil
}

func declaredProblemCode(ref *openapi3.SchemaRef, raw json.RawMessage) string {
	var code string
	if ref == nil || ref.Value == nil || json.Unmarshal(raw, &code) != nil || code == "" || len(code) > 64 ||
		strings.IndexFunc(code, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
		}) >= 0 {
		return ""
	}
	if slices.ContainsFunc(ref.Value.Enum, func(value any) bool { allowed, ok := value.(string); return ok && allowed == code }) {
		return code
	}
	return ""
}

// A problem link is printed for the user to inspect. Reject terminal controls
// and URL fields commonly used for credentials before rendering it.
func problemDocumentation(raw json.RawMessage) string {
	u, value := problemURI(raw)
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery {
		return ""
	}
	return value
}

func problemURI(raw json.RawMessage) (*url.URL, string) {
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" || len(value) > 2048 ||
		strings.IndexFunc(value, func(r rune) bool { return r < '!' || r > '~' }) >= 0 {
		return nil, ""
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return nil, ""
	}
	return u, value
}
