# Лаба 3 — Платформа для shop: заметки к отчёту

> **Где остановились (17.10.2026, ~00:40):**
> - ✅ Части 0–5 — полностью (в Части 3 проверь, что есть скриншот `kubectl get cluster shop-db -n shop -o yaml`).
> - ⏭ Осталось: README для сдачи из этого файла + вставить скриншоты.
> - Перед продолжением: запустить Docker Desktop → `minikube start` (всё установленное сохранится).

> Черновик. Сюда по ходу работы записываются ключевые факты, команды и выводы.
> Пометка 📸 — в этом месте нужно сделать скриншот.

Цель лабы
Собрать на локальном кластере платформу, которая защищает себя сама. Политики не пускают в кластер деплои с нарушениями. Helm-чарт shop проходит эти политики. Postgres обслуживает оператор, а не человек. И нужно руками проверить, что продолжает работать, когда падает control plane.

Этапы
Сервисы и окружение — сервисы готовы, окружение пока нет (см. ниже)
Ограждения: движок политик, 5+ правил и 5 манифестов, которые должны отклоняться
Helm-чарт для api/worker: reconciliation, rolling update, сломанный релиз и rollback
Postgres через оператора: CRD, spec/status, сравнение с controller-manager
Ронять control plane
Мониторинг: ServiceMonitor, PrometheusRule, 3 алерта, хотя бы один в состоянии firing

## Окружение

- Кластер: minikube v1.35.1 (k8s), драйвер docker (Docker Desktop 29.8, Apple Silicon), 1 нода, 4 CPU / 7 ГБ.
- Helm v4.3.1.
- Часть 5 (мониторинг) отложена до готовности стека из Лабы 2.

## Часть 0 — Сервисы

Код: `services/` (Go), один `Dockerfile` на оба сервиса (`--build-arg APP=api|worker`).

| Сервис | Эндпоинты | Особенности |
|---|---|---|
| `api` | `GET /health`, `POST /order`, `GET /orders`, `GET /version`, `GET /metrics` | `HEALTH_FAIL=true` → `/health` отвечает 500; graceful shutdown по SIGTERM |
| `worker` | `GET /health`, `GET /metrics` (порт 8080) | раз в 5 с переводит заказы `new → processed`, `FOR UPDATE SKIP LOCKED` — реплики не обрабатывают один заказ дважды |

- Подключение к БД — через переменную `DATABASE_URL`; схема создаётся сама, с ретраями, если postgres ещё не готов.
- Образ: multi-stage, финальный слой `distroless/static:nonroot`, ~14 МБ, запуск от UID 65532 (не root) — пригодится для политик.
- Образы собраны прямо в docker внутри minikube (`eval $(minikube docker-env)`), поэтому registry не нужен:
  - `registry.shop.local/api:1.0.0`, `registry.shop.local/api:1.1.0` (для rolling update)
  - `registry.shop.local/worker:1.0.0`
  - `registry.shop.local` — «внутренний доверенный реестр» магазина (на него завяжем правило в Части 1).

## Часть 1 — Ограждения на кластер

### Где в кластере происходит проверка (теория)

Любой `kubectl apply` / `helm install` — это HTTP-запрос к kube-apiserver. Перед записью в etcd он проходит цепочку:

```
запрос → аутентификация → авторизация (RBAC)
       → mutating admission     (можно изменить объект)
       → валидация по схеме
       → validating admission   (можно только сказать «да» или «нет»)
       → запись в etcd
```

Kyverno и Gatekeeper встраиваются в эту цепочку как **admission webhooks**: регистрируют объекты
`ValidatingWebhookConfiguration` / `MutatingWebhookConfiguration`, в которых apiserver прописано
«перед записью объектов такого-то вида отправь их на проверку в этот сервис». Если webhook ответил «нет» —
объект **не попадает в etcd**, а значит ни один контроллер его не увидит и ни один под не создастся.

**Вывод:** политика срабатывает **в момент записи объекта**, а не во время работы пода.

### Выбор движка: Kyverno

| | Kyverno | OPA/Gatekeeper |
|---|---|---|
| Язык правил | YAML (обычные k8s-манифесты) | Rego (отдельный язык) |
| Что умеет | validate + mutate + generate + проверка подписей образов | в основном validate (mutation ограниченная) |
| Сильная сторона | быстрый старт, привычный формат | сложная логика, единый язык политик для k8s и вне его (Terraform, API) |

**Обоснование выбора:** Kyverno не требует изучать отдельный язык — правила пишутся обычным YAML
в привычном формате Kubernetes-манифестов, что даёт быстрый старт. Сложная логика, ради которой стоит брать
Gatekeeper/Rego, для набора правил этой лабы не нужна.

### Правила (план)

| # | Правило | Польза |
|---|---|---|
| 1 | Обязательные `requests`/`limits` CPU и памяти | без лимитов один под может съесть ресурсы ноды и задеть соседей; без requests планировщик не знает, сколько места под нужно |
| 2 | Образы только из доверенного реестра `registry.shop.local/` | в кластер не попадёт непроверенный образ из публичного Docker Hub (supply chain) |
| 3 | Обязательные метки (`app.kubernetes.io/name`, …) | по меткам работают Service-селекторы, мониторинг, учёт владельцев; без них объект «ничей» |
| 4 | Запрет `privileged: true` | privileged-контейнер получает все capabilities и доступ к устройствам хоста — фактически root на ноде |
| 5 | Запрет запуска от root (`runAsNonRoot: true`) | при взломе приложения у атакующего не будет UID 0, это ограничивает последствия (в т.ч. при побеге из контейнера) |

**privileged и root — это разные вещи:**
- `privileged` — это про границу между контейнером и хостом: снимается почти вся изоляция (все capabilities, `/dev` хоста, никакого seccomp/AppArmor).
- `root` — это UID 0 *внутри* контейнера. Даже без privileged root в контейнере — это лишние права: запись в любые файлы образа, привязка к низким портам, и если ядро уязвимо — после побега это UID 0 на хосте (без user namespaces).

**Важный нюанс правила 5:** Kyverno видит только **спецификацию пода**, а не содержимое образа.
То, что в Dockerfile стоит `USER 65532`, Kyverno не знает → в манифесте нужно явно прописать
`securityContext.runAsNonRoot: true`. А дальше уже **kubelet** при старте контейнера сверит это с
метаданными образа и откажется запускать, если образ всё-таки стартует от root (ошибка
`CreateContainerConfigError`). Т.е. проверка двухуровневая: Kyverno — при записи, kubelet — при запуске.

### Установка Kyverno

```bash
helm repo add kyverno https://kyverno.github.io/kyverno/
helm install kyverno kyverno/kyverno -n kyverno --create-namespace --wait
kubectl get pods -n kyverno
kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations
```

📸 поды Kyverno в `Running` + список webhook-конфигураций.

Kyverno — это 4 контроллера:
- `admission-controller` — тот самый webhook, который apiserver вызывает при записи объекта;
- `background-controller` — применяет политики к уже существующим объектам (mutate/generate);
- `reports-controller` — фоновое сканирование и `PolicyReport` (отчёты о нарушениях);
- `cleanup-controller` — удаление ресурсов по политикам/TTL.

Версия: chart `kyverno-3.9.1`, Kyverno `v1.19.1`.

**Наблюдение:** в ConfigMap `kyverno` (`resourceFilters`) по умолчанию уже исключены namespace
`kube-system` и `kyverno` — Kyverno их вообще не проверяет, чтобы не заблокировать восстановление кластера.

**Наблюдение:** сразу после установки у `kyverno-resource-validating-webhook-cfg` **0 webhooks** —
Kyverno регистрирует webhook для конкретных видов ресурсов динамически, только когда появляются политики.
Пока политик нет, apiserver вообще не ходит в Kyverno за подами.

### Как политика ведёт себя в разных ситуациях (разбор вопросов)

**1. Кто увидит ошибку, если Pod из Deployment нарушает политику?**
Pod создаёт не пользователь, а контроллер ReplicaSet (в controller-manager). Если бы политика проверяла
только Pod, то `kubectl apply` Deployment прошёл бы успешно (сам Deployment ничего не нарушает),
ReplicaSet бы создался, а отказ получил бы контроллер — ошибка видна только в событиях ReplicaSet
(`FailedCreate`), Deployment висит в `0/3`. Пользователь узнает о проблеме не сразу.
Kyverno решает это **autogen**: правило, написанное для `Pod`, автоматически дублируется для
`Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob` (проверяется `spec.template`).
Поэтому отказ приходит **сразу на `kubectl apply` / `helm install`** — пользователю в консоль.

**2. Что будет с уже работающими подами при включении новой политики?**
Ничего — они продолжат работать. Admission срабатывает только на **запросы записи** (CREATE/UPDATE),
а поды уже лежат в etcd, и их никто заново не проверяет. Kyverno в фоне отметит их в `PolicyReport`
как нарушителей, но не тронет. Подвох — «отложенная бомба»: как только такой под пересоздастся
(упала нода, rollout, eviction), новый под уже не пройдёт admission. Поэтому в проде новые политики
сначала включают в режиме `Audit` (только отчёт), чистят нарушения, и только потом переводят в `Enforce`.

**3. Что если упал под Kyverno?** У webhook есть `failurePolicy`:
- `Fail` — apiserver отклоняет запрос, если webhook недоступен. Безопасно, но блокирует **все** деплои
  этих видов ресурсов, включая легальные и системные; в худшем случае — сам Kyverno не может пересоздаться.
- `Ignore` — apiserver пропускает запрос без проверки. Кластер работает, но защита **молча исчезает**:
  нарушающие объекты попадут в etcd, и никто об этом не узнает.

**4. Диагностика «apply сказал OK, а подов нет»:**
`kubectl get deploy` (видно `0/N`) → `kubectl describe rs <имя>` / `kubectl get events --sort-by=.metadata.creationTimestamp`
→ событие `FailedCreate` с текстом нарушенной политики.
Почему причины нет в выводе `apply`: `apply` сообщает только о записи **самого Deployment**, а он валиден.
Почему пусто в `get pods`: отклонённые поды **никогда не попадали в etcd** — показывать нечего.
По той же причине их нет и в `PolicyReport`: отчёты строятся по **существующим** объектам, а отказы
admission видны только в событиях (и в логах Kyverno).

Именно поэтому из-под политик исключают системные namespace (`kube-system`, `kyverno`) — чтобы
сбой webhook или слишком строгое правило не заблокировали восстановление самого кластера.

**Почему не требуем `limits.cpu`:** жёсткий CPU limit (CFS quota) может вызывать throttling даже при
наличии свободных ресурсов на ноде; при этом `requests.cpu` сохраняет информацию, необходимую планировщику
для размещения Pod. Память — несжимаемый ресурс (её нельзя «притормозить», только убить по OOM),
поэтому `limits.memory` обязателен.

### Остальные политики — ключевые приёмы

- **`disallow-privileged`**: поле `privileged` обычно отсутствует (= не privileged), поэтому pattern
  `privileged: false` без якоря отклонил бы нормальные поды. Нужен условный якорь
  `=(securityContext): =(privileged): false` — «если поле есть, то false».
- **`trusted-registry`** и **`disallow-privileged`** проверяют не только `containers`, но и
  `=(initContainers)`, `=(ephemeralContainers)` — иначе чужой образ / privileged можно протащить
  через init-контейнер или `kubectl debug`.
- **`require-run-as-non-root`**: `runAsNonRoot` можно задать на уровне пода или каждого контейнера →
  `anyPattern` (достаточно совпадения одного шаблона).
  - **Найденная дыра (v1):** под с `spec.securityContext.runAsNonRoot: true` и контейнером, который
    переопределил `runAsNonRoot: false`, проходил первый шаблон — контейнер мог стартовать от root.
    Закрыто: первый шаблон проверяет ещё и контейнеры.
  - **Ложное срабатывание (v2):** якорь `X(runAsNonRoot)` означает «поле должно **отсутствовать**»
    при любом значении — легальный под (уровень пода `true`, у одного контейнера тоже явно `true`,
    у второго не задано) отклонялся. Урок: правило безопасности нужно проверять в обе стороны —
    и «плохое не проходит», и «хорошее проходит».
  - **Итог (v3):** в первом шаблоне у контейнеров условный якорь `=(securityContext): =(runAsNonRoot): true`
    («если поле есть — оно true»). Проверено `--dry-run=server`: обход с переопределением → отказ,
    легальный под с sidecar → принят.
  - Попутный урок: исправленный файл не действует, пока не сделан `kubectl apply` — Kyverno работает
    с объектом политики в etcd, а не с файлом на диске (источник правды — кластер).
- Ограничение: `require-resources` и `require-run-as-non-root` не проверяют `initContainers` — для лабы допустимо.

### Проверка: 5 манифестов-нарушителей (`manifests/`)

Методика: сначала эталон `good-pod.yaml`, проходящий все 5 политик (контрольный образец — если отклоняется
он, ошибка в эталоне или политике). Затем 5 копий, в каждой сломана **ровно одна** вещь — иначе по отказу
нельзя доказать, что сработало именно нужное правило.

| Манифест | Что сломано | Кто отклонил |
|---|---|---|
| `good-pod.yaml` | ничего | принят, `Running` (kubelet согласен с `runAsNonRoot` для distroless-образа) 📸 |
| `bad-no-limits.yaml` | нет `resources` | `require-resources` |
| `bad-registry.yaml` | `nginx:1.27` из Docker Hub | `trusted-registry` |
| `bad-no-labels.yaml` | нет меток | `require-labels` |
| `bad-privileged.yaml` | `privileged: true` | `disallow-privileged` |
| `bad-root.yaml` | `runAsNonRoot: false` | `require-run-as-non-root` |

```bash
kubectl apply -f manifests/      # 📸 good-pod создан, 5 отказов — у каждого своя политика
```

Каждый манифест отклонён **ровно одной** политикой (проверено `--dry-run=server`).
`privileged: true` + `runAsNonRoot: true` — сам apiserver такую комбинацию допускает (privileged не
требует root: процесс без root не получит capabilities, но получит доступ к устройствам хоста и снятые
seccomp/AppArmor), поэтому отказ в `bad-privileged` — честная заслуга Kyverno, а не валидации API.

### Политика 1: обязательные метки (`policies/require-labels.yaml`)

Требуем `app.kubernetes.io/name` (что за приложение) и `app.kubernetes.io/managed-by` (чем управляется).
Синтаксис `pattern` — «трафарет», наложенный на объект; `"?*"` = поле есть и не пустое.

```bash
kubectl apply -f policies/require-labels.yaml
kubectl get clusterpolicy                     # READY = True
kubectl get validatingwebhookconfiguration kyverno-resource-validating-webhook-cfg   # WEBHOOKS: 0 → 1
```

- **Webhook появился сам** (0 → 1) — apiserver настраивает Kyverno, а не человек: описываешь правило,
  Kyverno сам регистрирует нужные webhook-конфигурации.
- **Autogen** (`kubectl get clusterpolicy require-labels -o yaml`, секция `status.autogen`): правило для `Pod`
  размножено на DaemonSet, Deployment, Job, ReplicaSet, ReplicationController, StatefulSet — с путём
  `spec.template.metadata.labels`, т.е. проверяется **шаблон будущих подов** ещё до их создания;
  для CronJob — отдельное правило (`spec.jobTemplate.spec.template...`).
- Нюанс: метку `managed-by` Helm сам **не ставит** — её должен проставить шаблон чарта.
- Цена правила: быстрый отладочный `kubectl run` без меток тоже будет отклоняться.
- `kyverno.io/v1 ClusterPolicy` в Kyverno 1.19 помечен deprecated — проект переходит на политики на CEL
  (`ValidatingPolicy`, тот же язык, что во встроенном `ValidatingAdmissionPolicy` Kubernetes).
  Пока полностью работает; для лабы используем его — это и есть «правила как обычный YAML».

**Живая иллюстрация «существующие поды не перепроверяются»:** под `test` был создан **до** применения
политики — он продолжил работать (admission его уже не касается), а фоновое сканирование отметило его
в отчёте: `kubectl get policyreport -n default` → `Pod test FAIL 1`. 📸
После `kubectl delete pod test` повторный `kubectl run test --image=registry.shop.local/api:1.0.0` → отказ. 📸

**Фон сканирует и системные namespace:** событие `PolicyViolation ... Deployment kube-system/coredns` —
фильтр `resourceFilters` в ConfigMap защищает `kube-system` только на admission, а background scan
их всё равно оценивает. Поэтому в каждой политике явно добавлен блок `exclude` для `kube-system` и `kyverno`.

### Итоги Части 1: что будет с Helm-чартом, нарушающим политику

| | autogen **включён** (по умолчанию) | autogen **выключен** (`pod-policies.kyverno.io/autogen-controllers: none`) |
|---|---|---|
| Кого отклоняет Kyverno | сам **Deployment** (проверяется `spec.template`) | только **Pod**, который создаёт контроллер ReplicaSet |
| `helm install` | **ошибка** сразу, текст политики в консоли | «успех» (с `--wait` — падение **по таймауту**) |
| Объекты | Deployment не попал в etcd → нет ни RS, ни подов | Deployment + 1 ReplicaSet + **0 подов** |
| Где искать причину | в выводе `helm install` | `kubectl describe rs` / `kubectl get events` → `FailedCreate` |

**Дыра от исключения `kube-system`:** кто может создавать поды в `kube-system`, тот обходит все политики
(например, запускает там privileged-под). Закрывается **RBAC** — права на `kube-system` только у админов и
системных компонентов. Дополнительный слой — встроенный **Pod Security Admission**
(метка `pod-security.kubernetes.io/enforce` на namespace): работает внутри apiserver без webhook,
поэтому не отключается падением Kyverno.

## Часть 2 — Чарт api и worker

### Модель: что делает Helm, а что — Kubernetes

```
values.yaml + templates/  ──(helm рендерит локально)──►  обычные YAML-манифесты
                                                         │ apply в apiserver
                                                         ▼
             admission (Kyverno) → etcd → контроллеры (Deployment, ReplicaSet) → поды
```

- Helm — **шаблонизатор + учёт релизов**, а не контроллер: за кластером не следит и ничего не чинит.
  Reconciliation (вернуть удалённый под, догнать `replicas`) делают **контроллеры Kubernetes**.
- История релизов хранится в кластере — Secret-ы `sh.helm.release.v1.<релиз>.v<N>`; на них опирается `helm rollback`.
- Для Kyverno запрос от Helm ничем не отличается от `kubectl apply` → требования политик закладываются прямо в шаблоны.

### Структура чарта (`chart/shop/`)

`values.yaml` — «интерфейс» чарта, решения:
- `registry` вынесен в одно поле — при смене реестра правится одна строка (и он же совпадает с правилом `trusted-registry`);
- `image.tag` у каждого сервиса — rolling update делается сменой одной строки;
- общий `securityContext` контейнеров: `runAsNonRoot: true` (политика 5), плюс сверх политик
  `allowPrivilegeEscalation: false` и `capabilities.drop: [ALL]` — процесс не может повысить права через setuid
  и не имеет даже базовых capabilities (нашему Go-сервису они не нужны);
- `env` — список, а не словарь: так можно передавать не только `value`, но и `valueFrom.secretKeyRef`
  (понадобится для пароля БД в Части 3).

Шаблоны: `api-deployment.yaml`, `api-service.yaml`, `worker-deployment.yaml`, `worker-service.yaml`.

- Метки пода (`spec.template.metadata.labels`) — именно их проверяет политика через autogen;
  `managed-by: {{ .Release.Service }}` → `Helm`.
- **`selector` неизменяем** после создания Deployment → в нём только стабильные метки (`name`, `component`,
  `instance`), никакой версии/тега, иначе следующий `helm upgrade` упадёт. Селекторы `api` и `worker`
  различаются по `component` — Service `api` не шлёт трафик на поды worker.
- **Известное ограничение (осознанный компромисс):** в селекторе нет `app.kubernetes.io/instance: {{ .Release.Name }}`.
  Поэтому в одном namespace можно ставить только **один** релиз чарта — второй релиз имел бы те же метки,
  и Service/Deployment одного релиза захватывали бы поды другого. Для лабы (один релиз `shop` в namespace `shop`)
  это допустимо; добавить метку позже можно только с пересозданием Deployment (selector неизменяем).

Проверка до установки — рендер + Kyverno без записи в кластер:
```bash
helm lint chart/shop
helm template shop chart/shop | kubectl apply --dry-run=server -f -
```
Результат: все объекты прошли 5 политик с первого раза (требования заложены в шаблоны заранее).

### Пробы: readiness vs liveness

| | readiness | liveness |
|---|---|---|
| Кто выполняет | kubelet | kubelet |
| Реакция на провал | под → `NotReady`, контроллер EndpointSlice убирает его из endpoints Service → трафик не идёт; контейнер **продолжает работать** | после `failureThreshold` провалов kubelet **перезапускает контейнер** |
| Смысл | «можно ли слать трафик» | «процесс жив и не завис» |

- Service сам пробы не выполняет — он опирается на готовность пода (через EndpointSlice).
- Наш `/health` **не ходит в БД** — правильно для liveness: иначе падение postgres привело бы к перезапуску
  всех подов api (каскадный отказ), и перезапуск ничего бы не исправил.
- Прогноз для `HEALTH_FAIL=true`: обе пробы падают → под `0/1` не Ready, плюс liveness перезапускает
  контейнер → растёт `RESTARTS`, затем `CrashLoopBackOff` (экспоненциальная задержка между рестартами).
- Почему в selector нельзя класть тег: `spec.selector` неизменяем, `helm upgrade` с новым тегом упадёт
  с `field is immutable`.

### Установка

```bash
helm install shop chart/shop -n shop --create-namespace
kubectl get deploy,rs,pods,svc -n shop
kubectl get secret -n shop        # sh.helm.release.v1.shop.v1 — история релиза
```
📸 установка прошла без единого отклонения политиками, `shop-api 3/3`, `shop-worker 2/2`.

- У ReplicaSet в селекторе есть метка `pod-template-hash=…`, которой нет в шаблоне чарта — её добавляет
  **Deployment-контроллер** (хэш от `spec.template`), чтобы ReplicaSet-ы разных версий не пересекались селекторами.
- В `managedFields` Deployment видно владельцев полей: `helm  Apply` (Helm 4 использует server-side apply) и
  `kube-controller-manager  Update  status` — контроллер пишет только в `status`. Модель `spec` (желаемое, пишет
  пользователь) / `status` (фактическое, пишет контроллер).

### Reconciliation на чарте

**Удаление пода api руками** (`kubectl delete pod -n shop shop-api-…`):
```
Killing           pod/shop-api-6c8fd6b68b-6wbdv   Stopping container api
SuccessfulCreate  replicaset/shop-api-6c8fd6b68b  Created pod: shop-api-6c8fd6b68b-6jh8x
```
📸 Вернулся **новый** под (другое имя и IP). Его создал **ReplicaSet-контроллер**: он держит watch на поды
(подписка на изменения, а не опрос), получает событие удаления и догоняет число подов до `spec.replicas`.
Helm в этом не участвует — он не контроллер.

**Смена `replicas` 3 → 5 в `values.yaml` + `helm upgrade`:**
```
ScalingReplicaSet  deployment/shop-api  Scaled up replica set shop-api-6c8fd6b68b from 3 to 5
```
📸 Helm лишь отрендерил манифест и записал в Deployment `spec.replicas: 5`; поды создали контроллеры
(Deployment → ReplicaSet → Pod). `helm history` — новая ревизия на каждый upgrade.

Наблюдения по ходу:
- изменённый, но **несохранённый / неприменённый** `values.yaml` на кластер не влияет — Helm читает values
  только в момент `install`/`upgrade`;
- `helm upgrade` **без изменений** всё равно создаёт новую ревизию — Helm фиксирует каждую попытку;
- ручной `kubectl scale` Helm сам не откатит — он не следит за кластером; дрейф устраняется только следующим
  `helm upgrade` (если значение в чарте отличается от живого).

### Rolling update без потерь (1.0.0 → 1.1.0)

**Стратегия.** Умолчания `maxSurge: 25%` (округление вверх) и `maxUnavailable: 25%` (вниз) при `replicas: 5`
дают surge = 2, unavailable = 1 → одновременно до 7 подов, из них готовых может быть всего 4; и обновление идёт
не по одному. Поэтому в шаблоне `api` задано:
```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxSurge: 1        # новые поды поднимаются строго по одному
    maxUnavailable: 0  # готовых подов никогда не меньше replicas
```

**Гонка при остановке пода.** При удалении пода параллельно и несинхронизированно происходят:
(1) kubelet шлёт SIGTERM → приложение делает graceful shutdown и перестаёт принимать соединения;
(2) контроллер EndpointSlice убирает под из endpoints → kube-proxy обновляет правила на нодах.
Если (1) успевает раньше (2), kube-proxy ещё шлёт запросы на под, который их не принимает → `connection refused`.
Решение — `preStop`-хук «подожди перед SIGTERM»:
```yaml
lifecycle:
  preStop:
    sleep:
      seconds: 5
```
Классический `exec: ["sleep","5"]` здесь **не сработал бы** — в distroless-образе нет бинарника `sleep`.
`preStop.sleep` — встроенное действие kubelet (не требует ничего в образе).

**Порядок важен:** preStop выкатывается **отдельным** upgrade до смены тега — хук есть только у подов, созданных
из нового шаблона, а в rollout удаляются старые поды, у которых его нет.
Этот upgrade сам вызвал rollout: `preStop` — часть `spec.template` → изменился `pod-template-hash` → новый
ReplicaSet. Изменение одной лишь `strategy` (она вне шаблона) нового ReplicaSet не создало бы.

**Проверка.** Нагрузка — цикл curl на ClusterIP Service изнутри ноды (`minikube ssh`): `port-forward` для этого
не подходит — он цепляется к одному поду и рвётся при его удалении.
```bash
minikube ssh -- 'ok=0; fail=0; while true; do code=$(curl -s -o /dev/null -w "%{http_code}" -m 1 http://<ClusterIP>:8080/health); ...'
kubectl get rs -n shop -l app.kubernetes.io/component=api -o wide -w
helm upgrade shop chart/shop -n shop      # после image.tag: "1.1.0" в values.yaml
```
📸 счётчик `ok=… fail=…` + `get rs -w`.

Результат: RS `…1.0.0` уменьшался по одному (`5→4→3→2→1→0`), новый RS `…1.1.0` рос по одному; события
`ScalingReplicaSet` чередуются «+1 новый / −1 старый». Итог: `shop-api-d8bc87ff4  5/5/5  api:1.1.0`.
Старые ReplicaSet-ы остались с 0 реплик — это история Deployment (`revisionHistoryLimit: 10`), на неё опирается
`kubectl rollout undo` (механизм отката, независимый от Helm).

**Счётчик за время rolling update: `ok=1117 fail=0`** — ни один запрос не провалился. 📸

За это отвечают вместе: `maxUnavailable: 0` (готовых подов всегда ≥ 5), readiness-проба (трафик идёт только на
готовые новые поды), `preStop.sleep` (старый под уходит из endpoints до SIGTERM) и graceful shutdown в приложении
(уже принятые запросы дорабатываются).

### Сломанная версия (`HEALTH_FAIL=true`) и `helm rollback`

**Попутная ошибка формата (ревизия 8, `failed`):** `env: [HEALTH_FAIL = "true"]` — это список из одной *строки*,
а `env` требует список объектов `{name, value}` → apiserver отклонил манифест ещё при построении патча
(`env: element 0: associative list with keys may not have non-map elements`). Объект не записан, в кластере
осталась ревизия 7. Это **структурная** ошибка — от неё защищает сам apiserver. Правильно:
```yaml
env:
  - name: HEALTH_FAIL
    value: "true"     # кавычки обязательны: value в env — строка
```

**Сломанная версия (ревизия 9)** — структурно корректна, apiserver и Kyverno её приняли; сломанность видна только
в runtime. От этого защищают readiness + стратегия, а не admission:
```
shop-api-64c479c5ff   DESIRED 1  CURRENT 1  READY 0   ← новый, сломанный
shop-api-d8bc87ff4    DESIRED 5  CURRENT 5  READY 5   ← старый, исправный

Readiness probe failed: HTTP probe failed with statuscode: 500
Liveness probe failed:  HTTP probe failed with statuscode: 500
Container api failed liveness probe, will be restarted
```
📸 `kubectl rollout status ... --timeout=60s` → `1 out of 5 new replicas have been updated... timed out`.

- Создан **ровно один** новый под (`maxSurge: 1`), он не прошёл readiness → **ни один старый не удалён**
  (`maxUnavailable: 0`); сервис всё время отвечал (`fail=0` в цикле curl).
- liveness перезапускает новый под → растёт `RESTARTS`, затем `CrashLoopBackOff`.
- Kubernetes **сам не откатывает**: через `progressDeadlineSeconds` (600 с) Deployment лишь получает условие
  `ProgressDeadlineExceeded`. Решение об откате — за человеком/CI.
- **`helm upgrade` ответил успехом** (ревизия 9 — `Upgrade complete`): Helm 4 без `--wait` не ждёт готовности
  подов, «успех» = apiserver *принял* объекты. В CI нужен `helm upgrade --wait` (или `--atomic` — автооткат
  при таймауте), иначе пайплайн зелёный при сломанном релизе.

**Откат:**
```bash
helm rollback shop 7 -n shop
```
📸
```
9    superseded  Upgrade complete
10   deployed    Rollback to 7
```
- Откат — **новая ревизия** (10), а не «возврат к номеру 7»: история Helm — журнал операций, только дописывается.
- Откат прошёл **мгновенно**: шаблон ревизии 7 совпадает по `pod-template-hash` с ReplicaSet `d8bc87ff4`, который
  всё время держал 5 готовых подов → контроллер переиспользовал его и уменьшил сломанный RS до 0, новых подов не создавал.
- `helm rollback` восстанавливает релиз в кластере, но **не трогает локальный `values.yaml`**: если там остался
  `HEALTH_FAIL=true`, следующий `helm upgrade` снова сломает релиз. Поэтому после отката значение убрано
  из файла (`env: []`) — источник правды (файлы чарта) приведён в соответствие с кластером.

## Часть 3 — Postgres через оператора

### Что такое оператор

```
оператор = CRD (новый вид объекта, например Cluster)
         + контроллер (свой reconcile loop со знаниями, как эксплуатировать postgres)
```
Пишешь `kind: Cluster` (`instances`, `storage`) — контроллер оператора превращает это в поды, PVC, Service-ы,
Secret-ы с паролями, настраивает репликацию и делает failover. То, что раньше делал DBA руками.

**Выбор: CloudNativePG (CNPG)** — управляет жизненным циклом кластера PostgreSQL через CRD, поддерживает
автоматическое восстановление и упрощает управление базой; наглядно показывает reconciliation — оператор
приводит фактическое состояние к описанному в манифесте. (Альтернатива — Zalando postgres-operator: поверх
Patroni и StatefulSet, тяжелее; CNPG управляет подами напрямую, без StatefulSet, и сам создаёт Secret с `uri`.)

### Конфликт оператора с ограждениями Части 1

Проверка до установки (рендер чарта оператора + Kyverno без записи):
```bash
helm template cnpg cnpg/cloudnative-pg -n cnpg-system | kubectl apply --dry-run=server -n cnpg-system -f -
```
Результат — под оператора блокируют **3 политики из 5**:
```
require-labels:    нет app.kubernetes.io/managed-by
require-resources: нет limits.memory
trusted-registry:  образ ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1
```
`disallow-privileged` и `require-run-as-non-root` проходят — CNPG по умолчанию уже non-root и без privileged.

Варианты решения:
| Подход | Минус |
|---|---|
| исключить namespace оператора/БД из политик | обход политик для целого namespace — любой под там без проверок |
| добавить `ghcr.io/cloudnative-pg/*` в доверенные реестры | растёт поверхность доверия: доверяем чужому реестру целиком |
| **перезалить образы в `registry.shop.local` и привести поды в соответствие** ✅ | больше работы: зеркалировать образы при каждом обновлении |

Выбран третий: сохраняется единый доверенный реестр, политики не ослабляются.
В чарте CNPG есть нужные настройки: `image.repository`, `resources`, `podLabels`.

### Установка оператора (один раз на кластер)

Зеркалирование образа в «наш реестр» (docker внутри minikube):
```bash
eval $(minikube docker-env)
docker pull ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1
docker tag  ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1 registry.shop.local/cloudnative-pg:1.30.1
```
`operator/cnpg-values.yaml`:
```yaml
image:
  repository: registry.shop.local/cloudnative-pg
resources:
  requests: {cpu: 100m, memory: 128Mi}
  limits:   {memory: 256Mi}
podLabels:
  app.kubernetes.io/managed-by: Helm
```
```bash
helm install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace -f operator/cnpg-values.yaml --wait
kubectl get pods -n cnpg-system
kubectl get crd | grep cnpg
```
📸 Оператор `Running`, ни одного отказа политик; появились CRD `clusters.postgresql.cnpg.io` и др.

Оператор ставится **отдельно от чарта `shop`**: он один на кластер, его CRD — кластерные объекты, а жизненный
цикл (обновления оператора) не связан с релизами приложения. Чарт `shop` только создаёт объект `Cluster`.

**Autogen не знает о CRD:** Kyverno распространяет Pod-политики только на встроенные контроллеры
(Deployment, StatefulSet, Job…). `kind: Cluster` для него — незнакомый объект без `spec.template`. Если в `Cluster`
забыть `resources`, `helm install` пройдёт, `Cluster` создастся, а поды postgres будут отклонены уже при создании
их оператором. Искать: `kubectl describe cluster`, `status` объекта, события namespace, логи оператора.

### Объект `Cluster` в чарте (`templates/postgres-cluster.yaml`)

Образ postgres тоже зеркалирован (`registry.shop.local/postgresql:18.6-system-trixie`). В шаблоне заложены
требования политик (значения — в секции `postgres:` в `values.yaml`):
- `imageName` из нашего реестра → `trusted-registry`;
- `resources` (requests cpu/memory, limits memory) → `require-resources`;
- `inheritedMetadata.labels` — оператор копирует эти метки на всё, что создаёт → `require-labels`;
- non-root и не privileged — CNPG так работает по умолчанию.

Замечание: метка `managed-by: Helm` на подах postgres проходит политику, но по смыслу неточна — поды создаёт и
обслуживает оператор, Helm создал только объект `Cluster`.

```bash
helm upgrade shop chart/shop -n shop
kubectl get pods -n shop -w
kubectl get cluster -n shop
```
📸 `shop-db  1/1  Cluster in healthy state  PRIMARY shop-db-1` — все поды прошли политики с первого раза.

**Что произошло (по событиям):**
1. Оператор создал **Job `shop-db-1-initdb`** — одноразовый `initdb` на новом PVC (каталог данных, база `app`,
   пользователь `app`). Job, а не постоянный под: инициализация нужна раз за жизнь тома; при перезапуске данные уже
   на PVC. После завершения CNPG Job удаляет.
2. Затем под `shop-db-1` — сам postgres.
3. Init-контейнер `bootstrap-controller` пода postgres запускается из **образа оператора**
   (`registry.shop.local/cloudnative-pg:1.30.1`) — он копирует в под управляющий бинарник CNPG (instance manager),
   который работает PID 1 рядом с postgres. Поэтому зеркалировать образ оператора было нужно и для подов базы.

**Из одного объекта `Cluster` оператор создал:**
| Вид | Объекты | Назначение |
|---|---|---|
| PVC | `shop-db-1` (1Gi, StorageClass `standard`) | данные postgres |
| Service | `shop-db-rw` / `shop-db-ro` / `shop-db-r` | primary (чтение+запись) / только реплики / любой инстанс |
| Secret | `shop-db-app` (user, password, `uri`, …), `shop-db-ca`, `shop-db-server`, `shop-db-replication` | доступ приложения и TLS |
| прочее | PodDisruptionBudget `shop-db-primary`, ServiceAccount, Role | защита primary от eviction, права instance manager |

Точка подключения для `api`/`worker` — `shop-db-rw`; готовая строка подключения — ключ `uri` в Secret `shop-db-app`.

Метки пода postgres: наши `app.kubernetes.io/name=postgres`, `managed-by=Helm` + добавленные оператором
`app.kubernetes.io/component=database`, `instance=shop-db`, `version=18`, `cnpg.io/cluster`, `cnpg.io/instanceRole=primary`, `role=primary`.

### Подключение api/worker к базе

`DATABASE_URL` берётся из Secret оператора, а не пишется текстом в values:
```yaml
env:
  - name: DATABASE_URL
    valueFrom:
      secretKeyRef:
        name: {{ .Release.Name }}-db-app   # совпадает с именем Cluster при любом имени релиза
        key: uri
  {{- with .Values.api.env }}            # пользовательские переменные (HEALTH_FAIL…) — только если список непустой
  {{- toYaml . | nindent 12 }}
  {{- end }}
```
- Сначала была попытка написать `{{ .Release.Name }}` прямо в `values.yaml` → `yaml: did not find expected key`:
  **values.yaml — не шаблон**, Helm рендерит `{{ }}` только в `templates/`; для YAML `{` — начало словаря.
- Варианты: захардкодить `shop-db-app` (ломается при другом имени релиза: `CreateContainerConfigError: secret not found`
  или, хуже, подключение к чужой базе), `tpl` по values (values становятся «полушаблоном»), **вынести связь в шаблон** ✅ —
  подключение к собственной базе чарта — внутренняя связь чарта, а не настройка.

Проверка:
```bash
minikube ssh -- 'curl -s -X POST -d "{\"item\":\"book\",\"qty\":2}" http://<ClusterIP>:8080/order; sleep 6; curl -s http://<ClusterIP>:8080/orders'
```
📸 `[{"id":1,"item":"book","qty":2,"status":"processed",...}]` — api записал заказ, worker обработал (< 1 с).

**Наблюдение — гонка миграций:** в логах `db: schema not ready, retrying: ERROR: duplicate key value violates unique
constraint "pg_type_typname_nsp_index"`. 7 подов одновременно выполнили `CREATE TABLE IF NOT EXISTS` — в Postgres это
не атомарно при параллельном запуске. Спас ретрай; в реальных проектах миграции запускают **один раз** отдельной Job
(например, Helm hook `pre-upgrade`), а не из каждой реплики.

### Reconciliation у оператора: удаление пода postgres

```bash
kubectl get pods -n shop -l cnpg.io/cluster=shop-db -w
kubectl delete pod shop-db-1 -n shop
```
Результат: под вернулся **с тем же именем** `shop-db-1` (новый IP `10.244.0.32 → 10.244.0.40`), `Cluster in healthy state`,
заказ `id:1` на месте — данные не потеряны. 📸

**Почему восстановление заняло ~3 минуты** (в это время `get cluster` показывал `Waiting for the instances to become active`,
а `api` отвечал `db error`):
- CNPG ставит поду `terminationGracePeriodSeconds: 1800` (`spec.stopDelay`) и `smartShutdownTimeout: 180`;
- при удалении instance manager делает **smart shutdown** Postgres: новые подключения не принимаются, существующие сессии
  дорабатывают; наши 7 подов держат соединения в пуле → ждали полные 180 с;
- затем **fast shutdown** (сессии обрываются), под удаляется, оператор создаёт новый.

**Вывод:** при `instances: 1` отказоустойчивости нет — оператор базу восстановит, но всё это время запись недоступна.
С `instances: 2+` CNPG при потере primary сделал бы **failover** на реплику за секунды, а Service `shop-db-rw`
переключился бы на новый primary (оператор знает роли `primary`/`replica`, метка `cnpg.io/instanceRole`).

**Почему то же имя и почему данные целы.** CNPG использует детерминированные имена инстансов. Имя пода `shop-db-1` =
имя PVC `shop-db-1` = идентичность инстанса: по нему оператор знает, какой том подключить. Данные лежат на PVC, который
переживает удаление пода и подключается к новому. (Ради того же StatefulSet даёт подам порядковые имена; CNPG делает это
сам, без StatefulSet.)

**Кто вернул под.** Контроллер оператора CNPG. Он следит (watch) за объектами `Cluster` **и за подами, которые создал**
(у них `ownerReference` → `Cluster`); удаление пода приходит ему событием, reconcile видит «инстанса 1 нет» и создаёт под.
ReplicaSet/StatefulSet не нужны — логика восстановления в самом операторе.

📸 `kubectl get cluster shop-db -n shop -o yaml`: `spec` (то, что написано в чарте: `instances`, `imageName`, `storage`,
`resources`, `inheritedMetadata` + значения по умолчанию, проставленные оператором) и `status` (пишет оператор:
`phase`, `currentPrimary`, `readyInstances`, `instancesStatus`, `latestGeneratedNode`…).

### Оператор vs `kube-controller-manager`

| | `kube-controller-manager` | Оператор (CNPG) |
|---|---|---|
| Принцип | reconcile loop: watch → сравнить желаемое (`spec`) с фактическим → устранить расхождение → записать `status` | тот же |
| Как общается с кластером | обычный клиент apiserver | обычный клиент apiserver — те же API, RBAC и admission (Kyverno проверял поды, созданные оператором, так же, как наши) |
| Какие объекты | встроенные: Deployment, ReplicaSet, Job, Node, EndpointSlice… | свои CRD: `Cluster`, `Backup`, `Pooler`… |
| Что «знает» | общие вещи: «нужно N одинаковых подов» | **эксплуатацию postgres**: роли primary/replica, failover, smart/fast shutdown, initdb один раз, TLS, пароли |
| Где запущен | компонент control plane (static pod на мастере) | обычный Deployment в namespace `cnpg-system` |
| Кто пишет | проект Kubernetes, поставляется с кластером | авторы конкретного проекта, ставится отдельно (Helm) |

Суть: оператор — это тот же паттерн контроллера, расширенный через CRD и наполненный знаниями о предметной области,
то есть автоматизированный DBA.

## Часть 4 — Падение control plane

### Как устроен control plane в minikube

etcd, kube-apiserver, kube-scheduler, kube-controller-manager — **static pods**: их запускает не apiserver, а **kubelet**
по манифестам в каталоге ноды:
```
/etc/kubernetes/manifests/ — etcd.yaml, kube-apiserver.yaml, kube-controller-manager.yaml, kube-scheduler.yaml
```
kubelet следит за каталогом: убрал файл — под остановлен, вернул — поднят. `kubectl delete pod etcd-minikube` не сработает:
в API виден лишь «зеркальный» под (mirror pod), kubelet пересоздаст его из файла.

```
CONTROL PLANE (управление)               DATA PLANE (работа)
etcd ← apiserver ← controller-manager    kubelet → container runtime → контейнеры
                 ← scheduler             kube-proxy → правила iptables
                 ← kubectl, helm         CoreDNS
                 ← операторы, Kyverno
```

### Прогноз до эксперимента

1. **etcd остановлен, apiserver жив:** apiserver принимает соединение, но запросы висят до таймаута и возвращают ошибку
   (`etcdserver: request timed out` / 500) — ни чтение, ни запись. **apiserver остановлен:** мгновенный `connection refused`.
   Вторичный эффект: liveness-проба apiserver (`/livez`) включает проверку etcd → kubelet со временем начнёт перезапускать apiserver.
2. **`/orders` будет отвечать:** ClusterIP → IP пода переводят уже записанные правила iptables (kube-proxy), контейнеры
   запущены kubelet и container runtime, данные — в postgres на PVC. Никому в пути запроса apiserver не нужен.
3. **Упал процесс в контейнере** → kubelet перезапустит его **локально** по `restartPolicy`, без apiserver.
   **Удалён весь под** → не вернётся: пересоздание делает контроллер (ReplicaSet/оператор) через apiserver. Более того,
   при лежащем control plane под нельзя даже удалить — `kubectl delete` тоже запрос в apiserver.

### Эксперимент

Нагрузка (терминал 1, с ноды через Service — `minikube ssh` идёт через docker, apiserver ему не нужен):
```bash
minikube ssh -- 'while true; do h=$(curl ... /health); o=$(curl ... /orders); echo "$(date +%T) health=$h orders=$o"; sleep 1; done'
```
Остановка etcd (терминал 2, внутри ноды):
```bash
sudo mv /etc/kubernetes/manifests/etcd.yaml /tmp/
docker ps | grep k8s_etcd          # через несколько секунд — пусто: kubelet остановил static pod
```

**Управление кластером — не работает** 📸:
```bash
kubectl get pods -n shop --request-timeout=15s     # ошибка
kubectl apply -f manifests/good-pod.yaml --request-timeout=15s   # ошибка
helm upgrade shop chart/shop -n shop               # ошибка
```

**Приложение — работает:**
- терминал 1: `health=200 orders=200` всё время простоя 📸;
- новый заказ при мёртвом etcd (с ноды): `POST /order` → `{"id":2,"item":"no-control-plane",...,"status":"new"}`, через 6 с —
  `"status":"processed"`: api пишет в postgres, worker обрабатывает — data plane полностью жив 📸;
- `docker kill` контейнера api на ноде → kubelet **локально** поднял новый контейнер (суффикс имени `_1 → _2`); после
  восстановления в `kubectl get pods` у `shop-api-…-dcsnf` — `RESTARTS 2 (3m55s ago)`: kubelet копил статус и отправил
  его в apiserver, когда тот вернулся 📸.

**apiserver упал вслед за etcd:** на ноде пропал контейнер `kube-apiserver` — его liveness (`/livez`) включает проверку
etcd, kubelet его убивал, а новый не мог подняться без etcd. После восстановления — `kube-apiserver-minikube RESTARTS 9`.

### Восстановление

```bash
sudo mv /tmp/etcd.yaml /etc/kubernetes/manifests/     # на ноде
kubectl get nodes                                      # повторять, пока не ответит
```

**Неожиданный эффект — первый `helm upgrade` после восстановления упал:**
```
failed calling webhook "mcluster.cnpg.io": ... dial tcp 10.110.237.7:443: connect: connection refused
```
- Оператор CNPG и Kyverno — **клиенты apiserver**. Без него они не могут работать и завершаются
  (`unable to start manager: ... Get "https://10.96.0.1:443/api": connection refused`), kubelet перезапускает их с растущей
  паузой (`CrashLoopBackOff`): `cnpg … RESTARTS 7`, `kyverno-admission-controller … RESTARTS 6`.
  Наши `api`/`worker` apiserver не используют — их это не затронуло.
- apiserver вернулся раньше, чем оператор вышел из паузы. При записи `Cluster` apiserver обязан вызвать webhook CNPG
  (`failurePolicy: Fail`) → сервис webhook ещё не отвечал → отказ. Тот же сценарий, что разбирали для Kyverno в Части 1.
- **«`kubectl` отвечает» ≠ «кластер полностью управляем».** Восстановление идёт по цепочке
  **etcd → apiserver → контроллеры/операторы/webhook-и**, каждое звено ждёт предыдущее.
- Через несколько минут `helm upgrade` прошёл — кластер снова управляем 📸.

### Итог: что выжило при падении control plane

| Компонент | При мёртвом etcd/apiserver |
|---|---|
| Поды api/worker/postgres, трафик через Service, запись в БД | ✅ работают (kubelet, container runtime, iptables kube-proxy, PVC) |
| Перезапуск упавшего контейнера | ✅ kubelet локально, по `restartPolicy` |
| `kubectl` / `helm` / любые изменения | ❌ |
| Пересоздание удалённого пода, масштабирование, rollout | ❌ контроллеры не могут ни читать, ни писать |
| Обновление endpoints (под стал NotReady → убрать из Service) | ❌ контроллер EndpointSlice и kube-proxy не получают изменений |
| Операторы, Kyverno, другие клиенты API | ❌ падают в CrashLoopBackOff, возвращаются **после** apiserver |

Кластер без control plane — **«замороженный», но не мёртвый**: всё, что уже запущено, продолжает работать, но ничто не
может измениться — ни по воле человека, ни в ответ на сбой.

**Почему одни поды упали, а другие нет.** Решает не «инфраструктура vs приложение», а **нужен ли процессу Kubernetes API**:
- оператор CNPG и Kyverno — клиенты API (watch, запись `status`, lease для leader election). Их авторы сознательно выбрали
  стратегию «потерял apiserver — завершись»: работать вслепую с устаревшей картиной кластера опаснее, чем перезапуститься
  и заново синхронизироваться;
- `api`/`worker` в Kubernetes API не ходят — им нужны только сеть (iptables уже настроены) и postgres.
Если бы у приложения был эндпоинт, читающий Kubernetes API, исход зависел бы от обработки ошибок: перехвачена — деградирует
один эндпоинт; не перехвачена — процесс падает, kubelet его перезапускает.

**Риски долгого простоя control plane:**
- под стал NotReady → kubelet не может даже **сообщить** об этом (обновление статуса — тоже запрос в apiserver) →
  EndpointSlice и iptables не меняются → трафик продолжает идти на подвисший под;
- нода под давлением памяти вытесняет поды → вытесненные/удалённые поды некому пересоздать (нет контроллеров и
  планировщика) → приложение может стать недоступным, хотя данные на PVC целы.
## Часть 5 — Мониторинг

_отложено до Лабы 2_

Стек Лабы 2 взят из `devops-nicotint/lecture-2-observability` (склонирован рядом): `kube-prometheus-stack`, релиз
`monitoring` в namespace `monitoring`; `ServiceMonitor`/`PrometheusRule` подхватываются по метке `release: monitoring`;
дашборды Grafana — ConfigMap с меткой `grafana_dashboard: "1"`.
### Стек мониторинга против наших политик

Проверка до установки (`helm template monitoring prometheus-community/kube-prometheus-stack | kubectl apply --dry-run=server`),
чарт 92.10.1:
- **9 образов из 5 реестров** (`docker.io`, `quay.io`, `registry.k8s.io`, `ghcr.io`) — grafana, k8s-sidecar, prometheus-operator,
  kube-webhook-certgen, node-exporter, kube-state-metrics, bats, alertmanager, prometheus;
- **заблокировано 6 нагрузок** (node-exporter, grafana, kube-state-metrics, prometheus-operator, 2 Job): почти везде
  `trusted-registry` + `require-resources`, у grafana ещё `require-labels`; `disallow-privileged` и `require-run-as-non-root` проходят;
- поды **Prometheus и Alertmanager** создаёт prometheus-operator из своих CRD (как CNPG — поды postgres) — dry-run их не видит,
  но заблокированы они будут так же.

| Подход | Плюсы | Минусы |
|---|---|---|
| A. перезалить 9 образов + прописать resources/метки каждому компоненту | политики не ослабляются | много рутины, повторять при каждом обновлении стека |
| **B. `PolicyException` Kyverno** ✅ | точечно и явно: видно, кому какие правила отключены; privileged/root по-прежнему запрещены | доверяем сторонним реестрам в одном namespace |
| C. `monitoring` в `exclude` всех политик | быстро | отключает все 5 правил, включая privileged; исключение размазано по 5 файлам |

**Выбор B:** позволяет ослабить только три необходимые политики (`trusted-registry`, `require-resources`, `require-labels`),
сохранив запреты на privileged-контейнеры и запуск от root.

**Метка — не граница безопасности.** Ограничить исключение конкретными компонентами по меткам полезно, но метки ставит тот,
кто создаёт под: имея право создавать поды в `monitoring`, можно повесить нужную метку на что угодно. Реальная граница —
**RBAC на namespace `monitoring`**; селектор по меткам защищает от случайностей, а не от злоумышленника.

### Включение PolicyException в Kyverno

В чарте Kyverno 3.9 исключения **по умолчанию выключены** (`--enablePolicyException=false`) — объект `PolicyException` был бы
проигнорирован.
```bash
helm upgrade kyverno kyverno/kyverno -n kyverno --reuse-values \
  --set features.policyExceptions.enabled=true \
  --set features.policyExceptions.namespace=kyverno
# → "--enablePolicyException=true" "--exceptionNamespace=kyverno"
```
**Зачем ограничивать namespace исключений:** право создавать `PolicyException` = право отключать политики. При `"*"`
разработчик с правами на свой `team-a` мог бы создать исключение — причём `match` может указывать **любые** namespace,
т.е. отключить политики даже для чужих подов (например, в `shop`). Поэтому исключения разрешены только в `kyverno`, а доступ
туда — у администраторов через RBAC.

### Исключение `policies/exceptions/monitoring.yaml`

```yaml
apiVersion: kyverno.io/v2
kind: PolicyException
metadata: {name: monitoring-stack, namespace: kyverno}
spec:
  exceptions:
    - {policyName: trusted-registry,  ruleNames: [check-image-registry, autogen-check-image-registry]}
    - {policyName: require-resources, ruleNames: [check-resources, autogen-check-resources]}
    - {policyName: require-labels,    ruleNames: [check-labels, autogen-check-labels]}
  match:
    any:
      - resources:
          namespaces: [monitoring]
          kinds: [Pod, Deployment, StatefulSet, DaemonSet, Job, ReplicaSet]
```
- В `ruleNames` нужны **и autogen-имена**: Deployment/StatefulSet проверяет не `check-resources`, а `autogen-check-resources`.
- Имена правил берутся **из кластера** (`kubectl get clusterpolicy <имя> -o jsonpath='{.status.autogen.rules[*].name}'`):
  в первой версии два имени были угаданы неверно (`validate-trusted-registry`, `check-required-labels`). Kyverno **не сообщает**
  о несуществующем имени правила — исключение молча не сработало бы.
- **Весь namespace, без селектора по меткам:** у компонентов стека нет гарантированно общей метки — их создают разные
  контроллеры (Helm, prometheus-operator), метки могут измениться при обновлении. Компенсация — строгий RBAC (создавать
  нагрузки в `monitoring` могут только администраторы) и сохранённые `disallow-privileged` и `require-run-as-non-root`.
- Поды Prometheus/Alertmanager: prometheus-operator создаёт **StatefulSet** → StatefulSet-контроллер создаёт **Pod**;
  Kyverno проверяет оба (StatefulSet — autogen-правилом, Pod — базовым), поэтому в `kinds` нужны оба.

```bash
kubectl apply -f policies/exceptions/monitoring.yaml
helm template monitoring prometheus-community/kube-prometheus-stack -n monitoring \
  | kubectl apply --dry-run=server -n monitoring -f - 2>&1 | grep -A4 blocked     # пусто — блокировок нет
helm upgrade --install monitoring prometheus-community/kube-prometheus-stack -n monitoring --create-namespace --wait
```
📸 Стек `kube-prometheus-stack-92.10.1` установлен, все поды `monitoring` в `Running` (prometheus, alertmanager, grafana,
operator, kube-state-metrics, node-exporter), ни одного `FailedCreate`.

### ServiceMonitor (`chart/shop/templates/servicemonitor.yaml`)

Prometheus в кластере настроен так: `serviceMonitorSelector: {matchLabels: {release: monitoring}}`,
`serviceMonitorNamespaceSelector: {}` → ServiceMonitor можно класть в namespace приложения, но с меткой `release: monitoring`
(без неё Prometheus его молча не увидит).

```
ServiceMonitor ──selector──► Service (по меткам Service, не подов)
                    prometheus-operator берёт EndpointSlice этого Service
                                ▼
          Prometheus скрейпит КАЖДЫЙ под отдельно: pod-ip:8080/metrics
```
- Один ServiceMonitor на оба сервиса: `matchExpressions` по `app.kubernetes.io/name: shop` и
  `app.kubernetes.io/component in (api, worker)`; `endpoints: port: http` (по имени), `path: /metrics`, `interval: 15s`.
- Ошибка по ходу: ключ `component` вместо `app.kubernetes.io/component` — селектор не совпал бы ни с одним Service,
  0 целей **без ошибок и предупреждений**. Проверка заранее: `kubectl get svc -n shop -l '<тот же селектор>'`.
- **Почему каждый под, а не ClusterIP:** counter растёт внутри конкретного экземпляра. Через ClusterIP Prometheus попадал бы
  на разные поды и видел бы значения разных счётчиков как один ряд; уменьшение `rate()` трактует как **сброс** счётчика и
  добавляет новое значение целиком → ложные всплески. Поэтому скрейпим поды по отдельности и агрегируем `sum(rate(...))`.

Результат: http://localhost:9090/targets → `serviceMonitor/shop/shop-shop/0` — **7/7 UP** (5 api + 2 worker) 📸.

**Известный шум minikube:** цели `kube-controller-manager`, `kube-etcd`, `kube-scheduler` — `down`: эти компоненты отдают
метрики только на `127.0.0.1` ноды, Prometheus из пода до них не достаёт. Из-за этого в Alertmanager постоянно горят
стандартные алерты стека (`TargetDown` и т.п.) — это не наши алерты и не поломка.

### Проектирование 3 алертов

Принцип: алерт — когда пострадал или вот-вот пострадает **пользователь** и нужен человек сейчас; иначе это шум.
Алерты покрывают **разные** части системы.

| # | Алерт | Условие | Почему важно |
|---|---|---|---|
| 1 | Высокая доля 5xx в API | доля 5xx среди **пользовательских** запросов > 5% в течение 2 мин | пользователи не могут создавать/смотреть заказы |
| 2 | Заказы не обрабатываются | worker давно не делал успешной итерации, при этом есть ожидающие заказы | заказ принят (`api` ответил 200), но не переходит в `processed` — бизнес-сбой, невидимый для HTTP-мониторинга |
| 3 | Нет ни одного живого worker | доступных реплик `shop-worker` < 1 | обработка заказов полностью остановлена |

**Слепые зоны, найденные при проектировании:**
- `/health` дёргает kubelet пробами (5 подов × каждые 5–10 с) — это служебный трафик; он разбавляет долю ошибок
  пользовательских запросов → исключаем `handler!="health"`. Значение метки — `health` **без слеша** (так в коде:
  `instrument("health", …)`); проверка: `count by (handler) (http_requests_total{namespace="shop"})`.
- **Метрики о сбое отдаёт сам сбоящий компонент:** `shop_worker_pending_orders` и `shop_worker_last_success_timestamp_seconds`
  экспортирует worker. Если worker упал целиком, ряды исчезают — алерт 2 молчит. Поэтому нужен алерт 3 на метрике
  **внешнего** источника (kube-state-metrics).
- **`up == 0` ≠ «цели нет»:** `up == 0` — цель есть, но не отвечает. При `scale --replicas=0` подов нет → нет endpoints →
  нет целей → ряда `up` нет вовсе, `up == 0` не сработает. Ловится через `absent()` или метрики kube-state-metrics.
- **`spec_replicas > 0 and available == 0` не ловит `scale --replicas=0`:** после масштабирования `spec.replicas` тоже 0.
  Это условие ловит только «хотим N, живых 0» (CrashLoopBackOff). Чтобы ловить оба случая, сравниваем с бизнес-минимумом:
  `kube_deployment_status_replicas_available{deployment="shop-worker"} < 1`.

### PrometheusRule (`chart/shop/templates/prometheusrule.yaml`)

Метка `release: monitoring` (тот же `ruleSelector`, что и для ServiceMonitor).

```yaml
- alert: ShopAPIHighErrorRate          # severity: critical, for: 2m
  expr: |
    sum(rate(http_requests_total{namespace="shop",code=~"5..",handler!="health"}[2m]))
    /
    sum(rate(http_requests_total{namespace="shop",handler!="health"}[2m]))
    > 0.05

- alert: ShopWorkerStalled             # severity: warning, for: 2m
  expr: |
    max by (namespace) (shop_worker_pending_orders{namespace="shop"}) > 0
    and
    ( time() - max by (namespace) (shop_worker_last_success_timestamp_seconds{namespace="shop"}) > 300 )

- alert: ShopWorkerUnavailable         # severity: critical, for: 1m
  expr: |
    kube_deployment_status_replicas_available{namespace="shop",deployment="shop-worker"} < 1
```

**Обоснование:**
1. **ShopAPIHighErrorRate** (errors, золотой сигнал) — доля 5xx среди пользовательских запросов > 5%: пользователи не могут
   создавать/смотреть заказы. Пробы `/health` исключены. Без пользовательского трафика `0/0 = NaN` → алерт молчит (правильно).
2. **ShopWorkerStalled** (бизнес-алерт) — есть ожидающие заказы, но ни один worker > 5 мин не делал успешной итерации. Ловит
   «api отвечает 200, а заказы не обрабатываются» (например, worker жив, но не может работать с БД). `max` по timestamp —
   «даже самый свежий worker давно молчит» = встали все.
3. **ShopWorkerUnavailable** (доступность) — нет ни одной доступной реплики worker. Метрика от **kube-state-metrics**, а не
   от worker → срабатывает и при CrashLoopBackOff, и при `scale --replicas=0`; закрывает слепую зону алерта 2.

**`for`** — условие должно держаться заданное время, прежде чем алерт перейдёт `pending → firing`: отсекает кратковременные
всплески (одна неудачная проба, рестарт пода), а не задаёт интервал проверки.

**Две молчаливые ошибки, найденные проверкой на вкладке Query:**
- алерт 2 в первой версии: `shop_worker_pending_orders > 0 and (… max by (namespace) …)` — `and` сопоставляет ряды по
  **всем** меткам; слева есть `pod`, `instance`…, справа только `namespace` → пар нет, результат всегда пуст. Проверка с
  заведомо истинными порогами (`>= 0`) дала **0 рядов**; после `max by (namespace)` на обеих сторонах — 1 ряд;
- алерт 3 в первой версии: `spec_replicas > 0 and available == 0` — не срабатывает при `scale --replicas=0` (spec тоже 0).

**Урок:** выражение алерта проверяется на вкладке Query с временно ослабленным порогом. Пустой результат при заведомо
истинном условии — ошибка в выражении (метки, имена), а не «всё хорошо». Prometheus такие правила принимает без жалоб.

### Срабатывание алерта: `ShopWorkerUnavailable`

```bash
helm upgrade shop chart/shop -n shop                        # правила загружены: /alerts → группа shop, 3 × Inactive 📸
kubectl scale deploy shop-worker -n shop --replicas=0      # провокация
```
- Prometheus (http://localhost:9090/alerts, фильтр `Shop`; группа `shop-shop-shop-<uid>.yaml > shop` среди ~20 групп стека):
  `Inactive` → **`Pending`** (условие истинно, идёт отсчёт `for: 1m`) 📸 → **`Firing`** 📸.
- Alertmanager (`kubectl port-forward -n monitoring svc/monitoring-kube-prometheus-alertmanager 9093:9093`, фильтр
  `namespace="shop"`): `ShopWorkerUnavailable`, `severity="critical"`, `deployment="shop-worker"` 📸.
- Метки алерта: `job="kube-state-metrics"`, `pod="monitoring-kube-state-metrics-…"` — **живое подтверждение** решения
  по слепой зоне: алерт о worker пришёл не от worker (его подов нет), а от kube-state-metrics, наблюдающего Deployment снаружи.
  Замечание: метки `pod`/`instance` указывают на kube-state-metrics, а не на проблемный сервис — в проде их убирают
  (`max by (namespace, deployment) (…)`), чтобы не путать дежурного.
- Рядом в Alertmanager горят 7 алертов `kube-system` (шум minikube: `TargetDown`, `etcdMembersDown`,
  `KubeSchedulerInstanceUnreachable`…) и **`Watchdog`** — он горит **всегда** нарочно: проверка всей цепочки
  Prometheus → Alertmanager → получатель («алерт мёртвого человека»); если Watchdog перестал приходить — сломался сам мониторинг.
- `ShopWorkerStalled` при нуле worker-ов **не срабатывает**: метрики `pending`/`last_success` отдаёт сам worker, без подов
  ряды исчезают — ровно та слепая зона, ради которой есть `ShopWorkerUnavailable`.

**Восстановление** — `helm upgrade shop chart/shop -n shop`, а не `kubectl scale --replicas=2`: число то же, но
источник правды — чарт; ручной `scale` создал бы дрейф (владельцем поля `replicas` в `managedFields` стал бы `kubectl`).
Алерт гаснет не мгновенно (скрейп kube-state-metrics + вычисление правила, до ~1 мин; при погашении `for` не применяется):
Prometheus → `Inactive`, Alertmanager получает «resolved», алерт пропадает из списка (UI Alertmanager нужно обновить вручную) 📸.

### Дашборд `shop`

Grafana sidecar подгружает дашборды из ConfigMap с меткой `grafana_dashboard: "1"` во **всех** namespace (`NAMESPACE=ALL`) →
дашборд кладётся в чарт: `files/shop-dashboard.json` + ConfigMap-шаблон с `.Files.Get` (как в Лабе 2).

Панели (golden signals + бизнес + инфраструктура):
| # | Панель | Какой вопрос закрывает |
|---|---|---|
| 1 | HTTP 5xx и доля ошибок API | почему сработал `ShopAPIHighErrorRate`, когда начались ошибки |
| 2 | Latency API, p95 | медленные ли ответы даже при успешных запросах |
| 3 | Traffic API: RPS по handler и коду | выросла ли нагрузка, связаны ли ошибки с конкретным эндпоинтом |
| 4 | Очередь заказов и обработка worker | отстают ли worker-ы от поступления заказов или встали совсем |
| 5 | Реплики api/worker: желаемые vs доступные | хватает ли работающих реплик (связь с `ShopWorkerUnavailable`) |
| 6 | CPU и память контейнеров (память — вместе с лимитом) | связана ли деградация с нехваткой ресурсов; «95 из 128 МиБ» = скоро OOMKill |

Порядок разбора алерта: панель 1 → сопоставить с 2 и 3 → проверить 5 и 6 — от симптома к причине.

PromQL панелей писался вручную и проверялся на Prometheus; JSON дашборда (`files/shop-dashboard.json`) сгенерирован по этим
запросам. CPU вынесен в отдельную панель 7 — у памяти и CPU разные единицы. Три находки при проверке запросов:
- **Доля 5xx давала `0 series`, а не `0`:** пока ошибок не было, ряда с `code=~"5.."` не существует → числитель пуст → вся
  дробь пуста → в Grafana «No data» вместо 0%. Исправление: `(sum(rate(…5xx…)) or vector(0)) / sum(rate(…))`.
  Снова «отсутствие ряда ≠ ноль».
- **`sum(shop_worker_pending_orders)` удваивал:** каждый worker отдаёт **общее** число ожидающих заказов из БД
  (`SELECT count(*)`), а не свою долю → два worker-а = 2× реальное. Нужен `max`.
- **CPU/память по `container=~"api|worker"` — пусто:** в minikube (runtime docker через cri-dockerd) cAdvisor отдаёт метрики
  только на уровне **пода**, без меток `container`/`image`. Фильтр по поду: `pod=~"shop-(api|worker)-.*"` (по одному
  контейнеру в поде). На containerd метка `container` есть — исходный запрос там работал бы.

ConfigMap-шаблон `templates/grafana-dashboard.yaml`:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-dashboard
  labels:
    grafana_dashboard: "1"
data:
  shop-dashboard.json: |
    {{- .Files.Get "files/shop-dashboard.json" | nindent 4 }}
```
**Почему ConfigMap в `shop`, а не в `monitoring`:** дашборд относится к приложению и живёт в его чарте — при
`helm uninstall shop` удаляется вместе с ним, не остаётся дашбордов от удалённых приложений. Права разделены: команда
приложения управляет `shop`, команда мониторинга — `monitoring` и Grafana. Если дашборд должен переживать удаление
приложения — хранить отдельно. Оборотная сторона `NAMESPACE=ALL` у sidecar: любая команда с правами на свой namespace
может создать/перезаписать дашборд в общей Grafana; в больших кластерах поиск ограничивают списком namespace.

```bash
helm upgrade shop chart/shop -n shop
kubectl port-forward -n monitoring svc/monitoring-grafana 3000:80          # admin / пароль из secret monitoring-grafana
minikube ssh -- 'while true; do curl -X POST …/order; curl …/orders; sleep 0.5; done'   # нагрузка ~3 RPS
```
Sidecar: `Writing /tmp/dashboards/shop-dashboard.json` → Dashboards → «shop — обзор» 📸.

Попутно: Kyverno предупреждает, что admission-controller в 1 реплике — не HA. После Части 4 цена понятна: при
`failurePolicy: Fail` пока единственная реплика лежит, в кластере нельзя создать ни одного пода.
