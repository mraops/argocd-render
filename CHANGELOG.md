# Changelog

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
