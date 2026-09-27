# Клиент-сервер: UI через gRPC, демон на проект, удалённое подключение

Возвращаем режим, удалённый в C1 (`1247e2c9e`, `81785e27c`, `027d6155c`,
2026-08-15), но проектируем его заново под три требования:

1. UI работает с воркспейсом только через gRPC.
2. Агент продолжает работу после закрытия UI; к нему можно подключиться
   снова (`sennit attach`).
3. К демону на другой машине можно подключиться по SSH.

- Ветка: `main`, дерево `4c5e5612f`
- Транспорт: gRPC с JSON-кодеком поверх существующих Go-типов (вариант «а»)
- PR: 23, в четырёх фазах плюс завершающая

Номера строк указаны по дереву `4c5e5612f`; перед каждым PR их надо сверить.
Объёмы предварительные.

## Зафиксированные решения

| | Решение | Почему |
|---|---|---|
| **кодек** | gRPC с `encoding.Codec` на `encoding/json`; `.proto` для сервиса не пишем | 121 метод и около 40 DTO уже описаны Go-типами. Зеркальные pb-структуры и конвертеры вернули бы «шесть точек на операцию», из-за которых режим удаляли. |
| **описание сервиса** | Генерируется из `workspace.FrontendWorkspace` через `go/types` | Интерфейс остаётся единственным источником. Новый метод требует правки интерфейса, `appws` и одной строки в таблице классов. |
| **топология** | Один демон на проект, ключ: git common dir | Совпадает с ключом `workspacelock`. Старый демон на пользователя требовал `pathIndex`, льготные периоды на воркспейс и защёлку `pending/closing`; с одним воркспейсом на процесс всё это не нужно. |
| **сокет** | Unix-сокет `0600`, `$XDG_RUNTIME_DIR/sennit/<hash16>.sock`, путь записывается в lock-файл | TCP не слушаем. На Windows используется AF_UNIX (Go поддерживает его с Windows 10), `go-winio` не возвращаем. |
| **удалённо** | `ssh host sennit daemon bridge --cwd <path>`: мост stdio↔сокет, gRPC поверх канала SSH | Своих TLS и аутентификации не делаем, права даёт SSH. |
| **встроенный режим** | Остаётся. `options.daemon = "off" | "auto"`, по умолчанию `off` до конца фазы 4 | Клиент-серверный режим сначала обкатывается как опция. Встроенный режим тоже ходит через кодек в тестах (PR 0.7), поэтому пути расходятся только транспортом. |
| **конфиг** | Источник истины для провайдеров, моделей, MCP, LSP, хуков и прав: конфиг машины демона. UI получает его снимок с вырезанными секретами. UI-настройки (тема, клавиши, спиннер, скроллбар, compact, diff, прозрачность, лимиты дополнений, прогресс, уведомления) клиент читает и пишет в своём глобальном конфиге. | Решение владельца от 2026-09-25: при удалённом подключении интерфейс выглядит так, как настроен у пользователя. В режиме в одном процессе оба источника совпадают, поведение не меняется. |
| **разрешение без клиентов** | Ход встаёт на паузу без таймаута и ждёт, пока подключится клиент. Демон не завершается по простою, пока есть ожидающий запрос. | Решение владельца от 2026-09-25. |
| **простой** | `options.daemon.idle_timeout`, по умолчанию 10 минут | Решение владельца от 2026-09-25. |
| **Windows** | Демон поддерживается в первой версии | Решение владельца от 2026-09-25. Задания CI фаз 2 и 3 идут и на Windows. |
| **бинарь** | Предложение, ждёт решения владельца: один бинарь `sennit` с подкомандами `daemon run` и `daemon bridge`. Код клиента живёт в пакете, который не импортирует `internal/app`, `agent`, `db`; это проверяет тест зависимостей. | Встроенный режим остаётся, поэтому клиентский бинарь всё равно содержал бы бэкенд. Supervisor запускает демон через `os.Executable()`, и локально версии клиента и демона всегда совпадают. Замер на `4c5e5612f` (`-s -w`): полный бинарь 86 МБ, UI без бэкенда 27 МБ. Вернуться к вопросу, если встроенный режим удалят или понадобится тонкий клиент. |
| **CLI-утилиты** | `doctor`, `models`, `gc`, `import`, `logs`, `session`, `stat` продолжают открывать БД и конфиг напрямую | Через демон они не нужны; удалённо их запускают по SSH. |

## Что уже готово

- **Граница UI.** `internal/ui` не линкует `agent`, `db`, `app`, `thread`;
  это проверяет `workspace/dependency_guard_test.go`. Все обращения идут
  через `workspace.Workspace` (`workspace/workspace.go:858`).
- **Один поток событий.** `AppWorkspace.Subscribe`
  (`workspace/appws/app_workspace_lifecycle.go:22`) переводит всё в 12 типов
  событий: `message.Message`, `session.Session`, `proto.Thread`,
  `permission.PermissionRequest`/`PermissionNotification`,
  `question.Request`/`Notification`, `history.File`, `skills.Event`,
  `workspace.AgentNotification`/`LSPEvent`/`MCPEvent`.
- **Кэш рядом с UI.** `ui/model/workspace_cache.go` уже выносит
  `AgentIsBusy`, `AgentModel`, `AgentIsReady`, `PermissionSkipRequests`,
  `AgentQueuedPromptsList`, LSP- и MCP-состояния в `tea.Cmd` с TTL.
- **Проверка полноты по рефлексии.** `read_only_workspace_classification_test.go`
  уже требует, чтобы каждый метод интерфейса был явно классифицирован.
  Тот же приём используется в PR 0.3 и 1.1.
- **Кодек частей сообщения.** `message.MarshalParts`/`UnmarshalParts`
  (`message/message.go:71,120`) уже восстанавливают все 8 конкретных типов
  частей; им пользуется БД.
- **Первый ответ побеждает.** `PermissionGrant/Deny` возвращают, закрыл ли
  вызов запрос; для нескольких клиентов этого достаточно после PR 0.2.
- **Прерванные ходы.** `app/interrupted.go` при старте помечает ход,
  прерванный убитым процессом, как отменённый. Для падения демона ничего
  добавлять не нужно.
- **gRPC уже в `go.sum`** косвенной зависимостью (`google.golang.org/grpc
  v1.83.2`); CGO не требуется.
- **Старый код** в `027d6155c^:internal/server/supervisor/` пригоден почти
  без изменений: проверка устаревшего сокета, single-flight запуск под
  `lock.File`, атрибуты detach для unix и Windows, ожидание готовности,
  проверка версии и BuildID.

## Что мешает (сводка исследования)

### Типы, которые не переживают JSON

| Тип | Проблема | Где |
|---|---|---|
| `message.Message` | `Parts []ContentPart`, интерфейс; нет `UnmarshalJSON` | `message/content.go:87,283` |
| `permission.PermissionRequest` | `Params any` декодируется в `map[string]any`; диалог делает type assertion на `proto.*PermissionsParams` и молча откатывается к сырому JSON | `permission/permission.go:75`, `ui/dialog/permissions.go:587-864` |
| там же | `GrantPersistent` строит ключ `json.Marshal(Params)` из копии клиента; у map ключи по алфавиту, у struct по порядку объявления, постоянное разрешение перестанет совпадать без ошибки | `permission/permission.go:485,618,678` |
| шесть Params-типов | Объявлены только в `agent/tools`, UI их импортировать не может: `WebFetch`, `WebSearch`, `AgentCancel`, `ReadMCPResource`, `ListMCPResources`, `RenameParams` | `agent/tools/fetch_types.go:33,44` и др. |
| `skills.SkillState`, `skills.Event` | `Err error` | `skills/skills.go:66-71` |
| `LSPEvent`, `MCPClientInfo`, `ModelRefreshResult`, `OAuthCompletion`, `AgentRunEvent` | поля типа `error` | `workspace/workspace.go:150,395,557,567,682,903` |
| `config.Config` | `RuntimeProviders json:"-"`, а UI читает `RuntimeProvider()` в четырёх местах; в снимке есть API-ключи и OAuth-токены | `config/config.go:568`; `ui/dialog/provider_settings.go:261`, `ui/dialog/accounts.go:423`, `ui/model/dialog_actions.go:355`, `ui/model/account_label.go:69` |
| `SetProviderAPIKey(apiKey any)` | `*oauth.Token` придёт как map и будет отклонён | `config/credential_store.go:339-366` |

### Ошибки, которые проверяет вызывающая сторона

`session.ErrNotFound` (`ui/model/update_session.go:173`,
`cmd/cmdutil.go:120`), `context.Canceled`/`DeadlineExceeded`
(`ui/model/send.go:131`, `ui/model/dialog_actions.go:273`,
`ui/model/mcp_auth.go:21`), ошибка квоты через `GetProviderQuotaInfo`
(`ui/model/send.go:132`, нужны model и settingsURL), `AgentRunEvent.Err` как
код выхода `sennit run` (`cmd/run.go:256`).

### Синхронные вызовы из `Update`/`View`

| Метод | Частота | Где |
|---|---|---|
| `WorkingDir` | каждый кадр | `ui/model/sidebar.go:127`, `layout.go:312`, `header.go:171` и ещё 8 |
| `BackgroundJobCounts` | каждый кадр | `sidebar.go:128` |
| `CurrentPlanUsage` | каждый кадр | `sidebar.go:153,242` |
| `Config()` | каждый кадр, около 55 мест | `sidebar.go:149,190`, `ui.go:1126`, `header.go:149`, `common.go:79,90` |
| `AgentIsSessionBusy` | каждое сообщение / кадр в дочерней делегации | `ui.go:678,879`, `child_session_panel.go:240` |
| `SupportsThreads`, `SupportsTasks` | по клавише | `keypress.go:30`, `child_session_nav.go:239` |
| `DockerMCPAvailable` | по «/» | `dialogs.go:188`, `dialog/commands.go:59` |
| `KnownProviders`, `ListAccounts`, `AccountCapabilities`, `CustomProviderTypes`, `OAuthConfiguredProxy` | в конструкторах диалогов | `dialog/models.go:93`, `dialog/accounts.go:80,141,337` и др. |
| `MCPPendingAuth`, `MCPAuthURL` | по событию MCP | `mcp_auth.go:67,75` |

### UI напрямую обращается к диску проекта

- `ui/completions/completions.go:659`: `fsext.ListDirectory(".")` для
  @-дополнений, от cwd процесса.
- `ui/model/editor_input.go:234-264`: @-упоминание читает файл проекта и
  сравнивает с `FileTrackerLastReadTime`.
- `ui/dialog/filepicker.go:80,110-117`: стартует в `WorkingDir()` сервера, а
  читает локальный диск.
- `fsext.PrettyPath`/`home.Short`: сокращает серверные пути по `$HOME`
  клиента.

Остаются на клиенте: вложения, выбранные пользователем
(`ui/common/attachment.go:17`), вставка и буфер обмена, `$EDITOR`,
уведомления, `browser.OpenURL`.

### Методы, возвращающие поведение

| Метод | Сейчас | Становится |
|---|---|---|
| `Subscribe(send func(any))` | блокирующий насос | server-stream с номерами событий |
| `SubscribeWith` | вне интерфейса, через type assertion (`ui/model/root.go:84`) | часть интерфейса |
| `AgentRunShellCommand(..., onProgress func(string), ...)` | колбэк | server-stream |
| `AgentRunStream` | канал | server-stream |
| `StartOAuth` → `OAuthFlow{Wait, Cancel}` | объект | хэндл потока + `OAuthWait` / `OAuthCancel` |
| `EnterWorktree`, `ExitWorktree`, `AttachThread` → `(Workspace, func())` | новый объект, UI заменяет `r.com.Workspace` (`ui/model/root.go:515-560`) | хэндл воркспейса + `ReleaseHandle` |
| `Resolver()` | интерфейс | удаляется из контракта: вызывающих в `ui` и `cmd` нет |
| `Shutdown()` | останавливает `App` | в клиенте закрывает соединение и освобождает хэндлы; демон не трогает |

### Процесс и блокировки

- `cmd/root.go:278-328` поднимает `App` в процессе и держит
  `workspacelock` на git common dir; при выходе `App.Shutdown`
  (`app/shutdown.go:211-432`) отменяет ходы, останавливает MCP, LSP,
  фоновые шеллы и треды.
- На один проект работает дерево `App`: корень, по одному на активный
  worktree (`appws/app_workspace_worktree.go:71-192`) и на живой тред
  (`app/threadspawn/spawner.go:156-183`). Демон держит всё дерево.
- `login`, `logout`, `accounts` тоже берут `workspacelock`
  (`cmd/login.go:91`, `cmd/accounts.go:46`) и при работающем демоне упадут с
  `ErrLocked`.
- БД глобальная (`~/.config/sennit`, WAL, `busy_timeout` 30 с), конфликтов
  между проектами нет.
- `pubsub` при полном буфере подписчика событие отбрасывает
  (`pubsub/broker.go:86-91`). Для удалённого подписчика это значит:
  медленный клиент теряет события и должен сделать resync.

### Отказы старой реализации и чем они закрываются здесь

| Отказ (C1 / аудиты) | Закрывается |
|---|---|
| Хэндл треда держал демон живым, `Manager` без `Shutdown` | Хэндлы привязаны к соединению (PR 1.3); простой считается по сессиям и задачам, а не по хэндлам (PR 2.1) |
| Синхронный HTTP в `Update` (`SupportsThreads` через `ListThreads`) | Класс C: геттер не делает сетевых вызовов, это проверяет тест (PR 1.4) |
| Гонка запуска демона при одновременных клиентах | single-flight под `lock.File` из старого supervisor (PR 2.2), тест с N параллельными стартами |
| Гонка остановки сервера с созданием воркспейса | Воркспейс создаётся при старте демона, а не по запросу клиента |
| «coder agent offline» при кратком обрыве | Демон не закрывает воркспейс при отключении клиента; клиент переподключается с повтором событий (PR 1.2) |
| Потерянные запросы разрешений | Повтор событий по номеру + снимок ожидающих запросов в `Hello` (PR 1.2) |
| `logout` всегда поднимал демон | Команды учётных записей не запускают демон (PR 2.3) |
| Клиентский и серверный слои расходились | Оба генерируются из интерфейса (PR 1.1); тест полноты классов |

## Классы методов

Каждый метод `FrontendWorkspace` получает один класс в таблице
`internal/workspace/wsrpc/classes.go`. Тест по рефлексии падает, если метод
без класса или класс без метода.

| Класс | Смысл | Реализация на клиенте |
|---|---|---|
| **U** | Унарный вызов, данные | Генерируется |
| **U!** | Унарный, но сейчас без `error`/`ctx`; сигнатура меняется в PR 0.1 | Генерируется после PR 0.1 |
| **C** | Геттер, который UI зовёт из `Update`/`View` | Читает кэш клиента (снимок + события), сети не касается |
| **S** | Поток | Пишется руками |
| **H** | Возвращает или использует хэндл | Пишется руками |
| **X** | Уходит из контракта или остаётся локальным | — |

Пометка **T** означает, что до генерации нужна правка типа из PR 0.2.

| Роль | Метод | Класс |
|---|---|---|
| SessionStore | `CreateSession`, `ListSessions`, `GetLastSession`, `RenameSession`, `DeleteSession`, `SessionDescendantCost` | U |
| | `GetSession` | U (T: код `ErrNotFound`) |
| | `SetCurrentSession`, `SetCurrentSessionGeneration` | U, по клиенту (PR 1.5) |
| | `ListMessages`, `ListMessagesBySessionIDs`, `ListUserMessages`, `ListAllUserMessages` | U (T: `Message`) |
| AgentController | `AgentRun` | U (T: коды `Canceled` и квоты) |
| | `AgentRunShellCommand`, `AgentRunStream` | S |
| | `AgentCancel`, `AgentClearQueue` | U! |
| | `AgentIsBusy`, `AgentIsSessionBusy`, `AgentModel`, `AgentIsReady`, `AgentReadyErr`, `AgentQueuedPromptsList` | C |
| | `AgentSummarize`, `UpdateAgentModel`, `ApplySessionModel`, `InitCoderAgent`, `InitCoderAgentNonInteractive` | U |
| | `ResetAgentToolCache` | X: переносится в сервер, в обработку новой сессии (кэш процессный, при двух клиентах один сбросил бы его другому) |
| UsageReporter | `Stats` | U |
| PermissionResolver | `PermissionGrant`, `PermissionGrantPersistent`, `PermissionDeny` | U! (T: по ID запроса) |
| | `PermissionSkipRequests` | C |
| | `PermissionSetSkipRequests` | U! |
| QuestionResponder | `QuestionAnswer` | U! |
| | `QuestionCancel` | U! (+ `batchID`) |
| FileServices | `UncommittedFiles`, `FileTrackerListReadFiles`, `ListSessionHistory` | U |
| | `FileTrackerRecordRead`, `FileTrackerLastReadTime` | U! |
| LSPController | `LSPStart`, `LSPStopAll` | U! |
| | `LSPGetStates`, `LSPGetDiagnosticCounts` | U (уже в `tea.Cmd`) |
| ConfigReader | `Config` | C (снимок без секретов, PR 0.5) |
| WorkingDirectory | `WorkingDir` | C (атрибут хэндла) |
| ConfigResolver | `Resolver` | X |
| ConfigFieldEditor | `SetConfigField`, `RemoveConfigField`, `SetCompactMode` | U |
| Accounts | `RecordAccount`, `ActivateAccount`, `UpdateAccount`, `RemoveAccount`, `PurgeAccounts`, `SetProviderProxy`, `RefreshAccountLimits` | U |
| | `ListAccounts` | U, вызов уходит из конструктора диалога в `tea.Cmd` |
| | `CurrentPlanUsage`, `AccountCapabilities` | C |
| ModelsRefresher | `RefreshProviderModels` | U (T: `Err`) |
| PreferredModelUpdater | `UpdatePreferredModel`, `OverridePreferredModel` | U |
| ProviderAPIKeySetter | `SetProviderAPIKey` | U! (`apiKey string`) |
| CustomProviderConfigurer | `ConfigureCustomProvider` | U |
| ProviderCatalog | `VerifyProviderAPIKey` | U |
| | `KnownProviders`, `CustomProviderTypes` | C |
| OAuthController | `StartOAuth` | H |
| | `CompleteOAuth` | U (T: ошибки в полях) |
| | `OAuthConfiguredProxy`, `OAuthValidateProxy`, `RefreshOAuthToken`, `RefreshOAuthTokenForAccount` | U |
| | `ImportCopilot` | U! (`ctx`, `error`) |
| ProjectLifecycle | `ProjectNeedsInitialization`, `MarkProjectInitialized`, `InitializePrompt`, `ListSkills`, `ReadSkill`, `ConfigProblems`, `BuiltinSkills`, `DoctorProblems`, `ListCustomCommands` | U |
| | `SkillStates` | U (T: `Err`) |
| MCPController | `WaitForMCPInit`, `MCPResources`, `ReadMCPResource`, `ListMCPPrompts`, `EnableDockerMCP`, `DisableDockerMCP` | U |
| | `MCPGetStates` | U (T: `Error`) |
| | `MCPAuthenticate` | U (T: коды `Canceled`/`DeadlineExceeded`) |
| | `MCPRefreshPrompts`, `MCPRefreshResources`, `RefreshMCPTools`, `RefreshDockerMCPAvailability` | U! |
| | `GetMCPPrompt` | U! (`ctx`) |
| | `DockerMCPAvailable`, `MCPPendingAuth`, `MCPAuthURL` | C |
| WorktreeController | `EnterWorktree`, `ExitWorktree` | H |
| ThreadController | `ListThreads`, `CreateThread`, `ActivateThread`, `CancelThread`, `RemoveThread` | U |
| | `SupportsThreads` | C |
| | `AttachThread` | H |
| TaskController | `ListTasks`, `CancelTask` | U |
| | `SupportsTasks` | C |
| BackgroundJobs | `BackgroundJobCounts` | C |
| EventSubscriber | `Subscribe`, `SubscribeWith` | S |
| | `Shutdown` | X (клиентский) |
| новые (PR 0.6) | `ListProjectFiles`, `AttachProjectFile` | U |
| новые (PR 1.1–1.3) | `Hello`, `Snapshot`, `ReleaseHandle`, `OAuthWait`, `OAuthCancel` | U / S |

## Фаза 0. Контракт, пригодный для провода

Все PR этой фазы работают в текущем процессе, транспорта ещё нет. Каждый
полезен и сам по себе, фазу можно остановить после любого PR.

### PR 0.1. Сигнатуры, которые могут сообщить об ошибке

**Сделано** (`287299dbb`, `b7e39af67`, `caf930146`). По ходу исправлены два
живых дефекта: отмена вопроса без ID могла отменить чужой вопрос, а
отсоединение треда отменяло вопрос родителя. Ошибки ответа на вопрос,
переключения yolo и ответа на разрешение теперь видны пользователю.

Правило из AGENTS.md: обёртка, которая возвращает `bool` или пустой
результат там, где операция может упасть, обманывает вызывающего. По сети
может упасть любая операция.

- Добавить `error` методам класса U!: `AgentCancel`, `AgentClearQueue`,
  `PermissionGrant`/`GrantPersistent`/`Deny` (`(bool, error)`),
  `PermissionSetSkipRequests`, `QuestionAnswer`, `QuestionCancel`,
  `FileTrackerRecordRead`, `FileTrackerLastReadTime`, `LSPStart`,
  `LSPStopAll`, `MCPRefreshPrompts`, `MCPRefreshResources`,
  `RefreshMCPTools`, `RefreshDockerMCPAvailability`, `ImportCopilot`.
- Добавить `ctx` в `GetMCPPrompt` и `ImportCopilot`.
- `QuestionCancel(batchID string)`: сейчас метод отменяет «текущий» вопрос,
  при двух клиентах это неоднозначно (`ui/model/dialogs.go:533`,
  `ui/model/root.go:152`).
- `SetProviderAPIKey(scope, providerID, apiKey string)`; ветка с
  `*oauth.Token` остаётся внутренней для `config`.
- `SubscribeWith(send func(any)) (stop func())` вносится в `EventSubscriber`;
  type assertion в `ui/model/root.go:84` и `appws/attached_thread.go:129`
  уходит.
- `Resolver()` удаляется из `FrontendWorkspace`; `ConfigureCustomProviderUsing`
  получает resolver параметром от `appws`.
- `ResetAgentToolCache` уходит из контракта, сброс выполняет `appws` при
  создании и переключении сессии.
- Все вызывающие в `ui` и `cmd` начинают обрабатывать новую ошибку (лог +
  уведомление там, где пользователь ждёт результата).

Объём: широкий, но механический; около 60 вызовов. Проверка:
`read_only_workspace` и его тест классификации обновлены, `go vet` чист.

### PR 0.2. Типы, переживающие JSON

**Сделано** (`d2fc8c404`, `b7b095b47`, `657df8bac`, `3fdf46bcd`). Отличия от
текста ниже:
- Тип ошибки живёт в листовом пакете `internal/wireerr` (`wireerr.Error`),
  не в `workspace`: его использует `skills.SkillState`, а `workspace`
  импортирует `skills`. `*wireerr.Error` намеренно не реализует `error`,
  чтобы ловушку typed nil ловил компилятор; отображение через `Text()`,
  идентичность через `workspace.DecodeError`.
- `workspace` не может импортировать `modelsrefresh` (тянет `db`), поэтому
  у него свой сентинел `workspace.ErrDiscoveryDisabled`; код
  `discovery_disabled` ставит `appws`.
- Снимок истории делегации (`agent/delegation_execution.go`, колонка
  `threads.execution`) сохраняет прежний формат через отдельный тип
  `delegationMessage`: новый кодек `Message` его бы молча изменил.
- Найдено сверх плана: сервис разрешений строил постоянное разрешение из
  полей копии клиента. Исправлено отдельным коммитом `sec:`.
- `lsp_rename` получил свой `proto.RenamePermissionsParams`.

- `message.Message`: `MarshalJSON`/`UnmarshalJSON`, делегирующие `Parts` в
  `MarshalParts`/`UnmarshalParts`.
- `permission.PermissionRequest.UnmarshalJSON`: реестр
  `toolName → reflect.Type` в `proto` (там же, где типы Params). `mcp_*`
  остаётся строкой, неизвестное имя декодируется в `map[string]any` с
  записью в лог. Для шести типов из `agent/tools` завести `proto`-типы и
  алиасы назад, как уже сделано для остальных (`type X = proto.X`); тест
  идентичности в `agent/tools/proto_identity_test.go` дополняется.
- `Grant`/`GrantPersistent`/`Deny` на стороне сервиса ищут ожидающий запрос
  по `ID` и берут Params из него, копию клиента не используют. Это закрывает
  тихую поломку ключа постоянного разрешения.
- `workspace.WireError{Code, Message, Quota *ProviderQuotaInfo}` с
  `Error()`, `Is()` и `QuotaInfo()`; заменяет поля `error` в `LSPEvent`,
  `MCPClientInfo`, `ModelRefreshResult`, `OAuthCompletion`, `AgentRunEvent`,
  `skills.SkillState`/`Event`. Пустое значение означает «нет ошибки».
- `workspace.EncodeError(error) *WireError` / `DecodeError(*WireError) error`:
  коды для `context.Canceled`, `context.DeadlineExceeded`,
  `session.ErrNotFound`, сентинелов `workspace.go:64-87`,
  `ErrReadOnlyOperation`, ошибки квоты, `internal` для остального.
  `DecodeError` возвращает значения, на которых `errors.Is`/`errors.As` и
  `GetProviderQuotaInfo` дают тот же ответ, что на исходной ошибке.

Проверка: для каждого исправленного типа тест кодирует и декодирует и
сравнивает `require.Equal`; тест диалога разрешений
(`TestDiffContentRenderer_GuardStopsBeforeToDiff` и соседи) прогоняется на
запросе, прошедшем через JSON. Перед мержем вернуть `Params any` без реестра
и убедиться, что тест диалога краснеет.

### PR 0.3. Тест полноты DTO

**Сделано** (`5cbeb464f`, ужесточён в `6dc95fbbc`). Нашёл `MCPState` без `UnmarshalText`.

Тест обходит метод-сет `FrontendWorkspace` и типы событий из `translateEvent`,
рекурсивно собирает все типы параметров и результатов и требует для
каждого образец в таблице `samples_test.go`. Для каждого образца:
encode → decode → `require.Equal`. Новый тип без образца роняет тест.
Интерфейсные поля, `func`, `chan` и `error` вне `WireError` роняют тест с
именем поля.

### PR 0.4. UI не зовёт воркспейс из `Update`/`View`

**Сделано** (`695b676d2`). Страж нашёл шесть синхронных вызовов, включая проверку прокси OAuth Codex по Enter.

**Уточнено 2026-09-26.** Методы класса C в клиенте PR 1.4 читают локальный
кэш и сети не касаются, поэтому их синхронные вызовы из `Update`/`View`
остаются. PR 0.4 убирает синхронные вызовы классов U, S и H и добавляет
тест-страж. Новые события (`BackgroundJobsChanged`, `PlanUsageChanged`,
занятость сессий, `ConfigChanged`) переезжают в PR 1.2/1.4, где у них
появляется потребитель: событие, которое никто не слушает, было бы мёртвым
механизмом. Текст ниже сохранён как исходный замысел.

- Новые события, публикуемые `app` и переводимые `translateEvent`:
  `ConfigChanged` (снимок, см. PR 0.5), `BackgroundJobsChanged`,
  `PlanUsageChanged{ProviderID, Usage}`, `SessionBusyChanged{SessionID,
  Busy}`. Для последнего проверить, хватает ли существующих
  `TurnStarted`/`Finished`; если между ними есть окна (очередь,
  суммаризация), нужен явный набор занятых сессий.
- `ui/model/workspace_cache.go` получает поля для всех методов класса C и
  обновляет их по событиям; прямые вызовы в таблице «Синхронные вызовы»
  заменяются чтением кэша.
- Вызовы в конструкторах диалогов (`KnownProviders`, `ListAccounts`,
  `AccountCapabilities`, `CustomProviderTypes`, `OAuthConfiguredProxy`)
  переносятся в `tea.Cmd` или в кэш; диалог открывается с индикатором
  загрузки.
- `update_settings.go:328` перестаёт полагаться на то, что указатель
  `Config()` обновляется на месте: он ждёт `ConfigChanged`.

Проверка: тестовый `Workspace`, у которого каждый метод класса C паникует,
если вызван не из горутины `tea.Cmd`. Реализовать через флаг в контексте
`Update`/`View` в тестовом харнессе `ui/model`.

### PR 0.5. Снимок конфигурации для фронтенда

**Сделано** (`2ee9e5a04`, `6dc95fbbc`). Снимок кэшируется по указателю опубликованного конфига.

**Уточнено 2026-09-26 по разбору.** UI читает `Config()` в 57 местах. Одно из
них реально ломается по сети: пять проверок авторизации через
`RuntimeProvider()` читают поле `RuntimeProviders` с `json:"-"`. Выбран
allowlist-DTO вместо отредактированного клона `*config.Config`: новый секрет
в конфиге тогда не утечёт по умолчанию, а методы, читающие скрытое
состояние, недоступны UI по построению.

- `workspace.FrontendConfig{Model, RecentModels, Providers []FrontendProvider,
  MCPNames, Agents, InitializeAs, DisabledSkills}` с теми методами, которые UI
  уже вызывает (`GetModel`, `ProviderName`, `SelectedCatalogModel`, ...).
  `FrontendProvider` несёт `ProviderAuth{Known, HasAPIKey, HasOAuth,
  OAuthExpiresAt, Account}` и `ProxyURL` без пароля; `Custom` считает сервер.
- `Config()` возвращает `*FrontendConfig`; `*config.Config` остаётся у `cmd`
  и `readOnlyWorkspace`. Проверка импортов не даёт `ui/model` и `ui/dialog`
  вызывать методы `*config.Config`.
- Тест полноты: `config.Config`, `providerstate.Provider`,
  `providerconfig.ProviderConfig`, `oauth.Token`, `csync.Map` запрещены на
  проводе; у типа с методами не может быть полей `json:"-"`; каждый метод
  `FrontendConfig` сравнивается на исходном и декодированном значении.
- Тест секретов: все строковые поля `config.Config` заполняются маркерами,
  в JSON снимка могут остаться только разрешённые.
- Порядок: сначала 0.5b, чтобы в DTO не попали UI-настройки.

Текст ниже сохранён как исходный замысел.

- `config.Config.FrontendSnapshot()` возвращает клон, где API-ключи
  заменены маркером наличия, а OAuth-токены обнулены с сохранением флага
  `HasOAuth`. Перед реализацией прочитать четыре места, где UI зовёт
  `RuntimeProvider()`, и перечислить поля, которые ему нужны.
- `RuntimeProviders` для UI: либо сериализуемый DTO в снимке, либо узкий
  метод на воркспейсе. Выбор делается по тому, что читают четыре места.
- `Config()` в `AppWorkspace` начинает возвращать снимок. В процессе это
  дешевле не станет, зато UI с первого дня видит то же, что увидит по сети.

Проверка: снимок проходит тест PR 0.3; отдельный тест ищет в JSON снимка
тестовые значения ключа и токена и падает, если находит.

### PR 0.5b. UI-настройки на стороне клиента

**Сделано** (`e55f200ed`).

**Уточнено 2026-09-26.** `internal/uiprefs`: `Prefs` + `Store{Prefs(), Set(key,
v), Subscribe}`; UI получает его через `common.Common`, отдельно от
`Workspace`. Во встроенном режиме адаптер оборачивает `*config.ConfigStore`
приложения и пишет через `SetConfigField(ScopeGlobal, ...)`, как сейчас;
проектные переопределения работают как сейчас. Удалённо клиент грузит
только глобальный конфиг (`config.LoadGlobal()`). Умолчание прозрачности
(Apple Terminal) считается на клиенте; умолчание лимитов дополнений зависит
от проекта и остаётся серверным.

- Перечислить поля `config.Config`, которые относятся только к
  отображению: `ThemeID`, `SpinnerMode`, `Scrollbar`, `Keybindings`,
  `CompactMode`, `DiffMode`, `TransparentEnabled`, `CompletionsLimits`,
  `Options.Progress`, `Options.Notifications`. Перед началом сверить список
  с местами чтения в `internal/ui` и с тем, что из этого читает сервер
  (например, `Options.Notifications` может использоваться агентом).
- `internal/uiprefs`: тип `Prefs` с этими полями, загрузка из глобального
  конфига клиента (тот же `sennitrc`/`sennit.json`, те же ключи) и запись
  через `config.ConfigStore` клиента. Проектный слой конфига для этих полей
  при удалённом подключении недоступен: он лежит на сервере. Это
  фиксируется в документации.
- UI читает эти поля из `Prefs`, а не из `Config()`; `SetCompactMode` и
  запись темы и клавиш из диалогов идут в `Prefs`. `SetCompactMode`
  уходит из `ConfigFieldEditor`.
- В режиме в одном процессе `Prefs` загружается из того же конфига, что и
  `App`, и обновляется по тому же событию перезагрузки. Проверка: тест
  меняет тему через диалог и видит изменение в файле глобального конфига,
  как сейчас.

### PR 0.5c. Аккаунты без токенов в UI

**Сделано** (`4da2ece1d`). Заодно формы аккаунта и настроек провайдера перестали записывать поля, которые пользователь не менял: иначе прокси без пароля затирал сохранённый.

Найдено тестом секретов 2026-09-26. `ListAccounts`, `RefreshAccountLimits`,
`RecordAccount` и `OAuthCompletion.Account` отдают UI `accounts.Account`
целиком, с `Token` (access и refresh) и `APIKey`, а `UpdateAccount`
принимает аккаунт от UI и сохраняет его токен. UI читает из токена только
срок (`ui/dialog/accounts.go:582-612`).

- `workspace.FrontendAccount{ID, Label, AccountID, Email, ProxyURL,
  Disabled, Usage, HasToken, TokenExpiresAt, TokenExpiresIn, HasAPIKey}`
  для всех методов, возвращающих аккаунты UI.
- `UpdateAccount` заменяется узким изменением полей, которые UI правит
  (метка, прокси, отключение: сверить с `ui/model/dialog_actions.go:390` и
  `cmd/accounts.go:224`); токен и ключ сервер берёт из сохранённого
  аккаунта.
- Запись `accounts.Account.Token` в `forbiddenTypeAllowList` удаляется.

Смежное, в PR 1.3: токен входа (StartOAuth → UI → CompleteOAuth,
ImportCopilot → RecordAccount) не должен проходить через UI; сервер
завершает поток за хэндлом.

### PR 0.6. Файлы проекта только через воркспейс

**Сделано** (`cf12dee95`).

- `ListProjectFiles(ctx, dir string, depth, limit int) ([]string, error)`:
  замена `fsext.ListDirectory(".")` в `ui/completions/completions.go:659`,
  с тем же фильтром ignore.
- `AttachProjectFile(ctx, sessionID, path string) (message.Attachment, bool,
  error)`: переносит в сервис логику `ui/model/editor_input.go:234-264`
  (Abs, Stat, сравнение с трекером, чтение); `bool` сообщает «файл уже
  прочитан агентом и не менялся».
- `Hello` пока не существует, поэтому `ServerHome()` добавляется в снимок
  конфигурации или в `WorkingDirectory`; `fsext.PrettyPath` для серверных
  путей в UI получает домашний каталог параметром.
- Файлпикер стартует в cwd клиента, а не в `WorkingDir()`: он выбирает файлы
  с диска пользователя.

### PR 0.7. Генератор и loopback

**Сделано** (`6f242da4f`, `ac5d90663`, и коммит 0.7c). Отличия и находки:
- Таблица классов живёт в `internal/workspace/wsrpc/classes.go`; генератор
  форматирует вывод библиотекой gofumpt, тест свежести не требует бинаря.
- Реестр событий `wsrpc/events.go`; `translateEvent` больше не пропускает
  в UI `notify.RunComplete` и `app.WorkspaceChanged`.
- Возможности, которые UI находил приведением типа
  (`SessionChangePreparer`, `WorktreeState`), вошли в контракт; тест
  `TestUIDoesNotTypeAssertWorkspace` запрещает такие приведения в
  `internal/ui`.
- Ограничение задания `wire`: тесты UI подают события прямо в `Update`,
  минуя `Subscribe`, поэтому кодек событий там не проверяется. Его
  покрывает `wsrpc` `TestEventRegistry_RoundTrips`; настоящую проверку даст
  PR 1.6 (сквозной поток через gRPC).
- `internal/cmd` пока не строит настоящий воркспейс ни в одном тесте, так
  что для него задание `wire` сейчас ничего не проверяет.

Открыто на следующие фазы:
- PR 1.3: токен входа OAuth ходит UI туда и обратно (записи
  `forbiddenTypeAllowList` в `wire_dto_test.go`).
- PR 2.3: `cmd/server_config.go` и `cmd/root.go` `uiPrefsStore` работают
  только с `AppWorkspace`; для команд через демон нужен другой путь.
- Генератор жёстко задаёт `LangVersion` go1.27.0; при смене `go` в
  `go.mod` обновить.

- `internal/workspace/wsrpc/gen`: `go:generate` читает
  `workspace.FrontendWorkspace` через `go/packages`, берёт классы из
  `classes.go` и для U и U! генерирует:
  - структуры запроса и ответа на метод (`XxxRequest{Args}`,
    `XxxResponse{Results, Err *WireError}`);
  - `loopback.go`: декоратор `Workspace`, который прогоняет аргументы и
    результаты каждого вызова через JSON-кодек и вызывает внутренний
    `Workspace`. События прогоняются через тот же кодек и реестр типов
    событий.
- CI: `go generate ./internal/workspace/wsrpc/... && git diff --exit-code`.
- Задание CI `SENNIT_TEST_WIRE=1`: тесты `ui/model`, `cmd` и интеграционные
  тесты `appws` получают loopback вместо прямого `AppWorkspace`.

Здесь фаза 0 проверяется целиком: всё, что сломается на проводе из-за
типов, сломается в loopback без сети. Перед мержем удалить реестр Params из
PR 0.2 на копии дерева и убедиться, что задание краснеет.

## Фаза 1. gRPC

### PR 1.1. Унарный транспорт

**Сделано.** Отличие от текста: gRPC-код живёт в подпакете
`internal/workspace/wsrpc/grpcws`, а `wsrpc` остаётся без gRPC, потому что
UI импортирует `wsrpc` ради таблицы классов. Тест
`ui/model/grpc_dependency_guard_test.go` запрещает UI линковать gRPC.
Общие заглушки для тестов loopback и gRPC лежат в `wsrpc/wsrpctest`.
Кодек: v1 `encoding.Codec` с именем `json`.

- `wsrpc/codec.go`: `encoding.RegisterCodec` с именем `json`; клиент
  выставляет `grpc.CallContentSubtype("json")`.
- Генератор дополнительно выпускает `grpc.ServiceDesc`
  (`sennit.workspace.v1.Workspace`), серверный адаптер и клиентский тип,
  реализующий методы U/U!.
- Хэндл воркспейса передаётся в metadata `sennit-handle`; пустой означает
  корень. Сервер разрешает его через реестр из PR 1.3 (в этом PR реестр
  содержит только корень).
- Ошибки: `WireError` в JSON кладётся в trailer `sennit-error`, код gRPC
  выбирается по коду `WireError`. Транспортные ошибки клиент превращает в
  `WireError{Code: "unavailable"}`, `errors.Is(err, ErrServerUnreachable)`.
- Методы без `ctx` на клиенте получают контекст с таймаутом из
  `options.daemon.call_timeout` (по умолчанию 30 с).
- Health: стандартный `grpc_health_v1` (он работает на своём proto-кодеке,
  это не мешает).
- `Hello` → `{ProtocolVersion, BuildID, Version, WorkingDir, ServerHome,
  Capabilities}`. Несовпадение `ProtocolVersion` клиент показывает текстом
  «демон версии X, выполните `sennit daemon restart`».

Проверка: тест полноты: каждый метод класса U/U! есть в `ServiceDesc`,
каждый RPC соответствует методу. Тесты поверх `bufconn`.

### PR 1.2. Поток событий с повтором

**Статус.** Поток событий с повтором и `Resync` сделан в `514a1bfe2`;
`AgentRunStream`/`AgentRunShellCommand` в `3fec9a4b6`, но с неверной
семантикой обрыва (см. ниже, исправляет 1.2c). `Snapshot` и обработка
`Resync` клиентом переехали в PR 1.4.

- На сервере для каждого хэндла кольцевой буфер событий с монотонным `Seq`
  (размер из опции, по умолчанию 4096).
- `Subscribe(SubscribeRequest{FromSeq}) returns (stream Envelope)`;
  `Envelope{Seq, Type, Payload json.RawMessage}`; тип выбирается по реестру
  из PR 0.7.
- Если `FromSeq` старше буфера, первым приходит `Resync`; клиент
  перечитывает сессию, ожидающие разрешения и вопросы, снимок кэша.
- Подписчик на сервере читает `pubsub` в собственную горутину и пишет в
  буфер; медленный gRPC-клиент не тормозит `pubsub`, отстав больше буфера,
  он получает `Resync`.
- `Snapshot` RPC: `{Seq, Config, AgentState, BusySessions, BackgroundJobs,
  PlanUsage, MCPPendingAuth, PendingPermissions, PendingQuestions,
  Capabilities}` одним ответом; клиент подписывается с `Seq` снимка.
  **Уточнено ревью (п. 2), делается в PR 1.4:** сервер фиксирует `Seq`
  (последний опубликованный номер) до сбора состояния, клиент подписывается
  с `Seq+1`; события, пришедшие между фиксацией и сбором, повторяются, поэтому
  применение события к кэшу идемпотентно (полные значения, а не дельты;
  для сообщений и сессий сравнение `UpdatedAt`). После `Resync` клиент
  запрашивает новый снимок и продолжает с его `Seq+1`. Тест: событие,
  опубликованное между снимком и подпиской, отражено в кэше ровно один
  раз и не откатывает более новое состояние.
- `AgentRunStream`, `AgentRunShellCommand`: server-stream. **Решение после
  ревью 2026-09-26 (`CLIENT-SERVER-REVIEW.md`, п. 1):** время жизни хода не
  зависит от потока. Сервер запускает ход на отсоединённом контексте
  (`context.WithoutCancel`), поток только наблюдает. Отмена `ctx` вызывающим
  на клиенте сначала шлёт `AgentCancel(sessionID)`, затем закрывает поток:
  для `sennit run` Ctrl-C по-прежнему останавливает ход. Обрыв соединения
  закрывает только наблюдение, ход доживает, его видно через `attach`.
  Серверная горутина дочитывает канал хода до конца, чтобы не течь.
  Реализовано в `3fec9a4b6` иначе; исправлено шагом 1.2c. Shell-команды
  (`AgentRunShellCommand`) оставлены на контексте потока: отменить их можно
  только через контекст, отдельного `Cancel` по ID нет, и отсоединённая
  команда стала бы неотменяемой по сети. Обрыв соединения останавливает
  запущенную `!`-команду.
- Паника в обработчике потока закрывает только этот поток. Сейчас
  `Subscribe` передаёт `w.app.Shutdown` как `onPanic`
  (`appws/app_workspace_lifecycle.go:23`); в демоне это недопустимо.

Проверка: тест обрывает соединение посреди хода, поднимает новое с
`FromSeq`, сверяет, что последовательность сообщений совпадает с
безобрывной; тест переполнения буфера получает `Resync`.

### PR 1.3. Хэндлы

**Статус.** 1.3a (хэндлы worktree и тредов, аренда) сделан в `d2aefc5e2`.
1.3b-1 (контракт: `Wait` возвращает сохранённый `OAuthCompletion`,
`CompleteOAuth` убран, `RecordAccount` принимает `AccountCredential` без
токена, `sennit login copilot` идёт через `StartOAuth` после попытки
`ImportCopilot`) сделан; 1.3b-2 (`StartOAuth`/`OAuthWait`/`OAuthCancel` по
gRPC, реестр pending-флоу с той же арендой, что и хэндлы) сделан. Открыто:
`ImportCopilot` обменивает токен без прокси провайдера (старый CLI учитывал
прокси); `sennit login copilot --force` при уже настроенном Copilot идёт в
device flow, а не берёт токен с диска. 1.3c (keepalive и настоящий тест
обрыва) сделан в `2e2a9005f`. PR 1.3 закрыт.

- Реестр `handle → Workspace` в демоне. Корень создаётся при старте.
- `EnterWorktree`/`ExitWorktree`/`AttachThread` на сервере вызывают
  `AppWorkspace`, кладут результат в реестр и возвращают `{Handle,
  WorkingDir}`; функция освобождения хранится рядом.
- `ReleaseHandle(handle)`. Хэндлы, выданные соединению, освобождаются при
  его закрытии после льготного периода (10 с, как в старом коде), чтобы
  переподключение их подхватывало. Тред при этом продолжает работать: хэндл
  держит просмотр, а не тред.
- **Уточнено ревью (п. 3):** присутствие клиента = живое соединение,
  подтверждённое keepalive, а не только открытые RPC. Сервер и клиент
  включают gRPC keepalive (сервер: `keepalive.ServerParameters{Time,
  Timeout}` и `EnforcementPolicy`; клиент: `keepalive.ClientParameters`),
  иначе полуоткрытое соединение через SSH держит хэндлы вечно. Тест: поток
  `Subscribe` и ход открыты, соединение рвётся без переподключения
  (dialer, который после обрыва отказывает) → по истечении льготного
  периода хэндлы освобождены, ход продолжает работать. Тест аренды из
  `d2aefc5e2` этого не проверяет: там переподключение глушится остановкой
  подписки. Делается шагом 1.3c.
- `StartOAuth` возвращает `{Result, FlowHandle}`; `OAuthWait(FlowHandle)`
  (server-stream с одним сообщением, чтобы ctx клиента отменял ожидание) и
  `OAuthCancel(FlowHandle)`. Поток OAuth, чей клиент отключился, отменяется
  по тому же льготному периоду.
- `ui/delegations/dock.go:174` делает короткий attach на каждый TTL; по сети
  это создание хэндла. Проверить, хватает ли ему `GetSession` по ID сессии
  треда без attach, и если да, заменить.

Проверка: утечка хэндлов: после N attach/detach и N обрывов реестр пуст;
тред, у которого отключился единственный зритель, доходит до конца.

### PR 1.4. Клиент `remote.Workspace`

**Дизайн, уточнён 2026-09-26.** Вместо отдельного события на каждый геттер
класса C (их 20, пять с параметром) сервер публикует одно событие
`workspace.ClientState` с полным состоянием: занятость (общая и множество
занятых сессий), очереди занятых сессий, модель и готовность агента
(`AgentReadyErr` как `*wireerr.Error`), пропуск разрешений, `FrontendConfig`,
`WorkingDir`, лимиты по провайдерам, возможности аккаунтов по известным
провайдерам, Docker MCP, ожидающая MCP-авторизация с URL, `WorktreeState`,
`SupportsThreads/Tasks`, счётчики фоновых задач. У состояния есть
монотонная `Version`. Сервер пересчитывает его после каждого события хаба
и по таймеру (1 с) и публикует, только если оно изменилось. Клиент
применяет состояние целиком и отбрасывает версии не новее текущей: это
делает идемпотентность из п. 2 ревью свойством по построению.

- **1.4a, сервер.** Хаб запускается при старте сервера, а не при первой
  подписке. Построитель `ClientState` из `Workspace` (в `wsrpc`, общий для
  loopback и gRPC). Публикатор состояния. RPC `Snapshot`: сервер фиксирует
  последний опубликованный `Seq`, затем собирает `{Seq, State,
  PendingPermissions, PendingQuestions}`; клиент подписывается с `Seq+1`.
  Ожидающие разрешения и вопросы в снимке нужны, чтобы UI, подключившийся к
  демону, увидел запрос, который ждёт без клиентов.
- **1.4b, клиент.** `grpcws.Client` получает кэш: при подключении снимок,
  затем внутренний насос событий, который обновляет кэш и раздаёт события
  подписчикам `Subscribe`/`SubscribeWith`. Геттеры класса C читают только
  кэш. После `Resync` и `Recovered` берётся новый снимок, ожидающие
  запросы из него отдаются подписчикам как события.
- **1.4c, UI.** Обработка `ConnectionEvent`: индикатор в строке статуса;
  после `Recovered`/`Resync` перечитываются текущая сессия, список сессий
  и делегаций. Применение событий `message.Message`/`session.Session` не
  откатывает более новое состояние (сравнение `UpdatedAt`).

- `internal/workspace/remote`: сгенерированные методы U/U!, ручные S и H,
  кэш класса C.
- Кэш заполняется `Snapshot` и обновляется событиями. Методы класса C
  читают только кэш.
- Состояние соединения публикуется в UI событием `ConnectionEvent{Lost |
  Recovered | VersionMismatch}`. Обработчик был удалён в `3c3a7dc12`, его
  можно взять оттуда. `Recovered` вызывает resync.
- Переподключение с backoff 250 мс…10 с, как в старом клиенте
  (`81785e27c^:internal/workspace/client_workspace.go:1142-1300`).

Проверка: тест закрывает соединение и вызывает каждый метод класса C; все
возвращаются без ошибки быстрее 1 мс. Бюджет времени не проверять под
`-race` (см. память о wall-clock бюджетах), под `-race` проверять только
отсутствие сетевых вызовов через счётчик на клиенте.

### PR 1.5. Несколько клиентов

**Уточнено 2026-09-26.** `SetCurrentSession` не маршрутизирует события: он
сообщает текущую сессию в herdr (`app.ReportCurrentSession`), интеграцию с
панелью терминала. Хранить его по клиенту на сервере незачем. В режиме
демона herdr относится к клиенту: панель принадлежит терминалу, в котором
запущен UI. Отсюда два пункта фазы 2: (1) supervisor запускает демон без
переменных herdr (как `ea751a79c` делает для дочерних процессов), иначе
демон навсегда привяжется к панели первого клиента; (2) клиент ведёт herdr
сам по потоку событий (начало и конец хода, запрос разрешения). В PR 1.5
остаётся проверка двух клиентов через gRPC: оба видят запрос, первый ответ
побеждает, оба окна закрываются, вопросы так же.

- Клиентский ID выдаётся в `Hello`, передаётся в metadata.
- `SetCurrentSession` хранится по клиенту; сейчас в `AppWorkspace` это
  no-op, семантику брать из старого `backend.go:961-993` (отказ клиенту без
  живого потока и устаревшему поколению).
- Разрешения и вопросы рассылаются всем подписчикам, первый ответ
  побеждает; остальные закрывают модальное окно по
  `PermissionNotification` / `question.Notification`.

Проверка: два клиента, один запрос разрешения, оба отвечают одновременно;
одно `true`, оба окна закрыты. (Для старой реализации это был
`multiclient_test.go`.)

### PR 1.6. Задание CI поверх gRPC

**Сделано.** `SENNIT_TEST_WIRE=grpc` обслуживает воркспейс настоящим
`grpcws.NewServer` по bufconn (`grpcws/grpcwstest.ServeGRPC`); задание
`wire` в CI гоняет оба режима. `ui/model/wire_event_path_test.go`
проводит события через `SubscribeWith` → gRPC → `Update` и закрывает
ограничение PR 0.7 (сломанный декодер `Params` роняет его в режиме grpc).
Найдено: `MCPPendingAuth` падал на nil. Известное ограничение:
`-race` вместе с `SENNIT_TEST_WIRE=grpc` находит гонки в несинхронизированных
счётчиках тестовых заглушек `ui/model` (тикер состояния читает их в фоне);
ни одно задание CI это сочетание не запускает.

**Фаза 1 закрыта.**

`SENNIT_TEST_WIRE=grpc`: те же пакеты, что в PR 0.7, но через
`bufconn` + `remote.Workspace` + серверный адаптер. После этого PR есть три
режима прогона: прямой, loopback, gRPC. Прямой и gRPC обязательны в CI,
loopback можно убрать, если он не находит ничего сверх gRPC.

## Фаза 2. Демон

### PR 2.1. `sennit daemon run`

- Bootstrap как в `cmd/root.go:278-328` (`WorkspaceLock: true`,
  `threadspawn.Attach`), затем слушает сокет.
- `workspacelock` записывает в lock-файл режим `daemon` и путь сокета;
  клиент находит демон по lock-файлу, детерминированный путь сокета служит
  запасным способом.
- Путь сокета: `$XDG_RUNTIME_DIR/sennit/<sha256(gitCommonDir)[:16]>.sock`,
  иначе `os.TempDir()`. Проверять длину 104 байта (macOS) и падать с ясной
  ошибкой.
- Простой: демон завершается, когда нет клиентов, нет занятых сессий, нет
  живых тредов и задач, нет фоновых шеллов, и так в течение
  `options.daemon.idle_timeout` (по умолчанию 10 мин). Ожидающий запрос
  разрешения и ожидающий вопрос (`question.Request`, в том числе у
  делегаций) считаются занятостью (ревью, п. 4); сквозные тесты PR 2.4
  проверяют оба случая.
- `SIGTERM`/`SIGINT` → `App.Shutdown`, как сейчас при выходе из TUI.
- Логи в существующий файловый лог; путь выводит `sennit daemon status`.
- MCP OAuth в демоне никогда не открывает браузер сам
  (`oauth/mcp/handler.go:160`); URL уходит клиенту через `MCPPendingAuth`.

### PR 2.2. Supervisor

- Демон запускается без переменных herdr (см. PR 1.5): иначе он унаследует
  панель терминала первого клиента.

Перенос `027d6155c^:internal/server/supervisor/` с изменениями:
- ключ по проекту, а не по пользователю;
- готовность через `grpc_health_v1` вместо `GET /v1/health`;
- `restartIfStale` сравнивает BuildID и останавливает только простаивающий
  демон; занятый демон с другой сборкой, но той же `ProtocolVersion`,
  используется как есть с предупреждением;
- `start.lock` в каталоге кэша проекта.

Проверка: 8 параллельных `EnsureRunning` поднимают один демон
(аналог `clientserverrace/race_test.go`); устаревший сокет после `kill -9`
убирается. Помнить: дочерний процесс под `-race` стоит секунду на выходе
(память о race atexit sleep).

### PR 2.3. Команды

**Сделано** (`fc138ef59`, `1e5d36339`, коммит 2.3c). `sennit` при
`options.daemon = "auto"` или `--daemon` работает через демон; `attach`,
`ps`, `daemon status/stop/restart/logs`, `run --detach`; `run`, `login`,
`logout`, `accounts` идут через демон, если он запущен, и сами его не
поднимают; herdr в режиме демона ведёт клиент (`herdr.TranslateFrontend`).
Открыто: `OAuthConfiguredProxy` и `OAuthProviderConfiguredProxy` отдают
клиенту URL прокси с паролем, тогда как `FrontendConfig` пароль вырезает.
Для удалённого режима (фаза 3) нужно решение: подставлять сохранённый прокси
на сервере, когда клиент прислал неизменённое значение, и не отдавать пароль
вовсе.

- `sennit` при `options.daemon = "auto"` подключается к демону или
  запускает его; `--no-daemon` принудительно поднимает `App` в процессе.
  Если lock держит не демон (встроенный TUI), поведение как сейчас:
  `ErrLocked` с PID владельца.
- `sennit attach [--session ID]`, `sennit daemon status|stop|restart|logs`,
  `sennit ps`: занятые сессии, ожидающие разрешения, живые треды и задачи.
- `sennit run --detach "prompt"`: отдаёт ход демону, печатает ID сессии и
  выходит. Без `--detach` `run` работает через демон, если тот запущен, и в
  процессе иначе.
- `login`, `logout`, `accounts`: через демон, если он запущен; демон не
  запускают никогда. Copilot device flow в `cmd/login.go:174-210` сейчас
  идёт мимо воркспейса; перевести на `StartOAuth`, как в TUI.
- Выход из TUI в режиме демона не отменяет ходы. Если есть занятые
  сессии, строка статуса при выходе сообщает, что работа продолжается, и
  как к ней вернуться.

### PR 2.4. Сквозные тесты демона

**Сделано** (`internal/daemon/e2e`, настоящий бинарь, фальшивый провайдер
OpenAI, ~67 с; под `-race` пропускается: демон собран без race, а сам код
покрыт race-прогонами своих пакетов). Сценарии: ход переживает отключение;
разрешение и вопрос ждут без клиентов и приходят подключившемуся;
`kill -9` посреди хода и перезапуск; worktree через демон; выход по
простою и не во время хода. Замечание: для хода верхнего уровня ожидающий
запрос не выделяется из «занятой сессии» (сессия занята весь ход), поэтому
п. 4 ревью изолированно проверяет `internal/daemon/idle_busy_check_test.go`.
Найден пробел, исправляется шагом 2.4b: после отключения клиента,
вошедшего в worktree, приложение worktree продолжает владеть сессией, а
получить к нему хэндл заново нельзя.

Реальный бинарь, реальный сокет, мок-провайдер из `common_test.go`:
- запуск хода, отсоединение, повторное подключение, ход завершён, история
  совпадает;
- запрос разрешения при отсутствии клиентов ждёт; подключившийся клиент его
  видит и отвечает;
- `kill -9` демона посреди хода; следующий старт помечает ход отменённым;
- worktree: вход через демон, отключение, подключение, выход;
- простой: демон завершается после `idle_timeout`, но не во время хода.

Помнить о macOS и Windows (память о новых тестах на других ОС): длина пути
сокета, AF_UNIX на Windows, права `0600` на Windows не работают так же, как
на unix.

**Фаза 2 закрыта** (`160f645f4` … `40905ca93`). Сверх плана: шаг 2.4b
(подключение к worktree, из которого отключился клиент, и учёт его работы
в простое) и аудит класса «долгоживущая работа на контексте вызова»:
loopback теперь отменяет контекст при возврате, как gRPC; найдено два
дефекта — приложение worktree (`cc1c24666`) и приложение изолированной
задачи (`40905ca93`, дефект был и во встроенном режиме).

## Фаза 3. Удалённое подключение

### PR 3.1. Мост через SSH

- `sennit daemon bridge --cwd <path>`: поднимает демон через supervisor,
  соединяется с сокетом и копирует stdio ↔ сокет.
- `sennit attach ssh://[user@]host[:port]/path` и
  `sennit --remote ssh://...`: запускает `ssh` с `bridge`, gRPC-клиент
  получает `net.Conn` поверх stdin/stdout дочернего процесса через
  собственный dialer. Используется системный `ssh` с конфигом пользователя
  (ключи, `ProxyJump`).
- Версии бинарей: `Hello` сравнивает `ProtocolVersion`; при несовпадении
  клиент показывает версии обеих сторон.

### PR 3.2. Разделение клиент/сервер в UI

- Вложения, вставка, буфер обмена, `$EDITOR`, уведомления, файлпикер:
  локальные, байты уходят в `message.Attachment`.
- @-дополнения и @-упоминания через `ListProjectFiles`/`AttachProjectFile`
  (PR 0.6).
- Серверные пути отображаются с `ServerHome` из `Hello`.
- Заголовок показывает хост при удалённом подключении.

### PR 3.3. OAuth и SSO через удалённый демон

- Codex слушает `localhost:1455` на машине демона
  (`oauth/codex/oauth.go:97-98`), браузер открыт на клиенте. Клиент при
  удалённом подключении слушает тот же порт у себя и пересылает запрос
  колбэка RPC `OAuthDeliverCallback(FlowHandle, rawQuery)`; демон отдаёт его
  своему обработчику, а ответ (страница `oauth/callback/page.go`) клиент
  отдаёт браузеру.
- MCP OAuth: тот же механизм для слушателя `oauth/mcp/handler.go:140-155`.
- Device flow (Copilot) работает без изменений.
- Codex читает `~/.codex` на машине демона (`oauth/codex/disk.go:121,188`);
  так и остаётся, это часть «конфиг машины демона».
- AWS SSO: проверить, где выполняется команда из `AWSSOCommand`. Если на
  демоне, пользователю показывается URL, как сейчас в
  `ui/dialog/aws_sso.go:283`.

Проверка: тест с двумя сетевыми неймспейсами не нужен; достаточно, что
клиентский слушатель и серверный обработчик связаны только RPC, и тест
сверяет, что токен дошёл.

## Фаза 4. Завершение

### PR 4.1. Документация

- AGENTS.md, раздел «Proto boundary»: убрать «There is no remote transport
  in this tree» и «nothing is sent anywhere»; описать классы методов,
  генератор, правило «новый метод = интерфейс + `appws` + строка в
  `classes.go` + образец DTO».
- README: режим демона, `attach`, удалённое подключение, ограничения
  (конфиг машины демона, CLI-утилиты локальные).
- CHANGELOG.

### PR 4.2. Смена умолчания

После обкатки `options.daemon` по умолчанию становится `auto`. Критерий
смены задаёт владелец (см. открытые вопросы).

## Порядок и зависимости

```
0.1 ─┬─ 0.2 ── 0.3 ─────────┐
     ├─ 0.4 ── 0.5 ── 0.5b ─┼─ 0.7 ── 1.1 ─┬─ 1.2 ─┬─ 1.4 ── 1.5 ── 1.6 ── 2.1 ── 2.2 ── 2.3 ── 2.4 ── 3.1 ── 3.2 ── 3.3 ── 4.1 ── 4.2
     └─ 0.6 ────────────────┘              └─ 1.3 ─┘
```

0.2, 0.4 и 0.6 после 0.1 независимы и могут идти параллельно, если каждый
агент работает в своей копии дерева (память о параллельных агентах).

## Объём (предварительно)

| Фаза | Рабочий код | Тесты | Примечание |
|---|---|---|---|
| 0 | 2,5–3,5 тыс. строк | 2–3 тыс. | 0.1 широкий, но механический; 0.4 самый рискованный |
| 1 | 3–4 тыс. рукописных + 3–4 тыс. сгенерированных | 3–4 тыс. | Старая реализация на 92 метода: ~8 тыс. рабочего кода без генерации |
| 2 | 1,5–2 тыс. | 1,5–2 тыс. | Supervisor переносится |
| 3 | 1–1,5 тыс. | 0,5–1 тыс. | |

## Открытые вопросы к владельцу

1. **Критерий смены умолчания на `auto`** в PR 4.2.

Решённые 2026-09-25 вопросы перенесены в «Зафиксированные решения»:
ожидающее разрешение без клиентов, `idle_timeout`, Windows, UI-настройки.
