# План рефакторинга

Только открытые пункты. Убирайте пункт, когда он закрыт; история остаётся
в git. Внутри фазы задачи независимы и могут идти параллельными коммитами.

Условные обозначения: **[S]** малая правка (≤ 1 файл, ≤ 1 час),
**[M]** средняя (несколько файлов, тесты), **[L]** структурная (новый пакет
или перенос ответственности).

## Решения первого аудита (2026-09-02, bdc47daab), которые стоит помнить

Все семь фаз закрыты. Три вывода остаются в силе:

**Кеширование окружения конфигурации отклонено, не отложено.** Записи
`env` резолвятся лениво против текущего окружения, и это зафиксировано
двумя тестами: повтор после ошибки авторизации и
`TestLoadRuntimeResolverReadsCurrentEnvironment`. Попытка закешировать
сломала оба. Проблема реальна — запись с `$(cmd)` выполняет команду на
каждое разрешаемое значение — но лечится не кешированием целиком.

**Три места в `internal/workspace` остаются на своих местах.** Перенос
`resolve_session.go`, `session_changes.go` и `custom_provider.go` в
`appws` дал бы цикл импорта или заставил бы UI импортировать бэкенд
напрямую.

**Ранняя проверка границ рабочей папки в мутирующих инструментах не
избыточна.** Без неё сначала срабатывают stat и проверка свежести, отказ
приходит с другим текстом, а файлы вне папки читаются до отказа.

---

# Аудит 2 (2026-09-02, `main`, b04a0e592)

Критерии: SOLID, DRY, KISS, YAGNI, границы контекстов. Каждая находка
подтверждена чтением кода; статистика (граф импортов, длина функций,
плотность комментариев) снята скриптами по `internal/`, без
`third_party`.

## Фаза 1. Баги и подозрительные места

### 1.1 [отклонён] Непроверенный `Close` при скачивании

Находка ошибочна. `errcheck` указывает на `outFile.Close()` в `defer`
(`download.go:187`), но это путь очистки: файл там всё равно удаляется по
`cleanupTmp`. Успешный путь закрывает файл явно и проверяет ошибку
(`download.go:204`) до `fsext.ReplaceFile`. Порядок «скопировать →
закрыть с проверкой → переименовать» уже верный, менять нечего.

### 1.2 [S] `git.BranchExists` не умеет отличать сбой от отсутствия

`internal/git/git.go:261-266` возвращает `false, nil` на любую ошибку
`rev-parse`. Следствия: ветка `err != nil` в `thread.Manager.Create`
(`manager.go:256`) мёртвая; при реальном сбое (не репозиторий, битый
`.git`) создание идёт дальше до `WorktreeAdd` и падает там с другим
текстом. Тот же паттерн `err == nil && !exists` в `thread/merge.go:167` и
`git.go:564`. Разделить «нет ветки» (exit 128 с `fatal: Needed a single
revision`) и прочие ошибки.

### 1.3 [S] `AgentRunStream` молча отключает разрешения

`internal/workspace/appws/app_workspace_agent.go:317` вызывает
`Permissions().AutoApproveSession(sessionID)`, и это никогда не
отзывается (`permission.go:621`, map только растёт). Сейчас единственный
вызывающий — `cmd/run.go:211`, но побочный эффект спрятан в методе с
названием «stream»: вызов из TUI дал бы обход разрешений. Сделать
явным на стороне `cmd/run` (отдельный метод контракта или параметр).

### 1.4 [S] Вложенное условие в `runTurn`

`internal/agent/run_turn.go:837-889`: `if A || B { if !A || B { return }
… }` сводится к `if B { return }; if A { drain; return }`
(B = `t.currentAssistant == nil`, A = `context.Canceled`). Логика верна,
но 60 строк комментариев обосновывают то, что после упрощения читается в
три строки.

### 1.5 [S] Мелочи

- `internal/workspace/read_only_workspace.go` — `ErrReadOnlyOperation.Error()`
  зашивает текст «the thread could not be reactivated» в общий тип
  ошибки read-only workspace.
- Мёртвые присваивания: `agent/tools/sennit_logs.go:608` (`pos =
  chunkStart`), `ui/chat/docker_mcp.go:211,240` (`action := tool`
  перезаписывается во всех ветках).

Остальные срабатывания расширенного набора линтеров (23 `nilerr`, `gosec`
про индексы и `math/rand`, `badCond` в chatlist) — ложные или осознанные:
текстовый ответ модели вместо Go-ошибки, как и записано в AGENTS.md.

## Фаза 2. SOLID

### 2.1 [L] `config.ConfigStore` — god-объект

71 метод: чтение и запись конфига, credentials, accounts, MCP-токены,
reload/watch, staleness и Docker MCP (`config/docker_mcp.go:121-182`).
Там же `var defaultDockerMCPCache` — процессное глобальное состояние в
пакете, о котором AGENTS.md говорит «config is a store, not global
state». Первый шаг — вынести Docker MCP (см. 5.4), затем credentials и
accounts в свои типы, оставив стору только снимок и запись полей.

### 2.2 [L] `agent.delegationFinalizer` делает всё

29 полей, пять мьютексов, `atomic.Pointer`, `sync.Once`: skills,
счётчики под-сессий, HTTP-клиент для fetch, delegation tools, кеш
runtime-inputs, readiness. Название говорит «финализатор». Разрезать по
ответственностям; skills и fetch-клиент точно не его.

### 2.3 [M] `ui/model.UI`

35 полей, 174 метода на 64 файла; `updateSession` 341 строка,
`updateMouse` 347, `Root.Update` 210. Минимум — вынести обработчики
`case`-веток в методы с именами, как уже сделано для `applyBusyState`.

### 2.4 [отклонён] `readOnlyWorkspace` написан руками

799 строк и 120 методов, из них 54 форвардят в `w.ws`, — но встраивать
`Workspace` нельзя, и это уже осознанное решение, описанное в шапке файла
(`read_only_workspace.go:45-69`). Без встраивания каждый метод контракта
обязан быть реализован явно, иначе падает `var _ Workspace =
(*readOnlyWorkspace)(nil)`. Это default-deny: новый мутирующий метод
контракта — ошибка компиляции, а не молчаливое проксирование в живой
workspace. Встраивание обменяло бы ошибку компиляции на тихую дыру.
Полноту классификации сторожит
`TestReadOnlyWorkspace_MethodClassificationIsComplete`. Форвардеры —
цена default-deny, а не небрежность.

### 2.5 [M] Контракт `FrontendWorkspace` неоднороден

`workspace.go:635-680` — 20 ролевых интерфейсов вперемешку с десятью
«голыми» методами (`VerifyProviderAPIKey`, `ImportCopilot`,
`KnownProviders`, `CurrentPlanUsage`…), плюс семь одно-методных
`Account*`-интерфейсов (`:319-345`) без самостоятельных потребителей.
Первый аудит (6.4) сообщал, что однометодные роли свёрнуты — семь
остались. Либо все методы в роли, либо роли только там, где уже есть
потребитель.

### 2.6 [M] `shutdownPhases.Shutdown` — пять списков и три флага

`app/shutdown.go:167-421`: `preCleanupHooks`, `shutdownHooks`,
`cleanupFuncs`, `criticalCleanupFuncs`, `finalCleanupFuncs` плюс пять
именованных хуков (`mcpClose`, `mainDBRelease`, `stopBackgroundShells`,
`stopLSP`, `agentWorkStopped`), пять copy-paste циклов и флаги
`hooksSucceeded` / `dependenciesStopped` / `repoUsersStopped`. Порядок и
условия запуска живут только в комментариях. Это одна упорядоченная
таблица фаз `{name, run, gate}`.

## Фаза 3. DRY

### 3.1 [M] Один набор зависимостей скопирован 4-5 раз

`cfg, sessions, messages, notify, runComplete, mcp, latency, builder`
повторяются в `coordinator`, `delegationFinalizer`, `turnDispatcher`,
частично в `runtimeBuilder` и `sessionAgent`. Один `deps`-структ,
передаваемый по указателю.

### 3.2 [S] Форвардеры в `delegationFinalizer`

`delegation_finalizer.go:394-420` — пять однострочных методов, которые
только добавляют `d.operationPort()` к вызову `d.builder.*`. Вызывать
builder напрямую.

### 3.3 [M] Два `Create` и два механизма отката в `thread`

`Manager.Create` (`manager.go:233`) и `TaskManager.Create`
(`tasks.go:162`) — одна хореография (beginOp → store.Create → publish →
lock control → Spawn → session → SetSession → setStatus → startRun), но
откат в одном через `unwinder` (`rollback.go`), в другом — через флаг
`owned` (`tasks.go:226`, `lifecycle.go:781`). Оставить `unwinder`,
общую часть вынести.

### 3.4 [S] Дубль в копировании результата edit

`ui/chat/tools_copy_file.go:79-151` — `formatEditResultForCopy` и
`formatMultiEditResultForCopy` дословно повторяют друг друга.

## Фаза 4. KISS

### 4.1 [L] Комментарии рассказывают историю багов вместо кода

`internal/agent`: 4822 строки комментариев на 8341 строку кода (58 %);
`internal/thread`: 2180 на ~2400 (~90 %). 864 вхождения «used to / no
longer / previously» в прод-коде. `runTurn` — 320 строк, из них кода
около 80. Объяснения вида «раньше здесь терялся промпт, потому что…» —
материал для commit-сообщений и регрессионных тестов, которые уже
существуют; в коде они прячут логику и стареют. Отдельным проходом
оставить «почему», убрать «как было». Самый большой выигрыш в
читаемости за наименьший риск.

### 4.2 [M] Длинные функции

`agent/providers.go:67 getProviderOptions` 265 строк,
`agent/usage.go:102 summarize` 285, `ui/model/keys.go:85
keyMapForPlatform` 250, `ui/dialog/question_form.go:497 Draw` 250,
`agent/tools/sennit_logs.go:568 scanBackward` 219.

## Фаза 5. YAGNI и границы контекстов

### 5.1 [L] `internal/proto` охраняет дублирование

914 строк в пакете с именем сетевого протокола, которого нет (AGENTS.md
это признаёт), и три `*_parity_test.go`, которые заставляют DTO повторять
доменные типы. Оставить только `proto.Thread` и `LSPClientInfo` (реально
живые DTO), остальное — алиасы или удаление; parity-тесты убрать вместе
с дублями.

### 5.2 [L] `internal/config` — хаб

Его импортируют 22 пакета, включая `lsp`, `projects`, `importer`,
`doctor` и все `ui/*`. Контракт `workspace` отдаёт UI `Config()
*config.Config` целиком (`workspace.go:305`). UI должен получать DTO с
тем, что читает, а не весь стор.

### 5.3 [M] Контракт `workspace` — не лист

Импортирует 15 внутренних пакетов (`config`, `shell`, `oauth`,
`accounts`, `git`, `stats`, `question`…). Первый аудит (5.4) убрал `shell` из
`ui/model`, но `workspace/accounts_dto.go:24` (`MaxBackgroundJobs =
shell.MaxBackgroundJobs`) возвращает его транзитивно. `ui/dialog`
напрямую импортирует `oauth`, `providers/accounts`, `proxyhttp`
(`account_form.go:199`); `.golangci.yml` сам признаёт утечку codex
sign-in.

### 5.4 [L] Docker MCP размазан по восьми пакетам

`config`, `ui/model`, `ui/dialog`, `ui/chat`, `workspace`, `appws`,
`mcpid`, `agent/tools` — вендорная фича без владельца. Свой пакет с
availability-кешем, конфигом и рендером; `config` и `workspace` знают
о нём только через общий MCP-контракт.

### 5.5 [L] `internal/agent` смешивает два контекста

14 000 строк: оркестрация (dispatcher, coordinator, turn) и провайдерная
сантехника (`rotation.go`, `credential_refresh.go`, `providers_build.go`,
`provider_log.go`, `compat.go`) — при существующем `internal/providers/*`.
Провайдерную часть переносить туда.

### 5.6 [S] Проверить необходимость

`agent/tools/sennit_logs.go` — 1357 строк на инструмент чтения собственных
логов с курсорной пагинацией и обратным чанковым сканом. Стоит проверить
по использованию, нужна ли такая мощность.

## Порядок

1. 1.2 — реальный баг, полчаса.
2. 3.1, 3.2 — общий `deps` и удаление форвардеров.
4. 5.4 → 2.1 — Docker MCP из `config`, затем разрез стора.
5. 4.1 — отдельным проходом, по пакету за коммит.
