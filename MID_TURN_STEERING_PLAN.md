# План mid-turn steering и фоновых субагентов

## Цель

Дать пользователю возможность отправлять уточнения, дополнительные требования
и просьбы обновить todo, пока главный агент выполняет tool call или ждёт
субагента.

Целевое поведение состоит из двух уровней:

1. **Mid-turn steering**: сообщение принимается во время активного turn и
   попадает в контекст главного агента перед ближайшим следующим обращением к
   модели.
2. **Фоновая делегация**: субагент продолжает работу независимо, а главный
   агент получает управление сразу после запуска и может обрабатывать новые
   инструкции, не дожидаясь результата делегации.

Первый уровень нужно реализовать отдельно и выпустить раньше второго. Он уже
частично поддерживается текущим dispatcher, поэтому даёт полезное поведение с
меньшим риском. Второй уровень требует отдельного lifecycle для фоновых задач и
протокола доставки позднего результата.

## Текущее поведение

### Что уже есть

- `sessionAgent.Run` сериализует выполнение по `sessionID`. Новый вызов для
  занятой сессии попадает в `dispatcher.messageQueue`:
  `internal/agent/agent.go:403-420`.
- `runTurn.prepareStep` забирает queued prompts без `RunID`, создаёт для них
  user messages и добавляет их в следующий model step:
  `internal/agent/turn.go:87-118`.
- Запросы с `RunID` не складываются внутрь чужого turn, а выполняются отдельными
  turns. Это необходимо сохранить для `braid run` и других клиентов, ожидающих
  собственный `RunComplete`.
- Backend/client-server путь уже принимает сообщения асинхронно:
  `Backend.SendMessage` резервирует accepted run, запускает goroutine и сразу
  возвращает управление клиенту.
- `thread_create` уже является полноценной фоновой моделью выполнения, но
  создаёт отдельный git worktree и поэтому не подходит как замена обычному
  лёгкому субагенту.

### Что мешает нужному UX

- Локальный `AppWorkspace.AgentRun` синхронно ждёт
  `AgentCoordinator.Run`. Пока его `tea.Cmd` не завершён,
  `editor.pendingSendActive` остаётся `true`, а последующие сообщения остаются
  во внутренней UI-очереди и не доходят до agent dispatcher.
- Обычный `agent` и пользовательские agent tools синхронно вызывают
  `runSubAgent`; tool result возвращается только после полного завершения
  дочернего `SessionAgent.Run`.
- Поэтому foreground-субагент блокирует следующий model step главного агента.
  Даже если сообщение уже принято backend-ом, модель увидит его только после
  возврата субагента.
- Нет task registry для лёгких фоновых делегаций, идентификаторов задач,
  completion notifications и явного управления cancel/wait/result.

## Семантика, которую нужно зафиксировать

### Steering message

- Интерактивное сообщение без `RunID`, отправленное во время активного turn,
  является steering message этого turn.
- Оно сохраняется как обычное user message ровно один раз перед следующим
  model call.
- Оно не запускает второй конкурентный model stream для той же сессии.
- Несколько steering messages доставляются FIFO.
- Attachments сохраняются и доставляются вместе с соответствующим сообщением.
- Если текущий turn завершился до следующего model step, сообщение становится
  следующим обычным turn, а не теряется.
- `Cancel` удаляет только те queued messages, которые были приняты до cancel;
  существующая семантика `acceptSeq` должна сохраниться.
- Сообщения с непустым `RunID` всегда сохраняют отдельный lifecycle и отдельный
  `RunComplete`.

### Background delegation

- Запуск фонового субагента сразу возвращает главному агенту `task_id`, статус
  и идентификатор child session.
- Завершение не пытается дописать поздний tool result к уже закрытому tool
  call. Вместо этого оно создаёт отдельное task-completion событие, которое
  доставляется главному агенту на безопасной границе между model steps.
- Результат, ошибка и cancel доставляются не более одного раза.
- Фоновая задача принадлежит workspace и parent session, а не HTTP request или
  отдельному `tea.Cmd`; закрытие workspace отменяет задачу и дожидается её
  остановки в пределах shutdown timeout.
- Главный агент и субагент не должны одновременно менять одни и те же файлы без
  явной изоляции. На первом этапе фоновые делегации, способные редактировать
  файлы, должны быть opt-in; безопасный default для background mode —
  read-only/research agents либо `isolation: worktree` в будущем.

## Этап 1. Нормализовать асинхронную отправку из TUI

- [ ] Вынести fire-and-forget dispatch из `Backend.SendMessage` в общий
  workspace/app-level компонент, чтобы local и client-server режимы
  использовали одинаковый accept protocol.
- [ ] Перед запуском goroutine выполнять `agent.ValidateCall` и
  `BeginAccepted`, чтобы `Cancel` не терял запрос в окне между submit и входом
  в `SessionAgent.Run`.
- [ ] Привязать goroutine локального режима к `App.globalCtx` и учитывать её в
  shutdown wait group. Не использовать `context.Background()` как lifetime
  owner.
- [ ] Сделать `AppWorkspace.AgentRun` возвращающимся после принятия сообщения,
  а не после завершения turn.
- [ ] Унифицировать доставку runtime errors: локальный путь должен публиковать
  `notify.TypeAgentError` и, при наличии `RunID`, надёжный `RunComplete` так же,
  как `backend.runAgent`.
- [ ] После accepted response сразу сбрасывать `pendingSendActive`, чтобы
  следующий Enter отправлял сообщение в agent dispatcher, а не ждал окончания
  предыдущего turn в UI queue.
- [ ] Оставить `pendingSendQueue` только для сериализации session creation/load
  и коротких submit requests, а не для сериализации полного agent run.

Основные файлы:

- `internal/backend/agent.go`;
- `internal/app/app.go`;
- `internal/workspace/app_workspace.go`;
- `internal/ui/model/ui.go`;
- `internal/ui/model/editor.go`.

### Критерии готовности этапа 1

- Local и client-server TUI принимают второй prompt, пока первый turn занят.
- Второй prompt появляется в `AgentQueuedPromptsList` без ожидания завершения
  первого `AgentRun`.
- Быстрый submit → Escape не теряет cancel.
- Shutdown не закрывает DB, пока принятые run goroutines ещё используют её.

## Этап 2. Довести mid-turn inbox до явного контракта

Текущую `messageQueue` следует оставить единственным источником queued user
input, но отделить API steering от общего повторного `Run`.

- [ ] Добавить в `Coordinator`/`SessionAgent` явный метод принятия
  интерактивного follow-up, например `Steer(SessionAgentCall)`, который
  атомарно выбирает: enqueue в активный turn либо запуск нового turn, если
  сессия уже освободилась.
- [ ] Не дублировать очередь в UI, coordinator и agent. Источником истины после
  accepted submit остаётся `dispatcher.messageQueue`.
- [ ] Сохранить текущую развилку:
  interactive/no `RunID` → fold в `prepareStep`; `RunID` → отдельный turn.
- [ ] Сделать drain FIFO и создание user messages одной логической операцией:
  при ошибке persistence нельзя молча потерять оставшуюся часть batch.
- [ ] Явно отметить persisted steering messages как часть активного turn для
  диагностики, не меняя их пользовательскую роль в истории.
- [ ] Проверить порядок `tool result → steering user message → следующий
  assistant step` для Anthropic, OpenAI Responses, Gemini и OpenAI-compatible
  providers. Сообщение нельзя вставлять между tool call и обязательным tool
  result, если provider требует их смежности.
- [ ] Если provider-протокол не принимает user message сразу после tool result
  в текущем step, нормализовать порядок в fantasy/Braid adapter, не подменяя
  steering системным prompt.
- [ ] Добавить событие queue changed вместо опоры только на polling TTL, чтобы
  queue pill обновлялся сразу после enqueue/drain/cancel.

Рекомендуемый порядок сообщений перед следующим model call:

```text
assistant(tool_call)
tool(tool_result)
user(steering message 1)
user(steering message 2)
assistant(next model step)
```

Основные файлы:

- `internal/agent/dispatch.go`;
- `internal/agent/agent.go`;
- `internal/agent/turn.go`;
- `internal/agent/coordinator.go`;
- `internal/message/`;
- при необходимости `charm.land/fantasy` или локальный adapter;
- `internal/ui/model/workspace_cache.go`.

### Критерии готовности этапа 2

- Сообщение, отправленное во время Bash/tool/subagent foreground execution,
  видно модели на первом model call после завершения текущего tool batch.
- Просьба «добавь ещё один todo» приводит к `todos` tool call до продолжения
  старого плана, если модель следует инструкции.
- Два follow-up сообщения сохраняют FIFO и не объединяются в одну строку.
- Follow-up, пришедший на границе завершения turn, выполняется ровно один раз:
  либо внутри текущего turn, либо следующим turn.

## Этап 3. Ввести lifecycle фоновых делегаций

### Task registry

- [ ] Добавить workspace-scoped manager лёгких agent tasks, не связанный с git
  worktrees. Он хранит `task_id`, parent session, child session, agent type,
  status, timestamps, result/error и cancel function.
- [ ] Использовать состояния `pending`, `running`, `completed`, `failed`,
  `cancelled`; переход в terminal state должен быть атомарным и одноразовым.
- [ ] Ограничить число одновременных задач на workspace и на parent turn;
  лишние задачи держать в bounded queue либо отклонять понятной ошибкой.
- [ ] Зарегистрировать manager в shutdown lifecycle и отменять все задачи при
  закрытии workspace.
- [ ] Сохранить child sessions и существующую агрегацию стоимости в parent
  session. Повторная обработка completion не должна удваивать cost.

### Agent tool API

- [ ] Расширить параметры built-in и custom agent tools режимом выполнения:
  `foreground | background`. Для обратной совместимости сначала оставить
  `foreground` default и разрешить модели явно выбрать `background`.
- [ ] В background mode создавать child session и запускать subagent через task
  manager, немедленно возвращая структурированные metadata:
  `task_id`, `child_session_id`, `status`.
- [ ] Добавить tools `task_list`, `task_wait`, `task_result`, `task_cancel` либо
  расширить существующий единый task tool. Не смешивать их с git-based
  `thread_*`: у механизмов разная изоляция и lifecycle.
- [ ] После стабилизации рассмотреть background default для read-only agents.
  Для modifying agents default менять только после появления конфликтной
  защиты или worktree isolation.

Основные файлы и новые зоны:

- `internal/agent/agent_tool.go`;
- `internal/agent/custom_agent_tool.go`;
- `internal/agent/subagents.go`;
- новый `internal/agenttask/` либо `internal/task/`;
- новые tools в `internal/agent/tools/`;
- wiring в `internal/app/` и `internal/backend/`.

## Этап 4. Доставлять completion фоновой задачи главному агенту

- [ ] Добавить отдельный per-session inbox для внутренних событий. Не класть
  completion в user prompt queue как будто его написал пользователь.
- [ ] Представлять completion структурированным internal message с полями
  `task_id`, agent, status, child session и summary/error.
- [ ] В `prepareStep` атомарно дренировать сначала завершённые task events,
  затем steering messages, сохраняя provider-valid ordering после всех tool
  results предыдущего step.
- [ ] Разбудить главный agent loop при completion, если он сейчас idle. Это
  должен быть новый internal continuation turn, а не конкурентный `Run` той же
  сессии.
- [ ] Если главный turn активен, только enqueue event; текущий model stream не
  прерывать.
- [ ] Если пользователь отменил parent session, terminal event сохранить для
  UI/history, но не запускать автоматический continuation до нового user turn.
- [ ] Ограничить размер результата: в parent context передавать summary и
  ссылку на child session; полный transcript остаётся в child session.
- [ ] Гарантировать at-most-once injection с sequence/idempotency marker.

Рекомендуемый поток:

```text
main model -> agent(background=true)
           <- {task_id: "...", status: "running"}
main model -> продолжает tools / обрабатывает steering
subagent   -> завершает child session
task inbox <- completion(task_id, summary)
main loop  -> ближайший safe model step или internal continuation
```

## Этап 5. UI и управление задачами

- [ ] Отличать foreground delegation от background task в agent tool renderer.
- [ ] Для фоновой задачи показывать task ID, agent name, elapsed time, status и
  ссылку на child session без бесконечного spinner у уже завершённого стартового
  tool call.
- [ ] Добавить панель/диалог активных tasks с cancel и переходом в child
  session.
- [ ] Показывать steering messages как queued, пока они не persisted и не
  включены в model step; после drain удалять queue pill без polling delay.
- [ ] Escape должен отменять активный main turn, но не все фоновые tasks.
  Отдельное действие должно отменять выбранную task; массовая отмена требует
  подтверждения.
- [ ] При просмотре child session сохранить текущий read-only режим, пока не
  появится отдельный безопасный протокол `send task follow-up`.

Основные файлы:

- `internal/ui/chat/agent.go`;
- `internal/ui/chat/tools.go`;
- `internal/ui/model/ui.go`;
- `internal/ui/model/workspace_cache.go`;
- новый dialog/model для task list при необходимости.

## Этап 6. Тестирование

### Agent и dispatcher

- [ ] Follow-up во время заблокированного foreground tool попадает в ближайший
  `PrepareStep`.
- [ ] Follow-up во время foreground subagent доставляется после его tool result.
- [ ] Гонка enqueue с завершением turn не создаёт duplicate/lost prompt.
- [ ] FIFO для нескольких prompts и attachments.
- [ ] `RunID` prompt остаётся отдельным turn и публикует свой `RunComplete`.
- [ ] Cancel before accept, while queued, during tool и after completion.
- [ ] Persistence failure не удаляет недоставленные inbox entries.

### Background tasks

- [ ] Старт возвращает task metadata до завершения gated subagent.
- [ ] Главный agent выполняет следующий model step, пока subagent gated.
- [ ] Completion будит idle parent ровно один раз.
- [ ] Completion во время активного parent turn ждёт safe boundary.
- [ ] Error/cancel/shutdown дают terminal state и не оставляют goroutines.
- [ ] Cost child session добавляется parent session ровно один раз.
- [ ] Concurrency limit и bounded queue работают детерминированно.
- [ ] Race tests под `go test -race` для registry, inbox, cancel и shutdown.

### Workspace и UI

- [ ] Одинаковые integration tests для `AppWorkspace` и `ClientWorkspace`:
  второй Enter принимается, пока первый run активен.
- [ ] `pendingSendActive` отражает только submit request, а не lifetime turn.
- [ ] Queue/task status обновляется событиями без синхронного HTTP из `Update`.
- [ ] Golden tests для queued steering, running background task, completed,
  failed и cancelled states.

## Этап 7. Наблюдаемость и rollout

- [ ] Добавить structured logs: enqueue/drain steering, task start/finish,
  completion injection, wakeup и duplicate suppression. Не логировать полный
  пользовательский prompt по умолчанию.
- [ ] Добавить метрики времени от submit до injection и от task completion до
  parent acknowledgement.
- [ ] Сначала включить асинхронный local submit и steering без feature flag.
- [ ] Background delegation выпустить за config option, например
  `options.background_agents`, пока не пройдены provider и race suites.
- [ ] После стабилизации включить explicit `background` mode по умолчанию, но не
  менять foreground semantics существующих agent definitions без migration
  note.
- [ ] Задокументировать разницу между queued steering, background agent task и
  isolated git thread.

## Порядок реализации

1. Асинхронный submit в local workspace и parity с backend.
2. Integration tests доставки follow-up через существующий `prepareStep`.
3. Явный steering API, события queue changed и provider-ordering tests.
4. Workspace-scoped task registry с lifecycle/cancel/shutdown.
5. Background mode для built-in `agent`, затем для custom agents.
6. Task completion inbox и internal continuation turns.
7. UI task management, ограничения modifying agents и постепенный rollout.

## Definition of Done

- Пользователь может отправить уточнение в любой момент активного turn, и оно
  учитывается на ближайшем допустимом model step без запуска конкурентного
  stream той же сессии.
- Пользователь может запустить субагента в background mode, продолжать диалог с
  главным агентом и позже получить результат субагента в том же parent context.
- Local и client-server режимы имеют одинаковую семантику submit, cancel,
  queue и completion.
- Ни один accepted prompt или task completion не теряется и не применяется
  дважды при гонках, cancel, reconnect или shutdown.
- `go test ./...`, targeted integration tests и `go test -race` для изменённых
  concurrency-компонентов проходят.
