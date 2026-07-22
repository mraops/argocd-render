# argocd-render

CLI-утилита для рендеринга Helm-чартов и генерации ArgoCD Application CR. Написана на Go, шаблоны встроены в бинарник.

## Возможности

- Два режима работы: генерация Application CR (default) и полный рендер в raw YAML (full-render)
- Интеграция с SOPS (age) для шифрования секретов
- Параллельный рендеринг приложений
- Кэширование зависимостей Helm (MD5)
- Встроенные шаблоны Application CR — не нужны внешние файлы
- Поддержка `--set key=value` для переопределения values в CI/CD пайплайнах
- Кросс-компиляция под linux amd64/arm64 и darwin arm64

## Режимы работы

### Default mode

Генерирует только ArgoCD Application CR. ArgoCD сам рендерит Helm-чарты при синхронизации.

Для приложений с SOPS-секретами генерируется Application с `plugin: {name: sops}` — рендеринг происходит через CMP sidecar в repo-server.

```
argocd-render --stage production
```

Результат: `rendered/argocd/applications/<stage>-<app>.yaml`

### Full-render mode

Рендерит Helm-чарты в статический YAML через `helm template`, затем генерирует Application CR с directory source на отрендеренные файлы. Секреты автоматически перешифровываются через SOPS.

```
argocd-render --full-render --stage production
```

Результат:
- `rendered/<stage>/apps/<app>/<kind>/<name>.yaml` — отрендеренные манифесты
- `rendered/argocd/applications/<stage>-<app>.yaml` — Application CR

## Установка

### Сборка из исходников

Требуется Go 1.24+:

```bash
make build
# бинарник: build/argocd-render
```

### Кросс-компиляция

```bash
make build-linux-amd64     # Linux x86_64
make build-linux-arm64     # Linux ARM64 (Mac M-series, AWS Graviton)
make build-darwin-arm64    # macOS Apple Silicon
make build-all             # все платформы сразу
```

Бинарники кладутся в `build/`:
```
build/
├── argocd-render-linux-amd64
├── argocd-render-linux-arm64
└── argocd-render-darwin-arm64
```

### Docker-образ

Образ включает бинарник + helm v3.19.2 + sops v3.12.2. Подходит для CI/CD пайплайнов.

```bash
# Сборка
make image TAG=v0.1.0

# Пуш в registry
make push TAG=v0.1.0

# ARM64
make image-arm64 TAG=v0.1.0
make push-arm64  TAG=v0.1.0

# Оба архитектуры
make image-all push-all TAG=v0.1.0
```

Переменные `IMAGE` и `TAG` можно задать через окружение:
```bash
export IMAGE=registry.example.com/tools/argocd-render
export TAG=v1.2.3
make image push
```

## Использование

### Базовые команды

```bash
# Рендер всех stage
argocd-render

# Рендер конкретного stage
argocd-render --stage production

# Рендер конкретного приложения
argocd-render --stage production --app grafana

# Полный рендер (raw YAML)
argocd-render --full-render
argocd-render --full-render --stage production

# Отладка (включает --debug в helm)
argocd-render --debug --stage production
```

### Переопределение values (--set)

Для CI/CD пайплайнов — переопределение values поверх values.yaml:

```bash
# Скалярные значения
argocd-render --set image.tag=v1.2.3

# Вложенные ключи (точка как разделитель)
argocd-render --set image.repository=registry.example.com/app

# Несколько значений
argocd-render --set image.tag=v1.2.3 --set replicas=3 --set resources.enabled=true

# Числа и булевы значения распознаются автоматически
argocd-render --set replicas=5 --set service.enabled=false
```

Можно указать несколько `--set` — значения мержатся поверх values.yaml и secrets.yaml.

### Инициализация репозитория

```bash
# Создать базовую структуру + example stage
argocd-render --init

# Создать структуру + example stage + указанный stage
argocd-render --init production

# Создать только projects/root-project.yaml (конфиг argocd)
argocd-render --init-config
```

`--init` создаёт:
```
charts/
projects/
.gitignore
projects/example/
├── apps/
├── namespaces/
├── rbac/
├── networkpolicy/
└── main.yaml
```

`--init-config` создаёт `projects/root-project.yaml` с примером конфигурации argocd. Нужен только в gitops-репозиториях (где stage использует `projectNamespace`), в репозиториях приложений — не требуется.

### SOPS шифрование/дешифрование

`--encrypt`/`--decrypt` обрабатывают **только файлы секретов** — те, чьё имя начинается на `secrets` (`secrets.yaml`, `secrets.yml`, `secrets-db.yaml` и т.п.). Остальные YAML (`app.yaml`, `values.yaml`, `.sops.yaml`) не трогаются — они читаются рендером как открытые метаданные. Это позволяет безопасно запускать шифрование на всей папке приложения.

```bash
# Зашифровать все secrets* в папке приложения (несколько файлов сразу)
argocd-render --encrypt projects/production/apps/myapp/

# Зашифровать один файл
argocd-render --encrypt projects/production/apps/myapp/secrets.yaml

# Расшифровать все secrets* в папке приложения
argocd-render --decrypt projects/production/apps/myapp/

# Расшифровать один файл
argocd-render --decrypt projects/production/apps/myapp/secrets.yaml
```

Файлы определяются как SOPS-зашифрованные по наличию поля `sops:` в YAML. Правила шифрования (age-ключ) берутся из `.sops.yaml` в корне репозитория.

### CMP sidecar и приватные репозитории

Директория `tools/` содержит образ CMP sidecar для `argocd-repo-server` и утилиты для рендера приложений с SOPS-секретами прямо в кластере:

```
tools/
├── cmp-sops/                # образ CMP sidecar
│   ├── Dockerfile           # argocd + sops + helm + helm-secrets + helm-repo-sync
│   ├── sops-generate.sh     # CMP-генератор: helm secrets template / sops -d
│   └── helm-repo-sync/      # Go CLI: ArgoCD repository-Secret'ы → helm repositories.yaml
└── sops/                    # утилиты локального шифрования
    ├── Makefile             # make encrypt/decrypt (sops)
    └── .sops.example.yaml   # пример .sops.yaml
```

#### Приватные helm-зависимости в CMP sidecar

ArgoCD v3 не прокидывает repository-credentials в CMP sidecar (нативно — только Git через `provideGitCreds`). Поэтому `helm dep build` в плагине падает на приватных helm-репозиториях. `tools/cmp-sops/helm-repo-sync` закрывает этот провал: читает Secret'ы с label `argocd.argoproj.io/secret-type=repository` (`type: helm`) через in-cluster Kubernetes API и пишет helm-совместимый `repositories.yaml`.

**Сборка образа:**
```bash
docker build tools/cmp-sops/ -t argocd-sops-cmp:latest
```
(контекст сборки — `tools/cmp-sops/`, Dockerfile там же)

**Подключение:**
1. Дать repo-server право читать Secret'ы (для helm-repo-sync) — через блок `repoServer.rbac` в ArgoCD helm-values:
   ```yaml
   repoServer:
     rbac:
       - apiGroups: [""]
         resources: ["secrets"]
         verbs: ["get", "list"]
   ```
   Чарт создаст Role и привяжет её к ServiceAccount repo-server'а. При желании доступ можно сузить через `resourceNames` до конкретных repository-Secret'ов.
2. Настроить CMP sidecar на использование образа (ConfigManagementPlugin manifest — в репозитории ArgoCD-инсталляции).

**Переменные helm-repo-sync** (env в контейнере sidecar):

| Переменная | По умолчанию | Описание |
|-----------|--------------|----------|
| `CMP_HELM_REPO_SYNC_TTL` | `86400` | Секунды жизни кэша `repositories.yaml`. В течение TTL повторные запуски пропускаются |
| `CMP_HELM_REPO_SYNC_OUT` | `/tmp/helm-config/repositories.yaml` | Путь к выходному файлу |
| `CMP_HELM_REPO_SYNC_NAMESPACE` | из serviceaccount | Namespace для чтения Secret'ов |
| `CMP_HELM_REPO_SYNC_INSECURE` | `0` | `1`/`true` — отключить проверку TLS (debug) |

Credentials не покидают pod (`emptyDir`, uid 999, файл `0600`) и не логируются. Покрытие: HTTP/HTTPS helm-репо. OCI-registry и Git-сабчарты не покрываются (последние — через нативный `provideGitCreds`).

Подробнее — в `tools/cmp-sops/helm-repo-sync/README.md`.

### Очистка кэша и артефактов

```bash
argocd-render --clean
```

Удаляет артефакты `helm dep` и кэш рендера, чтобы следующий рендер пересобрал всё с нуля. Удаляет по каждому чарту:
- `charts/<chart>/charts/*.tgz` — vendored dependencies
- `charts/<chart>/Chart.lock` — helm dependency lockfile
- `charts/<chart>/.dep.md5` — кэш-маркер argocd-render (хеш для skip-логики `helm dep build`)

И глобальный кэш `.render-cache/` (helm cache/config/data).

`--clean` — автономная команда: выполняет очистку и завершается, рендер не запускается. Удобно, когда `helm dep build` тащит устаревшие зависимости или кэш повредился.

### Версия

```bash
argocd-render --version
# argocd-render v0.1.0

argocd-render --help
# argocd-render v0.1.0 — GitOps Render Tool
```

## Структура репозитория

```
.
├── projects/
│   ├── root-project.yaml                    # глобальный конфиг argocd (gitops-репо)
│   └── <stage>/
│       ├── main.yaml                        # конфиг stage
│       ├── apps/
│       ├── namespaces/
│       ├── rbac/
│       └── networkpolicy/
├── charts/
│   ├── universal-helm-chart/                # Helm-чарт приложения
│   │   ├── Chart.yaml
│   │   ├── values.yaml
│   │   └── charts/
│   │       └── base-0.0.52.tgz
│   ├── kubernetes-resources/                # инфраструктурный чарт
│   │   ├── Chart.yaml
│   │   ├── values.yaml
│   │   └── templates/
│   │       ├── namespace.yaml               # {{- with .Values.namespace }}
│   │       ├── rbac.yaml                    # {{- with .Values.rbac }}
│   │       ├── networkpolicy.yaml           # {{- with .Values.networkpolicy }}
│   │       └── project.yaml                 # {{- with .Values.project }}
│   └── my-app/                              # кастомный чарт
│       ├── Chart.yaml
│       └── ...
└── rendered/                                # выходные данные (генерируется)
    ├── <stage>/apps/<app>/<kind>/           # raw YAML (full-render mode)
    └── argocd/applications/                 # Application CR (оба режима)
```

## Конфигурация

### projects/root-project.yaml (глобальный конфиг argocd)

Конфигурация ArgoCD для gitops-репозитория. Создаётся через `argocd-render --init-config`.

```yaml
argocd:
  root-namespace: argocd-system       # namespace для AppProject
  root-project: default               # ArgoCD project для bootstrap-приложений
  root-repo-url: https://git.example.com/org/gitops.git  # URL корневого репо
```

Файл требуется только если stage использует `projectNamespace` (т.е. это gitops-репозиторий). В репозиториях приложений `root-project.yaml` не нужен — параметры берутся из `main.yaml` stage.

Валидация: при наличии `projectNamespace` в stage проверяются обязательные поля `argocd.root-project` и `argocd.root-repo-url`. При отсутствии или неполном конфиге рендер падает с понятной ошибкой и примером.

### main.yaml (stage)

```yaml
repoUrl: https://git.example.com/org/apps.git    # URL репозитория
branch: master                                    # ветка
server: https://kubernetes.default.svc            # API server (опционально)
description: Production environment               # описание (опционально)
project: production                               # ArgoCD project по умолчанию (опционально)
projectNamespace: int-rvc                         # namespace для stage проекта (опционально)
sourceRepos:                                      # внешние репозитории (опционально)
  - url: https://git.example.com/org/infra.git
    branch: main
    path: rendered/argocd/applications
```

`projectNamespace` — признак gitops-репозитория. Если задан, bootstrap-приложение создаётся в этом namespace, а рендер требует наличия `projects/root-project.yaml`. Если не задан (репозиторий приложения) — конфиг argocd не требуется.

### app.yaml (приложение)

```yaml
chartName: universal-helm-chart       # имя чарта в charts/
namespace: production                 # целевой namespace
project: production                   # ArgoCD project (опционально, по умолчанию из main.yaml)
syncWave: "10"                        # sync wave (опционально, по умолчанию "10")
ignoreDifferences:                    # ignoreDifferences для Application (опционально)
  - group: apps
    kind: Deployment
    jsonPointers:
      - /spec/replicas
encryptKinds:                         # типы ресурсов для SOPS-шифрования
  - secret
```

#### Примеры app.yaml

Обычное приложение:
```yaml
chartName: universal-helm-chart
namespace: production
```

Приложение с SOPS-секретами:
```yaml
chartName: universal-helm-chart
namespace: production
encryptKinds:
  - secret
```

Namespace (через kubernetes-resources):
```yaml
chartName: kubernetes-resources
namespace: production
syncWave: "0"
```

RBAC доступы:
```yaml
chartName: kubernetes-resources
namespace: default
syncWave: "1"
```

AppProject:
```yaml
chartName: kubernetes-resources
namespace: argocd-system
project: default
syncWave: "-10"
```

#### Кастомизация Application CR

Секция `application` в app.yaml позволяет управлять параметрами генерируемого Application CR. Все параметры опциональны — если не указаны, используются дефолты.

```yaml
chartName: universal-helm-chart
namespace: production
application:
  prune: false                                  # по умолчанию зависит от типа ресурса (см. ниже)
  selfHeal: true                                # по умолчанию true
  syncOptions:                                  # по умолчанию [ServerSideApply=true, RespectIgnoreDifferences=true]
    - ServerSideApply=true
  finalizers:                                   # по умолчанию [resources-finalizer.argocd.argoproj.io]
    - resources-finalizer.argocd.argoproj.io
    - custom-finalizer.example.com
```

Дефолт `prune` зависит от типа ресурса:

| Тип ресурса | `prune` по умолчанию | Причина |
|-------------|---------------------|---------|
| namespace | `false` | prune удалит неймспейс со всем содержимым (PVC, Secret, Deployment) |
| приложение (`apps/`) | `true` | устаревшие ресурсы очищаются при обновлениях релиза |
| rbac | `true` | аккумулируется из всех файлов, синхронизация должна сходиться |
| networkpolicy | `true` | устаревшие политики должны удаляться |
| repo bootstrap | `false` | хардкод, не должен пруниться |

Остальные дефолты:

| Параметр | По умолчанию |
|----------|-------------|
| `selfHeal` | `true` |
| `syncOptions` | `["ServerSideApply=true", "RespectIgnoreDifferences=true"]` |
| `finalizers` | `["resources-finalizer.argocd.argoproj.io"]` |

Явное значение `prune` в `app.yaml` всегда перекрывает дефолт.

Примеры:

Отключить prune для критичного приложения (по умолчанию для apps это `true`):
```yaml
chartName: universal-helm-chart
namespace: production
application:
  prune: false
```

Без finalizer (ресурсы не удаляются при удалении Application):
```yaml
chartName: universal-helm-chart
namespace: production
application:
  finalizers: []
```

Кастомные syncOptions:
```yaml
chartName: universal-helm-chart
namespace: production
application:
  syncOptions:
    - ServerSideApply=true
    - CreateNamespace=true
    - RespectIgnoreDifferences=true
```

#### Кастомизация инфраструктурных Application CR

Для инфраструктурных ресурсов (namespaces, rbac, networkpolicy) также доступна кастомизация Application CR. Создай файл `app.yaml` в соответствующей директории:

```
projects/<stage>/
├── networkpolicy/
│   ├── app.yaml          ← кастомизация syncPolicy
│   ├── deny-all.yaml
│   └── deny-all-rvc1.yaml
├── rbac/
│   ├── app.yaml          ← кастомизация syncPolicy
│   └── ...
└── namespaces/
    ├── app.yaml          ← кастомизация syncPolicy
    └── ...
```

Формат `app.yaml` для инфры — тот же, что и для приложений:

```yaml
prune: true
selfHeal: true
syncOptions:
  - ServerSideApply=true
finalizers: []
```

Пример — включить prune для namespace (по умолчанию `false`, т.к. prune неймспейса удаляет всё внутри):
```yaml
# projects/production/namespaces/app.yaml
prune: true
```

Пример — кастомные finalizers для rbac:
```yaml
# projects/production/rbac/app.yaml
finalizers: []
syncOptions:
  - ServerSideApply=true
```

Дефолтные значения (если `app.yaml` отсутствует):

| Параметр | По умолчанию |
|----------|-------------|
| `prune` | `false` для namespaces, `true` для rbac/networkpolicy |
| `selfHeal` | `true` |
| `syncOptions` | `["ServerSideApply=true", "RespectIgnoreDifferences=true"]` |
| `finalizers` | `["resources-finalizer.argocd.argoproj.io"]` |

### values.yaml (для kubernetes-resources)

Namespace:
```yaml
namespace:
  name: production
  labels:
    env: production
  quota:
    cpu: "8"
    memory: "16Gi"
```

RBAC:
```yaml
rbac:
  groups:
    - name: devs
      clusterRoles:
        - cr-list-namespaces
      namespaces:
        - name: production
          roles:
            - name: view
              kind: ClusterRole
```

AppProject:
```yaml
project:
  name: production
  namespace: argocd-system
  description: Production environment
  sourceRepos:
    - "*"
  destinations:
    - namespace: "*"
      server: "*"
  namespaceResourceWhitelist:
    - group: "apps"
      kind: "Deployment"
    - group: ""
      kind: "ConfigMap"
```

## Makefile

### Сборка

| Цель | Описание |
|------|----------|
| `make build` | Сборка под текущую платформу |
| `make build-linux-amd64` | Кросс-компиляция Linux x86_64 |
| `make build-linux-arm64` | Кросс-компиляция Linux ARM64 |
| `make build-darwin-arm64` | Кросс-компиляция macOS ARM64 |
| `make build-all` | Все три платформы |
| `make tidy` | `go mod tidy` |
| `make clean` | Удалить `build/` |

### Docker

| Цель | Описание |
|------|----------|
| `make image` | Docker-образ linux/amd64 |
| `make image-arm64` | Docker-образ linux/arm64 |
| `make image-all` | Оба Docker-образа |
| `make push` | Push amd64-образа в registry |
| `make push-arm64` | Push arm64-образа в registry |
| `make push-all` | Push обоих образов |

### Версионирование

`make release` читает целевую версию **из верхней записи CHANGELOG.md** (`## vX.Y.Z`) — так тег и CHANGELOG всегда согласованы. Защиты: отказ если тег уже существует, отказ если версия не больше текущей, интерактивное подтверждение `[y/N]` (или `CONFIRM=1` для CI).

| Цель | Описание |
|------|----------|
| `make current-version` | Показать текущую версию |
| `make tag-list` | Показать последние 10 тегов |
| `make release` | Релиз по версии из верхи CHANGELOG.md (рекомендуется) |
| `make release-patch` | Принудительный patch-bump v0.3.10 → v0.3.11 |
| `make release-minor` | Принудительный minor-bump v0.3.10 → v0.4.0 |
| `make release-major` | Принудительный major-bump v0.3.10 → v1.0.0 |
| `make patch` / `make minor` / `make major` | Обратная совместимость — алиасы для `release-*` |

Пример workflow:
```bash
# 1. Поправить код и добавить запись в CHANGELOG.md:
#       ## v0.4.1
#       ### Fixed
#       - ...
make release
# current: v0.4.0
# release: v0.4.1
# action:  commit + tag v0.4.1 + push (branch and tag)
# Proceed? [y/N] y
# Released v0.4.1

# Для CI (без подтверждения)
make release CONFIRM=1

# Кастомное сообщение коммита релиза
make release MSG="release v0.4.1: fix helm dep cache"

# Принудительный бамп (CHANGELOG не обновлён / нужен нестандартный шаг)
make release-minor   # v0.4.0 → v0.5.0

# Собрать Docker-образ с версией из тега
make image TAG=$(make current-version) push
```

### Переменные

| Переменная | По умолчанию | Описание |
|-----------|--------------|----------|
| `IMAGE` | `argocd-render` | Полный путь к Docker-образу |
| `TAG` | `latest` | Тег Docker-образа |
| `VERSION` | git describe | Версия бинарника (автоматически из git-тега) |
| `BINARY` | `argocd-render` | Имя бинарника |
| `BUILDDIR` | `build` | Директория для сборки |

### Версионирование

Версия берётся из git-тега через `git describe --tags --always --dirty`:

```bash
# Посмотреть текущую версию
make current-version
# v0.1.0

# Список тегов
make tag-list

# Без тегов — хэш коммита
make current-version
# abc1234

# С тегом — semver
make patch && make current-version
# v0.1.1

# Незакоммиченные изменения — dirty
make current-version
# v0.1.1-dirty
```

## Зависимости

Рантайм (вызываются через CLI):
- `helm` — рендеринг чартов
- `sops` — шифрование/дешифрование секретов

Сборка:
- Go 1.24+
- Docker (для сборки образов)
