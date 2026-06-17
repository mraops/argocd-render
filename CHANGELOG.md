# Changelog

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
