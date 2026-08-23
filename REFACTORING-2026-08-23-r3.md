# Ревью проекта и план рефакторинга — раунд 3 (2026-08-23)

Третий проход после `REFACTORING.md` (2026-08-20, 52 пункта) и
`REFACTORING-2026-08-23.md` (128 пунктов), оба закрыты и удалены коммитом
`90562c91`. Этот документ — новый список: баги, нарушения границ слоёв,
SOLID/DRY/KISS/YAGNI, мёртвый код, и поэтапный план.

Метод: 8 независимых ревьюеров по областям (`agent`, `agent/tools`+`mcp`,
`thread`/`workspace`/`app`/`proto`, `config`/`shellconfig`/`hooks`,
`ui/model`+`ui/chat`, `ui/dialog`+`list`+прочий UI, `message`/`session`/
`db`/`permission`/`pubsub`/`shell`, `cmd`/`lsp`/`oauth`/листовые пакеты)
плюс `go vet`, `golangci-lint` (0 issues), `deadcode` и граф импортов
`go list`. Все **high**-баги перепроверены чтением кода вручную
(`safe.go`, `connection.go`, `content.go`, `root.go`, `mouse.go`,
`question_form.go`). Medium/low — со слов ревьюера с указанием строк;
перед правкой перечитать.

Условные обозначения: **S** — до часа, **M** — полдня–день, **L** — несколько
дней. Номера строк — на момент коммита `3affd046`.

---

## 1. Баги

### 1.1 High (P0) — чинить первыми

| # | Где | Что |
|---|-----|-----|
| B1 | `internal/agent/tools/safe.go:16-31`, `bash.go:379-388` | `safeCommands` содержит обёртки `env`, `nice`, `nohup`, `time`, `timeout` и `kill`/`killall`. Префиксное совпадение делает `timeout 5 rm -rf ~`, `nohup rm -rf .`, `env rm -rf x`, `kill -9 -1` «read-only»: permission-промпт не показывается (`isSafeReadOnly=true`), а `bannedCommands` не содержит `rm`. **Это обход разрешений.** Фикс: убрать обёртки и `kill*` из списка, либо разрешать argv[0] через уже построенный mvdan AST (`bashConfinementRefusal`) и проверять *внутреннюю* команду. Добавить тест в `permission_coverage_test.go`. |
| B2 | `internal/agent/tools/mcp/connection.go:96,144,188` vs `transport.go:107-114`, `connection.go:203` | Транспорт оборачивается в `channelTransport` **до** вызовов `closeIdleTransport(transport)` и `maybeStdioErr(err, transport)`; оба делают type-switch по `*mcp.StreamableClientTransport`/`*mcp.SSEClientTransport`/`*mcp.CommandTransport` и не разворачивают обёртку → `closeIdle` всегда `nil`: keep-alive соединения и горутины `http.Transport` текут на каждом renew/teardown/Close; stdio-диагностика npx никогда не добавляется. Фикс: держать сырой транспорт в локальной переменной или добавить case `*channelTransport` с рекурсией в `inner`. |
| B3 | `internal/message/content.go:384,422,437,451` | `FinishThinking`, `AppendReasoningContent`, `AppendReasoningSignature`, `SetReasoningResponsesData` пересобирают `ReasoningContent` по ручному списку полей и теряют `ThoughtSignature`/`ToolID`/`ResponsesData` (ровно та ошибка, от которой предостерегает комментарий у `FinishToolCall`). `agent/turn.go:361-368` вызывает `SetReasoningResponsesData`/`AppendThoughtSignature` и тут же `FinishThinking()` → метаданные reasoning (OpenAI Responses, Gemini) стёрты до первого flush; продолжить reasoning на следующем ходу нельзя. Фикс: копировать part и менять только нужные поля (`c.FinishedAt = now; m.Parts[i] = c`). |
| B4 | `internal/message/content.go:141` | `ResponsesReasoningMetadata.UnmarshalJSON` не обратен `MarshalJSON`: пишется envelope `{"type","data":{…}}`, читается плоская структура → после SQLite round-trip `ItemID`/`EncryptedContent`/`Summary` нулевые (тест `content_test.go:44-69` закрепляет это как «quirk»). Фикс: разворачивать `data` с fallback на плоский формат. |
| B5 | `internal/agent/usage.go:202-206` + `agent.go:676-683` | Esc во время авто-summarize: `summarize` возвращает `deleteErr` (обычно `nil`) при `context.Canceled`, `finishTurn` видит успех, `ToolCalls()` непуст → `requeueContinuation` с «previous session was interrupted…», а `cancelled`/mark сброшены → агент стартует **новый** ход на несжатом контексте и опять упирается в summarize. Фикс: возвращать `context.Canceled` после delete; `finishTurn` при отменённом summarize не ставит continuation. |
| B6 | `internal/agent/agent.go:822-825`, `continuation.go:81-100,141-146` | Цикл пробуждения continuation без backoff/лимита: если continuation падает **до** `foldCompletions` (`sessions.Get` not found, `stripContinuationPlaceholder`, `createStepAssistant`), inbox не тронут, `active==nil` → `startContinuation` тут же запускает ещё одну горутину. Сценарий: удалили родительскую сессию, пока фоновая задача бежала → горячий цикл записей в БД, `RunComplete` и `slog.Error` до выхода процесса. Фикс: лимит попыток на батч inbox + backoff; при `ErrNotFound` сессии — дропнуть inbox. |
| B7 | `internal/ui/model/root.go:292-341` | Роутер доставляет асинхронные результаты по **активному экрану**; `mainScreenOwned` помечен только `threadDockActivityLoadedMsg`. Открыли дашборд (ctrl+e) → `WindowSizeMsg`/тик уже запустил `dispatchBusyRefresh` (`busyFetchInFlight=true`) → `busyStateMsg` попадает в `handleDashboardMsg` и выбрасывается → `busyFetchInFlight` навсегда `true`, `dispatchBusyRefresh` больше не шлёт запросы. То же для `lsp.fetchInFlight`, `promptQueueCache.inFlight`, `sess.dialogLoading` (диалог сессий больше не открывается), `ops.*Loading`. Фикс: пометить `mainScreenOwned` все результаты, которые диспатчит только главный UI (`busyStateMsg`, `promptQueueMsg`, `lspStatesMsg`, `sessionsLoadedMsg`, `loadSessionMsg`, `modelSelectResult`, `yoloToggledMsg`, …) — или ввести owner-id на каждую команду. |
| B8 | `internal/ui/model/mouse.go:227-247` | Edge auto-scroll выполняется на **каждый** `MouseMotionMsg` (`MouseModeAllMotion`), не гейтится по drag (`m.chat.mouseDown`/`msg.Button`), и сравнивает абсолютный `msg.Y` с `m.chat.Height()-1` → наведение мыши на редактор/панель прокручивает чат и снапает к выделению. Фикс: только при активном drag и относительно чат-координаты `y`. |
| B9 | `internal/ui/dialog/question_form.go:246-254` + `question_freetext.go:255-262`, `ui/model/layout.go:146,182` | Esc на сфокусированном `FreeText` лишь блюрит редактор и ждёт второго Esc, но `Draw` на каждом кадре вызывает `activeInline.SetFocused(m.focus == uiFocusEditor)` → `editor.Focus()` → второй Esc опять «первый». В живом приложении Esc не отменяет свободный текстовый вопрос никогда (тест проходит, т.к. не рисует между нажатиями). Фикс: флаг «blurred by esc» в `FreeText`, который `SetFocused` не перебивает; либо форма проверяет свой флаг, а не `editor.Focused()`. |
| B10 | `internal/lsp/client.go:486-497,261,295-308` | `Restart` мёртвого LSP: `CloseAllFiles` удаляет запись из `openFiles` только при успешном `didClose`; у мёртвого сервера все close падают → карта не очищена → `OpenFile` в цикле reopen упирается в «already open» → новый сервер не получает ни одного `didOpen`. Фикс: чистить `openFiles` безусловно после `Close`. |

### 1.2 Medium (P1)

**agent**
- `internal/agent/continuation.go:82-86` — авто-continuation идёт через `a.Run` в обход `coordinator.run`: без `Runtime`, `ProviderOptions` (thinking), `MaxOutputTokens`, `OnAuthRefresh`, без ожидания MCP и предварительного refresh токена. Истёкший OAuth во время фоновой задачи → 401 без retry. Фикс: `Coordinator.runContinuation`, собирающий вызов через `runtimeFor`.
- `internal/agent/usage.go:100-113` vs `agent.go:676-761` — отложенный handoff очереди в `summarize` срабатывает и на пути авто-summarize из `finishTurn` (`claim != nil`): вложенный `a.Run` выполняется **до** публикации `AgentFinished`; его ошибка подменяет результат успешного внешнего хода. Фикс: handoff только при `claim == nil`.
- `internal/agent/coordinator.go:638-675`, `subagents.go:135` — под-агент может стартовать до того, как readiness-горутины выставят `SystemPrompt`/`Tools` (под-агенты пересобираются при каждой инвалидации runtime). Фикс: `Wait` на errgroup сборки в `runSubAgent`.

**tools / mcp**
- `lsp_rename.go:64`, `lsp_replace_symbol.go:138` — `if sessionID != "" && permissions != nil { requirePermission }`: без session ID запись идёт **без** разрешения. Фикс: `missingSessionID(...)` как Go error, как во всех write-tools.
- `lsp_definition.go:42`, `lsp_call_hierarchy.go:36`, `lsp_rename.go:48`, `references.go:39`, `lsp_symbols.go:31`, `lsp_replace_symbol.go:76` — `workingDir := cmp.Or(params.Path, ".")`, конструкторы не получают `workingDir` (`agent/tool_registry.go:166-175`); `grep.go:136`, `glob.go:63`, `ripgrep.go:70` — `params.Path` используется сырым. В thread-worktree все эти инструменты работают с основным checkout'ом / отвечают «no LSP client handles any file». Фикс: пробрасывать `workingDir` и `SmartJoin`.
- `sennit_logs.go:168-182` — теряет каждую запись лога, пересекающую границу 8 KB чанка (`remainder` берётся с хвоста вместо головы). Проверено: из 200 записей `readLastLines(p,100)` теряет две.
- `grep.go:385-397,274` — include-glob не заякорен: `*.js` матчит `.json`, `*.c` — `.cpp/.css`. Фикс: `^…$` по `filepath.Base` или `doublestar.Match`.
- `fetch.go:125,132`, `fetch_helpers.go:48,55` — `io.LimitReader` режет посреди руны → `utf8.ValidString` false → «not valid UTF-8» на валидной странице > 100 KB. Фикс: `truncateToRuneBoundary` до валидации.
- `write.go:143` — `notifyLSPs(..., params.FilePath)` с сырым относительным путём → LSP не уведомляется. Фикс: `filePath`.
- `lsp_replace_symbol.go:95-171` — собственный pipeline записи мимо `applyFileMutation` (нет freshness/CRLF/истории). Фикс: провести через `applyFileMutation`.

**thread / workspace / app**
- `internal/thread/lifecycle.go:646-651,504-516` — person-steered `send`: статус `running` выставлен, но если `RunAccepted` падает до dispatch-хука — отката нет, `rt.runID` не выставлен → тред навсегда `running`. Плюс гонка `decided`/`failed` в select. Фикс: в `failed` ветке дренировать `decided`, при ошибке — `restIdleAfterPersonTurn`.
- `internal/thread/manager.go:750-770` — `Merge` не проверяет статус: можно смержить тред с бегущим ходом (коммит полуготового worktree), затем `finishMerge` отменяет ход, а cancelled RunComplete дропается. Фикс: отказ для `Status.Active()` как в `Remove`.
- `internal/thread/manager.go:1028-1031,1050-1062` — `c.removed = true` до `WorktreeRemove`/`DeleteBranch`/`store.Delete`; при ошибке строка жива, но тред навсегда «has been removed». Фикс: ставить флаг после успешного удаления.
- `internal/workspace/app_workspace.go:415-435` — `sennit run` может потерять последний текстовый чанк: подписка берётся после старта `Run`, select `done` vs `messageEvents` неупорядочен. Фикс: подписка до старта, дренаж буфера в ветке `done`.
- `internal/workspace/threads.go:266-284` vs `app_workspace.go:829-833` — `SubscribeWith` шлёт `ev.Payload` без `translateEvent` → встроенный UI треда не видит ошибки агента, re-auth, MCP/LSP события.
- `internal/app/services.go:231,265` — `SetThreadManager` (main) vs `SetPermissionsSkip` (config-watcher goroutine) пишут/читают `threadManager` без блокировки.

**config / hooks**
- `internal/config/store.go:656-669` — `SetProviderAPIKey` для нового провайдера публикует `ProviderConfig`, не прошедший `mergeCatalogProviders` (нет `DefaultHeaders`, Vertex/Azure `ExtraParams`, `APIKeyTemplate`), reload не запускается (комментарий в `workspace/custom_provider.go:34` неверен). Фикс: прогнать через `mergeProviderOverride`+`applyProviderVendorSetup` или `autoReload`.
- `internal/config/store.go:636-642,324` — `findKnownProvider` и `ConfigPath` читают `knownProviders`/`workspacePath` без `writeMu`; `reloadFromDisk` их переприсваивает → data race под `-race` при reload во время сохранения ключа.
- `internal/config/watch.go:76-82` — упавший reload повторяется каждые 2 с навсегда (снапшот staleness не обновляется): sennitrc с синтаксической ошибкой = вечный перезапуск bash + discovery HTTP. Фикс: снимать снапшот и при ошибке / backoff.
- `internal/config/load.go:272-276`, `paths.go:26-65` — проектный `sennitrc` выполняется при открытии без trust-гейта; `sennit.json` проекта может ставить `env` (`os.Setenv`), stdio `mcp`/`lsp`, `hooks`. Дизайн-уровень: нужен trust-prompt при первом открытии либо «project config disabled until trusted».

**lsp / oauth / question / session / shell / filetracker**
- `internal/lsp/manager.go:223-241` — неудачный `Initialize`: callback публикует `StateStarting` навсегда. Фикс: `SetServerState(StateError)`.
- `internal/lsp/manager.go:203-222,244-247` — клиент в `StateError` с живым процессом перезаписывается новым → утечка процесса, `KillAll` его не достаёт.
- `internal/lsp/client.go:255-270` — `Restart` меняет `c.client`, `c.ctx`, `diagCountsCache` без синхронизации с читателями (diagnostics tool в фоне, UI каждый кадр); `c.diagnostics` не очищается.
- `internal/oauth/mcp/handler.go:359-366,226-231` — токен без срока: `ExpiresAt=0` → при restore `time.Unix(0,0)` = «истёк в 1970» → на каждом старте refresh/браузер.
- `internal/question/question.go:247-259,298-307` — второй `Ask` (параллельная делегация + родитель) перетирает `pending`; первый никогда не получит ответ. Двойной `Cancel` → `close` закрытого канала → panic.
- `internal/ui/dialog/*` (`provider_form.go:142`, `api_key_input.go:135`, `oauth.go:229-269`, `mcp_auth.go:150-165`; роутинг `ui/model/ui.go:658-663`) — результаты доставляются только переднему диалогу: permission-промпт поверх ProviderForm в `submitting` → форма навсегда заблокирована. Фикс: `Overlay.UpdateDialog` по ID (как FilePicker в `ui.go:653`), не отключать Close в ожидании.
- `internal/ui/dialog/oauth_codex.go:108-112,147-156` — `initiateAuth`/`stopPolling` как Cmd пишут/читают `m.flow` вне Update; Esc во время `Initializing` → listener не закрыт, порт занят, следующий sign-in падает.
- `internal/ui/dialog/question_form.go:214-217` — Esc/`[`/`]` перехватываются формой до ребёнка: для fill-in choice и note-редактора Esc отменяет весь батч; `[`/`]` в fill-in переключают табы.
- `internal/ui/model/shell.go:228-236`, `send.go:889-893` — результат `!cmd`, отброшенный по `loadGen`, не очищает `bangCancel` → `isAgentBusy()` залипает.
- `internal/ui/model/update_status.go:95-130` — таймер очистки статуса без seq: старый таймер стирает новое сообщение (ошибку).
- `internal/session/session.go:305-335`, `sql/sessions.sql:64-74` — `Save` пишет полную строку; три писателя (`turn.go:529`, `subagents.go:144`, `tools/todos.go:149`) с разными/без блокировок → потеря cost или todos. Фикс: узкие SQL-апдейты (`cost = cost + ?`, `SetSessionTodos`).
- `internal/filetracker/coverage.go:254-258` — `Shift` для диапазона, начинающегося выше сжимающейся правки, клампит `End=Start` → строки между считаются непрочитанными, следующая правка отклоняется.
- `internal/shell/dispatch.go:194-201` — shebang-диспетчер убивает только интерпретатор, не process group; при cancel внуки живут, `cmd.Run` висит на pipe. Фикс: переиспользовать `processGroupExecHandler`.
- `internal/shell/dispatch.go:60-63` — `./missing.sh` возвращает сырой `*PathError` → mvdan прерывает весь скрипт вместо exit 127.

### 1.3 Low (P2)

- `agent/runtime_cache.go:97-131` — `ctx.Canceled` одного вызывающего отдаётся всем waiter'ам flight'а.
- `agent/providers.go:712-713` — ошибки `Resolve` API key/base URL игнорируются (`_`).
- `agent/agent.go:1079`, `dispatch.go:1000`, `event.go:12,19` — `recordSessionModel`/`persistCanceledTurn`/телеметрия читают `a.model.Get()` вместо модели, закреплённой за ходом.
- `agent/coordinator.go:500-501` — коалесцированный RunComplete публикуется на живом `ctx` (внутренний defer специально использует `WithoutCancel`).
- `agent/agentic_fetch_tool.go:218-231,260-263` — missing session/message ID отдаётся как text error вопреки AGENTS.md.
- `agent/tools/ls.go:151-156` — `output =` вместо `+=`: depth-заметка затирает truncation-заметку. `ls.go:87-90` — инфраструктурная ошибка как text error.
- `agent/tools/mcp/tools.go:50-57,109-112` — `CallToolResult.IsError` не читается: ошибка MCP-инструмента приходит модели как успех.
- `agent/tools/todos.go:113,149` — Get/Save всей сессии параллельно с `UpdateTitleAndUsage` (см. Save выше).
- `agent/tools/write.go:100-106` — ошибка чтения существующего файла игнорируется → диалог показывает «новый файл», history с пустой базой.
- `agent/tools/mcp/tokenwrite.go:96-108` — запись конфига на диск под реестровым `publishMu`.
- `agent/tools/tools.go:169-178` — `writeFileWithHistory` любой error `GetByPathAndSession` считает «нет записи».
- `thread/manager.go:1298-1360` — `Wait` отдаёт `ErrNotFound` для треда, смерженного и удалённого во время ожидания.
- `thread/lifecycle.go:315,450,661` — `BeginAccepted` не `Close()` при ошибке `RunAccepted` до dispatch → `acceptedRuns` навсегда > 0, session state не GC'ится.
- `thread/tasks.go:283-291` — лимит задач на родителя не считает возобновлённые задачи (`parentSessionID` пуст).
- `thread/lifecycle.go:315,444,661` — `Coordinator()` «may be nil», но разыменовывается без проверки.
- `thread/tasks.go:362-368` — `TaskManager.Cancel` без `beginOp`.
- `config/config.go:648`, `agents.go:80` — `cloneForWrite` делит `Problems` с опубликованным Config; `setupAgents` мутирует in place.
- `config/providers_validate.go:85` — модели из кэша discovery помечаются как `ModelsSourceConfig`.
- `config/import.go:515` vs `toolnames.go:99` — `importKnownTools` без `agentic_fetch`, `ask_parent`.
- `hooks/input.go:207-216` — Claude Code `decision:"block"` → `DecisionNone` → fail-open.
- `shellconfig/{provider,mcp,lsp}.go` — `remove` локален для слоя: не удаляет запись из нижнего слоя (нужен tombstone `disable:true`).
- `shell/jq.go:257-268` — `jq -R` лишняя `""` на trailing newline. `shell/shell.go:251,266` — recover стирает env (`runner.Vars` пуст). `shell/exec_unix.go:69-76` — `Kill(-pid)` после Wait может попасть в переиспользованный pgid.
- `message/message.go:419-436` — при ошибке flush таймер не перевзводится.
- `permission/permission.go:393-397` — ключ persistent-grant для несуществующего файла отличается от существующего.
- `filetracker/service.go:66-76` — read-modify-write без транзакции.
- `commands/commands.go:143-164` — один нечитаемый подкаталог скрывает все команды источника.
- `cmd/logs.go:187,196` — непроверенные type assertion → panic; `logs` читает `--cwd` сырым.
- `cmd/login.go:154-162` — `os.Kill` в `NotifyContext` (no-op), `os.Exit` из горутины мимо `defer cleanup()`. `cmd/run.go:67` — свой `NotifyContext` на `Background()`, App-контекст не отменяется по SIGTERM.
- `ui/model/chat.go:840-861` — `RemoveMessage` не перерегистрирует nested-tool ID. `dialog_actions.go:472-478` — `substituteArgs` зависит от порядка map. `session.go:447-450` — files-секция на строку выше бюджета. `send.go:112`/`common/timer.go` — глобальный turn-таймер общий для главного UI и встроенного треда.
- `ui/dialog/permissions.go:445-470` — контент на 1 колонку шире viewport после toggle split; `:792-816` — `%v` структуры вместо JSON для tools без рендерера. `list/list.go:207-225` — `TotalHeight` stale при росте off-screen item. `mcp_auth.go:203-209` — `browser.OpenURL` синхронно в Update, ошибка не показывается; `:141-144` — Error-состояние тупик для очереди серверов. `doctor.go:235` — порядок MCP-проблем случаен.

---

## 2. Нарушения границ слоёв

Факты из графа импортов (`go list`, только non-test):

```
config  -> db shell clipboard oauth skills shellconfig
hooks   -> config            (ради 4-полевой HookConfig; тянет весь config)
message -> db                (доменная модель + SQLite-сервис в одном пакете)
thread/store_testing.go -> db + testify   (НЕ _test файл!)
proto   -> agent config lsp message oauth session
ui      -> hooks shell lsp oauth git diff fsext commands  (мимо workspace)
workspace -> app             (type-assert до *app.App)
app     -> thread            (комментарии утверждают обратное)
gc      -> thread            (ради Status.Terminal)
```

Конкретно:

- **L1** `internal/thread/store_testing.go` — production-файл доменного пакета импортирует `internal/db` и testify, вопреки `store.go:328-332`; используется только тестами пакета. Это ~150-строчная копия `threadspawn/store.go`. Фикс: переименовать в `store_export_test.go`, удалить копию (S).
- **L2** `internal/hooks/runner.go:12` — `hooks → config`. Фикс: `hooks.Hook` + `type config.HookConfig = hooks.Hook` (S).
- **L3** `internal/message/message.go:13` — модель сообщений и SQLite-сервис в одном пакете; каждый UI-потребитель `message.Message` линкует sqlc+sqlite. Фикс: `internal/message` = модель, `internal/message/store` (или `messagestore`) = сервис (M).
- **L4** `internal/config/doctor.go:11,246-268` (→ `clipboard`), `doctor.go:12,287` + `import.go:15` (→ `skills`), `modelcache.go:16,75` (→ `db`), `config.go:15-17,200-222` + `providers_merge.go:95-114` (→ `oauth/codex`, `oauth/copilot`). `EnvironmentProblems` и `SkillProblems` — в вызывающий слой; `SetupGitHubCopilot`/`SetupCodex`/`applyProviderVendorSetup` — рядом с построением провайдера или в `credentials`; modelcache — отдельный листовой пакет или JSON-файл (M).
- **L5** `internal/config/providers_merge.go:26`, `build.go:60` — `(*Config).configureProviders(ctx, store, …)`: чистый тип данных получает store для записи на диск. Передавать `globalDataPath`, возвращать список stale-ключей (S).
- **L6** `internal/app/services.go:27,84-86,98,111`, `thread_workspace.go:10-28`, `threadspawn/agenttool.go:207-210` — комментарии «app не импортирует thread», «*App удовлетворяет thread.Workspace» ложны; двойные поля `Threads tools.ThreadManager` + `threadManager *thread.Manager` существуют ради устаревшего обоснования. Фикс: одно типизированное поле на менеджер, адаптеры строятся в Attach, комментарии переписать (S).
- **L7** `internal/workspace/workspace.go:443-458` — `Workspace` = **94 метода** (комментарий говорит «~65», `read_only_workspace.go:46` — «~93»); `ui/common/common.go:28` отдаёт его целиком каждому компоненту. Роль-интерфейсы декоративны (16 узких использований vs 18 полных). Фикс: `Common` отдаёт только нужные срезы; удалить неиспользуемые методы (`AgentQueuedPrompts`, `SetCurrentSession`) (M).
- **L8** `internal/workspace/threads.go:180-184,252-255` — type-assert `h.Workspace().(*threadspawn.AppWorkspaceAdapter)` → `aw.App`. Допустимо для композиционного слоя, но вынести в `threadspawn.AppOf(handle)` (S).
- **L9** UI мимо workspace:
  - `ui/model/sidebar.go:12,113-118`, `dialogs.go:11,41,286` — `codex.LatestUsage()` (package-global в `oauth/codex`), `codex.ProviderID`.
  - `ui/model/sidebar.go:13,137-140` — `shell.BackgroundJobCounts`/`MaxBackgroundJobs` (Workspace возвращает shell-тип).
  - `ui/model/session.go:14-16,172-187,298,320-334` — `diff.GenerateDiff` по каждому файлу, `git`, `fsext` — доменная работа в UI.
  - `ui/model/ui.go:533` — `commands.LoadCustomCommands` читает ФС из UI; `ui.go:1143` — `workspace.ResetAgentToolCache()` package-level.
  - `ui/model/lsp.go:13,86-88,133-138,191-216` — switch по `lsp.State*`, `lsp.DiagnosticCounts`.
  - `ui/chat/tools_output.go:10,84-86,157,191` — импорт `internal/hooks` ради декодирования `HookMetadata` из JSON.
  - `ui/model/dialogs.go:179,325,399` — `config.DockerMCPAvailabilityCached()`, `event.StatsViewed()`.
  - `ui/dialog/oauth_codex.go:90-118,165-189`, `oauth_copilot.go:225-262` — диалог владеет OAuth-флоу (токен с диска, refresh, запись конфига, device-polling).
  - `ui/dialog/doctor.go:231-251` — дословная копия `agent/tools/sennit_info.go:178-196`; `EnvironmentProblems()` (`exec.LookPath`) синхронно в конструкторе.
  - `ui/model/chat.go:16`, `ui/chat/agent.go:14` — `proto` импортируется под алиасом `tools` (маскирует зависимость).
- **L10** `internal/session/session.go:172,275` — persistence-сервис шлёт телеметрию (`event.SessionCreated/Deleted`). `internal/db/datadirlock.go` — flock рабочего каталога в DB-пакете. `internal/gc/gc.go:17` — gc → thread ради `Status.Terminal()`, тогда как `stats` дублирует статусы строками (S).
- **L11** `internal/oauth/codex/http.go:516-549`, `internal/discover/discover.go:28-53`, `config.NewProxyHTTPClient` — три копии proxy-HTTP-клиента, каждая с комментарием про import cycle. Фикс: листовой `internal/proxyhttp` (S).
- **L12** `internal/proto/threads.go:19-30` дублирует `thread.Status.Active/Terminal` (`thread/types.go:51-74`) — нужен тест паритета (S).
- **L13** `internal/cmd/session.go:40` — `cmd → ui/chat` ради рендера транскрипта; допустимо, но тянет весь UI в CLI.

---

## 3. SOLID / DRY / KISS / YAGNI

### 3.1 Божественные объекты (SRP)
- `internal/agent/coordinator.go` (1296) — `Coordinator` интерфейс из **25 методов** (`:48-115`): dispatch, cancel/queue, model/runtime, title/summarize, thread/task wiring, delegation inbox, skills refresh. Плюс struct владеет построением провайдеров/моделей, OAuth/AWS re-auth, логированием skills, cost под-агентов. Фикс: ISP-фасеты `Runner`/`Queue`/`Runtime`/`Delegation`; вынести `modelBuilder`, `authRefresher`, `skills_log.go` (M).
- `internal/config/store.go` — `ConfigStore` = **67 методов**: персистенция, copy-on-write, reload, staleness, watcher, MCP-токены, Docker MCP, modelcache, credentials. AGENTS.md «Config is a Service, accessed via `config.Service`» — такого типа нет. Вынести watcher+staleness, MCP-token, docker в свои типы по образцу `credentials.Store` (L).
- `internal/ui/model` — `UI`: ~16.6k строк в 50 файлах, 20 подструктур состояния, `Update` — type-switch на десять `updateX(msg, cmds) ([]tea.Cmd, bool)`; `done bool` всегда `false` у `updateSystem/Status/Prompts/Threads/Shell`. Сделать именованные группы (session panel, sidebar, threads dock, editor, notifications) реальными компонентами со своим `Update/Draw` (L).
- `internal/lsp/client.go` (789) — lifecycle + file-sync + diagnostics cache + request façade в одном типе; блокировка `Restart` нечитаема (M).
- `internal/agent/tools/bash.go` (596) — banned-таблица + mvdan confinement walker (`199-335`) + handler + форматирование; walker → `confinement.go` (S).
- `internal/agent/agent.go` (1140) / `dispatch.go` (1053) — `dispatchDecision` (чистый протокол) в `agent.go`, а обёртки `sessionAgent` в `dispatch.go`; поменять местами (M).
- `internal/config/import.go` (564) + `project_init.go` — фича `sennit import` в пакете config, config не загружает; → `internal/importer` (M). `config.go` (855) — `ProviderConfig`+`SetupGitHubCopilot/SetupCodex/ToProvider/TestConnection` → `provider.go` (S).
- `internal/ui/dialog/question_form.go:472-707` — `Draw` на 120 строк раскладки табов + hit-layer; hover по ручному скану, click по Compositor. `layoutTabs(labels, avail)` чистой функцией (M).
- `internal/ui/dialog/permissions.go:362-368,527-568,620-639` — знание о tools размазано по трём switch/реестрам; один дескриптор `{hasDiff, header(p), content(p)}` (S).

### 3.2 DRY — дубликаты
| Где | Что | Effort |
|---|---|---|
| `agent/coordinator.go:813-855` vs `866-911` | `buildAgentModel` / `buildCustomAgentModel` — одни 35 строк | S |
| `agent/title.go:355-385` vs `usage.go:222-232,316-319` | OpenRouter cost loop + формула стоимости (и `promptTokens` считаются **по-разному**) | S |
| `agent/coordinator.go:1141-1153` vs `:414-428` | преамбула runtime/refresh/rebuild в `Summarize` и `run` | S |
| `agent/dispatch.go:569` | `drainNext` инлайнит `canceledBySeq` | S |
| `agent/tools/edit.go:347-391` vs `393-440` | `deleteContent`/`replaceContent` | S |
| `agent/tools/fetch.go:213-222` vs `fetch_helpers.go:183-192`; `fetch.go:191` vs `search_backend.go:135` | `convertHTMLToMarkdown` ×2, `truncateToRuneBoundary`/`truncateUTF8` | S |
| `agent/tools/web_search.go:56-66` vs `tools.go:222-232`; `tools.go:249-258`; `download.go:140` vs `tools.go:152` | HTTP client, `renderToolDescription`, `ensureParentDir` | S |
| `agent/tools/mcp/{tools,prompts,resources,init}.go`, `registry.go:353-360`, `session.go:321-326`, `state.go:244-251` | блок «lock publishMu → ownsSessionLocked → catalogMu → Del/Set → catalogChanged → updateStateLocked» ×5; `Del+catalogChanged` ×4 при существующем `clearCatalog` | M |
| `thread/store_testing.go:95-197` vs `threadspawn/store.go:68-165` | дословный Store | S |
| `thread/manager.go:337,603,733`, `tasks.go:229,407` | `registerParent(...)` ×5 | S |
| `thread/tasks.go:239-244` vs `lifecycle.go:297-298` | `permission.WithDelegation` ставится дважды | S |
| `config/model_resolve.go:52-58` vs `:87-93`; `providers_shared.go:17` vs `resolve.go:342`; `agents_markdown.go:110-131` vs `watch.go:150-190`; `agents_markdown.go:232` vs `skills/skills.go:185` | match loop, `resolveProviderHeaders`=`resolveMap`, agent dirs/walk, `splitFrontmatter` | S |
| `cmd/login_codex.go:265-285`, `ui/dialog/oauth_codex.go:90-105`, `cmd/models_codex.go:293-310`, `config/credentials/credentials.go:380-395` | 4 варианта «токен Codex CLI с диска → refresh → fallback» | M |
| `cmd/logout.go:83-142`; `cmd/models.go:50-77` vs `80-108`; `models_codex.go:249-276` vs `models.go:294-310`; `cmd/logs.go:76-105` vs `131-162`; `cmd/gc.go:140,181` (не `emitJSON`) | logout×3, фильтры моделей, diff моделей, tail логов | S |
| `message/message.go:529-598`, `:746-822`, `:251-272` | `List*` ×4, `UnmarshalParts` ×8, `Update` debounce-ветка | S |
| `stats/gather.go:173-330`; `stats.go:454/463` | row→struct ×3, `AgentName`/`delegationAgentName` | S |
| `ui/model/update_session.go:125-151` vs `309-330`; `layout.go:495-544` vs `545-585`, `145-160` vs `181-196`; `chat.go:1337,1455,1505`, `877-887` vs `1144-1151`, `1474-1482` vs `1071-1077`, `809-828`; `workspace_cache.go:257-266,350-392` + `lsp.go:29-106` vs `ttl_cache.go`; `dialog_actions.go:77-108` vs `142-181` и паттерн «loading/generation/closure» ×8; `dialogs.go:30-54` vs `266-299`, `148-154` vs `170-176`; `child_session_nav.go:89-96` vs `168-178` | UI model дубликаты | S–M |
| `ui/chat/agent.go:207-340` vs `496-592`; `823-839` vs `848-867` | `AgentToolMessageItem`/`AgenticFetchToolMessageItem` ~130 строк (у fetch нет `Restyle` → палитра не обновляется) | M |
| `ui/chat/{assistant.go:338,messages.go:363,notice.go:60,shell.go:165,tools_item.go:117,user.go:222}` | «split RawRender, prefix, cache by (width,key)» ×6 | S |
| `ui/chat/streaming_markdown.go:196-238` vs `458-487`, `213-221` vs `632-636` | `isSafeBoundaryIncremental` дублирует `isSafeBoundaryAt` | S |
| `ui/dialog/*` — `ListItemStyles{…}` ×8; `heightOffset` ×3; «meaningful answer» ×3; `fillInCursor`/`noteCursor`; `Single/MultiChoice.HandleMouseClick`; три формы ввода (`arguments.go`, `thread_create.go`, `provider_form.go`); header ×2; `completions.scrollbarThumbBounds` vs `common.ScrollbarThumbBounds` | диалоги | S–M |
| `shell/dispatch.go:203-213` vs `exec_unix.go:418-438` | маппинг exit-status | S |
| `shellconfig/register.go:39` vs `flags.go:200` | `appendArr`/`opAppend` | S |
| `oauth/callback/page.go:116-136`, `cmd/login.go`, `logout.go` | «Sennit» строкой при наличии `brand.Name` | S |

### 3.3 KISS / перформанс
- `ui/model/layout.go:120-122` + `sidebar.go:146-213` — `updateSidebarScrollState` перерисовывает весь сайдбар (включая `BackgroundJobCounts()` под мьютексом) **каждый кадр** (M).
- `ui/list/filterable.go:134-138` — fuzzy-фильтр + `SetItems` на каждый кадр; `FilteredItems()` ещё несколько раз за кадр из `commands.go` (S).
- `ui/dialog/models_list.go:176-234` — `fuzzy.Find` по всем items на каждую группу; `SetSelected` сравнивает с неверной границей (S).
- `ui/dialog/permissions.go:224-229,445-462` — контент рендерится дважды за кадр; `dialogHorizontalPadding = 2` хардкодом (S).
- `ui/dialog/filepicker.go:182-186,232-236` — копия всей map превью на каждое нажатие (S).
- `config/load.go:108-145` — `applyWorkspaceConfig` на каждый reload маршалит весь Config в JSON, мержит, анмаршалит, заново `setDefaults` — ради `data_directory` (M).
- `config/load.go:165-239` — `PushPopEnvOverrides` держится через `configureProviders` включая до 3 с discovery HTTP; второй workspace блокируется (M).
- `db/migrations/…initial.sql` — триггер `update_messages_updated_at` дублирует `updated_at` из `UpdateMessage` (две записи на каждый flush стрима); `ListMessagesBySession` без составного индекса `(session_id, created_at)` (S).
- `agent/coordinator.go:431-467` — коалесцирование `onComplete` и `sync.Once` объяснены сценарием «run дважды», которого больше нет; `:1156-1176` type-assert `*sessionAgent` с fallback, который никто не использует; `:763-785` AllowedMCP-фильтр (S).
- `agent/dispatch.go:101-104` vs `:224-230` — комментарий о порядке блокировок ложен (`release` держит `statesMu` и берёт `acceptedMu`) (S).
- `herdr/translate.go:119-149` — `time.Sleep(100ms)` re-subscribe без ctx (S).
- `shell/expand.go:104-107`; `shell/run.go:107-129,178-192` (`RunAndPersist`/`RunAndCapturePTY` для одного вызывающего); `history/file.go:42-82` (интерфейсы ради одного теста) (S).
- `config/load.go:432`, `doctor.go:247` — `testing.Testing()` в production-коде (S).
- `ui/dialog/commands.go:188-194`, `permissions.go:441` — `Draw` мутирует состояние (S).
- `ui/dialog/api_key_input.go:263` — `charmtone.Cherry` хардкодом; `styles/themes.go:109-134` хардкодит diff/ANSI цвета вопреки комментарию `:59-60` (S).
- Bubble Tea: `ui.go:533-544,1055,1146`, `mcp_actions.go:308-375`, `dialog_actions.go:215`, `editor_input.go:260-263,388-392` (`os.Stat` в Update), `session.go:464`, `mcp_auth.go:34,86-91`, `update_integrations.go:141` — Cmd-замыкания разыменовывают `m.com.*` вместо снапшота локалов (S).

### 3.4 YAGNI / устаревшие обоснования
- `internal/event` — все функции no-op, но ~20 call sites (`event.SetNonInteractive`, `SessionListed` …). Удалить или свести к `event.Send(name, kv...)` (S).
- `internal/version/version.go:238-283` — `BuildID` (стат бинаря при каждом старте) и `Commit` никем не читаются (S).
- `internal/lsp/info.go:23-55` — `ClientInfo.MarshalJSON/UnmarshalJSON` для несуществующего wire-формата (S).
- `config/providers_merge.go:135-172` — `applyPendingDiskActions`: строится только `{ScopeGlobal, "providers.anthropic"}`; ветки `fields != nil` и non-global мертвы (S).
- `agent/tools/mcp/tokenwrite.go:149-227` — `tokenWrites`/`waitTokenWrites` бессмысленны, пока commit под `publishMu` (связано с low-багом) (M).
- `agent/tools/mcp/registry.go:223-421`, `authcoordinator.go:87` — package-level `ArmInit/WaitForInit/Close/SubscribeEvents/GetState/BeginAuth`, `defaultRegistry`, `refMu/liveWorkspaces` — только тесты (S).
- `hooks/runner.go:88` — `Run` возвращает всегда-nil error (S).
- `shell/background.go:69,98` — `startHook` тест-сим на production struct; `run.go:47` `TermWidth` нигде не читается (S).
- `ui/dialog/select_dialog.go:363-376`, `sessions.go:293-320` — `FullHelp` чанкует ≤5 элементов по 4 (S).
- `mcpid` (9 строк) — оправдан только как cycle-breaker; можно в `brand`. `ansiext` (25 строк, один вызывающий) — в `diffview`. `env`, `diff`, `stringext`, `filepathext`, `dns`, `latency` — оправданы.

---

## 4. Мёртвый код

`deadcode ./...` (без тестов) даёт 106 символов, плюс находки ревьюеров. Удалять
пакетами; для каждого — `grep -rn` перед удалением.

**Целые пакеты/файлы**
- `internal/diffdetect` — никто не импортирует.
- `internal/proto`: `Message`, `RunComplete`, `AgentEvent`(+`Type`), `PermissionRequest`(+`UnmarshalJSON`, `unmarshalToolParams`, `DelegationRef`), `PermissionNotification`, `ServerNotice`/`Level` (только switch в `ui/model/update_status.go:49,83`, `ui.go:638`), `ConfigProviderKeyRequest`/`APIKeyKind`, `Session`, `QuestionItem`/`QuestionChoice`, `Attachment`, `AgentMessage`, `LSPClientInfo` — плюс мёртвые `case` в `herdr/translate.go:34-50`. (AGENTS.md уже просит удалить.)
- `workspace.ErrServerUnreachable`, `ErrWorkspaceGone`, `ErrStreamClosed` (`workspace.go:58-71`); `Workspace.AgentQueuedPrompts`, `SetCurrentSession`; `app_workspace.go:96-102` `SetCurrentSession*`.

**agent**
- `AcceptedRun.SessionID`, `coordinator.agents` map (пишется, не читается), `Coordinator.Steer`/`sessionAgent.Steer` (только тесты), `dispatcher.enqueueCall`, `errRuntimeChanged`-ветка `coordinator.go:1121-1123`, `getCacheControlOptions`, `SessionAgent.Summarize/GenerateTitle` интерфейсные методы (fallback не достигается), `prompt.WithTimeFunc/WithPlatform`.
- tools: `truncateOutput` (обёртка), `formatAnswer` 2-й параметр / `formatAnswers` всегда-nil error, `cmp.Or(params.URI,…)` в `read_mcp_resource.go:51`, `SmartJoin(workingDir, URI)` как permission-path в `read_mcp_resource.go`/`list_mcp_resources.go`.

**config / hooks / shell / misc**
- `GlobalWorkspaceDir`, `SetTransparentBackground`, `ProjectInitFlag`, `AllToolNames` (только тесты), `WithExpander`, `credentials.WithExchangeToken`, `applyToken` 1-й параметр, `store_testing.go` (non-test файл).
- `hooks.Runner.Hooks`.
- `shell.ShellType*`, `Shell.SetEnv/GetEnv/SetWorkingDir/SetBlockFuncs/Logger`, `BackgroundShellInfo`, `BackgroundShell.Wait`, `BackgroundShellManager.List/Remove`.
- `pubsub.PayloadType*`/`Payload`, `GetSubscriberCount`, `DropCount`, `MustDeliverDropCount`, `SetMustDeliverTimeout`.
- `session.CreateTitleSession`, `IsAgentToolSession`; `message.Service.Flush(id)`, `DeleteSessionMessages`, `WithDebounce`; `history.Service.Get/Delete/DeleteSessionFiles/ListBySession/ListLatestSessionFiles`; `db.ListSessionsSince`, `db.ResetPool`; `gc.Delete/DeleteWith` параметр `q`.
- `fsext.Lookup`, `LookupClosest`, `traverseUp`, `GlobGitignoreAware`, `ShouldExcludeFile`, `WindowsSystemDrive`; `git.AbortMerge`; `log.NewHTTPClient`; `lsp.Client.Kill`; `oauth/mcp` `callbackReceiver.bind`, поле `port`, `Handler.Token`; `cmd/import.go:115-119` `kinds`; `cmd.MaybePrependStdin/ResolveCwd/SchemaID` (экспорт не нужен); `projects.Load`; `csync.CompareAndDelete`; `event.SessionSwitched`.

**ui**
- `threadsDashboard.Tick`, `ApplyThreadsLoaded`; `Chat.InvalidateRenderCaches`, `Chat.Focus`, `Chat.SetMessages` всегда-nil Cmd; `threadBlockGeometry`; `withGOOS`; `DefaultKeyMap`.
- `chat.SendMsg`, `HighlightableMessageItem`, `FocusableMessageItem`, `capTodosForDelegation`, `PanelLiveActivityProvider`/`PanelStatusLine`/`renderPanelStatusLine`/`currentTodoActivity` (+ данные todos, которые никто не рендерит), `NestedToolContainer.AddNestedTool`.
- `list.SelectFirstInView/LastInView`, `InvalidateFrozen`, `BeginSelectionDrag/EndSelectionDrag` (+`freezeSuppressed`), `PrependItems` (List и Filterable).
- `diffview.fileName` (нет сеттера), `DefaultLightStyle`, `ContextLines/LineNumbers/Height/YOffset/InfiniteYScroll`, `isEven/isOdd/ternary`; `ThreadRemoveConfirm.threadID`; `ModelsList.Render`; `MCPAuth` spinner tick в `Prompt`.
- `rendercachetest.AssertPerWidthCacheHit`, `testenv.*` (если это не test-support по замыслу — проверить).

---

## 5. Дрейф документации

- `AGENTS.md` «Config is a Service, accessed via `config.Service`» — типа нет.
- `AGENTS.md` §«Testing with Mock Providers» — `config.UseMockProviders`/`ResetProviders` не существуют.
- `AGENTS.md` «internal/llm/prompt», «internal/tui/components/core» в примерах команд — путей нет.
- `internal/app/services.go:84-111`, `thread_workspace.go:10-28`, `threadspawn/agenttool.go:207-210` — см. L6.
- `internal/workspace/workspace.go` «~65 методов» / `read_only_workspace.go` «~93» — 94.
- `internal/config/credentials/credentials.go:177-181,431`, `store.go:761-765`, `resolve.go:21-33,187-190` («client mode»), `agent/coordinator.go:431-467`, `agent/dispatch.go:101-104`, `thread/manager.go:1366-1394` (godoc прикреплён не к той функции), `ui/chat/streaming_markdown.go:476-520` (два doc-комментария, первый устарел), `workspace/custom_provider.go:34` («triggers a full config reload» — нет), `ui/styles/themes.go:59-60`.

---

## 6. План рефакторинга

Порядок — по риску для пользователя, затем по тому, что разблокирует
следующие шаги. Каждая фаза — отдельный PR (или несколько), зелёный CI
(`-race`) обязателен, golden-файлы обновлять осознанно.

### Фаза 0 — безопасность и потеря данных (P0, ~2 дня)
1. **B1** `safeCommands` — убрать обёртки/`kill*`, тест на `timeout … rm`, `nohup … rm`, `env … rm`, `kill -9 -1` в `permission_coverage_test.go`.
2. `lsp_rename`/`lsp_replace_symbol` без session ID → Go error; провести `lsp_replace_symbol` через `applyFileMutation` (закрывает сразу два P1).
3. **B3+B4** reasoning metadata: `FinishThinking` & co. копируют part; `UnmarshalJSON` разворачивает envelope; снять «quirk»-закрепление в `content_test.go`, добавить round-trip тест для всех полей.
4. **B2** MCP transport unwrap (утечка соединений) + тест: `closeIdleTransport(&channelTransport{inner: streamable})` ≠ nil.
5. **B5+B6** agent: Esc во время summarize; лимит/backoff continuation + drop inbox при `ErrNotFound`. Тест: удалить сессию, доставить completion → ровно одна ошибка, не цикл.
6. `session.Save` → узкие апдейты (`AddSessionUsage`, `SetSessionTodos`, `SetSummaryMessageID`); удалить `Save` или оставить только для title-flow.

*Критерий готовности:* все шесть пунктов покрыты тестами; `task lint`, `go test -race ./...` зелёные; CI зелёный.

### Фаза 1 — UI залипания (P0/P1, ~2 дня)
1. **B7** `mainScreenOwned` на все результаты главного UI (или owner-id). Тест: открыть дашборд при in-flight busy fetch → после возврата `dispatchBusyRefresh` снова шлёт запрос.
2. **B8** edge-scroll только при drag, относительные координаты.
3. **B9** Esc в FreeText; и `question_form.go:214-217` — интерфейс «текстовый редактор сфокусирован?» вместо type-assert `*FreeText`.
4. Роутинг результатов диалогов по ID (`Overlay.UpdateDialog`), Close никогда не отключать в ожидании; `oauth_codex` — flow в сообщении, закрытие из Update.
5. `bangCancel` при dropped result; seq в `ClearStatusMsg`; `RemoveMessage` nested IDs; per-UI turn-таймер.

### Фаза 2 — thread / workspace / lsp / question (P1, ~3 дня)
1. thread: откат статуса в `send` при ошибке до dispatch + дренаж `decided`; `Merge` отказ для активного; `removed` после успешного delete; `Wait` и `ErrNotFound`; `BeginAccepted` Close; nil-guard `Coordinator()`; `TaskManager.Cancel` через `beginOp`; `checkActiveCaps` по строке.
2. workspace: подписка до старта run + дренаж в `done`; `SubscribeWith` через `translateEvent`; lock на `app.threadManager`.
3. lsp: `Restart` чистит `openFiles`/`diagnostics`, RWMutex на `c.client`; `Initialize` failure → `StateError`; `Shutdown` старого клиента перед заменой.
4. question: per-request ID, отказ/очередь на второй `Ask`, idempotent `Cancel`.
5. oauth/mcp: `ExpiresAt==0` → zero time.
6. shell: shebang через `processGroupExecHandler`; missing script → 127; recover не трогает env; jq `-R`.
7. filetracker `Shift`; `InTx` для read-modify-write.
8. tools: `workingDir` во все LSP/grep/glob/ripgrep инструменты (+ тест в thread-worktree); `sennit_logs` chunk boundary; grep include anchor; fetch rune boundary; write `notifyLSPs(filePath)`; `IsError` у MCP; `ls` `+=`.

### Фаза 3 — config (P1, ~2 дня)
1. `SetProviderAPIKey` через merge-пайплайн; RLock в `findKnownProvider`/`ConfigPath`; backoff watcher.
2. **Trust-гейт проектного конфига** — обсудить дизайн отдельно (TECHDEBT или issue); минимум — флаг `options.trust_project_config` и предупреждение при первом запуске в новом проекте.
3. `cloneForWrite` клонирует `Problems`; `ModelsSource` в `providers_validate`; `importKnownTools` из `allToolNames`; hooks `block`/`approve`; `remove` через tombstone.

### Фаза 4 — границы слоёв (M–L, ~1 неделя)
1. L1 `store_testing.go` → `_test`; удалить копию Store.
2. L2 `hooks.Hook` + alias в config.
3. L6 app/thread поля и комментарии; L8 `threadspawn.AppOf`.
4. L4/L5: `EnvironmentProblems`/`SkillProblems` из config; `configureProviders` без store; vendor setup из `config.go`; `internal/proxyhttp` (L11); modelcache без db.
5. L3 `message` → модель + store (самая большая правка, отдельный PR; после неё `ui` перестаёт линковать sqlite через message).
6. L9 UI: `Workspace.CodexPlanUsage()`, DTO для `BackgroundJobCounts`, `SessionFilesWithStats()` в workspace, `CollectProblems()` в workspace (убирает копию doctor/sennit_info), `hooks.HookMetadata` → proto DTO, LSP state enum → `proto`/workspace, `commands.LoadCustomCommands` и `ResetAgentToolCache` за Workspace, переименовать алиас `tools` → `proto`.
7. L7 `Workspace` — убрать мёртвые методы, `Common` с ролевыми срезами; синхронизировать комментарии о числе методов; тест паритета `proto.Thread*Status` ↔ `thread.Status` (L12).
8. L10: телеметрия из `session`, `datadirlock` из `db`, `gc` без `thread`.
9. Добавить depguard-правила на новые границы (`hooks` ↛ `config`, `message` ↛ `db` после разделения, `config` ↛ `clipboard|skills`).

### Фаза 5 — DRY/KISS сводка (S-пункты, можно параллельно, ~3 дня)
По таблице 3.2 сверху вниз; отдельно — перформанс-пункты 3.3 (сайдбар каждый кадр, filterable на каждый кадр, `applyWorkspaceConfig`, триггер/индекс БД).

### Фаза 6 — SRP (L, по одному PR)
1. `Coordinator` на фасеты + `modelBuilder`/`authRefresher`/`skills_log.go`.
2. `ConfigStore`: watcher+staleness, MCP-token, docker в свои типы.
3. `ui/model.UI`: компоненты session panel / sidebar / threads dock / editor / notifications; убрать константный `done`.
4. `lsp.Client` разделить; `bash.go` confinement walker; `import.go` → `internal/importer`.

### Фаза 7 — мёртвый код и документация (S, ~1 день)
1. Раздел 4 целиком; снова `deadcode ./...` → 0 non-test символов (или задокументированные исключения).
2. Раздел 5: AGENTS.md (`config.Service`, mock providers, пути), комментарии в app/thread/workspace/credentials/coordinator/dispatch.
3. Закрыть этот документ: открытые хвосты → `TECHDEBT.md`, файл удалить (как делалось с предыдущими планами).

### Системные меры (чтобы класс багов не вернулся)
- Тест-таблица «каждая команда из `safeCommands` не может быть обёрткой другой команды» + «каждый writing tool без session ID возвращает Go error».
- Тест «каждый `*Msg`-результат, диспатчимый из `ui/model` с `inFlight`-флагом, реализует `mainScreenMsg`» (рефлексией по списку типов).
- Тест round-trip для каждого `ContentPart`: marshal → unmarshal → `reflect.DeepEqual`.
- Правило ревью (уже в TECHDEBT): Cmd-замыкания не захватывают `m`; расширить grep-lint на `m.com.` внутри `func() tea.Msg`.
