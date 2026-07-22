# Changelog

## v0.4.0

### Added
- **CMP sidecar для ArgoCD repo-server** (`tools/cmp-sops/`) — рендерит helm-чарты с on-the-fly дешифровкой SOPS-секретов прямо в кластере. argocd-render помечает приложения с `secrets*.yaml` как `source.plugin: {name: sops}`, и этот sidecar выполняет фактический рендер
  - `sops-generate.sh` — CMP-генератор: три режима (argocd-render / standard helm / full-render), multi-env и single-env values, helm dep build с retry
  - `Dockerfile` — образ на базе `argocd:v3.4.4` с sops/helm/helm-secrets/yq
  - README по подключению (RBAC через `repoServer.rbac`, CMP plugin config, SOPS_AGE_KEY)
- **`helm-repo-sync`** (`tools/cmp-sops/helm-repo-sync/`) — Go CLI, закрывает архитектурное ограничение ArgoCD v3 (CMP sidecar не получает repository-credentials). Читает ArgoCD repository-Secret'ы (`type: helm`, не OCI) через in-cluster Kubernetes API и пишет helm-совместимый `repositories.yaml`. TTL-кэширование, base64-декодирование данных Secret'ов, atomic-write (0600), credentials не логируются
- Утилиты локального шифрования SOPS (`tools/sops/`) — `make encrypt/decrypt` + пример `.sops.yaml`

### Changed
- Структура `tools/` перегруппирована: `tools/cmp-sops/` (образ sidecar) и `tools/sops/` (локальные утилиты)
- `tools/repo-sync` → `tools/cmp-sops/helm-repo-sync`; бинарник, Go-модуль, log-префикс и env-var'ы (`CMP_HELM_REPO_SYNC_*`) переименованы для консистентности
- `tools/Dockerfile.sops-cmp` → `tools/cmp-sops/Dockerfile`
- В `sops-generate.sh` env-var `ARGOCD_ENV_APP_ENV` → `ARGOCD_ENV_VALUES_ENV` (значение в `source.plugin.env`: `APP_ENV` → `VALUES_ENV`)
- Убраны избыточные флаги `--repository-cache`/`--repository-config` из `helm dep build` — helm берёт пути из `HELM_CACHE_HOME`/`HELM_CONFIG_HOME`; путь `repositories.yaml` приведён к канону helm (`/tmp/helm-config/repositories.yaml`)
- Удалён `tools/repo-rbac.example.yaml` — RBAC описан в README через `repoServer.rbac` чарта `argo-cd`

### Breaking changes
- Application CR с `source.plugin.env: APP_ENV` нужно переименовать в `VALUES_ENV`
- Образ CMP sidecar меняет тег/расположение (Dockerfile переехал в `tools/cmp-sops/`)

## v0.3.11

### Changed
- Makefile: секция версионирования переписана. Общая функция `_release` (commit + tag + push с guard'ом на существующий тег) вместо трёх копий логики. Добавлены явные цели `release-patch` / `release-minor` / `release-major`; `release` = алиас для `release-patch`; `patch`/`minor`/`major` сохранены как алиасы для обратной совместимости. Защита от двойного релиза: если тег уже существует — `make release-*` падает с понятной ошибкой

## v0.3.10

### Added
- Флаг `--clean` — автономная команда для очистки артефактов `helm dep` и кэша рендера. Удаляет по каждому чарту всю папку `charts/` (vendored `.tgz` и распакованные subcharts), `Chart.lock` и `.dep.md5`, плюс глобальный `.render-cache/`. Выполняет очистку и завершается, рендер не запускается. Удобно, когда `helm dep build` тащит устаревшие зависимости или кэш повредился

## v0.3.9

### Changed
- Дефолт `prune` теперь зависит от типа ресурса. Раньше все Application CR получали `prune: true`, что опасно для инфраструктуры — automated prune удаляет любой ресурс, пропавший из манифеста (PVC, Secret, namespace со всем содержимым). Теперь:
  - **namespace** → `prune: false` (защита от удаления неймспейса с данными)
  - **приложения** (`apps/`) → `prune: true` (как и раньше — устаревшие ресурсы очищаются)
  - **rbac, networkpolicy** → `prune: true` (без изменений)
  - **repo bootstrap** → `prune: false` (хардкод, без изменений)
- Явное значение `prune` в `app.yaml` всегда перекрывает дефолт — приоритет конфига сохранён
- `applyAppSettings` получил третий параметр `defaultPruneOverride *bool` для пиннинга дефолта per-category

## v0.3.8

### Added
- AppProject передаёт все resource whitelist/blacklist (`clusterResourceWhitelist`, `clusterResourceBlacklist`, `namespaceResourceWhitelist`, `namespaceResourceBlacklist`) из `main.yaml` stage в шаблон проекта

## v0.3.7

### Changed
- `--encrypt`/`--decrypt` обрабатывают **только файлы секретов** (имя начинается с `secrets`, `.yaml`/`.yml`). Остальные YAML (`app.yaml`, `values.yaml`, `.sops.yaml`) больше не трогаются — раньше шифрование по папке приложения ломало метаданные, которые рендер читает как открытый текст. Поддерживается несколько `secrets*` файлов в одной папке (`secrets.yaml`, `secrets-db.yml` и т.п.)
- В выводе `--encrypt`/`--decrypt` показывается корректный относительный путь к файлу (раньше мог быть пустым)
- При отсутствии `secrets*` файлов выводится `No secrets* files found`

## v0.3.6

### Fixed
- AppProject-файлы (`rendered/argocd/projects/<stage>.yaml`) больше не затираются при рендере нескольких стейджей: `renderProject` перестал вызывать `os.RemoveAll` для общей директории проектов в каждом стейдже. Раньше выживал только AppProject последнего отрендеренного стейджа
- Корректный выбор свежесгенерированного файла проекта (из kind-поддиректории) вместо уже записанных проектов соседних стейджей — устранена гонка, когда имя внутри YAML не совпадало с именем файла
- AppProject генерируется **для каждого стейджа всегда** — убран жёсткий триггер по `namespaceResourceWhitelist`/`sourceRepos` в `main.yaml`. Раньше стейдж без этих полей не получал AppProject
- Гарантирован непустой `sourceRepos`: если `repoUrl` стейджа пуст, берётся `argocd.root-repo-url` из config (шаблон `project.yaml` рендерится только при непустом `sourceRepos`)
- Cleanup устаревших AppProject: при рендере всех стейджей удаляются `projects/<stage>.yaml` для стейджей, которых больше нет. Работает в обоих режимах, не срабатывает при фильтре `--stage`
- Корневой AppProject не генерируется: стейдж, имя которого **точно совпадает** со значением `argocd.root-project` из config, пропускается в `renderProject` (ни AppProject, ни Application CR для него не создаются). Устаревший файл такого стейджа также удаляется cleanup
- Убрана генерация Application CR `*-project.yaml` для каждого стейджа: раскатка AppProject теперь лежит на едином bootstrap-Application (deployed ansible'ом через multi-source на `rendered/argocd/projects`). AppProject-файлы в `rendered/argocd/projects/<stage>.yaml` продолжают генерироваться как обычно. Устаревшие `*-project.yaml` удаляются cleanup, т.к. `project` больше не попадает в `activeApps`

## v0.3.5

### Fixed
- Команда `--init` теперь корректно работает со стейджем, переданным через флаг `--stage`: `argocd-render --init --stage sft-prod` создаёт стейдж вместо падения в обычный рендер
- Комбинация `--init --init-config --stage <name>` создаёт и `projects/root-project.yaml`, и стейдж (раньше после `--init-config` обработка прерывалась, и стейдж не создавался)
- `cmdInitConfig` больше не вызывает `os.Exit` в составе init-потока: при существующем `root-project.yaml` выводится предупреждение и инициализация продолжается. Поведение одиночного `--init-config` (включая `exit 1` при повторе) сохранено
- Удалён мёртвый дублирующий блок обработки `--init`

### Added
- Примеры `--init --stage test` и `--init --init-config --stage test` в справку (`--help`)

## v0.3.4

### Added
- Поддержка `.yml` расширения для всех YAML-файлов (`main`, `app`, `values`, `secrets`, `root-project`): автоматически выбирается существующий файл, приоритет `.yaml`
- Хелперы `yamlPath`, `yamlChartRelPath`, `chartValuesFile` для определения расширений и путей

### Fixed
- Пути в `valueFiles` теперь строятся относительно chart path (через `../../projects/...`), чтобы ArgoCD корректно резолвил файлы вне директории чарта
- В `valueFiles` первым добавляется `values.yaml`/`values.yml` из чарта, затем values из проекта
- Поддержка `values.yml` в chart dir (раньше жёстко прописывался `values.yaml`)

## v0.3.3

### Added
- Поддержка `app.yaml` для кастомизации инфраструктурных приложений (namespace/rbac/networkpolicy)
