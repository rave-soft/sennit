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

3.6 закрыт (`3a199a2cc`): `ProviderConfig`, `VariableResolver` и хелперы
разрешения переехали в листовой `internal/providers/config`,
`providers/runtime` больше не импортирует `internal/config`,
`RuntimeProcessor` сузился до `Process`, три ветки `processor == nil`
удалены. `Providers()` теперь принимает `bool`, а не весь конфиг.
Остаётся 3.7.

3.5 закрыт (`7eedfe354`): семь копий хореографии «запись + обновление
снимка под одним мьютексом» сведены к `fileStaleness.withWrite` и
`withWriteAddPath`, форвардеры убраны, мёртвая обвязка процессора удалена.

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

4.4 закрыт (`28db54638`). Реальный баг там был один: fetch-делегат не
помечался `IsSubAgent`, из-за чего получал напоминание про todo,
предназначенное верхнеуровневому агенту, не имея самого инструмента.
Через общий `buildAgent` его не пустили осознанно: `toolSpecs` привязан к
реальному рабочему каталогу и разрешениям, и это утекло бы в песочницу
делегата. Остаётся 4.5.

4.3 закрыт (`18ff70d4e`), но **не полностью**: первый пункт задания
отклонён по существу. Ранняя проверка границ рабочей папки в edit/write/
multiedit не дублирует проверку в `applyFileMutation`: без неё сначала
срабатывают stat и проверка свежести, отказ приходит с другим текстом, а
файлы вне рабочей папки читаются до отказа. Оставлено как есть.

4.2 закрыт (`4d927b7d2`): арифметика покрытия чтения переехала в листовой
`internal/filetracker/coverage`, дубль в `tools` стал алиасом, минус 300
строк.

Пункт 4.1 закрыт (`919bab433`): память именованных агентов включена.
Это включает ранее не работавшую функцию, которая тратит входные токены
на каждое делегирование к именованному агенту; бюджет ограничен
`subagent_memory.go`.

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

5.2 и 5.3 закрыты (`8f139c19e`). Проверка ключа идёт через
`Workspace.VerifyProviderAPIKey` и строит провайдера тем же путём, что и
агент; `TestConnection` получил `ctx`, так что закрытие диалога реально
отменяет запрос (таймаут в 5 с там был, а отмены от вызывающего не было
вовсе). `NewDoctor` вернулся к форме `(диалог, cmd)` и не делает
ввод-вывод в конструкторе; `ui/dialog` больше не импортирует
`internal/doctor`.

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

6.1 и 6.4 закрыты (`ce9d6ecdf`, AGENTS.md). Из «мёртвых» методов реально
мёртвыми оказались только `SendThread` и `AgentQueuedPrompts`:
`ActivateThread` получил вызов в 2.1, а `MCPAuthURL` используется в
`ui/model/mcp_auth.go`. Восемь однометодных ролей свёрнуты обратно с
восстановлением документации из `a09ecd54a^`.

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
