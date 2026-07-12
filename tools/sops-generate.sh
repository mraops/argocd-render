#!/bin/sh
set -e

log() { echo "[sops-cmp] $*" >&2; }

# DEBUG: dump APP_ENV to verify it reaches the plugin from source.plugin.env.
# ArgoCD exposes Application source.plugin.env vars with the ARGOCD_ENV_ prefix.
# Brackets reveal trailing/leading whitespace; len catches empty/missing var.
log "DEBUG: ARGOCD_ENV_APP_ENV=[${ARGOCD_ENV_APP_ENV}] (len=${#ARGOCD_ENV_APP_ENV})"

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
    # Two modes based on $ARGOCD_ENV_APP_ENV:
    #
    # APP_ENV set   (multi-env, universal helm projects):
    #   values.yaml | values.yml       (base defaults, optional)
    #   values-<APP_ENV>.yaml|yml      (env override, optional)
    #   secrets-<APP_ENV>.yaml|yml     (env secrets, SOPS, optional)
    #   → only the matching env suffix is picked; other values-*/secrets-*
    #     are ignored.
    #
    # APP_ENV unset (argocd-render legacy layout):
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

    if [ -n "$ARGOCD_ENV_APP_ENV" ]; then
        log "multi-env mode: APP_ENV=$ARGOCD_ENV_APP_ENV"
        # base values
        add_src "values.yaml" || add_src "values.yml"
        # env values
        add_src "values-${ARGOCD_ENV_APP_ENV}.yaml" || true
        add_src "values-${ARGOCD_ENV_APP_ENV}.yml"  || true
        # env secrets (helm-secrets decrypts by naming convention)
        add_src "secrets-${ARGOCD_ENV_APP_ENV}.yaml" || true
        add_src "secrets-${ARGOCD_ENV_APP_ENV}.yml"  || true
        if [ -z "$SOURCES" ]; then
            log "WARNING: APP_ENV=$ARGOCD_ENV_APP_ENV but no values/secrets found for it"
        fi
    else
        # legacy: base values + all secrets*
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
        # helm dependencies resolve. repo-sync is idempotent with TTL caching
        # (CMP_REPO_SYNC_TTL, default 300s). Failure is non-fatal: helm dep build
        # may still succeed with public repos or a previously written file.
        if command -v repo-sync >/dev/null 2>&1; then
            repo-sync || log "WARNING: repo-sync failed (continuing with existing repositories.yaml)"
        fi
        log "building helm dependencies"
        DEP_ATTEMPTS=3
        DEP_TIMEOUT=60
        dep_ok=0
        i=1
        while [ "$i" -le "$DEP_ATTEMPTS" ]; do
            if timeout "${DEP_TIMEOUT}s" helm dep build "$CHART_DIR" \
                --repository-cache /tmp/helm-cache \
                --repository-config /tmp/helm-config/helm/repositories.yaml \
                >/tmp/dep.log 2>&1; then
                dep_ok=1
                break
            fi
            log "helm dep build attempt $i/$DEP_ATTEMPTS failed: $(tr '\n' ' ' </tmp/dep.log 2>/dev/null)"
            i=$((i + 1))
        done
        if [ "$dep_ok" -ne 1 ]; then
            log "ERROR: helm dep build failed after $DEP_ATTEMPTS attempts"
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
