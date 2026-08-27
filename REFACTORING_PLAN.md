# План рефакторинга и найденные баги

Аудит `internal/` от 2026-08-27. Статика чистая (`go vet`, `golangci-lint` — 0 замечаний,
`go test -race ./internal/...` — зелёный), поэтому всё ниже найдено чтением кода.
Каждый пункт — с `файл:строка` и сценарием; уверенность помечена (H — подтверждено
трассировкой вызовов, M — логика прочитана, сценарий правдоподобен).

Не покрыто аудитом: `third_party/` (см. §4), `ui/common`, `ui/styles`.

Открытые пункты `TECHDEBT.md` здесь не дублируются.

---

## 0. Статус

**Этап 0 (баги B1-B10) выполнен 2026-08-27.** Все десять закрыты, каждый с
регрессионным тестом, про который проверено, что он падает без фикса.
Проверка после работ: `go build ./...`, `gofmt`, `go mod tidy -diff`,
`golangci-lint` (0 замечаний), `./scripts/check_log_capitalization.sh`,
`./scripts/check_cross_platform.sh` (windows/darwin/linux) и
`go test -race -failfast -count=1 ./...` — всё зелёное.

Найдено по ходу и **не** починено (отдельные решения):

- `config/store.go` `RemoveRuntimeConfigField` при `ScopeWorkspace` пишет в
  `globalDataPath`, а не в файл workspace, в отличие от всех соседних
  мутаторов, которые резолвят путь через `ConfigPath(scope)`. Сегодня
  безвредно: единственный вызывающий (`providerload/loader.go:116`) всегда
  передаёт `ScopeGlobal`. Латентный баг, а не намеренное поведение.
- Copilot в TUI: пользователь, который достаёт GitHub **только** через
  прокси, по-прежнему не может ввести его до первого входа — прокси
  берётся из уже сохранённого `providers.copilot.proxy_url`. У codex для
  этого есть шаг `OAuthStateProxy` (`oauthProxyConfigurer`). Включать ли
  такой шаг для Copilot — UX-решение: он появится у **всех** пользователей
  Copilot, поэтому в багфикс не вошёл.
- Гонка на неохраняемом поле `app.AgentCoordinator` (см. §2) осталась, но
  чинить её стало проще: после B4 поле меняется в одном месте.


### Вторая волна (2026-08-27, после этапа 0)

Закрыто из §2 и этапа 1, каждое с регрессионным тестом: гонка стоимости
сессии (`usage.go` и `turn.go` → инкремент в SQL через `SaveUsage`),
`acceptSeq`, теряемый при requeue континуации, ошибка summarize, оставлявшая
очередь висеть, `context.Canceled` как сентинел в MCP, потеря части кредов
при reload и экранирование ID провайдера в пути lock-файла, `CreateFile`,
обнулявший существующий файл, двойной `didOpen`, ключи хук-патча как
JSONPath, инъекция опций git через имя ветки с ведущим дефисом, утечка
горутины `AgentRunStream` и проглоченная ошибка стрима, дублирование в
`AppendContent`, залипание диалога Stats, утечка контекста MCP-авторизации,
мутация общего каталога моделей, двойное вычитание ширины в выводе shell,
воскрешение `pending` у удалённого сообщения, три места с уже отменённым
контекстом в `internal/thread`, статус ответа Ollama, выбор отключённого
аккаунта ротатором.

Этап 1 (DRY в `internal/agent`) выполнен частично: схлопнут дубль
`UpdateModels`/`runUpdateModels`, извлечён общий resolve-refresh-retry,
вариадик `runtimeOperationPort` заменён на явный параметр, снят алиас
`dispatch`, убрано write-only поле `agents`, упрощён `buildStreamAgent`.

### Ложные срабатывания аудита — проверено, чинить нечего

- **Пул `internal/db`.** `os.MkdirAll` на висячем симлинке падает раньше,
  чем откроется вторая БД: путь fail-closed, двух `*sql.DB` на один файл не
  возникает. Указанные в §2 номера строк не соответствуют коду.
- **Права файла в `lsp/util/edit.go`.** `os.WriteFile` не применяет режим к
  уже существующему файлу (mode передаётся в `open(2)` только при
  `O_CREAT`), а `applyTextEdits` всегда читает файл до записи — права не
  терялись.
- **Шесть `buildXProvider` в `runtime_builder.go`.** Не близнецы: у
  Anthropic своя логика Bearer и срезания `X-Api-Key`. Обобщение дженериками
  здесь противоречит KISS — пункт снят.
- **`snapshotStreamRuntime`.** Не бесполезный алиас, а точка переопределения
  для `TestRunSubAgentUsesOneRuntimeSnapshotForBudgetAndProvider`. Оставлен,
  добавлен комментарий.
- **`runSennitLogsText` и соседи** формально без прод-вызовов, но на них
  висит ~27 тестов — удаление требует отдельного решения, а не зачистки.
- **Абандон горутины в `hooks/runner.go`** — осознанный компромисс с
  обоснованием в комментарии, а не недосмотр.

---

## 1. Баги — критичные (закрыто)

| # | Где | Что | Сценарий | Ув. |
|---|-----|-----|----------|-----|
| B1 | `thread/lifecycle.go:356-364` | `startRun` при `coord == nil` синхронно зовёт `handleRunComplete`, который берёт `c.opMu` (`:944`), а все вызывающие (`manager.go:272→360`, `lifecycle.go:779→814`, `tasks.go:293`) уже держат этот же нереентерабельный `opMu` | Spawn вернул App без координатора (документированный случай, `app/app.go:161-164`) → Create/Send виснут навсегда с захваченным локом потока | H |
| B2 | `agent/turn.go:909-917` | `handleStreamError`: ошибка создания синтетического tool-result возвращается **до** `AddFinish`/`Update` (`:919-963`) | БД занята/закрыта при shutdown во время прерванного tool-calling хода → assistant-сообщение остаётся «незавершённым», UI показывает «running» вечно — ровно то, от чего защищает комментарий на `:843-852` | H |
| B3 | `skills/watch.go:39-49` | `scanSkillFiles`: колбэк `fastwalk` выполняется на нескольких воркерах, `snapshot` map пишется без мьютекса (в `skills.go:234` тот же обход защищён) | Два SKILL.md в разных подкаталогах → `fatal error: concurrent map writes` на poll-е | H |
| B4 | `cmd/root.go:265` + `cmd/run.go:167` | `sennit run`: Attach инжектирует delegation-tools в `a.AgentCoordinator` (`threadspawn/attach.go:172-174`), затем `InitCoderAgentNonInteractive` перезаписывает `app.AgentCoordinator` (`app/services.go:302`) новым без thread/task-tools; старый не `Close()`-ится | В неинтерактивном режиме агент не видит инструменты потоков; утечка координатора | H |
| B5 | `agent/delegation_finalizer.go:118-157, 804-809` | `runtimeInputs()` вызывается 4-5 раз на каждый `turnDispatcher.run` (`turn_dispatcher.go:198,230,235,240`) и каждый раз клонирует `http.DefaultTransport` в новый `*http.Transport` (свой пул idle-соединений, никогда не закрывается) | Постоянный трафик → churn транспортов/горутин; кеш скомпилированного runtime не работает, т.к. inputs пересобираются до проверки кеша | H |
| B6 | `config/store.go:526-554` | `RemoveRuntimeConfigField`/`WriteRuntimeConfigFields` обходят `stalenessMu` и не перечитывают snapshot; watcher (`watch.go:930`) видит собственную запись как внешнюю → лишний `ReloadFromDisk` + `onExternalChange` (переинициализация MCP). Для `ScopeWorkspace` пишет в `globalDataPath` (`:527`), а не в workspace-файл | `providerload/loader.go:116` | H |
| B7 | `cmd/accounts.go:288-291` | API-ключ печатается в терминал; ввод обрезается по первому пробелу | Секрет в scrollback/логах терминала | H |
| B8 | `cmd/session.go:499-501` | Паника при `$PAGER`, состоящем из пробелов (`strings.Fields` → пустой slice → `[0]`) | `PAGER=" " sennit session …` | H |
| B9 | `fsext/atomicwrite.go:37-79`, `conditionalreplace.go:69-96` | Нет `fsync` перед `rename` | Сбой питания сразу после записи конфига/аккаунтов → пустой или нулевой файл на месте валидного | H |
| B10 | `oauth/copilot/oauth.go:49,119,160` | Три захардкоженных `http.Client{Timeout:30s}` без прокси (Codex прокси поддерживает) | Логин Copilot за прокси невозможен | H |

## 2. Баги — средние — что осталось

Закрытые пункты удалены (история в git, сводка в §0). Ложные срабатывания
перечислены в §0 и сюда не возвращаются.

### agent
- `delegation_finalizer.go:172,668` — `OnAuthRefresh` саб-агента получает
  `active=nil`; после 401 креды обновляются у главного агента, а саб-агент
  продолжает со старым ключом, вшитым в провайдер при сборке
  (`runtime_builder.go:1424`) → повторный 401.
- `tools/sennit_logs.go:684-692` — off-by-one в `lineStart`, когда чанк
  оканчивается на `\n`; `alreadyReturned` (`:698`) не срабатывает,
  курсорная пагинация работает по случайности.

### thread / workspace / app
- **Гонка на `app.AgentCoordinator`** (`app/services.go` ↔ `app/app.go:158`,
  `app_workspace.go:195-299`, ~15 читателей без синхронизации). После B4
  поле меняется в одном месте — чинить стало проще. Самый весомый из
  оставшихся.
- `threadspawn/services.go:42-49` — `Coordinator()` навсегда кеширует nil
  или устаревший координатор → `registerParent`/`DeliverTaskCompletion`
  бьют в nil, завершения задач теряются молча.
- `thread/lifecycle.go:1010` + `manager.go:438,1186-1211` — auto-merge
  поток, завершающийся во время Shutdown, остаётся в статусе `running` до
  следующего `Recover`.
- `workspace/read_only_workspace.go` — `GetLastSession`,
  `UncommittedFiles`, `Stats` проксируют в родителя;
  `PrepareSessionChanges` (`:838`) считает файлы потока незакоммиченными
  относительно родительского репозитория.
- `app/shutdown.go:206-213` — nil-deref `p.app.agentDispatcher` при
  `p.app == nil` (пустой `&app.App{}` разрешён `app.go:41-44`).
- `threadspawn/attach.go:162-164` — события задач форвардятся только в
  git-workspace, хотя `TaskManager` есть всегда.

### config / oauth / cmd
- `config/provider.go:869-975` — `TestConnection` глотает ошибки резолвера
  (`apiKey, _ =`) и игнорирует `ProxyURL`: у пользователя за прокси
  проверка падает с непонятным сообщением.
- `config/credentials/credentials.go:488` — `ImportCopilot` ходит в сеть с
  `context.TODO()` без дедлайна на старте.
- `shellconfig/builder.go:62-84` — повторный `add` ранее затомбстоненного
  имени пишет флаги рядом с `__sennit_tombstone`, и `ParseTombstone`
  (`:35`) отвергает конфиг.
- `oauth/codex/oauth.go:174-176`, `oauth/mcp/handler.go:570-572` — `error=`
  в колбэке обрабатывается до проверки `state`: любой локальный запрос на
  loopback-порт срывает вход.
- `cmd/login.go:1049` — утечка signal-handler'ов;
  `cmd/login_codex.go:74-101` — частичная запись при неудачном логине;
  `cmd/logout.go:71,145` — осиротевшая ветка `"hyper"`.

### fsext / log
- `fsext/atomicwrite.go:49` — `AtomicCreateFile` через `os.Link`, падает на
  ФС без hardlink; `conditionalreplace.go:86-96` — межпроцессный TOCTOU.
- `fsext/ls.go:335` — `ListDirectory` следует по симлинкам, glob — нет.
- `log/http.go:38` — тело запроса буферизуется целиком независимо от
  уровня логирования; `:76-83,167-191` — редакция пропускает
  `Cookie`/`Set-Cookie`, `key`, `credential`, `private_key`. **Секреты в
  логах — брать в первую очередь.**

### ui
- `ui/attachments/attachments.go:196,255-256` — off-by-one в overflow-чипе;
  при `width < maxItemWidth` `fits = -1`, лимит не срабатывает и чипы
  вылезают за строку. **(H)**
- `model/keypress.go:386-394` — триггер `@`-completion берёт
  `len(curValue)` вместо позиции курсора: `@` в середине текста вставляет
  не туда.
- `model/mcp_actions.go:11-37` — безусловный `closeDialogMsg` закрывает
  чужой диалог, если тот успел открыться.
- `dialog/question_form.go:1105-1115,1185` — неизвестный `req.Type` →
  `comps[0].SetFocused` на nil.
- `model/chat.go:1205-1250` — `DelayedClickMsg.ItemIdx` сырой индекс,
  сдвигается при `RemoveMessage`/`AppendMessages` в 400-мс окне.
- `model/editor_input.go:350,447` — байтовая длина смешана с rune-колонкой.
- `model/threads.go:255-261,564` — `selected()` отдаёт указатель в
  массив, который `applyEvent` сдвигает in-place (латентно).
- `model/skills.go:105-108` — сортировка глобального кеша in-place из
  render-пути.
- `dialog/accounts.go:928` — `m.accs` затирается до проверки `Err`;
  `:1153-1167` — `ShortHelp` никогда не рисуется.
- `dialog/oauth.go:265-268` — неудача `browser.OpenURL` останавливает
  polling, хотя код и URL уже на экране.
- `chat/assistant.go:473-507` — «инкрементальный» ключ не срабатывает,
  полный хеш reasoning-текста на каждом тике (перф).
- `chat/streaming_markdown.go:181-189` — замороженный префикс не
  ревалидируется при позднем `[ref]: url`.
- `list/list.go:728-739` — `SetItems` чистит `cache`, но не
  `freezeSuppressed`.

---

## 3. План рефакторинга (SOLID / DRY / KISS / YAGNI)

Порядок — по соотношению «риск/выигрыш». Каждый этап — отдельный PR (или серия), не смешивать с багфиксами из §1-2.

### ~~Этап 0. Багфиксы §1 (B1-B10)~~ — выполнено (см. §0)

### Этап 1. DRY в `internal/agent` (низкий риск, чистый выигрыш)
1. Удалить дубли: `runtime_builder.go:515-527 UpdateModels` ≡ `turn_dispatcher.go:75-87 runUpdateModels`; `buildAgentModel:327-369` ≡ `buildCustomAgentModel:380-425` (у одного нет `break` на `:344-348` — разное поведение при нескольких совпадениях, выбрать одно). 
2. Блок «runtimeFor → refreshTokenIfExpired → key changed? → runtimeFor → newActiveRuntime» ×3 (`turn_dispatcher.go:93-105,156-172,226-245`) → один метод.
3. Шесть `buildXProvider` (`runtime_builder.go:1081-1390`) различаются только типом опций → generic/таблица.
4. **`runtimeInputs()` — вычислять лениво один раз и кешировать** в `delegationFinalizer` (закрывает B5); `runtimeToolInputs` (`:71-87`) перестаёт таскать `toolBuildErr` в данных.
5. YAGNI: `runtimeOperationPort ...` variadic + `[0]` (`:529-539`) → указатель; `buildStreamAgent` (5 return-значений, 4 pass-through); `snapshotStreamRuntime` = алиас `effectiveStreamRuntime`; лестницы type-assertion в `runSubAgent:572-620,670-675` — единственная реализация `*sessionAgent` → конкретный тип или один маленький интерфейс; `coordinatorAgentPort`, алиас `dispatch = dispatcher`.
6. Мёртвый код: `sennit_logs.go:206-248` (`runSennitLogsText`, `stripMetaFooter`, `readLastLines`), `logRecord.size`, `coordinator.go:41-42`, `turn_dispatcher.go:43 agents`.

### Этап 2. Разрезать god-файлы `internal/agent` (SRP/ISP)
- `agent.go` (1330): `session_call.go` (типы), `run_turn.go` (`runTurn` 230 строк, `finishTurn`, `dispatchDecision`), `agent.go` — конструктор + интерфейс. Интерфейс `SessionAgent` (20 методов) → разбить: `Steer`, `BeginAccepted`, `RegisterDelegationParent`, `SendToParent`, `GenerateTitle` имеют по одному вызывающему.
- `dispatch.go:916-1097` — persistence/pubsub-методы (`persistCanceledTurn`, `Cancel`, `CancelAll`) противоречат заголовку файла → в `agent.go`. `CancelAll` — polling 10 мс → `sync.Cond`/done-канал.
- Функции >100 строк: `handleStreamError` (turn.go:810-965), `runSubAgent` (545-694), `agenticFetchTool` (803-898, замыкание в 3 уровня), `scanBackward` (sennit_logs.go:612-805), `NewBashTool`, `buildTools` (runtime_builder.go:202-312).
- `usage.go summarize` (~250 строк) — вынести вычисление usage из транзакции сессии (см. гонку в §2).

### Этап 3. `internal/thread` + `workspace` + `app`
1. `thread/manager.go` (1454) + `lifecycle.go` (1310): `merge.go` (`mergeAttempt/finishMerge/discardMerged/keepMergedBranch/recordDiscardNotice`, `manager.go:808-1009`), `permissions.go` (`lifecycle.go:442-490,634-649`); `steer` (`:514-625`) и `handleRunComplete` (`:918-1070`) → match/park/finalize.
2. Единый `lifecycle.releaseRuntime(ctx, c, sessionID, cancelRun bool)` вместо пяти копий teardown (`manager.go:878-898, 1047-1059, 1186-1211`, `lifecycle.go:868-877, 1013-1019`). Заодно чинит бегущие с `m.ctx`/caller-ctx Release'ы (§2).
3. `m.registerThreadParent(handle, st)` вместо трёх копий (`manager.go:334-338, 598-604, 731-740`).
4. **`AppWorkspace` — фасад на ~100 методов, интерфейс на 94 (`workspace.go:489-504`)** — ISP-нарушение №1 в проекте. Разбить на role-структуры (sessions, agent, MCP, accounts, config), встроить. `readOnlyWorkspace` с ~60 override'ами и reflection-тестом → default-deny. `accountStore()` вместо шести `accounts.NewFileStore(config.GlobalAccountsFile())` (`app_workspace.go:749-818`).
5. `app/shutdown.go:169-418` `Shutdown` (~250 строк) → функция на фазу; `tasks.go:162-305 Create` (~145).
6. `app/events.go:94-125` ≡ `151-177`; `thread/store.go` три конвертера `[]db.Thread→[]Thread`; `message/content.go:412-521` семь «найти первую часть типа T» → generic; `store/service.go:250-264` N round-trip'ов при наличии `DeleteSessionMessages`; `lock/lock.go` `File`/`TryFile`.
7. YAGNI: `deliverCompletion` (обёртка в одну строку), `threadControl.parentSessionID` дублирует колонку БД, `AttachDeps`/`AttachWithDeps` только для тестов, `services.go:231-239` тестовый accessor в prod, `SetCurrentSessionGeneration` игнорирует аргументы.

### Этап 4. `internal/config` + `oauth` + `shellconfig`
1. `ConfigStore` (7 мьютексов, staleness, agents snapshot, MCP epochs): `store_credentials.go` (`UpdateProviderAccount`, `ActivateAccount`, `SetProviderAPIKey`, `PersistRefreshedToken`, `withRefreshLock`); agent-snapshot из `watch.go` в свой тип.
2. `applyProviderSetup(id, *ProviderConfig)` вместо четырёх копий Copilot/Codex-switch (`store.go:326,954,982`, `reload.go:791`) — закрывает два бага §2. Дубль «собрать ProviderConfig + записать api_key/oauth» (`store.go:355-398` vs `932-1011`).
3. `load.go:536/562` — `jsons.Merge` дважды (O(n²) marshal), убрать первый.
4. `oauth`: loopback-сервер дублируется (`codex/oauth.go:97-204`, `mcp/handler.go:384-667`) → общий пакет; Copilot перевести на `postToken`/`httpClient` Codex (закрывает B10).
5. `shellconfig`: одинаковый dispatch в `hook.go:21`, `lsp.go:22`, `mcp.go:24`, `provider.go:23`, `model.go:32`; `lspRemove`==`mcpRemove`; три «filter []any by id/name».
6. `cmd`: switch алиасов провайдеров ×3 (accounts/login/logout), style-переменные ×2, discovery+enrich ×2 (`models.go`, `providerload/discover.go`), `refreshCmd.RunE` ~140 строк, `session.go` 679 строк.
7. Мёртвое: `project_init.go ProjectInitFlag`, `shell/shell.go:29-36 ShellType`, `BackgroundShellInfo`, `WaitContext`, `mcp/handler.go:479 bind()`, `codex/http.go:38 ValidateProxy`, `codex/usage.go:135 RecordUsage`, `loader.go` pass-through обёртки и `cmp` import-keeper, `import.go kinds`.

### Этап 5. `internal/ui`
1. `session_panel.go`: секции agents/threads — копипаста в `sessionPanelPlan` (495-524), `sessionPanelRowLayout` (734-764), `drawSessionPanel` (946-1033) + три header-функции + тройки hover/rect → тип `panelSection`.
2. `model/ui.go`: `UI` — 20+ под-состояний в одной структуре; `Update` (576-727) — 25-case type switch, уже делегирующий в `update*` → таблица. `updateSession` (339 строк), `keyMapForPlatform` (265), `generateLayout` (211).
3. `chat/agent.go` (1115) → item / render contexts / status renderers; `RenderTool` Agent vs AgenticFetch (455-515 vs 599-645); `chat/file.go` Edit/MultiEdit (224-271 vs 306-362); `tools_copy.go formatParametersForCopy` 15 case → таблица.
4. `diffview.go`: `getContent` ×2, header-блок ×4, `renderUnified`/`renderSplit` общий скелет.
5. `dialog`: `permissions.go` списки имён инструментов ×4 (364/562/581/655) → из реестра; spinner-boilerplate ×2; `stats.go` три breakdown-рендерера; `question_form.go Draw` (253 строки); `batchActions` (312).
6. `model/chat.go`: «RawRender-or-Render» ×3, `SelectFirst/Last/Next/Prev` → один `walk(step)`.
7. Кеш-идиома ×3: `threads_cache.go:86-211` ≡ `agents_cache.go:63-168` ≡ `threads_dock.go:153-273` → generic `listCache[T]`. Model-update замыкания ×4 (`dialog_actions.go:80-111,145-184,399-411`, `update_settings.go:321-328`) → `updatePreferredModelCmd`. Busy-guard новой сессии ×3, внешний редактор ×2, scroll+follow (`mouse.go:257-277` ≡ `keypress.go:549-568`). `layout.go`: inline-editor ×2, compact split ×2, `Draw` мутирует layout (`:115-118`) — перенести в Update. `updateSession`, `applyChromeDialogAction`, `handleEditorBindingKeyPress` → таблицы обработчиков.
8. Мёртвое: `ActionToggleNotifications`, `choiceList.heightChanged`, `ToolRenderOpts.IsSpinning`, комментарии на несуществующие `sendAfterSessionLoaded`/`toolOutputMarkdownContent`, `list.go:699` self-assign, `InvalidateFrozen`≡`Invalidate`, `filepicker.go:1267`, `key.NewBinding` в каждом `ShortHelp`; `keys.go` биндинги без `key.Matches` (`Chat.Tab`, `AddAttachment`, `ScrollLeft/Right`, `Editor.Commands`, `MentionFile`); `requestSessionLoad` — обёртка; `keys.go:324-334` на darwin маппит `ctrl+c`→`super+c` (проверить намерение).

### Этап 6. `lsp` / `hooks` / `discover` / `fsext`
- Один обход fastwalk вместо `skills.go:247` / `watch.go:34` (закрывает B3).
- `apply*Meta` ×4 в `discover` (litellm/llamacpp/lmstudio/omlx) → generic.
- `lsp`: дубль `handlesFile` (`client.go:244`/`filesync.go:52`); `lifecycle.go:303-399 restart` → `failCandidate`; `runtime` с 6 func-hook полями (половина тестовые) → интерфейс; `prepareRestart` closure-returning-closure для первого старта.
- `fsext`: одна temp-write последовательность вместо трёх (`atomicwrite.go:28-84`, `conditionalreplace.go:57-77`) с fsync (закрывает B9); `shouldIgnore` ×2 (`ls.go:183-225`/`307-325`); `traverseUp`/`traverseUpBounded`; всегда-true `gitignore` в `globWithDoubleStar`.

---

## 4. `third_party/` — недокументированный форк

`go.mod:203,205` заменяют `charm.land/fantasy@0.40.0` и `charmbracelet/x/powernap@v0.1.6` на локальные копии. Пять коммитов с 2026-08-24 внесли ~600 строк поведенческих правок (retry/429-ротация, untyped-overload, schema preservation, LSP). Базовый SHA и список патчей нигде не записаны — обновление upstream превращается в ручной трёхсторонний merge.

**Действие:** `third_party/PATCHES.md` с upstream-SHA и rationale на каждый патч; попытаться отправить патчи upstream; либо `git subtree` с записанным baseline.

---

## 5. Порядок работ (резюме)

1. Багфиксы B1-B10 + тесты на сценарии (каждый — отдельный коммит).
2. `third_party/PATCHES.md`.
3. Этап 1 (agent DRY) → Этап 2 (agent split).
4. Этап 3 (thread/workspace; `AppWorkspace` — самый большой выигрыш и самый большой риск, делать последним в этапе, под reflection-тест).
5. Этап 4 (config/oauth) → Этап 6 (мелкие пакеты) → Этап 5 (UI, наименее рискованный, можно параллельно).
6. Средние баги §2 закрываются по ходу этапов, где указано «закрывает», остальные — отдельными коммитами по приоритету: гонки usage/coordinator, утечки горутин (`AgentRunStream`, hooks), потом UI.

Перед каждым push: `make check` + скрипты CI-lint, затем проверить CI (см. память проекта).
