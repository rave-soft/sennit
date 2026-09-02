# План рефакторинга

Результат аудита от 2026-09-02 (`main`, bdc47daab). Каждая находка
подтверждена чтением кода; пункты 3.3 и 3.4 дополнительно временными тестами.
Фазы упорядочены так, чтобы сначала закрыть баги малыми правками, потом
разобрать структурные причины этих багов. Внутри фазы задачи независимы и
могут идти параллельными коммитами. Убирайте пункт, когда он закрыт;
история остаётся в git.

Условные обозначения: **[S]** малая правка (≤ 1 файл, ≤ 1 час),
**[M]** средняя (несколько файлов, тесты), **[L]** структурная (новый пакет
или перенос ответственности).

---

## Фаза 1. Баги, ломающие пользователя — закрыта

Все девять пунктов исправлены и закоммичены (`cec59b06d`..`e3973d2f4`), каждый
с регрессионным тестом. Один пункт закрыт наполовину, остаток ниже.

### 1.1-bis [L] Экран треда всё ещё маршрутизирует по активному экрану

Осталось от пункта 1.1. Дашборд починен (`221307322`):
`handleDashboardMsg` классифицирует сообщение один раз и отдаёт всё,
что не ввод, в `r.main`, опираясь на инвариант «при активном дашборде
тред всегда отвязан», который закреплён тестом.

Для `screenThread` тот же приём не работает. Встроенный UI треда строится
тем же конструктором `New(...)`, что и главный экран (`WithEmbedded`
отключает только онбординг, прогресс-бар и панель тредов), поэтому оба
`*UI` живы одновременно и рассылают одни и те же нетегированные типы:
`pubsub.Event[message.Message]`, `[session.Session]`,
`[permission.PermissionRequest]`, `[question.Request]`,
`modelSettingUpdatedMsg`, `mcpStateChangedMsg`, `providerConfiguredResult`,
`themeSetMsg`, `permissionResponseMsg` и другие. Любое правило «по
активному экрану» или «всё в main» ошибается в одну из сторон.

Тегировано сейчас: 12 типов через `uiOwnedMsg` (список закреплён
compile-time проверками в `root.go:780-796`) и один через
`uimsg.MainScreenMsg`.

Форма настоящего фикса: расширить существующий механизм `uiOwned` на весь
нетегированный набор, чтобы `Root` диспетчеризовал по владельцу, а не по
активному экрану. Тегировать по месту отправки, а не оборачивать команды
в `Root`: обёртка команды сломает `tea.BatchMsg`/`tea.SequenceMsg` и
служебные сообщения вроде `QuitMsg`, которые обязаны дойти до рантайма.
После этого fallback `default:` можно инвертировать целиком, а
`handleDashboardMsg` вернуть к обработке одного лишь ввода.

Симптом уже обходили не в том месте: `workspace/appws/attached_thread.go:47-58`
добавляет fallback на родителя в `PermissionGrant` именно из-за этой
маршрутизации. После фикса обход убрать.

## Фаза 2. Баги в фоне и ресурсы — закрыта

Все семь пунктов исправлены и закоммичены (`bf1501c90`..`9ed3c50a4`), каждый
с регрессионным тестом.

Два решения, принятые по ходу и стоящие того, чтобы их знать:

- **2.1** решён не добавлением метода в `Workspace`, а тем, что реактивация
  стала явной. `AttachThread` больше не воскрешает тред сам; это делает
  `ActivateThread`, который уже был в контракте и не имел ни одного
  вызова. Зовёт его только путь drill-in (`attachThreadCmd`). Тред, который
  не удалось поднять, открывается read-only с предупреждением, а не с
  ошибкой: для слитого треда это нормальное состояние, а не сбой.
- **2.2** ввёл `titleTimeout` на `sessionAgent`, переопределяемый в тестах.
  Первый вариант ждал реальные 45 секунд и раздувал набор `internal/agent`
  с 8 до 54 секунд, под `-race` до 75.

---

## Фаза 3. `ConfigStore`

Пункты 3.1-3.3 закрыты (`056addc33`, `49ae3f1c2`). Пункт 3.4 **отклонён**,
подробности ниже. Остаются структурные 3.5-3.7.

### 3.4 [отклонён] Кеширование окружения конфигурации

Попытка (`b65f967b4`, откачена в `4a…`) кешировала результат
`Config.RuntimeEnvironment()` при сборке конфигурации. Она сломала два
зафиксированных контракта, и оба обнаружились только полным прогоном:

- `TestRunSubAgent_RetryUsesRefreshedCredential` — ключ вида `$MY_KEY`,
  повёрнутый во внешнем окружении, переставал подхватываться при повторе
  после ошибки авторизации: retry вечно переразрешал мёртвый ключ.
- `TestLoadRuntimeResolverReadsCurrentEnvironment` (`internal/providerload`) —
  прямо требует, чтобы запись `env`, заданная как `$OTHER_VAR`, отражала
  окружение на момент чтения, а не на момент сборки.

Промежуточный вариант (кешировать только записи `env`, читать процессное
окружение живьём) закрывал первый тест, но не второй.

Проблема реальна: запись `env` с `$(cmd)` выполняет команду на каждое
разрешаемое значение, то есть N записей × M значений. Но лечится это не
кешированием целиком. Любая следующая попытка обязана сохранить живую
интерполяцию переменных, устранив лишь повторную подстановку команд, и
пройти оба теста выше. Пока не найдено решение, отвечающее этому —
оставить как есть.

### 3.5 [L] Разбор `ConfigStore`

Методы размазаны по `store.go`, `store_credentials.go`,
`provider_accounts.go`, `mcp_token.go`, `docker_mcp.go`, `reload.go`,
`watch.go`, `staleness.go`, `accounts_service.go`; пять мьютексов плюс два
в watcher; порядок захвата описан в трёх разных комментариях
(`store.go:38-67`, `configfile.go:24-34`, `credentials.go:176-182`).

Действия, по порядку:
- `fileStaleness.withWrite(fn func() (wrote bool, err error)) error`,
  все мутаторы через него; форвардеры `preReloadFileSnapshots`/
  `trackedConfigPathSet`/`captureStalenessSnapshot` (`staleness.go:145-159`)
  убрать. Закрывает пять копий `s.staleness.mu.Lock(); write; refresh`
  в `store.go:351-723`.
- `snapshotOverridesLocked()` без лока для `RuntimeSnapshot`
  (`store.go:170-183`) и `snapshotOverrides` (`reload.go:394-407`).
- Выделить `credentialsPublisher` (`UpdateProviderAccount`,
  `SetProviderAPIKey`, `PersistRefreshedToken`) и `mcpTokenStore` в
  отдельные типы с явной зависимостью от `mutateInMemory`/`update`.
- Удалить мёртвое: `RuntimeInput.KnownProviders` (`build.go:363,373`
  всегда nil), `providerload/loader.go:86-91 providers()` (дубль
  `providers/runtime.Providers()`), заглушку `var _ = cmp.Or[string]`
  (`loader.go:231`), `configruntime.LoadWithProcessor`,
  `RuntimeResult.Resolver` после 3.4.

- Свести `Resolver()` и `RuntimeSnapshot().Resolver` к одному значению.
  После кеширования окружения (3.4) `cfg.RuntimeResolver()` дёшев, поэтому
  `Resolver()` может возвращать `s.Config().RuntimeResolver()` вместо
  отдельного поля `s.resolver`. Сейчас они расходятся только в одном окне:
  store, созданный через `NewStore` с `StoreOptions.Resolver`, до первого
  reload отдаёт инжектированный резолвер из `Resolver()`, но не из
  `RuntimeSnapshot()`; первый же reload его затирает
  (`reload.go:207`). Решить заодно судьбу `StoreOptions.Resolver`.

### 3.6 [L] Развязать `config` ↔ `providers/runtime`

`providers/runtime/provider.go:173` импортирует `config` ради
`ProviderConfig`, `VariableResolver`, `ResolveProviderHeaders`; поэтому
`config` вызывает `FromConfig`/`ApplyPostCredentialSetup` через
`RuntimeProcessor` (`runtime.go:42-46`) с единственной реализацией из
однострочных форвардеров (`providerload/loader.go:30-37`) и ветками
`s.processor == nil` в трёх мутаторах.

Действия:
- Вынести `ProviderConfig`, `VariableResolver`, `ResolveProviderHeaders`,
  `ResolveOptionalProviderProxy` в листовой пакет
  (`providers/config` или расширить `providers/state`).
- `providers/runtime` перестаёт зависеть от `config`; `config` зовёт compile
  напрямую; `RuntimeProcessor` сужается до `Process`.
- Codex-специфику из `config/provider_accounts.go:469-494,570-576`
  (`backfillCodexIdentity`, AccountID из JWT) перенести туда, где живёт
  `AccountUsageFetcher` (`workspace/appws`).

### 3.7 [L] Два владельца живых кредов

Остаток от 3.1. После точечного фикса `APIKey`/`OAuthToken`/`Account`
живут одновременно в `Providers` и в `RuntimeProviders`, и обе копии надо
держать согласованными вручную — ровно та конструкция, из которой выросли
3.1, 3.2 и 3.5.

Целевое состояние: `Providers` — только то, что лежит на диске;
`RuntimeProviders` — единственный владелец живых кредов. Работа:
разметить 46 вызовов `Providers.Get` на «читает дисковое поле» и «читает
кред», перевести вторую группу на `RuntimeProvider(id)`, затем убрать
зеркалирование из `UpdateProviderAccount`. Делать после 3.6, когда
`ProviderConfig` переедет в листовой пакет.


---

## Фаза 4. `internal/agent`

Пункт 4.1 закрыт (`919bab433`): память именованных агентов включена.
Это включает ранее не работавшую функцию, которая тратит входные токены
на каждое делегирование к именованному агенту; бюджет ограничен
`subagent_memory.go`.

### 4.2 [M] Откат дубля `FileCoverage` из коммита 09a6d8a3d

`tools/file_tracking.go:19-105` копирует `filetracker/coverage.go:36-145`
(с ручной O(n²) сортировкой). В проде из `tools` нужен только `Covers`
(`edit.go:225`); `Add/Shift/Empty` живут ради мока в `edit_test.go:36-40`.
Причина копии: `filetracker` тянет `db`, хотя `coverage.go` от него не
зависит.

Действия:
- Вынести `Coverage`/`LineRange` в leaf-пакет
  `internal/filetracker/coverage`, использовать в обоих местах.
- `tools.FileTracking` возвращает его напрямую; конверсию в
  `tool_adapters.go:24-31` убрать.
- `todoSessions` (`tool_adapters.go:159-186`) удалить:
  `sessionstore.Service` удовлетворяет `TodoSessions{Get; SetTodos}`
  напрямую, `tools/todos.go` и так импортирует `session`.
- Адаптер `fileHistory` оставить, он транслирует `sql.ErrNoRows`.

### 4.3 [M] Дублирование в мутирующих инструментах

- `confinementRefusal` дважды на мутацию: в `edit.go:69`, `multiedit.go`,
  `write.go` и снова в `applyFileMutation`.
- Freshness-сообщение скопировано `edit.go:304-327` ↔ `write.go:511-534`.
- Эпилог `mutationCommitted → IsError → notifyLSPs → result+diagnostics`
  повторён в edit/multiedit/write/replace_symbol.
- `read_core.go:29` игнорирует `*lsp.Manager` и `*skills.Tracker`, но
  `NewMultiReadTool`/`NewReadTool` и `tool_registry.go:169` их передают.

Действия: `finishFileMutation(...)`, общий билдер freshness-сообщения,
один confinement-чек, убрать мёртвые параметры.

### 4.4 [M] Ceremony в делегировании

- `runtimeOperationPort{agent: ..., inputs: ...}` набран 9 раз
  (`turn_dispatcher.go:106,141,143,223`, `delegation_finalizer.go:403-423`).
  Сделать `port()`.
- Форвардинг-обёртки без non-test ссылок: `delegation_finalizer.go:394-422,
  509`, `runtime_builder.go:1251,1624`. Удалить, тестировать реальные
  методы.
- «Синхронный» путь субагента (`ChildSessionID == ""`,
  `updateParentSessionCost`, `parentCostMu`, `SessionSetup`,
  `:571-585,759-782`) достижим только из `coordinator_test.go`. Удалить
  вместе с тестами или оставить с явным TODO.
- `agenticFetchFactory` (`:1135-1145`) собирает `NewSessionAgent` мимо
  `buildAgent`: `IsSubAgent` не выставлен, `compat.go:40` инжектит
  родительский todo-reminder; второй ручной список инструментов. Строить
  через `buildAgent` с allowlist из `toolSpecs`.

### 4.5 [L] Auth-политика вне билдера рантайма

`runtime_builder.go:180-185` конструирует `accounts.NewFileStore(
config.GlobalAccountsFile())`; `:904` читает `codex.UsageFor`; ротация
аккаунтов (`:795-1071`) и refresh кредов (`:626-1240`) живут внутри
билдера; сравнения с `codex.ProviderID` в `maxtokens.go:29`,
`providers.go:131`, `provider_log.go:181,412`, `runtime_builder.go:1356-1364`.

Действия:
- Инжектить `accounts.Store` и `UsageLookup` через `CoordinatorOpts`.
- Вынести ротацию, refresh и `build*Provider` в `rotation.go`,
  `credential_refresh.go`, `providers_build.go`.
- Provider-специфику скрыть за `typeclass`/capabilities.
- `CoordinatorOpts.History` принимать узкий `fileHistoryStore` (2 метода),
  тогда импорт `history/store` из `coordinator.go:141` и
  `delegation_finalizer.go:52` исчезает.
- `tools/sennit_info.go`: `EnvironmentProblems` передавать функцией через
  конструктор вместо импорта `internal/doctor`.

---

## Фаза 5. UI за `Workspace`

Пункты 5.5-5.7 закрыты (`8fb2275e9`): вместо россыпи nil-guard'ов
добавлены nil-безопасные аксессоры на `Config`, дублирующиеся обработчики
диалогов сведены к двум хелперам, тестовые символы убраны из продакшн-кода.

### 5.1 [L] Codex/Copilot OAuth из диалога в Workspace

`ui/dialog/oauth_codex.go:127-167` (`initiateAuth`) читает токены Codex
CLI с диска, обменивает refresh-token по сети, биндит loopback-порт;
`:219-243` (`afterSave`) качает модели и пишет `providers.codex.models` и
`proxy_url` в конфиг. Это дубль `cmd/login_codex.go:219-240`.
`oauth_copilot.go:108,150` аналогично зовёт `copilot.RequestDeviceCode`/
`PollForToken`.

Действия: `Workspace.StartOAuth(ctx, providerID, proxy) (OAuthSession,
error)` с DTO `{URL, UserCode, ExpiresIn, Interval}` + `Wait`/`Cancel`;
`Workspace.CompleteOAuth(providerID, token, opts)` внутри делает
RecordAccount и post-save. `cmd/login_codex.go` использует те же методы.
Диалог остаётся view + Action; импорты `oauth/codex`, `oauth/copilot`,
`proxyhttp` из `ui/dialog` исчезают.

### 5.2 [M] Проверка API-ключа из UI

`ui/dialog/api_key_input.go:305-333` строит `config.ProviderConfig`,
зовёт `providerruntime.FromConfig` и `TestConnection`; проверяет ключ не
тем провайдером, каким потом будет ходить агент (без `ProxyURL`, ротации,
кастомных заголовков). Плюс `time.Sleep(750ms)` в команде.

Действия: `Workspace.VerifyProviderAPIKey(ctx, providerID, key) error`;
минимальную длительность спиннера через `tea.Tick`.

### 5.3 [M] Doctor: I/O в Update и агрегация в UI

`ui/dialog/doctor.go:71-72,240-262`, вызов из `model/dialogs.go:100`.
Конструктор синхронно зовёт `doctor.EnvironmentProblems()` →
`clipboard.MissingHTMLHelpers()` → `exec.LookPath` по PATH. Рядом UI сам
склеивает `ConfigProblems` + `SkillProblems` + `MCPGetStates`.

Действия: `Workspace.DoctorProblems() []config.Problem`; `NewDoctor` в
форме `(dlg, tea.Cmd)` как `NewFilePicker`; импорт `internal/doctor` из UI
исчезает.

### 5.4 [M] DTO вместо импортов бэкенда в `model` и `common`

- `model/sidebar.go:89,270-272`: `shell.BackgroundJobCounts`,
  `shell.MaxBackgroundJobs`.
- `model/auth.go:25`: `providerruntime.ToProvider(providerCfg)`.
- `common/elements.go:83`, `dialog/provider_settings.go:104`,
  `dialog/accounts.go:75,280`: `accounts.Usage`, `accounts.CapabilitiesOf`.
- `model/update_session.go:313`, `ui.go:638`: `pubsub.Event[history.File]`.

Действия: DTO/алиасы в `workspace` по образцу `workspace.LSPClientInfo`
(`BackgroundJobCounts`, `MaxBackgroundJobs`, `AccountUsage`,
`ProviderCapabilities`); `Workspace.KnownProvider(id) (catwalk.Provider,
bool)` вместо `ToProvider`. `permission.PermissionRequest` в UI остаётся,
это задокументированный компромисс через alias-идентичность.

---

## Фаза 6. Границы workspace / app / proto

### 6.1 [M] Сузить `Workspace`

`workspace/workspace.go:575-609`: 113 методов; после a09ecd54a
`FrontendWorkspace` встраивает 21 однометодный интерфейс, из них 11 без
потребителей (`ConfigResolver`, `PreferredModelOverrider`,
`CompactModeSetter`, `ProviderAPIKeySetter`, `CopilotImporter`,
`OAuthTokenRefresher`, `ProviderCatalog`, `CustomProviderTypeLister`,
`AccountLimitsRefresher`, `PlanUsageReporter`). Методы без вызовов из
`ui`/`cmd`: `ActivateThread`, `SendThread`, `MCPAuthURL`,
`AgentQueuedPrompts`. Каждый обязан быть в `read_only_workspace.go`
(775 строк форвардинга) и стабах.

Действия: удалить 4 мёртвых метода; схлопнуть однометодные интерфейсы в
2–3 роли (`ConfigMutator`, `ProviderAccounts`, `ProviderInfo`); вернуть
контрактные комментарии, удалённые в a09ecd54a.

### 6.2 [M] `workspace` как контракт, а не use-case'ы

- `custom_provider.go` (импорт `discover`), `session_changes.go`
  (`diff`), `resolve_session.go` → в `appws` или `workspace/usecase`.
- `MCPController.ListMCPPrompts() []commands.MCPPrompt`
  (`workspace.go:484`) транзитивно тянет `agent/tools/mcp`, `hooks`,
  `shellconfig` в UI. Свой DTO `workspace.MCPPrompt`.
- `dependency_guard_test.go:42-48` проверяет только прямые импорты.
  Переписать на `go list -deps` / `packages.Load` с запретом `agent/...`.
- `LSPEvent.Error` и `MCPClientInfo.Error` (`workspace.go:139,460`) —
  `error` в data-only DTO; сделать `string`, как `proto.LSPClientInfo.Error`.

### 6.3 [M] `appws → threadspawn` и три копии flatten

- `threadspawn/protoconv.go` (конвертация в UI-DTO) перенести в `appws`;
  `proto.ThreadEvent` и `EventToProto` удалить, единственный потребитель
  (`app_workspace_lifecycle.go:71-75`) тут же разбирает на поля.
- Одна `flatten(thread.Thread)`, от которой строятся `proto.Thread`,
  `tools.ThreadInfo` (`agenttool.go:129-145`), `tools.TaskInfo`
  (`tasktool.go:147-159`).
- `thread.RunComplete ↔ notify.RunComplete`
  (`coordinator_adapter.go:220-270`): заменить pump-горутину на каждую
  подписку map-адаптером без горутины.
- `threadspawn/spawner.go:37`: `frontend func(*app.App) workspace.Workspace`
  → `any` + type-assert в `appws`, чтобы `threadspawn` не импортировал
  `workspace`.
- Вызов `attach` из `appws` вынести в `cmd/root.go:282`, где он и
  используется.



---

## Фаза 7. Мёртвый код и мелкие дубли

- [S] `internal/event`: все функции пустые, `internal/log` зависит от
  него только ради no-op `event.Error` в `RecoverPanic`. Убрать импорт из
  `log`; пакет удалить или оставить заглушку `NewSessionTelemetry` для
  `sessionstore.TelemetrySink`.
- [S] `session/store/service.go:72-74,556-574`:
  `CreateAgentToolSessionID`/`ParseAgentToolSessionID` не трогают БД,
  `IsAgentToolSession` без production-вызовов. Перенести функциями в
  `session`, из интерфейса убрать; `read_only_workspace.go:652,728`
  перестаёт их проксировать.
---

## Что проверено и оставлено как есть

- SQLite: все запросы пишут `strftime('%s','now')`, `Finish.Time` везде в
  секундах, `db.InTx` и `UpdateFileRead` корректно делают
  `defer Rollback` + `Commit`.
- Pubsub-брокер ограничен (буфер 4096, drop вместо блокировки,
  `PublishMustDeliver` с таймаутом), `csync` и `lock` без замечаний.
- Bubble Tea: ни одного `go func` в UI, ни одной `tea.Msg`-замыкания,
  мутирующей модель; все записи через `Workspace` внутри `tea.Cmd`;
  `thread_completion.go` (4bb5c66ae) корректен.
- Shutdown-последовательность `app/shutdown.go`, `AgentDispatcher`,
  `AgentRunStream`, `Manager.Shutdown`, read-only обёртка с
  compile-time default-deny.
- Лок-дисциплина в `mcp`, `hooks.Runner.Run`, `permission`, порядок
  «хуки → permission».
- Допустимые связи: `thread → permission/git`, `cmd → ui/*` (composition
  root), `db → brand`, `fsext → lock`, `tools → lsp`,
  `agent → session/store`, `skills → pubsub` (после удаления глобального
  брокера).
- В TECHDEBT.md ничего из перечисленного не отслеживается; Copilot-заголовки
  (`providers/runtime/provider.go:214-215`, после 9f367e6a7 применяются
  безусловно) относятся к уже записанному пункту «GitHub Copilot identity».
