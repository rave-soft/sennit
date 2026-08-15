# План ребрендинга Braid в Wythe

## Обратная совместимость не требуется

Отдельно и явно: **слой обратной совместимости с именем Braid не нужен**.
Это осознанное решение, а не упущение плана.

Из этого следует:

- никаких alias-бинарников `braid`, никакого dual-read конфигов;
- `BRAID_*` переменные окружения не читаются и не экспортируются;
- `.braid/`, `braidrc`, `braid.json`, `.braidignore`, `BRAID.md` больше не
  участвуют в discovery;
- `braid://` URI, `braid_info`/`braid_logs`, skills `braid-config`/`braid-hooks`
  переименовываются без aliases;
- deprecation-предупреждений и окна совместимости нет;
- миграция пользовательских данных из старых каталогов не выполняется
  автоматически (см. «Судьба существующих данных»).

Единственные упоминания Braid, которые остаются в дереве, — историческая и
лицензионная атрибуция (`NOTICE`, `LICENSE.md`), исторические записи в
changelog и явно помеченные fixtures. Всё остальное — чистая замена.

Это радикально упрощает работу: ребрендинг становится согласованным
переименованием публичных контрактов, а не миграцией с двойными путями,
приоритетами и тестовой матрицей upgrade-сценариев.

## Цель

Полностью перевести продукт, CLI, пользовательские интерфейсы, документацию и
каналы поставки с бренда Braid на Wythe.

Имя Braid используется в путях, переменных окружения, hook API, именах
инструментов, URI, базе данных, IPC, Go module path и пакетах дистрибуции —
каждая из этих зон требует отдельного прохода, даже без слоя совместимости.

## Почему Wythe

Wythe — вертикальный слой каменной кладки. Стена собирается из нескольких
wythes, связанных между собой тычковыми рядами: самостоятельные параллельные
слои, работающие как одна конструкция. Это прямое продолжение метафоры Braid и
существующего словаря thread/strand, но без ее главного дефекта.

Имя выбрано по двум жестким критериям.

**Отсутствие пересечений с CLI-инструментами.** Проверены Homebrew, официальные
репозитории Arch, AUR, Debian contents по `bin/wythe`, crates.io, npm, PyPI,
GitHub по точному имени и локальный PATH. Совпадений нет ни одного.

**Короткий уникальный префикс.** По корпусу из 27 045 имен команд (все
исполняемые файлы в PATH плюс формулы и алиасы Homebrew) префикс `wyt`
уникален: три символа и Tab дают завершение. Для сравнения, у `braid` и
рассматривавшегося `chorda` минимальный уникальный префикс равен полному слову
(`brai` конфликтует с `brainfuck`, `chord` — с формулой `chordii`).

Отвергнутые кандидаты и причины зафиксированы отдельно; ключевые: Hawser (AUR-
пакет `hawser` и crate `hawser`, плюс историческое имя git-lfs), Ferrule (crate
— CLI к базам данных), Truss (системный трассировщик в Solaris и FreeBSD),
Sennit (несколько равноправных написаний), Chorda (префикс `ch` — 261 команда).

Написание бренда: `Wythe` в тексте, `wythe` для CLI и файлов, `WYTHE` для
переменных окружения. Длина совпадает с `BRAID` — пять букв.

## Судьба существующих данных

Автоматической миграции нет. Нужно выбрать одно поведение и задокументировать
его:

- [ ] Вариант A (рекомендуемый): Wythe стартует с чистым профилем в новых
  каталогах, старые каталоги Braid не читаются и не удаляются. В release notes
  и migration guide даются ручные команды переноса.
- [ ] Вариант B: одноразовая явная команда `wythe migrate-from-braid`,
  запускаемая пользователем вручную, вне обычного старта.

В обоих вариантах Wythe ничего не удаляет и не перезаписывает в старом
профиле — откат на Braid всегда возможен простым запуском старого бинарника.

## Решения, которые нужно принять до реализации

- [x] Имя и написание бренда утверждены — см. «Почему Wythe».
- [ ] Определить новый GitHub repository и Go module path, предположительно
  `github.com/rave-soft/wythe`.
- [ ] Определить основной домен, URL документации, schema URL, support и issue
  tracker.
- [ ] Утвердить владельцев и имена пакетов для Homebrew, npm, AUR, Scoop, Nix,
  Winget, deb/rpm/apk и других каналов.
- [ ] Утвердить логотип, terminal wordmark, иконку, палитру и короткое описание
  продукта.
- [ ] Выбрать вариант A или B в разделе «Судьба существующих данных».
- [ ] Решить судьбу старого репозитория: rename с redirect либо архивный
  репозиторий с указателем на Wythe.

## Принципы

1. Одномоментное переключение: новые имена становятся единственными в том же
   релизе, промежуточных состояний нет.
2. Нельзя слепо заменять `Charm` и `charm.land`: часть таких упоминаний относится
   к зависимостям, лицензиям и upstream attribution.
3. Generated-файлы нужно обновлять генераторами, а не ручной заменой.
4. Историческая и лицензионная атрибуция не переписывается.

## Этап 1. Зафиксировать карту бренда

- [ ] Создать таблицу `старое имя -> новое имя` для всех публичных
  идентификаторов.
- [ ] Разделить упоминания Braid на категории: пользовательский бренд,
  историческая атрибуция, тестовые fixtures, third-party данные. Всё, что не
  атрибуция и не third-party, подлежит замене.
- [ ] Добавить централизованные константы имени приложения, CLI, URL, User-Agent
  и имен файлов там, где сейчас используются строковые литералы. Это главная
  страховка от пропусков при сплошной замене.
- [ ] Зафиксировать baseline: полный `go test ./...`, build и текущий список
  артефактов GoReleaser.

Основные зоны инвентаризации:

- `go.mod`, Go imports, linker flags в `Taskfile.yaml` и `.goreleaser.yml`;
- `internal/cmd/`, `internal/config/`, `internal/db/`, `internal/hooks/`;
- `internal/shell/`, `internal/skills/`, `internal/agent/tools/`;
- `internal/ui/`, `internal/oauth/`;
- `README.md`, `docs/`, `schema.json`, builtin skills и agent prompts;
- packaging и release-конфигурация.

## Этап 2. Публичные контракты

### CLI и публичные имена

- [ ] Выпускать единственный бинарник `wythe`.
- [ ] Переименовать публичные tools в `wythe_info` и `wythe_logs`.
- [ ] Перевести skills URI на `wythe://skills/...`.
- [ ] Переименовать builtin skills `braid-config` и `braid-hooks` в
  `wythe-config` и `wythe-hooks`.

### Переменные окружения и hooks

- [ ] Перевести все runtime, server, test и config variables на `WYTHE_*`.
- [ ] В hook payload передавать `WYTHE_EVENT`, `WYTHE_TOOL_NAME`,
  `WYTHE_SESSION_ID`, `WYTHE_CWD`, `WYTHE_PROJECT_DIR` и tool input variables.
- [ ] Обновить marker variables до `WYTHE=1`, `AGENT=wythe`, `AI_AGENT=wythe`.
- [ ] Перенести специальную логику, где любое `BRAID_<NAME>` становится override
  для `<NAME>`, на префикс `WYTHE_`.
- [ ] Явно указать в migration guide, что пользовательские hook-скрипты,
  завязанные на `BRAID_*`, сломаются и требуют правки.

### Config и context discovery

- [ ] Сделать единственными именами `.wythe/`, `wytherc`, `.wytherc`,
  `wythe.json`, `.wythe.json` и `.wytheignore`.
- [ ] Перевести `.wythe/agents`, `.wythe/skills`, глобальные skills и
  `~/.wythe/commands`.
- [ ] Перевести context discovery на `WYTHE.md`, его casing и `.local` variants.
- [ ] Обновить `$WYTHE_VERSION` в shell config.
- [ ] Убедиться, что старые имена нигде не остались в списках discovery — иначе
  получится молчаливый частичный dual-read.

## Этап 3. Хранилище и IPC

- [ ] Перевести глобальный config/data/cache на каталоги `wythe` на Unix,
  Windows и при использовании XDG overrides.
- [ ] Переименовать `braid.db`, логи, credentials/state и project registry.
- [ ] Пересмотреть существующую legacy-миграцию `.braid/braid.db`: без
  требований совместимости этот код удаляется целиком.
- [ ] Перевести lock-файлы, panic/server logs, Unix socket/Windows pipe и
  внутренний host `api.braid.localhost` на Wythe.
- [ ] Проверить, что Wythe не открывает и не блокирует старые пути, поэтому
  параллельный запуск Braid и Wythe безопасен по построению.
- [ ] Тесты: fresh install на каждой поддерживаемой ОС; upgrade-сценарии не
  нужны.

Критичные текущие точки: `internal/config/config.go`,
`internal/config/load.go`, `internal/db/connect.go`,
`internal/db/legacy_import.go`, `internal/db/datadirlock.go` и
`internal/projects/projects.go`.

## Этап 4. Код и технические идентификаторы

- [ ] Переименовать repository и module path в `go.mod`.
- [ ] Обновить все Go imports и module-qualified linker flags.
- [ ] Обновить Cobra root command, usage, help, completion и manpage generation.
- [ ] Переименовать Braid-specific файлы, типы и symbols, не создавая
  бессмысленный churn внутренних имен.
- [ ] Обновить application name, User-Agent и HTTP metadata.
- [ ] Регенерировать JSON schema и проверить ее `$id` и descriptions.
- [ ] Проверить код и тесты на case-insensitive файловых системах.

## Этап 5. Продуктовый интерфейс

- [ ] Заменить бренд в system prompts и agent templates на Wythe.
- [ ] Обновить terminal header, большой wordmark и small logo под `WYTHE`.
  Длина совпадает с `BRAID` — пять букв, — поэтому ширина и раскладка
  ASCII-логотипа сохраняются, меняются только глифы.
- [ ] Заменить notification icon и app name на всех платформах.
- [ ] Обновить OAuth callback page, title, тексты успешной авторизации и assets.
- [ ] Обновить attribution string `Generated with Braid` на Wythe с учетом
  сохраненных пользовательских настроек.
- [ ] Проверить onboarding, ошибки, doctor output, crash report URL и все
  команды, видимые пользователю.
- [ ] Обновить golden/snapshot tests и добавить smoke-тест отсутствия старого
  имени в актуальном UI.

## Этап 6. Документация и developer experience

- [ ] Переписать `README.md`, install snippets, examples и screenshots.
- [ ] Обновить `AGENTS.md`, repo-local agent definitions и development skills.
- [ ] Обновить документацию config/hooks, включая таблицы precedence и env vars.
- [ ] Написать migration guide, который честно говорит: совместимости нет,
  вот список сломанных контрактов и вот ручные шаги переноса.
- [ ] Обновить sample config, schema references и editor integration.
- [ ] Проверить ссылки, repository URLs, support contacts и issue templates.
- [ ] Сохранить корректную историческую и лицензионную атрибуцию в `NOTICE` и
  `LICENSE.md`; не выдавать upstream code/assets за новые материалы Wythe.

## Этап 7. Сборка, упаковка и выпуск

- [ ] Перевести `.goreleaser.yml` на `project_name: wythe`, бинарник `wythe`,
  архивы, checksums, completions и `wythe.1`.
- [ ] Обновить `Makefile`, `Taskfile.yaml`, `.gitignore`, `flake.nix` и dev scripts.
- [ ] Создать пакеты Homebrew, npm, AUR, Scoop, Nix, Winget, deb/rpm/apk;
  проверить ownership и signing secrets.
- [ ] Пометить старые package names deprecated с указателем на Wythe, где канал
  это поддерживает; redirect-совместимость установки не требуется.
- [ ] Проверить repository URL, release notes URL, license URL и issue tracker
  во всех manifests.
- [ ] Проверить CI/CD вне текущего дерева: workflows, release bots, branch
  protection, badges, webhooks, secrets, container registries и mirrors.
- [ ] Выпустить release candidate во всех поддерживаемых архитектурах и ОС.
- [ ] Проверить установку, shell completions, manpage и uninstall для каждого
  канала.

## Этап 8. Запуск

- [ ] Опубликовать анонс с причиной переименования, таблицей новых имен и явным
  предупреждением о breaking change без окна совместимости.
- [ ] Архивировать или переименовать старый репозиторий с указателем на Wythe.
- [ ] Отслеживать обращения по config discovery, hooks, packages, PATH и
  «пропавшей» истории; отвечать ссылкой на migration guide.

## Таблица переименований

| Контракт | Старое значение | Новое значение |
| --- | --- | --- |
| CLI | `braid` | `wythe` |
| Env | `BRAID_*` | `WYTHE_*` |
| Hooks | `BRAID_*`, `AGENT=braid` | `WYTHE_*`, `AGENT=wythe` |
| Config dir | `.braid`, `~/.config/braid` | `.wythe`, `~/.config/wythe` |
| Config files | `braidrc`, `braid.json` | `wytherc`, `wythe.json` |
| Context | `BRAID.md` variants | `WYTHE.md` variants |
| Ignore | `.braidignore` | `.wytheignore` |
| Skills URI | `braid://skills/` | `wythe://skills/` |
| Tools | `braid_info`, `braid_logs` | `wythe_info`, `wythe_logs` |
| State/DB | Braid paths/names | Wythe paths/names |
| Packages | Braid package names | Wythe package names |

Колонки «переходное поведение» в этой таблице сознательно нет.

## Проверка качества

- [ ] `gofumpt -w .` или `task fmt` не создает незапланированных изменений.
- [ ] `go test ./...` проходит на чистом профиле.
- [ ] `go build .` создает рабочий бинарник Wythe.
- [ ] `task lint:fix`/lint проходит без новых suppressions.
- [ ] GoReleaser snapshot создает только ожидаемые артефакты и имена.
- [ ] Тестовая матрица покрывает Linux, macOS и Windows, fresh install.
- [ ] Поиск `(?i)braid` классифицирован: каждое оставшееся упоминание является
  исторической атрибуцией, migration docs или fixture. Compatibility code в
  этом списке быть не должно.
- [ ] Поиск старых repository/domain/package identifiers не находит случайных
  ссылок в актуальном продукте.

## Критерии готовности релиза

1. Пользователь устанавливает Wythe с нуля и нигде в штатном UX не видит Braid.
2. Все официальные пакеты, документация, schema и release assets используют
   согласованные идентификаторы Wythe.
3. Migration guide явно перечисляет все сломанные контракты.
4. Для каждого оставшегося упоминания Braid есть документированная причина.

## Основные риски

| Риск | Последствие | Мера снижения |
| --- | --- | --- |
| Пропущенный литерал имени | Частично сломанный discovery или UI | Централизованные константы плюс smoke-тест на `(?i)braid` |
| Смена env/hooks без aliases | Поломка пользовательской автоматизации | Принято сознательно; явное предупреждение в анонсе и guide |
| Пользователь теряет доступ к истории | Впечатление потери данных | Старый профиль не удаляется, ручной перенос описан в guide |
| Смена module path | Сломанные imports и linker flags | Автоматическая замена плюс регенерация |
| Незанятые package names | Невозможность согласованного релиза | Резервирование до публикации кода |
| Слепая замена Charm | Нарушение атрибуции и dependency URLs | Ручная классификация упоминаний |
| Разные имена по каналам | Supply-chain риск и путаница | Единый release manifest и smoke-тесты |

## Разбиение на релизы

Многофазная схема с подготовительным релизом больше не нужна: без слоя
совместимости ребрендинг умещается в один релиз.

### Единственный релиз: первый Wythe

- Переключить бренд, CLI, repository/module, хранилище, UI, docs и package names
  одним изменением.
- Опубликовать migration guide одновременно с релизом.

### Последующие патчи

- Исправить пропущенные упоминания и edge cases установки по обратной связи.
