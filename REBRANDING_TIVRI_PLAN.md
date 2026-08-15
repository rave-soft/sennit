# План ребрендинга Braid в Tivri

## Цель

Полностью перевести продукт, CLI, пользовательские интерфейсы, документацию и
каналы поставки с бренда Braid на Tivri, сохранив пользовательские настройки,
историю, автоматизацию и возможность безопасного обновления существующих
установок.

Ребрендинг считается миграцией публичных контрактов, а не глобальной заменой
строк: имя Braid используется в путях, переменных окружения, hook API, именах
инструментов, URI, базе данных, IPC, Go module path и пакетах дистрибуции.

## Решения, которые нужно принять до реализации

- [ ] Подтвердить написание бренда: `Tivri` в тексте, `tivri` для CLI и файлов,
  `TIVRI` для переменных окружения.
- [ ] Определить новый GitHub repository и Go module path, предположительно
  `github.com/rave-soft/tivri`.
- [ ] Определить основной домен, URL документации, schema URL, support и issue
  tracker.
- [ ] Утвердить владельцев и имена пакетов для Homebrew, npm, AUR, Scoop, Nix,
  Winget, deb/rpm/apk и других каналов.
- [ ] Утвердить логотип, terminal wordmark, иконку, палитру и короткое описание
  продукта.
- [ ] Зафиксировать срок поддержки старого имени `braid` и старых публичных
  контрактов. Рекомендуемый минимум: один стабильный релизный цикл.
- [ ] Решить судьбу старого репозитория: rename с redirect либо архивный
  репозиторий с указателем на Tivri.

## Принципы миграции

1. Сначала добавить совместимость и перенос данных, затем переключать значения
   по умолчанию на Tivri.
2. Новые имена имеют приоритет, старые читаются как fallback и вызывают одно
   понятное предупреждение о deprecated contract.
3. Миграции данных должны быть идемпотентными, атомарными и покрыты тестами.
4. Нельзя слепо заменять `Charm` и `charm.land`: часть таких упоминаний относится
   к зависимостям, лицензиям и upstream attribution.
5. Generated-файлы нужно обновлять генераторами, а не ручной заменой.

## Этап 1. Зафиксировать карту бренда и контракты

- [ ] Создать таблицу `старое имя -> новое имя -> срок совместимости` для всех
  публичных идентификаторов.
- [ ] Разделить упоминания Braid на категории: пользовательский бренд,
  совместимость, историческая атрибуция, тестовые fixtures, third-party данные.
- [ ] Добавить централизованные константы имени приложения, CLI, URL, User-Agent
  и имен файлов там, где сейчас используются строковые литералы.
- [ ] Зафиксировать baseline: полный `go test ./...`, build и текущий список
  артефактов GoReleaser.

Основные зоны инвентаризации:

- `go.mod`, Go imports, linker flags в `Taskfile.yaml` и `.goreleaser.yml`;
- `internal/cmd/`, `internal/config/`, `internal/db/`, `internal/hooks/`;
- `internal/shell/`, `internal/skills/`, `internal/agent/tools/`;
- `internal/ui/`, `internal/oauth/`;
- `README.md`, `docs/`, `schema.json`, builtin skills и agent prompts;
- packaging и release-конфигурация.

## Этап 2. Добавить слой обратной совместимости

Этот этап должен попасть в релиз под старым или переходным именем до удаления
старых путей.

### CLI и публичные имена

- [ ] Выпускать основной бинарник `tivri`.
- [ ] На переходный период оставить `braid` как wrapper, symlink или отдельный
  compatibility artifact с предупреждением о переименовании.
- [ ] Определить aliases для публичных tools `braid_info` и `braid_logs` либо
  версионированно заменить их на `tivri_info` и `tivri_logs`.
- [ ] Поддержать чтение старого URI `braid://skills/...` вместе с новым
  `tivri://skills/...`.
- [ ] Сохранить aliases для builtin skills `braid-config` и `braid-hooks`, если
  их имена доступны пользователям или моделям.

### Переменные окружения и hooks

- [ ] Ввести `TIVRI_*` для всех runtime, server, test и config variables.
- [ ] Реализовать приоритет `TIVRI_*` над `BRAID_*`; конфликт значений должен
  диагностироваться.
- [ ] В hook payload добавить `TIVRI_EVENT`, `TIVRI_TOOL_NAME`,
  `TIVRI_SESSION_ID`, `TIVRI_CWD`, `TIVRI_PROJECT_DIR` и tool input variables.
- [ ] В переходный период экспортировать и старые `BRAID_*`, чтобы существующие
  hook-скрипты продолжали работать.
- [ ] Обновить marker variables до `TIVRI=1`, `AGENT=tivri`, `AI_AGENT=tivri`,
  сохранив необходимый legacy marker.
- [ ] Учесть специальную логику, где любое `BRAID_<NAME>` сейчас становится
  override для `<NAME>`.

### Config и context discovery

- [ ] Сделать новыми canonical именами `.tivri/`, `tivrirc`, `.tivrirc`,
  `tivri.json`, `.tivri.json` и `.tivriignore`.
- [ ] Сохранить dual-read для `.braid/`, `braidrc`, `.braidrc`, `braid.json`,
  `.braid.json` и `.braidignore`.
- [ ] Явно задокументировать precedence при одновременном наличии Tivri и
  Braid config; новые файлы должны побеждать.
- [ ] Поддержать старые `.braid/agents`, `.braid/skills`, глобальные skills и
  `~/.braid/commands`.
- [ ] Добавить `TIVRI.md`, casing и `.local` variants в context discovery,
  временно продолжая читать `BRAID.md` variants.
- [ ] Обновить `$TIVRI_VERSION` в shell config с fallback на `$BRAID_VERSION`.

## Этап 3. Мигрировать пользовательские данные и IPC

- [ ] Перенести глобальный config/data/cache из каталогов `braid` в `tivri` на
  Unix, Windows и при использовании XDG overrides.
- [ ] Перенести `braid.db`, логи, credentials/state и project registry без
  потери данных.
- [ ] Определить поведение для project state `.braid/`: автоматический rename,
  copy-on-first-run или длительный dual-read. Предпочтительно не менять рабочее
  дерево без явного согласия пользователя.
- [ ] Учесть существующую legacy-миграцию `.braid/braid.db` и не запускать
  импорт повторно.
- [ ] Добавить migration marker/version и атомарное восстановление после
  прерванного переноса.
- [ ] Обработать конфликт, если старый и новый профили уже содержат данные;
  ничего не перезаписывать молча.
- [ ] Защититься от одновременного запуска Braid и Tivri на одном профиле.
- [ ] Перевести lock-файлы, panic/server logs, Unix socket/Windows pipe и
  внутренний host `api.braid.localhost` на Tivri с переходной стратегией.
- [ ] Добавить тесты fresh install, upgrade, повторной миграции, конфликта,
  rollback и concurrent startup на поддерживаемых ОС.

Критичные текущие точки: `internal/config/config.go`,
`internal/config/load.go`, `internal/db/connect.go`,
`internal/db/legacy_import.go`, `internal/db/datadirlock.go` и
`internal/projects/projects.go`.

## Этап 4. Переименовать код и технические идентификаторы

- [ ] Переименовать repository и module path в `go.mod`.
- [ ] Обновить все Go imports и module-qualified linker flags.
- [ ] Обновить Cobra root command, usage, help, completion и manpage generation.
- [ ] Переименовать Braid-specific файлы, типы и symbols там, где они являются
  частью актуального бренда, не создавая бессмысленный churn внутренних имен.
- [ ] Обновить application name, User-Agent и HTTP metadata.
- [ ] Обновить `braid://`, tool names и skill identifiers согласно принятой
  политике совместимости.
- [ ] Регенерировать JSON schema и проверить ее `$id` и descriptions.
- [ ] Проверить код и тесты на case-insensitive файловых системах.

## Этап 5. Обновить продуктовый интерфейс

- [ ] Заменить бренд в system prompts и agent templates на Tivri.
- [ ] Обновить terminal header, большой wordmark и small logo под пять букв
  `TIVRI`.
- [ ] Заменить notification icon и app name на всех платформах.
- [ ] Обновить OAuth callback page, title, тексты успешной авторизации и assets.
- [ ] Обновить attribution string `Generated with Braid` на Tivri с учетом
  сохраненных пользовательских настроек.
- [ ] Проверить onboarding, ошибки, doctor output, crash report URL и все
  команды, видимые пользователю.
- [ ] Обновить golden/snapshot tests и добавить smoke-тест отсутствия старого
  имени в актуальном UI.

## Этап 6. Документация и developer experience

- [ ] Переписать `README.md`, install snippets, examples и screenshots.
- [ ] Обновить `AGENTS.md`, repo-local agent definitions и development skills.
- [ ] Обновить документацию config/hooks, включая таблицы precedence и env vars.
- [ ] Добавить отдельный migration guide `Braid -> Tivri` с командами проверки,
  путями данных и способом отката.
- [ ] Обновить sample config, schema references и editor integration.
- [ ] Проверить ссылки, repository URLs, support contacts и issue templates.
- [ ] Сохранить корректную историческую и лицензионную атрибуцию в `NOTICE` и
  `LICENSE.md`; не выдавать upstream code/assets за новые материалы Tivri.
- [ ] Пометить старую документацию и имена deprecated с датой удаления.

## Этап 7. Сборка, упаковка и выпуск

- [ ] Перевести `.goreleaser.yml` на `project_name: tivri`, бинарник `tivri`,
  архивы, checksums, completions и `tivri.1`.
- [ ] Обновить `Makefile`, `Taskfile.yaml`, `.gitignore`, `flake.nix` и dev scripts.
- [ ] Создать или переименовать пакеты Homebrew, npm, AUR, Scoop, Nix, Winget,
  deb/rpm/apk; проверить ownership и signing secrets.
- [ ] Настроить redirects/deprecation notices для старых package names, где это
  поддерживается.
- [ ] Проверить repository URL, release notes URL, license URL и issue tracker
  во всех manifests.
- [ ] Проверить CI/CD вне текущего дерева: workflows, release bots, branch
  protection, badges, webhooks, secrets, container registries и mirrors.
- [ ] Выпустить release candidate во всех поддерживаемых архитектурах и ОС.
- [ ] Проверить установку, upgrade поверх Braid, shell completions, manpage,
  uninstall и rollback для каждого канала.

## Этап 8. Запуск и последующее удаление legacy

- [ ] Опубликовать анонс с причиной переименования, таблицей новых имен,
  migration guide и датами окончания совместимости.
- [ ] В первом Tivri release собирать диагностику ошибок миграции без передачи
  пользовательских данных.
- [ ] Отслеживать обращения по config discovery, hooks, packages, PATH и потере
  истории.
- [ ] Исправить критичные migration bugs до удаления aliases.
- [ ] В объявленном major release удалить `braid` binary alias, `BRAID_*`, старые
  paths и URI только после анализа использования.
- [ ] Оставить понятную ошибку или отдельный migration utility для пользователей,
  пропустивших переходные версии.

## Матрица обязательной совместимости

| Контракт | Новое значение | Переходное поведение |
| --- | --- | --- |
| CLI | `tivri` | Временный alias `braid` |
| Env | `TIVRI_*` | Читать `BRAID_*` как fallback |
| Hooks | `TIVRI_*`, `AGENT=tivri` | Экспортировать legacy vars |
| Config dir | `.tivri`, `~/.config/tivri` | Dual-read старых каталогов |
| Config files | `tivrirc`, `tivri.json` | Читать Braid variants с меньшим приоритетом |
| Context | `TIVRI.md` variants | Читать `BRAID.md` variants |
| Ignore | `.tivriignore` | Читать `.braidignore` |
| Skills URI | `tivri://skills/` | Alias для `braid://skills/` |
| Tools | `tivri_info`, `tivri_logs` | Временные aliases старых имен |
| State/DB | Tivri paths/names | Идемпотентная миграция Braid state |
| Packages | Tivri package names | Deprecated Braid packages/redirects |

## Проверка качества

- [ ] `gofumpt -w .` или `task fmt` не создает незапланированных изменений.
- [ ] `go test ./...` проходит с чистым профилем и с fixtures старого профиля.
- [ ] `go build .` создает рабочий бинарник Tivri.
- [ ] `task lint:fix`/lint проходит без новых suppressions.
- [ ] GoReleaser snapshot создает только ожидаемые артефакты и имена.
- [ ] Тестовая матрица покрывает Linux, macOS и Windows, fresh install и upgrade.
- [ ] Поиск `(?i)braid` классифицирован: каждое оставшееся упоминание является
  compatibility code, migration docs, fixture или юридической атрибуцией.
- [ ] Поиск старых repository/domain/package identifiers не находит случайных
  ссылок в актуальном продукте.
- [ ] Существующие config, agents, skills, sessions, credentials, hooks и project
  history доступны после обновления.
- [ ] Откат на предыдущую версию не повреждает исходные данные.

## Критерии готовности релиза

1. Пользователь устанавливает Tivri с нуля и нигде в штатном UX не видит Braid.
2. Пользователь обновляется с последней Braid без ручного копирования файлов и
   сохраняет настройки, sessions, credentials, agents, skills и hooks.
3. Автоматизация на `BRAID_*` продолжает работать в течение объявленного окна
   совместимости и получает понятное предупреждение.
4. Все официальные пакеты, документация, schema и release assets
   используют согласованные идентификаторы Tivri.
5. Для каждого оставшегося упоминания Braid есть документированная причина и
   дата пересмотра или удаления.

## Основные риски

| Риск | Последствие | Мера снижения |
| --- | --- | --- |
| Простая замена путей | Пустой профиль и «потеря» истории | Dual-read и атомарная миграция |
| Одновременный запуск имен | Повреждение БД/state | Общий migration lock и обнаружение процесса |
| Смена env/hooks без aliases | Поломка пользовательской автоматизации | Переходный экспорт обоих контрактов |
| Смена module path | Сломанные imports и linker flags | Автоматическая замена плюс регенерация |
| Незанятые package names | Невозможность согласованного релиза | Резервирование до публикации кода |
| Слепая замена Charm | Нарушение атрибуции и dependency URLs | Ручная классификация упоминаний |
| Разные имена по каналам | Supply-chain риск и путаница | Единый release manifest и smoke-тесты |

## Рекомендуемое разбиение на релизы

### Release A: подготовительный Braid

- Добавить migration metadata, dual-read, aliases и предупреждения.
- Не менять canonical storage до проверки миграций на реальных fixtures.

### Release B: первый Tivri

- Переключить бренд, CLI, repository/module, UI, docs и package names.
- Оставить слой совместимости и старые данные нетронутыми как источник отката.

### Release C: стабилизация

- Исправить migration edge cases, завершить перенос каналов поставки и измерить
  использование deprecated contracts.

### Следующий major release

- Удалить legacy aliases и старые discovery paths только после объявленного
  срока и подтверждения низкого использования.
