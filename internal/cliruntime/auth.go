package cliruntime

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
)

func envName(s string) string {
	var out strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			previous := runes[i-1]
			if unicode.IsLower(previous) {
				out.WriteByte('_')
			}
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(unicode.ToUpper(r))
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

func serverURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("--server must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("--server must use HTTP(S)")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme == "http" && !loopback && !allowHTTP {
		return nil, errors.New("HTTPS is required; --allow-http explicitly permits an HTTP development server")
	}
	return u, nil
}

func newTransport(caFile, certFile, keyFile string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, errors.New("cannot read --ca-file")
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, errors.New("--ca-file contains no PEM certificates")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("--client-cert and --client-key must be supplied together")
	}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, errors.New("cannot load the TLS client certificate/key pair")
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{pair}
	}
	return transport, nil
}

func authenticate(req *http.Request, spec *openapi3.T, op *openapi3.Operation, prefix string, mappings []string, clientCert bool) error {
	requirements := spec.Security
	if op.Security != nil {
		requirements = *op.Security
	}
	if len(requirements) == 0 {
		return nil
	}
	overrides := map[string]string{}
	for _, mapping := range mappings {
		name, variable, ok := strings.Cut(mapping, "=")
		if !ok || name == "" || variable == "" {
			return errors.New("--auth-env expects securityScheme=ENVIRONMENT_VARIABLE")
		}
		if spec.Components == nil || spec.Components.SecuritySchemes[name] == nil {
			return errors.New("--auth-env names an unknown security scheme")
		}
		overrides[name] = variable
	}
	for _, requirement := range requirements {
		if len(requirement) == 0 {
			return nil
		}
		names := make([]string, 0, len(requirement))
		for name := range requirement {
			names = append(names, name)
		}
		slices.Sort(names)
		candidate := req.Clone(req.Context())
		candidate.Header = req.Header.Clone()
		u := *req.URL
		candidate.URL = &u
		available := true
		for _, name := range names {
			ref := spec.Components.SecuritySchemes[name]
			if ref == nil || ref.Value == nil {
				return errors.New("contract references an unknown security scheme")
			}
			scheme := ref.Value
			variable := prefix + "_" + envName(name)
			bearer := scheme.Type == "oauth2" || scheme.Type == "openIdConnect" || (scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "bearer"))
			if bearer {
				variable = prefix + "_TOKEN"
			}
			if override, ok := overrides[name]; ok {
				variable = override
			}
			credential, exists := os.LookupEnv(variable)
			if scheme.Type == "mutualTLS" {
				if !clientCert {
					available = false
				}
				continue
			}
			if !exists || credential == "" {
				available = false
				break
			}
			if strings.ContainsAny(credential, "\r\n\x00") {
				return errors.New("credential contains an invalid control character")
			}
			switch {
			case bearer:
				candidate.Header.Set("Authorization", "Bearer "+credential)
			case scheme.Type == "http" && strings.EqualFold(scheme.Scheme, "basic"):
				user, password, ok := strings.Cut(credential, ":")
				if !ok {
					return fmt.Errorf("%s must contain username:password", variable)
				}
				candidate.SetBasicAuth(user, password)
			case scheme.Type == "apiKey":
				switch scheme.In {
				case "header":
					candidate.Header.Set(scheme.Name, credential)
				case "query":
					query := candidate.URL.Query()
					query.Set(scheme.Name, credential)
					candidate.URL.RawQuery = query.Encode()
				case "cookie":
					for _, r := range credential {
						if r < 0x21 || r > 0x7e || r == '"' || r == ',' || r == ';' || r == '\\' {
							return errors.New("API-key cookie credential contains an invalid cookie character")
						}
					}
					candidate.AddCookie(&http.Cookie{Name: scheme.Name, Value: credential})
				default:
					return errors.New("unsupported API-key location")
				}
			default:
				return fmt.Errorf("unsupported security scheme %s", name)
			}
		}
		if available {
			req.Header = candidate.Header
			req.URL = candidate.URL
			return nil
		}
	}
	return fmt.Errorf("authentication required: set %s_TOKEN for bearer auth or use --auth-env scheme=ENV", prefix)
}
