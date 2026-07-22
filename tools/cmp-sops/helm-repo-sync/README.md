# helm-repo-sync

Go CLI-бинарь, который синхронизирует ArgoCD repository-Secret'ы в helm
`repositories.yaml`. Предназначен для запуска внутри CMP sidecar в
`argocd-repo-server` перед `helm dep build`, чтобы приватные helm-зависимости
резолвились с теми же credentials, что уже настроены в ArgoCD Repositories.

## Контекст проблемы

ArgoCD v3 не прокидывает repository-credentials в CMP sidecar (нативно —
только Git через `provideGitCreds`). Поэтому `helm dep build` в плагине падает
на `401`/`403` для приватных helm-репозиториев. `helm-repo-sync` закрывает этот
провал для HTTP/HTTPS helm-репо: читает Secret'ы с label
`argocd.argoproj.io/secret-type=repository` (где `type: helm`, не OCI) через
in-cluster Kubernetes API и пишет helm-совместимый `repositories.yaml`.

## Что делает

1. Проверяет TTL: если `repositories.yaml` свежий — выход без работы.
2. Читает токен/CA/namespace из serviceaccount mount.
3. Запрашивает `/api/v1/namespaces/<ns>/secrets?labelSelector=...`.
4. Фильтрует по `type == "helm"` && `enableOCI != "true"`.
5. Атомарно пишет `repositories.yaml` (mode `0600`, директория `0700`).

Credentials не покидают pod (emptyDir sidecar) и не логируются — в логе только
имя репозитория и URL.

## Переменные окружения

| Переменная | По умолчанию | Описание |
|-----------|--------------|----------|
| `CMP_HELM_REPO_SYNC_TTL` | `86400` | Секунды жизни кэша. В течение TTL повторные запуски пропускаются |
| `CMP_HELM_REPO_SYNC_OUT` | `/tmp/helm-config/repositories-<project>.yaml` | Путь к выходному файлу (по умолчанию per-project; override для debug) |
| `CMP_HELM_REPO_SYNC_NAMESPACE` | из `/var/run/secrets/.../namespace` | Namespace для чтения Secret'ов |
| `CMP_HELM_REPO_SYNC_API_HOST` | `kubernetes.default.svc` | Адрес Kubernetes API |
| `CMP_HELM_REPO_SYNC_INSECURE` | `0` | `1`/`true` — отключить проверку TLS (только debug) |

## RBAC

Бинарю нужен ServiceAccount с правом `get`/`list` Secret'ов в namespace
ArgoCD. Проще всего выдать его через блок `repoServer.rbac` в helm-values чарта
`argo-cd` — чарт сам создаст Role и привяжет её к ServiceAccount repo-server'а:

```yaml
repoServer:
  rbac:
    - apiGroups: [""]
      resources: ["secrets"]
      verbs: ["get", "list"]
    - apiGroups: ["argoproj.io"]
      resources: ["appprojects"]
      verbs: ["get"]
```

`appprojects get` нужен для чтения `spec.sourceRepos` — allowlist'а, по которому
фильтруются credentials и валидируются зависимости (см. «Security» ниже).

При желании доступ к Secret'ам можно сузить через `resourceNames` до конкретных
repository-Secret'ов.

## Security: sourceRepos filtering

helm-repo-sync закрывает два аспекта sourceRepos bypass, присущего CMP sidecar:

1. **Credential isolation.** Читаются только repository-Secret'ы (label
   `argocd.argoproj.io/secret-type=repository`), и только те, чей URL разрешён
   в `spec.sourceRepos` AppProject текущего приложения (`ARGOCD_APP_PROJECT_NAME`).
   Проект A не получает credentials приватного helm-репо проекта B. Кэш
   `repositories.yaml` — per-project (`/tmp/helm-config/repositories-<project>.yaml`),
   поэтому приложения разных проектов не разделяют credentials.

2. **Валидация Chart.yaml dependencies.** С флагом `--chart <dir>` helm-repo-sync
   парсит `dependencies:` и проверяет каждый `repository:` URL против sourceRepos.
   Зависимость от неразрешённого helm-репо (включая публичные) → fail-closed
   (exit 1) до запуска `helm dep build`.

**Fail-closed:** если AppProject не найден, `sourceRepos` пуст или K8s API
недоступен — helm-repo-sync отказывается материализовать credentials. dep build
падает с понятной ошибкой вместо silent bypass.

**Мгновенная инвалидация кэша:** repositories.yaml кэшируется per-project с TTL
(`CMP_HELM_REPO_SYNC_TTL`), но изменения `sourceRepos` применяются **немедленно**
— helm-repo-sync хранит `resourceVersion` AppProject (файл `.rv` рядом с
repositories.yaml) и при каждом запуске сверяет его. Если AppProject изменился
(например, оператор убрал репо из sourceRepos) — кэш инвалидируется, repositories.yaml
перестраивается, даже если TTL не истёк. AppProject GET выполняется всегда; при
его сбое — fail-closed.

**Matching:** точное совпадение URL (string equality); спецзначение `*` в
sourceRepos разрешает любой URL.

**Ограничения:**
- Транзитивные subchart-зависимости (subchart своего subchart'а) не валидируются
  — они не видны в верхнем `Chart.yaml`. Используйте vendored зависимости
  (`charts/*.tgz`) для полного контроля.
- Для работы политики Devops должен прописать `sourceRepos` во все AppProject,
  чьи приложения используют helm-зависимости (через `projectSourceRepos` в
  main.yaml argocd-render).

Альтернатива — raw-манифест (если ArgoCD ставится не через helm-чарт):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: argocd-cmp-helm-repo-sync
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list"]
  - apiGroups: ["argoproj.io"]
    resources: ["appprojects"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: argocd-cmp-helm-repo-sync
subjects:
  - kind: ServiceAccount
    name: argocd-repo-server   # ServiceAccount, с которым крутится repo-server
roleRef:
  kind: Role
  name: argocd-cmp-helm-repo-sync
  apiGroup: rbac.authorization.k8s.io
```

## Сборка

Встроена в `../Dockerfile` как первая multi-stage стадия (`build-helm-repo-sync`).
Локально для проверки:

```sh
cd tools/cmp-sops/helm-repo-sync
go build -o /tmp/helm-repo-sync .
```

Зависимостей нет — только stdlib.

## Покрытие и ограничения

- ✅ HTTP/HTTPS helm-репозитории (`type: helm`, не OCI)
- ⏭️ OCI-registry — пропускается (нужен `helm registry login`, отдельная задача)
- ⏭️ Git-сабчарты — пропускаются (покрыты нативным `provideGitCreds`)
