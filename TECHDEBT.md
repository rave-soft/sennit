# Technical debt

What is still open after the Crush fork, the rebrand, and the removal of the
external services. Every entry carries its reason and a concrete next step, so
it can be picked up again without repeating the investigation.

Closed entries do not accumulate here: once a debt is paid the entry is
deleted, and the history stays in git.

## Open debt

- **Gemini and two consecutive user contents.** Steering is delivered as its
  own `user` message after all tool results
  (`internal/agent/completion_inbox.go`, `prepareStep` in
  `internal/agent/turn.go`). Anthropic merges such a pair itself; for
  OpenAI-compatible providers this was verified with a live request (200, and
  the model acted on the steering specifically); in OpenAI Responses keeping
  them separate is what the protocol expects. On Gemini it is untested and,
  per the investigation below, cannot be fixed from `internal/agent` — closing
  requires either a live Gemini request settling whether it actually 400s, or
  a fantasy-side fix.

  Traced 2026-08-22 through `prepareStep`/`foldSteering`
  (`internal/agent/turn.go:106,237`) and fantasy's Gemini adapter
  (`charm.land/fantasy@v0.40.0/providers/google/google.go`,
  `toGooglePrompt`). For a `user → assistant(tool_calls) → tool →
  user(steering)` turn, the `fantasy.Message` list `prepareStep` hands to the
  model is `[..., Message{Role: Assistant, ToolCallPart...}, Message{Role:
  Tool, ToolResultPart...}, Message{Role: User, TextPart(steering)}]` — the
  tool-result message and the steering message are two distinct
  `fantasy.Message`s with different `Role`s (`Tool` vs `User`), appended by
  two different stages (fold of the prior step's results, then
  `foldSteering`). `toGooglePrompt` is a single `switch msg.Role` over that
  list with one `case` per role and no merge step anywhere in the function:
  `case fantasy.MessageRoleTool` reads only `ContentTypeToolResult` parts and
  emits `&genai.Content{Role: genai.RoleUser, Parts: [FunctionResponse...]}`
  (google.go:484-552); `case fantasy.MessageRoleUser` reads only
  `ContentTypeText`/`ContentTypeFile` parts and emits its own
  `&genai.Content{Role: genai.RoleUser, Parts: [...]}` (google.go:384-414).
  So the concrete `genai.Content` sequence we send ends `..., {Role: "model",
  Parts: [FunctionCall...]}, {Role: "user", Parts: [FunctionResponse...]},
  {Role: "user", Parts: [Text: steering]}` — two adjacent `user`-role
  entries, confirming the entry's original claim.

  Merging cannot be done from our seam. The two source messages have
  different `fantasy.Message.Role`s, and each of `toGooglePrompt`'s role
  branches only reads its own part type: the `Tool` branch's part loop has no
  case for `ContentTypeText` (silently ignored — no `default`), and the
  `User` branch's loop has no case for `ContentTypeToolResult`. So combining
  the tool results and the steering text into one `fantasy.Message` — under
  either `Role: Tool` or `Role: User` — does not produce one merged
  `genai.Content`; it silently drops whichever part type the branch does not
  know about (either the tool results, which is a much worse failure than
  today, or the steering text, which defeats the point). There is no third
  seam here: `prepareStep` only supplies fantasy's `[]fantasy.Message`, and
  the role-to-`genai.Content` mapping — including the merge that would need
  to happen — lives entirely inside `toGooglePrompt`, below where
  `internal/agent` can reach. A real fix is a fantasy change (either merging
  adjacent same-mapped-role `genai.Content`s in `toGooglePrompt`, or fantasy
  exposing a hook before the request is built).

  Next step: run one real `user → assistant(tool_calls) → tool →
  user(steering)` request against Gemini once a key is available. If it
  400s, the fix has to land in fantasy (file it upstream); there is nothing
  further to try from this side first.

- **Bash в confined-workspace: статический разбор есть, песочницы нет.**
  Фаза 0 закрыла рабочий каталог, а разбор команды через `mvdan/sh` теперь
  ловит и литеральные абсолютные пути в аргументах и целях редиректов вне
  границы (`bashConfinementRefusal` в `internal/agent/tools/bash.go`).
  Отказ, а не запрос разрешения: confined-workspace — это тред, который
  наследует yolo, и запрос там был бы автоматически одобрен. Аргументы
  заведомо read-only команд (`cat`, `diff`, `ls`, `grep`, `git diff/log/
  show`…, см. `readOnlyCommands`) не проверяются — граница держит
  изменения внутри, а не запрещает смотреть наружу; редиректы проверяются
  всегда, `/dev/null` не считается «снаружи».

  Осталось непокрытым и намеренно не угадывается: `$VAR`, `$(...)`,
  арифметические подстановки, глобы и относительные пути, убегающие через
  симлинк во время выполнения. Всё это зафиксировано тестом
  `TestBashTool_ConfinedWorkspaceDoesNotCatchDynamicPaths`, чтобы никто не
  принял проверку за песочницу. Настоящая граница — это bubblewrap или
  landlock; до тех пор динамические пути в треде держит только
  permission-промпт.

- **Хвосты ревью-раунда 3 (2026-08-23).** `REFACTORING-2026-08-23-r3.md`
  закрыт и удалён: разделы 1.1–1.3 (баги) и 5 (дрейф документации) сделаны
  целиком, фазы 4–7 (границы слоёв, SRP, DRY/KISS, мёртвый код) — нет.
  Проверено против кода на 2026-08-25; ниже только то, что осталось
  открытым, с местом и следующим шагом. Номера строк не приводятся: они
  уже сдвинулись один раз, ориентир — имя символа.

  Открытых багов в этом списке не осталось: все шесть закрыты и удалены
  отсюда 2026-08-28/29 — см. `REVIEW-2026-08-28.md`, где то же дерево
  проверено заново. Для справки, чем каждый кончился:

  - trust-гейт для проектного конфига **построен**: `IsTrusted` в
    `internal/config/trust.go`, применяется в `buildConfig`, и
    недоверенный проект получает объяснение «project configuration is
    disabled because this project is not trusted» вместо молчаливого
    исполнения;
  - `provider remove` **переведён на tombstone** — проектные слои тут
    были ни при чём (`providers` глобален), но глобальных слоёв четыре,
    и провайдер из машинного конфига не маскировался;
  - `turnTimer` **привязан к сессии**, а не к процессу;
  - остальные три (`lsp_replace_symbol`, `filetracker`, гонка
    `lsp.Client.Restart`) были закрыты ещё раньше и здесь только
    числились.


  **Границы слоёв.** Закрыто: `hooks ↛ config` (L2), `threadspawn.AppOf`
  (L8), листовой `internal/proxyhttp` (L11), тест паритета
  `proto.Thread*Status` ↔ `thread.Status` (L12), `thread/store_testing.go`
  → `store_export_test.go` (половина L1). Открыто:

  - L4 `config → clipboard | skills | db | oauth/{codex,copilot}` —
    **три четверти закрыты 2026-08-29, оставшаяся четверть
    переформулирована.**

    `clipboard` — ушёл: `EnvironmentProblems` и `SkillProblems` вынесены
    в новый `internal/doctor`. Они и не отвечали на вопрос «правильно ли
    настроено» — они отвечают «чего нет на машине» и «что сломалось при
    обходе диска», а `config.Doctor` намеренно отвечает только по
    конфигу и остаётся воспроизводимым где угодно. Все три вызывающих
    (`cmd`, `ui/dialog`, `agent/tools`) уже принимали функцию
    параметром, так что шов был готов. Заодно `SkillProblems` получил
    тесты, которых у него не было.

    `db` — оказался ложным: `internal/config` не импортирует
    `internal/db` и не импортировал (`go list -deps` даёт **0**). Через
    базу ходил `internal/config/modelcache` — сосед по каталогу, не
    зависимость: сам `config` его не зовёт, зовут `providerload` и
    `cmd`. Пакет переехал в `internal/modelcache`, где он и живёт по
    смыслу. SQLite он открывает по делу, и никакой перенос этого не
    отменяет.

    `skills` — остались два места, оба не про эту границу:
    `import.go` уехал в `internal/importer`, а `SplitFrontmatter` — в
    листовой `internal/frontmatter`, откуда его берут все трое (skills,
    config, importer). Заявленная причина экспорта («у config этот импорт
    и так есть — см. doctor.go и import.go») к тому моменту стала ложной:
    оба файла из конфига ушли. Проверено: `go list -deps
    ./internal/config` больше не содержит ни `skills`, ни `clipboard`, ни
    `db`. Заодно у парсера появились тесты на BOM, CRLF, пустой блок и
    горизонтальную черту в теле — их не было ни одного.

    `oauth/{codex,copilot}` — **закрыт наполовину 2026-08-30, вторая
    половина снята по замеру.**

    Записанный «следующий шаг» (унести `SetupGitHubCopilot`/`SetupCodex`
    к построению провайдера или в `credentials`) не работает:
    `ApplyPostCredentialSetup` зовут из самого `internal/config` в
    четырёх местах (`reload.go:165`,
    `store_credentials.go:146,314,340`), так что импорт с методами не
    уедет. Оставался бы хук из `init()` — форма, отвергнутая в этой же
    серии на границе `workspace`: контракт без реализации получает
    молчаливый no-op вместо ошибки компиляции.

    **Сделано другое и по делу.** Тяжёлым в этой зависимости был ровно
    один символ — `codex.FetchUsage`, то есть поход по HTTP к вендору из
    пакета, который импортирует почти всё. Шов уже был:
    `refreshAccountLimits` принимала fetcher параметром ради тестов.
    Теперь параметр стал частью экспортированной сигнатуры и возвращает
    `accounts.Usage` вместо вендорного типа, а проводка переехала в
    `internal/workspace/appws`, где пакеты входа и так есть.

    **Остаток снят по замеру, а не по усталости.** После этого конфигу от
    `oauth/{codex,copilot}` нужна только чистая идентичность вендора:
    `ProviderID`, `ProviderName`, `APIBaseURL`, `AccountIDHeader`, два
    построителя заголовков и чтение claim'а из JWT — всё на stdlib.
    Стоимость этого импорта измерена: **12 зависимостей из 343**
    (`event`, `log`, `oauth/callback`, сами два пакета, плюс
    `html/template`, `text/template`, `lumberjack`, `x/term`). Вынести
    идентичность в листовой пакет — это правка в **31 файле**, потому что
    граница между «идентичностью» и «потоком» проходит не по файлам: в
    `usage.go` рядом лежат чистое хранилище снимков в памяти и
    HTTP-запрос. Тридцать один файл ради двенадцати зависимостей — нет.

    Что осталось записать в другой пункт: `internal/ui/model/sidebar.go`
    и `internal/ui/dialog/oauth_codex.go` импортируют `oauth/codex`
    напрямую. Это L9, а не L4.
  - ~~L5 `(*Config).configureProviders(ctx, store, …)`~~ — **снят
    2026-08-29: описанного кода нет.** Ни метода, ни файла
    `internal/config/providers_merge.go` в дереве не осталось —
    построение провайдеров уехало в `internal/providerload`. Правку
    сделали сильнее, чем предлагал «следующий шаг» здесь: store приходит
    туда не как аргумент чистому типу данных, а полем `RuntimeInput.Store`
    и под интерфейсом `config.RuntimeStore` из двух методов
    (`RemoveRuntimeConfigField`, `WriteRuntimeConfigFields`) — то есть
    записывающий получает право записать ровно два вида полей, а не
    store целиком. Закрыто не этой работой, а чужой; запись пережила
    код.
  - L7 `workspace.Workspace` — ролевые интерфейсы написаны (14 штук в
    `workspace.go`), но `Workspace` складывает их обратно, и
    `ui/common.Common` отдаёт его целиком каждому компоненту. Замер
    2026-08-28, однако, не подтверждает выгоду от сужения: полное имя
    `workspace.Workspace` встречается в `internal/ui` всего в восьми
    объявлениях, и оставшиеся компоненты реально охватывают по три-четыре
    роли. Разбор — в `REVIEW-2026-08-28.md`, пункт 11. Хвост про
    «удалить `AgentQueuedPrompts`/`SetCurrentSession`, если они всё ещё
    не вызываются» снят: проверено 2026-08-29, вызываются (3 и 2 раза).
  - L9 UI мимо workspace. depguard закрыл `agent`, `thread`, `db`, `app`,
    но `internal/ui/**` по-прежнему импортирует `commands`, `config`,
    `diff`, `discover`, `git`, `history`, `hooks`, `lsp`, `message`,
    `oauth/{codex,copilot}`, `permission`, `question`, `session`, `shell`,
    `skills`, `stats`. Следующий шаг: по списку из плана — `diff`/`git`/
    `fsext` в `session.go`, `commands.LoadCustomCommands`, LSP-состояния,
    `hooks.HookMetadata` → proto DTO, OAuth-флоу из диалогов; затем
    расширить depguard.
  - ~~L10 `internal/db/datadirlock.go`~~ — **закрыт 2026-08-29.** Файл
    переехал в собственный листовой пакет `internal/workspacelock`
    (`Acquire`, `Lock`, `ErrLocked`); переезд оказался чистым — код не
    зависел от остального `internal/db` вовсе, только от `brand`, `lock`
    и `version`. Смысл в том, что блокировка проекта — это flock на
    каталоге, а не работа с базой: `Bootstrap` берёт её **до** того, как
    открыта хоть одна БД, и раньше платил за это импортом всего
    storage-слоя вместе с драйвером. Свойство закреплено
    `dependency_guard_test.go` в новом пакете.

  **Божественные объекты.** Закрыто: walker вынесен в
  `agent/tools/confinement.go`, `ProviderConfig` — в
  `config/provider.go`, `lsp.Client` разнесён на
  `runtime`/`diagnosticsStore`/`filesync`/`requests` (403 строки вместо
  834). Открыто: `Coordinator` (интерфейс из 20 методов плюс построение
  провайдеров, OAuth-refresh и cost под-агентов) и `internal/ui/model`
  (18.2k строк non-test в 54 файлах). `sennit import` **вынесен
  2026-08-29** в `internal/importer` (557 строк). Связность оказалась
  тонкой: наружу торчали `ProviderConfig`, `GlobalConfig()`,
  `ResolveModelString`, `ClaudeToolNames`, `CanonicalToolName` — уже
  экспортированные, — плюс два внутренних (`stringList`, `validAgentID`).
  Оба экспортированы, а не скопированы, и именно в этом смысл: импорт
  обязан принимать ровно то, что принимает загрузка, иначе `sennit
  import` напишет агента, которого не прочитает `sennit`.

  `Coordinator` из этого списка снят 2026-08-29: интерфейс на месте, но
  ни один потребитель больше не получает его целиком — `AgentDispatcher`
  сузился до 2 методов, `DelegationParent.Parent` до 1
  (`CompletionDeliverer`), адаптер в `threadspawn` до 7. Делегированная
  задача больше не может через ссылку на родителя отменить его ход.

  `ConfigStore` снят как god object по числу, но записанное здесь
  обоснование было неверным: «главный потребитель зовёт около 35 из 75»
  — это моё же непроверенное число. Перемеренное независимым обходом
  селекторов через `go/types`: `agent` — 10, максимум по дереву 14
  (`workspace/appws`). Вывод устоял, но по другой причине: мешает не
  размер, а пересылка стора в пакетные функции `internal/config`. Двум
  чисто читающим потребителям (`lsp`, `prompt`) порт из 3 методов всё же
  выдан 2026-08-29. Разбор — в `REVIEW-2026-08-28.md`, пункт 14.

  **KISS / перформанс.** Закрыто: индекс `(session_id, created_at)` и
  снятие триггера `update_messages_updated_at`, сигнатурный пропуск
  перерисовки сайдбара; `testing.Testing()` из production-кода ушёл — 0
  вхождений на 2026-08-28.

  Закрыто и то, что записано здесь как открытое:
  `PushPopEnvOverrides` больше не держит лок на время discovery.
  `internal/providerload/loader.go:53-56` снимает его явно перед
  `runDiscoveryRequests` и берёт заново после, так что три секунды
  HTTP второй workspace не ждёт. Проверено по коду 2026-08-29.

  ~~`applyWorkspaceConfig`~~ — **снят 2026-08-30, это не дефект.**
  Разобрано по коду, и обе половины записи оказались неточными.

  JSON-круг — это и есть механизм слияния: workspace-слой применяется
  так же, как все остальные, — маршалингом накопленного конфига и
  повторной загрузкой с байтами слоя сверху. Заменить его — значит
  писать руками мержер для большой структуры, что хуже во всех
  отношениях. Повторный `setDefaults` зовётся не «ради
  `data_directory`»: круг теряет неэкспортированные поля и nil-ность, и
  этот вызов их чинит. Цена платится только там, где workspace-конфиг
  есть — путь отсекается сразу, если файла нет.

  **Настоящий риск здесь другой, и он теперь закрыт тестом.** Круг молча
  роняет всё, чего не видит `encoding/json`. Сегодня у `Config` два
  неэкспортированных поля, и оба восстанавливаются:
  `workingDir` — через `setDefaults`, `jsonAgentsBlockDetected` — явным
  OR. Третье, добавленное без такой же строчки, пережило бы все тесты,
  которые не грузят workspace-конфиг, и тихо сбрасывалось бы у тех, у
  кого файл есть. `TestConfigUnexportedFieldsSurviveTheWorkspaceMerge`
  читает объявление `Config` и требует запись в таблице на каждое
  неэкспортированное поле — с указанием, что именно его восстанавливает.

  **YAGNI и мёртвый код.** `internal/event` из списка снят: no-op'ы —
  задокументированное решение, а не долг. `internal/diffdetect` и
  `version.BuildID` удалены.

  Формулировка «цель — 0 non-test символов» отсюда снята как ошибочная,
  и `internal/testenv` из списка убран. `deadcode` считает достижимость
  от `main`, поэтому всё, что вызывается только из тестов, попадает в
  отчёт независимо от того, живое оно: `internal/testenv` — рабочий
  тестовый хелпер семи пакетов, а шесть «мёртвых» форвардеров в
  `delegation_finalizer.go` имеют по 2-19 вызовов в тестах. Вывод
  `deadcode` — не список задач.

  Разбор 2026-08-28 (89 символов после удаления восьми): по-настоящему
  мёртвых, без единой ссылки где бы то ни было, оставалось девять, из
  них восемь удалены. Остальное требует проверки по одному, и искать
  там надо не мёртвое, а боевой код без боевого потребителя — этот
  вопрос уже дал одну настоящую находку (механизм прерывания ожидания,
  §4.1 в `REVIEW-2026-08-28.md`). Подробности и метод — там же.

  **Разбор продолжен 2026-08-29: 84 → 74, гнездо `internal/agent/tools`
  закрыто целиком.** Классификация оказалась не двоичной, а тройной, и
  разные ответы требуют разного:

  - *Вытеснено, а комментарий этого не заметил* — удалять.
    `writeFileWithHistory` объявлял себя тем, «чем пользуются
    write/edit/multiedit, когда инструмент фиксирует содержимое файла»,
    а не пользовался им никто. Настоящий путь — `applyFileMutation`,
    который пишет через `fsext.AtomicWriteFileIfUnchanged` и отвергает
    файл, изменившийся между показанным диффом и записью. Оставшийся
    хелпер писал `os.WriteFile`, то есть единственный способ им
    воспользоваться — отказаться от этой проверки. Хуже, чем мёртвый
    код: приглашение. Удалён; его тест проверял только, что байты
    дошли до диска, и заменён на тест `recordFileHistory`, у которого
    покрытия не было вовсе.
  - *Обёртка, которой никто не хочет* — удалять.
    `validatePageKeyCursor` склеивала `openPageKeyCursor` и
    `finishPageKeyCursor`, но каждому настоящему вызывающему нужна
    граница, которую первая возвращает, — ровно то, что doc-комментарий
    над ними и объясняет. Та же форма, что `DoctorProblems`.
  - *Тестовое приспособление, живущее в боевом файле* — переносить, не
    удалять. `searchWithRipgrep`, `parseNumstat`, `readTextFile` и
    четыре «pre-T5» входа `sennit_logs` собирают результат целиком,
    тогда как боевой код везде страничный или потоковый. В боевом файле
    они читаются как второй, более простой способ сделать то же самое.
    Уехали в `testhelpers_collect_test.go`.

  Проверено и **не** тронуто: `multi_read` открывает курсор и не зовёт
  `finishPageKeyCursor`, в отличие от всех остальных инструментов, —
  выглядит как пропущенная проверка, но его курсор индексный, по
  собственному списку файлов запроса, поколения там нет и проверять
  нечего. `capTodosForDelegation` оставлен: его комментарий честно
  говорит, что вызывающего удалили, а логику держат осознанно.

  **Разбор доведён 2026-08-30: 84 → 65, и остаток классифицирован.**
  Дальше идти по этому списку смысла нет, и это вывод, а не усталость:
  оставшееся делится на три группы, из которых удалять нечего.

  - **Швы для тестов чужих пакетов.** `db.ResetPool` (32 вызова из
    тестов девяти пакетов), `db.UseMigratedTemplate` (12),
    `message/store.WithDebounce` (6), `toolmeta.Builtins` (5),
    `prompt.WithTimeFunc`/`WithPlatform`, `credentials.WithExchangeToken`.
    Их нельзя убрать в `_test.go`: файлы тестов не импортируются другими
    пакетами. Они обязаны остаться экспортированными и обязаны навсегда
    остаться в выводе `deadcode ./...`. Образец честной формулировки уже
    есть у `config.LoadData` — «это не потерянный вызывающий».
  - **Швы для тестов своего пакета с правдивым комментарием.**
    `config.WithExpander`, `accounts.WithClock`/`WithDebounce`,
    `csync.CompareAndDelete`. Технически переносятся в `_test.go`, но
    смысла нет: их doc-комментарии прямо говорят «production оставляет
    это незаданным». Двигать нужно было там, где комментарий **врал**, а
    не там, где он верен.
  - **Ложные срабатывания по природе анализа.**
    `proto.Attachment.MarshalJSON`/`UnmarshalJSON` — `encoding/json`
    находит их проверкой интерфейса в рантайме, чего статический анализ
    достижимости не видит. `AgentMessage` несёт `Attachments` и
    маршалится на каждой диспетчеризации. **Список §4 в ревью от
    2026-08-28 предлагал удалить оба** — это молча изменило бы кодировку
    всех вложений. Теперь над ними стоит комментарий, объясняющий, почему
    `deadcode` о них врёт.

  Что было сделано во втором проходе (72 → 65): в тестовые файлы уехали
  `dispatcher.enqueueCall`, `runtimeCache.invalidate`/`stats`,
  `lsp.HandleDiagnostics`, `git.AbortMerge`, `log.NewHTTPClient` — у
  каждого doc-комментарий описывал аудиторию, которой нет («используйте
  откуда угодно, где не держите лок», «тесты и ручная диспетчеризация»).
  Удалён `diffview.DefaultLightStyle`: приложение собирает
  `diffview.Style` из активной темы, и `DefaultDarkStyle` не существует,
  то есть это был дефолт без пути стать дефолтом. Схлопнут
  `config.AllToolNames` — у него был неэкспортированный близнец, который
  звало production, пока экспортированный обслуживал только тесты.

  Отдельно про `git.AbortMerge`: поток слияния тредов **не прерывает**
  слияние. `MergeIntoWorktree` при конфликте оставляет worktree в
  середине merge и возвращает список путей, а `mergeAttempt` на входе
  перепроверяет `ConflictedFiles` — замысел в том, чтобы агент разрешил
  конфликт на месте и слил снова. Экспортированный `AbortMerge` в
  `git.go` читался как шаг, который конфликтная ветка забыла сделать.

  Остаётся заметный пласт — фасад `internal/agent/tools/mcp`
  (`ArmInit`, `WaitForInit`, `Close`, `SubscribeEvents`, `GetState`,
  `BeginAuth` над `defaultRegistry`). Боевых вызывающих у него **ноль**:
  все держат свой реестр через `app.App.MCP`. Комментарий там до сих пор
  называл «множество существующих вызывающих (agent, workspace, app,
  commands)» — их нет; исправлено.

  **Обещанной выгоды от разматывания нет — проверено 2026-08-30 и
  записано, чтобы не начинать это снова.** Я написал, что
  process-global реестр мешает тестам пакета идти параллельно. Это
  неверно: в пакете уже 57 вызовов `t.Parallel()`, весь прогон — 4.3 с,
  а те несколько тестов, что лезут в `defaultRegistry` напрямую (включая
  подмену `broker`), параллельными не объявлены. Верхнеуровневые
  последовательные тесты в Go не пересекаются телами с параллельными —
  паузированные возобновляются только после того, как все стартовали, —
  так что общего состояния между ними по построению нет.

  Значит цена — механическая переделка пяти тестовых файлов, лезущих в
  поля `defaultRegistry`, а выгода — только шесть строк из вывода
  `deadcode`. Не стоит того; пласт остаётся, и теперь с причиной.

  **Системные меры из плана — заведены 2026-08-29, все три.**

  - «Каждая команда из `safeCommands` не обёртка другой» —
    `TestSafeCommands_ContainNoExecutors`. Это денилист, и он ловит
    только тех исполнителей, о ком подумали; место своё он оправдывает
    тем, что отказ здесь тихий и полный — новая запись не ухудшает
    промпт, а убирает его, — и тем, что падающий тест прочитают скорее,
    чем doc-комментарий.

    **Мера нашла второй обход той же формы, живой.** `safeCommands`
    держал `git branch`, `git tag` и `git remote` — читающие в голом
    виде и мутирующие с аргументами, а сопоставление префиксное.
    Проверено до правки: `git branch -D nope` выполнялся при **нуле**
    запросов разрешения; `bannedCommands` никакого git не несёт. Так же
    проходили `git tag -d`, `git tag v9` (создаёт), `git branch <имя>`
    (создаёт), `git remote remove`. Закрыто гейтом по аргументам
    (`argumentGatedSafeCommands`), который требует, чтобы **каждый**
    оставшийся токен был из списка read-only флагов: git принимает
    `git branch -v -D x`, поэтому проверки одного токена после префикса
    не хватило бы — читающий флаг провёл бы разрушающий мимо гейта.
  - Round-trip для каждого `ContentPart` — `internal/message/roundtrip_test.go`.
    Полнота проверяется не руками: тест читает исходники пакета и требует
    фикстуру для каждого типа с методом `isPart()`, плюс отдельно
    требует, чтобы ни одно поле фикстуры не осталось нулевым — иначе
    фикстура, а не код, решает, что покрыто. Цена забытой записи платится
    пользователем: `MarshalParts` на незнакомом типе даёт ошибку, а
    `UnmarshalParts` незнакомую обёртку **пропускает** (намеренно), так
    что наполовину подключённый тип пишется нормально и возвращается
    отсутствующим.
  - Линт на `m.com.` внутри `func() tea.Msg` — сделан AST-проверкой
    (`internal/ui/model/async_capture_guard_test.go`), а не grep'ом, и
    шире формулировки: запрещено чтение **любого** ресивера метода
    внутри такого замыкания, потому что после фазы 3 команду можно
    построить и на ресивере состояния. Нарушений в дереве нет — правило
    заводится как страховка уже соблюдаемой конвенции.

## Decisions deferred pending confirmation

### GitHub Copilot

Not removed, and keeping it is a deliberate decision (2026-08) rather than an
omission. It is a model provider, like OpenAI or Anthropic, not a Charm
service: removing it takes a backend away from the user, it does not detach
the fork from someone else's infrastructure.

What ships with it is worth knowing, because "just another provider" does not
describe the situation fully. The client authenticates with someone else's
OAuth application and presents itself as someone else's product:

- `clientID = "Iv1.b507a08c87ecfe98"` (`internal/oauth/copilot/oauth.go`) —
  the Copilot/VS Code client ID, not one registered to rave-soft.
- `userAgent = "GitHubCopilotChat/0.32.4"`, `editorVersion = "vscode/1.105.1"`
  (`internal/oauth/copilot/http.go`) — to GitHub's API, Sennit presents itself
  as the Copilot Chat extension for VS Code.

All of this is inherited from upstream rather than introduced here. An
inconsistency lives next to it: `SignupURL` in
`internal/oauth/copilot/urls.go` carries `editor=sennit`, so signup is branded
as Sennit while the traffic claims to be VS Code.

Three possible next steps, none of them started:

1. Do nothing — the status quo this entry records.
2. Remove Copilot entirely. The surface is larger than it looks: it is
   mentioned in 36 files, 24 of them non-test. The `internal/oauth/copilot`
   package (419 lines) and the `internal/ui/dialog/oauth_copilot.go` dialog
   (77 lines) go entirely; then the branches in `internal/cmd/login.go` and
   `logout.go`, the refresh path in
   `internal/config/credentials/credentials.go`, `SetupGitHubCopilot` and the
   token exchange in `internal/config/{config,store,providers_merge,reload}.go`,
   the dialog wiring in `internal/ui/model/`, the "model not enabled in
   Copilot" diagnostic in `internal/agent/turn.go`, and the plumbing in
   `internal/workspace/`. After that `catwalk.InferenceProviderCopilot` is no
   longer used anywhere.
3. Keep it, but stop impersonating VS Code: register a GitHub OAuth App of our
   own and send an honest `User-Agent`. Three constants to edit, but it needs
   an application registered, and whether GitHub grants such a client access
   to the Copilot API is an empirical question — theirs is a restricted list.

## Limitations of imported agent definitions

Not debt but the contract of `sennit import` — documented here because the
code points at this section (`internal/config/import.go`, the `sennit-config`
skill).

User's decision (2026-08): Sennit no longer scans another tool's config
directories on its own. Discovery — `internal/config/agents_markdown.go`
(`agentDirs`) and `internal/config/load.go` (`GlobalSkillsDirs`,
`projectSkillSubdirs`) — reads only `.sennit/agents` and `.sennit/skills`
(plus their global equivalents and `options.skills_paths`). It used to take in
`.claude/agents`, `.opencode/agent`, `.agents/skills`, `.claude/skills`,
`.cursor/skills`, and `.opencode/skills` as well — meaning Sennit silently
trusted the contents of directories another tool writes, without validating
them and without telling the user what failed to apply.

Bringing files over now goes through an explicit import only — `sennit import
claude|opencode [--skills] [--agents] [--dry-run] [--global] [--force]`
(`internal/config/import.go`, `internal/cmd/import.go`). The import copies
into `.sennit/skills`/`.sennit/agents` rather than reading a foreign directory
on the fly, and prints a report for every file (`imported`/`adjusted`/
`skipped` plus the reason or warnings) — the same limitations below still
apply, but the user now sees them at import time instead of discovering them
afterwards in the logs.

- `permission:` blocks from opencode files are **not applied**, neither on a
  normal load nor after an import. A role locked to read-only there gets
  everything listed in its `tools` under Sennit. The import reports this as a
  warning ("permission block is not supported; restrict tools via the tools
  list instead") and leaves a comment in the frontmatter, but does not write
  the field itself. Restrict via the `tools` list or the config's
  `permissions` section instead.
- The `model` field from foreign files understands `provider/model-id`
  references when such a model exists among the configured providers — it
  resolves through `config.ResolveModelString`. There are no `large`/`small`
  slots for `Agent.Model` any more: those words carry no special meaning even
  in Sennit's own agent files and are treated like any other unresolvable
  string. Values that resolve to nothing (`opus`, say — a model name from
  another tool that is not in the config — or literally `large`/`small`) are
  not dropped silently on import: the `model` field is omitted (the agent
  inherits the main model), the original value stays in the frontmatter as a
  comment (`# original model: ... — not available`), and the import reports it
  as a warning rather than an `slog.Debug`.
- `reasoning_effort` / opencode's `effort`: `low`/`medium`/`high` carry over
  as they are; close but non-standard values (`max`, `minimal`, ...) map onto
  the nearest valid level with a warning; unrecognized ones are dropped with a
  warning and left as a frontmatter comment.
- opencode's `temperature`/`top_p`: `config.Agent` has no such fields. The
  import neither rejects the file nor invents a field — the value stays as a
  frontmatter comment and the import reports a warning.
- Tool names with no counterpart (`WebSearch`, for instance) are not dropped
  silently on import — they are reported as a warning naming the tool. On a
  normal load of `.sennit/agents`, the Claude Code name translation
  (`Read`→`read` and so on, `ClaudeToolNames` in `agents_markdown.go`) no
  longer applies at all: files in Sennit's own directory are expected to name
  tools Sennit's way already. The translator was not deleted — it is used by
  the import only.
- Delegation is single-level: a role cannot call another role.

## The agent's continuation/dispatch tests were intermittently red under `-race` in CI — resolved, verdict below

Closed 2026-08-22. The two suspects the original entry named —
`dispatchDecision`'s `Continuation` branch and the steering-follow-up RunID
drop — are **ruled out** as the cause: `Continuation` is set true in exactly
one place (`startContinuation`), on a call carrying only a fixed placeholder
prompt, never a real user's; grep confirms no other constructor sets it. The
steering branch does not discard content either — it clears the queued
call's `RunID` and still enqueues the call itself, which `foldSteering`
persists via `createUserMessage` like any other queued follow-up. Neither
branch can lose a real user prompt.

Tracing every exit from `runTurn` (`internal/agent/agent.go`) turned up a
different, real bug with the same silent-discard shape: `reporter :=
newCompletionReporter(...)` and the deferred call that publishes
`notify.RunComplete` through it used to be constructed *after*
`a.sessions.Get`, `a.getSessionMessages`, and `a.createUserMessage` — so a
failure in any of those three returned before any reporter existed at all.
The prompt vanished with no persisted message and no terminal event, which
is exactly the `userCount == 0` / no-RunComplete shape the CI failure
showed. Fixed by constructing the reporter and registering its deferred
publish immediately once a call becomes the active run, before that
fallible setup runs, so every exit publishes a terminal event — a no-op via
`completionReporter`'s own `sync.Once` wherever `finishTurn` already spent
it explicitly. Proven with
`TestRun_CreateUserMessageFailurePublishesTerminalEvent`
(`internal/agent/run_early_failure_test.go`), which fails against the
pre-fix code (times out waiting for a `RunComplete` that never arrives) and
passes after it. `dispatchDecision`'s two drop branches were also given a
`slog.Debug` line each, so a discard leaves a trace even in the cases this
investigation could rule out.

Not settled: whether this specific gap is *the* cause of the two CI
failures on record (`TestSendToParent_AndCompletionBothSurviveSameDrain`,
`TestDeliverTaskCompletion_RaceWithUserPromptStartsOnlyOneTurn`). Neither
failure's own model ever returns an error, so the natural trigger would be
a transient SQLite failure under CI's slower/differently-loaded runners
(`session.Get`/`createUserMessage` against a real DB) — plausible, not
reproduced. If the `race` job flakes again on this same shape (queue
drained, session idle, nothing persisted, no terminal event), re-open with
the new run's log; if it flakes on a *different* shape, this entry's
reasoning about the two original suspects still stands and does not need
repeating.

## Windows and macOS CI: what the real logs turned out to be

The three-OS `build` matrix (added 2026-08-21) was red on two legs. Two rounds of
work, 2026-08-22. Round one reasoned from static analysis, fixed real bugs, and
still left 81 Windows failures; round two read the actual runner log and found the
dominant causes had nothing to do with path spelling.

**Round one -- production, path canonicalization.** On Windows the same directory
arrives under two spellings: `t.TempDir()` yields the 8.3 short form
(`C:\Users\RUNNER~1\...`) while git and `filepath.Abs` yield the long one, so a raw
`==` says they differ. Added `fsext.Canonical` and used it at three sites:
`threadspawn/attach.go`'s repo-root check -- which silently left a Windows user
standing at their own repo root with **no thread manager** -- `db/connect.go`'s
pool key, and `fsext/lookup.go`'s stop-at-home checks. Pinned by symlink tests,
which exercise the same aliasing class on Linux. `home.Long` separator mixing and
the `confinement_test.go` hand-built JSON (which meant the confinement boundary was
never exercised on Windows at all) were fixed in the same round.

**Round two -- the 56-failure cluster was never about paths.** Two distinct
resource-lifetime bugs, both invisible on Linux because it happily unlinks open
files and removes directories that are somebody's cwd:

- `t.Cleanup` is LIFO, and several tests registered `t.Cleanup(ResetPool)` *before*
  `t.TempDir()`, so the directory was removed while the SQLite handle was open.
- `ResolveCwd` (`internal/cmd/root.go`) does a process-global `os.Chdir` into
  `--cwd` and never returns -- correct for the CLI, but it left the test process
  sitting inside a `t.TempDir()` that a later cleanup wanted to remove.

**Round two -- production, a leaked workspace flock.** `Bootstrap` registered a
final-cleanup closure calling `wsLock.Release()`, then set `wsLock = nil` on the
next line to disarm an earlier failure-path defer. Closures capture by reference,
so the cleanup released a **nil** lock -- a documented no-op -- and the OS flock on
`sennit.lock` was never dropped. Masked on Linux by process exit; it mattered for a
second `Bootstrap` of the same data directory inside one process.

**Round two -- production, `NewManager` mis-anchored absolute worktree dirs.** It
used `filepath.IsAbs`, not the codebase's `filepathext.SmartIsAbs`, so a config
`WorktreeDir` written Unix-style (`/var/tmp/...`, legal and portable in a config
file) was treated as relative on Windows and silently anchored under the repo.

**Round two -- production, truncation dropped the file name.** `IsLikelyPath`
excluded backslash as a shell metacharacter, so every Windows path failed the check
and fell through to plain right-truncation instead of `TruncatePath`'s head elision
-- cutting away the one part of a path that identifies it.

**Round two -- production, config staleness race.** Closed by holding `stalenessMu`
across the write *and* the snapshot refresh; the comment in `SetConfigFields`
records the bounded-latency trade-off this accepts.

**macOS -- production, keybindings leaked into golden files.** `keys.go` rewrites
`ctrl+` to `super+` on darwin, and `ui.go` passed `runtime.GOOS` straight into
`configuredKeyMap`, so every golden test rendered the host's key hints. The
platform is now an injectable field defaulting to `runtime.GOOS`; goldens pin
`"linux"`. No golden file content changed.

**The tool that made all of this checkable.** `internal/testenv`'s
`AssertRemovableOnWindows` reproduces the Windows constraint on Linux: it walks
`/proc/self/fd` and checks `os.Getwd()` against the directory about to be removed,
registered right after `t.TempDir()` so LIFO runs it just before `RemoveAll`. It
names the exact fd or cwd Windows would refuse -- on `internal/cmd` it named the
same `\003` directory the Windows log did. Prefer extending it over reasoning about
Windows semantics from a Linux box. The same move was applied a second time to
the watcher cluster below: `describeExternalChange` in `watch_test.go` now prints
the staleness verdict, the tracked path set, and any untracked candidate whenever
one of those tests fails, so the next Windows run reports the cause instead of
costing another round of reasoning.

**Round three -- `TestApplyWorkspaceConfig`, fixed.** The round-two diagnosis was
right: removing `workspaceDir` and writing a *file* there makes the subsequent
open fail with ENOTDIR on Unix (not `os.IsNotExist`, so `applyWorkspaceConfig`
correctly surfaces a real error) but with `ERROR_PATH_NOT_FOUND` on Windows, which
Go's `syscall.Errno.Is` maps to `fs.ErrNotExist` -- indistinguishable there from
"no config here", so the provocation never fires and `require.Error` aborts the
subtest. Because the trailing `os.Remove(workspaceDir)` sat after that assertion,
it never ran, and the leftover file broke every later subtest's
`os.MkdirAll(workspaceDir, ...)` -- one platform gap fanning out to four failures.
Fixed by moving cleanup into `t.Cleanup` (so it runs regardless of whether the
assertions pass) and skipping the subtest on Windows with a comment naming the
exact errno mapping; no portable provocation produces a genuine non-not-exist read
error identically on both platforms. Unix coverage is unchanged.

**Round three -- the external-change watcher, still open on Windows only.**
`TestWatchForExternalChanges_IgnoresOwnWrites[_TightPoll]` and
`TestExternalChangeDetected_NewCandidateFile`. The round-two `stalenessMu` fix
(holding the write and the snapshot refresh under one lock) is confirmed still
necessary and still correct -- reverting it reliably fails `_TightPoll` under
`-race` on Linux -- but all three tests still fail on Windows CI, and this round
could not find a further static cause. Ruled out, with reasoning: `os.Stat` on
Windows (`GetFileAttributesEx`, see `stat_windows.go`) is not the
`FindFirstFile`-directory-enumeration path that has the well-known lazy
last-write-time cache, so the snapshot's own re-stat of the path it just wrote
should be accurate; `os.Rename`/`MoveFileEx` does not reset a file's write time,
so the atomic-rename identity change (old file replaced by the temp file's inode)
should not perturb size or mtime either. Both leads the task brief asked to check
came back "should be fine" rather than "found the bug" -- which is not the same as
ruled out with certainty, since neither claim can actually be exercised on this
Linux box. `TestExternalChangeDetected_NewCandidateFile` is the stranger of the
three: it fails at the *first* assertion (`require.False(t,
store.externalChangeDetected())`), before the test writes anything at all, so it
cannot share the own-write race with the other two. Tracing every path both
`Load` and `externalChangeDetected` touch (`lookupConfigs`, `globalConfigPaths`,
`GlobalConfig`/`GlobalConfigData`, `worktreeRoot`/`projectBoundary`,
`ConfigStaleness`, `agentFilesChanged`) found no GOOS-conditional branch reachable
under this test's setup (env-var overrides for both global paths, no git repo at
`t.TempDir()`) that would make two back-to-back calls with no intervening
filesystem change disagree. All three tests pass reliably on Linux under `-race
-count=10`, consistent with a genuinely Windows-only cause rather than a
timing-sensitive test. Left unfixed rather than guessed at, three rounds running
now -- this needs an actual Windows box: instrument `externalChangeDetected` (or
temporarily log `ConfigStaleness().Changed/Missing/Errors` and the tracked-vs-
candidate set diff) on a real Windows CI run rather than reasoning further from
Linux.

**Round four -- production, the watcher fired forever on Windows.** `systemConfigPath`
is `""` on Windows by design (no system-wide config), and `globalConfigPaths()`
returned it as a list element regardless. `externalChangeDetected` then ran every
candidate through `filepath.Abs`, and `filepath.Abs("")` returns the process's
working directory with no error -- which is never a tracked config path, so the
function returned true unconditionally. `WatchForExternalChanges` polls every 2s and
fires `OnExternalChange` on true, so every Windows user got a permanent 2-second
loop of disk reload, MCP re-init, and `ConfigChanged`. `CaptureStalenessSnapshot`
skipped empty paths; the asymmetry between two consumers of one list was the bug.
Fixed at the source (empties never enter the list) plus a guard in the extracted
`hasUntrackedCandidate`, and `isGlobalConfigPath("")` no longer reports true via
`filepath.Clean("") == "."`.

This one is the payoff for the self-diagnosing failure output: three rounds of
static reasoning missed it, and `describeExternalChange` named it in one run by
printing `untracked candidates=["D:\a\sennit\sennit\internal\config"]` -- the
test process's own cwd, which nothing in the test had created.

**Round four -- the `smartIsAbs` seam did not actually work.** The GOOS-parameterised
core still called `filepath.IsAbs`, whose own judgment follows the *build's* GOOS,
so `smartIsAbs("linux", "/var/tmp")` on a real Windows run got Windows rules and the
cross-platform test proved nothing. Now `isAbsFor(goos, path)` decides: it delegates
to `filepath.IsAbs` when goos is the host, so production keeps exact stdlib
semantics (reserved device names, degenerate UNC, `\\?\` paths), and uses a
hand-rolled rule only when asked what the *other* platform would say.
`TestIsAbsFor_WindowsRules` pins that rule from Linux -- it was added because a
mutation disabling the UNC branch left the suite green.

**Latent, not touched:** `filetracker`'s `filepath.Rel(s.workingDir, path)` has the
same spelling sensitivity, but it was not in the failure list and widening scope on
suspicion was not worth it.

**Round five (2026-08-29) -- a test that killed its own fake server.** The
macOS leg went red twice on the same commit, on
`TestClient_FailedCandidateCleanupCannotBlockKillOrShutdown`. The scenario needs
a server that is alive and has stopped reading stdin, so the caller's write
blocks on pipe back-pressure; the fake server did it with `select {}`, which
parks the only goroutine that process has -- the exact condition the runtime's
deadlock detector panics on. Whether it fires depends on what else happens to be
runnable, so the server sometimes went quiet and sometimes died, and a dead
server closes the pipe, which makes the write return at once. The failure then
reads as "large send completed", i.e. the opposite of what was being set up.

Same lesson as round two, one layer down: the test was marked unix-only because
Windows showed this symptom, and the symptom was read as a platform property.
It was the test. Confirmed by removing the skip and running it: with the fake
server fixed, the Windows leg passes, so the wedge holds on all three platforms
and the skip had been standing in for a bug in the test file. The failure
message now also reports whether the candidate process is still alive --
"connection is closed" reads identically whether the send raced a transport we
closed or the server died holding its end of the pipe, and those need different
fixes.
