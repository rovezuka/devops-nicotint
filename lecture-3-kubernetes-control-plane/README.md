# Лабораторная работа 3 — Платформа для shop

> ⚠️ **Черновик (сырой вариант).** Текст будет дорабатываться и редактироваться.

Цель работы — собрать на локальном кластере платформу, которая защищает себя сама:
1. ограждения политикой — кластер не принимает деплои, нарушающие правила;
2. Helm-чарт `shop` (`api`, `worker`, `postgres`), который этим правилам соответствует, а `postgres` обслуживает оператор;
3. проверка на живом кластере, что переживает падение control plane;
4. мониторинг поверх стека из Лабы 2: `ServiceMonitor`, 3 алерта, дашборд.

## Окружение и структура

- Кластер: **minikube** (Kubernetes v1.35.1), драйвер docker (Docker Desktop, Apple Silicon), 1 нода, 4 CPU / 7 ГБ.
- Helm v4.3.1, Kyverno v1.19.1 (chart 3.9.1), CloudNativePG 1.30.1 (chart 0.29.1), kube-prometheus-stack 92.10.1.

```
lab3/
├── services/            # Часть 0: код api и worker (Go) + Dockerfile
├── policies/            # Часть 1: 5 ClusterPolicy Kyverno
│   └── exceptions/      # Часть 5: PolicyException для стека мониторинга
├── manifests/           # Часть 1: эталонный под + 5 манифестов-нарушителей
├── operator/            # Часть 3: values оператора CNPG
├── chart/shop/          # Части 2, 3, 5: Helm-чарт shop
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── files/shop-dashboard.json
│   └── templates/
│       ├── api-deployment.yaml, api-service.yaml
│       ├── worker-deployment.yaml, worker-service.yaml
│       ├── postgres-cluster.yaml          # CRD-объект Cluster (CNPG)
│       ├── servicemonitor.yaml, prometheusrule.yaml
│       └── grafana-dashboard.yaml
└── screenshots/
```

---

## Часть 0 — Сервисы

| Сервис | Эндпоинты | Особенности |
|---|---|---|
| `api` | `GET /health`, `POST /order`, `GET /orders`, `GET /version`, `GET /metrics` | `HEALTH_FAIL=true` → `/health` отвечает 500; graceful shutdown по SIGTERM |
| `worker` | `GET /health`, `GET /metrics` | раз в 5 с переводит заказы `new → processed`; `FOR UPDATE SKIP LOCKED` — реплики не обрабатывают один заказ дважды |

- Подключение к БД — через `DATABASE_URL`; схема создаётся при старте с ретраями, если postgres ещё не готов.
- Метрики Prometheus: `http_requests_total`, `http_request_duration_seconds` (api); `shop_worker_pending_orders`,
  `shop_worker_orders_processed_total`, `shop_worker_errors_total`, `shop_worker_last_success_timestamp_seconds` (worker).
- Образ: multi-stage, финальный слой `distroless/static:nonroot` (~14 МБ), запуск от UID 65532.
- Образы собираются прямо в docker внутри minikube (`eval $(minikube docker-env)`) с именами
  `registry.shop.local/api:1.0.0`, `registry.shop.local/api:1.1.0`, `registry.shop.local/worker:1.0.0`.
  `registry.shop.local` играет роль внутреннего доверенного реестра — на него завязано правило из Части 1.

---

## Часть 1 — Ограждения на кластер

### Где происходит проверка

Любой `kubectl apply` / `helm install` — это запрос к kube-apiserver. Перед записью в etcd он проходит цепочку:

```
запрос → аутентификация → авторизация (RBAC)
       → mutating admission → валидация по схеме → validating admission
       → запись в etcd
```

Движок политик встраивается в неё как **admission webhook**. Если webhook отказал — объект не попадает в etcd,
ни один контроллер его не увидит, ни один под не создастся. Политика срабатывает **в момент записи объекта**, а не во
время работы пода.

### Выбор движка: Kyverno

| | Kyverno | OPA/Gatekeeper |
|---|---|---|
| Язык правил | YAML (обычные k8s-манифесты) | Rego |
| Возможности | validate, mutate, generate, проверка подписей образов | в основном validate |
| Сильная сторона | быстрый старт, привычный формат | сложная логика, единый язык политик и вне k8s |

**Обоснование:** Kyverno не требует изучать отдельный язык — правила пишутся обычным YAML в привычном формате
Kubernetes-манифестов, что даёт быстрый старт. Сложная логика, ради которой стоит брать Gatekeeper/Rego, для этого
набора правил не нужна.

```bash
helm install kyverno kyverno/kyverno -n kyverno --create-namespace --wait
```

### Правила

| # | Политика | Что требует | Польза |
|---|---|---|---|
| 1 | `require-labels` | метки `app.kubernetes.io/name` и `app.kubernetes.io/managed-by` | по меткам работают Service-селекторы, мониторинг, учёт владельцев; без них объект «ничей» |
| 2 | `require-resources` | `requests.cpu`, `requests.memory`, `limits.memory` | без лимита памяти один под может съесть ноду и задеть соседей; без requests планировщик не знает, сколько места нужно |
| 3 | `trusted-registry` | образы только из `registry.shop.local/` (в т.ч. init- и ephemeral-контейнеры) | в кластер не попадёт непроверенный образ из публичного реестра (supply chain) |
| 4 | `disallow-privileged` | нет `privileged: true` (в т.ч. init- и ephemeral-контейнеры) | privileged-контейнер получает все capabilities и устройства хоста — фактически root на ноде |
| 5 | `require-run-as-non-root` | `runAsNonRoot: true` на уровне пода или каждого контейнера | при взломе приложения у атакующего нет UID 0 — ограничение последствий, в т.ч. при побеге из контейнера |

- **Почему не требуем `limits.cpu`:** жёсткий CPU limit (CFS quota) вызывает throttling даже при свободных ресурсах на
  ноде; `requests.cpu` сохраняет информацию, нужную планировщику. Память — несжимаемый ресурс (её можно только отнять
  OOMKill-ом), поэтому `limits.memory` обязателен.
- **privileged ≠ root:** privileged снимает границу контейнер↔хост (capabilities, `/dev`, seccomp); root — это UID 0 *внутри*
  контейнера. Правила независимы; apiserver допускает `privileged: true` вместе с `runAsNonRoot: true`.
- **Kyverno видит только спецификацию пода, а не образ:** `USER 65532` из Dockerfile ему неизвестен, поэтому
  `runAsNonRoot: true` прописывается явно. Дальше kubelet при старте сверит это с метаданными образа — проверка в два
  этапа: Kyverno при записи, kubelet при запуске.
- Системные namespace (`kube-system`, `kyverno`) исключены в каждой политике (`exclude`), чтобы сбой webhook или
  строгое правило не заблокировали восстановление самого кластера. Обратная сторона — кто может создавать поды в
  `kube-system`, обходит политики; это закрывается RBAC (и, дополнительно, встроенным Pod Security Admission).

![Политика require-labels](<screenshots/Снимок экрана 2026-10-16 в 21.50.30.png>)

### Как это работает: наблюдения

**Autogen.** Правило, написанное для `Pod`, Kyverno автоматически размножает на Deployment, StatefulSet, DaemonSet, Job,
ReplicaSet (путь `spec.template...`) и CronJob. Поэтому нарушающий Deployment отклоняется **сразу на `apply`/`helm install`**,
а не «тихо» на уровне подов. Без autogen `apply` Deployment прошёл бы успешно, но ReplicaSet получал бы отказы при
создании подов (`0/N`, причина — только в событиях `FailedCreate` у ReplicaSet).

**Webhook регистрирует сам Kyverno:** после установки у `kyverno-resource-validating-webhook-cfg` было 0 webhooks, после
первой политики — 1.

![Первая политика: webhook появился, kubectl run отклонён](<screenshots/Снимок экрана 2026-10-16 в 21.56.05.png>)

**Существующие поды не перепроверяются.** Под `test` был создан **до** применения политики — он продолжил работать
(admission срабатывает только на запись), а фоновое сканирование отметило его в `PolicyReport`. Это «отложенная бомба»:
при пересоздании такой под уже не пройдёт. Поэтому в проде новые политики сначала включают в режиме `Audit`.

![PolicyReport: существующий под отмечен как нарушитель](<screenshots/Снимок экрана 2026-10-16 в 22.01.47.png>)

**Правило проверяется в обе стороны** — «плохое не проходит» и «хорошее проходит». При разработке
`require-run-as-non-root`:
- v1 пропускала под с `runAsNonRoot: true` на уровне пода и контейнером, переопределившим `runAsNonRoot: false`;
- v2 (якорь `X(runAsNonRoot)` — «поле должно отсутствовать») отклоняла легальный под;
- v3: первый шаблон `anyPattern` проверяет и контейнеры условным якорем `=(securityContext): =(runAsNonRoot): true`.

![Проверка политик на сервере](<screenshots/Снимок экрана 2026-10-16 в 22.06.46.png>)
![Все 5 политик Ready](<screenshots/Снимок экрана 2026-10-16 в 22.19.22.png>)
![kubectl run нарушает сразу три политики](<screenshots/Снимок экрана 2026-10-16 в 22.28.19.png>)

### Проверка: 5 манифестов-нарушителей

Сначала эталон `good-pod.yaml`, проходящий все 5 политик; затем 5 копий, в каждой сломана **ровно одна** вещь — иначе
по отказу нельзя доказать, что сработало именно нужное правило.

| Манифест | Что сломано | Кто отклонил |
|---|---|---|
| `good-pod.yaml` | ничего | принят, `Running` |
| `bad-no-limits.yaml` | нет `resources` | `require-resources` |
| `bad-registry.yaml` | образ `nginx:1.27` | `trusted-registry` |
| `bad-no-labels.yaml` | нет меток | `require-labels` |
| `bad-privileged.yaml` | `privileged: true` | `disallow-privileged` |
| `bad-root.yaml` | `runAsNonRoot: false` | `require-run-as-non-root` |

![good-pod создан, 5 манифестов отклонены — каждый своей политикой](<screenshots/Снимок экрана 2026-10-16 в 22.37.04.png>)
![Итоговый список политик](<screenshots/Снимок экрана 2026-10-16 в 22.39.52.png>)

---

## Часть 2 — Чарт api и worker

Helm — шаблонизатор и учёт релизов, а **не контроллер**: за кластером он не следит. Reconciliation делают контроллеры
Kubernetes; история релизов хранится в кластере в Secret-ах `sh.helm.release.v1.<релиз>.v<N>`. Для Kyverno запрос от Helm
ничем не отличается от `kubectl apply`, поэтому требования политик заложены прямо в шаблоны.

Решения в чарте:
- `values.yaml`: реестр вынесен в одно поле `registry`, тег меняется одной строкой; общий `securityContext`:
  `runAsNonRoot: true` + сверх политик `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`;
  `env` — список (позволяет `valueFrom.secretKeyRef`).
- Метки пода (`spec.template.metadata.labels`) — именно их проверяет политика; `managed-by: {{ .Release.Service }}`.
- `selector` **неизменяем** после создания → в нём только стабильные метки (`name`, `component`), без версии.
- Пробы readiness и liveness на `/health`: readiness убирает под из endpoints Service (контейнер продолжает работать),
  liveness перезапускает контейнер. `/health` не ходит в БД — иначе падение postgres перезапустило бы все поды api.
- **Известное ограничение:** в селекторе нет `app.kubernetes.io/instance`, поэтому в одном namespace можно ставить только
  один релиз чарта.

Проверка до установки — `helm template shop chart/shop | kubectl apply --dry-run=server -f -`: все объекты прошли
5 политик с первого раза.

![helm install без отклонений политиками](<screenshots/Снимок экрана 2026-10-16 в 23.09.56.png>)

### Reconciliation

**Удаление пода api руками** → вернулся **новый** под (другое имя и IP). Его создал ReplicaSet-контроллер: он держит
watch на поды, получает событие удаления и догоняет число подов до `spec.replicas`. Helm в этом не участвует.

![Удаление пода: Terminating → новый под Running, события SuccessfulCreate](<screenshots/Снимок экрана 2026-10-16 в 23.14.51.png>)

**`replicas` 3 → 5 в `values.yaml` + `helm upgrade`** → Helm записал в Deployment `spec.replicas: 5`, поды создали
контроллеры (Deployment → ReplicaSet → Pod). Наблюдения: несохранённый/неприменённый `values.yaml` на кластер не влияет;
`helm upgrade` без изменений всё равно создаёт новую ревизию; ручной `kubectl scale` Helm сам не откатит.

![replicas 5/5 и история ревизий](<screenshots/Снимок экрана 2026-10-16 в 23.20.21.png>)

### Rolling update без потерь (1.0.0 → 1.1.0)

Умолчания `25%/25%` при 5 репликах дают до 7 подов, из которых готовых может быть 4. Поэтому:
```yaml
strategy:
  rollingUpdate:
    maxSurge: 1        # новые поды — строго по одному
    maxUnavailable: 0  # готовых подов никогда не меньше replicas
```
**Гонка при остановке пода:** SIGTERM контейнеру и удаление пода из endpoints происходят параллельно; если приложение
перестанет принимать соединения раньше, чем kube-proxy обновит правила, запросы получат `connection refused`. Решение —
`preStop`-хук «подожди 5 с». Классический `exec: ["sleep","5"]` в distroless-образе не сработал бы (нет бинарника),
поэтому используется встроенное действие kubelet:
```yaml
lifecycle:
  preStop:
    sleep:
      seconds: 5
```
preStop выкатывался **отдельным** upgrade до смены тега: хук есть только у подов из нового шаблона, а в rollout
удаляются старые. Этот upgrade сам вызвал rollout — `preStop` входит в `spec.template` (изменилась `pod-template-hash`).

![Выкатка preStop + strategy: поды обновляются по одному](<screenshots/Снимок экрана 2026-10-16 в 23.24.54.png>)

Нагрузка — цикл curl на ClusterIP Service изнутри ноды (`port-forward` не подходит: он цепляется к одному поду).
Старый ReplicaSet уменьшался по одному, новый рос по одному:

![Rolling update: ReplicaSet-ы перетекают по одному](<screenshots/Снимок экрана 2026-10-16 в 23.30.10.png>)
![Rolling update: завершение](<screenshots/Снимок экрана 2026-10-16 в 23.30.19.png>)

**Результат: `ok=1117 fail=0`** — ни один запрос не провалился. Это заслуга четырёх механизмов вместе:
`maxUnavailable: 0`, readiness-проба, `preStop.sleep` и graceful shutdown приложения.

![ok=1117 fail=0](<screenshots/Снимок экрана 2026-10-16 в 23.31.35.png>)

### Сломанная версия и откат

Попутная ошибка формата (`env: [HEALTH_FAIL = "true"]` — список строк вместо объектов) была отклонена apiserver ещё при
построении патча — ревизия 8 `failed`, в кластере ничего не изменилось. Это **структурная** ошибка, от неё защищает
apiserver.

Ревизия 9 с `HEALTH_FAIL=true` структурно корректна — сломанность видна только в runtime:
- создан **ровно один** новый под (`maxSurge: 1`), он не прошёл readiness → **ни один старый не удалён** (`maxUnavailable: 0`);
- liveness перезапускает новый под, растёт `RESTARTS`;
- `kubectl rollout status` зависает; Kubernetes **сам не откатывает** (через `progressDeadlineSeconds` лишь ставит условие
  `ProgressDeadlineExceeded`);
- **`helm upgrade` ответил успехом** — Helm 4 без `--wait` не ждёт готовности подов. В CI нужен `--wait` / `--atomic`.

![Сломанный релиз: rollout завис, новый RS 0/1, старый 5/5](<screenshots/Снимок экрана 2026-10-16 в 23.45.47.png>)
![Новый под не проходит пробы и перезапускается](<screenshots/Снимок экрана 2026-10-16 в 23.46.05.png>)
![История: ревизия 8 failed, ревизия 9 «Upgrade complete»](<screenshots/Снимок экрана 2026-10-16 в 23.45.57.png>)

`helm rollback shop 7`:
- откат — **новая ревизия** (10, «Rollback to 7»): история Helm — журнал операций;
- откат прошёл мгновенно: шаблон ревизии 7 совпал по `pod-template-hash` с ReplicaSet, который всё время держал 5 готовых
  подов, — контроллер переиспользовал его и уменьшил сломанный до 0;
- `helm rollback` не трогает локальный `values.yaml` → `HEALTH_FAIL` убран из файла, иначе следующий upgrade снова сломал бы релиз.

Сервис отвечал всё время эксперимента: **`ok=3669 fail=0`**.

![Откат: rollout завершён, сломанный RS = 0](<screenshots/Снимок экрана 2026-10-16 в 23.47.00.png>)
![История: Rollback to 7](<screenshots/Снимок экрана 2026-10-16 в 23.48.02.png>)
![Сломанный под удалён после отката](<screenshots/Снимок экрана 2026-10-16 в 23.49.24.png>)
![ok=3669 fail=0 за время сломанного релиза и отката](<screenshots/Снимок экрана 2026-10-16 в 23.49.32.png>)

---

## Часть 3 — Postgres через оператора

**Оператор = CRD + контроллер** со знаниями о предметной области. Пишешь `kind: Cluster` (`instances`, `storage`), а
контроллер превращает это в поды, PVC, Service-ы, Secret-ы, настраивает репликацию и делает failover.

**Выбор: CloudNativePG** — управляет жизненным циклом PostgreSQL через CRD, поддерживает автоматическое восстановление,
управляет подами напрямую (без StatefulSet) и сам создаёт Secret с готовым `uri` для подключения.

### Оператор против наших политик

Проверка до установки (`helm template … | kubectl apply --dry-run=server`) показала: под оператора блокируют
`require-labels`, `require-resources`, `trusted-registry`. Варианты — исключить namespace, расширить список доверенных
реестров или **перезалить образы в свой реестр и привести поды в соответствие**. Выбран последний: единый доверенный
реестр сохраняется, политики не ослабляются.

- Образы оператора и postgres зеркалированы в `registry.shop.local` (pull + tag).
- `operator/cnpg-values.yaml`: `image.repository`, `resources`, `podLabels` (`managed-by`).
- Оператор ставится **отдельно** от чарта `shop`: он один на кластер, его CRD — кластерные объекты.

![Установка CNPG: оператор Running, CRD](<screenshots/Снимок экрана 2026-10-17 в 00.14.42.png>)

**Autogen не знает о CRD:** `kind: Cluster` для Kyverno — незнакомый объект без `spec.template`. Если забыть в нём
`resources`, `helm install` пройдёт, а поды postgres будут отклонены уже при создании их оператором.

### Объект `Cluster` в чарте

`templates/postgres-cluster.yaml`: `imageName` из нашего реестра, `resources`, `storage.size: 1Gi`, `instances: 1`,
`inheritedMetadata.labels` — оператор копирует их на всё, что создаёт (для `require-labels`).

Из одного объекта `Cluster` оператор создал: Job `shop-db-1-initdb` (одноразовая инициализация тома), под `shop-db-1`,
PVC `shop-db-1`, Service-ы `shop-db-rw` / `-ro` / `-r` (primary / реплики / любой инстанс), Secret-ы `shop-db-app`
(логин, пароль, `uri`), `-ca`, `-server`, `-replication`, а также PodDisruptionBudget, ServiceAccount и Role.
Init-контейнер пода postgres запускается из образа оператора — он копирует внутрь instance manager.

![Postgres поднят: initdb Completed, Cluster in healthy state, созданные объекты](<screenshots/Снимок экрана 2026-10-17 в 00.20.53.png>)

`api`/`worker` подключены через `DATABASE_URL` из Secret оператора (`valueFrom.secretKeyRef`, имя
`{{ .Release.Name }}-db-app` прямо в шаблоне — `values.yaml` не шаблонизируется).

![Схема создана, заказ записан api и обработан worker](<screenshots/Снимок экрана 2026-10-17 в 00.31.21.png>)

### Reconciliation у оператора

`kubectl delete pod shop-db-1` → оператор вернул под **с тем же именем** `shop-db-1` (новый IP), данные на месте.
Имя пода = имя PVC = идентичность инстанса; данные лежат на PVC, который переживает удаление пода. Под вернул контроллер
CNPG: он следит за `Cluster` и за своими подами (`ownerReference`).

Восстановление заняло ~3 минуты: при удалении instance manager делает **smart shutdown** (ждёт закрытия сессий,
`smartShutdownTimeout: 180`), а наши поды держат соединения в пуле. Всё это время `api` отвечал `db error`.
**Вывод:** при `instances: 1` нет отказоустойчивости; с `instances: 2+` CNPG сделал бы failover на реплику за секунды.

![Удаление пода postgres и его возврат](<screenshots/Снимок экрана 2026-10-17 в 00.40.14.png>)
![Во время smart shutdown: Waiting for the instances to become active](<screenshots/Снимок экрана 2026-10-17 в 00.41.18.png>)

### spec и status

`spec` — то, что описано в чарте, плюс значения по умолчанию, проставленные оператором (`bootstrap.initdb`,
`smartShutdownTimeout`, `stopDelay`, параметры postgres…). `status` пишет оператор: `phase`, `currentPrimary`,
`readyInstances`, `instancesStatus`, `conditions`, сертификаты и т.д.

![Cluster spec (1)](<screenshots/Снимок экрана 2026-10-17 в 00.42.49.png>)
![Cluster spec (2)](<screenshots/Снимок экрана 2026-10-17 в 00.42.56.png>)
![Cluster status (1)](<screenshots/Снимок экрана 2026-10-17 в 00.43.22.png>)
![Cluster status (2)](<screenshots/Снимок экрана 2026-10-17 в 00.43.28.png>)

### Оператор vs `kube-controller-manager`

| | `kube-controller-manager` | Оператор (CNPG) |
|---|---|---|
| Принцип | reconcile loop: watch → сравнить `spec` с фактом → устранить расхождение → записать `status` | тот же |
| Как общается с кластером | обычный клиент apiserver | обычный клиент apiserver — те же API, RBAC и admission (Kyverno проверял поды оператора) |
| Объекты | встроенные: Deployment, ReplicaSet, Job, Node, EndpointSlice… | свои CRD: `Cluster`, `Backup`, `Pooler`… |
| Что «знает» | общее: «нужно N одинаковых подов» | эксплуатацию postgres: primary/replica, failover, smart/fast shutdown, initdb, TLS, пароли |
| Где запущен | компонент control plane (static pod) | обычный Deployment в `cnpg-system` |
| Кто пишет | проект Kubernetes | авторы проекта; ставится отдельно |

Оператор — тот же паттерн контроллера, расширенный через CRD и наполненный знаниями о предметной области: автоматизированный DBA.

---

## Часть 4 — Падение control plane

В minikube etcd, kube-apiserver, scheduler и controller-manager — **static pods**: их запускает kubelet по манифестам из
`/etc/kubernetes/manifests/`. Убрал файл — kubelet остановил под; вернул — поднял.

```bash
minikube ssh
sudo mv /etc/kubernetes/manifests/etcd.yaml /tmp/      # остановить etcd
sudo mv /tmp/etcd.yaml /etc/kubernetes/manifests/      # вернуть
```

![Остановка etcd: контейнер исчез](<screenshots/Снимок экрана 2026-10-20 в 12.56.40.png>)

**Управление — не работает:** `kubectl get`, `kubectl apply`, `helm upgrade` завершаются таймаутами.

![kubectl и helm не работают](<screenshots/Снимок экрана 2026-10-20 в 12.58.42.png>)

**Приложение — работает:** цикл curl через Service всё время отвечал `health=200 orders=200`; новый заказ, созданный при
мёртвом etcd, записан в postgres и обработан worker-ом. Путь запроса — правила iptables (kube-proxy), контейнеры
(kubelet + runtime), данные на PVC — apiserver не нужен.

![health=200 orders=200 при мёртвом etcd](<screenshots/Снимок экрана 2026-10-20 в 13.02.15.png>)
![Заказ создан и обработан без control plane](<screenshots/Снимок экрана 2026-10-20 в 13.03.08.png>)

**apiserver упал вслед за etcd:** его liveness (`/livez`) проверяет etcd — kubelet убивал apiserver, а новый не мог
подняться (контейнер `Exited`, после восстановления `RESTARTS 9`). Контейнер api, убитый `docker kill` прямо на ноде,
kubelet перезапустил **локально**, без apiserver; после восстановления это видно как `RESTARTS 2`.

![apiserver Exited; возврат etcd](<screenshots/Снимок экрана 2026-10-20 в 13.04.53.png>)
![Восстановление: нода ещё NotReady](<screenshots/Снимок экрана 2026-10-20 в 13.06.50.png>)

**Неожиданный эффект восстановления:** первый `helm upgrade` упал на webhook CNPG (`connection refused`). Оператор CNPG
и Kyverno — клиенты apiserver: без него они завершаются и уходят в `CrashLoopBackOff`; apiserver вернулся раньше, чем
оператор вышел из паузы, а `failurePolicy: Fail` превратил недоступный webhook в отказ. Через несколько минут `helm upgrade`
прошёл. **«`kubectl` отвечает» ≠ «кластер полностью управляем»:** восстановление идёт по цепочке
etcd → apiserver → контроллеры/операторы/webhook-и.

![После восстановления: RESTARTS 2 у убитого пода, helm upgrade упал на webhook CNPG](<screenshots/Снимок экрана 2026-10-20 в 13.08.55.png>)

### Что выжило

| Компонент | При мёртвом etcd/apiserver |
|---|---|
| Поды api/worker/postgres, трафик через Service, запись в БД | ✅ работают |
| Перезапуск упавшего контейнера | ✅ kubelet локально, по `restartPolicy` |
| `kubectl` / `helm` / любые изменения | ❌ |
| Пересоздание удалённого пода, масштабирование, rollout | ❌ контроллеры не могут ни читать, ни писать |
| Обновление endpoints (под стал NotReady) | ❌ kubelet не может даже сообщить статус |
| Операторы, Kyverno — клиенты API | ❌ CrashLoopBackOff, возвращаются после apiserver |

Кластер без control plane — **«замороженный», но не мёртвый**: запущенное продолжает работать, но ничто не может
измениться — ни по воле человека, ни в ответ на сбой. Почему одни поды упали, а другие нет: решает не «инфраструктура vs
приложение», а **нужен ли процессу Kubernetes API**.

---

## Часть 5 — Мониторинг

Используется стек из Лабы 2: `kube-prometheus-stack`, релиз `monitoring` в namespace `monitoring`.

### Стек против наших политик → PolicyException

Стек тянет 9 образов из 5 реестров без наших лимитов и меток; заблокировано 6 нагрузок (плюс поды Prometheus/Alertmanager,
которые создаёт prometheus-operator). Выбран **`PolicyException` Kyverno**: ослабляются только три необходимые политики
(`trusted-registry`, `require-resources`, `require-labels`), запреты privileged и root сохраняются.

- Исключения в Kyverno 3.9 по умолчанию выключены; включены с ограничением namespace:
  `features.policyExceptions.enabled=true`, `namespace=kyverno`. Право создавать `PolicyException` = право отключать
  политики (причём `match` может указывать любые namespace), поэтому — только в `kyverno` под RBAC администраторов.
- В `ruleNames` указаны и autogen-имена (`autogen-check-…`) — иначе Deployment/StatefulSet всё равно отклонялись бы.
- Исключение на весь namespace `monitoring`: общей метки у компонентов стека нет (их создают разные контроллеры).
  Метка в любом случае не граница безопасности; реальная граница — RBAC на namespace.

![Включение PolicyException в Kyverno](<screenshots/Снимок экрана 2026-10-20 в 13.16.26.png>)
![PolicyException](<screenshots/Снимок экрана 2026-10-20 в 13.23.31.png>)
![Исключение применено, блокировок нет](<screenshots/Снимок экрана 2026-10-20 в 13.31.41.png>)
![Установка kube-prometheus-stack](<screenshots/Снимок экрана 2026-10-20 в 13.31.51.png>)
![Поды стека мониторинга Running](<screenshots/Снимок экрана 2026-10-20 в 13.31.59.png>)

### ServiceMonitor

Prometheus выбирает ServiceMonitor по метке `release: monitoring` из любого namespace. ServiceMonitor выбирает **Service**
по меткам, а Prometheus скрейпит **каждый под отдельно** (через EndpointSlice): counter растёт внутри конкретного
экземпляра, и при скрейпе через ClusterIP `rate()` принимал бы переключение между подами за сброс счётчика.

![servicemonitor.yaml](<screenshots/Снимок экрана 2026-10-20 в 13.43.39.png>)
![Targets: serviceMonitor/shop/shop-shop/0 — 7/7 UP](<screenshots/Снимок экрана 2026-10-20 в 13.50.58.png>)

Цели `kube-controller-manager`, `kube-etcd`, `kube-scheduler` — `down`: в minikube они отдают метрики только на
`127.0.0.1` ноды. Из-за этого в Alertmanager постоянно горят стандартные алерты стека — это известный шум minikube.

### 3 алерта (`templates/prometheusrule.yaml`)

| Алерт | Выражение | Обоснование |
|---|---|---|
| `ShopAPIHighErrorRate` (critical, `for: 2m`) | доля 5xx среди запросов без `handler="health"` > 5% | пользователи не могут создавать и смотреть заказы. Пробы kubelet исключены — служебный трафик разбавлял бы ошибки пользователей |
| `ShopWorkerStalled` (warning, `for: 2m`) | `max(pending) > 0` **and** `time() - max(last_success) > 300` | бизнес-сбой: `api` отвечает 200, но заказы не переходят в `processed` (например, worker не может работать с БД) |
| `ShopWorkerUnavailable` (critical, `for: 1m`) | `kube_deployment_status_replicas_available{deployment="shop-worker"} < 1` | обработка заказов остановлена. Метрика от kube-state-metrics, а не от worker |

Слепые зоны, учтённые при проектировании:
- метрики `pending`/`last_success` отдаёт сам worker — если он упал целиком, ряды исчезают и алерт 2 молчит; поэтому
  алерт 3 построен на **внешнем** источнике;
- `up == 0` не срабатывает при `scale --replicas=0` — целей нет вовсе, а не «цель не отвечает»;
- `spec_replicas > 0 and available == 0` не ловит `scale --replicas=0` (spec тоже 0) — поэтому сравнение с бизнес-минимумом `< 1`;
- `and` сопоставляет ряды по всем меткам — без `max by (namespace)` на обеих сторонах алерт 2 никогда бы не сработал.
  Выражения алертов проверялись на вкладке Query с ослабленным порогом: пустой результат при заведомо истинном условии — ошибка.

`for` — условие должно держаться заданное время до перехода `pending → firing`: отсекает кратковременные всплески.

### Срабатывание алерта

`kubectl scale deploy shop-worker -n shop --replicas=0` → `ShopWorkerUnavailable`: Inactive → Pending → **Firing**,
алерт дошёл до Alertmanager. Его метки `job="kube-state-metrics"` подтверждают: сигнал пришёл не от worker (подов нет),
а от наблюдателя снаружи.

![Prometheus: ShopWorkerUnavailable FIRING](<screenshots/Снимок экрана 2026-10-20 в 14.04.13.png>)
![Alertmanager: ShopWorkerUnavailable, severity=critical](<screenshots/Снимок экрана 2026-10-20 в 14.07.10.png>)

Восстановление — `helm upgrade` (источник правды — чарт, а не ручной `kubectl scale`). Алерт погас: Prometheus → Inactive,
в Alertmanager остался только шум minikube (`kube-system`) и `Watchdog` — он горит всегда нарочно, как проверка всей
цепочки оповещений.

![Prometheus: все алерты shop Inactive](<screenshots/Снимок экрана 2026-10-20 в 14.09.06.png>)
![Alertmanager: алерта shop больше нет](<screenshots/Снимок экрана 2026-10-20 в 14.09.13.png>)

### Дашборд

ConfigMap с меткой `grafana_dashboard: "1"` в чарте (`templates/grafana-dashboard.yaml` + `files/shop-dashboard.json`);
sidecar Grafana ищет такие ConfigMap во всех namespace. ConfigMap лежит в `shop`, рядом с приложением: удаляется вместе
с релизом, а права разделены между командой приложения и командой мониторинга.

| # | Панель | Какой вопрос закрывает |
|---|---|---|
| 1 | Ошибки API (доля 5xx) | почему сработал `ShopAPIHighErrorRate` |
| 2 | Latency p95 | медленные ли ответы при успешных запросах |
| 3 | Traffic (RPS по handler и коду) | выросла ли нагрузка, связаны ли ошибки с эндпоинтом |
| 4 | Заказы: очередь и обработка | отстают ли worker-ы или встали совсем |
| 5 | Реплики: желаемые vs доступные | хватает ли реплик (связь с `ShopWorkerUnavailable`) |
| 6 | Память подов vs лимит | близко ли к OOMKill |
| 7 | CPU подов | связана ли деградация с нагрузкой на CPU |

Находки в PromQL: доля 5xx без ошибок давала «нет данных» вместо 0 (`or vector(0)`); `sum` по ожидающим заказам удваивал
значение (каждый worker отдаёт общее число из БД — нужен `max`); в minikube (docker + cri-dockerd) cAdvisor отдаёт метрики
только на уровне пода, без метки `container`.

На панели 5 видно, как доступные реплики worker упали до 0 и вернулись — момент срабатывания `ShopWorkerUnavailable`.

![Дашборд shop — обзор](<screenshots/Снимок экрана 2026-10-20 в 14.29.54.png>)

---

## Итоги

- На кластере стоят ограждения Kyverno (5 правил), заведённые раньше чарта; 5 манифестов-нарушителей отклоняются,
  каждый своей политикой.
- Helm-чарт `shop` (`api`, `worker`, `postgres` через CNPG) ставится без единого отклонения; rolling update проходит
  без потерь (`fail=0`), сломанный релиз не роняет сервис и откатывается `helm rollback`.
- Поды `api` и `postgres` при ручном удалении возвращаются сами — ReplicaSet-контроллер и оператор соответственно.
- При падении control plane `shop` продолжает отвечать и обрабатывать заказы; после восстановления кластер снова управляем.
- Мониторинг поверх стека из Лабы 2: ServiceMonitor (7/7 целей), 3 алерта с обоснованием (один показан в состоянии
  firing), дашборд из 7 панелей.
