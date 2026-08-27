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

## 2. Баги — средние (открыто)

### agent
- `agent/usage.go` `summarize` — read-modify-write всей строки сессии (`sessions.Get` → долгий стрим → `Save`); `AddCost` из завершающейся делегации (`delegation_finalizer.go:476`) или title-generation, попавшие в окно, затираются. Та же гонка уже: `turn.go:718-724`. Фикс в `delegation_finalizer.go:457-466` закрыл только child-сторону. **Лечить атомарным `UPDATE … SET cost = cost + ?` вместо Get/Save.**
- `delegation_finalizer.go:172,668` — `OnAuthRefresh` для саб-агента передаёт `active=nil`; после 401 креды обновляются в main-агенте, но саб-агент продолжает с `t.model` со старым ключом (`runtime_builder.go:1424`) → повторный 401.
- `agent.go:481-483` — `dispatchDecision` ставит `call.acceptSeq` на копии по значению; `finishTurn:770 → requeueContinuation` (`dispatch.go:498-509`) кладёт в очередь с `acceptSeq==0` → любой Cancel соседа (`canceledBySeq:440`) сбрасывает post-summary continuation.
- `agent.go:752-753` — ошибка summarize в `finishTurn` → ранний return без `AgentFinished` и `drainNext`; очередь с RunID-промптами зависает, ожидающие `RunComplete` блокируются.
- `tools/mcp/connection.go:181,401-436` — `context.Canceled` используется как сентинел «потеря ownership/ping failed»; реальные сбои классифицируются как отмена пользователем и пропускают `StateError`. `connection.go:55` сравнивает `err.Error() != "signal: killed"` строкой.
- `tools/sennit_logs.go:684-692` — off-by-one в `lineStart` при `data` с завершающим `\n`; `alreadyReturned` (`:698`) никогда не срабатывает, курсорная пагинация «работает случайно».
- `turn.go:715-743` — `t.sessionLock` держится через два обращения к БД и `RotateThreshold` (I/O + полная пересборка runtime).

### thread / workspace / app
- `threadspawn/services.go:42-49` — `Coordinator()` кеширует nil/устаревший координатор навсегда → `registerParent`/`DeliverTaskCompletion` (`manager.go:337,603,737`) бьют в nil; завершения задач теряются молча.
- `app/services.go:302` ↔ `app/app.go:158`, `app_workspace.go:195-299` — поле `app.AgentCoordinator` пишется/читается без синхронизации (~15 читателей).
- `thread/lifecycle.go:1010` + `manager.go:438, 1186-1211` — auto-merge поток, завершающийся во время Shutdown: `runtime` обнулён до `onRunSuccess`, `mergeAttempt` падает на отменённом `m.ctx`, строка остаётся `running` до следующего Recover.
- `workspace/app_workspace.go:177-186` — ошибка `RunAndCaptureStream` возвращается только при `onProgress == nil`; стриминговый путь отдаёт нулевой результат как успех.
- `workspace/app_workspace.go:421-532` — `AgentRunStream`: небуферизованный `out`, все send безусловные; потребитель, переставший читать (`cmd/run.go:214` при ошибке), навсегда блокирует горутину — `ctx.Done` не выбирается.
- `thread/lifecycle.go:804-812` — отложенный `spawner.Release(ctx, …)` с ctx вызывающего; если ctx отменён, Release тоже падает → утекает целый App/БД (`:799` правильно использует `Background`).
- `thread/manager.go:404-408` — `failCreate` на отменённом ctx оставляет строку `pending` до конца процесса.
- `workspace/read_only_workspace.go` — `GetLastSession`, `UncommittedFiles`, `Stats` проксируют в родителя; `PrepareSessionChanges` (`:838`) помечает файлы потока «незакоммиченными» относительно родительского репо.
- `message/store/service.go:184-206` — `Delete` vs идущий стрим: `Update` между 189-195 пересоздаёт `pending[id]`, запись никогда не вытесняется.
- `message/content.go:390-401` — `AppendContent` дописывает дельту в **каждую** `TextContent`-часть (нет `return`) → дублирование текста.
- `db/connect.go:74-86`, `datadirlock.go:364-377` — ключ пула канонизируется до `MkdirAll`; символическая ссылка на ещё не существующий data-dir даёт два `*sql.DB` на один файл (WAL multi-connection случай, о котором предупреждает сам код).
- `app/shutdown.go:206-213` — nil-deref `p.app.agentDispatcher` при `p.app == nil` (пустой `&app.App{}` разрешён `app.go:41-44`).
- `thread/tasks.go:263-269` — delegation parent регистрируется до `setStatus`, который может упасть; комментарий утверждает обратный порядок.
- `threadspawn/attach.go:162-164` — события задач форвардятся только в git-workspace, хотя TaskManager есть всегда.

### config / oauth / shell / cmd
- `config/reload.go:777-797` — при бампе `credentialVersion` во время reload копируются только `APIKey`/`OAuthToken`; `Account`, `ProxyURL`, `APIKeyTemplate` из `UpdateProviderAccount` (`store.go:315-325`) теряются, `SetupCodex` не перезапускается (только Copilot, `:791`).
- `config/store.go:1091-1095` — `RefreshLockPath` подставляет provider ID в имя файла без экранирования (`ProviderFieldKey` экранирует) → ID с `/` или `..` создаёт lock-файлы вне `locks/`.
- `config/provider.go:869-975` — `TestConnection` глотает ошибки резолвера (`apiKey, _ =`), `&http.Client{}` игнорирует `ProxyURL`.
- `config/credentials/credentials.go:488` — `ImportCopilot` ходит в сеть с `context.TODO()` без дедлайна на старте.
- `config/store.go:946-958` — `SetProviderAPIKey` для токена вызывает только `SetupGitHubCopilot`, Codex-токен не получает заголовков (в отличие от `UpdateProviderAccount:326-331`).
- `shellconfig/builder.go:62-84` — повторный `add` имени, затомбстоненного раньше, пишет флаги рядом с `__sennit_tombstone`; `ParseTombstone` (`:35`) отвергает конфиг.
- `oauth/codex/oauth.go:174-176`, `oauth/mcp/handler.go:570-572` — `error=` в колбэке обрабатывается до проверки `state`: любой локальный запрос на loopback-порт срывает вход.
- `cmd/login.go:1049` — утечка signal-handler'ов; `cmd/login_codex.go:74-101` — частичная запись при неудачном логине; `providers/accounts/rotator.go` `Pick` берёт отключённый единственный аккаунт; `providerload/loader.go:115-117` игнорирует ошибку `RemoveRuntimeConfigField`; `cmd/logout.go:71,145` осиротевшая ветка `"hyper"`.
- `providers/accounts` `rotator.Pick`: используется отключённый единственный аккаунт.

### lsp / hooks / discover / fsext / log / git
- `hooks/hooks.go:183` — `sjson.SetRawBytes(out, k, v)` трактует ключи патча как пути: `"a.b"`, `*`, `#` вкладываются/падают вместо top-level merge.
- `hooks/runner.go:197-209` — по таймауту хука горутина и дочерний процесс утекают навсегда.
- `lsp/filesync.go:65-74` — Get/didOpen/Set неатомарны → дубли `didOpen`.
- `lsp/util/edit.go:178-189` — `CreateFile` без `Overwrite` затирает существующий файл; `:82` `WriteFile 0o644` теряет исходные права.
- `discover/ollama.go:59-70` — статус `/api/show` не проверяется; тело ошибки декодится в нули.
- `fsext/atomicwrite.go:49` — `AtomicCreateFile` через `os.Link`; падает на ФС без hardlink. `conditionalreplace.go:86-96` — межпроцессный TOCTOU.
- `log/http.go:38` — тело запроса буферизуется целиком вне зависимости от уровня; `:76-83,167-191` редакция пропускает `Cookie`/`Set-Cookie`, `key`, `credential`, `private_key`.
- `git/git.go:243,497,515,566,615,630` — ref/branch передаются без `--` → инъекция опций при имени, начинающемся с `-`.
- `fsext/ls.go:335` — `ListDirectory` следует по симлинкам, glob — нет.

### ui
- `dialog/stats.go:693-699,767` — `errs[i]` не очищается при успешной перезагрузке → старая ошибка показывается вечно. `stats.go:559` — `StatsLoadedMsg` не `DialogAddressed`: диалог поверх Stats роняет сообщение, «Reading usage…» навсегда.
- `dialog/mcp_auth.go:1448,1457` — `m.cancelAuth = nil` без вызова → утечка контекста из `startAuth` (`:1474`).
- `chat/shell.go:623-625,715-716` — ширина вычитается дважды (`width-2` + `cappedMessageWidth`); вывод усечён на 2 колонки, `xOffset` clamp (`:787`) смещён.
- `dialog/models.go:1335-1353` — `displayProvider := provider` делит backing array `Models`; каждое открытие мутирует общий каталог в `m.providers`.
- `chat/assistant.go:473-507` — «инкрементальный» `thinkingKey` никогда не срабатывает (второй вызов за кадр не проходит строгий `>`), полный хеш reasoning-текста на каждом тике.
- `chat/shell.go:617-622` — комментарий обещает кеш в `RawRender`, кеша нет; `HoverableAt` (`:644`) ре-триммит на каждое движение мыши.
- `dialog/accounts.go:1153-1167` — `ShortHelp` никогда не рисуется; `:928` — `m.accs` затирается до проверки `Err`.
- `dialog/oauth.go:265-268` — неудача `browser.OpenURL` останавливает polling, хотя код и URL на экране.
- `model/chat.go:1205-1250` — `DelayedClickMsg.ItemIdx` сырой индекс; `RemoveMessage`/`AppendMessages` в 400-мс окне сдвигают его.
- `chat/streaming_markdown.go:181-189` — замороженный префикс не ревалидируется при позднем `[ref]: url`.
- `dialog/question_form.go:1105-1115,1185` — неизвестный `req.Type` → `comps[0].SetFocused` на nil.
- `list/list.go:728-739` — `SetItems` чистит `cache`, но не `freezeSuppressed`.
- `ui/attachments/attachments.go:196,255-256` — off-by-one в overflow-чипе: «3 more…» при 2 скрытых; при `width < maxItemWidth` `fits = -1`, лимит не срабатывает, чипы вылезают за строку. **(H)**
- `model/keypress.go:386-394` — триггер `@`-completion берёт `len(curValue)` вместо позиции курсора; `@` в середине текста → `insertCompletionText` (`editor.go:114-127`) вставляет не туда.
- `model/mcp_actions.go:11-37` — `runMCPPrompt` шлёт безусловный `closeDialogMsg`; если за это время открылся другой диалог (permission prompt), закроется он.
- `model/threads.go:255-261,564` + `threads_cache.go:177` — `selected()` отдаёт указатель в `m.visible`, алиасящий `cache.value`; in-place delete в `applyEvent` сдвигает массив (латентно).
- `model/editor_input.go:350,447` — байтовая длина смешана с rune-колонкой при фиксапе курсора.
- `model/skills.go:105-108` — сортировка глобального `builtinSkillsCache.skills` in-place из render-пути.

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
