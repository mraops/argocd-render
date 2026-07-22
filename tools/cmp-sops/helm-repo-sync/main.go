// Command helm-repo-sync materializes helm's repositories.yaml from ArgoCD
// repository Secrets. It is meant to run inside the ArgoCD repo-server CMP
// sidecar before `helm dep build`, so private helm-chart dependencies resolve
// with the same credentials already configured in ArgoCD Repositories.
//
// ArgoCD v3 does not pass repository credentials into CMP sidecars (only Git
// creds via provideGitCreds). This binary closes that gap for HTTP/HTTPS helm
// repositories by reading the Secrets labeled
// argocd.argoproj.io/secret-type=repository (type: helm, not OCI) through the
// in-cluster Kubernetes API and writing a helm-compatible repositories.yaml.
//
// It is idempotent: a successful run writes a timestamp marker; subsequent runs
// within CMP_HELM_REPO_SYNC_TTL seconds exit immediately. Credentials are written
// to the output file (mode 0600) but never logged.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	labelKey    = "argocd.argoproj.io/secret-type"
	labelValue  = "repository"
	defaultTTL  = 86400 // seconds
	defaultOut  = "/tmp/helm-config/repositories.yaml"
	saTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNSFile    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	apiHostEnv  = "CMP_HELM_REPO_SYNC_API_HOST"  // default: kubernetes.default.svc
	ttlEnv      = "CMP_HELM_REPO_SYNC_TTL"       // seconds
	outEnv      = "CMP_HELM_REPO_SYNC_OUT"       // repositories.yaml path
	nsEnv       = "CMP_HELM_REPO_SYNC_NAMESPACE" // namespace to read secrets from
	insecureEnv = "CMP_HELM_REPO_SYNC_INSECURE"  // skip TLS verify (debug only)
)

// secret mirrors the fields we need from a Kubernetes Secret.
type secret struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// secretList is the top-level list response.
type secretList struct {
	Items []secret `json:"items"`
}

// repoEntry is one line in helm's repositories.yaml.
type repoEntry struct {
	Name     string
	URL      string
	Username string
	Password string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[helm-repo-sync] ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	out := envOr(outEnv, defaultOut)

	// TTL short-circuit: skip the API entirely when the cache is fresh.
	if ttl := envInt(ttlEnv, defaultTTL); ttl > 0 {
		if age, ok := fileAge(out); ok && age < time.Duration(ttl)*time.Second {
			fmt.Fprintf(os.Stderr, "[helm-repo-sync] repositories.yaml up to date (age %ds, ttl %ds)\n", int(age.Seconds()), ttl)
			return nil
		}
	}

	token, err := os.ReadFile(saTokenFile)
	if err != nil {
		return fmt.Errorf("read serviceaccount token: %w", err)
	}
	ns, err := resolveNamespace()
	if err != nil {
		return err
	}
	host := envOr(apiHostEnv, "kubernetes.default.svc")

	client, err := newHTTPClient()
	if err != nil {
		return fmt.Errorf("build http client: %w", err)
	}

	secrets, err := listSecrets(client, host, ns, strings.TrimSpace(string(token)))
	if err != nil {
		return err
	}

	entries, skipped := filterSecrets(secrets)
	if len(entries) == 0 {
		// No helm repos configured. Write an empty (but valid) repositories.yaml
		// so helm sees a well-formed file and we still record the sync timestamp.
		fmt.Fprintf(os.Stderr, "[helm-repo-sync] no http helm repositories found (%d secrets seen, %d skipped)\n", len(secrets), skipped)
		return writeRepositories(out, nil, time.Now())
	}

	fmt.Fprintf(os.Stderr, "[helm-repo-sync] synced %d http helm repos (%d skipped)\n", len(entries), skipped)
	return writeRepositories(out, entries, time.Now())
}

// resolveNamespace picks the namespace from env, then the serviceaccount file.
func resolveNamespace() (string, error) {
	if v := os.Getenv(nsEnv); v != "" {
		return v, nil
	}
	b, err := os.ReadFile(saNSFile)
	if err != nil {
		return "", fmt.Errorf("read serviceaccount namespace (set %s to override): %w", nsEnv, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// newHTTPClient builds an http.Client trusting the cluster CA from the
// serviceaccount secret. CMP_HELM_REPO_SYNC_INSECURE=1 disables verification
// (debug only).
func newHTTPClient() (*http.Client, error) {
	insecure := os.Getenv(insecureEnv) == "1" || os.Getenv(insecureEnv) == "true"
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tlsConf.InsecureSkipVerify = true
	} else {
		caPEM, err := os.ReadFile(saCAFile)
		if err != nil {
			return nil, fmt.Errorf("read serviceaccount ca.crt: %w", err)
		}
		pool := x509.NewCertPool()
		// ca.crt may contain one or more PEM blocks; AppendCertsFromPEM handles
		// all of them. Reject obviously-empty inputs rather than failing silently.
		if !pool.AppendCertsFromPEM(caPEM) {
			// Fall back to parsing each block explicitly to give a clearer error.
			if _, decodeErr := pem.Decode(caPEM); decodeErr == nil && len(caPEM) == 0 {
				return nil, errors.New("serviceaccount ca.crt is empty")
			}
			return nil, errors.New("serviceaccount ca.crt contains no usable certificates")
		}
		tlsConf.RootCAs = pool
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConf}}, nil
}

// listSecrets queries the Kubernetes API for ArgoCD repository Secrets.
func listSecrets(client *http.Client, host, ns, token string) ([]secret, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets", ns)
	q := fmt.Sprintf("labelSelector=%s=%s", labelKey, labelValue)
	url := fmt.Sprintf("https://%s%s?%s", host, path, q)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s api request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("k8s api %s: status %d: %s", path, resp.StatusCode, truncate(body, 512))
	}

	var list secretList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode secret list: %w", err)
	}
	return list.Items, nil
}

// filterSecrets keeps HTTP/HTTPS helm repositories (type: helm, not OCI) and
// drops git/OCI/invalid entries. The Kubernetes API returns Secret `data`
// base64-encoded (whether the Secret was created via `data` or `stringData`),
// so each field is decoded to its plaintext before any comparison or use.
// A field that fails to decode (should not happen for a well-formed Secret)
// causes the whole Secret to be skipped with a warning.
func filterSecrets(items []secret) (entries []repoEntry, skipped int) {
	for _, s := range items {
		// Decode every data field from base64. Kubernetes stores all Secret
		// values base64-encoded in `data`; without this, comparisons like
		// `type == "helm"` silently never match (base64("helm") != "helm")
		// and credentials land in repositories.yaml still encoded.
		dec := make(map[string]string, len(s.Data))
		bad := false
		for k, v := range s.Data {
			if v == "" {
				dec[k] = ""
				continue
			}
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[helm-repo-sync] skip %s: field %q is not valid base64: %v\n", s.Metadata.Name, k, err)
				bad = true
				break
			}
			dec[k] = string(b)
		}
		if bad {
			skipped++
			continue
		}
		s.Data = dec

		typ := s.Data["type"]
		url := s.Data["url"]
		name := s.Data["name"]
		// Name falls back to the Secret name (helm requires a repo name).
		if name == "" {
			name = s.Metadata.Name
		}
		// Only HTTP/HTTPS helm charts. OCI needs `helm registry login`, git is
		// covered by ArgoCD's native provideGitCreds — both out of scope here.
		if typ != "helm" {
			skipped++
			fmt.Fprintf(os.Stderr, "[helm-repo-sync] skip %s: type=%q (not helm)\n", name, typ)
			continue
		}
		if strings.EqualFold(s.Data["enableOCI"], "true") {
			skipped++
			fmt.Fprintf(os.Stderr, "[helm-repo-sync] skip %s: OCI registry (use helm registry login)\n", name)
			continue
		}
		if url == "" {
			skipped++
			fmt.Fprintf(os.Stderr, "[helm-repo-sync] skip %s: empty url\n", name)
			continue
		}
		entries = append(entries, repoEntry{
			Name:     name,
			URL:      url,
			Username: s.Data["username"],
			Password: s.Data["password"],
		})
		fmt.Fprintf(os.Stderr, "[helm-repo-sync] add %s -> %s\n", name, url)
	}
	return entries, skipped
}

// writeRepositories renders helm's repositories.yaml and writes it atomically.
// The file is created with mode 0600 and its directory with 0700 so the
// plaintext credentials stay readable only by the sidecar user.
func writeRepositories(path string, entries []repoEntry, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	var b strings.Builder
	b.WriteString("apiVersion: v1\n")
	b.WriteString("generated: \"" + now.UTC().Format(time.RFC3339) + "\"\n")
	if len(entries) == 0 {
		// helm accepts an empty repositories list; keep the key for clarity.
		b.WriteString("repositories: []\n")
	} else {
		b.WriteString("repositories:\n")
		for _, e := range entries {
			b.WriteString(fmt.Sprintf("  - name: %s\n", yamlScalar(e.Name)))
			b.WriteString(fmt.Sprintf("    url: %s\n", yamlScalar(e.URL)))
			if e.Username != "" {
				b.WriteString(fmt.Sprintf("    username: %s\n", yamlScalar(e.Username)))
			}
			if e.Password != "" {
				b.WriteString(fmt.Sprintf("    password: %s\n", yamlScalar(e.Password)))
			}
		}
	}

	tmp, err := os.CreateTemp(dir, ".repositories.yaml.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails.
	defer func() {
		if _, err := os.Stat(tmpName); err == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := os.Chmod(tmpName, 0600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename -> %s: %w", path, err)
	}
	return nil
}

// yamlScalar quotes a string for YAML when it could otherwise be misparsed
// (special chars, leading indicators, empty). Plain scalars are left unquoted
// for readability; everything else gets double-quoted with escaping.
func yamlScalar(s string) string {
	if s == "" {
		return "\"\""
	}
	if isPlainSafe(s) {
		return s
	}
	// Double-quote and escape backslashes + quotes.
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return "\"" + r.Replace(s) + "\""
}

// isPlainSafe reports whether s can be emitted as a YAML plain scalar without
// ambiguity. Conservative: anything with control chars, leading indicators,
// or characters that change YAML parsing is quoted.
func isPlainSafe(s string) bool {
	if s == "" {
		return false
	}
	switch s { // YAML reserved literals
	case "null", "Null", "NULL", "~", "true", "True", "TRUE", "false", "False", "FALSE":
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	// Leading indicators that change YAML parsing; '-' is unsafe as the first
	// char (list item), so it is quoted unconditionally for simplicity.
	switch s[0] {
	case '!', '&', '*', '?', '|', '>', '%', '@', '`', '"', '\'', '#', ',', '[', ']', '{', '}', ' ', '\t', '-':
		return false
	}
	if strings.ContainsAny(s, ": \t#\"'") {
		// ": " starts a mapping, " #" starts a comment, embedded quotes risk
		// ambiguity — quote to be safe.
		return false
	}
	return true
}

// --- helpers ---

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// fileAge returns the age of path's mtime. ok=false if the file doesn't exist.
func fileAge(path string) (time.Duration, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
