# План удаления разделения моделей на large/small

## Цель

Перевести Braid на одну модель во всех слоях: конфигурации, agent runtime,
вспомогательных задачах, UI и HTTP API. После изменений выбранная
`config.Model` должна быть единственным источником модели для основного агента,
генерации заголовков, суммаризации и `agentic_fetch`.

Работа считается завершённой, когда production-код больше не содержит понятий
`large model`, `small model`, `model pair` и не обращается к
`catwalk.Provider.DefaultSmallModelID`. Допустимы только упоминания старого
формата в миграциях и regression-тестах.

## Зафиксированные решения

- Использовать одну выбранную модель для всех операций, включая title,
  summarize и `agentic_fetch`.
- Не вводить замену `small` под другим именем (`helper`, `fast`, `cheap`): это
  сохранило бы то же архитектурное разделение.
- Пользовательские агенты с собственным `Agent.Model` продолжают использовать
  эту модель как свою единственную модель. Пустой `Agent.Model` наследует
  основную модель приложения.
- Удалить endpoint `GET
  /v1/workspaces/{id}/agent/default-small-model` и весь его transport-контракт.
  Это breaking change для внешних клиентов, поэтому его нужно отметить в
  release notes.
- Сохранить чтение старых persisted-конфигов настолько, насколько это требуется
  для безопасного обновления. В частности,
  `dropIncompatibleRecentModels` должен продолжать отбрасывать старый объект
  `recent_models.{large,small}`. Это миграция данных, а не часть runtime-модели.
- Продолжать явно отклонять старый shell-синтаксис `model large` и
  `model small`, но переписать сообщение без утверждения о существующих
  слотах. Удалить такую защиту можно позднее, когда закончится объявленный срок
  совместимости.

## Этап 1. Зафиксировать baseline и защитить целевое поведение

- [ ] Запустить `go test ./...` и сохранить список уже существующих падений,
  чтобы не смешивать их с результатами рефакторинга.
- [ ] Добавить или скорректировать тесты, фиксирующие целевую семантику:
  основной run, title, summarize и `agentic_fetch` получают одну и ту же
  модель, если у агента нет собственного `Agent.Model`.
- [ ] Зафиксировать отдельным тестом, что пользовательский агент с
  `Agent.Model` использует только свою модель.
- [ ] Сохранить regression-тест загрузки старого
  `recent_models.{large,small}`: приложение стартует, несовместимое поле
  удаляется и затем строится в актуальном плоском формате.

Результат этапа: тесты описывают не текущую пару моделей, а целевой
single-model contract.

## Этап 2. Упростить построение модели в coordinator

Основная зона: `internal/agent/coordinator.go`.

- [ ] Переименовать `buildAgentModels` в `buildAgentModel` и вернуть один
  `Model` вместо пары `(Model, Model)`.
- [ ] Оставить в функции только разрешение `cfg.Config().Model`, проверку
  провайдера, поиск catalog model, применение `:exacto` и создание
  `fantasy.LanguageModel`.
- [ ] Удалить `defaultSmallModel` и обращение к
  `catwalk.Provider.DefaultSmallModelID`.
- [ ] Свести парные ошибки к единственным вариантам, например
  `errModelNotSelected`, `errModelProviderNotConfigured` и
  `errModelNotFound`.
- [ ] В `buildAgent` передавать одну primary-модель. Для кастомного агента
  использовать результат `buildCustomAgentModel`, иначе основную модель.
- [ ] В `agentic_fetch_tool.go` использовать `buildAgentModel` и создавать
  fetch-агента с одной моделью.
- [ ] Удалить `coordinator_default_small_model_test.go`; его полезные сценарии
  перенести в тесты single-model resolution.
- [ ] Обновить комментарии readiness/race-тестов, которые сейчас требуют
  настройки «large and small models».

Результат этапа: coordinator нигде не вычисляет и не строит вспомогательную
модель.

## Этап 3. Упростить runtime cache и SessionAgent

Основные зоны: `internal/agent/runtime_cache.go`,
`internal/agent/agent.go`, `internal/agent/turn.go`.

- [ ] Заменить `compiledRuntime.large` и `compiledRuntime.small` одним полем
  `model`.
- [ ] Обновить комментарий `SessionAgentCall.Runtime`: runtime несёт одну
  модель и tools, а не model pair.
- [ ] Заменить `sessionAgent.largeModel` и `sessionAgent.smallModel` одним
  `model *csync.Value[Model]`.
- [ ] Заменить `SessionAgentOptions.LargeModel`/`SmallModel` на `Model`.
- [ ] Заменить `SessionAgent.SetModels(large, small)` на `SetModel(model)`.
- [ ] Удалить `runtimeSmall` и `runtimeLarge`; обращаться к единственному
  `runtime.model`.
- [ ] Переименовать локальные переменные и параметры `largeModel` в `model` в
  `agent.go`, `turn.go`, `event.go` и связанных helper-функциях.
- [ ] Сохранить `RuntimeModel` только если он действительно нужен прямым
  тестовым/внешним вызовам. Если production-вызовов нет, удалить его вместе с
  compatibility-комментарием и перевести тесты на `Runtime`.
- [ ] Обновить `Model()` так, чтобы он возвращал единственное поле модели.
- [ ] Обновить test builders и fixtures, создающие `SessionAgentOptions`, без
  механического заполнения двух одинаковых полей.

Результат этапа: типы runtime не позволяют представить две модели.

## Этап 4. Упростить вспомогательные операции

- [ ] Изменить `generateTitle`: принимать одну модель и делать одну попытку.
  Удалить массив attempts с именами `small`/`large` и fallback между моделями.
- [ ] Сохранить существующий fallback названия на `DefaultSessionName`, если
  единственная модель завершилась ошибкой, была отменена или достигла лимита.
- [ ] Проверить лимит title generation: оставить текущую логику `40` токенов
  для non-reasoning модели и model default для reasoning модели либо вынести
  её изменение в отдельную задачу. Удаление model pair не должно незаметно
  менять token policy.
- [ ] Оставить summarize на единственной модели и переименовать все локальные
  переменные, сейчас называемые `largeModel`.
- [ ] Перевести `agentic_fetch` на единственную выбранную модель. Не менять в
  этом рефакторинге набор tools, permissions и session lifecycle.
- [ ] Проверить учёт usage/cost: title, summarize и fetch должны записывать
  фактически использованную единственную модель без fallback-веток.

Результат этапа: все операции имеют одинаковую и очевидную модельную семантику.

## Этап 5. Удалить default-small-model API

Удалить контракт по всей вертикали, а не оставлять неиспользуемые адаптеры:

- [ ] `App.GetDefaultSmallModel` в `internal/app/app.go`.
- [ ] `Backend.GetDefaultSmallModel` в `internal/backend/agent.go`.
- [ ] `Workspace.GetDefaultSmallModel` и реализации в
  `internal/workspace/app_workspace.go`, `client_workspace.go` и
  `read_only_workspace.go`.
- [ ] HTTP route из `internal/server/server.go` и handler из
  `internal/server/proto.go`.
- [ ] `Client.GetDefaultSmallModel` из `internal/client/proto.go`.
- [ ] Стабы и тестовые реализации workspace-интерфейсов.
- [ ] Сгенерированные Swagger-файлы `internal/swagger/docs.go`,
  `swagger.json`, `swagger.yaml` обновить штатным генератором проекта, а не
  ручным редактированием.
- [ ] Найти фактических внешних потребителей endpoint. Если он уже опубликован,
  отметить удаление в migration/release notes и повысить API version либо
  включить изменение в ближайший объявленный breaking release.

Результат этапа: ни local, ни client/server режим не выставляет понятие default
small model наружу.

## Этап 6. Удалить legacy large/small API из config и UI

Основные зоны: `internal/config/config.go`, `internal/ui/`.

- [ ] Удалить `SelectedModelType` и константы
  `SelectedModelTypeLarge`/`SelectedModelTypeSmall`.
- [ ] Заменить `GetProviderForModel(modelType)` на метод без аргумента,
  возвращающий провайдера `Config.Model`.
- [ ] Заменить `GetModelByType(modelType)` на нейтральный метод без аргумента,
  например `SelectedCatalogModel()`; не создавать новый «model type» enum.
- [ ] Переименовать или удалить `Config.LargeModel()` в пользу того же
  нейтрального accessor, чтобы не осталось двух способов получить выбранную
  модель.
- [ ] Обновить UI-вызовы в `internal/ui/model/ui.go`, `header.go`,
  `dialog/reasoning.go`, `dialog/commands.go` и model dialogs.
- [ ] Удалить `ModelType` из UI actions/dialog contracts, если после перехода
  он больше ничего не различает.
- [ ] Обновить UI tests и golden files только после проверки, что визуальное
  поведение выбора и аутентификации модели не изменилось.
- [ ] В agent config оставить `Agent.Model` как обычную ссылку
  `provider/model`; удалить комментарии, описывающие `large`/`small` как
  бывшие специальные значения, кроме migration-документации.

Результат этапа: основной код конфигурации и UI оперирует просто selected
model, без фиктивного enum со всегда одним значением.

## Этап 7. Очистить совместимость, тесты и документацию

- [ ] Удалить тесты, проверяющие внутреннее наличие пары моделей, и заменить их
  проверками единственной модели.
- [ ] Сохранить только migration-тесты старых пользовательских данных и тест
  понятной ошибки для `model large`/`model small`.
- [ ] Обновить `TECHDEBT.md` и другие документы: описывать `large`/`small`
  только как удалённый исторический формат.
- [ ] Проверить `.go`, `.md`, `.tpl`, Swagger и testdata поиском по словам и
  идентификаторам:

  ```sh
  rg -n 'SelectedModelType|LargeModel|SmallModel|largeModel|smallModel|defaultSmallModel|default-small-model|model pair' .
  ```

- [ ] Разобрать каждый остаток вручную. Не заменять обычные английские слова
  `small`/`large`, не относящиеся к моделям.
- [ ] Не редактировать записанные VCR/testdata payloads только ради текста,
  если в них нет model-pair contract; обновлять такие fixtures штатным способом
  лишь при изменении поведения теста.

Результат этапа: production-код не содержит старой архитектуры, а исторические
упоминания ограничены миграциями и их тестами.

## Проверка

После каждого этапа запускать тесты затронутого пакета. Перед завершением:

```sh
gofumpt -w .
go test ./internal/config ./internal/agent ./internal/workspace ./internal/backend ./internal/server ./internal/client
go test ./internal/ui/...
go test ./...
go build .
task lint:fix
git diff --check
```

Если `gofumpt` или `task` недоступны, использовать команды fallback из
`AGENTS.md` и явно зафиксировать это в итоговом отчёте.

## Критерии готовности

- В конфигурации существует одна выбранная модель `Config.Model`.
- `compiledRuntime`, `SessionAgent` и `SessionAgentOptions` содержат одну
  модель.
- Основной run, title, summarize и `agentic_fetch` используют эту модель либо
  явную `Agent.Model` конкретного пользовательского агента.
- Braid не читает `DefaultSmallModelID` и не строит дополнительный provider/
  language-model instance для внутренних задач.
- Endpoint `default-small-model` отсутствует в server, client, workspace и
  Swagger.
- `SelectedModelType`, `LargeModel`, `SmallModel`, `largeModel` и `smallModel`
  отсутствуют в production-коде.
- Старые persisted-конфиги не препятствуют запуску и покрыты migration-тестами.
- Полный test/build/lint pipeline проходит.

## Риски и контроль границ

- Использование основной модели для title и `agentic_fetch` может увеличить
  стоимость. Это ожидаемое следствие удаления разделения, а не регрессия,
  которую следует скрывать новым implicit helper selection.
- Удаление endpoint является публичным breaking change. Его нельзя оставить
  как endpoint, возвращающий основную модель: это сохранит ложный контракт и
  затруднит окончательное удаление.
- Массовое переименование полей затрагивает concurrency-sensitive код runtime
  cache и queued runs. Рефакторинг должен сохранять snapshot модели на весь
  queued lifecycle и не читать изменившийся config посреди turn.
- Не следует одновременно менять provider options, title token policy,
  permissions или устройство runtime cache. Такие изменения усложнят проверку
  эквивалентности и должны идти отдельными задачами.
