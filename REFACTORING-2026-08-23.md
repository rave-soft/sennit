# План рефакторинга — ревью 2026-08-23

Второе полнопроектное ревью, поверх плана 2026-08-20 (`REFACTORING.md`,
все 62 пункта закрыты). Базлайн: ~997 Go-файлов; `go vet`, `golangci-lint`
и `go build ./...` чистые. Ревью шло пятью зонами: `internal/ui`,
`internal/agent` (+tools, permission, event), `internal/config`
(+shellconfig, discover, oauth, home, csync), app/CLI/data-слой
(app, cmd, thread, workspace, session, message, db, proto, main.go)
и сквозные утилиты (shell, fsext, lsp, skills, hooks, git, lock, log,
herdr и пр.). Все пункты — новые, сверены с `REFACTORING.md` и
`TECHDEBT.md`; дубликаты закрытых пунктов исключены.

Порядок фаз: сначала дыры в permission-модели (это безопасность, не
рефакторинг), затем корректность high, затем low, затем границы,
дедупликация и чистка. Каждый багфикс — с регрессионным тестом.

Ключ трудозатрат: S = часы, M = день-два, L = несколько дней.

---

## Фаза 0 — Безопасность: обходы permission-модели — DONE (2026-08-23)

Единая тема: команда/инструмент выполняет запись мимо промпта или мимо
confinement. Закрыто одним заходом; регрессионные тесты:
`TestContainsCommandChaining` (новые случаи), `confinement_test.go`
(download, bash working_dir), `TestIsWhitelistedDockerTool_OnlyForTheManagedGateway`,
`TestRedactBody`, `TestHTTPRoundTripLogger_DoesNotBufferStreamingResponse`.
Остаточный долг (bash может писать наружу изнутри команды) записан в
TECHDEBT.md.

- [x] **`internal/agent/tools/safe.go:61` + `bash.go:217` — обход промпта
  через `>`, `<`, перевод строки, одиночный `&`, `<(`.**
  `chainingMetacharacters` содержит только `;`, `|`, `&&`, `$(`, `` ` ``.
  Команда с «безопасным» префиксом выполняется без разрешения:
  `echo payload > ~/.bashrc`, `"echo hi\nrm -rf dir"`, `echo x & rm -rf dir`.
  Проверено по коду вручную. Добавить недостающие метасимволы (или парсить
  синтаксическим деревом mvdan/sh, оно уже в зависимостях). **S**
- [x] **Confinement применяется только к write/edit/multiedit.** `download`
  пишет по абсолютному пути, `lsp_rename`/`lsp_replace_symbol` мутируют
  файлы, `bash` пишет куда угодно — никто из них не вызывает
  `confinementRefusal`. Тред с унаследованным skip-permissions может менять
  основной checkout — ровно сценарий, ради которого confinement вводили. **M**
- [x] **`internal/agent/tools/tools.go:290` — `confinementRefusal`
  fail-open:** при ошибке резолва пути возвращает «не отказывать».
  Ошибка границы безопасности должна закрывать. **S**
- [x] **`internal/agent/tools/mcp-tools.go:13,111` — permission-байпас по
  имени сервера:** whitelist `mcp_docker_*` освобождает от разрешения в т.ч.
  мутирующие `mcp-add`/`mcp-config-set`, и срабатывает для любого
  пользовательского MCP-сервера с именем `docker`. Проверять реальный тип
  сервера, а не имя. **S**
- [x] **`tools/download.go:83`, `tools/lsp_rename.go:65`,
  `tools/lsp_replace_symbol.go:135` — permission-запрос без `ToolCallID`:**
  `hookApproved(ctx, "")` всегда false → allow-решение PreToolUse-хука
  молча игнорируется; UI-индикатор не привязывается к вызову. **S**
- [x] **`tools/bash.go:213` — относительный `working_dir` резолвится от cwd
  процесса, а не workspace** (нет `filepathext.SmartJoin`, в отличие от
  файловых инструментов): в thread-worktree команда выполняется в основном
  репозитории. Проверено по коду вручную. **S**
- [x] **`internal/permission/permission.go:363` — `os.Stat(opts.Path)`
  относительно cwd процесса**, ключ `sessionPermissions` для относительных
  путей неканоничен: persistent-grant может не совпасть или совпасть с
  чужим. **S**
- [x] **`internal/config/provider.go:116,148` — Google API-ключ утекает в
  текст ошибки:** ключ в query, `*url.Error.Error()` включает полный URL →
  ключ в UI/логах. Перенести в заголовок `x-goog-api-key` или редактировать
  URL. **S**
- [x] **`internal/log/http.go:31,65` — debug-логгер HTTP логирует тела без
  redaction** (OAuth-токены из token-exchange попадают в лог, при
  редактируемых заголовках) и полностью буферизует тело до возврата —
  в `--debug` SSE-стриминг «схлопывается» в один кусок. **M**

## Фаза 1 — Корректность, bug-high

**СДЕЛАНО (2026-08-23).** Все пункты закрыты, на каждый есть регрессионный
тест, падающий на старом коде. Полный прогон `go test ./...`, `-race` по
затронутым пакетам и `golangci-lint run` — зелёные. Тесты:
`TestReloadFromDisk_ConcurrentOverrideSurvives`,
`TestReloadFromDisk_ConcurrentWriteDuringReloadIsNotAbsorbed`,
`TestConfigStore_RuntimeOverrides_ValueIsSnapshot`,
`TestSetMCPToken_ProjectScopedServerPersistsToWorkspace`,
`TestSetMCPToken_GloballyDeclaredServerStillWritesGlobal`,
`TestApplyWorkspaceConfig_TokenOnlyOverlayMergesWithoutBogusServer`,
`TestRunTurn_CancelledCtxStillPublishesTerminalEvent`,
`TestTaskManager_CreateFailsWithRealIDWhenSetSessionErrors`,
`TestClient_Restart_ReopensPreviouslyOpenFiles`,
`TestClient_HandlesFile_RejectsURIAcceptsPath`,
`TestStartServer_ConcurrentStartsCreateOneClient`,
`TestHandleFileEvent_SessionClearedBeforeCmdRuns`,
`TestOAuthCodexStartPollingSetsCancelFuncBeforeReturning`,
`TestHandleThreadAttachedTearsDownPreviousAttachment`,
`TestHandleThreadAttachedStaleAfterLeavingDashboard`,
`TestOAuthEscStopsPollingBeforeClosing`,
`TestAgenticFetchToolMessageItem_FinishedInputWithoutResultShowsCurrentActivity`,
`TestFenceState_MatchesCommonMark`,
`TestList_AtBottom_TallFirstVisibleItemScrolledToBottom`,
`TestRenderDiff_HorizontalScrollShiftsContent`,
`TestForegroundGrad_BoldSetsSGR`,
`TestQuestionFormBracketKeyTypedIntoFreeText`,
`TestQuestionFormBracketKeySwitchesTabsOutsideFreeText`.

Решения, отличающиеся от исходного плана:
- `Overrides()` теперь возвращает значение, а не указатель; мутации — через
  `SetSkipPermissionRequests`/`SetEnabledChannels` под `writeMu`. Раздача
  указателя сама по себе была гонкой (`app/bootstrap.go` писал поля без
  блокировки).
- Токен MCP для проектного сервера пишется в workspace-конфиг
  (`.sennit/sennit.json`) как overlay `mcp.<name>.oauth_token`; каталог
  `.sennit` Sennit сам закрывает своим `.gitignore`, так что в git токен не
  попадёт. Все пропуски записи теперь логируются `slog.Warn`.
- Горизонтальный скролл диффа: `WrapLines(p.diffXOffset == 0)` — перенос и
  скролл взаимоисключающи; в `diffview` это зафиксировано в док-комментариях
  `XOffset`/`WrapLines`.
- Парность code-fence заменена на конечный автомат `fenceState` с
  CommonMark-правилами закрытия (тот же символ, не короче, без info-string).
- Поздний attach треда: если пользователь ушёл с дашборда, воркспейс
  освобождается, а не переключает экран. Отличить устаревший ответ от
  свежего запроса к *другому* треду по текущему состоянию нельзя — это
  отмечено в комментарии.


### config / thread / agent

- [x] **`internal/config/reload.go:72,155` — reload молча откатывает
  конкурентные мутации overrides:** снапшот в начале, долгий discovery без
  writeMu, запись снапшота в конце. Выбор модели / `SkipPermissionRequests`
  во время reload молча откатываются, staleness-снапшот «поглощает» запись.
  Кредензалы защищены циклом по `credentialVersion` — распространить тот же
  приём на остальные поля. **M**
- [x] **`internal/config/mcp_token.go:79` — OAuth-токен MCP-сервера из
  проектного конфига никогда не сохраняется:** `SetMCPToken` пишет только в
  глобальный data-файл и требует существования ключа там → project-scoped
  сервер получает `errAtomicWriteNoop` без лога; браузерная авторизация при
  каждом старте. **M**
- [x] **`internal/agent/agent.go:836` — терминальный `RunComplete` теряется
  при отменённом ctx:** publish идёт с внешним ctx рана;
  `PublishMustDeliver` при отменённом ctx не доставляет никому. Флаш строкой
  выше уже использует `context.WithoutCancel` — публикация должна тоже
  (образец: `publishCanceledQueueDrops`, dispatch.go:886). Клиент, ждущий по
  RunID, висит до таймаута. **S**
- [x] **`internal/thread/tasks.go:209` — `st` перезаписывается нулевым
  значением при ошибке `SetSession`**, и `failCreate` помечает failed пустой
  ID: задача навсегда pending, слот в `checkActiveCaps` занят до рестарта.
  Близнец уже починенного бага в `manager.go:308` (`newSt`). **S**

### LSP

- [x] **`internal/lsp/client.go:296` — после `Restart` открытые файлы не
  переоткрываются:** в `OpenFile` передаётся URI вместо пути,
  `HandlesFile` падает молча. Диагностика после `lsp_restart` мертва.
  Правильный образец — `RefreshOpenFiles` (client.go:473). **S**
- [x] **`internal/lsp/manager.go:170` — гонка `startServer`:** проверки и
  `Set` не атомарны, два конкурентных `Start` запускают два процесса,
  проигравший утекает навсегда. Использовать `csync.Map.GetOrSet` или
  per-name mutex. **M**

### UI — гонки cmd-замыканий (системный класс, см. «Системное» ниже)

- [x] `model/editor_input.go:171-201` — `insertFileCompletion` мутирует
  `m.sess.fileReads` и читает `m.sess.current.ID` в cmd-горутине; race со
  `send.go:84`/`newSession`. **S**
- [x] `model/session.go:343` — `handleFileEvent`: nil-check на
  Update-горутине, разыменование в cmd-горутине; событие file + `ctrl+n` →
  падение TUI. **S**
- [x] `model/dialog_actions.go:423` — `ActionFilePickerSelected` зовёт
  `m.dialog.CloseDialog` из cmd-горутины — гонка по стеку диалогов. **S**
- [x] `model/ui.go:1147` — `copyChatHighlight`: `m.chat.ClearMouse()` как
  шаг `tea.Sequence` пишет mouse-поля Chat из cmd-горутины. **S**
- [x] `dialog/oauth_codex.go:128` — `m.cancelFunc = cancel` внутри
  полл-замыкания; та же гонка уже исправлена в `oauth_copilot.go:57`. **S**

### UI — утечки и логика

- [x] **`model/root.go:484-517` — `handleThreadAttached` перезаписывает
  `r.thread` без `stop()`/`detach()` предыдущей привязки:** двойной Enter по
  треду → утечка pump-горутины и attached-workspace навсегда; поздний attach
  насильно переключает экран. **M**
- [x] **`dialog/oauth.go:197` — esc из Display/Initializing не вызывает
  `stopPolling`:** отменённый вход в Copilot поллит GitHub до `expiresIn`;
  у Codex занят loopback-порт, повторный вход падает. **M**
- [x] **`chat/agent.go:665` — agentic_fetch считается завершённым сразу после
  стрима аргументов** (`IsPending` вместо `!HasResult && !IsCanceled`, как в
  соседней agent-ветке): многоминутный вызов рендерится свёрнутым без
  спиннера. **S**
- [x] **`chat/streaming_markdown.go:649` — парность code-fence как голый
  счётчик чётности** без учёта символа/длины/info-string: markdown-в-markdown
  ломает stablePrefix, хвост сообщения навсегда рендерится прозой. **M**
- [x] **`list/list.go:168` — `AtBottom` не вычитает `offsetLine`:**
  на высоком первом видимом item follow-режим чата не включается,
  автоскролл умирает. **S**
- [x] **`diffview/diffview.go:592-701` — wrapped-рендер игнорирует
  `xOffset`**, а permission-диалог строит diff именно с
  `.XOffset().WrapLines(true)` — горизонтальный скролл диффа мёртв. **M**
- [x] **`styles/grad.go:23,37` — результат `style.Bold(true)` отброшен**
  (lipgloss иммутабелен): wordmark никогда не жирный. **S**
- [x] **`dialog/question_form.go:131-139` — `[`/`]` перехватываются до
  маршрутизации в активный компонент:** набрать скобку в
  FreeText-поле невозможно. **S**

## Фаза 2 — Корректность, bug-low (по пакетам)

### internal/agent + tools

- [ ] `providers.go:741` — `ExtraBody["tool_stream"]=true` для ZAI мутирует
  map, разделяемую с хранимым Config (гонка + протечка флага в будущие
  поколения конфига); клонировать, как соседний `headers`. **S**
- [ ] `usage.go:114,178` — cleanup-пути summarize используют отменяемый ctx:
  при отмене остаётся «вечное» пустое summary-сообщение; перевести на
  `context.WithoutCancel` + таймаут, как в `handleStreamError`. **S**
- [ ] `agent.go:669` — окно idle между `clearActiveIfMatch` и `summarize`:
  continuation успевает занять сессию → успешный ход завершается
  `ErrSessionBusy`. **M**
- [ ] `turn.go:710` — «model is not enabled» жёстко классифицируется как
  Copilot-квота с ссылкой на настройки Copilot для любого провайдера;
  гейтить по `t.model.ModelCfg.Provider`. **S**
- [ ] `tools/tools.go:143` — `resolveWithinWorkdir` считает `..foo` внешним
  путём (`HasPrefix(relPath, "..")`); тот же дефект чинили в
  `fsext.HasPrefix` — использовать его. **S**
- [ ] `subagents.go:120` — lost update стоимости родительской сессии:
  read-modify-write без сериализации при параллельных суб-агентах. **S/M**
- [ ] `tools/fetch.go:176` — обрезка `content[:MaxFetchSize]` по байтам
  рубит UTF-8-руну после валидации всего контента. **S**
- [ ] `providers.go:216` — google-ветка не гейтит thinking по `CanReason`:
  не-reasoning google-модели получают пустой `thinking_level` — кандидат на
  400. **S**

### internal/config / discover / credentials / home / csync

- [ ] `providers_merge.go:251` + `store.go:311` — удаление устаревшего
  Anthropic-OAuth целится в data-файл, а провайдер живёт в пользовательском
  конфиге: вечный no-op + безусловная перезапись файла → reload-пинг-понг
  между двумя инстансами с периодом ~2с. **S**
- [ ] `store.go:274` — `Overrides()` отдаёт незащищённый указатель;
  читатели/писатели работают мимо writeMu — data race с
  `pinPreferredModelLocked` и reload. **S/M**
- [ ] `store.go:526` — `RemoveConfigField` не обновляет staleness-снапшот
  атомарно с записью (в отличие от `SetConfigFields`): watcher принимает
  свою запись за внешнюю → лишний полный reload. **S**
- [ ] `docker_mcp.go:158` — `DisableDockerMCP` переписывает весь смёрженный
  `mcp`-блок в глобальный файл: project-scoped серверы (с oauth_token)
  копируются во все проекты. Использовать
  `RemoveConfigField(ScopeGlobal, "mcp.docker")`. **S**
- [ ] `credentials/credentials.go:228` — токен публикуется в память до
  persist на диск: упавшая запись оставляет на диске потраченный
  refresh-токен → reuse detection → отзыв token family. Логировать как
  критическое + ретраить persist. **S/M**
- [ ] `discover/lmstudio.go:102` — `SupportsImages` перетирается безусловно,
  вопреки контракту Enricher «пользовательские переопределения
  побеждают». **S**
- [ ] `providers_validate.go:73` + `discover/discover.go:190` — рукописные
  модели утекают в discovery-кэш и воскресают после удаления из конфига;
  `ModelsSource` дрейфует Config→Cache. **M**
- [ ] `home/home.go:35` — `Short` матчит префикс без границы разделителя:
  `/home/bobby` у пользователя `bob` показывается как `~by`. **S**
- [ ] `discover/discover.go:85` — `doRequest` глотает ошибки резолвера
  base_url/api_key: упавшая `$(cmd)`-подстановка маскируется как 401. **S**
- [ ] `csync/maps.go:33` — `Map.Reset` алиасит карту вызывающего (в
  `NewMap` этот класс чинили в фазе 0); клонировать. **S**

### app / cmd / thread / workspace / message

- [ ] `thread/tasks.go:229` — ошибка `setStatus(StatusRunning)` возвращается
  без `failCreate`, в отличие от всех соседних путей: висящий mid-create
  ряд. **S**
- [ ] `thread/tasks.go:416` — реактивация задачи через `task_send`
  регистрирует `DelegationParent` с `Depth: 0` вместо сохранённой глубины —
  ослабляет `maxTaskCascadeDepth`. **S**
- [ ] `thread/lifecycle.go:808` — ошибка `store.Get` в `handleRunComplete`
  после снятия runtime → молчаливый return, ряд «вечно Running» до
  рестарта; нужен retry/лог. **S**
- [ ] `cmd/run.go:66` — подписка на `os.Kill` вместо `syscall.SIGTERM`:
  `kill <pid>` не даёт graceful cancel. **S**
- [ ] `workspace/app_workspace.go:433` — чтение из `messageEvents` без
  проверки `ok`: закрытый брокером канал → busy-spin 100% CPU до
  `done`. **S**
- [ ] `app/app.go:145` — ошибка `InitCoderAgent` в `New` не откатывает уже
  запущенные MCP/watchers/herdr-bridge; в `LocalSpawner.Spawn` каждый
  неудачный spawn копит подпроцессы. **M**
- [ ] `app/lsp_events.go:95` — неатомарный get-modify-set на `csync.Map` в
  `updateLSPDiagnostics` конкурентно с `updateLSPState` — lost update
  счётчика диагностик. **S**
- [ ] `cmd/threads.go:140` — обрезка goal по байтам (`goal[:59]`) режет
  кириллицу посреди руны; рядом `cmdutil.go:139` уже использует
  `ansi.Truncate`. **S**
- [ ] `thread/manager.go:218` — гонка check-then-act по имени треда: сырое
  UNIQUE-constraint сообщение вместо «name is already in use»; маппить
  ошибку. **S**

### shell / hooks / herdr / git / richpaste / lsp

- [ ] `shell/run.go` + `stream.go:49` — вывод фоновых (`cmd &`) джобов
  пишется в голые `bytes.Buffer` после возврата `Run` (interp не ждёт
  bgProcs) — data race; `BackgroundShell` уже использует мьютексный
  `syncBuffer`, выровнять. **M**
- [ ] `hooks/input.go:54` — `BuildEnv` не вычищает `HERDR_*`, которые
  bash-tool снимает намеренно: хук со вложенным sennit аттачится к
  родительской pane. **S**
- [ ] `herdr/client.go:175` — `releaseAgent` уходит в сокет мимо очереди
  writeLoop: буферизованный отчёт `working` может уйти после release. **S**
- [ ] `lsp/client.go:232` — offset encoding для `workspace/applyEdit`
  захватывается до `initialize`: не-дефолтная кодировка (clangd) даёт правки
  по неверным смещениям. **S**
- [ ] `lsp/client.go:450,606` — `fileInfo.Version++` на разделяемом
  указателе без синхронизации. **S**
- [ ] `lsp/manager.go:110` — сравнение путей без `fsext.Canonical`: при
  symlinked cwd / Windows-спеллинге LSP молча не стартует. **S**
- [ ] `lsp/handlers.go:15` — `HandleWorkspaceConfiguration` игнорирует
  `params.items`: сервер с 2+ секциями получает рассинхронизированный
  массив. **S**
- [ ] `git/git.go:187` — `UncommittedFiles` падает при unborn HEAD (репо без
  коммитов). **S**
- [ ] `richpaste/resolve.go:95,158` — `MaxBytes` не применяется к
  data:-URI: base64-картинка обходит `MaxAttachmentSize`. **S**

### internal/ui (bug-low, сгруппировано)

- [ ] Ещё четыре cmd-замыкания с чтением состояния модели:
  `model/history.go:20`, `model/mouse.go:262`,
  `model/editor_input.go:330,444,543`, `dialog/api_key_input.go:295`
  (method value). Закрывать вместе с фазой 1/UI. **S**
- [ ] `model/chat.go:467` — `SetMessages` не сбрасывает mouse-состояние:
  отложенный `DelayedClickMsg` кликает случайный item новой сессии. **S**
- [ ] `model/update_session.go:156` — `sessionFilesUpdatesMsg` без
  sessionID-guard'а: sidebar показывает файлы чужой сессии. **S**
- [ ] `model/lsp.go:228`, `model/mcp.go:104` — off-by-one в «and N more»
  (в `skills.go:150` посчитано верно; вынести общий helper). **S**
- [ ] `model/layout.go:707,413,186` — три разных ширины одного редактора
  (w-31/-32/-34): обрезка QuestionForm. **M**
- [ ] `model/session.go:380` + `sidebar.go:1282` — `filesInfo` и
  `fileChangeCount` считают Uncommitted по-разному. **S**
- [ ] `model/thread_completion.go:2254` — `threadLastStatus` никогда не
  чистится: неограниченный рост map. **S**
- [ ] `dialog/question_form.go:222` — esc съедается формой до компонента:
  отмена батча с потерей набранного вместо blur. **S**
- [ ] `dialog/question_choice_base.go:478` — `strings.Repeat(" ", -1)` →
  panic при нулевой ширине области. **S**
- [ ] `dialog/question_choice_base.go:412` — высота не учитывает wrap
  длинных label. **S/M**
- [ ] `dialog/question_confirm.go:279` — stale-compositor кнопок после
  прокрутки: клик по старым координатам жмёт невидимую кнопку. **S**
- [ ] `dialog/select_dialog.go:144` + `commands.go:112` — асинхронный msg
  или ресайз молча стирает набранный фильтр палитры. **S**
- [ ] `chat/assistant.go:481` — хэш thinking по 64-байтовому сэмплу:
  stale-рендер при перезаписи той же длины. **S**
- [ ] `chat/replace_symbol.go:43` — приоритет `&&`/`||`: diff строится по
  битым данным при ошибке Unmarshal. **S**
- [ ] `chat/question.go:59` — байтовый срез заголовка: `�` на
  кириллице. **S**
- [ ] `chat/shell.go:332` — «…»-индикатор дописывается после
  `ansi.Truncate`: строка шире области. **S**
- [ ] `chat/shell.go:184 vs 272` — `HoverableAt` и `RawRender` считают
  строки по-разному: кликабельный, но неразворачиваемый item. **S**
- [ ] `chat/streaming_markdown.go:404` — CRLF отключает префикс-кэш:
  полный ре-рендер каждый flush. **S**
- [ ] `chat/tools_copy.go:56,177` — heredoc уплощается `\n→' '` при
  копировании; итерация map без сортировки — недетерминированная
  копия. **S**
- [ ] `list/list.go:691` — `SetItems([])` залипает `offsetIdx=-1`: пустой
  рендер непустого списка. **S**
- [ ] `list/list.go:522` — скролл через межэлементный gap прыгает. **S**
- [ ] `completions/completions.go:606` — тумб скроллбара инвертирован при
  `SetReverse(true)`. **S**
- [ ] `completions/item.go:52` и `list/item.go:86` — оба per-width кэша
  не работают (nil-guard пишет в локальную переменную / `invalidate`
  обнуляет навсегда). **S**
- [ ] `common/markdown.go:69,89,104` — ошибка `NewTermRenderer` кэширует
  nil навсегда (nil-deref на этой ширине); `rendererLocks` не чистится на
  смене темы. **S**
- [ ] `common/ansi16.go:199` — `simulateCarriageReturns` теряет SGR и хвост
  прежней строки. **M**
- [ ] `common/button.go:43`, `common/elements.go:146,240,275` — байтовые
  индексы/ширины: подчёркивание и обрезка едут на не-ASCII. **S**
- [ ] `anim/anim.go:85,425` — `settingsHash` без `NoScramble`; неатомарный
  wrap шага (латентный index out of range). **S**
- [ ] `diffview/diffview.go:463,125` — «…» на пробельной SGR-строке;
  `ContextLines`/`TabWidth` игнорируются после первого `String()`. **S**
- [ ] `attachments/attachments.go:177,62` — off-by-one чипов («1 more…» при
  нуле скрытых); delete-режим не принимает двузначные номера. **S**
- [ ] `image/image.go:109` — `paint` растягивает на канвас, убивая
  пропорции; аспект клетки не учтён в blocks-режиме. **M**

## Фаза 3 — Границы контекстов / design

- [ ] **UI → config напрямую:** `common/common.go:12` (`Common.Config()`),
  `chat/messages.go:375`, `chat/agent.go:113`, `chat/tools_core.go:267` —
  view-код опрашивает `internal/config` и таскает `*config.Config` в
  сигнатурах рендереров; инжектировать резолв при конструировании (есть
  `workspace.ConfigAccessor`). `chat/docker_mcp.go:11` — импорт config ради
  константы `DockerMCPName` (место — в `proto`). **M**
- [ ] **UI → oauth/codex напрямую:** `model/sidebar.go:12,113` —
  `codex.LatestUsage()` на render-пути мимо workspace-фасада. **S**
- [ ] **cmd → ui/chat + agent/tools:** `cmd/session.go:22,27` —
  `session show/export` тянет TUI-рендерер и тип из agent/tools мимо app;
  вынести форматирование транскрипта в нейтральный пакет или зафиксировать
  исключение комментарием. **M**
- [ ] **shell → message:** `shell/persist_message.go` — доменная логика
  bang-режима (плюс матчинг текста ошибки FK) в утилитарном пакете;
  перенести к вызывающему (workspace). **S**
- [ ] **herdr:** шапка `client.go` обещает «decoupled from proto and domain
  layers», а `translate.go` импортирует proto, message, permission, pubsub,
  notify. Перенести Translate/BridgeLocal в app или переписать
  контракт. **S/M**
- [ ] `config/config.go:306` — jsonschema-enum тем зашивает в config знание
  палитр UI (комментарий рядом утверждает обратное); генерировать из
  реестра, как discover-типы. **S**
- [ ] `agent/compat.go:25` — безусловное «your todo list is currently
  empty» даже при непустом `session.Todos` — ложное утверждение модели о
  своём состоянии. **S**
- [ ] `app/threadspawn/attach.go:147` — production-путь строит TaskManager
  через `NewTaskManagerForTest`; переименовать конструктор. **S**
- [ ] `shell/expand.go:90` — комментарий обещает block funcs внутри
  `$(...)`, но они не подключены: команды из config-значений идут без
  deny-list. Либо подключить, либо задокументировать доверие явно. **S**
- [ ] `agent/agent.go:500` — дроп continuation репортится как
  `SteerEnqueued`: контракт enum нарушен. **S**
- [ ] IO/запись в Update-петле UI: `model/editor_input.go:73`
  (`os.CreateTemp` в обработчике клавиши), `dialog/oauth.go:135`
  (чтение диска в конструкторе), `model/dialogs.go:454`
  (`QuestionAnswer` — channel send из `HandleKey`). **S–M**

## Фаза 4 — Дедупликация (DRY)

- [ ] `thread/manager.go:326,595,732` + `tasks.go:220,416` — конструирование
  `DelegationParent` повторено 5 раз с расхождениями (в одном из них родился
  баг Depth); один хелпер `registerParent`. **S**
- [ ] `discover/{lmstudio,llamacpp,litellm,omlx}.go` — четыре enricher'а на
  одном копипаст-скелете (~40 строк × 4); generic `fetchJSON[T]` + чистые
  apply-функции; заодно убрать мёртвый `error` из `EnrichModels`. **M**
- [ ] `tools/bash.go:258-300 vs 330-372` — два почти идентичных блока
  завершения (~45 строк), расходятся молча. **S**
- [ ] `fsext/lookup.go` — четыре функции = две пары одинаковых замыканий;
  `fsext` — три независимых ignore-списка с дрейфом, мапа `SkipHidden`
  аллоцируется на каждый вызов из glob. **S**
- [ ] UI-дубли: markdown-рендер вопроса ×6 в dialog; «line-buffer → clamp →
  blit → scrollbar» ×3 (общий `lineViewport` заодно чинит stale-compositor);
  OAuth-диалоги oauth/mcp_auth/aws_sso почти построчно совпадают;
  `tools_copy_file.go` — задвоенный switch ext→lang; «truncate + and N
  more» ×3 (lsp/mcp/skills); два elapsed-форматтера
  (`common/timer.go` vs `presentation`). **M–L суммарно**
- [ ] Двойной `cappedMessageWidth` во всех tool-рендерерах
  (`chat/tools_item.go:59` + каждый `RenderTool`): тела в
  `min(width-4,118)`, hook-индикатор шире тела, `hasCappedWidth` не
  работает, отрицательные ширины на узком терминале без guard'ов. **M**

## Фаза 5 — Чистка / мёртвый код / KISS

- [ ] `chat/agent.go:397-464,705-767` — ~140 строк недостижимого хвоста в
  обоих `RenderTool` (именно там лежит «правильный» pending-код, что и
  замаскировало баг agentic_fetch). **M**
- [ ] `lsp/handlers.go:79` — `fileWatchHandler` никогда не устанавливается:
  весь путь registerCapability→watchers мёртв; удалить или довести. **S**
- [ ] `main.go:3-10` — swagger-аннотации несуществующего API (остаток
  upstream). **S**
- [ ] `db/connect.go:39,213` — `goose.SetBaseFS` вызывается дважды. **S**
- [ ] `thread/manager.go:1097` — док-блок `Shutdown` приклеен к
  `SetPermissionsSkip`; сам `Shutdown` без документации. **S**
- [ ] `shell/jq.go:122` — неизвестный флаг до фильтра молча становится
  фильтром; `shell/expand.go:158` — не-ASCII stderr превращается в
  `????`. **S**
- [ ] `hooks/runner.go:98` — дедупликация хуков только по Command: таймаут
  и имя первого приписываются обоим. **S**
- [ ] `tools/search.go:264` — sleep до 2с под глобальным мьютексом, без
  ctx; `tools/question.go:90` — единственный инструмент без проверки
  пустого sessionID. **S**
- [ ] `lsp/manager.go:90,409` — WaitGroup из горутин ради синхронного
  колбэка; сравнение ошибки строкой `"signal: killed"`. **S**
- [ ] `fsext/fileutil.go:154` — `limit*2` с авторским «NOTE: why x2?» —
  задокументировать или убрать. **S**
- [ ] UI-мертвечина (проверено grep'ом): семь неинстанцируемых
  `*ToolMessageItem`-обёрток; `SetSpinningFunc`; мёртвые builder-опции
  diffview (`LineNumbers`/`Height`/`YOffset`/`InfiniteYScroll`/
  `ContextLines`) с машинерией height-ellipsis; `anim.Width()`;
  `list.PrependItems`/`InvalidateFrozen` и `FilterableList.Append/Prepend`
  только в тестах; `notification/native.go ResetNotifyFunc`;
  `completions.KeyMap/Full/ShortHelp`; `common/interface.go Model[T]`;
  `util.Cursor`; `capabilities.SupportsTrueColor/SupportsSixelGraphics`;
  22 из 25 `styles.Brand*`; `image.ResetCache()` — пустой no-op, честно
  вызываемый из dialog_actions; write-only поля в permissions/question_*;
  `model/chat.go:1534` — мёртвая итерация графем на каждый двойной клик;
  `model/threads_dock.go:985` — identity-функция; опечатка
  `hightlightCode`. **S–M суммарно**
- [ ] `mcp/registry.go:212` + `tokenwrite.go:99` — production
  `tokenPersist` — заглушка `return nil`, ключ без `gjson.Escape`; удалить
  или доделать. **S**
- [ ] `config/paths.go:159` — `worktreeRootCache` не инвалидируется после
  `git init` в сессии. **S**
- [ ] Устаревшие комментарии: `credentials.go:406` (`loadTokenFromDisk`),
  `shellconfig/load.go:16` («runs while write lock is held»),
  `presentation.go:13» («depends only on leaf UI packages» — импортирует
  session), doc-ссылки chat на несуществующие файлы. **S**

## Учётные поправки к REFACTORING.md (2026-08-20)

Три пункта числятся `[x]`, но выполнены не полностью — снять галочки или
явно признать отказ:

- [ ] Фаза 5, пункт 11: вторая половина («extract `pendingState` into a
  channel-driven `messageWriter`») не сделана —
  `internal/message/message.go:84-118,336` на месте, включая busy-wait
  `time.Sleep(time.Millisecond)`; «flush before read» протекает к
  вызывающим.
- [ ] Фаза 1: `diffview/style.go:28` — `DefaultLightStyle` числится
  удалённым, но существует.
- [ ] Фаза 0: `workspace/resolve_session.go:44` — клиентский скан с TODO
  «expose GetLast» остался, хотя пункт закрыт (функционально корректно,
  но статус неверен).

## Системное (дешёвая профилактика вместо ловли поштучно)

1. **Гонки cmd-замыканий в UI** — 12+ мест одного класса при живой
  конвенции в AGENTS.md. Завести механическую проверку: линтер/ревью-правило
  «возвращаемое `func() tea.Msg` не захватывает `m`», плюс`-race`-прогон
  TUI-тестов в CI.
2. **«Кэши, которые молча не кэшируют»** — три независимых случая
  (completions/item, list/BaseItem, chat/ShellItem). Тест-хелпер,
  проверяющий cache-hit после повторного Render.
3. **Байтовые обрезки/индексы вместо `ansi.Truncate`/рун** — пять мест в
  этом ревью (cmd/threads, chat/question, tools/fetch, common/button,
  elements). Один grep-линт по `\[:\d+\]` в UI-пакетах окупится.
4. **Тема «permission обходится сбоку»** (фаза 0) — после фикса добавить
  таблично-управляемый тест: каждый инструмент, способный писать,
  обязан проходить permission+confinement; новый инструмент без записи в
  таблицу — красный тест.

## Чистые зоны этого ревью (чтобы не перепроверять)

`internal/event` (осознанный шим), `internal/app/shutdown.go`,
`agent_dispatch.go`, `internal/db` (пул, InTx, миграции),
`internal/session`, `internal/gc` + `cmd/gc.go`, `internal/lock`,
`internal/log/log.go`, `internal/stats`, `internal/testenv`,
`internal/clipboard`, `internal/filepathext`, `internal/csync`
(кроме `Map.Reset`), `config/configfile.go`, `oauth/codex/oauth.go`,
`oauth/mcp/handler.go`, `shellconfig/builder.go`, `config/staleness.go`.
