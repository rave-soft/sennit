# Архитектурное ревью Braid и план рефакторинга

Дата: 2026-08-11. Объём кодовой базы: ~177 000 строк Go (~110k прод + ~68k тестов).
Метод: пять параллельных ревью по подсистемам (`agent`+`permission`+`hooks`, `ui`,
`config`+`shellconfig`+`oauth`, каркас `app`/`cmd`/`server`/`backend`/`workspace`/`thread`,
сквозные пакеты), сведённые в этот документ. Ссылки `файл:строка` актуальны на момент
ревью (main, `12662b01`).

Этот документ заменяет ревью от 2026-08-09. Его план (фазы 0–4) выполнен по существу
и повторно проверен по коду: dispatcher, per-workspace MCP/LSP, proto-алиасы,
permission-очередь, закрытие автоапрува под-агентов, разрез `configureProviders`,
чистка Hyper/catwalk, фиксы csync/pubsub/history/projects/unmarshalParts — всё на месте
и работает. Ниже — только текущее состояние.

Документ прошёл встречную проверку: замечания сверены с кодом и вшиты в находки
и план (уточнены фиксы legacy-импорта, gc, thread lifecycle, локов Summarize,
VCR-режимов; риск ряда задач поднят). Инверсия цен кэша, вокруг которой возникло
разночтение из-за неинтуитивных имён полей, отдельно перепроверена по полной
цепочке: см. §3.3.

---

## 1. Резюме

С прошлого ревью кодовая база выросла на ~34k строк: unified session panel, threads
(strands), todos, единая глобальная БД, `braid gc`, `braid import`, model-cache в SQLite,
discovery-refresh. Новый код в целом написан по правилам, выведенным из прошлого ревью
(границы `internal/thread` образцовые, комментарии-обоснования на месте), но три
системные проблемы сменили состав:

1. **Порча данных при миграции на единую БД.** Legacy-импорт складывает сохранённый
   `message_count` с числом заново вставленных сообщений (при консистентной
   legacy-БД и полном импорте получается ×2) и затирает `updated_at` временем
   импорта у сессий, в которые вставилось хотя бы одно сообщение. Ломается
   сортировка «последняя сессия», а gc-ретенция отсчитывается от импорта, а не от
   реальной активности. Существующий тест воспроизводит путь, но не проверяет
   счётчик и timestamp. Затрагивает обновившихся пользователей с такими сессиями.

2. **Жизненный цикл тредов не закрыт.** В client/server-режиме завершённый тред
   навсегда держит свой backend-воркспейс (полный `app.App` с MCP/LSP/вотчерами);
   idle-shutdown демона не срабатывает. У `thread.Manager` нет `Shutdown`, `Recover`
   при старте затирает running-треды другого процесса (общая БД, а локальный режим
   не берёт workspace-lock).

3. **Синхронный I/O вернулся в TUI Update.** Старый пункт 3.12 был закрыт для
   sessions-диалога, но новые фичи принесли новый: `SupportsThreads()` — это полный
   `ListThreads` по HTTP, и он зовётся с хвоста **каждого** Update; переключение
   сессии — N+1 запросов; Enter, `/`, Ctrl+P, конфиг-мутации из диалогов — всё
   синхронно в теле Update. `UI.Update` вырос с 707 до 919 строк, `ui.go` — до 5 761;
   teatest-сетки (0.2 прошлого плана) по-прежнему нет.

Плюс шлейф средних находок: три lock-bypass'а вокруг нового dispatcher'а в agent,
инвертированные цены кэша в braidrc `model add` (с тестом-соучастником), мутация
опубликованного `Config` после reload, утечка памяти в `message.service.pending`,
double-close в `pubsub.Shutdown`, и свежая копипаста (три копии TTL-кэша в UI,
клон confirm-диалога, близнецы `attachLocalThreads`/`attachServerThreads`) — те же
болезни, что план 2026-08-09 искоренял, воспроизведённые новым кодом.

---

## 2. Карта системы (текущая)

| Пакет | Строк | Динамика | Роль |
|---|---:|---|---|
| `internal/ui` | 59 967 | +15k | Bubble Tea v2 TUI; `Root`-роутер экранов, session panel, threads dock |
| `internal/agent` | 29 841 | +4k | Цикл агента, `dispatcher`, `runTurn`, per-workspace `mcp.Registry` |
| `internal/config` | 15 463 | +4k | Load/merge/store, OAuth, model-cache в SQLite, `braid import` |
| `internal/server` | 6 680 | +1.2k | HTTP/h2c, тонкие хендлеры, `server/threads.go` |
| `internal/cmd` | 6 149 | | Cobra + supervisor (вынесен в `cmd/supervisor/`) |
| `internal/workspace` | 5 818 | +2.3k | Роль-интерфейсы, threads через оба режима |
| `internal/backend` | 5 437 | | Мульти-воркспейс, refcount/holds, `thread_spawner` |
| `internal/db` | 3 450 | +1k | Единая глобальная БД, refcounted-пул, legacy-импорт, gc |
| `internal/thread` | 2 133 | новый | Ядро тредов (strands): Manager, Spawner, merge-флоу |
| `internal/discover` | 1 841 | новый | Model discovery, реестр enricher'ов |
| `internal/herdr` | 859 | | Изолированная аналитика со своим словарём событий |

Файлы-гиганты (нетестовые): `ui/model/ui.go` 5 761 (**вырос**), `config/load.go` 1 820,
`workspace/client_workspace.go` 1 675, `ui/chat/tools.go` 1 647, `agent/agent.go` 1 616
(**сжался** с 2 258), `ui/model/chat.go` 1 594 (новый), `config/store.go` 1 418,
`server/proto.go` 1 219 (тонкие хендлеры — не помойка), `agent/coordinator.go` 1 188,
`agent/tools/mcp/init.go` 1 131, `styles/quickstyle.go` — по-прежнему одна функция
на 985 строк.

---

## 3. Находки

### 3.1. Критично

- **Legacy-импорт БД портит данные.** `db/legacy_import.go:132-135` вставляет сессию
  с сохранённым `message_count`, затем вставка сообщений через триггер
  `update_session_message_count_on_insert` инкрементирует счётчик ещё раз. Точный
  итог — legacy `message_count` + число успешно вставленных сообщений; ×2
  получается только при консистентном исходном счётчике и полном импорте. Каскад
  того же триггера возбуждает `update_sessions_updated_at`: время импорта получают
  сессии, в которые успешно вставилось хотя бы одно сообщение; ломается сортировка
  и gc-ретенция. Существующий тест создаёт сценарий, но не проверяет счётчик и
  `updated_at`. Рядом:
  `legacy_import.go:136-141` глушит **любую** ошибку вставки как «конфликт id»
  (disk full / SQLITE_BUSY → тихая потеря сессии, файл переименуется в `.imported`).

- **Утечка воркспейсов тредов (client/server).** `thread/manager.go`:
  `spawner.Release` вызывается только в `abortSpawn` (:229) и `Remove` (:503);
  успешный `onRunComplete`/`finishMerge` оставляет живой handle. `threadSpawner`
  держит backend-воркспейс «как живой SSE-стрим» (`backend/thread_spawner.go:57-60`)
  — завершённый тред блокирует idle-shutdown демона до явного `braid threads rm`.
  `Manager` не имеет `Shutdown`; при teardown родителя hold спавнера никто не снимает.
  Комментарий `thread_spawner.go:31-33` («release immediately when the thread is
  done») коду не соответствует.

- **Блокирующий HTTP на UI-горутине.** `ClientWorkspace.SupportsThreads` = полный
  `ListThreads` (`workspace/threads.go:189-192`); guard исполняется в теле Update
  в трёх TTL-кэшах (`threads_cache.go:81`, `threads_dock.go:119`,
  `thread_indicator.go:72`); indicator и dock дёргаются через
  `staleWorkspaceRefreshCmds` с хвоста Update при протухании TTL (`ui.go:1721`),
  тогда как dashboard-cache живёт в своём экране. После установки `inFlight` повторный
  HTTP не выполняется, но для воркспейса **без** поддержки тредов false
  возвращается до установки `inFlight` — probe повторяется постоянно. Тот же
  вызов — на каждый ввод `/` (`ui.go:3209`) и Ctrl+P. Также синхронно в Update:
  `loadSessionMsg` — `ListMessages` + рекурсивный N+1 `loadNestedToolCalls`
  (`ui.go:887`, `:1862`); `sendMessage` — `AgentReadyErr` на каждый Enter и
  `CreateSession` при отправке без активной сессии (`ui.go:4605-4624`);
  конфиг-мутации из
  `applyDialogAction` (`UpdatePreferredModel` `ui.go:2609`, `ImportCopilot` `:2787`,
  `SetConfigField` `:2457`, Grant/Deny `:2625`); `FilePicker` синхронно открывает и
  декодирует допустимую картинку при смене выбранного пути, пока для сочетания
  path/размер превью ещё нет transmission (`filepicker.go:191-204,307-319`).

### 3.2. Средне — конкурентность

- **agent: три lock-bypass'а вокруг dispatcher.** (а) Прямой `Summarize` входит
  в сессию мимо per-session мьютекса: busy-check `agent.go:726` и
  `activeRequests.Set` `:749-751` не атомарны — конкурентный Run проходит свой
  busy-check в окне. (б) Re-queue продолжения после auto-summarize: `Get → append →
  Set` без per-session лока (`agent.go:629-637`) — lost update; заметить: сам
  `enqueueCall` лока не берёт (`dispatch.go:182-197`), его держат вызыватели, так
  что фикс — брать тот же лок вокруг continuation либо атомарный helper.
  (в) `dispatcher.clearQueue` не берёт per-session мьютекс (`dispatch.go:403-411`),
  в отличие от `cancel`.
- **config: `SetupAgents` мутирует опубликованный Config.** `store.go:1365` делает
  `setConfig(cfg)`, затем `:1380` переписывает `Agents`/`Problems` на живом объекте —
  нарушение инварианта «published Config is immutable» (`store.go:127`), map
  read/write race. Тот же паттерн у UI (`ui.go:2728,2808`);
  `client_workspace.go:89,114` **не** затронут — там `SetupAgents` работает над
  локальным, ещё не опубликованным объектом. Фикс — reorder: `SetupAgents` до
  `setConfig`.
- **config: миграции пишут глобальный файл без flock.** `migrateBloatedModelCache`
  (`load.go:1315,1354`) и `migrateDisableNotifications` (`load.go:1471-1545`,
  выполняется на **каждый** Load и reload) минуют `store.atomicWrite`/`lock.File` —
  окно затирания записи другого процесса.
- **pubsub: double-close `b.done`.** `broker.go:174-180` — select-then-close без
  лока; два конкурентных Shutdown → panic. Лечится `sync.Once`.
- **message: утечка памяти.** `service.pending` (`message/message.go:110`) растёт на
  каждый уникальный message ID и чистится только в `Delete`; каждая запись держит
  два полных снапшота Message со всеми parts. Нужна эвикция после финального flush.
- **thread: `Recover` затирает чужие живые треды.** `manager.go:554-576` помечает
  все pending/running interrupted при старте; БД общая, а локальный режим не берёт
  workspace-lock (`cmd/root.go:263-283`) — два процесса конкурируют за одни
  worktree/ветки.
- **shell: TOCTOU на лимите фоновых джобов** (`background.go:91-117`) + менеджер —
  процессный синглтон: в server-режиме все воркспейсы делят лимит 50 и пространство ID.
- **db: `braid gc` TOCTOU** — selection выполняется вне транзакции удаления
  (`cmd/gc.go:151-189`): сессия, ожившая в окне, удалится со свежими сообщениями.
  Простой guard `DELETE ... AND updated_at < ?` не сработает: messages удаляются
  первыми (`gc.go:312-315`), и их delete-триггер сам поднимает `updated_at`
  сессии; к тому же подтверждённые descendants удаляются независимо от
  собственного возраста. Нужна одна `BEGIN IMMEDIATE`-транзакция на selection
  (roots, descendant closure, thread status) и deletion. Интеграционные тесты у
  gc есть (`cmd/gc_test.go` — descendants, threads, project scope, dry-run,
  retention, VACUUM); не покрыты гонки eligibility, новый descendant в окне
  и rollback.

### 3.3. Средне — корректность и стоимость

- **Инвертированные цены кэша в braidrc.** Перепроверено по полной цепочке
  (встречное замечание «маппинг соответствует расчёту» не подтвердилось):
  `cost_per_1m_in_cached` → `CostPer1MInCached` (catwalk `provider.go:94`), и
  учёт умножает его на `CacheCreationTokens`, а `CostPer1MOutCached` — на
  `CacheReadTokens` (`agent/agent.go:1244-1245`, `:1302-1303`); внутренний
  контракт: in_cached = cache creation, out_cached = cache hit.
  `shellconfig/model.go:72-73` маппит наоборот: `--price-cache-create` →
  `cost_per_1m_out_cached`, `--price-cache-hit` → `cost_per_1m_in_cached`.
  Тест `model_test.go:57-63` закрепляет баг.
- **`UpdateModels` на каждый Run** (`coordinator.go:328,1019-1038`) пересобирает
  все инструменты и всех custom-агентов: 2(N+1) readiness-горутин с `prompt.Build`
  (git-сабпроцессы), новый `hooks.Runner` (перекомпиляция regex) — на каждый
  пользовательский промпт.
- **Load vs reload разошлись.** Блок мерджа workspace-конфига продублирован
  (`load.go:64-84` vs `store.go:1271-1287`), и `Load` прогоняет
  `dropIncompatibleRecentModels`, а reload — нет: старый `recent_models` на reload
  роняет unmarshal, и вся workspace-секция молча отбрасывается.
- **`SupportsThreads` без кэша и склейка ошибок** (`workspace/threads.go:189-192`):
  сетевой сбой = «треды не поддерживаются»; CLI-команды делают второй такой же
  запрос сразу после первого.
- SSE-события тредов уходят с пустым `WorkspaceID` (`server/events.go:169-177`) —
  wire-поле, всегда пустое, ловушка для будущих потребителей.
- MCP-мелочи: `oauthRoundTripper` при 401 повторяет POST без тела и глотает ошибку
  `Authorize` (`mcp/init.go:1028-1043`); `stdioCheck` дублирует argv[0]
  (`:1121-1126`); отменённый auth-флоу может залипнуть в StateError.

### 3.4. Средне — дублирование (новая копипаста)

- **Три копии TTL-кэш-машины в UI** (~220 строк): `threadIndicatorState` ≡
  `threadsDockState` ≡ `threadsCacheState` — идентичная шестёрка методов;
  комментарии сами признают «same TTL-cache idiom». Просится `ttlCache[T]`
  (bool-зачаток уже есть в `workspace_cache.go:79`).
- **Клон confirm-диалога**: `dialog/quit.go` ↔ `dialog/thread_remove_confirm.go` —
  ~120 из ~148 строк дословно. Нужен общий `confirmDialog`.
- **Слой копипасты вокруг делегаций/тредов** (~200 строк): `formatElapsed`/
  `formatTokenCount` скопированы в `child_session_panel.go:287-311` из
  `chat/agent.go:902-926`; статус-строка — 4 варианта; классификация todo — 4 копии
  switch; `drawThreadBlocks` ↔ `drawDelegationBlocks` — ~55 строк × 2.
- **`attachLocalThreads` ↔ `attachServerThreads`** — ~40-строчные близнецы
  (`cmd/threads.go:31-68` / `backend/threads.go:23-62`), отличается только Spawner.
- **`proto.Todo` — трёхполевая копия `session.Todo`** с конверторами в 2-3 местах
  (`server/events.go:254`, `client_workspace.go:1537,1636`); алиас-паттерн из
  `proto/message.go` убрал бы их. То же — `LSPClientInfo` в трёх копиях (осталось
  со старого ревью).
- **Диалоговая инфраструктура недовнедрена**: `selectDialog` — 4 диалога из ~20,
  `Base` — 3; преамбула width/innerWidth скопирована 10 раз; item-boilerplate
  (`SetFocused`/`SetMatch`/`Render`) — в 7 диалогах.
- **mcp/init.go**: два почти идентичных OAuth-блока HTTP/SSE (`:883-923` vs
  `:958-992`); `getOrRenewClient` дублирует хвост `connectAndRegister`.

### 3.5. Тестовое покрытие

| Хорошо | Плохо |
|---|---|
| Конкурентность agent покрыта целенаправленно: dispatch_cancel (8 тестов), race-инъекции в summarize, run_complete | **`TestCoderAgent` фактически выключен**: кассеты на диске — старые записи `hyper.charm.land`, скип без `BRAID_TEST_OPENAI_BASE_URL`; 10 e2e-сценариев не гоняются |
| config: load_test 2 547 строк, path-инвариант, race-сценарии single-flight включены и зелёные | **teatest/e2e TUI — по-прежнему ноль** (0.2 прошлого плана); golden только в diffview |
| thread 901/2 064, discover 1 086/1 841, threads покрыты с обеих сторон workspace; у gc — 6 интеграционных сценариев (`cmd/gc_test.go`) | **db 26.6%**: у gc нет race/rollback-сценариев; баг импорта прошёл мимо теста (проверяет заголовок, не счётчики и не updated_at) |
| csync 90.9%, pubsub 85.6%, diffview 89.4%, completions 76.7% | dialog 27.8% (28 файлов без тестов, `question_form.go` 782 строки), styles/logo 0%, chat просел 53→46.8%, history 22.6%, ui/image 3.1% |

### 3.6. Мёртвый код (сводный список на чистку)

UI: `chat/unified_diff.go` целиком; 9 letterform'ов старого wordmark;
`util.ExecShell`; `list.AdjustArea`/`InvalidateFrozen`/drag-freeze механика;
навигация сайдбара `pageUp/pageDown/toHome/toEnd`; `common.Model[T]`;
`styles.LoadingIcon`/`BorderThin`/`BorderThick`; ~28 write-only полей `Styles`
(вся секция `Pills.Queue*/Todo*/Help*` — очередь переехала в session panel);
`UI.keyenh`; `common/elements.go:128 FormatCredits`; вырожденный
`ThemeForProvider(_ string)`. Прочее: `db.ConnectReadOnly` + оба `openDBReadOnly`
(в modernc-версии к тому же `_txlock=immediate` на ro-соединении);
`csync.NewMapFrom`, тип `LazySlice` целиком; `env.NewFromMap`;
`agent.ErrRequestCancelled` (сравнивается, но никогда не возвращается);
пустой `config/copilot.go`; мёртвый параметр `actions` в
`validateCustomProviders` (+устаревшие комментарии); duration-арифметика в
no-op `event/all.go`.

### 3.7. Осознанные политики, требующие явного решения (не баги)

- **Хуки не оборачивают инструменты под-агентов** (`hooked_tool.go:31-42`) —
  осознанно, но следствие: PreToolUse-guard на `bash rm` не сработает внутри
  task-агента; защита — только permission-промпт. Если хуки — guard-rail,
  это дыра политики.
- **client/server-режим всё ещё выключен по умолчанию** (`BRAID_CLIENT_SERVER`,
  `cmd/root.go:210-215`) — критичная утечка тредов жила именно там, потому что
  режим не обкатывается.
- **Copilot**: чтение `~/.config/github-copilot/apps.json` с vscode-client-ID и
  имитацией заголовков — ToS-риск, как и был.
- **`PushPopBraidEnv`** всё так же мутирует `os.Environ()` процесса
  (`load.go:208-238`); бонус-дефект: `restore()` создаёт пустую переменную вместо
  `Unsetenv`. Реальный фикс — env-снапшот в аргументах, это отдельный проект.

---

## 4. Что сделано хорошо (сохранить)

- **`internal/thread` — образцовые границы**: ядро без UI/HTTP-импортов, `Spawner`
  как интерфейс с двумя честными реализациями, merge-флоу с разбором
  ff/checked-out/moved-base. Новые пакеты `discover` и `herdr` — так же чисто.
- **`dispatch.go`** — чистый тип без внешних зависимостей, cancel-mark по
  монотонным seq; конкурентность тестируется инъекциями гонок, а не заклинаниями.
- **Локинг `ConfigStore` задокументирован образцово** (роль каждого из пяти
  мьютексов, lock ordering); `pendingDiskAction` — чистое решение реэнтрантного
  autoReload; кросс-процессный OAuth-refresh с adopt/borrow-семантикой покрыт
  включёнными герметичными тестами.
- **`workspace_cache.go`** — эталонная мемоизация (TTL-бэкстоп, generation-guard,
  invalidate на run-границах); реестр рендереров инструментов с тестом полноты;
  `Root`-роутер экранов с честным контрактом.
- **Единая БД**: refcounted-пул + зеркальный refcounted flock по inode,
  `_txlock=immediate`, продуманный `braid gc` UX (dry-run, --json, мотивированные
  per-table delete).
- **`braid import`** — dry-run, отчёт по каждому файлу, непереводимые поля
  сохраняются комментариями в frontmatter вместо молчаливой потери.
- **Комментарии-обоснования** — по-прежнему главный нематериальный актив; новый
  код им следует.

---

## 5. План рефакторинга

Принцип тот же: сначала баги данных и утечки (дёшево, срочно), затем отзывчивость
UI, затем конкурентные окна, затем дедупликация — и параллельно достроить
страховочную сетку, без которой роутер Update и дальнейшие разрезы делать нельзя.
Каждый пункт фаз 1–4 — отдельный PR с зелёными тестами.

### Фаза 1 — баги данных и утечки (сразу, в этом порядке)

| # | Задача | Файлы | Риск |
|---|---|---|---|
| 1.1 | Legacy-импорт: вставлять сессии с `message_count = 0` (триггер досчитает). Восстановление `updated_at` требует **миграции триггера** `update_sessions_updated_at` — сейчас любой явный UPDATE перетирается на now (`initial.sql:16-21`); только затем явный UPDATE в конце транзакции (время импорта получают сессии, у которых вставилось хоть одно сообщение). Единая политика ошибок для **всех пяти** таблиц (`legacy_import.go:132-283`: sessions/messages/files/read_files/threads): `INSERT ... ON CONFLICT DO NOTHING` + `RowsAffected` — пропускать только PK/UNIQUE-конфликты, FK/CHECK/NOT NULL и operational errors → rollback всей транзакции. Явно определить судьбу зависимых rows пропущенной session: сейчас messages/files/read_files молча пропускаются, а threads не получают `skippedSessions` и могут привязаться к чужой session с совпавшим ID; отчёт должен учитывать все такие пропуски. Тесты на счётчики, updated_at, конфликты parent/dependent IDs и rollback | `db/legacy_import.go`, `db/migrations/` | средний |
| 1.2 | Треды: **отдельный высокорисковый lifecycle-проект**, а не точечный Release — наивный `Release` в `onRunComplete`/`finishMerge` оставит released handle в `m.running`, и `Send` пойдёт в закрытый App (`manager.go:342-363`). Нужны: атомарное изъятие runtime ownership, per-thread сериализация, single-flight respawn, проверка RunID/generation у stale RunComplete, release на всех терминальных исходах, `Manager.Shutdown` с ожиданием горутин; упорядоченный teardown (запрет новых операций → cancel/join manager goroutines → release handles → release thread-store DB reference), а не независимые параллельные cleanup-функции App; решение по attach-семантике завершённых тредов (после release `AttachThread` вернёт «not currently running» — нужен respawn либо read-only load persisted-сессии); попутно фикс derived client ID `<uuid>/thread/<id>`, не проходящего UUID-валидацию backend. Ownership/state-machine тесты обязательны | `thread/manager.go`, `backend/thread_spawner.go`, `workspace/threads.go`, wiring cleanup | **высокий** |
| 1.3 | Инверсия цен кэша: поменять местами jsonKey у `--price-cache-create`/`--price-cache-hit`, починить тест-соучастник (инверсия перепроверена, см. §3.3) | `shellconfig/model.go:72-73`, `model_test.go` | минимальный |
| 1.4 | `cfg.SetupAgents()` **до** `setConfig(cfg)` в reload; из UI-вызовов (`ui.go:2728,2808`) убрать мутацию опубликованного конфига (`client_workspace.go` не затронут — работает с локальным объектом) | `config/store.go:1365-1380`, `ui.go` | низкий |
| 1.5 | `pubsub.Shutdown` через `sync.Once` | `pubsub/broker.go:174-180` | минимальный |
| 1.6 | Эвикция `message.service.pending` (растёт на каждый уникальный message ID): удалять запись только после finished-снапшота, под `s.mu`, при отсутствии `dirty`/`flushing` — по любому terminal flush эвиктить нельзя (terminal бывает и у tool-call/reasoning) | `message/message.go` | средний |
| 1.7 | gc: selection (roots, descendant closure, thread status) и deletion — в одной `BEGIN IMMEDIATE`-транзакции (guard по `updated_at` не работает — см. §3.2, delete-триггер messages сам поднимает его); существующие тесты `gc_test.go` сохранить, добавить race-сценарии: ожившая сессия, новый descendant в окне, rollback | `cmd/gc.go`, `internal/db` | средний |

### Фаза 2 — отзывчивость TUI (максимальный видимый эффект)

| # | Задача | Риск |
|---|---|---|
| 2.0 | **Минимальная command-driving тест-обвязка UI** (предусловие 2.2–2.4): детерминированная прокачка `tea.Cmd` с fake workspace — проверяет исполнение команд, stale-result guard'ы, повторный Enter, permission round-trip и маршрутизацию; golden-снимки — поверх неё, не вместо (rendering-golden state-машины не ловит) | средний |
| 2.1 | Кэшировать `SupportsThreads` одной пробой на старте воркспейса. Не «час работы»: бит в `proto.Workspace` меняет wire-контракт и wiring server/backend; кэшу нужна инвалидация; транспортную ошибку нельзя кэшировать как «unsupported»; для 409 нет отдельной таксономии ошибок. Убирает HTTP из хвоста Update, `/` и Ctrl+P | средний |
| 2.2 | `loadSessionMsg` → async `tea.Cmd`. Generation-guard для загрузки сессии **ещё не существует** (в отличие от sessions-диалога) — построить: generation + ожидаемый session ID, отбрасывание stale-результата до любых изменений UI/LSP/history — иначе поздний старый ответ заменит текущую сессию | средний |
| 2.3 | `sendMessage`: `AgentReadyErr`/`CreateSession` — в cmd; конфиг-мутации из `applyDialogAction` (UpdatePreferredModel, ImportCopilot, SetConfigField, Grant/Deny, SetCompactMode) — в cmd с loading-состоянием | средний |
| 2.4 | FilePicker: decode/transmit превью — async; cache key включает path, metadata/mtime и размеры/encoding, результат несёт generation выбранного файла и отбрасывается после смены курсора; добавить разумный лимит размера decode. Не мутировать `previewingImage` из `tea.Cmd`, применять результат отдельным msg в Update | средний |
| 2.5 | agent: не пересобирать мир на каждый Run — кэшировать `buildTools`/`buildAgent`. Это не только стоимость, но и correctness: конкурентные Run могут публиковать tools из разных снапшотов конфига. Инвалидация зависит не только от модели/конфига: MCP registry, skills, thread manager, hooks, LSP, permissions, web-search backend — карту зависимостей составить до правки | средний→высокий |
| 2.6 | Устранение N+1 в `loadNestedToolCalls` — отдельная задача поверх 2.2: ID глубоких дочерних сессий известны только после разбора предыдущего уровня; нужен новый recursive/batch запрос в DB и соответствующий workspace/server API | средний |

### Фаза 3 — конкурентные окна (каждый фикс точечный)

| # | Задача | Файлы |
|---|---|---|
| 3.1 | `Summarize`: под `sessMu` — **только** атомарный busy-check + регистрация active/cancel entry; лок отпустить до DB I/O и сборки промпта (держать его на весь диапазон нельзя), очистка entry на всех ранних выходах | `agent.go:726-751` |
| 3.2 | Re-queue после auto-summarize — под тем же per-session локом (сам `enqueueCall` лока не берёт — его держат вызыватели) либо новый действительно атомарный helper в dispatcher; проставить acceptSeq | `agent.go:629-637`, `dispatch.go:182-197` |
| 3.3 | `clearQueue` — под per-session мьютекс, симметрично `cancel` | `dispatch.go:403-411` |
| 3.4 | Конфиг-миграции: **без** once-маркера (помешает retry после частичной ошибки и пропустит легаси-поле, дописанное старой версией) — `lock.File` + повторное чтение актуальных байт под локом + идемпотентное преобразование; для миграции между двумя файлами — фиксированный порядок захвата обоих локов и повторное чтение обоих файлов | `load.go:1315,1471` |
| 3.5 | Общий хелпер мерджа workspace-конфига для Load и reload (закрывает и расхождение `dropIncompatibleRecentModels`, и молчаливый skip секции — добавить warn) | `load.go:64-84`, `store.go:1271-1287` |
| 3.6 | shell background: per-workspace менеджер как зависимость, которой владеет конкретный App и которая передаётся Bash/JobOutput/JobKill — сейчас Shutdown одного App делает `KillAll` процессного синглтона и убивает джобы чужого воркспейса; атомарный check-and-set лимита | `shell/background.go` |
| 3.7 | Локальный режим берёт **repo-scoped** workspace-lock, закрывающий один git-репозиторий даже при разных `--data-dir`; существующий data-dir lock подходит только при явно зафиксированном инварианте «один repo → один data dir». Простого owner ID для `Recover` недостаточно: альтернатива потребует lease/heartbeat/epoch и является отдельным распределённым протоколом, не точечным фиксом | `cmd/root.go:263-283`, `thread/manager.go:554-576`, `db/datadirlock.go` |

### Фаза 4 — дедупликация (пока копии не разошлись)

| # | Задача | Эффект |
|---|---|---|
| 4.1 | `ttlCache[T]` в ui/model; перевести threadIndicator/threadsDock/threadsCache и bool-кэши workspace_cache | −220 строк, канон для будущих кэшей |
| 4.2 | `confirmDialog` (quit + thread_remove_confirm); домиграция commands/sessions на `selectDialog`, остальных диалогов на `Base`; `list.BaseItem` для item-boilerplate | −500+ строк, преамбула Draw перестаёт тиражироваться |
| 4.3 | Общие форматтеры делегаций/тредов: formatElapsed/formatTokenCount/статус-строка/todo-классификация/рендер строки todo — один пакет-хелпер; слить `drawThreadBlocks`/`drawDelegationBlocks` | −200 строк |
| 4.4 | Один хелпер attach-threads с параметром-Spawner вместо близнецов в cmd и backend | −40 строк, один путь wiring |
| 4.5 | `proto.Todo` → алиас `session.Todo`; `LSPClientInfo` → алиасы по образцу `proto/message.go` | Конверторы исчезают |
| 4.6 | mcp/init.go: общий OAuth-блок для HTTP/SSE; `getOrRenewClient` переиспользует хвост `connectAndRegister`; починить `stdioCheck` argv; `oauthRoundTripper` — retry через `req.GetBody` (либо явный отказ от retry), закрытие исходного response body, не глотать ошибку `Authorize`; закрыть гонку `suppressLock`, cancel-без-unlock и восстановление `StateNeedsAuth` после отменённого auth-флоу | −100 строк + пачка мелких багов |
| 4.7 | Чистка мёртвого кода по списку §3.6 | −1 000+ строк |

### Фаза 5 — страховочная сетка и структура

| # | Задача | Примечание |
|---|---|---|
| 5.1 | **Вернуть `TestCoderAgent` в CI** — двумя шагами: сначала разделить VCR-режимы (сейчас без `BRAID_TEST_OPENAI_BASE_URL` тест безусловно skip-ается, а с ней всегда RecordOnly на живой эндпоинт — `agent_test.go:57-95`): replay свежих кассет по умолчанию + отдельный явный флаг записи; затем перезаписать кассеты локальной моделью (команда в TECHDEBT.md) | блокирует дальнейшие разрезы Run |
| 5.2 | Golden-снимки `uv.ScreenBuffer` поверх обвязки 2.0 для 3-5 ключевых сценариев (открыть модели, отправить сообщение, permission-flow, панель тредов) — как дополнительный уровень проверки, не единственный | предусловие 5.3, после 2.0 |
| 5.3 | Роутер сообщений вместо 919-строчного `Update`: mouse-обработка (~450 строк) и `loadSessionMsg` — в отдельные обработчики; продолжить вектор apply*-методов | только после 2.0 и 5.2 |
| 5.4 | Тесты: db/gc (см. 1.7), history.CreateVersion конкурентный, dialog топ-5 непокрытых | параллельно всему |
| 5.5 | Решение по client/server: флип дефолта или dogfood-режим в CI — утечка 1.2 жила там именно потому, что режим не обкатывается | после 1.2 |

### Фаза 6 — отложенные решения (по мере надобности)

- **Хуки для под-агентов**: зафиксировать политику явно (документировать «хуки не
  guard-rail для делегаций») либо оборачивать под-агентов с дедупликацией событий.
  Плюс `PostToolUse`/`Stop` — расширение публичного контракта.
- **`PushPopBraidEnv`** → передача env-снапшота в discovery вместо мутации
  `os.Environ()`; попутно фикс `restore()` (Unsetenv для отсутствовавших).
- **Copilot ToS-риск** — решение владельца проекта (см. TECHDEBT.md).
- **`event`-каркас** — оставлен осознанно ради diff с upstream; опустошить
  duration-арифметику в `all.go`, остальное не трогать.
- **`ErrRequestCancelled`** — либо возвращать из dispatcher, либо удалить вместе
  со сравнением в `app_workspace.go:308`.

### Ожидаемый порядок исполнения

```
Фаза 1 (1.3 → 1.4 → 1.5 — в первый же заход; 1.1 с миграцией триггера следом;
        1.6, 1.7 за ними; 1.2 — отдельный высокорисковый проект, вести параллельно)
  → Фаза 2 (2.0 обвязка → 2.1 → 2.2–2.5; 2.6 поверх 2.2)
  → Фаза 3 (целиком, точечные PR, можно параллельно с фазой 2)
  → Фаза 4 (4.1–4.4 до появления новых копий; 4.7 в тихий период)
  → Фаза 5 (5.1 — разделение VCR-режимов сразу, перезапись кассет по появлении
             LLM-эндпоинта; 5.2 после 2.0; 5.3 строго после 5.2; 5.5 после 1.2)
  → Фаза 6 по решению
```
