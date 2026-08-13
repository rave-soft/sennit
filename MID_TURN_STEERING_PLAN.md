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

## Границы плана

Этапы 1–2 (mid-turn steering) выпускаются как самостоятельная фича и релизятся
раньше остального. Этапы 3–7 (background delegation) — подтверждённая цель, а
не опция: лёгкая фоновая делегация без git worktree нужна, потому что у тредов
другая стоимость запуска. Но строится она не рядом с `thread.Manager`, а из
него: см. этап 3. Разделение на этапы здесь про порядок работ и риск, а не про
необходимость фичи.

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
  лёгкому субагенту. При этом `thread.Manager` — готовый lifecycle фоновых
  делегаций (статусы, события, persistence, `Recover`, `Shutdown`, `Send`), и
  строить второй такой рядом не нужно; см. этап 3.
- Результат треда сегодня не попадает в контекст родительского агента:
  `onRunComplete` пишет его в store и публикует событие, а родитель обязан
  опрашивать `thread_status` или блокироваться в `thread_wait`.

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
- Фоновая задача не переживает рестарт процесса, но её состояние переживает:
  задачи персистятся наравне с тредами, и общий `Recover` при старте переводит
  задачи в нетерминальных состояниях в `interrupted`. Ни одна делегация не
  остаётся показанной как выполняющаяся, и child sessions не теряют владельца.

### Permissions фоновой задачи

Это блокирующий вопрос этапа 3, а не деталь UI. Фоновый субагент упрётся в
permission request в момент, когда главный агент стримит ответ, и текущая модель
разрешений на такую ситуацию не рассчитана.

Решение: запрос фоновой задачи показывается пользователю как обычный permission
prompt в parent session, с явной пометкой задачи-источника.

- Auto-approve (`Permissions.AutoApproveSession`) как default для background
  недопустим: он снимает единственную защиту ровно там, где пользователь не
  видит происходящего.
- Prompt содержит `task_id` и agent name, чтобы пользователь понимал, что
  запрос пришёл не от текущего видимого turn.
- Запросы от нескольких фоновых задач сериализуются в одну FIFO-очередь на
  parent session и показываются по одному. Запрос главного агента имеет
  приоритет над фоновыми: пользователь не должен ждать ответа по фоновой
  задаче, чтобы продолжить основной диалог.
- Пока пользователь не ответил, задача остаётся `running` и удерживает слот
  concurrency limit. Отказ переводит задачу в terminal state с понятной
  причиной.
- Timeout на ответ отсутствует: задача ждёт сколько угодно. Молчание
  пользователя не должно уничтожать уже проделанную работу, а автоматический
  отказ по таймеру — это именно уничтожение результата без его ведома. Выход из
  ожидания только явный: ответ на prompt либо отмена задачи. Цена решения —
  занятый слот concurrency, поэтому ожидающая задача должна быть хорошо видна в
  task-панели.
- Read-only default для background mode сохраняется, но обосновывается теперь
  file-conflict, а не отсутствием permission-механизма.

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

## Этап 1.5. Спайк: доставка steering в одном блоке с tool result

Выполняется до начала этапа 2 и до оценки его сроков.

Целевая форма доставки: steering-текст едет **в том же user message, что и
tool result**, отдельным текстовым блоком рядом с `tool_result`, а не следующим
самостоятельным `user` message. У Anthropic `tool_result` и так является
content block внутри `user` message, поэтому проблема «можно ли ставить user
после tool result» исчезает по построению, а не решается нормализацией. Так же
устроена доставка mid-turn сообщений в Claude Code.

Это меняет и предмет спайка: проверять нужно не порядок сообщений, а
поддержку текста рядом с tool result в одном блоке.

- [ ] Прогнать реальный запрос, где user message содержит `tool_result` и
  текстовый блок одновременно, против Anthropic, OpenAI Responses, Gemini и
  OpenAI-compatible providers.
- [ ] Зафиксировать результат таблицей provider → поддерживает совмещение →
  требуемый fallback.
- [ ] Для providers без поддержки совмещения определить fallback (отдельный
  `user` message после всех tool results) и его место — fantasy adapter либо
  локальный слой — до написания кода этапа 2.
- [ ] Проверить, что при нескольких tool results в одном step steering
  прикрепляется ровно к одному из них, а не дублируется по каждому.

Спайк одноразовый и не требует продакшн-качества: цель — знание, а не код.

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
- [ ] Прикреплять drained steering к user message с tool results текущего step
  вместо добавления отдельного сообщения в конец `prepared.Messages`. Fallback
  для providers без поддержки совмещения — по результатам этапа 1.5. Steering
  ни при каких обстоятельствах не подменяется системным prompt.
- [ ] Сохранять steering в истории как обычное user message независимо от того,
  каким блоком он был доставлен модели. Форма доставки — деталь протокола, а не
  изменение роли сообщения в истории.
- [ ] Зафиксировать взаимодействие steering с авто-суммаризацией и компакцией
  контекста. Fold происходит в `prepareStep`, там же рядом живёт
  auto-summarize: нужно гарантировать, что steering-сообщение, попавшее в шаг,
  который затем суммаризуется, отменяется или падает с ошибкой, остаётся в
  истории ровно один раз — не теряется и не дублируется при повторном шаге.
- [ ] Добавить событие queue changed вместо опоры только на polling TTL, чтобы
  queue pill обновлялся сразу после enqueue/drain/cancel.

Целевая форма запроса перед следующим model call:

```text
assistant(tool_call)
user[ tool_result, text(steering message 1), text(steering message 2) ]
assistant(next model step)
```

Fallback для providers, не принимающих текст рядом с tool result:

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
- Steering-сообщение, попавшее в шаг с последующей авто-суммаризацией или
  ошибкой шага, присутствует в истории ровно один раз.
- Форма доставки принимается всеми провайдерами из таблицы этапа 1.5: основная
  там, где совмещение поддерживается, fallback — где нет.
- Steering, доставленный блоком рядом с tool result, сохранён в истории как
  обычное user message и отображается в UI как сообщение пользователя.

Этапы 1–2 релизятся здесь как самостоятельная фича, после чего работа
продолжается этапом 3.

## Этап 3. Ввести lifecycle фоновых делегаций

### Отправная точка: `internal/thread/`

Второй registry писать не нужно — он уже написан. `thread.Manager` это
workspace-scoped менеджер фоновых делегаций со всем, что перечисляет этот этап:
статусы и атомарные переходы (`setStatus`), события (`Subscribe`),
persistence (`store`), восстановление после рестарта (`Recover`), shutdown,
follow-up в работающую делегацию (`Send`), сериализация операций
(`beginOp`/`control`). Отличие треда от лёгкой фоновой задачи — только git
worktree и merge-политика поверх этого lifecycle.

Поэтому целевая модель: **один lifecycle, две конфигурации**. Тред — это задача
с worktree и merge-политикой; лёгкая задача — та же задача без них. Разделять
их надо по свойствам изоляции, а не по отдельным подсистемам, иначе получаются
два набора одних и тех же гонок, два shutdown-пути и два места, где чинить один
и тот же баг.

- [ ] Выделить из `thread.Manager` общий lifecycle: идентификатор, parent
  session, child session, agent type, статус, timestamps, результат/ошибка,
  cancel, события, persistence, recovery, shutdown, send.
- [ ] Оставить worktree, merge-политику и merge-специфичные статусы
  (`merging`, `merged`, `conflict`, `merge_blocked`) как надстройку варианта
  «тред», а не как часть общего ядра.
- [ ] Провести рефакторинг так, чтобы поведение существующих тредов не
  изменилось: тесты `internal/thread/` и `internal/workspace/threads_*_test.go`
  должны пройти без правок ожиданий.

### Общий task registry

- [ ] Ввести лёгкую задачу как конфигурацию общего lifecycle без worktree и без
  merge-политики.
- [ ] Использовать общий набор состояний: `pending`, `running`, `completed`,
  `failed`, `cancelled`/`interrupted`; переход в terminal state атомарен и
  одноразов. Не заводить для задач собственную параллельную номенклатуру
  статусов.
- [ ] Персистить задачи так же, как треды, и обрабатывать их в общем `Recover`:
  при старте активные задачи переводятся в `interrupted`. In-memory registry
  отвергнут сознательно — иначе два похожих механизма ведут себя по-разному
  после краша, и худшее поведение достаётся новому.
- [ ] Ограничить число одновременных задач на workspace и на parent turn;
  лишние задачи держать в bounded queue либо отклонять понятной ошибкой.
  Проверить, как этот лимит соотносится с уже существующей сериализацией
  операций в `beginOp`/`control`.
- [ ] Сохранить child sessions и существующую агрегацию стоимости в parent
  session. Повторная обработка completion не должна удваивать cost.
- [ ] Реализовать permission prompt фоновой задачи согласно разделу
  «Permissions фоновой задачи»: FIFO-очередь на parent session, пометка
  источника, приоритет запросов главного агента, удержание слота concurrency
  на время ожидания ответа. Политика общая для тредов и задач.
- [ ] Выяснить, что происходит с permission request внутри треда вне
  YOLO-режима сегодня: `LocalSpawner` пробрасывает `parentYOLO` в
  `app.Bootstrap`, но всплывает ли запрос к пользователю — не проверено. Если
  нет, это существующий пробел, который общая политика должна закрыть для обоих
  механизмов сразу.

### Agent tool API

- [ ] Расширить параметры built-in и custom agent tools режимом выполнения:
  `foreground | background`. Для обратной совместимости сначала оставить
  `foreground` default и разрешить модели явно выбрать `background`.
- [ ] В background mode создавать child session и запускать subagent через task
  manager, немедленно возвращая структурированные metadata:
  `task_id`, `child_session_id`, `status`.
- [ ] Добавить отдельные tools `task_list`, `task_output`, `task_result`,
  `task_cancel`, `task_send` — не единый tool с полем `action`: раздельные
  схемы параметров модель путает заметно реже. Инструменты остаются отдельными
  от `thread_*`, потому что у механизмов разная изоляция и разный контракт для
  модели, даже когда lifecycle под ними общий.
- [ ] `task_send` делать сразу, по образцу `thread_send` (`Manager.Send`):
  протокол follow-up в работающую делегацию уже отлажен, включая респавн
  workspace для прерванной делегации. Откладывать его нет причин.
- [ ] Блокирующего `task_wait` не делать. Результат приходит сам через
  completion inbox, поэтому ожидание в инструменте — чистая потеря: главный
  агент простаивает там, где мог бы работать. Вместо него `task_output` даёт
  чтение частичного вывода работающей задачи без блокировки.
- [ ] После стабилизации рассмотреть background default для read-only agents.
  Для modifying agents default менять только после появления конфликтной
  защиты или worktree isolation — а worktree isolation для задачи это и есть
  тред, отдельный механизм под неё не нужен.

Основные файлы и новые зоны:

- `internal/thread/manager.go`, `types.go`, `store.go`, `events.go` — выделение
  общего lifecycle;
- `internal/thread/agenttool.go` — точка, где уже связаны tool и manager;
- `internal/agent/agent_tool.go`;
- `internal/agent/custom_agent_tool.go`;
- `internal/agent/subagents.go`;
- новые tools в `internal/agent/tools/` рядом с `thread_*.go`;
- `internal/db/threads.sql.go` и миграции — если общий store требует изменения
  схемы;
- wiring в `internal/app/` и `internal/backend/`.

## Этап 4. Доставлять completion фоновой делегации главному агенту

Этап охватывает и треды, и лёгкие задачи. Сегодня `Manager.onRunComplete`
(`internal/thread/manager.go:388`) записывает результат в store и публикует
событие, но в контекст родительского агента не попадает ничего: родитель обязан
опрашивать `thread_status` или блокироваться в `thread_wait`. Блокирующий
`thread_wait` существует именно потому, что inbox'а нет. Как только inbox
появится, он должен обслуживать оба механизма — иначе у двух вариантов одного
lifecycle останутся разные способы вернуть результат.

- [ ] Добавить отдельный per-session inbox для внутренних событий. Не класть
  completion в user prompt queue как будто его написал пользователь.
- [ ] Представлять completion структурированным internal message с полями
  идентификатор делегации, agent, status, child session и финальным ответом
  субагента либо ошибкой.
- [ ] Подключить к inbox переходы в terminal state общего lifecycle, а не
  только лёгких задач: завершение треда доставляется родителю тем же путём.
- [ ] Для тредов доставлять событие на завершении run. Отдельно решить, что
  доставляется по итогам merge-фазы (`merged`, `conflict`, `merge_blocked`):
  это второе terminal-событие той же делегации, и правило at-most-once должно
  учитывать оба.
- [ ] Перевести `thread_wait` в legacy: убрать из дефолтного набора
  инструментов (`internal/config/config.go:978-982`), сохранив сам инструмент
  для сценариев вида «дождаться всех тредов перед merge». Блокирующее ожидание
  перестаёт быть основным способом получить результат.
- [ ] В `prepareStep` атомарно дренировать сначала завершённые task events,
  затем steering messages, сохраняя provider-valid ordering после всех tool
  results предыдущего step.
- [ ] Разбудить главный agent loop при completion, если он сейчас idle. Это
  должен быть новый internal continuation turn, а не конкурентный `Run` той же
  сессии. См. подраздел ниже: это единственное место в системе, где turn
  начинается без пользовательского ввода, поэтому его контракт нужно описать
  целиком, а не одним пунктом.
- [ ] Если главный turn активен, только enqueue event; текущий model stream не
  прерывать.
- [ ] Если пользователь отменил parent session, terminal event сохранить для
  UI/history, но не запускать автоматический continuation до нового user turn.
- [ ] Передавать в parent context финальный ответ субагента целиком — ровно то,
  что foreground-делегация вернула бы как tool result, плюс `task_id` и ссылку
  на child session. Background не должен давать результат хуже foreground.
  Полный transcript по-прежнему остаётся только в child session.
- [ ] Гарантировать at-most-once injection с sequence/idempotency marker.

### Internal continuation turn

Turn без пользовательского ввода затрагивает dispatcher, cancel, cost и UI
одновременно, поэтому контракт фиксируется отдельно.

- Запуск. Continuation стартует только когда сессия не busy и в inbox есть хотя
  бы одно недоставленное terminal-событие. Проверка занятости и переход к
  запуску выполняются под тем же mutex, что и обычный dispatch, иначе
  continuation и пользовательский prompt стартуют одновременно.
- Идентичность. У continuation нет `RunID` пользователя. Он не публикует
  `RunComplete`, которого кто-то ждёт снаружи, но публикует обычные message
  events, чтобы UI отрисовал ответ.
- Cancel. Escape во время continuation отменяет только его. Отменённый
  continuation не возвращает событие в inbox: событие уже доставлено, повторная
  инъекция запрещена контрактом at-most-once. Незавершённость видна
  пользователю по состоянию task в списке.
- Cancel родителя. Если пользователь отменил parent session, continuation не
  запускается; событие ждёт следующего пользовательского turn и доставляется
  через `prepareStep`.
- Cost. Стоимость continuation относится к parent session как обычный turn.
  Агрегация стоимости child session выполняется один раз в момент перехода
  задачи в terminal state, а не в момент инъекции события.
- Каскад. Continuation может запускать новые фоновые задачи, но глубина цепочки
  ограничена жёсткой константой 3 без настройки в config. Задача наследует depth
  от turn, который её породил; completion несёт этот depth в inbox;
  continuation с depth на пределе выполняется, но запуск фоновых задач в нём
  запрещён с явной ошибкой инструмента. Счётчик сбрасывается на каждом
  пользовательском turn. Лимит поднимается позже по фактам эксплуатации, а не
  превращается в очередную опцию. Без ограничения возможен
  бесконечный самоподдерживающийся цикл с неконтролируемыми расходами.
- UI. Continuation отображается как продолжение той же сессии с явной пометкой
  причины («задача X завершена»), а не как сообщение пользователя.

Рекомендуемый поток:

```text
main model -> agent(background=true)
           <- {task_id: "...", status: "running"}
main model -> продолжает tools / обрабатывает steering
subagent   -> завершает child session
task inbox <- completion(task_id, final answer)
main loop  -> ближайший safe model step или internal continuation
```

## Этап 5. UI и управление задачами

- [ ] Отличать foreground delegation от background task в agent tool renderer.
- [ ] Для фоновой задачи показывать task ID, agent name, elapsed time, status и
  ссылку на child session без бесконечного spinner у уже завершённого стартового
  tool call.
- [ ] Показывать задачи и треды в одной панели активных делегаций, а не в двух
  раздельных списках: под ними общий lifecycle, и пользователю нужен один ответ
  на вопрос «что сейчас работает». Тип делегации и наличие worktree — это
  атрибут строки, а не повод для отдельного экрана. Панель поддерживает cancel
  и переход в child session.
- [ ] Показывать steering messages как queued, пока они не persisted и не
  включены в model step; после drain удалять queue pill без polling delay.
- [ ] Escape должен отменять активный main turn, но не все фоновые tasks.
  Отдельное действие должно отменять выбранную task; массовая отмена требует
  подтверждения.
- [ ] Разрешить отправку follow-up из просмотра child session, опираясь на тот
  же путь, что и `task_send`/`thread_send`. Read-only режим больше не нужен:
  протокол существует и отлажен в `Manager.Send`, откладывать было бы
  искусственным ограничением.
- [ ] Escape в просмотре child session отменяет саму делегацию, а не только
  просмотр; поведение должно совпадать для задач и тредов.

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
- [ ] Permission request фоновой задачи показывается в parent session с
  пометкой источника; запросы нескольких задач сериализуются FIFO; запрос
  главного агента вытесняет фоновые.
- [ ] Отказ в разрешении даёт terminal state задачи с понятной причиной.
- [ ] Cancel continuation не возвращает событие в inbox и не порождает
  повторную инъекцию.
- [ ] Цепочка continuation обрывается на пределе глубины: запуск фоновой задачи
  возвращает ошибку инструмента, сам continuation выполняется.
- [ ] Пользовательский turn сбрасывает счётчик глубины.
- [ ] После рестарта процесса задачи прошлого запуска не показываются как
  running.
- [ ] `task_output` возвращает частичный вывод работающей задачи и не блокирует
  главный агент.
- [ ] Задача, ожидающая ответа на permission prompt, видна в task-панели и
  снимается только ответом или отменой.
- [ ] Race tests под `go test -race` для registry, inbox, cancel и shutdown.

### Регрессия тредов

- [ ] Существующие тесты `internal/thread/` и
  `internal/workspace/threads_*_test.go` проходят после выделения общего
  lifecycle без правок ожиданий.
- [ ] `Recover` одинаково обрабатывает прерванные треды и прерванные задачи.
- [ ] Завершение треда доставляется родителю через inbox ровно один раз, и
  merge-фаза не порождает второй инъекции того же результата.
- [ ] `thread_send` и `task_send` идут одним путём: follow-up в прерванную
  делегацию респавнит workspace и не теряет сообщение.
- [ ] Треды и задачи делят один concurrency-учёт там, где это заявлено, и не
  вытесняют друг друга неожиданным образом.

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
- [ ] Background delegation выпустить за постоянной config option
  `options.background_agents`. Флаг не временный: он остаётся в продукте как
  способ полностью выключить фоновых агентов для тех, кому такое поведение не
  нужно. Значение по умолчанию выбирается при релизе, но сама опция не
  удаляется после стабилизации.
- [ ] Не менять foreground semantics существующих agent definitions без
  migration note.
- [ ] Задокументировать три понятия и границы между ними: queued steering,
  лёгкая фоновая задача и тред с worktree. Отдельно объяснить, что задача и
  тред — два варианта одной делегации, отличающиеся изоляцией и merge-фазой, а
  не два независимых механизма.
- [ ] Дать критерий выбора: когда модель должна брать задачу, а когда тред.
  Без него модель будет выбирать по названию инструмента, а не по свойствам.

## Порядок реализации

1. Асинхронный submit в local workspace и parity с backend.
2. Integration tests доставки follow-up через существующий `prepareStep`.
3. Спайк provider ordering (этап 1.5) — до оценки сроков этапа 2.
4. Явный steering API, события queue changed, нормализация порядка и
   взаимодействие с авто-суммаризацией.
5. Релиз steering как самостоятельной фичи.
6. Выделение общего lifecycle из `thread.Manager` без изменения поведения
   тредов — фундамент всего остального; регрессионные тесты тредов зелёные.
7. Лёгкая задача как конфигурация общего lifecycle без worktree, с общими
   persistence и `Recover`.
8. Permission prompt делегации в parent session с FIFO-очередью — общий для
   тредов и задач.
9. Background mode для built-in `agent`, затем для custom agents; `task_send`
   по образцу `thread_send`.
10. Completion inbox для обоих механизмов и internal continuation turns с
    лимитом глубины; перевод `thread_wait` в legacy.
11. Единая панель делегаций, ограничения modifying agents и постепенный
    rollout.

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
- Фоновая задача не может выполнить действие, требующее разрешения, в обход
  выбранной permission policy.
- Рестарт процесса не оставляет делегаций, показанных как выполняющиеся, — ни
  тредов, ни задач.
- Треды и лёгкие задачи используют один lifecycle, один способ вернуть
  результат родителю и одну панель управления. Поведение существующих тредов
  при этом не изменилось, кроме появления доставки результата через inbox.
- `go test ./...`, targeted integration tests и `go test -race` для изменённых
  concurrency-компонентов проходят.
