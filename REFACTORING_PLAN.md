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

## 2. Баги — средние — закрыто

Все пункты закрыты (2026-08-27/28), каждый с регрессионным тестом, про
который проверено, что он падает без фикса. История в git.

Не «починено», а осознанно задокументировано:

- `chat/streaming_markdown.go` — замороженный префикс не ревалидируется при
  позднем `[ref]: url`. Ревалидация стоит дороже выигрыша от заморозки;
  ограничение описано в коде вместо того, чтобы притворяться, что его нет.
- `hooks/runner.go` — брошенная по таймауту горутина остаётся осознанным
  компромиссом, но теперь `Run` хотя бы **сообщает** об этом ошибкой.

---

## 3. Рефакторинг (SOLID / DRY / KISS / YAGNI) — выполнено

| Этап | Результат |
|---|---|
| 1. DRY в `agent` | Схлопнут дубль `UpdateModels`, извлечён общий resolve-refresh-retry, вариадик → явный параметр, снят алиас `dispatch`, один `buildModel` для обоих путей, `runtimeInputs` кешируется с инвалидацией по версии конфига/поколению скиллов/идентичности адаптеров |
| 2. God-файлы `agent` | `agent.go` **1374 → 285 строк**: выделены `session_call.go`, `run_turn.go`, `cancel.go`; четыре функции >100 строк разбиты на шаги |
| 3. `thread` + `workspace` | Слияние → `merge.go`, permission-relay → `permissions.go`, пять копий teardown → `releaseRuntime`. **`Workspace` разбит на 14 ролевых интерфейсов**, `app_workspace.go` ~1100 → каркас + 11 файлов по ролям, `readOnlyWorkspace` инвертирован на default-deny |
| 4. `config` + `oauth` | Кредитная половина `ConfigStore` → `store_credentials.go` (−450 строк), убран двойной merge, дедуплицирован dispatch в `shellconfig` |
| 5. UI | Три копипаст-секции панели → `panelSection`, две кеш-машины → generic `listCache`, 25-case switch → таблица, дедупликация guard'ов, `diffview` |
| 6. `lsp`/`discover`/`skills`/`fsext` | Один обход fastwalk, generic-энричер, `failCandidate`, `reusableClient`, объединён `traverseUp` |

Не сделано намеренно, с обоснованием: обобщение шести `buildXProvider`
(у Anthropic своя логика — против KISS), объединение двух loopback-серверов
OAuth (разные контракты: одноразовый против конкурентного), замена 10-мс
поллинга в `CancelAll` на condvar (потребовала бы менять дисциплину
блокировок), вынос мутации layout из `Draw` (сломало тест — откачено),
сегментация интерфейса `SessionAgent` (одна прод-реализация, моки в пакете).

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
