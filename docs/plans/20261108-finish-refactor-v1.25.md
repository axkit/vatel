# Завершение рефакторинга vatel и релиз v1.25.0

## Overview
- Довести до релиза большой рефакторинг API vatel на ветке `refactor`: новые builder-функции эндпоинтов, generic-carriers, middleware по позициям, авторизация в пакете `auth/`.
- Закрыть проблемы, найденные проектом Гостли (`/Users/gera/go/src/git.elephantsoft.ru/gostly/app/docs/backlog/vatel-issues-from-gostly.md`): необязательное тело (№4), OPTIONS/HEAD (№6), секреты в журнале (№10, №11), плюс найденные при разборе и ревью плана хвосты.
- Дать обработчикам контекст с отменой при остановке сервера и таймаутом (отмену по разрыву соединения клиентом fasthttp не поддерживает: `RequestCtx.Done()` — канал сервера, закрывается только в `Shutdown`).
- Выпустить одним релизом **v1.25.0** с `retract v1.24.2` в go.mod. Номер должен быть выше v1.24.2, иначе Go не подхватит отзыв ошибочной версии из прокси.
- Уже сделано до плана (2026-11-08, в рабочей копии): errors v1.1.0, `WithClientErrorFields`, `WithSuccessStatusCode`/`WithManualStatusCode` и реальный статус в метриках, защита от повторного `BuildHandlers`, `auth` не отвергает плохой токен на публичных эндпоинтах (Гостли №2, №7).
- Отложено в бэклог: `docs/backlog/log-level-header-overrides-confidential.md` (заголовок `X-Set-Log-Level` может включить запись тел на конфиденциальных эндпоинтах).
- Не делаем: тело у DELETE (RFC 9110 — семантика не определена; Гостли №5 остаётся на query, фиксируется в MIGRATION.md).

## Context (from discovery)
- Ядро: `vatel.go` (Vatel, опции, Shutdown), `endpoint.go` (конвейер запроса, compile, логирование, ошибки), `decode.go` (разбор path/query/body), `carriers.go`, `options.go` (EndpointOption и builders), `middleware.go` (MiddlewarePos, EndpointMeta), `context.go`.
- WebSocket: `ws_vatel.go`, `ws_context.go`, `ws_access_token_update.go`.
- Авторизация: `auth/auth.go` (+ `auth/auth_test.go` через настоящий router).
- Маскирование: `jsonmask/jsonmask.go` — `mask:"-"` удаляет поле, `mask:"<name>"` применяет функцию из `AddFunc`. Известные дефекты (ревью): panic на не-структурах и nil, переполнение стека на рекурсивных типах, не заходит в `*Struct`/`[]*Struct`, игнорирует тег на полях-структурах (`time.Time`), вставляет отсутствующие поля, незарегистрированное имя оставляет значение открытым.
- fasthttp/router кладёт path-параметры в user values `fctx`; `logEnter`/`logExit`/`writeErrorResponse` пишут все user values в журнал через `VisitUserValues`.
- fasthttp v1.31.0: тело ответа на HEAD отбрасывает только `Server.serveConn`; `RequestCtx.Done()` = `s.done`.
- router v1.4.4: `GlobalOPTIONS` вызывается только для зарегистрированных путей, неизвестный путь — 404.
- Тесты: `handler_test.go`, `endpoint_test.go`, `carriers_test.go`, `custom_claims_test.go`, `testutil_test.go`, `auth/auth_test.go`. Базовое покрытие (2026-11-09, до Task 2): `vatel` 54.5%, `auth` 85.5%, `jsonmask` 92.5%.
- Пример: `example/` (порт 8080 на машине разработчика занят `rnp-serve`).
- Зависимости: axkit/errors v1.1.0; axkit/bitset через локальный `replace`, v1.0.4 опубликована и совпадает с локальным HEAD.
- `vatel_test.gox` — устаревший отслеживаемый файл.

## Development Approach
- **testing approach**: TDD — сначала тест, который воспроизводит проблему или описывает новое поведение, потом код.
- complete each task fully before moving to the next
- make small, focused changes; каждая задача — отдельный коммит на ветке `refactor`
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** — `go test -race ./...` и `go vet ./...`
- **CRITICAL: update this plan file when scope changes during implementation**
- обратная совместимость с v0.14 уже нарушена самим рефакторингом; новые задачи не должны ломать сверх этого без записи в MIGRATION.md. Каждое изменение поведения сразу дописывается в раздел «Для MIGRATION.md» этого плана.

## Testing Strategy
- **unit tests**: стиль `handler_test.go` (`run(v, GET(...), fctx)`) — для конвейера без роутера; стиль `auth/auth_test.go` (`router.New()` + `BuildHandlers` + `mux.Handler`) — везде, где важны user values роутера (path-параметры, журнал).
- **настоящий сервер**: `fasthttputil.NewInmemoryListener` + `fasthttp.Server` + `fasthttp.Client` — для HEAD и проверки, что тело не отправляется.
- журнал проверять через `zerolog.New(&buf)` и разбор строк JSON.
- **e2e**: копия `example/` на свободном порту + curl в Task 13.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

**Необязательное тело.** Опция `vatel.WithOptionalBody()`: тело пустое или из одних пробелов → разбор пропускается, вход нулевой, `Validator` для тела не вызывается (документировать: `Validate()`, задающий значения по умолчанию, не сработает). На GET/DELETE/HEAD/OPTIONS опция — ошибка compile. По умолчанию пустое тело по-прежнему 400.

**Query отдельно от body.** Интерфейс `Querier { QueryInput() any }`; `QueryParams[T]` реализует его вместо `Input()`. Query разбирается для любого метода, если контроллер — `Querier`. `Inputer` как раньше: GET/DELETE/HEAD/OPTIONS — query, POST/PUT/PATCH — JSON body. `Validator` — отдельный шаг в `buildHandler` после всего разбора: сначала вход Querier, затем вход Inputer; первая ошибка → 400. `Validate()` должен быть объявлен на именованном типе (у анонимной структуры методов нет) — в README. В журнале: Querier → `reqQuery`, Inputer → `reqInput`.

**OPTIONS и HEAD.** Builders `vatel.HEAD`, `vatel.OPTIONS`; compile принимает эти методы (вход — query). Тело ответа на HEAD отбрасывает сервер fasthttp. CORS preflight — `mux.GlobalOPTIONS` (только для зарегистрированных путей); явный `vatel.OPTIONS` на пути заменяет его; `RequireAuth`/`RequirePerms` на OPTIONS ломает preflight (браузер не шлёт `Authorization`) — всё в README.

**jsonmask — укрепление.** `Fields`: nil, скаляры и map → пустые правила; корень-срез или массив структур (`Body[[]T]`, `Output[[]T]` — указатель на срез) → правила элемента с путём `#.<поле>`, чтобы секреты в элементах массива маскировались; заход в `*Struct`, `[]Struct`, `[]*Struct`; учёт посещённых типов против рекурсии; поле с непустым тегом `mask` — лист (маскируется или удаляется целиком, без захода внутрь). `Mask`: правило применяется, только если поле есть (`gjson ... Exists()`). Незарегистрированное имя функции → `"***"`.

**Секреты в журнале — тег `mask`.**
- Vatel по умолчанию использует `jsonmask.New()`, если `WithJsonMasker` не задан.
- На compile считается, есть ли в типе входа/результата хоть один тег `mask`; маскирование выполняется только тогда. Ключи журнала не меняются (`requestBody`, `respBody`, `reqInput`, `result` — с маскированными значениями).
- `LogReqInput`/`LogRespOutput` пишут JSON (уже сериализованный `buf` для ответа), пропущенный через маскирование, а не `Interface(struct)`.
- Ошибка `json.Marshal` ответа: в журнал только ошибка, без объекта.
- Path и query параметры: `mask:"-"` — не писать; иной непустой `mask` — `"***"` (функции jsonmask к параметрам не применяются — документировать). Path-параметры пишутся в журнал только из `decodeParams`; их ключи (имена `{name}` из шаблона пути, собираются на compile) пропускаются во всех трёх обходах `VisitUserValues` — заодно уходит дублирование ключей.

**Контекст запроса.** `Vatel` создаёт базовый контекст `context.WithCancel(Background)`; `Shutdown` его отменяет (идемпотентно, без гонки с fasthttp). `ctx.Context()` возвращает его или, если задан таймаут, `context.WithTimeout(base, d)` с `cancel` в конце обработки. Таймаут: `vatel.WithTimeout(d)` на эндпоинт и `vatel.WithDefaultTimeout(d)` на Vatel. Ошибка обработчика `context.DeadlineExceeded` → 504, `context.Canceled` → 503 (через `errors.Is`). Таймаут не прерывает обработчик сам — только код, который слушает контекст (документировать).

**Хвосты.**
- WS: ошибка разбора `data` → `ErrInvalidRequestBody` (400).
- `auth.RequirePerms` дописывает имена прав (отсортированно) в `Endpoint.Perms`; `Disabled` проверяется во время запроса в обоих `Require*`; `Authorizer == nil` → `ErrInternalError` вместо panic. Комментарий `vatel.Perms`: для HTTP это метаданные, проверку делает `auth`; на WS-эндпоинте `Perms` требуют `WithWsAuthFunc`.
- `LogSilent`: ничего не пишется ни на каком пути (middleware, разбор, обработчик, WS), кроме ошибок 5xx; `LogSilent` главнее остальных битов, включая заданные заголовком. Уровень журнала передаётся в `writeErrorResponse`/`runMiddleware`/`wsWriteErrorResponse`; `writeErrorResponse` всегда использует `e.EndpointMeta` (сейчас `OnErrorResponse` получает пустые метаданные при ошибке из middleware).
- Удалить `NoInputLog`, `NoResultLog`, `LanguageLabel`, `WithLanguageLabel`, остаток `atomic.Load` для `LogOptions`.
- `BuildHandlers`: ошибки WS-регистрации возвращаются, а не panic. Контракт без повтора: после неудачной сборки `Vatel` непригоден — повторный вызов возвращает ошибку «previous build failed» (откат частичной регистрации в router невозможен, транзакционность не нужна). Задокументировать в комментарии `BuildHandlers`.
- WS: `OnTokenUpdate(f)`; ошибка `f` возвращается из `UpdateAccessToken`.

**Релиз.** bitset v1.0.4 вместо `replace`; `retract v1.24.2`; README, MIGRATION.md, CLAUDE.md.

## Technical Details
- `Endpoint`: поля `OptionalBody bool`, `Timeout time.Duration`; внутренние `pathParamKeys []string`, `querierFields []fieldInfo`, `queryMaskFields jsonmask.Fields`, `isQuerier bool`.
  - ➕ отклонение: полей `inputHasMask`, `resultHasMask`, `queryHasMask` нет — их заменяет `jsonmask.Fields.HasRules()` на предвычисленных правилах.
  - ➕ ревью фазы 1: копии настроек `Vatel` в `Endpoint` (`baseCtx`, `verboseError`, `logRequestID`, `clientErrorFields`, `staticLoggingLevel`, `ala`, `mr`) заменены указателем `e.v *Vatel`; `resultPrototyper` убран — правила результата неизвестного при компиляции типа строятся по типу и кэшируются (`resultFieldsByType`).
- ➕ `Vatel`: `mu`, `srv`, `ln`, `served`, `closed`, `inflight`; `DefaultIdleTimeout`, `ErrServerClosed`, `WithServerConfig`; внутренний `serve(ln, mux)` для тестов на in-memory listener.
- `decodeBody(body, dest, optional bool)` без вызова `Validate`; `validate(in any) error` вызывается из `buildHandler`.
- `fieldInfo.mask string` — значение тега `mask`.
- `compileCommon`: `if e.jm == nil { e.jm = defaultMasker }`, `defaultMasker = jsonmask.New()`.
- `Vatel`: `baseCtx context.Context`, `cancel context.CancelFunc`, `defaultTimeout time.Duration`; `newContext(fctx, ctx)` внутренний, публичный `NewContext(fctx)` сохраняет сигнатуру (Background).
- `retract` в go.mod:
  ```
  retract v1.24.2 // published by mistake, use v1.25.0 or later
  ```

## Для MIGRATION.md
Дописывается по ходу задач. Уже известно:
- новый API регистрации (`vatel.GET/...` + `EndpointOption`), carriers, позиционные middleware, авторизация в `vatel/auth`;
- errors v1.1.0: клиент без режима отладки не видит полей ошибки; безопасные поля — `WithClientErrorFields`;
- плохой/просроченный/отозванный токен на публичном эндпоинте больше не даёт 401;
- `WithSuccessStatusCode` теперь действует; метрики получают реальный статус;
- повторный `BuildHandlers` → ошибка (и после неудачной сборки тоже); `ListenAndServe` после `BuildHandlers` не регистрирует маршруты заново; ошибки WS-регистрации возвращаются, а не panic;
- `X-Set-Log-Level` действует на один запрос (в v0.14 сохранялся на эндпоинте);
- удалены `NoInputLog`, `NoResultLog`, `LanguageLabel` (были публичными и в v0.14);
- тело у DELETE не поддерживается — query (`QueryParams`).
- новое: `WithOptionalBody()` (поле `Endpoint.OptionalBody`) для POST/PUT/PATCH — пустое тело или тело из пробелов допустимо, вход остаётся нулевым, `Validate()` не вызывается; на GET/DELETE — ошибка `BuildHandlers`. Без опции тело из одних пробелов теперь даёт 400 «empty request body» (раньше — 400 «not valid JSON»).
- новое: интерфейс `Querier { QueryInput() any }`; `QueryParams[T]` реализует `QueryInput()` вместо `Input()` (код, вызывавший `Input()` у `QueryParams`, нужно поправить; `QueryParams` больше не `Inputer`). Query разбирается для любого метода, в том числе POST/PUT/PATCH, поэтому `QueryParams` + `Body` в одном контроллере дают и query, и тело (раньше двусмысленный `Input()` отключал разбор входа совсем). Старый `Inputer` без изменений: GET/DELETE — query, POST/PUT/PATCH — тело. `Validate()` вызывается после всего разбора: сначала у входа `Querier`, затем у входа `Inputer`, в том числе у query-входа `Inputer` на GET/DELETE; первая ошибка → 400. В журнале вход `Querier` пишется под ключом `reqQuery`, вход `Inputer` — под `reqInput`.
- новое: builders `vatel.HEAD` и `vatel.OPTIONS` (раньше эти методы давали ошибку `BuildHandlers` «unknown HTTP method»). Вход `Inputer` на HEAD/OPTIONS разбирается из query, как на GET; `WithOptionalBody()` на них — ошибка `BuildHandlers`. Тело ответа на HEAD отбрасывает сервер fasthttp. CORS preflight — `mux.GlobalOPTIONS` (вызывается только для зарегистрированных путей, неизвестный путь — 404); явный `vatel.OPTIONS` на пути заменяет `GlobalOPTIONS`; `RequireAuth`/`RequirePerms` на OPTIONS ломает preflight.
- jsonmask: `Fields` больше не паникует (nil, скаляры, map → пустые правила; новый `Fields.HasRules()`), поддерживает рекурсивные типы, корень-срез/массив (`*[]T`, `[]*T`), заходит в `*Struct`, `[]Struct`, `[]*Struct`. Поле с непустым тегом `mask` — лист: значение-структура (`time.Time`, вложенная структура, указатель, срез) маскируется или удаляется целиком. Тег на встроенной структуре без json-имени применяется к каждому её продвинутому полю. Незарегистрированное имя функции теперь даёт `"***"` (раньше значение оставалось открытым). Отсутствующее поле (`omitempty`) не добавляется. Карты и interface не обходятся.
- журнал: поля с тегом `mask` маскируются всегда — без `WithJsonMasker` используется `jsonmask.New()` (незарегистрированное имя функции → `"***"`). Маскируются `requestBody`, `reqInput`, `reqQuery`, `respBody`, `result`, в том числе в элементах массивов (`Body[[]T]`, `Output[[]T]`) и в `OptionalOutput[T]`; ответ клиенту не маскируется. Ключи `maskedRequestBody`/`maskedRespBody` убраны — замаскированное тело пишется под прежними `requestBody`/`respBody`. `result` пишется как сериализованный ответ (тот же JSON, что получил клиент, после маскирования), а не через `Interface`. При ошибке маскирования в журнал идёт только ошибка (`requestBodyMaskingFailed`, `reqInputMaskingFailed`, `respMaskingFailed`), без незамаскированного значения (раньше тело запроса писалось открытым). При ошибке `json.Marshal` ответа объект ответа в журнал не пишется (раньше — `result`). Тип без тегов `mask` логируется как прежде.
- журнал: path- и query-параметры с тегом `mask` маскируются — `mask:"-"` не пишется, любой другой непустой `mask` → `"***"` (функции jsonmask к параметрам не применяются); тег `mask` на вложенной структуре query-входа действует на её поля без своего тега. Значения в структуре входа не меняются. Path-параметры пишутся только из разбора `Paramer` (ключ больше не дублируется в журнале; параметр пути, которого нет в структуре `Paramer`, больше не пишется), user values с именами `{name}` из шаблона пути в журнал не попадают; при `LogEnter|LogExit` path-параметры — только в строке входа (раньше дублировались в строке выхода через user values).
- новое: контекст запроса. `ctx.Context()` — дочерний от базового контекста `Vatel` (раньше `context.Background()`); `Shutdown` отменяет его — после ожидания запросов в полёте (до дедлайна `ctx` из `Shutdown`), а без сервера из `ListenAndServe` сразу; повторный `Shutdown` безопасен. Отмены по разрыву соединения клиентом нет (fasthttp его не видит). Таймаут: `vatel.WithTimeout(d)` (поле `Endpoint.Timeout`) на эндпоинт и `vatel.WithDefaultTimeout(d)` на `Vatel`; контекст с таймаутом отменяется и в конце обработки запроса. Таймаут не прерывает обработчик сам — только код, слушающий контекст. Ошибка без своего статуса (не `*errors.Error` или `*errors.Error` со статусом 0), в цепочке которой `context.DeadlineExceeded`, → 504, `context.Canceled` → 503; явный статус ошибки главнее. `*errors.Error` без статуса (например, `errors.Wrap(err, "msg")` от обычной ошибки) теперь даёт 500, а не 200. `NewContext(fctx)` по-прежнему с `context.Background()`.
- WS: ошибка разбора поля `data` сообщения во вход `Inputer` → `ErrInvalidRequestBody` (400, код `VTL-0007`, сообщение «ws message data is not valid JSON»); раньше — необёрнутая ошибка `json.Unmarshal` и статус 500 (в журнале, метриках и `Alarm`).
- auth: `RequirePerms` дописывает имена прав (отсортированно) в `Endpoint.Perms` — middleware видят их в `EndpointMeta.Perms` (на WS-эндпоинте это требует `WithWsAuthFunc`, иначе ошибка `BuildHandlers`). `Config.Disabled` проверяется во время запроса и в `RequirePerms` (раньше — только при создании опции; `RequireAuth` и `Middleware` уже проверяли во время запроса). `Config.Authorizer == nil` при `RequirePerms` → 500 `ErrInternalError` вместо panic. Комментарий `vatel.Perms`: для HTTP это только метаданные, проверку делает `auth`.
- журнал: `LogSilent` теперь действительно глушит журнал — на эндпоинте (HTTP и WS) не пишется ничего, включая ошибки 4xx из middleware, разбора и обработчика; пишутся только ошибки 5xx (раньше `LogSilent` ничего не отключал, ошибки писались всегда). `LogSilent` главнее остальных битов; заголовок `X-Set-Log-Level` не отменяет `LogSilent` эндпоинта, а `silent` в заголовке глушит запрос.
- middleware `OnErrorResponse` всегда получает метаданные эндпоинта (`Method`, `Path`, `Perms`, `LogOptions`); раньше при ошибке из middleware (`OnRequest`/`OnAuthentication`/`OnAuthorization`/`OnHandle`) приходил пустой `EndpointMeta`.
- удалена опция `WithLanguageLabel` (вместе с полем `LanguageLabel`).
- WS: ошибка без статуса (не `*errors.Error` или `*errors.Error` со статусом 0) в журнале/метриках/`Alarm` считается как в HTTP — 500 (`context.DeadlineExceeded` → 504, `context.Canceled` → 503); раньше `*errors.Error` без статуса давал в метриках статус 0 и не вызывал `Alarm`.
- `BuildHandlers`: WS-эндпоинт без `WithWebsocketSupport` и повторный WS-путь теперь возвращают ошибку с путём эндпоинта (раньше — panic; `MustBuildHandlers` по-прежнему паникует). Ошибка повторного WS-пути из `WebsocketVatel.RegisterEndpoint` — обычная ошибка «WS endpoint <path>: already registered», а не `*errors.Error` с полем `path`. После неудачной сборки `Vatel` непригоден: повторный `BuildHandlers` и `ListenAndServe` возвращают ошибку «previous build failed» (раньше `ListenAndServe` после неудачного `BuildHandlers` запускал сервер с частью маршрутов).
- новое: `WebsocketVatel.OnTokenUpdate(f func(cid uint64, tp AuthPayload) error)` — вызывается из `UpdateAccessToken` после сохранения payload; ошибка `f` возвращается из `UpdateAccessToken` (и уходит клиенту эндпоинта `updateAccessToken`).
- модуль: версия v1.24.2 отозвана (`retract` в go.mod, опубликована по ошибке — переходить на v1.25.0 или новее); `github.com/axkit/bitset` — обычная зависимость v1.0.4 (без `replace`).
- ➕ ревью фазы 1 (перенесено в MIGRATION.md; пункты, отличающиеся только от v1.24.2, вынесены там в раздел 4):
  - журнал: тело запроса типа с тегами `mask` пишется из разобранного входа (маршалинг + маскирование), не сырым — `encoding/json` принимает ключи в другом регистре и последний из дубликатов; неразбираемое тело такого типа не пишется (`requestBodyLoggingFailed`). Ключ ошибки маскирования ответа — `respBodyMaskingFailed`/`resultMaskingFailed` (было `respMaskingFailed`). При `LogRespBody|LogRespOutput` ответ пишется один раз, под `respBody`. `reqId` больше не дублируется; нестроковое значение `message` не вызывает panic.
  - ошибки: клиенту уходит первая `*errors.Error`/`*errors.ErrorTemplate` в цепочке (`fmt.Errorf("%w")` сохраняет статус, текст обёртки не уходит); цепочка без неё → общая ошибка (`internal error`, `VTL-0005`) без режима отладки. То же для WS. Ошибка разбора маскированного path/query-параметра не содержит значения.
  - ошибка `OnSuccessResponse` действительно игнорируется (раньше перезаписывала статус на 500 и дописывала тело ошибки).
  - `Shutdown`: отменяет контексты запросов по дедлайну `ctx` (не по возврату), ждёт до 1 с отменённые запросы; не ждёт простаивающие keep-alive соединения; гонка `v.srv` устранена; `ListenAndServe` после `Shutdown` → `ErrServerClosed`. Сервер: `IdleTimeout = DefaultIdleTimeout` (90 с), `CloseOnShutdown`, новая опция `WithServerConfig`.
  - `BuildHandlers`: panic router (дубликат/конфликт пути, свой `GET /`) и путь без ведущего `/` → ошибка.
  - path-параметры поддерживают те же типы, что query (указатели, `date.Date`); `float32` разбирается с 32-битной точностью.
  - jsonmask: собственный тег продвинутого поля главнее тега встроенной структуры.
  - WS: `OnTokenUpdate` вызывается до сохранения payload, ошибка отвергает токен; ошибка декодирования токена → `ErrUnauthorized`; `WithLogger` теперь используется; `AuthMessageData` и `ErrAuthorizationHeaderMissed` помечены `Deprecated`.
  - auth: nil `Decoder` и ошибка `RevokeCheck` → 500 `VTL-0005` на защищённых эндпоинтах; `Disabled` задаётся до начала обслуживания (гонка данных при изменении во время работы).
  - метрики: размер ответа HEAD — 0.

## What Goes Where
- **Implementation Steps**: код, тесты, документация в этом репозитории, коммиты на ветке `refactor`.
- **Post-Completion**: слияние в main, тег и публикация, проверка прокси, уведомление Гостли.

## Implementation Steps

### Task 1: Базовый коммит и форматирование
**Files:**
- Delete: `vatel_test.gox`
- Modify: все изменённые и новые файлы рабочей копии

- [x] убедиться, что `go test -race ./...` и `go vet ./...` проходят
- [x] удалить `vatel_test.gox`
- [x] коммит «refactor: new endpoint API, carriers, positional middleware, auth package» — всё, кроме `docs/` и `CLAUDE.md` (CLAUDE.md обновляется в Task 15; go.mod пока с локальным replace — исправляется в Task 13)
- [x] `gofmt -w .`, тесты, отдельный коммит «style: gofmt»
- [x] отдельные коммиты: этот план; пункт бэклога `docs/backlog/log-level-header-overrides-confidential.md`

### Task 2: Необязательное тело запроса и отдельный шаг Validator
**Files:**
- Modify: `options.go`, `endpoint.go`, `decode.go`
- Modify: `handler_test.go`, `endpoint_test.go`

- [x] тесты: POST с `WithOptionalBody()` — пустое тело и тело из пробелов → 200, вход нулевой; с телом → разобран; невалидный JSON → 400; без опции пустое тело → 400 (регрессия)
- [x] тест: `WithOptionalBody()` на GET → ошибка compile
- [x] тест: при пустом теле `Validator` не вызывается; при непустом — вызывается (через `buildHandler`)
- [x] обновить `TestDecodeBody_ValidatorSuccess`/`Failure` под вынесенный `validate`
- [x] реализовать `OptionalBody`, `WithOptionalBody()`, `decodeBody(..., optional)`, `validate(in)` в `buildHandler`
- [x] run tests — must pass before next task

### Task 3: Querier — query отдельно от body, Validator для query
**Files:**
- Modify: `endpoint.go`, `carriers.go`, `decode.go`
- Modify: `carriers_test.go`, `handler_test.go`, `endpoint_test.go`

- [x] тесты: `QueryParams` + `Body` на POST — разобраны и query, и тело; `QueryParams` на GET и DELETE — query
- [x] тест: старый стиль `Input()` на GET → query, на POST → body (регрессия v0.14)
- [x] тесты Validator: только Querier; только Inputer-query (старый стиль); оба — ошибка query-валидации даёт 400 и тело не валидируется
- [x] тест журнала: Querier пишется под `reqQuery`, Inputer — под `reqInput`
- [x] ввести `Querier`, перевести `QueryParams[T]` на `QueryInput()`, разбор и валидация в `buildHandler`; обновить doc-комментарии `QueryParams`, `Body`, `Inputer`
- [x] run tests — must pass before next task

### Task 4: Методы OPTIONS и HEAD
**Files:**
- Modify: `options.go`, `endpoint.go`
- Modify: `carriers_test.go` (TestBuilderFunctions), `handler_test.go`, `endpoint_test.go`

- [x] тест через настоящий сервер в памяти: `vatel.HEAD` отвечает статусом и заголовками без тела
- [x] тест: `vatel.OPTIONS` компилируется и обслуживается; неизвестный метод (`TRACE`) — ошибка compile
- [x] табличный тест по всем методам (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS): `Querier` → query разобран; старый `Inputer` → query для GET/DELETE/HEAD/OPTIONS и body для POST/PUT/PATCH
- [x] тест: `WithOptionalBody()` на DELETE, HEAD, OPTIONS → ошибка compile (на GET проверено в Task 2)
- [x] тест: `mux.GlobalOPTIONS` + эндпоинты vatel — OPTIONS на зарегистрированный путь → обработчик; на неизвестный путь → 404
- [x] добавить builders и ветку в `compile`
- [x] run tests — must pass before next task

### Task 5: jsonmask — укрепление
**Files:**
- Modify: `jsonmask/jsonmask.go`
- Modify: `jsonmask/jsonmask_test.go`

- [x] тесты `Fields` без panic: nil, `string`, `map`, рекурсивные `type Node struct{ Children []Node }` и `Next *Node`
- [x] тест: корень `*[]T` и `[]*T`, где у `T` поле `mask:"secret"` → `Mask` маскирует поле в каждом элементе массива
- [x] тесты обхода: теги внутри `*Struct`, `[]Struct`, `[]*Struct` маскируются
- [x] тесты листа: `At time.Time mask:"secret"` → `"***"`; `Creds Credentials mask:"-"` → удалено целиком
- [x] тест: отсутствующее (`omitempty`) поле не появляется после `Mask`
- [x] тест: незарегистрированное имя → `"***"`; зарегистрированная функция применяется; `mask:"-"` удаляет (регрессия)
- [x] реализовать; добавить функцию «есть ли хоть одно правило» для compile (`Fields.HasRules()` или `len` после фильтрации)
- [x] run tests — must pass before next task

### Task 6: Маскирование input/output в журнале
**Files:**
- Modify: `endpoint.go`, `vatel.go`, `ws_vatel.go`, `carriers.go` (тип результата `OptionalOutput` для правил на compile)
- Modify: `handler_test.go`

- [x] тест: compile без `WithJsonMasker` для `Output[[]T]`, `Output[string]`, `OptionalOutput[T]`, `Body[[]T]`, рекурсивного типа — без panic
- [x] тест журнала: `Body[[]T]` и `Output[[]T]` с полем `mask:"secret"` в `T` → значения в каждом элементе `"***"`
- [x] тесты (журнал в буфер): `LogReqBody`, `LogReqInput`, `LogRespBody`, `LogRespOutput` с полем `mask:"secret"` → `"***"`, ключи журнала прежние; без `WithJsonMasker` тоже маскируется
- [x] тест: тип без тегов `mask` — журнал прежний (ключи и значения)
- [x] тест: ошибка `json.Marshal` ответа (например, `chan` в результате) — в журнале нет объекта ответа
- [x] реализовать masker по умолчанию, флаги `*HasMask`, маскирование `reqInput`/`reqQuery`/`result` (переиспользовать `buf`), путь ошибки marshal
- [x] run tests — must pass before next task

### Task 7: Маскирование path и query параметров
**Files:**
- Modify: `endpoint.go`, `decode.go`
- Modify: `handler_test.go` (или новый `logging_test.go`)

- [x] тесты через `router.New()` + `BuildHandlers`, журнал выхода и журнал ошибки: path-параметр с `mask:"secret"` → `"***"`, с `mask:"-"` — отсутствует, ключ не дублируется
- [x] те же тесты для query-параметров
- [x] тест: значения в структуре входа не изменены; параметры без тега пишутся как раньше
- [x] `fieldInfo.mask`, логирование в `decodeParams`/`decodeURLQuery`, `pathParamKeys` из шаблона пути, пропуск этих ключей в `logEnter`/`logExit`/`writeErrorResponse`
- [x] run tests — must pass before next task

### Task 8: Контекст запроса — Shutdown и таймаут
**Files:**
- Modify: `vatel.go`, `context.go`, `endpoint.go`, `options.go`
- Modify: `handler_test.go`

- [x] тест: `ctx.Context()` в обработчике отменяется после `v.Shutdown(ctx)`; повторный `Shutdown` не паникует
- [x] тест: `WithTimeout(50ms)` — `ctx.Context().Deadline()` задан, обработчик, ждущий `Done()`, получает `DeadlineExceeded`; `WithDefaultTimeout` применяется к эндпоинтам без своего таймаута
- [x] тест: обработчик вернул `context.DeadlineExceeded` → 504, `context.Canceled` → 503
- [x] тест: без таймаута контекст без дедлайна; `NewContext(fctx)` по-прежнему работает
- [x] реализовать базовый контекст в `Vatel`, отмену в `Shutdown`, опции, `cancel` в конце обработки, коды 503/504; исправить комментарий `Context()` в интерфейсе
- [x] run tests — must pass before next task

### Task 9: WS — ошибка разбора входа даёт 400
**Files:**
- Modify: `ws_vatel.go`
- Create: `ws_vatel_test.go`

- [x] тест: `decodeWSInput` с невалидным `data` → ошибка со статусом 400 и кодом `VTL-0007`; с валидным → разобрано
- [x] вынести разбор в `decodeWSInput(data, dest) error`, обернуть в `ErrInvalidRequestBody`
- [x] run tests — must pass before next task

### Task 10: auth — Perms в метаданных, Disabled, nil Authorizer
**Files:**
- Modify: `auth/auth.go`, `options.go` (комментарий `Perms`)
- Modify: `auth/auth_test.go`

- [x] тест: middleware на `OnHandle` видит в `EndpointMeta.Perms` отсортированные имена из `RequirePerms`
- [x] тест: `Config.Disabled = true`, выставленный после создания опций, отключает и `RequirePerms`, и `RequireAuth`
- [x] тест: `Authorizer == nil` → 500, процесс не падает
- [x] реализовать; уточнить комментарий `vatel.Perms` (HTTP — метаданные; WS — требует `WithWsAuthFunc`)
- [x] run tests — must pass before next task

### Task 11: LogSilent, метаданные в OnErrorResponse, удаление неиспользуемого
**Files:**
- Modify: `endpoint.go`, `options.go`, `ws_vatel.go`
- Modify: `handler_test.go`, `carriers_test.go` (TestEndpointOptions)

- [x] тест: `OnErrorResponse` получает Method/Path, когда `OnAuthorization` отвергает запрос
- [x] тесты (журнал в буфер) для `LogSilent`: успех; 4xx из middleware, из разбора, из обработчика — пусто; 5xx — пишется; заголовок `X-Set-Log-Level` не отменяет `LogSilent`
- [x] тесты WS (журнал в буфер, `wsWriteErrorResponse` пишет в `io.Writer`, чтобы тестировать без настоящего соединения): `LogSilent` — 4xx пусто, 5xx пишется
- [x] передать уровень журнала в `runMiddleware`/`writeErrorResponse`/`wsWriteErrorResponse`, всегда `e.EndpointMeta`; реализовать `LogSilent`
- [x] удалить `NoInputLog`, `NoResultLog`, `LanguageLabel`, `WithLanguageLabel`, `atomic.Load` для `LogOptions` (HTTP и WS); обновить затронутые тесты
- [x] run tests — must pass before next task

### Task 12: BuildHandlers без panic, OnTokenUpdate
**Files:**
- Modify: `vatel.go`, `ws_vatel.go`, `example/users_controller.go`
- Modify: `ws_vatel_test.go`

- [x] тесты: WS-эндпоинт без `WithWebsocketSupport` → `BuildHandlers` возвращает ошибку; повторный WS-путь → ошибка; `MustBuildHandlers` в этих случаях паникует
- [x] тест: после неудачной сборки повторный `BuildHandlers` возвращает ошибку «previous build failed», а не регистрирует маршруты частично второй раз
- [x] тест: `OnTokenUpdate(f)` + `UpdateAccessToken` → `f` вызван с payload; ошибка `f` возвращается из `UpdateAccessToken`
- [x] заменить panic на ошибки в `buildHandlers`; добавить `OnTokenUpdate`; убрать неверный комментарий про порядок `/users/me` в примере
- [x] run tests — must pass before next task

### Task 13: Подготовка релиза — зависимости, retract, пример
**Files:**
- Modify: `go.mod`, `go.sum`, `example/main.go`
- Create: `example/cors.go`, `example/cors_test.go`

- [x] заменить `replace github.com/axkit/bitset` на `require github.com/axkit/bitset v1.0.4`, `go mod tidy`
- [x] добавить `retract v1.24.2 // published by mistake, use v1.25.0 or later`
- [x] проверить в чистом окружении: `GOFLAGS=-mod=mod GOWORK=off go build ./... && go test ./...` (дополнительно `go build` с пустым `GOMODCACHE` из proxy.golang.org — ок)
- [x] пример: адрес из переменной `ADDR` (по умолчанию `:8080`); CORS preflight через `mux.GlobalOPTIONS` (`example/cors.go`, тест `example/cors_test.go`; `Access-Control-Allow-Origin` на обычных ответах — middleware `OnRequest`)
- [x] запустить пример на свободном порту и проверить curl: healthcheck 200; плохой токен на публичном эндпоинте 200; без токена на защищённом 401; создание заказа с правами 201; удаление 204; OPTIONS на зарегистрированный путь — CORS-заголовки (порт 18080, наблюдалось: 200, 200, 401, 201, 204; OPTIONS /orders → 204 с `Allow`/`Access-Control-Allow-*`; OPTIONS на неизвестный путь → 404; 401 тоже несёт `Access-Control-Allow-Origin`)

### Task 14: Verify acceptance criteria
- [x] Гостли №2, №4, №6, №7, №10, №11 закрыты или имеют описанный способ; №5 — осознанно нет
- [x] все пункты «Хвосты» и «Контекст запроса» из Solution Overview сделаны
- [x] `go test -race ./...`, `go vet ./...`, `gofmt -l .` (пусто)
- [x] покрытие не ниже базового: `vatel` 54.5%, `auth` 85.5%, `jsonmask` 92.5% (стало: `vatel` 73.4%, `auth` 92.3%, `jsonmask` 96.1%)
- ➕ добавлены тесты на пункты Гостли без прямого теста (№1, №3, №8, №9): `TestHandler_UndecodableBody_400`, `TestHandler_OnAuthorization_RunsBeforeBodyDecode`, `TestMiddleware_SeesEndpointMeta`; код менять не пришлось

**Проверка по пунктам Гостли:**
- №1 тело не JSON → 400: `decodeBody` оборачивает любую ошибку `json.Unmarshal` (синтаксис, тип, лишние данные, свой `UnmarshalJSON`, `time.Time`) в `ErrInvalidRequestBody` — `TestHandler_UndecodableBody_400`, `TestHandler_PostBody_InvalidJSON`, `TestDecodeBody_InvalidJSON`.
- №2 поля ошибки до клиента: журнал не мутирует ошибку (errors v1.1.0), безопасные поля — `WithClientErrorFields` — `TestHandler_WithClientErrorFields`, `TestHandler_ErrorFields_HiddenFromClient`.
- №3 экранирование: errors v1.1.0, ответ — валидный JSON при кавычках и `\` в значении поля и в тексте обёрнутой ошибки (verbose) — `TestHandler_WithClientErrorFields`, `TestHandler_UndecodableBody_400`.
- №4 необязательное тело: `WithOptionalBody()` — `TestHandler_OptionalBody`, `TestHandler_OptionalBody_Validator`, `TestCompile_OptionalBodyOnMethodsWithoutBody`.
- №5 тело DELETE: осознанно нет; query через `QueryParams` на DELETE — `TestHandler_QueryParams_GETAndDELETE`; в MIGRATION.md (раздел выше).
- №6 OPTIONS: `vatel.OPTIONS`, `vatel.HEAD`, CORS через `mux.GlobalOPTIONS` — `TestOPTIONS_CompilesAndServes`, `TestHEAD_InMemoryServer_NoBody`, `TestRouter_GlobalOPTIONS_WithVatelEndpoints`, `example.TestCORSPreflight`.
- №7 плохой токен на публичном эндпоинте: `TestBadTokenOnPublicEndpoint` (auth), `TestHandler_Auth_PublicEndpoint_TokenOptional`.
- №8 точка после проверки токена до разбора тела: позиция `OnAuthorization` — `TestHandler_OnAuthorization_RunsBeforeBodyDecode`.
- №9 описание эндпоинта в middleware: `EndpointMeta` (метод, шаблон пути с префиксом, права) во всех позициях — `TestMiddleware_SeesEndpointMeta`, `TestRequirePermsSetsMetaPerms`, `TestHandler_OnErrorResponse_MetaOnAuthorizationReject`.
- №10 тела в журнале: тег `mask`, masker по умолчанию — `TestHandler_Log_MasksTaggedFields`, `TestHandler_Log_MasksArrayElements`, `TestHandler_Log_MasksOptionalOutput`, `TestHandler_Log_MasksQueryInputs`, `TestHandler_Log_MarshalErrorOmitsResult`.
- №11 параметры пути в журнале: `mask` на path/query-параметрах — `TestLog_PathParamsMasked`, `TestLog_PathParamDecodeError_Masked`, `TestLog_QueryParamsMasked`.

**Проверка «Контекст запроса» и «Хвосты»:**
- базовый контекст, отмена в `Shutdown` (повторный безопасен; `RequestCtx.Done()` не используется) — `TestContext_ShutdownCancelsHandlerContext`, `TestContext_ShutdownWithServer_CancelsAfterDeadline`; `WithTimeout`/`WithDefaultTimeout`, отмена в конце запроса — `TestContext_EndpointTimeout`, `TestContext_DefaultTimeout`, `TestContext_TimeoutCancelledAfterRequest`, `TestContext_NoTimeout_NoDeadline`; 504/503 — `TestContext_ErrorStatus`.
- WS ошибка разбора `data` → 400 `VTL-0007` — `TestDecodeWSInput_Invalid`, `TestDecodeWSInput_Valid`.
- auth: права отсортированно в `Endpoint.Perms` — `TestRequirePermsSetsMetaPerms`; `Disabled` во время запроса — `TestDisabledAfterOptionsCreated`; nil `Authorizer` → 500 — `TestNilAuthorizer`; комментарий `vatel.Perms` уточнён (options.go); `Perms` на WS без `WithWsAuthFunc` — `TestWsCompile_PermsWithoutAuthFunc_Errors`.
- `LogSilent` (HTTP и WS), метаданные в `OnErrorResponse` — `TestHandler_LogSilent`, `TestWsWriteErrorResponse_LogSilent`, `TestHandler_NotSilent_4xxLogged`, `TestHandler_OnErrorResponse_MetaOnAuthorizationReject`.
- удалены `NoInputLog`, `NoResultLog`, `LanguageLabel`, `WithLanguageLabel`, `atomic.` — grep по `*.go` пуст.
- `BuildHandlers` без panic, «previous build failed», комментарий в `BuildHandlers` — `TestBuildHandlers_WSErrors`, `TestBuildHandlers_AfterFailedBuild`, `TestBuildHandlers_Twice`.
- `OnTokenUpdate` — `TestOnTokenUpdate`.

### Task 15: [Final] Update documentation
- [x] README.md: быстрый старт на новом API, carriers (`Validate()` на именованном типе), middleware, `auth`, `WithClientErrorFields`, `WithOptionalBody`, маскирование `mask` (параметры → `"***"`), контекст и таймауты (что таймаут не прерывает обработчик сам), CORS через `mux.GlobalOPTIONS` (только зарегистрированные пути; не ставить `Require*` на OPTIONS)
- [x] MIGRATION.md (v0.14 → v1.25): соответствие старого и нового API + все пункты раздела «Для MIGRATION.md» этого плана; проверить, что каждый пункт раздела перенесён
- [x] CLAUDE.md — новая архитектура; уровень журнала по умолчанию 0 (только ошибки), а не `LogFullOnExit`
- [x] move this plan to `docs/plans/completed/` (done by orchestrator after reviews)
- ➕ в MIGRATION.md добавлены отличия от v0.14.1, найденные сравнением с `git show v0.14.1:*` (не из раздела «Для MIGRATION.md»): без токена на защищённом эндпоинте теперь `VTL-0004` вместо `VTL-0001`; у `ErrForbidden` нет полей `user`/`role`; `reqId` только с `WithRequestID()` (в v0.14 всегда); `Result()` = nil → нет тела (было `null`); ошибка разбора query → 400 `VTL-0006` (было 500); ошибка `OnSuccessResponse` игнорируется; `OnErrorResponse` в v0.14 паниковал при добавлении; убран `?description=true`; `X-Set-Log-Level` в v0.14 не обрабатывал запрос

## Post-Completion
*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Релиз (только с подтверждением пользователя — необратимо):**
- слить `refactor` в `main` (PR), поставить тег `v1.25.0`, запушить
- проверить в прокси: `go list -m -versions github.com/axkit/vatel` — v1.25.0 есть; `go list -m -retracted -f '{{.Retracted}}' github.com/axkit/vatel@v1.24.2` — отозвана; `go get github.com/axkit/vatel@latest` в чистом модуле выбирает v1.25.0
- GitHub release с текстом из MIGRATION.md

**Внешние проекты:**
- сообщить сессии Гостли (app-fa) версию и ссылку на MIGRATION.md: что закрыто, какие обходы можно убрать (`core.InstallBodyDecodeCheck`, ручная запись `suggestedName`, CORS вне vatel), что им нужно сделать (переход на errors v1.1.0 — их backlog `vatel-upgrade-errors-v1.md`, `WithClientErrorFields("reason", "suggestedName")`, `WithOptionalBody()` для POST …/ticket)
