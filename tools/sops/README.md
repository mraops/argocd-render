# sops — утилиты локального шифрования

Локальные `make`-цели для шифрования/дешифрования файлов секретов через
[SOPS](https://github.com/getsops/sops) (age). Применяются разработчиками и в
CI для подготовки `secrets*.yaml` перед коммитом в GitOps-репозиторий —
расшифровка в кластере выполняется CMP sidecar из `../cmp-sops/`.

## Состав

| Файл | Назначение |
|------|-----------|
| `Makefile` | цели `encrypt` / `decrypt` — обходят файлы/папки и применяют `sops -e -i` / `sops -d -i` по правилам из `.sops.yaml` |
| `.sops.example.yaml` | пример `.sops.yaml` с `creation_rules` (path_regex + age-ключ) — скопировать в корень репозитория как `.sops.yaml`, подставив свой age-public-key |

## Требования

- `sops` (v3.13.2 — как в образе CMP)
- `yq` (mikefarah, v4.x — для чтения `path_regex` и проверки `has("sops")`)
- `jq` (только если шифруете JSON-секреты)
- age-private-key в окружении (`SOPS_AGE_KEY` / `SOPS_AGE_KEY_FILE`) — для дешифровки

## Использование

Запускать из корня репозитория (где лежит `.sops.yaml`):

```sh
# Зашифровать все файлы, подпадающие под path_regex из .sops.yaml
make -f tools/sops/Makefile encrypt projects/production/apps/myapp/

# Расшифровать
make -f tools/sops/Makefile decrypt projects/production/apps/myapp/

# Один файл
make -f tools/sops/Makefile encrypt projects/production/apps/myapp/secrets.yaml
```

## Как это работает

1. Читает `path_regex` из `.sops.yaml` (все правила объединяются через `|` в один regex).
2. Обходит цель (файл или директорию рекурсивно), фильтрует файлы по объединённому regex.
3. Для каждого файла проверяет, зашифрован ли он уже:
   - YAML — `yq has("sops")`
   - JSON — `jq has("sops")`
   - прочее — `grep '^sops:'`
4. `encrypt` — пропускает уже зашифрованные, шифрует остальные.
5. `decrypt` — пропускает незашифрованные, расшифровывает остальные.
6. `sops -e -i` / `sops -d -i` — in-place, правила шифрования (age-ключ) берутся из `.sops.yaml`.

Если цель не указана — по умолчанию `.` (текущая директория). Если `.sops.yaml`
отсутствует или в нём нет `path_regex` — `make` падает с понятной ошибкой.

## .sops.yaml

Правила шифрования лежат в `.sops.yaml` в **корне репозитория** (не здесь). Пример —
`.sops.example.yaml`, скопируйте и подставьте свой age-public-key:

```yaml
creation_rules:
  - path_regex: ^vault/.*\.ya?ml$
    age: <AGE_PUBLIC_KEY>
    mac_only_encrypted: true
  - path_regex: secrets.*\.ya?ml$
    age: <AGE_PUBLIC_KEY>
    mac_only_encrypted: true
```

## Связь с argocd-render / CMP sidecar

- `argocd-render` помечает приложения с `secrets*.yaml` как `source.plugin: {name: sops}`.
- CMP sidecar (`../cmp-sops/`) при рендере расшифровывает секреты on-the-fly через `helm-secrets` / `sops -d`.
- Эти `make`-цели — для **локальной/CI** подготовки файлов (шифрование перед коммитом), а не для кластера.
