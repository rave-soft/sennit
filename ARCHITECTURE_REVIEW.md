# Архитектурное ревью Braid и план рефакторинга

Дата: 2026-08-09. Объём кодовой базы: ~143 000 строк Go, 582 файла.
Метод: пять параллельных ревью по подсистемам (`agent`, `ui`, `config`+`shellconfig`,
каркас `app`/`cmd`/`server`/`backend`/`workspace`, сквозные пакеты), сведённые в этот документ.
Все ссылки вида `файл:строка` актуальны на момент ревью.

> **Статус реализации (2026-08-09, коммиты `91688614`…`d06ce1e4`).** Фазы 0–4 плана
> выполнены: чистка (ф. 1), точечные баги (ф. 2), границы и дедупликация (ф. 3 целиком,
> включая proto-алиасы, `hostaddr`, supervisor, `AgentRunStream`, роль-интерфейсы),
> структурные изменения (ф. 4: per-store каталог, `dispatcher`, `runTurn`+репортер,
> per-workspace MCP/LSP, разрез `configureProviders`, permission-очередь, закрытие
> автоапрува под-агентов, три порции разбора `UI`). Ссылки `файл:строка` в тексте ниже
> соответствуют состоянию ДО этих правок. Открытым осталось: перезапись кассет
> `TestCoderAgent` (нужен LLM-эндпоинт; инфраструктура готова, см. TECHDEBT.md),
> роутер сообщений вместо 707-строчного `Update` (ждёт teatest-сетки), хуки для
> инструментов под-агентов, дубль enum в swagger от алиаса `proto.MessageRole`
> и фаза 5 (отдельные проекты).

---

## 1. Резюме

Braid — зрелый форк Crush с необычно высокой культурой кода: комментарии объясняют
«почему», а не «что»; долг зафиксирован в `TECHDEBT.md` с причинами и следующими шагами;
критичные места (жизненный цикл воркспейсов, OAuth single-flight, дебаунс записи
сообщений) продуманы всерьёз. На этом фоне выделяются **три системные проблемы**,
которые порождают большинство частных находок:

1. **Глобальное состояние процесса против мульти-воркспейсного режима.**
   MCP, LSP-состояния, skills, каталог провайдеров и env-мутации живут в package-level
   переменных, при том что `backend` держит N воркспейсов в одном процессе. Следствия:
   закрытие одного воркспейса глушит MCP-брокер всему серверу, `GetLSPStates(workspaceID)`
   возвращает чужое состояние, второй воркспейс перезатирает MCP-реестр первого.

2. **Два дублирующих пути исполнения** — in-process и client/server. Копии типов
   (`proto.Message` ≈ 700 строк ручного зеркала `message.Message`), копии логики
   (резолв модели, резолв сессии, неинтерактивный прогон) с уже разошедшимся
   поведением. При этом client/server-режим выключен по умолчанию
   (`BRAID_CLIENT_SERVER`, `cmd/root.go:216`) — ~15k строк почти не обкатываются.

3. **Концентрация сложности в god-файлах при отсутствии страховочных тестов.**
   `ui/model/ui.go` — 4 940 строк (Update на 707), `agent.Run` — 757 строк,
   `configureProviders` — 290 строк с сетевым I/O под мьютексом записи. Единственный
   e2e-тест агента (`TestCoderAgent`, 10 сценариев) отключён из-за старых VCR-кассет,
   e2e-тестов TUI нет вовсе. Крупный рефакторинг сейчас пришлось бы делать вслепую.

Отдельно: известный по `TECHDEBT.md` баг «потеря полей провайдера при записи
OAuth-токена» разобран до корня (см. §3.4) — формулировка в TECHDEBT неточна,
реальный механизм другой, и чинится он дешевле, чем предполагалось.

---

## 2. Карта системы

### Слои и направление зависимостей

```
cmd ──► workspace ──► client ──► server ──► backend ──► app ──► agent / lsp / mcp / db
  │         │                                  ▲          ▲
  └─────────┴──────────────────────────────────┴──────────┘
                  proto (wire-типы; импортирует message, lsp, config, agent/tools)
```

Циклов нет, граф ацикличен, но «слипшийся»: `go list -deps ./internal/cmd` даёт все
внутренние пакеты; TUI транзитивно тянет `server`, `backend`, `swagger`.
`internal/config` — пакет-хаб: его импортируют почти все подсистемы.

### Крупнейшие подсистемы

| Пакет | Строк | Роль |
|---|---:|---|
| `internal/ui` | 44 975 | Bubble Tea v2 TUI (`model`, `dialog`, `chat`, `styles`, …) |
| `internal/agent` | 25 646 | Цикл агента, координатор, инструменты, MCP |
| `internal/config` | 11 102 | Загрузка/merge/запись конфига, OAuth refresh, каталог провайдеров |
| `internal/cmd` | 5 477 | Cobra-команды + supervisor демона + аналитика |
| `internal/server` | 5 476 | HTTP/h2c поверх unix-сокета, 80 роутов `/v1/...`, SSE |
| `internal/backend` | 4 945 | Мульти-воркспейсный слой сервера, жизненный цикл |
| `internal/shell` | 4 791 | Встроенный POSIX-интерпретатор (mvdan.cc/sh), jq, coreutils |
| `internal/swagger` | 4 497 | Сгенерирован swaggo/swag, руками не правится |
| `internal/workspace` | 3 538 | Интерфейс `Workspace` (~70 методов), in-process и remote реализации |
| `internal/db` | 2 480 | SQLite (sqlc + goose-миграции), lock на data-dir |

### Файлы-гиганты (нетестовые)

| Файл | Строк |
|---|---:|
| `internal/ui/model/ui.go` | 4 940 |
| `internal/agent/agent.go` | 2 258 |
| `internal/ui/chat/tools.go` | 1 676 |
| `internal/agent/coordinator.go` | 1 636 |
| `internal/config/load.go` | 1 427 |
| `internal/agent/tools/mcp/init.go` | 1 268 |
| `internal/config/store.go` | 1 249 |
| `internal/ui/styles/quickstyle.go` | 1 058 (одна функция на 970 строк) |
| `internal/cmd/root.go` | 988 (~450 строк — supervisor демона) |

---

## 3. Сквозные проблемы

### 3.1. Глобальное состояние против мульти-воркспейса — критично

- `internal/agent/tools/mcp/init.go:67-100` — весь реестр MCP (`sessions`, `states`,
  `broker`, `initOnce`, …) — package-level. Каждый `app.New` (`app/app.go:143-144`)
  вызывает `mcp.ArmInit(); go mcp.Initialize(...)` со своим `ConfigStore` — второй
  воркспейс перезатирает реестр первого.
- `app/app.go:161` кладёт `mcp.Close(ctx)` в `cleanupFuncs`, а `mcp/init.go:276`
  внутри делает `broker.Shutdown()` — **закрытие одного воркспейса глушит MCP всему
  серверу**. Авторы знают частично: `mcp/init.go:214-221` фильтрует одно событие
  с комментарием «cross-workspace injection path», остальные не фильтруются.
- `app/lsp_events.go:39-42` — `lspStates`/`lspBroker` глобальны; ключ — имя LSP.
  `backend/events.go:26-33` принимает `workspaceID`, проверяет его и возвращает
  глобальное состояние чужих воркспейсов. `MCPGetStates(_ string)`
  (`backend/events.go:97-108`) игнорирует workspaceID демонстративно.
- `internal/skills`: двойное состояние — пакетные глобалы `broker`/`latestStates`
  плюс `Manager` с `globalMirror` (`skills/manager.go:124-149`), синхронизируются вручную.
- `internal/config/provider.go:23-27` — каталог провайдеров кэшируется `sync.Once`
  на процесс, хотя зависит от `cfg.Options.DisableDefaultProviders`: первый воркспейс
  фиксирует решение для всех.
- `config/load.go:181-207` — `PushPopBraidEnv` мутирует `os.Environ()` на время
  `configureProviders`: два параллельных воркспейса с разными `BRAID_*` — гонка
  уровня процесса, невидимая для `-race`.
- `coordinator.go:231` блокируется на глобальном `mcp.WaitForInit(ctx)` — MCP второго
  воркспейса не инициализируется повторно.

### 3.2. Дублирование local vs client/server

- **`proto.Message`** (`internal/proto/message.go`, 715 строк) — полная копия
  `message.Message` со всеми аксессорами. Мост — два зеркальных switch по 9 вариантов
  частей: `server/events.go:268-326` и `workspace/client_workspace.go:1276-1336`.
  Новое поле в `message.ToolResult` требует правок в 4 местах и молча теряется,
  если забыть одно. `LSPClientInfo`/`LSPEvent` существуют в **трёх** копиях
  (`app/lsp_events.go:21-37`, `proto/proto.go:291-343`, `workspace/workspace.go:82-105`).
  При этом `proto/tools.go:1-8` уже применяет правильный паттерн (алиасы канонических
  типов) — но только к параметрам инструментов.
- **Резолв модели** — близнецы `app/provider.go` и `cmd/run.go:533-607` с реальными
  расхождениями: `synthetic/moonshot/kimi-k2` парсится только локальной версией;
  сравнение провайдера `EqualFold` vs точное `!=`; разные тексты ошибок.
- **`resolveSession`** — `app/app.go:237-262` vs `cmd/run.go:607-638`; локальная
  версия проверяет `IsAgentToolSession`/`ParentSessionID`, клиентская — нет.
- **`braid run` пробивает абстракцию `Workspace`** (`cmd/run.go:97-160`): в remote-режиме
  работает напрямую через `*client.Client`, в локальном — через приведение типа
  `ws.(*workspace.AppWorkspace)`. `cmd/session.go:107-133` и `cmd/stats.go:138-243`
  сами открывают БД, минуя все слои, — параллельно живому серверу.

### 3.3. Нарушения слоёв

- `app/app.go:42-43` импортирует `ui/anim` и `ui/styles` (спиннер в `RunNonInteractive`);
  `backend/backend.go:508` использует `ui/util.NewWarnMsg` как payload серверного события.
- Событийная шина ядра типизирована UI-фреймворком: `app.events` — `pubsub.Broker[tea.Msg]`,
  `App.Subscribe(program *tea.Program)` (`app/app.go:645-675`).
- `agent.go:33-35` — ядро агента импортирует lipgloss/charmtone и формирует стилизованную
  гиперссылку внутри текста ошибки (`agent.go:1152-1158`), рядом с комментарием
  «The TUI owns the display copy».
- `client` импортирует `server` ради `ParseHostURL`/`DefaultHost` (`client/client.go:19,35`).
- `ui/model` импортирует `app` напрямую в обход `workspace` (`ui.go:918`, `:1323`),
  причём рядом (`ui.go:925`) стоит дублирующий case с типом из `workspace` —
  незавершённая миграция.

### 3.4. Баг потери полей OAuth-провайдера — разбор до корня

Формулировка в `TECHDEBT.md` («запись токена перезаписывает секцию провайдера полями
учётных данных») **неточна**: `writeConfigFields` (`store.go:329-349`) пишет через
`sjson.Set` и чужие ключи не трогает. Реальная цепочка:

1. `refreshOAuthTokenLocked` → `applyToken` кладёт токен в **in-memory** `Config`.
2. `SetConfigFields` пишет на диск только `api_key` + `oauth` (`store.go:673-678`).
3. `SetConfigFields` → `autoReload` → `reloadFromDiskLocked` (`store.go:1105`)
   **пересобирает конфиг с диска целиком** — всё, что жило только в памяти, исчезает.
4. `configureProviders` восстанавливает поля только для провайдеров из встроенного
   каталога (`load.go:222-361`); кастомный провайдер без `base_url`/`models`
   отбраковывается (`load.go:439-464`).

Корень: **полный `ProviderConfig` никогда не персистится** — `SetProviderAPIKey`
собирает полную запись только в память (`store.go:544-567`), на диск уходят два ключа.
Следствия: Copilot фактически **не сломан** (он в каталоге и восстанавливается при
каждом reload); сломан любой OAuth-провайдер вне каталога.

Отключённые тесты `refresh_singleflight_test.go` падают ещё и по **второй, независимой
причине**: фикстура пишет в `<tmp>/crush.json`, а reload читает `lookupConfigs(<tmp>)` —
т.е. реальный `~/.config/braid/braid.json` пользователя (`BRAID_GLOBAL_CONFIG` не
выставлен). Файл `crush.json` при reload не читается вообще. Архитектурный дефект:
`configPath(scope)` (`store.go:267-277`) и набор путей reload (`load.go:897`) — два
независимых источника истины без инварианта «путь записи ⊆ пути чтения».

### 3.5. Конкурентность — точечные дефекты

- `agent.go:1185`, `:1457` — безусловный `activeRequests.Del` там, где основной путь
  использует `CompareAndDelete`: окно, в котором стирается запись более нового запуска.
- Три расходящиеся копии протокола «слить очередь» (`Run` хвост `agent.go:1230-1318`,
  `drainQueueForStep :396-431`, хвост `Summarize :1457-1468`) — последняя не проверяет
  `cancelMark`: отменённые промпты, поставленные в очередь во время суммаризации, выполнятся.
- `csync.Map.GetOrSet` (`csync/maps.go:97-106`) **не атомарен** — `Get`, потом `Set`
  вне общего лока; имя обещает атомарность.
- `permission.Request` держит глобальный `requestMu` **на всё время ожидания ответа
  пользователя** (`permission/permission.go:214-215`) — все параллельные tool-calls
  процесса стоят в одной очереди.
- `pubsub.PublishMustDeliver` (`broker.go:201-236`) блокируется до 50 мс на подписчика
  под `RLock` — инверсия приоритетов для `Subscribe`; per-subscription горутины не
  завершаются после `Shutdown()` — утечка для глобальных брокеров.
- `history.CreateVersion` (`history/file.go:67-134`) — TOCTOU: версия вычисляется вне
  транзакции, залатано retry-петлёй, в которой `err` никогда не присваивается.
- `projects.Register` (`projects/projects.go:78-121`) — `Load`/`Save` берут мьютекс
  порознь (read-modify-write не защищён), запись без temp+rename → усечённый
  `projects.json` при падении.
- Cross-process: конфиг с токенами (`0600`) переписывается целиком любым
  `SetConfigFields` по несекретному полю; секреты и настройки не разделены.

### 3.6. Незавершённый ребрендинг и мёртвый код

- **Hyper в UI** (~250–350 недостижимых строк): `logo.Opts.Hyper` + `hyperLetterforms`
  (нигде не включается), цепочка `UI.hyperCredits` → `header` → `sidebar` →
  `common.ModelInfo` (событие `creditsUpdatedMsg` никогда не отправляется),
  `hyperRefreshDoneMsg`/`refreshHyperAndRetrySelect` с зашитым провайдером `"hyper"`
  (`ui.go:2110-2154`), фиктивная тема `HyperbraidObsidiana` = `CharmtonePantera`
  (`styles/themes.go:113-116`) и весь механизм `themeKey`/`applyThemeForProvider` вокруг неё.
- **Мёртвый код после удаления catwalk-сервиса**: `config/provider.go:19-21,30-48,80-121`
  (`syncer`, `cache`), `load.go:36` (`defaultCatwalkURL`), `DisableProviderAutoUpdate`
  читается, но ни на что не влияет.
- **`internal/event`** — телеметрия вычищена честно (no-op, posthog удалён из go.mod),
  но каркас из ~30 пустых функций по-прежнему импортируется из `session`, `log`, `cmd`;
  развилка `shouldEnableMetrics` (`cmd/root.go:886`) мертва.
- `cmd/login.go:28` — пример «Authenticate with Charm Hyper» при switch только по copilot;
  `cmd/logout.go` рекламирует платформу `hyper`; `main.go:6-7` — swagger-контакт Charm.
- Тип `catwalk.Provider` торчит в публичном контракте фронтенда:
  `workspace.AgentModel.CatwalkCfg` (`workspace/workspace.go:109`).
- Мёртвые экспорты: `csync.NewLazyMap`/`NewLazySlice`/`NewMapFrom`,
  `permission.activeRequest`+`activeRequestMu`, `env.NewFromMap`, `diffdetect.Inspect`,
  `agent.isYolo` (поле-призрак), ветка `Cancel` по ключу `sessionID + "-summarize"`
  (`agent.go:1947`), который нигде не устанавливается.

### 3.7. Дублирование внутри подсистем

- **`agent/tools`**: блок «проверка пути вне workdir + запрос permission» (~20 строк)
  повторён в 12+ инструментах с расходящимися деталями; путь «создать файл»
  продублирован трижды (`write.go:85-140`, `edit.go:107`, `multiedit.go:148`);
  HTTP-transport настраивается вручную в `fetch`/`download`/`web_fetch`; нет единой
  политики «ошибка модели vs ошибка шага».
- **`ui/dialog`**: `notifications.go` ↔ `reasoning.go` — один диалог, скопированный
  дважды (~640 строк, различаются ~200); 7 разных `Draw` с одинаковой преамбулой;
  правила против багов копипасты institutionalized в `AGENTS.md` вместо общего
  базового типа. Сборка вложения из файла — 3 копии (`dialog/actions.go:174`,
  `ui.go:4595`, `ui.go:4670`).
- **`config`**: `Load` и `reloadFromDiskLocked` — расходящиеся дубликаты одной
  последовательности (`load.go:40-168` vs `store.go:1105-1224`): reload теряет часть
  стартовых дефолтов, rollback не откатывает уже сделанный `RemoveConfigField`.
- **`shellconfig`**: JSON-ключи конфига продублированы строковыми литералами
  (`options.go:180-253` и др.) без связи с тегами `config.Config` — переименование
  поля ломает braidrc молча.

### 3.8. Прочие заметные находки

- **Схема БД**: комментарии `-- Unix timestamp in milliseconds` при фактических
  секундах во всей схеме (`db/migrations/20250424200609_initial.sql:11-12,35-36,52-53`);
  триггер `update_sessions_updated_at` опровергает комментарий в
  `session/session.go:239-241` — переименование сессии подбрасывает её вверх списка;
  таблица `files` хранит полные версии файлов без ретенции.
- **Сериализация сообщений**: `parts` — один TEXT-блоб без версии формата; неизвестный
  тип части = ошибка на весь `List` → **сессия становится нечитаемой целиком**
  (`message/message.go:563-646`).
- **Permission-модель**: ключ нормализуется до директории (`permission.go:236-247`) —
  «разрешить навсегда» для файла даёт доступ ко всей директории.
- **Под-агенты обходят и permissions, и хуки**: `agentic_fetch_tool.go:165-199` создаёт
  инструменты без `permission.Service` и автоапрувит дочернюю сессию;
  `wrapToolsWithHooks` намеренно не оборачивает под-агентов. Две параллельные модели
  безопасности, внутренняя — пустая.
- **hooks**: поддержан только `PreToolUse`; env-префикс `BRAID_` вместо `CLAUDE_`
  ломает совместимость экосистемных хуков; гонка на буферах при таймауте
  (`hooks/runner.go:180-200`).
- **oauth/copilot**: переиспользование client ID и токена VS Code Copilot из
  `~/.config/github-copilot/apps.json` (`copilot/disk.go:10-27`) — ToS-риск и хрупкость.
- **`SetProviderAPIKey` пишет на диск до валидации** (`store.go:507-565`): при
  неизвестном провайдере диск изменён, память — нет. `ImportCopilot` делает двойную
  запись одних и тех же ключей (`store.go:943-952`).
- `configureProviders` (`load.go:209-499`) выполняет **сетевой model discovery под
  `writeMu`** — таймаут в 3 с блокирует все мутаторы конфига.
- `filetracker` нормализует пути относительно `os.Getwd()` вместо директории
  воркспейса (`filetracker/service.go:63,83`); panic-лог пишется в CWD
  (`log/log.go:72-74`).
- Синхронный I/O в `Update` TUI: `dialog.NewSessions` делает блокирующий
  `ListSessions` (HTTP в remote-режиме) прямо из обработки клавиши
  (`ui.go:4307` → `dialog/sessions.go:64`); `InitCoderAgent` — синхронно из
  `handleSelectModel` (`ui.go:2214`); повсеместный `context.TODO()`.

---

## 4. Тестовое покрытие: где сетка есть, а где нет

| Хорошо | Плохо |
|---|---|
| `server` 2 702 строк тестов / 2 774 прод; `backend` 2 680 / 2 265, включая e2e против живого HTTP и race-тесты | `TestCoderAgent` — единственный e2e цикла агента, 10 сценариев — **отключён** (кассеты записаны против `hyper.charm.land`) |
| `config` 1:1 тест/код, race-тесты CoW | Тесты `config` внутрипакетные, лезут в приватные поля, негерметичны (читают домашний конфиг) |
| `ui/chat` 53 %, golden-тесты `diffview`, инвариантные version-bump-тесты | `ui/dialog` 7 % на 10 421 строк; `styles` и `logo` — 0 %; **ни одного teatest/e2e теста TUI**; `Update` на 707 строк без прямых тестов |
| `agenttest` строит настоящий `Coordinator` продакшн-конструктором | Стабы `Workspace` встраивают интерфейс на 70 методов — падения только в рантайме |

---

## 5. Что сделано хорошо (сохранить при рефакторинге)

- **`workspace.Workspace` как граница UI↔бэкенд** — один TUI работает и in-process,
  и remote; явные ошибки-состояния (`ErrWorkspaceGone`, `ErrServerUnreachable`).
- **Жизненный цикл воркспейсов в `backend`** — refcount по SSE, `pending` против гонки
  teardown/create, латч `closing`, idle-shutdown; порядок захвата локов задокументирован.
- **OAuth single-flight** — двухуровневый (in-process + flock), различение
  «усыновить чужой токен» / «одолжить refresh», rotating refresh tokens.
- **Дебаунс записи стриминга** (`message.Service`) с честным контрактом
  «Flush перед чтением» и `FlushAll` на shutdown.
- **`hooked_tool.go`** — образцовый декоратор; **`herdr`** — эталон изоляции через
  собственный словарь событий и единую точку трансляции; **`notify`** — чистые DTO.
- **`ui/list`** — настоящий переиспользуемый компонент с ленивым рендером и
  версионированным кэшем; **`Overlay`** с grace-period против случайного подтверждения.
- **`shellconfig`** — слоёная схема с единым движком флагов; образец того, как стоило бы
  нарезать и `config`.
- **Комментарии-обоснования и `TECHDEBT.md`/`AGENTS.md`** — главный нематериальный актив.

---

## 6. План рефакторинга

Принцип: сначала страховочная сетка, затем чистка и точечные баги (дешёвые, снимают шум),
затем границы и дедупликация, и только потом крупные структурные изменения.

### Фаза 0 — страховочная сетка (до любых крупных правок)

| # | Задача | Риск | Эффект |
|---|---|---|---|
| 0.1 | Реанимировать `TestCoderAgent`: перенаправить `hyperBuilder` (`agent/common_test.go`) на локальный OpenAI-совместимый эндпоинт, перезаписать кассеты | низкий | Возвращает 10 e2e-сценариев цикла агента — обязательное условие для фаз 3–4 |
| 0.2 | e2e-тесты TUI через `teatest`: открыть модели, отправить сообщение, отменить агента, permission-flow, golden-снимки | средний | Без этого разбор `ui.go` делать нельзя |
| 0.3 | Тест-инвариант «путь записи конфига ⊆ пути чтения» + герметизация фикстур через `t.Setenv("BRAID_GLOBAL_CONFIG"/"BRAID_GLOBAL_DATA")` | очень низкий | Ловит класс багов из §3.4, реанимирует часть отключённых тестов |

### Фаза 1 — чистка (риск ≈ 0, можно сразу)

1. **Hyper из UI**: `logo.Opts.Hyper`+`hyperLetterforms`, цепочка `hyperCredits`,
   `hyperRefreshDoneMsg`/`refreshHyperAndRetrySelect`, тема `HyperbraidObsidiana`
   с механизмом `themeKey` (либо использовать каркас для настоящей темы Braid
   и переименовать `CharmtonePantera` → `BraidDark`).
2. **Мёртвый код catwalk** в `config/provider.go`, `defaultCatwalkURL`,
   `DisableProviderAutoUpdate`.
3. **Charm-косметика**: `cmd/login.go:28`, `hyper` в `cmd/logout.go`, swagger-контакт
   в `main.go` + перегенерация, `event.Init()`/`shouldEnableMetrics` из трёх команд.
4. **Мёртвые экспорты**: `csync.NewLazyMap`/`NewLazySlice`/`NewMapFrom`,
   `permission.activeRequest`, `env.NewFromMap`, `diffdetect.Inspect`, `agent.isYolo`,
   ветка `-summarize` в `Cancel`.
5. Исправить лживые комментарии о миллисекундах в initial-миграции; правило
   «timestamps в секундах» — в `AGENTS.md`.
6. Panic-лог — в data dir вместо CWD (`log/log.go:72-74`).

### Фаза 2 — точечные баги (риск низкий/средний, каждый — отдельный PR)

| # | Задача | Файлы | Риск |
|---|---|---|---|
| 2.1 | Баг §3.4: писать `type`/`base_url`/`name` провайдера вместе с кредами, **только если провайдера нет во встроенном каталоге** | `store.go:673-678`, `:519-522` | низкий |
| 2.2 | Убрать reload после записи кредов: `SetConfigFields` → `update()` в OAuth-путях | `store.go` | средний |
| 2.3 | `activeRequests.Del` → `CompareAndDelete` | `agent.go:1185,1457` | минимальный |
| 2.4 | `csync.Map.GetOrSet` — атомарность (один `Lock`, `fn` под локом; проверить call-sites на дедлок); `CompareAndDelete` — типизировать | `csync/maps.go` | средний |
| 2.5 | `pubsub`: снапшот подписчиков до отправки (убирает RLock-инверсию), завершение per-subscription горутин по `<-b.done` | `pubsub/broker.go` | средний |
| 2.6 | `history.CreateVersion`: `MAX(version)` внутри транзакции, убрать retry и мёртвый `err`; ретенция версий | `history/file.go` | средний |
| 2.7 | `projects.Register`: один мьютекс на операцию + `lock.File` + temp+rename | `projects/projects.go` | низкий |
| 2.8 | `unmarshalParts`: игнорировать неизвестные типы частей с логом, добавить версию формата | `message/message.go` | средний |
| 2.9 | `SetProviderAPIKey`: валидация до записи на диск; `ImportCopilot` — одна запись вместо двух | `store.go:507-565,943-952` | низкий |
| 2.10 | `filetracker`: инжектить workdir вместо `os.Getwd()` | `filetracker/service.go` | средний |
| 2.11 | `CancelAll`: `sync.WaitGroup` вместо busy-wait; чистка `dispatchMu` по завершении сессий | `agent.go:1997,340` | минимальный |

### Фаза 3 — границы и дедупликация (устраняет §3.2, §3.3, §3.7)

| # | Задача | Риск | Эффект |
|---|---|---|---|
| 3.1 | `ParseHostURL`/`DefaultHost` из `server` в `proto` — снимает зависимость client→server | минимальный | SDK собирается без сервера |
| 3.2 | Один резолвер модели: перенести из `app/provider.go` в `config`, удалить копию из `cmd/run.go` (тесты уже есть) | низкий | Чинит расхождение поведения `-m` между режимами |
| 3.3 | `proto.Message` → алиасы над `message` по образцу `proto/tools.go`; сверить JSON-теги. Альтернатива при желании развязать полностью: чистые типы контента в `internal/messagetype` | средний | −700 строк, невозможность «забыть поле» |
| 3.4 | `braid run` через `workspace.Workspace`: метод `AgentRunStream(ctx, sessionID, prompt, io.Writer)` в обеих реализациях; спиннер остаётся в `cmd` | средний | Оба режима ходят одним путём; удаляется второй `resolveSession` |
| 3.5 | Развязка слоёв: `RunNonInteractive` со спиннером — из `app` в `cmd`/`workspace`; `backend` → собственный proto-тип предупреждения вместо `ui/util`; далее `pubsub.Broker[tea.Msg]` → `Broker[any]` | низкий→средний | Ядро перестаёт знать про Bubble Tea |
| 3.6 | Ошибка Copilot-квоты: структурированная ошибка вместо lipgloss-строки в `agent.go:1149-1174` | минимальный | Убирает 3 UI-импорта из ядра агента |
| 3.7 | Supervisor демона из `cmd/root.go` (~450 строк) в `internal/server/supervisor`; тесты переезжают | низкий | root.go — только флаги |
| 3.8 | `toolkit` для `agent/tools`: `RequirePermission`, `ResolveWithinWorkdir`, `writeFileWithHistory`, общий HTTP-клиент | низкий | −400 строк дублей, единая политика ошибок |
| 3.9 | Разрезать `coordinator.go` по файлам: `providers.go`, `auth.go`, `subagents.go`; списки `copilotResponsesModels` → в метаданные каталога | низкий | Механическое разделение |
| 3.10 | `ui`: `SelectDialog[T]` вместо клонов `notifications`/`reasoning`; базовый `dialog.Base` с шаблонным `Draw`; хелпер вложений; реестр рендереров инструментов вместо switch на 26 case + тест полноты | средний | Институционализированная копипаста уходит в код |
| 3.11 | Разрезать `workspace.Workspace` (70 методов) на роль-интерфейсы (`SessionReader`, `AgentController`, `ConfigMutator`, …) | средний | Вменяемые стабы, явные потребности компонентов |
| 3.12 | Убрать синхронный I/O из `Update`: диалоги открываются через `tea.Cmd` + `LoadingDialog`; `context.TODO()` → контекст модели | средний | Убирает фризы TUI в remote-режиме |

### Фаза 4 — структурные изменения (после фаз 0–3)

| # | Задача | Риск | Примечание |
|---|---|---|---|
| 4.1 | **MCP/LSP/skills из глобалов в per-workspace объекты** (`*mcp.Registry` в `app.App` и т.д.). Промежуточный шаг с малым риском: `mcp.Close` перестаёт трогать общий брокер | высокий | Обязательное условие корректного мульти-воркспейса; делать одним заходом с явным DI |
| 4.2 | Каталог провайдеров: убрать `providerOnce`, сделать полем `ConfigStore` | низкий | После удаления catwalk синглтон ничего не экономит |
| 4.3 | `agent`: вынести протокол очереди/отмены в тип `dispatcher` (9 полей + Begin/Accept/drain/Cancel). Схлопывает три копии протокола; существующие 1000+ строк dispatch-тестов — страховка | средний | Максимальный эффект в подсистеме |
| 4.4 | `agent`: единая точка `RunComplete` (`sync.Once`-репортер) вместо 4 точек + `skipRunComplete`; затем `runSession`-объект турна вместо 14 замыканий — `Run` сжимается до ~120 строк | низкий → средний | Строго после 0.1 |
| 4.5 | `config`: разрезать `configureProviders` на `mergeCatalog → validate → apply`; сетевой discovery — за пределы `writeMu`; слить `Load` и `reloadFromDiskLocked` | средний | Табличная фиксация текущего поведения тестами — до правки |
| 4.6 | `ui`: разбор god-модели — `Sidebar`, `PillsPanel`, `EditorPanel`, `WorkspaceState`; роутер сообщений вместо 707-строчного `Update` | высокий | Только после 0.2 |
| 4.7 | `permission`: не держать `requestMu` во время ожидания пользователя — очередь показа + ожидание на канале | высокий | UI полагается на «один активный диалог» — нужна очередь в модели |
| 4.8 | Под-агенты: пропускать их инструменты через настоящий `permission.Service` и хуки вместо `AutoApproveSession` | средний | Закрывает вторую, пустую модель безопасности |

### Фаза 5 — отдельные проекты (по мере надобности)

- **`credentials.json`**: секреты отдельно от настроек (0600, вне merge). Решает сразу
  несколько проблем §3.4/3.5, но требует миграции — отдельный проект.
- **Генерация ключей `shellconfig` из json-тегов `Config`** (go:generate) или хотя бы
  рефлексивный тест соответствия `optionSpecs` ↔ поля `Options`.
- **hooks**: `PostToolUse`/`Stop`/`SessionStart` + `CLAUDE_*`-алиасы env — расширение
  публичного контракта, продумать до фиксации.
- **Сузить permission-ключ до файла** для write/edit с явной опцией «для директории».
- **`internal/shell`**: вынести jq/coreutils/шебанг-диспатч в подпакеты — огромный diff
  при нулевой семантике; делать в тихий период.
- Решение по судьбе client/server-режима: либо включать по умолчанию и гонять в CI
  (тогда фаза 3 обязательна), либо явно объявить экспериментальным.

### Ожидаемый порядок исполнения

```
Фаза 0 (0.1 → 0.3 → 0.2)
  → Фаза 1 (целиком, можно параллельно с 0.2)
  → Фаза 2 (2.1–2.3, 2.9 в первую очередь — чинят живые баги)
  → Фаза 3 (3.1 → 3.2 → 3.6 → 3.7 → 3.8/3.9 → 3.3 → 3.4 → 3.5 → 3.10–3.12)
  → Фаза 4 (4.2 → 4.3 → 4.4 → 4.1 → 4.5 → 4.8 → 4.6 → 4.7)
  → Фаза 5 по решению
```

Каждый пункт фаз 2–4 — отдельный PR с зелёными тестами; пункты фазы 4 не начинать,
пока не влиты соответствующие страховочные тесты фазы 0.
