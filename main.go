package main

import (
	"bytes"
	"crypto/md5"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"flag"

	"gopkg.in/yaml.v3"
)

//go:embed templates/*.yaml
var embeddedFS embed.FS

var (
	repoRoot   string
	cacheDir   string
	helmHome   string
	chartsDir  string
	configFile string
	debugMode  bool
	appVersion = "dev"
	stageAppsDir    = "apps"
	renderedAppsDir = "apps"

	// Default Application settings (overridable per-app via app.yaml)
	defaultFinalizers = []string{"resources-finalizer.argocd.argoproj.io"}
	defaultPrune      = true
	defaultSelfHeal   = true
	defaultSyncOptions = []string{"ServerSideApply=true", "RespectIgnoreDifferences=true"}

	kubernetesResourcesChart = "kubernetes-resources"
)

type encryptedEntry struct {
	ciphertext string
	data       map[string]interface{}
}

func init() {
	cwd, _ := os.Getwd()
	repoRoot = cwd
	cacheDir = filepath.Join(repoRoot, ".render-cache")
	helmHome = filepath.Join(cacheDir, "helm")
	chartsDir = filepath.Join(repoRoot, "charts")
	configFile = filepath.Join(repoRoot, "projects", "root-project.yaml")
}

var cachedConfig map[string]interface{}

func getConfig() map[string]interface{} {
	if cachedConfig != nil {
		return cachedConfig
	}
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		cachedConfig = make(map[string]interface{})
		return cachedConfig
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		cachedConfig = make(map[string]interface{})
		return cachedConfig
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		cachedConfig = make(map[string]interface{})
		return cachedConfig
	}
	if m == nil {
		m = make(map[string]interface{})
	}
	cachedConfig = m
	return cachedConfig
}

func getCfgString(keys ...string) string {
	m := getConfig()
	for i, k := range keys {
		v, ok := m[k]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			s, _ := v.(string)
			return s
		}
		m, ok = v.(map[string]interface{})
		if !ok {
			return ""
		}
	}
	return ""
}

func validateConfig(stageMeta map[string]interface{}) {
	if _, ok := stageMeta["projectNamespace"]; !ok {
		return
	}
	missing := []string{}
	for _, key := range []struct {
		keys    []string
		display string
	}{
		{[]string{"argocd", "root-project"}, "argocd.root-project"},
		{[]string{"argocd", "root-repo-url"}, "argocd.root-repo-url"},
	} {
		if getCfgString(key.keys...) == "" {
			missing = append(missing, key.display)
		}
	}

	if len(missing) > 0 {
		if _, err := os.Stat(configFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "ERROR: %s not found\n", configFile)
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: %s missing required fields:\n", configFile)
			for _, k := range missing {
				fmt.Fprintf(os.Stderr, "  - %s\n", k)
			}
		}
		fmt.Fprintf(os.Stderr, "\nRequired config example:\n")
		fmt.Fprintf(os.Stderr, "  argocd:\n")
		fmt.Fprintf(os.Stderr, "    root-namespace: argocd-system\n")
		fmt.Fprintf(os.Stderr, "    root-project: default\n")
		fmt.Fprintf(os.Stderr, "    root-repo-url: https://git.example.com/org/gitops.git\n")
		os.Exit(1)
	}
}

func helmEnv() []string {
	env := os.Environ()
	set := func(key, val string) {
		env = append(env, key+"="+val)
	}
	set("HELM_CONFIG_HOME", filepath.Join(helmHome, "config"))
	set("HELM_CACHE_HOME", filepath.Join(helmHome, "cache"))
	set("HELM_DATA_HOME", filepath.Join(helmHome, "data"))
	return env
}

func deepMerge(base, override map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		if existing, ok := result[k]; ok {
			if em, ok := existing.(map[string]interface{}); ok {
				if vm, ok := v.(map[string]interface{}); ok {
					result[k] = deepMerge(em, vm)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}

func loadYAML(path string) map[string]interface{} {
	data, err := os.ReadFile(path)
	if err != nil {
		return make(map[string]interface{})
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return make(map[string]interface{})
	}
	if m == nil {
		return make(map[string]interface{})
	}
	return m
}

func writeYAML(path string, data interface{}) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	out, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// --- Template rendering ---

func renderTemplate(templateName string, vars map[string]string) (map[string]interface{}, error) {
	data, err := embeddedFS.ReadFile("templates/" + templateName)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", templateName, err)
	}
	s := string(data)
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	var result map[string]interface{}
	if err := yaml.Unmarshal([]byte(s), &result); err != nil {
		return nil, fmt.Errorf("parse template %s: %w\n%s", templateName, err, s)
	}
	return result, nil
}

// --- SOPS ---

func decryptSOPS(filePath string) (map[string]interface{}, error) {
	cmd := exec.Command("sops", "-d", filePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sops -d %s: %s", filePath, strings.TrimSpace(string(out)))
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("parse decrypted %s: %w", filePath, err)
	}
	if m == nil {
		return make(map[string]interface{}), nil
	}
	return m, nil
}

func decryptContent(ciphertext string) (string, error) {
	tmp, err := os.CreateTemp("", "sops-dec-*.yaml")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(ciphertext); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	cmd := exec.Command("sops", "-d", "--input-type", "yaml", "--output-type", "yaml", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func isSOPSEncrypted(filePath string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := newLineScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "sops:") {
			return true
		}
	}
	return false
}

func saveEncryptedFiles(outputDir string, kinds []string) map[string]encryptedEntry {
	saved := make(map[string]encryptedEntry)
	for _, kind := range kinds {
		kindDir := filepath.Join(outputDir, kind)
		entries, err := os.ReadDir(kindDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml") {
				continue
			}
			fp := filepath.Join(kindDir, e.Name())
			if !isSOPSEncrypted(fp) {
				continue
			}
			ciphertext, err := os.ReadFile(fp)
			if err != nil {
				continue
			}
			plain, err := decryptContent(string(ciphertext))
			if err != nil {
				continue
			}
			var data map[string]interface{}
			if err := yaml.Unmarshal([]byte(plain), &data); err != nil {
				continue
			}
			saved[e.Name()] = encryptedEntry{ciphertext: string(ciphertext), data: data}
		}
	}
	return saved
}

func encryptSOPSSecrets(outputDir string, kinds []string, saved map[string]encryptedEntry) {
	info, err := os.Stat(outputDir)
	if err != nil || !info.IsDir() {
		return
	}
	for _, kind := range kinds {
		kindDir := filepath.Join(outputDir, kind)
		entries, err := os.ReadDir(kindDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml") {
				continue
			}
			fp := filepath.Join(kindDir, e.Name())
			newPlain, err := os.ReadFile(fp)
			if err != nil {
				continue
			}
			if saved != nil {
				if entry, ok := saved[e.Name()]; ok {
					var newData map[string]interface{}
					if err := yaml.Unmarshal(newPlain, &newData); err == nil && deepEqualMap(newData, entry.data) {
						os.WriteFile(fp, []byte(entry.ciphertext), 0644)
						continue
					}
				}
			}
			cmd := exec.Command("sops", "-e", "--input-type", "yaml", "--output-type", "yaml", fp)
			out, err := cmd.CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  WARNING sops encrypt %s: %s\n", e.Name(), strings.TrimSpace(string(out)))
				continue
			}
			os.WriteFile(fp, out, 0644)
		}
	}
}

func deepEqualMap(a, b map[string]interface{}) bool {
	aj, _ := yaml.Marshal(a)
	bj, _ := yaml.Marshal(b)
	return string(aj) == string(bj)
}

// --- Discovery ---

func discoverStages(stageFilter string) []string {
	projectsDir := filepath.Join(repoRoot, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	var stages []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mainFile := filepath.Join(projectsDir, e.Name(), "main.yaml")
		if _, err := os.Stat(mainFile); os.IsNotExist(err) {
			continue
		}
		if stageFilter != "" && e.Name() != stageFilter {
			continue
		}
		stages = append(stages, filepath.Join(projectsDir, e.Name()))
	}
	return stages
}

func discoverApps(stageDir, appFilter string) []string {
	appsDir := filepath.Join(stageDir, stageAppsDir)
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil
	}
	var apps []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		appFile := filepath.Join(appsDir, e.Name(), "app.yaml")
		if _, err := os.Stat(appFile); os.IsNotExist(err) {
			continue
		}
		if appFilter != "" && e.Name() != appFilter {
			continue
		}
		apps = append(apps, filepath.Join(appsDir, e.Name()))
	}
	return apps
}

func discoverNamespaceNames(stageDir string) []string {
	nsDir := filepath.Join(stageDir, "namespaces")
	files := discoverInfraFiles(nsDir)
	seen := make(map[string]bool)
	var names []string
	for _, f := range files {
		data := loadYAML(f)
		name := ""
		if ns, ok := data["namespace"].(map[string]interface{}); ok {
			if n, ok := ns["name"].(string); ok {
				name = n
			}
		}
		if name == "" {
			if n, ok := data["name"].(string); ok {
				name = n
			}
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// --- Repos ---

func extractRepos(stageMeta map[string]interface{}) []map[string]interface{} {
	var repos []map[string]interface{}
	sources, _ := stageMeta["sourceRepos"].([]interface{})
	for _, s := range sources {
		entry, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := entry["url"]; ok {
			repos = append(repos, entry)
		}
	}
	repoURL, _ := stageMeta["repoUrl"].(string)
	rootRepoURL := getCfgString("argocd", "root-repo-url")
	if repoURL != "" && rootRepoURL != "" && repoURL != rootRepoURL {
		branch, _ := stageMeta["branch"].(string)
		if branch == "" {
			branch = "master"
		}
		repos = append(repos, map[string]interface{}{
			"url":    repoURL,
			"branch": branch,
			"path":   "rendered/argocd/applications",
		})
	}
	return repos
}

// --- Infrastructure ---

func fileStem(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

func discoverInfraFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files
}

func discoverInfraFilesRecursive(dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func renderProject(stageDir, stageName string, stageMeta map[string]interface{}, argocdAppsDir string) bool {
	chartDir := filepath.Join(chartsDir, kubernetesResourcesChart)
	if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); os.IsNotExist(err) {
		return false
	}

	// Only render project if main.yaml has project definition fields
	if stageMeta["namespaceResourceWhitelist"] == nil && stageMeta["sourceRepos"] == nil {
		return false
	}

	ns := getCfgString("argocd", "root-namespace")
	if ns == "" {
		ns = "argocd-system"
	}
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}

	// Build destinations and sourceNamespaces from namespace files
	nsNames := discoverNamespaceNames(stageDir)
	var destinations []interface{}
	var sourceNS []interface{}
	for _, n := range nsNames {
		destinations = append(destinations, map[string]interface{}{
			"namespace": n,
			"server":    server,
		})
		sourceNS = append(sourceNS, n)
	}

	repoURL, _ := stageMeta["repoUrl"].(string)
	projValues := map[string]interface{}{
		"projectName":                stageName,
		"projectNamespace":           ns,
		"description":                stageMeta["description"],
		"sourceRepos":                []interface{}{repoURL},
		"destinations":               destinations,
		"clusterResourceWhitelist":   []interface{}{},
		"namespaceResourceWhitelist": stageMeta["namespaceResourceWhitelist"],
		"sourceNamespaces":           sourceNS,
	}

	projOutDir := filepath.Join(repoRoot, "rendered", "argocd", "projects")
	os.RemoveAll(projOutDir)
	if _, err := helmTemplateToDir(chartDir, "project", ns, projValues, projOutDir); err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR render project: %v\n", err)
		return false
	}

	// helmTemplateToDir creates subdirs by kind — move file to projects/<stage>.yaml
	projFile := ""
	filepath.WalkDir(projOutDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml") {
			projFile = path
		}
		return nil
	})
	if projFile != "" {
		targetPath := filepath.Join(projOutDir, stageName+".yaml")
		data, _ := os.ReadFile(projFile)
		os.RemoveAll(projOutDir)
		os.MkdirAll(projOutDir, 0755)
		os.WriteFile(targetPath, data, 0644)
	}

	// Generate Application CR — directory source, root-repo-url
	hubRepoURL := getCfgString("argocd", "root-repo-url")
	if hubRepoURL == "" {
		hubRepoURL = repoURL
	}
	rootProject := getCfgString("argocd", "root-project")
	if rootProject == "" {
		rootProject = ""
	}

	app, _ := renderTemplate("application.yaml", map[string]string{
		"name":      stageName + "-project",
		"sync_wave": "-10",
		"stage":     stageName,
		"app_name":  "project",
		"project":   rootProject,
		"repo_url":  hubRepoURL,
		"branch":    "master",
		"path":      "rendered/argocd/projects",
		"server":    server,
		"namespace": ns,
	})
	if app != nil {
		applyAppSettings(app, map[string]interface{}{
			"application": map[string]interface{}{
				"prune": false, "selfHeal": true,
				"syncOptions": []interface{}{"ServerSideApply=true"},
				"finalizers":  []interface{}{},
			},
		})
		writeYAML(filepath.Join(argocdAppsDir, stageName+"-project.yaml"), app)
	}
	fmt.Printf("  Rendered project (%d destinations)\n", len(destinations))
	return true
}

func renderInfraFullRender(stageDir, outputBase, stageName string, stageMeta map[string]interface{}, cliOverrides map[string]interface{}) map[string]bool {
	chartDir := filepath.Join(chartsDir, kubernetesResourcesChart)
	if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); os.IsNotExist(err) {
		return nil
	}

	active := make(map[string]bool)
	repoURL, _ := stageMeta["repoUrl"].(string)
	branch, _ := stageMeta["branch"].(string)
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	stageProject, _ := stageMeta["project"].(string)
	if stageProject == "" {
		stageProject = stageName
	}
	rootProject := getCfgString("argocd", "root-project")
	if rootProject == "" {
		rootProject = ""
	}
	hubRepoURL := getCfgString("argocd", "root-repo-url")
	if hubRepoURL == "" {
		hubRepoURL = repoURL
	}

	// Project rendered by renderProject() in renderStage — skipped here

	// 2. Namespaces (syncWave: 0, per file)
	nsDir := filepath.Join(stageDir, "namespaces")
	nsFiles := discoverInfraFiles(nsDir)
	for _, f := range nsFiles {
		values := loadYAML(f)
		if len(values) == 0 {
			continue
		}
		nsName := "default"
		if ns, ok := values["namespace"].(map[string]interface{}); ok {
			if n, ok := ns["name"].(string); ok {
				nsName = n
			}
		}
		outDir := filepath.Join(outputBase, "namespaces", nsName)
		os.RemoveAll(outDir)
		if _, err := helmTemplateToDir(chartDir, nsName, nsName, values, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR render namespace %s: %v\n", nsName, err)
			continue
		}
		appName := stageName + "-namespace-" + nsName
		active["namespace-"+nsName] = true
		app, _ := renderTemplate("application.yaml", map[string]string{
			"name":      appName,
			"sync_wave": "0",
			"stage":     stageName,
			"app_name":  "namespace-" + nsName,
			"project":   rootProject,
			"repo_url":  hubRepoURL,
			"branch":    branch,
			"path":      "rendered/" + stageName + "/namespaces/" + nsName,
			"server":    server,
			"namespace": nsName,
		})
		if app != nil {
			applyAppSettings(app, nil)
			argocdAppsDir := filepath.Join(repoRoot, "rendered", "argocd", "applications")
			writeYAML(filepath.Join(argocdAppsDir, appName+".yaml"), app)
		}
		fmt.Printf("  Rendered infra: namespace/%s\n", nsName)
	}

	// Cleanup stale namespace dirs
	nsOutPath := filepath.Join(outputBase, "namespaces")
	if entries, err := os.ReadDir(nsOutPath); err == nil {
		for _, e := range entries {
			if e.IsDir() && !active["namespace-"+e.Name()] {
				os.RemoveAll(filepath.Join(nsOutPath, e.Name()))
				fmt.Printf("  Removed: namespaces/%s\n", e.Name())
			}
		}
	}

	// 3. RBAC (syncWave: 1, aggregated)
	rbacDir := filepath.Join(stageDir, "rbac")
	rbacFiles := discoverInfraFilesRecursive(rbacDir)
	if len(rbacFiles) > 0 {
		rbacValues := make(map[string]interface{})
		for _, f := range rbacFiles {
			data := loadYAML(f)
			rbacValues = deepMerge(rbacValues, data)
		}
		if len(rbacValues) > 0 {
			outDir := filepath.Join(outputBase, "rbac")
			os.RemoveAll(outDir)
			if _, err := helmTemplateToDir(chartDir, "rbac", "default", rbacValues, outDir); err != nil {
				fmt.Fprintf(os.Stderr, "  ERROR render rbac: %v\n", err)
			} else {
				appName := stageName + "-rbac"
				active["rbac"] = true
				app, _ := renderTemplate("application.yaml", map[string]string{
					"name":      appName,
					"sync_wave": "1",
					"stage":     stageName,
					"app_name":  "rbac",
					"project":   rootProject,
					"repo_url":  hubRepoURL,
					"branch":    branch,
					"path":      "rendered/" + stageName + "/rbac",
					"server":    server,
					"namespace": "default",
				})
				if app != nil {
					applyAppSettings(app, nil)
					argocdAppsDir := filepath.Join(repoRoot, "rendered", "argocd", "applications")
					writeYAML(filepath.Join(argocdAppsDir, appName+".yaml"), app)
				}
				fmt.Printf("  Rendered infra: rbac (%d files)\n", len(rbacFiles))
			}
		}
	}

	// 4. NetworkPolicy (syncWave: 2, aggregated)
	npDir := filepath.Join(stageDir, "networkpolicy")
	npFiles := discoverInfraFiles(npDir)
	if len(npFiles) > 0 {
		npValues := make(map[string]interface{})
		for _, f := range npFiles {
			data := loadYAML(f)
			npValues = deepMerge(npValues, data)
		}
		if len(npValues) > 0 {
			outDir := filepath.Join(outputBase, "networkpolicy")
			os.RemoveAll(outDir)
			if _, err := helmTemplateToDir(chartDir, "networkpolicy", "default", npValues, outDir); err != nil {
				fmt.Fprintf(os.Stderr, "  ERROR render networkpolicy: %v\n", err)
			} else {
				appName := stageName + "-networkpolicy"
				active["networkpolicy"] = true
				app, _ := renderTemplate("application.yaml", map[string]string{
					"name":      appName,
					"sync_wave": "2",
					"stage":     stageName,
					"app_name":  "networkpolicy",
					"project":   rootProject,
					"repo_url":  hubRepoURL,
					"branch":    branch,
					"path":      "rendered/" + stageName + "/networkpolicy",
					"server":    server,
					"namespace": "default",
				})
				if app != nil {
					applyAppSettings(app, nil)
					argocdAppsDir := filepath.Join(repoRoot, "rendered", "argocd", "applications")
					writeYAML(filepath.Join(argocdAppsDir, appName+".yaml"), app)
				}
				fmt.Printf("  Rendered infra: networkpolicy (%d files)\n", len(npFiles))
			}
		}
	}

	return active
}

func renderInfraDefaultMode(stageDir, stageName string, stageMeta map[string]interface{}) map[string]bool {
	chartDir := filepath.Join(chartsDir, kubernetesResourcesChart)
	if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); os.IsNotExist(err) {
		return nil
	}

	active := make(map[string]bool)
	repoURL, _ := stageMeta["repoUrl"].(string)
	branch, _ := stageMeta["branch"].(string)
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	rootProject := getCfgString("argocd", "root-project")
	if rootProject == "" {
		rootProject = ""
	}
	hubRepoURL := getCfgString("argocd", "root-repo-url")
	if hubRepoURL == "" {
		hubRepoURL = repoURL
	}
	argocdAppsDir := filepath.Join(repoRoot, "rendered", "argocd", "applications")

	// Project rendered by renderProject() in renderStage — skipped here

	// 2. Namespaces (per file)
	nsDir := filepath.Join(stageDir, "namespaces")
	nsFiles := discoverInfraFiles(nsDir)
	for _, f := range nsFiles {
		data := loadYAML(f)
		if len(data) == 0 {
			continue
		}
		nsName := "default"
		if ns, ok := data["namespace"].(map[string]interface{}); ok {
			if n, ok := ns["name"].(string); ok {
				nsName = n
			}
		}
		appName := stageName + "-namespace-" + nsName
		active["namespace-"+nsName] = true
		relPath, _ := filepath.Rel(filepath.Join(chartsDir, kubernetesResourcesChart), f)
		app, _ := renderTemplate("application-helm.yaml", map[string]string{
			"name":         appName,
			"sync_wave":    "0",
			"stage":        stageName,
			"app_name":     "namespace-" + nsName,
			"project":      rootProject,
			"repo_url":     hubRepoURL,
			"branch":       branch,
			"chart_path":   "charts/" + kubernetesResourcesChart,
			"values_path":  relPath,
			"release_name": nsName,
			"server":       server,
			"namespace":    nsName,
		})
		if app != nil {
			applyAppSettings(app, nil)
			writeYAML(filepath.Join(argocdAppsDir, appName+".yaml"), app)
		}
		fmt.Printf("  Application: namespace/%s (helm)\n", nsName)
	}

	// 3. RBAC (aggregated valueFiles)
	rbacDir := filepath.Join(stageDir, "rbac")
	rbacFiles := discoverInfraFilesRecursive(rbacDir)
	if len(rbacFiles) > 0 {
		appName := stageName + "-rbac"
		active["rbac"] = true

		var relPaths []string
		for _, f := range rbacFiles {
			rel, _ := filepath.Rel(filepath.Join(chartsDir, kubernetesResourcesChart), f)
			relPaths = append(relPaths, rel)
		}

		app, _ := renderTemplate("application-helm.yaml", map[string]string{
			"name":         appName,
			"sync_wave":    "1",
			"stage":        stageName,
			"app_name":     "rbac",
			"project":      rootProject,
			"repo_url":     hubRepoURL,
			"branch":       branch,
			"chart_path":   "charts/" + kubernetesResourcesChart,
			"values_path":  relPaths[0],
			"release_name": "rbac",
			"server":       server,
			"namespace":    "default",
		})
		if app != nil && len(relPaths) > 1 {
			spec, _ := app["spec"].(map[string]interface{})
			if spec != nil {
				source, _ := spec["source"].(map[string]interface{})
				if source != nil {
					helm, _ := source["helm"].(map[string]interface{})
					if helm != nil {
						helm["valueFiles"] = relPaths
					}
				}
			}
		}
		if app != nil {
			applyAppSettings(app, nil)
			writeYAML(filepath.Join(argocdAppsDir, appName+".yaml"), app)
		}
		fmt.Printf("  Application: rbac (helm, %d files)\n", len(rbacFiles))
	}

	// 4. NetworkPolicy (aggregated valueFiles)
	npDir := filepath.Join(stageDir, "networkpolicy")
	npFiles := discoverInfraFiles(npDir)
	if len(npFiles) > 0 {
		appName := stageName + "-networkpolicy"
		active["networkpolicy"] = true

		var relPaths []string
		for _, f := range npFiles {
			rel, _ := filepath.Rel(filepath.Join(chartsDir, kubernetesResourcesChart), f)
			relPaths = append(relPaths, rel)
		}

		app, _ := renderTemplate("application-helm.yaml", map[string]string{
			"name":         appName,
			"sync_wave":    "2",
			"stage":        stageName,
			"app_name":     "networkpolicy",
			"project":      rootProject,
			"repo_url":     hubRepoURL,
			"branch":       branch,
			"chart_path":   "charts/" + kubernetesResourcesChart,
			"values_path":  relPaths[0],
			"release_name": "networkpolicy",
			"server":       server,
			"namespace":    "default",
		})
		if app != nil && len(relPaths) > 1 {
			spec, _ := app["spec"].(map[string]interface{})
			if spec != nil {
				source, _ := spec["source"].(map[string]interface{})
				if source != nil {
					helm, _ := source["helm"].(map[string]interface{})
					if helm != nil {
						helm["valueFiles"] = relPaths
					}
				}
			}
		}
		if app != nil {
			applyAppSettings(app, nil)
			writeYAML(filepath.Join(argocdAppsDir, appName+".yaml"), app)
		}
		fmt.Printf("  Application: networkpolicy (helm, %d files)\n", len(npFiles))
	}

	return active
}

// --- Values ---

func readChartDefaults(chartDir string) map[string]interface{} {
	values := make(map[string]interface{})
	for _, ext := range []string{".yaml", ".yml"} {
		fp := filepath.Join(chartDir, "values"+ext)
		if _, err := os.Stat(fp); os.IsNotExist(err) {
			continue
		}
		values = deepMerge(values, loadYAML(fp))
	}
	return values
}

func mergeValues(chartDir, envValuesFile string) map[string]interface{} {
	values := readChartDefaults(chartDir)
	if envValuesFile != "" {
		if _, err := os.Stat(envValuesFile); err == nil {
			values = deepMerge(values, loadYAML(envValuesFile))
		}
	}
	return values
}

// --- Helm dependency management ---

func depHash(chartDir string) string {
	h := md5.New()
	for _, name := range []string{"Chart.yaml", "requirements.yaml"} {
		data, err := os.ReadFile(filepath.Join(chartDir, name))
		if err != nil {
			continue
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func helmDepBuild(chartDir string) bool {
	hashFile := filepath.Join(chartDir, ".dep.md5")
	currentHash := depHash(chartDir)
	if data, err := os.ReadFile(hashFile); err == nil && strings.TrimSpace(string(data)) == currentHash {
		fmt.Printf("  Up to date: %s\n", filepath.Base(chartDir))
		return true
	}
	fmt.Printf("  Building: %s\n", filepath.Base(chartDir))
	os.Remove(filepath.Join(chartDir, "Chart.lock"))
	chartsSub := filepath.Join(chartDir, "charts")
	if entries, err := os.ReadDir(chartsSub); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tgz") {
				os.Remove(filepath.Join(chartsSub, e.Name()))
			}
		}
	}
	args := []string{"dep", "build", chartDir}
	if debugMode {
		args = append(args, "--debug")
	}
	cmd := exec.Command("helm", args...)
	cmd.Env = helmEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR helm dep build %s: %s\n", filepath.Base(chartDir), strings.TrimSpace(string(out)))
		return false
	}
	os.WriteFile(hashFile, []byte(currentHash), 0644)
	return true
}

func collectChartDirs(appFilter string) []string {
	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(chartsDir, e.Name(), "Chart.yaml")); os.IsNotExist(err) {
			continue
		}
		if appFilter != "" && e.Name() != appFilter {
			continue
		}
		dirs = append(dirs, filepath.Join(chartsDir, e.Name()))
	}
	return dirs
}

func prepareDeps(chartDirs []string) bool {
	fmt.Printf("  Building dependencies: %d charts\n", len(chartDirs))
	maxW := len(chartDirs)
	if maxW > 4 {
		maxW = 4
	}
	sem := make(chan struct{}, maxW)
	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	for _, d := range chartDirs {
		d := d
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if !helmDepBuild(d) {
				mu.Lock()
				failed = append(failed, filepath.Base(d))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "  FAILED: %s\n", strings.Join(failed, ", "))
		return false
	}
	return true
}

// --- Rendering ---

func helmTemplateToDir(chartDir, releaseName, namespace string, valuesData map[string]interface{}, outputDir string) ([]string, error) {
	tmp, err := os.CreateTemp("", "values-*.yaml")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	valBytes, _ := yaml.Marshal(valuesData)
	tmp.Write(valBytes)
	tmp.Close()

	args := []string{"template", releaseName, chartDir, "-f", tmpPath, "--namespace", namespace, "--include-crds"}
	if debugMode {
		args = append(args, "--debug")
	}
	cmd := exec.Command("helm", args...)
	cmd.Env = helmEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("helm template %s: %s", releaseName, strings.TrimSpace(string(out)))
	}

	os.MkdirAll(outputDir, 0755)
	var written []string
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]interface{}
		if err := decoder.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if doc == nil {
			continue
		}
		kind, _ := doc["kind"].(string)
		if kind == "" {
			kind = "Unknown"
		}
		name := "unnamed"
		if meta, ok := doc["metadata"].(map[string]interface{}); ok {
			if n, ok := meta["name"].(string); ok {
				name = n
			}
		}
		kindDir := filepath.Join(outputDir, strings.ToLower(kind))
		os.MkdirAll(kindDir, 0755)
		outFile := filepath.Join(kindDir, name+".yaml")
		if err := writeYAML(outFile, doc); err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR writing %s: %v\n", outFile, err)
			continue
		}
		written = append(written, outFile)
	}
	return written, nil
}

func renderApp(stageDir, appDir, outputBase, stageProject string, cliOverrides map[string]interface{}) (string, string, map[string]interface{}) {
	appMeta := loadYAML(filepath.Join(appDir, "app.yaml"))
	instanceName := filepath.Base(appDir)
	chartName, _ := appMeta["chartName"].(string)
	namespace, _ := appMeta["namespace"].(string)
	project, _ := appMeta["project"].(string)
	if project == "" {
		project = stageProject
	}
	ignoreDiffs := appMeta["ignoreDifferences"]
	encryptKinds, _ := appMeta["encryptKinds"]
	syncWave, _ := appMeta["syncWave"].(string)
	if syncWave == "" {
		syncWave = "10"
	}
	stageName := filepath.Base(stageDir)

	chartDir := filepath.Join(chartsDir, chartName)
	if _, err := os.Stat(chartDir); os.IsNotExist(err) {
		return "skip", instanceName, nil
	}

	appOutput := filepath.Join(outputBase, renderedAppsDir, instanceName)
	envValues := filepath.Join(appDir, "values.yaml")
	secretsFile := filepath.Join(appDir, "secrets.yaml")
	hasSOPS := false
	if _, err := os.Stat(secretsFile); err == nil {
		hasSOPS = true
	}

	values := mergeValues(chartDir, envValues)

	if hasSOPS {
		secrets, err := decryptSOPS(secretsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR decrypt %s: %v\n", secretsFile, err)
			return "error", instanceName, nil
		}
		values = deepMerge(values, secrets)
	}

	if len(cliOverrides) > 0 {
		values = deepMerge(values, cliOverrides)
	}

	var encryptKindsList []string
	if hasSOPS {
		if ekSlice, ok := encryptKinds.([]interface{}); ok {
			for _, v := range ekSlice {
				if s, ok := v.(string); ok {
					encryptKindsList = append(encryptKindsList, strings.ToLower(s))
				}
			}
		}
		if len(encryptKindsList) == 0 {
			encryptKindsList = []string{"secret"}
		}
	}

	var saved map[string]encryptedEntry
	if len(encryptKindsList) > 0 {
		saved = saveEncryptedFiles(appOutput, encryptKindsList)
	}

	os.RemoveAll(appOutput)

	renderedFiles, err := helmTemplateToDir(chartDir, instanceName, namespace, values, appOutput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR render %s: %v\n", instanceName, err)
		return "error", instanceName, nil
	}
	_ = renderedFiles

	if hasSOPS {
		encryptSOPSSecrets(appOutput, encryptKindsList, saved)
	}

	return "rendered", instanceName, map[string]interface{}{
		"instanceName":     instanceName,
		"chartName":        chartName,
		"namespace":        namespace,
		"project":          project,
		"stage":            stageName,
		"ignoreDifferences": ignoreDiffs,
		"hasSops":          hasSOPS,
		"syncWave":         syncWave,
		"application":      appMeta["application"],
	}
}

// --- Application generation ---

func generateAppApplication(appMeta map[string]interface{}, stageMeta map[string]interface{}, repoURL, branch string) map[string]interface{} {
	stage, _ := appMeta["stage"].(string)
	instanceName, _ := appMeta["instanceName"].(string)
	namespace, _ := appMeta["namespace"].(string)
	project, _ := appMeta["project"].(string)
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	syncWave, _ := appMeta["syncWave"].(string)
	if syncWave == "" {
		syncWave = "10"
	}

	app, err := renderTemplate("application.yaml", map[string]string{
		"name":      stage + "-" + instanceName,
		"sync_wave": syncWave,
		"stage":     stage,
		"app_name":  instanceName,
		"project":   project,
		"repo_url":  repoURL,
		"branch":    branch,
		"path":      "rendered/" + stage + "/" + renderedAppsDir + "/" + instanceName,
		"server":    server,
		"namespace": namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR generate app application: %v\n", err)
		return nil
	}

	if hasSops, _ := appMeta["hasSops"].(bool); hasSops {
		removeNestedKey(app, "spec", "source", "directory")
		setNestedKey(app, map[string]interface{}{"name": "sops"}, "spec", "source", "plugin")
	}

	if ignoreDiffs, ok := appMeta["ignoreDifferences"]; ok && ignoreDiffs != nil {
		spec, _ := app["spec"].(map[string]interface{})
		if spec != nil {
			spec["ignoreDifferences"] = ignoreDiffs
		}
	}


		applyAppSettings(app, appMeta)

	return app
}

func generateAppApplicationHelm(appMeta map[string]interface{}, stageMeta map[string]interface{}, repoURL, branch string) map[string]interface{} {
	stage, _ := appMeta["stage"].(string)
	instanceName, _ := appMeta["instanceName"].(string)
	chartName, _ := appMeta["chartName"].(string)
	namespace, _ := appMeta["namespace"].(string)
	project, _ := appMeta["project"].(string)
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	syncWave, _ := appMeta["syncWave"].(string)
	if syncWave == "" {
		syncWave = "10"
	}

	app, err := renderTemplate("application-helm.yaml", map[string]string{
		"name":         stage + "-" + instanceName,
		"sync_wave":    syncWave,
		"stage":        stage,
		"app_name":     instanceName,
		"project":      project,
		"repo_url":     repoURL,
		"branch":       branch,
		"chart_path":   "charts/" + chartName,
		"values_path":  "../../projects/" + stage + "/apps/" + instanceName + "/values.yaml",
		"release_name": instanceName,
		"server":       server,
		"namespace":    namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR generate helm application: %v\n", err)
		return nil
	}

	if ignoreDiffs, ok := appMeta["ignoreDifferences"]; ok && ignoreDiffs != nil {
		spec, _ := app["spec"].(map[string]interface{})
		if spec != nil {
			spec["ignoreDifferences"] = ignoreDiffs
		}
	}


		applyAppSettings(app, appMeta)

	return app
}

func generateSOPSApplication(appMeta map[string]interface{}, stageMeta map[string]interface{}, repoURL, branch string) map[string]interface{} {
	stage, _ := appMeta["stage"].(string)
	instanceName, _ := appMeta["instanceName"].(string)
	namespace, _ := appMeta["namespace"].(string)
	project, _ := appMeta["project"].(string)
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	syncWave, _ := appMeta["syncWave"].(string)
	if syncWave == "" {
		syncWave = "10"
	}

	app, err := renderTemplate("application.yaml", map[string]string{
		"name":      stage + "-" + instanceName,
		"sync_wave": syncWave,
		"stage":     stage,
		"app_name":  instanceName,
		"project":   project,
		"repo_url":  repoURL,
		"branch":    branch,
		"path":      "projects/" + stage + "/apps/" + instanceName,
		"server":    server,
		"namespace": namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR generate sops application: %v\n", err)
		return nil
	}

	removeNestedKey(app, "spec", "source", "directory")
	setNestedKey(app, map[string]interface{}{"name": "sops"}, "spec", "source", "plugin")

	if ignoreDiffs, ok := appMeta["ignoreDifferences"]; ok && ignoreDiffs != nil {
		spec, _ := app["spec"].(map[string]interface{})
		if spec != nil {
			spec["ignoreDifferences"] = ignoreDiffs
		}
	}


		applyAppSettings(app, appMeta)

	return app
}

func generateRepoApplication(stageName string, stageMeta map[string]interface{}, repoConf map[string]interface{}, branch string) map[string]interface{} {
	repoName, _ := repoConf["name"].(string)
	repoURL, _ := repoConf["url"].(string)
	if repoName == "" {
		repoName = filepath.Base(repoURL)
		ext := filepath.Ext(repoName)
		if ext != "" {
			repoName = strings.TrimSuffix(repoName, ext)
		}
	}
	repoBranch, _ := repoConf["branch"].(string)
	if repoBranch == "" {
		repoBranch = branch
	}
	repoPath, _ := repoConf["path"].(string)
	if repoPath == "" {
		repoPath = "rendered/argocd/applications"
	}
	server, _ := stageMeta["server"].(string)
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	stageProject, _ := stageMeta["project"].(string)
	if stageProject == "" {
		stageProject = stageName
	}
	projectNS, _ := stageMeta["projectNamespace"].(string)
	if projectNS == "" {
		projectNS = stageName
	}

	app, err := renderTemplate("repo-application.yaml", map[string]string{
		"name":      repoName + "-bootstrap",
		"sync_wave": "5",
		"stage":     stageName,
		"repo_name": repoName,
		"project":   stageProject,
		"repo_url":  repoURL,
		"branch":    repoBranch,
		"path":      repoPath,
		"server":    server,
		"namespace":      projectNS,
		"metadata_namespace": projectNS,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR generate repo application: %v\n", err)
		return nil
	}
	applyAppSettings(app, map[string]interface{}{
		"application": map[string]interface{}{
			"prune":    false,
			"selfHeal": true,
			"syncOptions": []interface{}{"ServerSideApply=true"},
		},
	})
	return app
}

// --- Stage rendering ---

// applyAppSettings adds finalizers, syncPolicy, syncOptions to Application CR
// based on app.yaml "application" section with defaults.
func applyAppSettings(app map[string]interface{}, appMeta map[string]interface{}) {
	// Read "application" overrides from app.yaml
	appConf, _ := appMeta["application"].(map[string]interface{})

	// Finalizers
	finalizers := defaultFinalizers
	if f, ok := appConf["finalizers"].([]interface{}); ok {
		finalizers = toStringSlice(f)
	}
	if len(finalizers) > 0 {
		meta, _ := app["metadata"].(map[string]interface{})
		if meta != nil {
			meta["finalizers"] = finalizers
		}
	}

	// SyncPolicy
	prune := defaultPrune
	selfHeal := defaultSelfHeal
	syncOpts := defaultSyncOptions

	if v, ok := appConf["prune"].(bool); ok {
		prune = v
	}
	if v, ok := appConf["selfHeal"].(bool); ok {
		selfHeal = v
	}
	if v, ok := appConf["syncOptions"].([]interface{}); ok {
		syncOpts = toStringSlice(v)
	}

	spec, _ := app["spec"].(map[string]interface{})
	if spec != nil {
		spec["syncPolicy"] = map[string]interface{}{
			"automated": map[string]interface{}{
				"prune":    prune,
				"selfHeal": selfHeal,
			},
			"syncOptions": syncOpts,
		}
	}
}

func toStringSlice(v []interface{}) []string {
	result := make([]string, 0, len(v))
	for _, item := range v {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func renderStage(stageDir, appFilter string, fullRender bool, cliOverrides map[string]interface{}) {
	stageMeta := loadYAML(filepath.Join(stageDir, "main.yaml"))
	stageName := filepath.Base(stageDir)
	validateConfig(stageMeta)
	repoURL, _ := stageMeta["repoUrl"].(string)
	hubRepoURL := getCfgString("argocd", "root-repo-url")
	if hubRepoURL == "" {
		hubRepoURL = repoURL
	}
	branch, _ := stageMeta["branch"].(string)
	stageProject, _ := stageMeta["project"].(string)
	if stageProject == "" {
		stageProject = stageName
	}
	fmt.Printf("\nStage: %s\n", stageName)

	outputBase := filepath.Join(repoRoot, "rendered", stageName)
	argocdAppsDir := filepath.Join(repoRoot, "rendered", "argocd", "applications")

	appDirs := discoverApps(stageDir, appFilter)
	repoEntries := extractRepos(stageMeta)

	// Project — always rendered as static YAML (both modes)
	projectActive := renderProject(stageDir, stageName, stageMeta, argocdAppsDir)

	if fullRender {
		// FULL RENDER MODE
		// ... (project already rendered, add to activeApps below)
		type appResult struct {
			status string
			name   string
			meta   map[string]interface{}
		}
		var results []appResult

		if len(appDirs) > 0 {
			maxW := len(appDirs)
			if maxW > 4 {
				maxW = 4
			}
			sem := make(chan struct{}, maxW)
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, ad := range appDirs {
				ad := ad
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					status, name, meta := renderApp(stageDir, ad, outputBase, stageProject, cliOverrides)
					mu.Lock()
					results = append(results, appResult{status, name, meta})
					mu.Unlock()
				}()
			}
			wg.Wait()

			sort.Slice(results, func(i, j int) bool {
				return results[i].name < results[j].name
			})
			for _, r := range results {
				switch r.status {
				case "rendered":
					fmt.Printf("  Rendered app: %s\n", r.name)
				case "skip":
					fmt.Printf("  SKIP %s: chart not found\n", r.name)
				case "error":
					fmt.Fprintf(os.Stderr, "  ERROR rendering %s\n", r.name)
				}
			}
		} else {
			fmt.Println("  No apps found, skipping app rendering")
		}

		// Generate Application YAMLs (directory source)
		fmt.Println("  Generating Applications...")
		activeApps := make(map[string]bool)
		if projectActive {
			activeApps["project"] = true
		}
		for _, r := range results {
			if r.status == "rendered" && r.meta != nil {
				activeApps[r.name] = true
				appYAML := generateAppApplication(r.meta, stageMeta, repoURL, branch)
				if appYAML != nil {
					writeYAML(filepath.Join(argocdAppsDir, stageName+"-"+r.name+".yaml"), appYAML)
				}
			}
		}

		// Repos
		activeRepos := renderRepos(stageName, stageMeta, repoEntries, argocdAppsDir, branch)

		// Cleanup stale rendered app dirs
		renderedAppsPath := filepath.Join(outputBase, renderedAppsDir)
		if entries, err := os.ReadDir(renderedAppsPath); err == nil {
			for _, e := range entries {
				if e.IsDir() && !activeApps[e.Name()] {
					os.RemoveAll(filepath.Join(renderedAppsPath, e.Name()))
					fmt.Printf("  Removed: applications/%s\n", e.Name())
				}
			}
		}

		// Infrastructure (full-render mode)
		activeInfra := renderInfraFullRender(stageDir, outputBase, stageName, stageMeta, cliOverrides)
		for name := range activeInfra {
			activeApps[name] = true
		}

		// Cleanup stale Application YAMLs
		cleanupStaleApps(argocdAppsDir, stageName, activeApps, activeRepos)

	} else {
		// DEFAULT MODE (only Application CRs)
		if _, err := os.Stat(outputBase); err == nil {
			os.RemoveAll(outputBase)
			fmt.Printf("  Removed: rendered/%s/\n", stageName)
		}

		fmt.Println("  Generating Applications...")
		activeApps := make(map[string]bool)
		if projectActive {
			activeApps["project"] = true
		}

		for _, ad := range appDirs {
			appMetaFile := loadYAML(filepath.Join(ad, "app.yaml"))
			instanceName := filepath.Base(ad)
			secretsFile := filepath.Join(ad, "secrets.yaml")
			hasSOPS := false
			if _, err := os.Stat(secretsFile); err == nil {
				hasSOPS = true
			}
			chartName, _ := appMetaFile["chartName"].(string)
			namespace, _ := appMetaFile["namespace"].(string)
			project, _ := appMetaFile["project"].(string)
			if project == "" {
				project = stageProject
			}
			syncWave, _ := appMetaFile["syncWave"].(string)
			if syncWave == "" {
				syncWave = "10"
			}
			meta := map[string]interface{}{
				"instanceName": instanceName,
				"chartName":    chartName,
				"namespace":    namespace,
				"project":      project,
				"stage":        stageName,
				"ignoreDifferences": appMetaFile["ignoreDifferences"],
				"hasSops":      hasSOPS,
				"syncWave":     syncWave,
				"application":  appMetaFile["application"],
			}
			activeApps[instanceName] = true

			var appYAML map[string]interface{}
			if hasSOPS {
				appYAML = generateSOPSApplication(meta, stageMeta, repoURL, branch)
			} else {
				appYAML = generateAppApplicationHelm(meta, stageMeta, repoURL, branch)
			}
			if appYAML != nil {
				writeYAML(filepath.Join(argocdAppsDir, stageName+"-"+instanceName+".yaml"), appYAML)
			}
			mode := "helm"
			if hasSOPS {
				mode = "sops"
			}
			fmt.Printf("  Application: %s (%s)\n", instanceName, mode)
		}

		// Repos
		activeRepos := renderRepos(stageName, stageMeta, repoEntries, argocdAppsDir, branch)

		// Infrastructure (default mode)
		activeInfra := renderInfraDefaultMode(stageDir, stageName, stageMeta)
		for name := range activeInfra {
			activeApps[name] = true
		}

		// Cleanup stale Application YAMLs
		cleanupStaleApps(argocdAppsDir, stageName, activeApps, activeRepos)
	}

	fmt.Printf("  Applications -> %s/\n", argocdAppsDir)
}

func renderRepos(stageName string, stageMeta map[string]interface{}, repoEntries []map[string]interface{}, argocdAppsDir, branch string) map[string]bool {
	activeRepos := make(map[string]bool)
	for _, repoConf := range repoEntries {
		repoName, _ := repoConf["name"].(string)
		repoURL, _ := repoConf["url"].(string)
		if repoName == "" {
			repoName = filepath.Base(repoURL)
			ext := filepath.Ext(repoName)
			if ext != "" {
				repoName = strings.TrimSuffix(repoName, ext)
			}
		}
		activeRepos[repoName] = true
		repoApp := generateRepoApplication(stageName, stageMeta, repoConf, branch)
		if repoApp != nil {
			writeYAML(filepath.Join(argocdAppsDir, stageName+"-"+repoName+"-bootstrap.yaml"), repoApp)
		}
		fmt.Printf("  External repo: %s\n", repoName)
	}
	return activeRepos
}

func cleanupStaleApps(argocdAppsDir, stageName string, activeApps, activeRepos map[string]bool) {
	entries, err := os.ReadDir(argocdAppsDir)
	if err != nil {
		return
	}
	keep := make(map[string]bool)
	for name := range activeApps {
		keep[stageName+"-"+name] = true
	}
	for name := range activeRepos {
		keep[stageName+"-"+name+"-bootstrap"] = true
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), stageName+"-") || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".yaml")
		if !keep[stem] {
			os.Remove(filepath.Join(argocdAppsDir, e.Name()))
			fmt.Printf("  Removed: applications/%s\n", e.Name())
		}
	}
}

// --- Init ---

func cmdInit(stageName string) {
	fmt.Println("Initializing GitOps repository structure...\n")
	initData, err := embeddedFS.ReadFile("templates/init-repository.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: init template not found: %v\n", err)
		os.Exit(1)
	}
	var initTemplate map[string]interface{}
	if err := yaml.Unmarshal(initData, &initTemplate); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: parse init template: %v\n", err)
		os.Exit(1)
	}

	rootDirs, _ := initTemplate["rootDirectories"].([]interface{})
	for _, d := range rootDirs {
		dir, _ := d.(string)
		if dir == "" {
			continue
		}
		path := filepath.Join(repoRoot, dir)
		if _, err := os.Stat(path); err == nil {
			rel, _ := filepath.Rel(repoRoot, path)
			fmt.Printf("  Exists: %s/\n", rel)
			continue
		}
		os.MkdirAll(path, 0755)
		rel, _ := filepath.Rel(repoRoot, path)
		fmt.Printf("  Created: %s/\n", rel)
	}

	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	if _, err := os.Stat(gitignorePath); err == nil {
		rel, _ := filepath.Rel(repoRoot, gitignorePath)
		fmt.Printf("  Exists: %s\n", rel)
	} else {
		gitignore, _ := initTemplate["gitignore"].(string)
		os.WriteFile(gitignorePath, []byte(gitignore), 0644)
		rel, _ := filepath.Rel(repoRoot, gitignorePath)
		fmt.Printf("  Created: %s\n", rel)
	}

	// Always create example stage
	stagesToCreate := []string{"example"}
	if stageName != "" && stageName != "example" {
		stagesToCreate = append(stagesToCreate, stageName)
	}
	for _, s := range stagesToCreate {
		createStage(initTemplate, s)
	}

	fmt.Println("\nDone. Next steps:")
	steps, _ := initTemplate["nextSteps"].([]interface{})
	for i, s := range steps {
		step, _ := s.(string)
		step = strings.ReplaceAll(step, "{stage}", "example")
		fmt.Printf("  %d. %s\n", i+1, step)
	}
}

func cmdInitConfig() {
	fmt.Println("Creating projects/root-project.yaml...\n")
	configPath := filepath.Join(repoRoot, "projects", "root-project.yaml")
	if _, err := os.Stat(configPath); err == nil {
		rel, _ := filepath.Rel(repoRoot, configPath)
		fmt.Fprintf(os.Stderr, "  Already exists: %s\n", rel)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
		os.Exit(1)
	}
	configExample := map[string]interface{}{
		"argocd": map[string]interface{}{
			"root-namespace": "argocd-system",
			"root-project":   "default",
			"root-repo-url":  "https://git.example.com/org/gitops.git",
		},
	}
	if err := writeYAML(configPath, configExample); err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR: %v\n", err)
		os.Exit(1)
	}
	rel, _ := filepath.Rel(repoRoot, configPath)
	fmt.Printf("  Created: %s\n", rel)
	fmt.Printf("  --> Edit with your ArgoCD settings\n")
}

func createStage(initTemplate map[string]interface{}, stageName string) {
	stageTpl, _ := initTemplate["stage"].(map[string]interface{})
	stageDir := filepath.Join(repoRoot, "projects", stageName)
	fmt.Printf("\nStage: %s\n", stageName)

	dirs, _ := stageTpl["directories"].([]interface{})
	for _, d := range dirs {
		dir, _ := d.(string)
		if dir == "" {
			continue
		}
		path := filepath.Join(stageDir, dir)
		if _, err := os.Stat(path); err == nil {
			rel, _ := filepath.Rel(repoRoot, path)
			fmt.Printf("  Exists: %s/\n", rel)
			continue
		}
		os.MkdirAll(path, 0755)
		rel, _ := filepath.Rel(repoRoot, path)
		fmt.Printf("  Created: %s/\n", rel)
	}

	stageFile := filepath.Join(stageDir, "main.yaml")
	if _, err := os.Stat(stageFile); err == nil {
		rel, _ := filepath.Rel(repoRoot, stageFile)
		fmt.Printf("  Exists: %s\n", rel)
	} else {
		stageYAML, _ := stageTpl["stageYaml"].(map[string]interface{})
		if stageYAML != nil {
			writeYAML(stageFile, stageYAML)
			rel, _ := filepath.Rel(repoRoot, stageFile)
			fmt.Printf("  Created: %s\n", rel)
			fmt.Printf("  --> Edit %s with your repo URL\n", rel)
		}
	}
}

// --- Encrypt/Decrypt ---

func collectYAMLFiles(root string) []string {
	var files []string
	info, err := os.Stat(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s is not a valid file or directory\n", root)
		os.Exit(1)
	}
	if !info.IsDir() {
		return []string{root}
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func cmdEncrypt(path string) {
	files := collectYAMLFiles(path)
	for _, f := range files {
		if isSOPSEncrypted(f) {
			rel, _ := filepath.Rel(repoRoot, f)
			fmt.Printf("  Skip (already encrypted): %s\n", rel)
			continue
		}
		cmd := exec.Command("sops", "-e", "-i", "--input-type", "yaml", "--output-type", "yaml", f)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %s: %s\n", filepath.Base(f), strings.TrimSpace(string(out)))
			continue
		}
		rel, _ := filepath.Rel(repoRoot, f)
		fmt.Printf("  Encrypted: %s\n", rel)
	}
}

func cmdDecrypt(path string) {
	files := collectYAMLFiles(path)
	for _, f := range files {
		if !isSOPSEncrypted(f) {
			rel, _ := filepath.Rel(repoRoot, f)
			fmt.Printf("  Skip (not encrypted): %s\n", rel)
			continue
		}
		cmd := exec.Command("sops", "-d", "-i", "--input-type", "yaml", "--output-type", "yaml", f)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %s: %s\n", filepath.Base(f), strings.TrimSpace(string(out)))
			continue
		}
		rel, _ := filepath.Rel(repoRoot, f)
		fmt.Printf("  Decrypted: %s\n", rel)
	}
}

// --- CLI overrides (--set key=value) ---

func parseSetArgs(setFlags []string) map[string]interface{} {
	result := make(map[string]interface{})
	for _, s := range setFlags {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "  ERROR: invalid --set format: %s (expected key=value)\n", s)
			continue
		}
		keys := strings.Split(parts[0], ".")
		value := parseScalar(parts[1])
		setNestedValue(result, keys, value)
	}
	return result
}

func parseScalar(s string) interface{} {
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if s == "null" || s == "~" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func setNestedValue(m map[string]interface{}, keys []string, value interface{}) {
	for i := 0; i < len(keys)-1; i++ {
		next, ok := m[keys[i]].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			m[keys[i]] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = value
}

// --- Helpers ---

func removeNestedKey(m map[string]interface{}, keys ...string) {
	for i := 0; i < len(keys)-1; i++ {
		v, ok := m[keys[i]].(map[string]interface{})
		if !ok {
			return
		}
		m = v
	}
	delete(m, keys[len(keys)-1])
}

func setNestedKey(m map[string]interface{}, value interface{}, keys ...string) {
	for i := 0; i < len(keys)-1; i++ {
		v, ok := m[keys[i]].(map[string]interface{})
		if !ok {
			v = make(map[string]interface{})
			m[keys[i]] = v
		}
		m = v
	}
	m[keys[len(keys)-1]] = value
}

func newLineScanner(r io.Reader) *lineScanner {
	return &lineScanner{reader: r, buf: make([]byte, 0, 4096)}
}

type lineScanner struct {
	reader io.Reader
	buf    []byte
	line   string
	done   bool
}

func (s *lineScanner) Scan() bool {
	if s.done {
		return false
	}
	for {
		idx := bytes.IndexByte(s.buf, '\n')
		if idx >= 0 {
			s.line = string(s.buf[:idx])
			s.buf = s.buf[idx+1:]
			return true
		}
		tmp := make([]byte, 4096)
		n, err := s.reader.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
		}
		if err != nil {
			s.done = true
			if len(s.buf) > 0 {
				s.line = string(s.buf)
				s.buf = nil
				return true
			}
			return false
		}
	}
}

func (s *lineScanner) Text() string {
	return s.line
}

// --- Main ---

func main() {
	var (
		stage      string
		app        string
		fullRender bool
		encrypt    string
		decrypt    string
		setArgs    multiString
	)

	// Manual --init handling: filter it from args before flag.Parse
	initStage := ""
	filteredArgs := make([]string, 0, len(os.Args))
	filteredArgs = append(filteredArgs, os.Args[0])
	skipNext := false
	for i := 1; i < len(os.Args); i++ {
		if skipNext {
			skipNext = false
			continue
		}
		if os.Args[i] == "--init" {
			if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				initStage = os.Args[i+1]
				skipNext = true
			}
			continue
		}
		if os.Args[i] == "--init-config" {
			cmdInitConfig()
			return
		}
		filteredArgs = append(filteredArgs, os.Args[i])
	}

	if len(filteredArgs) == 1 && initStage != "" || (len(os.Args) > 1 && os.Args[1] == "--init" && len(filteredArgs) == 1) {
		cmdInit(initStage)
		return
	}

	flagSet := newFlagSet("argocd-render")
	showVersion := flagSet.Bool("version", false, "Print version and exit")
	flagSet.StringVar(&stage, "stage", "", "Render only specified stage")
	flagSet.StringVar(&app, "app", "", "Render only specified app")
	flagSet.BoolVar(&debugMode, "debug", false, "Enable helm debug mode")
	flagSet.BoolVar(&fullRender, "full-render", false, "Full render Helm charts to raw YAML")
	flagSet.StringVar(&encrypt, "encrypt", "", "Encrypt YAML file or directory via SOPS")
	flagSet.StringVar(&decrypt, "decrypt", "", "Decrypt SOPS file or directory")
	flagSet.Var(&setArgs, "set", "Set helm values (key=value, can be repeated)")
	flagSet.Parse(filteredArgs[1:])

	if *showVersion {
		fmt.Printf("argocd-render %s\n", appVersion)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "--init" {
		cmdInit(initStage)
		return
	}

	if debugMode {
		_ = debugMode
	}

	if encrypt != "" {
		cmdEncrypt(encrypt)
		return
	}

	if decrypt != "" {
		cmdDecrypt(decrypt)
		return
	}

	cliOverrides := parseSetArgs(setArgs)

	stages := discoverStages(stage)
	if len(stages) == 0 {
		fmt.Println("No stages found.")
		os.Exit(1)
	}

	if fullRender {
		fmt.Println("Phase 1: Preparing dependencies")
		chartDirs := collectChartDirs(app)
		if !prepareDeps(chartDirs) {
			os.Exit(1)
		}
	}

	fmt.Printf("\nPhase 2: Rendering %d stage(s)\n", len(stages))
	activeStageNames := make(map[string]bool)
	for _, stageDir := range stages {
		renderStage(stageDir, app, fullRender, cliOverrides)
		activeStageNames[filepath.Base(stageDir)] = true
	}

	// Cleanup stages that no longer exist
	if fullRender && stage == "" {
		renderedDir := filepath.Join(repoRoot, "rendered")
		appsDir := filepath.Join(repoRoot, "rendered", "argocd", "applications")
		if entries, err := os.ReadDir(renderedDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() || e.Name() == "argocd" {
					continue
				}
				if !activeStageNames[e.Name()] {
					os.RemoveAll(filepath.Join(renderedDir, e.Name()))
					fmt.Printf("  Removed: rendered/%s/\n", e.Name())
				}
			}
		}
		if entries, err := os.ReadDir(appsDir); err == nil {
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".yaml") {
					continue
				}
				matched := false
				for s := range activeStageNames {
					if strings.HasPrefix(e.Name(), s+"-") {
						matched = true
						break
					}
				}
				if !matched {
					os.Remove(filepath.Join(appsDir, e.Name()))
					fmt.Printf("  Removed: applications/%s\n", e.Name())
				}
			}
		}
	}

	fmt.Println("\nDone.")
}

// multiString implements flag.Value for repeated --set flags
type multiString []string

func (m *multiString) String() string { return strings.Join(*m, ", ") }
func (m *multiString) Set(val string) error {
	*m = append(*m, val)
	return nil
}

// newFlagSet creates a flag.FlagSet with custom usage
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "argocd-render %s — GitOps Render Tool\n\n", appVersion)
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", name)
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s                                    Render all stages\n", name)
		fmt.Fprintf(os.Stderr, "  %s --stage test                       Render only stage test\n", name)
		fmt.Fprintf(os.Stderr, "  %s --stage test --app grafana         Render specific app\n", name)
		fmt.Fprintf(os.Stderr, "  %s --init                             Create directory structure\n", name)
		fmt.Fprintf(os.Stderr, "  %s --init test                        Create stage test\n", name)
		fmt.Fprintf(os.Stderr, "  %s --init-config                      Create projects/root-project.yaml\n", name)
		fmt.Fprintf(os.Stderr, "  %s --full-render                      Full render to raw YAML\n", name)
		fmt.Fprintf(os.Stderr, "  %s --set image.tag=v1.0 --set replicas=3\n", name)
		fmt.Fprintf(os.Stderr, "  %s --encrypt secrets/                 Encrypt YAML files\n", name)
		fmt.Fprintf(os.Stderr, "  %s --decrypt rendered/                Decrypt SOPS files\n", name)
	}
	return fs
}
