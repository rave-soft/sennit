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

## Фаза 2. Баги в фоне и ресурсы

### 2.1 [M] Док тредов респавнит целые App как побочный эффект отрисовки

`internal/workspace/appws/threads.go:163-227`,
`internal/ui/threads/dock.go:112-116,169`, `internal/thread/lifecycle.go:1314`.

`AttachThread` для треда без живого handle делает `reactivate()` → полный
`app.Bootstrap` (lock, DB, MCP init, координатор, watchers); `detach` no-op.
Док дёргает `AttachThread` каждые 8 с ради `MessageCount` и показывает
idle-треды, которые после рестарта остаются idle без runtime. Итог: после
рестарта с N idle-тредами в первые 8 с поднимаются N App.

Действия:
- Read-only нужды (док, `GetSession`) обслуживать через
  `NewReadOnlyWorkspace`, сессии лежат в общей БД.
- Реактивация только по явному действию пользователя (Enter в дашборде).

### 2.2 [S] Генерация заголовка сессии живёт в неотслеживаемой горутине

`internal/agent/run_turn.go:710`, `internal/agent/title.go:67`.

`go a.generateTitle` с `WithoutCancel`, без таймаута, мимо
`readinessLifecycle.launch`; `Coordinator.Close` её не видит. Срабатывает
и для каждой дочерней сессии делегирования, перезаписывая заголовок из
`CreateSubAgentSession`.

Действия: контекст lifecycle + `WithTimeout(30–60s)`, запуск через
`readinessLifecycle.launch`; пропуск для `isSubAgent` с явным title.

### 2.3 [S] `abandonGrace` хуков короче эскалации SIGINT→SIGKILL

`internal/hooks/runner.go:21` (1 с) vs `internal/shell/exec_unix.go:19`
(2 с). Хук с `trap '' INT` по таймауту даёт ложный «goroutine abandoned»
и потерянный stdout/stderr, хотя через секунду штатно завершается.

Действие: вывести grace из kill-timeout shell (`DefaultKillTimeout +
500ms`) или задать хукам kill-timeout меньше grace.

### 2.4 [M] Инфраструктурные ошибки и `ctx.Err()` уходят модели как текст

Нарушение правила «text response vs Go error» из AGENTS.md:
- `tools/mcp-tools.go:169-172`, `tools/list_mcp_resources.go:76-79`:
  `RunTool` err → текст, включая `context.Canceled`. Esc сохраняется в
  истории как обычный tool-result «context canceled».
- `tools/lsp_rename.go:63-64` и definition/references/call_hierarchy:
  любая ошибка `resolveSymbol` → «Symbol not found», включая `ctx.Err()`
  из `filepath.Walk` и EACCES.
- `tools/sennit_logs.go:176-180`, `agent_trace.go`, `git_tools.go`
  (`gitError`): ошибки `os.Open`/`Stat`/`CreateTemp` как текст.

Действия: `if ctx.Err() != nil || errors.Is(err, context.Canceled) {
return ToolResponse{}, err }` перед текстовым fallback; в
`resolveSymbolResults` отдельный sentinel «нет совпадений»; в
logs/trace/git разделить пользовательские ошибки и `%w` ошибки ФС.

### 2.5 [S] `Bootstrap` не тушит App при ошибке после `newApp`

`internal/app/bootstrap.go:239-244`. Практически недостижимо, но при
ошибке `AddFinalCleanup` остаются MCP-init, watchers, herdr-bridge.
Действие: `appInstance.mainDBRelease = nil; appInstance.Shutdown()` по
образцу `app.go:172-173`.

### 2.6 [S] `forwardSkillsToThreads` / `forwardAgentsToThreads` живут до конца процесса

`internal/app/threadspawn/attach.go:196-197` запускаются на
`cmd.Context()`; `Skills.SubscribeEvents` после `App.Shutdown` не
закрывается. Действие: контекст, отменяемый через `AddPreCleanupHook`, как
у watchers в `watch.go:82-87`.

### 2.7 [S] `shell.Manager.Remove` снимает работающий job с учёта

`internal/shell/background.go:287-295`. После `delete(m.shells, id)`
процесс недостижим для `Kill`/`Cleanup`/`Shutdown`. Сейчас оба вызова
(`tools/bash.go:303,349`) делают это после `done`, поэтому это ловушка API.
Действие: возвращать ошибку при `!shell.IsDone()`.

---

## Фаза 3. `ConfigStore`: три источника правды

Баги 3.1–3.3 внесены коммитами 9f367e6a7 и bdc47daab и имеют общую
причину: `Providers`, `RuntimeProviders` и диск обновляются по отдельности.
Сначала точечные фиксы, потом 3.5.

### 3.1 [M] Обновление кредов не доходит до `ProviderConfig`

`internal/config/store_credentials.go:147-164` (`UpdateProviderAccount`),
`:470` (`PersistRefreshedToken`) пишут только в runtime-провайдер.
Читатели старого поля: `config/credentials/credentials.go:164-171`,
`agent/runtime_builder.go:626-631,1093`.

Сценарий: после refresh `ProviderConfig.OAuthToken` в памяти остаётся
просроченным → refresh повторяется каждый ход, `credentialVersion`
бампится, runtime пересобирается; `refreshApiKeyTemplate` затирает свежий
runtime-токен устаревшим. Если запись на диск не удалась, следующий ход
обменяет уже потраченный refresh token.

Действие: в `UpdateProviderAccount`/`SetProviderAPIKey`/
`PersistRefreshedToken` синхронно обновлять
`cfg.Providers[id].APIKey/OAuthToken/Account`. Альтернатива — перевести
всех читателей на `cfg.RuntimeProvider(id)`, но это больше правок.

### 3.2 [M] Reload при конкурентном refresh копирует runtime всех провайдеров

`internal/config/reload.go:504-513`. До 9f367e6a7 копировались только
провайдеры, чьи креды изменились между `startConfig` и `current`.
Подтверждено тестом: правка `base_url` на диске во время refresh даёт
`Providers["mock"].BaseURL` новый, `RuntimeProvider("mock").BaseURL`
старый.

Действие: снимать `startRuntime` в начале reload и переносить только id,
у которых `current` отличается по `APIKey/OAuthToken/Account/ProxyURL/
APIKeyTemplate`. Тест из аудита добавить в пакет.

### 3.3 [M] Typed-мутатор делает следующий тик watcher'а «внешним изменением»

`internal/config/store.go:590-591` (`updateLocked`) сужает tracked-набор
до загруженных файлов, тогда как `Load`/`reloadFromDisk` трекают все
`configPaths` (`load.go:124`, `reload.go:566`), включая несуществующие
глобальные слои. `hasUntrackedCandidate` (`watch.go:278-292`) считает их
новыми. Подтверждено тестом: после `SetCompactMode` набор сжимается с 4 до
2 путей, `externalChangeDetected()==true`. В проде `UpdatePreferredModel`,
`SetCompactMode`, `PersistRefreshedToken`, `SetProviderAPIKey` вызывают
полный `ReloadFromDisk` с discovery и `OnExternalChange` →
реинициализацию MCP (`app/watch.go:40-47`). Запись и refresh снапшота в
`updateLocked` не под одним `staleness.mu`.

Действие: `updateLocked` делать через ту же хореографию, что
`SetConfigFields` (`store.go:455-486`), без пересборки tracked-набора.
Тест-аналог `IgnoresOwnWrites` для typed-мутатора.

### 3.4 [M] `runtimeEnvironmentResolver` пересобирает окружение на каждый `ResolveValue`

`internal/config/runtime.go:76-112`: `os.Environ()` плюс выполнение всех
`Env[key]` (включая `$(cmd)`) на каждое разрешаемое значение. Резолвер
уходит в MCP init, discovery, `ActivateAccount`, `SetProviderAPIKey`.
Дополнительно `RuntimeSnapshot()` (`store.go:177`) и `Resolver()`
(`store.go:199-203`) отдают два разных резолвера, `StoreOptions.Resolver`
игнорируется.

Действие: вычислять окружение один раз при сборке config, держать
готовый `shellVariableResolver` в `s.resolver`, `RuntimeSnapshot` отдаёт
его же.

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

---

## Фаза 4. `internal/agent`

### 4.1 [S] Мёртвая память именованных агентов

`AgentID` выставляется только в `delegation_finalizer.go:1011`
(`TaskCreateArgs`), но `subAgentTaskRun` (`:851-859`) его не протягивает,
и `carryOverMessages` (`:887-889`) всегда выходит на `agentID == ""`.
`subagent_memory.go` (576 строк) достижим только из тестов.

Действия: протянуть `agentID` через `subAgentTaskRun`; тест через
`runNamedAgent`, а не через прямой `runSubAgent`. Если carry-over не нужен,
удалить `subagent_memory.go` целиком.

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

### 5.5 [S] Копипаста в `dialog_actions.go` и выборе модели

- `ActionToggleThinking` (`:99-131`) и `ActionSelectReasoningEffort`
  (`:170-215`): ~35 одинаковых строк. Хелпер
  `m.updateCoderModel(mutate func(*config.SelectedModel), info string)
  tea.Cmd`.
- `ActionSelectNotificationStyle` (`:72-90`) и
  `ActionToggleTransparentBackground` (`:133-160`): хелпер
  `m.setGlobalOptionGuarded(state, key, v, mk)`.
- `handleSelectModel` (`ui.go:947-999`) и `importCopilotResult`
  (`update_settings.go:266-296`): одна `m.continueModelSelection(
  providerID, model, onboarding, gen)`.

### 5.6 [S] Инвариант `Config().Options.TUI`

Без guard: `model/dialogs.go:492`, `ui.go:419`, `keypress.go:435`,
`onboarding.go:87`; с guard: `dialog_actions.go:146`, `ui.go:312`.
`config/defaults.go:45-49` гарантирует non-nil в проде, но
`stubWorkspace.Config()` в `workspace/read_only_workspace_test.go:526`
возвращает `&config.Config{}`. Выбрать одно: accessor'ы на `Config` (как
`ThemeID()`/`SpinnerMode()`) или убрать guard'ы.

### 5.7 [S] Символы, достижимые только из тестов

`deadcode ./...`: `model/keys.go:81 DefaultKeyMap`, `ui.go:258 withGOOS`,
`permission_response_state.go:54 isLoading`, `bang_mode_state.go:24
isEmpty`, `editor_placeholder_state.go:44`, `diffview/diffview.go:131-189`
(5 геттеров), `diffview/style.go:27 DefaultLightStyle`. Перенести в
`_test.go` или удалить.

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

### 6.4 [S] Обновить AGENTS.md

Секция «Proto boundary» описывает состояние до 82b152578: перечисляет как
живые уже удалённые `proto.Message`, `RunComplete`, `AgentEvent`,
`PermissionRequest`, `PermissionNotification`, `ConfigProviderKeyRequest`
и `proto/permission.go`; утверждает, что `proto.Thread` строится в
`workspace/app_workspace.go` (на деле `appws/threads.go` через
`threadspawn`). Переписать абзац по факту, после 6.3 ещё раз.

---

## Фаза 7. Мёртвый код и мелкие дубли

- [S] `internal/event`: все функции пустые, `internal/log` зависит от
  него только ради no-op `event.Error` в `RecoverPanic`. Убрать импорт из
  `log`; пакет удалить или оставить заглушку `NewSessionTelemetry` для
  `sessionstore.TelemetrySink`.
- [S] `pubsub/events.go:401-427`: `PayloadType`, `PayloadType*`,
  `Payload` без ссылок. Удалить.
- [M] `history/store/service.go:38-78`: интерфейсы
  `fileVersionStore`/`fileVersionTransaction` живут ради тест-сима
  `serializedVersionStore`; `CreateVersion` (`:104-134`) не переведён на
  `db.InTx`, хотя `db/tx.go:9-13` его для этого перечисляет. Методы
  `Get`, `ListBySession`, `ListLatestSessionFiles`, `Delete`,
  `DeleteSessionFiles` (`:29-35`) без вызовов вне пакета; три одинаковых
  цикла `fromDBItem` (`:155-191`). Перевести на `db.InTx`, интерфейсы и
  мёртвые методы удалить; sqlc-запрос `DeleteSessionFiles` оставить
  (`gc/gc.go:349`).
- [S] `skills/skills.go:81-85,103-116`, `manager.go:159-186`: глобальный
  `broker` без подписчиков, `SetLatestStates`/`GetLatestStates` ради
  единственного `appws/app_workspace_project.go:63`, хотя
  `Manager.States()` уже используется. `appws` читает `Manager.States()`;
  удалить глобальные `broker`, `latestStates`, `WithGlobalMirror`.
- [S] `session/store/service.go:72-74,556-574`:
  `CreateAgentToolSessionID`/`ParseAgentToolSessionID` не трогают БД,
  `IsAgentToolSession` без production-вызовов. Перенести функциями в
  `session`, из интерфейса убрать; `read_only_workspace.go:652,728`
  перестаёт их проксировать.
- [S] `db/read_files_atomic.go:122,131`: SQL руками, дубль
  `RecordFileRead`/`GetFileRead` из `sql/read_files.sql`. В
  `updateFileReadIn` использовать `New(db).GetFileRead` /
  `RecordFileRead`.
- [S] `lsp/client.go:110` и `lifecycle.go:581`: `registerHandlers`
  дважды за один `Initialize`. Оставить один вызов.

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
