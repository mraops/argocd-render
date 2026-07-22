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
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	labelKey       = "argocd.argoproj.io/secret-type"
	labelValue     = "repository"
	defaultTTL     = 86400                                   // seconds
	defaultOutTmpl = "/tmp/helm-config/repositories-%s.yaml" // %s = AppProject name
	saTokenFile    = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAFile       = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNSFile       = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	apiHostEnv     = "CMP_HELM_REPO_SYNC_API_HOST"  // default: kubernetes.default.svc
	ttlEnv         = "CMP_HELM_REPO_SYNC_TTL"       // seconds
	outEnv         = "CMP_HELM_REPO_SYNC_OUT"       // repositories.yaml path (overrides per-project default)
	nsEnv          = "CMP_HELM_REPO_SYNC_NAMESPACE" // namespace to read secrets/appprojects from
	insecureEnv    = "CMP_HELM_REPO_SYNC_INSECURE"  // skip TLS verify (debug only)
	appProjectEnv  = "ARGOCD_APP_PROJECT_NAME"      // injected by ArgoCD into the CMP plugin process
	allowAll       = "*"                            // sourceRepos wildcard: permits any URL
)

// appVersion is injected at build time via -ldflags "-X main.appVersion=...".
// Defaults to "dev" for local builds; CI stamps it with the git tag.
var appVersion = "dev"

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

// appProject mirrors the fields we need from an ArgoCD AppProject. The
// project's spec.sourceRepos is the allowlist that credentials and chart
// dependencies are filtered against. Metadata.ResourceVersion is captured so
// the cache can be invalidated the instant the AppProject changes (e.g. an
// operator removes a repo from sourceRepos).
type appProject struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		SourceRepos []string `json:"sourceRepos"`
	} `json:"spec"`
}

// repoEntry is one line in helm's repositories.yaml.
type repoEntry struct {
	Name     string
	URL      string
	Username string
	Password string
}

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	chartDir := flag.String("chart", "", "Chart directory whose Chart.yaml dependencies are validated against sourceRepos (optional)")
	flag.Parse()
	if *showVersion {
		fmt.Printf("helm-repo-sync %s\n", appVersion)
		return
	}
	if err := run(*chartDir); err != nil {
		fmt.Fprintf(os.Stderr, "[helm-repo-sync] ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run(chartDir string) error {
	// Resolve the AppProject first. Without it there is no sourceRepos
	// allowlist and we fail-closed (refuse to materialize any credentials).
	project, err := resolveProject()
	if err != nil {
		return err
	}

	// Output path is per-project so apps in different projects never share a
	// repositories.yaml (each carries only its own allowed credentials).
	// CMP_HELM_REPO_SYNC_OUT overrides the default template for debugging.
	out := fmt.Sprintf(defaultOutTmpl, project)
	if v := envOr(outEnv, ""); v != "" {
		out = v
	}
	// The resourceVersion is cached next to repositories.yaml so the cache can
	// be invalidated the instant the AppProject changes (not just on TTL).
	rvFile := out + ".rv"

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

	// Always fetch the AppProject. This is a small, cheap GET, and it is the
	// only way to detect sourceRepos changes without waiting for TTL — the whole
	// point of resourceVersion-based invalidation. Fail-closed on any error.
	allowed, curRV, err := fetchAppProject(client, host, ns, project, strings.TrimSpace(string(token)))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[helm-repo-sync] project=%s sourceRepos=%d rv=%s\n", project, len(allowed), curRV)

	// Cache short-circuit: if the AppProject resourceVersion is unchanged AND
	// the repositories.yaml is still within TTL, skip the Secret list + filter
	// (the expensive part). An operator editing sourceRepos bumps resourceVersion,
	// which invalidates this immediately even mid-TTL.
	if ttl := envInt(ttlEnv, defaultTTL); ttl > 0 {
		if cachedRV := readRV(rvFile); cachedRV == curRV && cachedRV != "" {
			if age, ok := fileAge(out); ok && age < time.Duration(ttl)*time.Second {
				fmt.Fprintf(os.Stderr, "[helm-repo-sync] cache hit (project=%s, rv=%s, age %ds, ttl %ds)\n", project, curRV, int(age.Seconds()), ttl)
				fmt.Println(out)
				return nil
			}
		}
	}

	// Validate Chart.yaml dependencies against sourceRepos BEFORE materializing
	// credentials. Catches public-repo URLs the project doesn't allow.
	if chartDir != "" {
		if err := validateChartDeps(chartDir, allowed); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[helm-repo-sync] Chart.yaml dependencies OK (project=%s)\n", project)
	}

	secrets, err := listSecrets(client, host, ns, strings.TrimSpace(string(token)))
	if err != nil {
		return err
	}

	entries, skipped := filterSecrets(secrets, allowed)
	if len(entries) == 0 {
		// No allowed helm repos. Write an empty (but valid) repositories.yaml so
		// helm sees a well-formed file and dep build fails clearly on missing
		// credentials rather than a parse error.
		fmt.Fprintf(os.Stderr, "[helm-repo-sync] no allowed helm repos for project=%s (%d secrets seen, %d skipped)\n", project, len(secrets), skipped)
		if err := writeRepositories(out, nil, time.Now()); err != nil {
			return err
		}
		writeRV(rvFile, curRV)
		fmt.Println(out)
		return nil
	}

	fmt.Fprintf(os.Stderr, "[helm-repo-sync] synced %d helm repos for project=%s (%d skipped)\n", len(entries), project, skipped)
	if err := writeRepositories(out, entries, time.Now()); err != nil {
		return err
	}
	writeRV(rvFile, curRV)
	fmt.Println(out)
	return nil
}

// readRV returns the cached AppProject resourceVersion, or "" if absent.
func readRV(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeRV stores the AppProject resourceVersion alongside repositories.yaml so
// the next run can detect AppProject changes and invalidate the cache. The file
// holds only the opaque version string (no secrets).
func writeRV(path, rv string) {
	if rv == "" {
		return
	}
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(path, []byte(rv), 0600)
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

// resolveProject returns the AppProject name injected by ArgoCD into the CMP
// plugin process via ARGOCD_APP_PROJECT_NAME. It is required: without a project
// there is no sourceRepos allowlist to filter against, and we fail-closed.
func resolveProject() (string, error) {
	v := os.Getenv(appProjectEnv)
	if v == "" {
		return "", fmt.Errorf("%s env is empty; ArgoCD must inject the AppProject name (fail-closed: no allowlist)", appProjectEnv)
	}
	return v, nil
}

// fetchAppProject reads an ArgoCD AppProject and returns its spec.sourceRepos
// and metadata.resourceVersion. The resourceVersion is used for cache
// invalidation: when the AppProject changes (e.g. sourceRepos edited), the
// version bumps and the cache is rebuilt immediately rather than waiting for
// TTL. Any API error (404, RBAC denial, network) is fatal — fail-closed: an
// unreadable project means we cannot enforce sourceRepos.
func fetchAppProject(client *http.Client, host, ns, name, token string) (sourceRepos []string, resourceVersion string, err error) {
	path := fmt.Sprintf("/apis/argoproj.io/v1alpha1/namespaces/%s/appprojects/%s", ns, name)
	url := fmt.Sprintf("https://%s%s", host, path)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("k8s api request for AppProject %s: %w", name, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("k8s api %s: status %d: %s", path, resp.StatusCode, truncate(body, 512))
	}

	var proj appProject
	if err := json.Unmarshal(body, &proj); err != nil {
		return nil, "", fmt.Errorf("decode AppProject %s: %w", name, err)
	}
	return proj.Spec.SourceRepos, proj.Metadata.ResourceVersion, nil
}

// isAllowed reports whether url is permitted by the sourceRepos allowlist.
// Matching is exact string equality; the wildcard "*" permits any URL.
func isAllowed(url string, allowed []string) bool {
	for _, a := range allowed {
		if a == allowAll || a == url {
			return true
		}
	}
	return false
}

// filterSecrets keeps HTTP/HTTPS helm repositories (type: helm, not OCI) whose
// URL is permitted by the AppProject sourceRepos allowlist, and drops the rest.
// The Kubernetes API returns Secret `data` base64-encoded (whether the Secret
// was created via `data` or `stringData`), so each field is decoded to its
// plaintext before any comparison or use. A field that fails to decode (should
// not happen for a well-formed Secret) causes the whole Secret to be skipped
// with a warning.
func filterSecrets(items []secret, allowed []string) (entries []repoEntry, skipped int) {
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
		// sourceRepos gate: only materialize credentials for repos the AppProject
		// explicitly allows. This closes the cross-project credential leak.
		if !isAllowed(url, allowed) {
			skipped++
			fmt.Fprintf(os.Stderr, "[helm-repo-sync] skip %s: url %q not in sourceRepos\n", name, url)
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

// chartDepRepoRe matches a helm dependency repository URL in Chart.yaml, e.g.:
//   - name: cert-manager
//     repository: https://charts.jetstack.io
//   - repository: https://charts.jetstack.io
//
// It captures the value (URL or alias like "@myrepo"). We match any indented
// "repository:" line inside the dependencies block rather than tying it to a
// preceding "- name:", which is brittle across the formatting variants helm
// and chart authors use. Local/alias references (starting with "@", "file://",
// or empty) are ignored — they point at a repo already defined elsewhere.
var chartDepRepoRe = regexp.MustCompile(`(?m)^[ \t]+(?:-[ \t]*)?repository:[ \t]*(.+?)[ \t]*$`)

// validateChartDeps reads <chartDir>/Chart.yaml and checks that every
// dependency's repository URL is permitted by the AppProject sourceRepos
// allowlist. This closes the public-repo bypass: a chart pulling from an
// unapproved helm repository fails fast instead of fetching it at dep build.
//
// Parsing is intentionally lightweight (regexp over lines) to keep the binary
// stdlib-only — Chart.yaml dependency blocks are simple enough that a full YAML
// parser adds no value here. Only top-level Chart.yaml dependencies are
// validated; transitive subchart-of-subchart dependencies are not visible and
// are a documented limitation.
func validateChartDeps(chartDir string, allowed []string) error {
	chartFile := filepath.Join(chartDir, "Chart.yaml")
	data, err := os.ReadFile(chartFile)
	if err != nil {
		// No Chart.yaml → nothing to validate (the caller only runs dep build
		// when Chart.yaml exists, so this is a defensive no-op).
		return nil
	}
	// Only inspect the dependencies: section. Cut everything before it so the
	// regexp doesn't match "repository:" keys elsewhere in the file.
	content := string(data)
	// Find the top-level "dependencies:" key (a line with no indentation).
	var block string
	lines := strings.Split(content, "\n")
	startIdx := -1
	for i, line := range lines {
		if line == "dependencies:" {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return nil // no dependencies block
	}
	// Collect lines until the next top-level key (no indentation, not a list
	// item) — that ends the dependencies list.
	for _, line := range lines[startIdx:] {
		if line == "dependencies:" {
			block += line + "\n"
			continue
		}
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' && line[0] != '-' {
			break // next top-level key reached
		}
		block += line + "\n"
	}

	var denied []string
	for _, m := range chartDepRepoRe.FindAllStringSubmatch(block, -1) {
		repo := strings.TrimSpace(m[1])
		// Strip surrounding quotes so quoted aliases ("@myrepo") are handled.
		repo = strings.Trim(repo, `"'`)
		// Skip local/alias references: "@alias", "file://./charts/x", empty.
		// They point at a repo declared elsewhere in the chart, not a remote URL.
		if repo == "" || strings.HasPrefix(repo, "@") || strings.HasPrefix(repo, "file://") {
			continue
		}
		if !isAllowed(repo, allowed) {
			denied = append(denied, repo)
		}
	}
	if len(denied) > 0 {
		return fmt.Errorf("Chart.yaml dependencies reference repos not in sourceRepos: %s", strings.Join(denied, ", "))
	}
	return nil
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
