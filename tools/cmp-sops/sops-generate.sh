#!/bin/sh
set -e

log() { echo "[sops-cmp] $*" >&2; }

# dep_hint reads /tmp/dep.log (helm dep build stderr) and surfaces actionable
# hints for the most common failures, instead of dumping helm's verbose output.
# Each recognized pattern logs a targeted message with a concrete next step;
# the full log is appended at the end for anything we didn't match.
dep_hint() {
    repo_config="$1"
    [ -f /tmp/dep.log ] || return 0
    log "ERROR: helm dep build failed after retries. Diagnosing:"
    matched=0

    # 401/403 on a chart repository index — credentials missing or wrong.
    if grep -qE '401 Unauthorized|403 Forbidden' /tmp/dep.log; then
        matched=1
        # Extract the offending repo URL(s).
        urls=$(grep -oE 'https?://[^ "]+' /tmp/dep.log | sort -u | grep -vE 'index\.yaml$' || true)
        log "  cause: authentication failed for a helm repository (401/403)."
        if [ -n "$urls" ]; then
            log "  repos: $(echo "$urls" | tr '\n' ' ')"
        fi
        log "  fix:   add the repository to ArgoCD Repositories (with username/password)"
        log "         AND list its URL in the AppProject sourceRepos"
        log "         (via projectSourceRepos in main.yaml)."
        log "         helm-repo-sync only materializes creds for repos that are both"
        log "         registered in ArgoCD AND allowed by sourceRepos."
    fi

    # "no cached repository for <hash>" — helm couldn't resolve a dependency
    # because its repository isn't in repositories.yaml at all.
    if grep -q 'no cached repository for' /tmp/dep.log; then
        matched=1
        log "  cause: a Chart.yaml dependency points at a helm repository that is"
        log "         neither registered in ArgoCD nor in sourceRepos."
        # Pull the dependency names from Chart.yaml to help identify which one.
        if [ -f "$CHART_DIR/Chart.yaml" ]; then
            deps=$(grep -E '^[[:space:]]*-[[:space:]]*name:' "$CHART_DIR/Chart.yaml" | sed 's/.*name:[[:space:]]*//' | tr '\n' ' ')
            [ -n "$deps" ] && log "  Chart.yaml dependencies: $deps"
        fi
        log "  fix:   for each dependency repository, either:"
        log "         - register it in ArgoCD Repositories + add URL to sourceRepos, or"
        log "         - vendor the chart into charts/*.tgz (skip dep build)."
    fi

    # Repository URL not declared anywhere (helm "no repository definition").
    if grep -q 'no repository definition for' /tmp/dep.log; then
        matched=1
        log "  cause: a Chart.yaml dependency uses a repository URL that helm cannot"
        log "         resolve. It may be missing from repositories.yaml."
        log "  fix:   ensure the repository URL in Chart.yaml matches exactly the URL"
        log "         in ArgoCD Repositories / sourceRepos (no trailing slash, same scheme)."
    fi

    if [ "$matched" -ne 1 ]; then
        log "  (unrecognized error — raw helm output below)"
    fi
    # Always show the (flattened) raw log so nothing is hidden.
    log "  raw: $(tr '\n' ' ' < /tmp/dep.log | sed 's/[[:space:]]\+/ /g')"
}

# DEBUG: dump VALUES_ENV to verify it reaches the plugin from source.plugin.env.
# ArgoCD exposes Application source.plugin.env vars with the ARGOCD_ENV_ prefix.
# Brackets reveal trailing/leading whitespace; len catches empty/missing var.
log "DEBUG: ARGOCD_ENV_VALUES_ENV=[${ARGOCD_ENV_VALUES_ENV}] (len=${#ARGOCD_ENV_VALUES_ENV})"

# --- Mode detection (by files present in cwd = source.path of the Application) ---
#   app.yaml   -> argocd-render mode (chart lives at $REPO_ROOT/charts/<chartName>;
#                 chartName/namespace read from app.yaml). Current gitops layout.
#   Chart.yaml -> standard helm mode (chart = cwd; for any helm project without
#                 argocd-render). INSTANCE = basename of cwd; NAMESPACE from
#                 $ARGOCD_APP_NAMESPACE env or "default".
#   neither    -> full-render mode (no helm: just sops -d every SOPS-encrypted
#                 YAML in the tree). Unchanged.

if [ -f "app.yaml" ] || [ -f "Chart.yaml" ]; then
    # ============================================================
    # DEFAULT MODE: helm secrets template with on-the-fly decryption
    # ============================================================

    if [ -f "app.yaml" ]; then
        # --- argocd-render layout ---
        CHART_NAME=$(yq '.chartName' app.yaml)
        NAMESPACE=$(yq '.namespace' app.yaml)
        INSTANCE=$(basename "$(pwd)")

        # Find repo root (directory containing charts/)
        REPO_ROOT=""
        dir=$(pwd)
        while [ "$dir" != "/" ]; do
            if [ -d "$dir/charts" ]; then
                REPO_ROOT="$dir"
                break
            fi
            dir=$(dirname "$dir")
        done

        if [ -z "$REPO_ROOT" ]; then
            log "ERROR: charts/ directory not found"
            exit 1
        fi

        CHART_DIR="$REPO_ROOT/charts/$CHART_NAME"
        if [ ! -d "$CHART_DIR" ]; then
            log "ERROR: chart not found: $CHART_DIR"
            exit 1
        fi
        log "argocd-render mode: instance=$INSTANCE chart=$CHART_NAME namespace=$NAMESPACE"
    else
        # --- standard helm layout (chart = cwd) ---
        CHART_DIR="$(pwd)"
        INSTANCE=$(basename "$(pwd)")
        NAMESPACE="${ARGOCD_APP_NAMESPACE:-default}"
        log "standard helm mode: instance=$INSTANCE chart=$CHART_DIR namespace=$NAMESPACE"
    fi

    # ============================================================
    # Build the -f flag list.
    # helm merges -f left-to-right (later overrides earlier).
    # Two modes based on $ARGOCD_ENV_VALUES_ENV:
    #
    # VALUES_ENV set   (multi-env, universal helm projects):
    #   values.yaml | values.yml       (base defaults, optional)
    #   values-<VALUES_ENV>.yaml|yml      (env override, optional)
    #   secrets-<VALUES_ENV>.yaml|yml     (env secrets, SOPS, optional)
    #   → only the matching env suffix is picked; other values-*/secrets-*
    #     are ignored.
    #
    # VALUES_ENV unset (single-env):
    #   values.yaml | values.yml       (optional)
    #   secrets*.yaml / secrets*.yml   (all of them, sorted; helm-secrets
    #     decrypts on the fly by naming convention)
    # ============================================================
    SOURCES=""

    add_src() {
        # append a file path to $SOURCES (newline-separated), skip if missing
        [ -f "$1" ] || return 0
        if [ -z "$SOURCES" ]; then
            SOURCES=$1
        else
            SOURCES="$SOURCES
$1"
        fi
    }

    if [ -n "$ARGOCD_ENV_VALUES_ENV" ]; then
        log "multi-env mode: VALUES_ENV=$ARGOCD_ENV_VALUES_ENV"
        # base values
        add_src "values.yaml" || add_src "values.yml"
        # env values
        add_src "values-${ARGOCD_ENV_VALUES_ENV}.yaml" || true
        add_src "values-${ARGOCD_ENV_VALUES_ENV}.yml"  || true
        # env secrets (helm-secrets decrypts by naming convention)
        add_src "secrets-${ARGOCD_ENV_VALUES_ENV}.yaml" || true
        add_src "secrets-${ARGOCD_ENV_VALUES_ENV}.yml"  || true
        if [ -z "$SOURCES" ]; then
            log "WARNING: VALUES_ENV=$ARGOCD_ENV_VALUES_ENV but no values/secrets found for it"
        fi
    else
        # single-env: base values + all secrets*
        add_src "values.yaml" || add_src "values.yml"
        [ -n "$SOURCES" ] || log "WARNING: no values.yaml/values.yml found; rendering with secrets only (if any)"

        SECRETS=$(find . -maxdepth 1 -type f \
                    \( -name 'secrets*.yaml' -o -name 'secrets*.yml' \) \
                    | sed 's|^\./||' | sort | grep -vE '^\.sops\.ya?ml$' || true)
        OLDIFS=$IFS
        IFS='
'
        for sec in $SECRETS; do
            add_src "$sec"
        done
        IFS=$OLDIFS
    fi

    VALUE_FLAGS=""
    if [ -n "$SOURCES" ]; then
        OLDIFS=$IFS
        IFS='
'
        for src in $SOURCES; do
            VALUE_FLAGS="$VALUE_FLAGS -f $src"
        done
        IFS=$OLDIFS
    fi

    # Build helm dependencies if needed (with retry for transient network errors).
    # Called as plain `helm`, NOT `helm secrets` (dep build is not a secret command).
    # 3 attempts × 60s = 180s — fits under the raised CMP gRPC deadline
    # (server/controller.repo.server.timeout.seconds = 200s in values.yml).
    if [ -f "$CHART_DIR/Chart.yaml" ] && ! ls "$CHART_DIR/charts/"*.tgz >/dev/null 2>&1; then
        # Sync ArgoCD repository Secrets into helm's repositories.yaml so private
        # helm dependencies resolve. helm-repo-sync filters credentials by the
        # AppProject sourceRepos allowlist (ARGOCD_APP_PROJECT_NAME) and validates
        # Chart.yaml dependencies against it (fail-closed on policy violation).
        # Idempotent with TTL caching (CMP_HELM_REPO_SYNC_TTL, default 300s).
        # It prints the per-project repositories.yaml path to stdout; we capture
        # it and pass to helm dep build via --repository-config so helm reads the
        # right file (otherwise it looks at the default HELM_CONFIG_HOME path).
        REPO_CONFIG=""
        if command -v helm-repo-sync >/dev/null 2>&1; then
            if REPO_CONFIG=$(helm-repo-sync --chart "$CHART_DIR" 2>>/tmp/repo-sync.err); then :; else
                log "WARNING: helm-repo-sync failed (continuing with default repositories.yaml)"
                log "  $(tr '\n' ' ' </tmp/repo-sync.err 2>/dev/null)"
            fi
        fi
        # Build the --repository-config flag only when helm-repo-sync returned a path.
        DEP_REPO_FLAG=""
        if [ -n "$REPO_CONFIG" ]; then
            DEP_REPO_FLAG="--repository-config=$REPO_CONFIG"
        fi

        log "building helm dependencies"
        DEP_ATTEMPTS=3
        DEP_TIMEOUT=60
        dep_ok=0
        i=1
        while [ "$i" -le "$DEP_ATTEMPTS" ]; do
            if timeout "${DEP_TIMEOUT}s" helm dep build "$CHART_DIR" $DEP_REPO_FLAG \
                >/tmp/dep.log 2>&1; then
                dep_ok=1
                break
            fi
            log "helm dep build attempt $i/$DEP_ATTEMPTS failed"
            i=$((i + 1))
        done
        if [ "$dep_ok" -ne 1 ]; then
            # Parse the most common failure causes out of helm's verbose stderr
            # and surface a actionable message instead of the raw log dump.
            dep_hint "$REPO_CONFIG"
            exit 1
        fi
    fi

    # Render. helm-secrets wrapper decrypts every -f file matching the
    # "secrets*.{yaml,yml}" convention on the fly; plain values pass through.
    if [ -n "$SOURCES" ]; then
        log "values sources (in merge order): $(printf '%s' "$SOURCES" | tr '\n' ',' | sed 's/,$//')"
    else
        log "values sources: (none)"
    fi
    log "helm secrets template $INSTANCE $CHART_DIR${VALUE_FLAGS:+ (with values)}"
    # shellcheck disable=SC2086 # intentional: split VALUE_FLAGS into -f args
    helm secrets template "$INSTANCE" "$CHART_DIR" \
        $VALUE_FLAGS \
        -n "$NAMESPACE" \
        --include-crds

else
    # ============================================================
    # FULL-RENDER MODE: decrypt SOPS-encrypted files only (no helm)
    # ============================================================

    log "full-render mode: decrypting files"

    find . -type f \( -name '*.yaml' -o -name '*.yml' \) | sort | while IFS= read -r f; do
        if grep -q '^sops:' "$f" 2>/dev/null; then
            sops -d --input-type yaml --output-type yaml "$f"
        else
            cat "$f"
        fi
        echo "---"
    done
fi
