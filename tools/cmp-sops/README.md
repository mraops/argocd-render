# cmp-sops — образ CMP sidecar для ArgoCD repo-server

Образ Config Management Plugin (CMP) sidecar для `argocd-repo-server`. Рендерит
helm-чарты с on-the-fly дешифровкой SOPS-секретов и резолвит приватные
helm-зависимости через ArgoCD repository-Secret'ы.

ArgoCD активирует этот плагин (`name: sops`) для приложений, у которых в
`source.path` есть SOPS-зашифрованные файлы. argocd-render помечает такие
приложения как `source.plugin: {name: sops}` вместо стандартного helm-source.

## Состав

```
cmp-sops/
├── Dockerfile           # multi-stage сборка образа
├── sops-generate.sh     # CMP-генератор (вызывается ArgoCD как generate-команда)
└── helm-repo-sync/      # Go CLI: ArgoCD repository-Secret'ы → helm repositories.yaml
    └── ... (см. helm-repo-sync/README.md)
```

## Dockerfile

Multi-stage build, финальный образ на базе `quay.io/argoproj/argocd:v3.4.4`.

| Stage | Что делает |
|-------|-----------|
| `build-helm-repo-sync` | Собирает Go-бинарь `helm-repo-sync` из `helm-repo-sync/` (golang:1.24-alpine, CGO off, pure stdlib) |
| `download` (ubuntu:25.10) | Скачивает sops v3.13.2, yq v4.53.3, helm v3.19.2; ставит helm-secrets plugin в `/opt/helm-plugins` |
| final (argocd:v3.4.4) | Копирует инструменты в `/usr/local/bin/`, плагин в `/opt/helm-plugins` |

**Инструменты в финальном образе** (`/usr/local/bin/`): `sops`, `yq`, `helm`, `helm-repo-sync`, `sops-generate.sh`.

**ENV (переопределены в финальной стадии — multi-stage не переносит ENV):**
```
HELM_PLUGINS=/opt/helm-plugins
HELM_CACHE_HOME=/tmp/helm-cache
HELM_CONFIG_HOME=/tmp/helm-config
XDG_CACHE_HOME=/tmp/helm-cache
XDG_CONFIG_HOME=/tmp/helm-config
```
`/tmp` writable под uid 999 (argocd), там же живёт `repositories.yaml`, который пишет helm-repo-sync.

## sops-generate.sh

CMP-генератор — его вызывает ArgoCD как `generate`-команду. cwd = `source.path`
Application. **stdout → манифесты** (захватываются ArgoCD), **stderr → логи** с
префиксом `[sops-cmp]`.

### Режимы (авто-определение по файлам в cwd)

| Условие | Режим | Что делает |
|---------|-------|-----------|
| `app.yaml` | **argocd-render** | `chartName`/`namespace` из app.yaml, чарт в `$REPO_ROOT/charts/<chartName>` (поиск `charts/` вверх по дереву), `helm secrets template` |
| `Chart.yaml` (без app.yaml) | **standard helm** | Чарт = cwd, namespace из `$ARGOCD_APP_NAMESPACE` (default), `helm secrets template` |
| ничего | **full-render** | `sops -d` для каждого SOPS-файла в дереве, без helm |

### Value-файлы (`-f`)

helm merges `-f` left-to-right (позже перекрывает раньше). Два режима по
`$ARGOCD_ENV_VALUES_ENV` (из `source.plugin.env` Application):

**Multi-env** (`VALUES_ENV` задан, universal helm projects):
```
values.yaml                          # base
values-<VALUES_ENV>.yaml|.yml        # env override
secrets-<VALUES_ENV>.yaml|.yml       # env secrets (SOPS)
```
Подбирается только суффикс текущего env.

**Single-env** (`VALUES_ENV` не задан):
```
values.yaml                          # base
secrets*.yaml / secrets*.yml         # все secrets-файлы (sorted)
```
helm-secrets декриптит их on-the-fly по content (SOPS MAC), не по имени.

### helm dependencies

`helm dep build` (plain `helm`, не `helm secrets`) с retry 3×60s = 180s — вписывается
в поднятый gRPC-дедлайн 200s (`reposerver.repo.server.timeout.seconds`). Guard: только
если нет предвендоренных `.tgz` в `charts/`. Пути cache/config берёт из ENV
(`HELM_CACHE_HOME`/`HELM_CONFIG_HOME`) — `repositories.yaml` живёт в
`/tmp/helm-config/repositories-<project>.yaml` (per-project, его пишет helm-repo-sync).

Перед dep build вызывается `helm-repo-sync --chart <CHART_DIR>` — он:
- читает AppProject (`ARGOCD_APP_PROJECT_NAME`) → `spec.sourceRepos` allowlist
- валидирует `Chart.yaml` dependencies против allowlist (fail-closed при нарушении)
- материализует `repositories.yaml` только из разрешённых repository-Secret'ов
- печатает путь к созданному per-project файлу в stdout

`sops-generate.sh` захватывает этот путь и передаёт в `helm dep build --repository-config=<path>`,
чтобы helm читал именно per-project файл (иначе он ищет дефолтный путь из `HELM_CONFIG_HOME`
и не находит credentials → 401).

При ошибке dep build `dep_hint()` парсит типичные причины (401/403, no cached repository,
no repository definition) и выдаёт конкретный cause + fix вместо дампа сырого stderr helm.

Это закрывает sourceRepos bypass, присущий CMP sidecar (credential isolation между
проектами + блокировка неразрешённых публичных helm-репо в зависимостях). Подробнее —
в `helm-repo-sync/README.md` (раздел «Security»).

### Финальный рендер

```sh
helm secrets template "$INSTANCE" "$CHART_DIR" $VALUE_FLAGS -n "$NAMESPACE" --include-crds
```
- `helm secrets` — плагин helm-secrets, декриптит SOPS-values on-the-fly
- `--include-crds` — включает CRDs из `crds/` чарта

## Сборка

```bash
# из корня репозитория
docker build tools/cmp-sops/ -t argocd-sops-cmp:latest
docker push argocd-sops-cmp:latest
```
Контекст сборки — `tools/cmp-sops/` (Dockerfile там же, COPY-пути относительные).

## Подключение

1. **RBAC для helm-repo-sync** — дать repo-server право читать Secret'ы через блок
   `repoServer.rbac` в ArgoCD helm-values:
   ```yaml
   repoServer:
     rbac:
       - apiGroups: [""]
         resources: ["secrets"]
         verbs: ["get", "list"]
   ```
   Чарт создаст Role и привяжет к ServiceAccount repo-server'а. Можно сузить через
   `resourceNames` до конкретных repository-Secret'ов.

2. **CMP plugin config** — ConfigManagementPlugin manifest (в репозитории ArgoCD-инсталляции,
   не здесь). Discovery срабатывает на любой YAML с маркером `^sops:`, generate-команда
   указывает на `/usr/local/bin/sops-generate.sh`:
   ```yaml
   configs:
     cmp:
       create: true
       plugins:
         sops:
           discover:
             find:
               command: [sh, -c, "find . -type f \\( -name '*.yaml' -o -name '*.yml' \\) -exec grep -q '^sops:' {} \\; -print"]
           generate:
             command: [/usr/local/bin/sops-generate.sh]
   ```

3. **Sidecar в repo-server** — `repoServer.extraContainers` монтирует образ, пробрасывает
   `SOPS_AGE_KEY` (из Secret с age-ключом) и helm-ENV. Пример — в репозитории
   ArgoCD-инсталляции.

4. **SOPS_AGE_KEY** — приватный age-ключ для дешифровки, пробрасывается в sidecar через env
   из Secret (например `sops-age-key`). Без него sops не сможет расшифровать секреты.

## Переменные окружения sidecar'а

| Переменная | Источник | Назначение |
|-----------|----------|-----------|
| `SOPS_AGE_KEY` | Secret (age-key) | Приватный age-ключ для sops-дешифровки |
| `ARGOCD_ENV_VALUES_ENV` | Application `source.plugin.env` (`VALUES_ENV`) | Выбор multi-env values/secrets-файлов |
| `ARGOCD_APP_NAMESPACE` | ArgoCD (стандартный) | Namespace в standard-helm режиме |
| `HELM_PLUGINS` / `HELM_*_HOME` | Образ ENV | Пути helm-плагинов/кэша/конфига |
| `CMP_HELM_REPO_SYNC_*` | (опционально) | Настройки helm-repo-sync — см. `helm-repo-sync/README.md` |

## Связь с argocd-render и tools/sops

- **argocd-render** генерирует Application CR: приложения с `secrets*.yaml` получают
  `source.plugin: {name: sops}` → ArgoCD вызывает этот sidecar.
- **tools/sops/** — `make encrypt/decrypt` для локальной/CI подготовки SOPS-файлов
  перед коммитом; этот sidecar расшифровывает их в кластере.

Подробнее про резолв приватных репозиториев — в `helm-repo-sync/README.md`.
