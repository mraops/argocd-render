# Changelog

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
