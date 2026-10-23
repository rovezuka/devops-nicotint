# Лабораторная работа 3 — Платформа для shop

Цель работы — собрать на локальном кластере платформу, которая будет защищать себя сама:
1. ограждения политикой — кластер не принимает деплои, нарушающие правила;
2. Helm-чарт `shop` (`api`, `worker`, `postgres`), который этим правилам соответствует, а `postgres` обслуживает оператор;
3. проверка на живом кластере, что переживает падение control plane;
4. мониторинг поверх стека из Лабы 2: `ServiceMonitor`, 3 алерта, дашборд.

![cats](mems/cat-dance-cat.gif)

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

Часть 0 мне полностью помог сделать Claude, потому что он сказал, что это не главная цель работы и короче твоя часть будет дальше...

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

Любой `kubectl apply` / `helm install` - это запрос к kube-apiserver. Перед записью в etcd он проходит цепочку:

```
запрос → аутентификация → авторизация (RBAC)
       → mutating admission → валидация по схеме → validating admission
       → запись в etcd
```

Дальше нужно было выбрать движок политик: либо Kyverno, либо OPA / Gatekeeper. В табличке ниже приведено вполне наглядное сравнение этих двух движков

### Выбор движка: Kyverno

| | Kyverno | OPA/Gatekeeper |
|---|---|---|
| Язык правил | YAML (обычные k8s-манифесты) | Rego |
| Возможности | validate, mutate, generate, проверка подписей образов | в основном validate |
| Сильная сторона | быстрый старт, привычный формат | сложная логика, единый язык политик и вне k8s |

**Обоснование:** я, как мне кажется, конечно же решил пойти по пути меньшего сопротивления)) в Kyverno используется обычный YAML как язык правил (Rego я вообще впервые слышу), у него не такая сложная логика, как у gatekeeper, привычный формат и быстрый старт. В общем-то выбор очевиден...

```bash
helm install kyverno kyverno/kyverno -n kyverno --create-namespace --wait
```
Дальше нужно описать 5 правил на весь кластер. Вот табличка:

### Правила

| # | Политика | Польза |
|---|---|---|
| 1 | `require-labels` | по меткам работают Service-селекторы, мониторинг, учёт владельцев; без них объект «ничей» |
| 2 | `require-resources` | без лимита памяти один под может съесть ноду и задеть соседей; без requests планировщик не знает, сколько места нужно |
| 3 | `trusted-registry` | в кластер не попадёт непроверенный образ из публичного реестра (supply chain) |
| 4 | `disallow-privileged` | privileged-контейнер получает все capabilities и устройства хоста — фактически root на ноде |
| 5 | `require-run-as-non-root` | при взломе приложения у атакующего нет UID 0 — ограничение последствий, в т.ч. при побеге из контейнера |

![Политика require-labels](<screenshots/Снимок экрана 2026-10-16 в 21.50.30.png>)

### Как это работает: наблюдения

Написал первую политику `require-labels`: у каждого пода должны быть метки `app.kubernetes.io/name` и `app.kubernetes.io/managed-by`. После применения можно заметить, что:

**Webhook регистрирует сам Kyverno:** после установки у `kyverno-resource-validating-webhook-cfg` было 0 webhooks, после
первой политики — 1.

![Первая политика: webhook появился, kubectl run отклонён](<screenshots/Снимок экрана 2026-10-16 в 21.56.05.png>)

**интересный факт).** У меня под `test` был создан **до** применения политики, но он продолжил работать (admission срабатывает только на запись), а фоновое сканирование отметило его в `PolicyReport`.
при пересоздании такой под уже не пройдёт.

![PolicyReport: существующий под отмечен как нарушитель](<screenshots/Снимок экрана 2026-10-16 в 22.01.47.png>)

**Написание остальных политик** - вот такие:
- `require-resources`: обязательны requests CPU и памяти и limits памяти.
- `trusted-registry`: образы только из registry.shop.local
- `disallow-privileged`: запрет privileged: true
- `require-run-as-non-root`: runAsNonRoot: true на уровне пода или каждого контейнера

Прогнали через `dry-run=server`, потом применил все пять, и все стали `Ready` - кайфули

![Проверка политик на сервере](<screenshots/Снимок экрана 2026-10-16 в 22.06.46.png>)
![Все 5 политик Ready](<screenshots/Снимок экрана 2026-10-16 в 22.19.22.png>)
![kubectl run нарушает сразу три политики](<screenshots/Снимок экрана 2026-10-16 в 22.28.19.png>)

### Проверка: 5 манифестов-нарушителей

Сначала был написан правильный `good-pod.yaml`, который проходил все 5 политик; затем 5 копий, в каждой из которых сломана ровно одна вещь — чтобы можно было доказать, что сработало именно нужное правило.

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

1 часть - ВСЕ

---

## Часть 2 — Чарт api и worker

Во второй части нужно было написать `Chart.yaml`, `values.yaml` (`api.replicas: 3`, `worker.replicas: 2`») и шаблоны `Deployment`/`Service` для `api` и `worker`, с readiness- и liveness-пробами на `/health`. 

- `values.yaml`: реестр вынесен в одно поле `registry`, тег меняется одной строкой; общий `securityContext`:
  `runAsNonRoot: true` + сверх политик `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`;
  `env` — список (позволяет `valueFrom.secretKeyRef`).

Перед установкой проверил чарт через Kyverno — `helm template shop chart/shop | kubectl apply --dry-run=server -f -`: все объекты прошли
5 политик с первого раза

![helm install без отклонений политиками](<screenshots/Снимок экрана 2026-10-16 в 23.09.56.png>)

### Reconciliation

**Удаление пода api руками** → вернулся новый под!! (другое имя и IP). Его создал ReplicaSet-контроллер: он держит
watch на поды, получает событие удаления и догоняет число подов до `spec.replicas`

![Удаление пода: Terminating → новый под Running, события SuccessfulCreate](<screenshots/Снимок экрана 2026-10-16 в 23.14.51.png>)

**`replicas` 3 → 5 в `values.yaml` + `helm upgrade`** → Helm записал в Deployment `spec.replicas: 5`, поды создали
контроллеры (Deployment → ReplicaSet → Pod)

![replicas 5/5 и история ревизий](<screenshots/Снимок экрана 2026-10-16 в 23.20.21.png>)

### Rolling update без потерь (1.0.0 → 1.1.0)

Здесь нужно было обновить `api` на новую версию так, что ни один запрос не упал, а все поды менялись по одному. Тут есть две опасности:
1. слишком много подов будут меняться одновременно
2. под выключится в момент, когда ему будут посылать запросы

Поэтому задали в шаблоне:
```yaml
strategy:
  rollingUpdate:
    maxSurge: 1        # новые поды — строго по одному
    maxUnavailable: 0  # готовых подов никогда не меньше replicas
```
Так будет создаваться один под, идет ожидание, что он готов и только после этого старый убирается - и все повторяется

Для второй опасности добавили в шаблон это:
```yaml
lifecycle:
  preStop:
    sleep:
      seconds: 5
```
Перед завершением нужно подождать 5 секунд, как раз за это время под гарантированно будет вычеркнут из Service, новые запросы на него не пойдут и только потом он завершится

preStop выкатывался **отдельным** upgrade до смены тега: хук есть только у подов из нового шаблона, а в rollout
удаляются старые.

![Выкатка preStop + strategy: поды обновляются по одному](<screenshots/Снимок экрана 2026-10-16 в 23.24.54.png>)

Нагрузка — цикл curl на ClusterIP Service изнутри ноды (`port-forward` не подходит: он цепляется к одному поду).
Старый ReplicaSet уменьшался по одному, новый рос по одному:

![Rolling update: ReplicaSet-ы перетекают по одному](<screenshots/Снимок экрана 2026-10-16 в 23.30.10.png>)
![Rolling update: завершение](<screenshots/Снимок экрана 2026-10-16 в 23.30.19.png>)

**Результат: `ok=1117 fail=0`** — ни один запрос не провалился. Это заслуга четырёх механизмов вместе:
`maxUnavailable: 0`, readiness-проба, `preStop.sleep` и graceful shutdown приложения.

![ok=1117 fail=0](<screenshots/Снимок экрана 2026-10-16 в 23.31.35.png>)

### Сломанная версия и откат

Здесь нужно специально выкатить сломанную версию `api` и показать, что Kubernetes не дает ей навредить: обновление застревает, старые поды остаются на месте, а сервис продолжает отвечать.

`HEALTH_FAIL=true` — сломанность видна в runtime:
- создан **ровно один** новый под (`maxSurge: 1`), он не прошёл readiness → **ни один старый не удалён** (`maxUnavailable: 0`);
- liveness перезапускает новый под, растёт `RESTARTS`
- `kubectl rollout status` зависает
- **`helm upgrade` ответил успехом** — Helm 4 без `--wait` не ждёт готовности подов

![Сломанный релиз: rollout завис, новый RS 0/1, старый 5/5](<screenshots/Снимок экрана 2026-10-16 в 23.45.47.png>)
![Новый под не проходит пробы и перезапускается](<screenshots/Снимок экрана 2026-10-16 в 23.46.05.png>)
![История: ревизия 8 failed, ревизия 9 «Upgrade complete»](<screenshots/Снимок экрана 2026-10-16 в 23.45.57.png>)

`helm rollback shop 7`:
- откат — **новая ревизия** (10, «Rollback to 7»): история Helm — журнал операций (так вышло, что именно 7 ревизия у меня рабочая, потому что 8 ревизия упала из-за синтаксической ошибки...)
- откат прошёл мгновенно: шаблон ревизии 7 совпал по `pod-template-hash` с ReplicaSet, который всё время держал 5 готовых подов, — контроллер переиспользовал его и уменьшил сломанный до 0
- `helm rollback` не трогает локальный `values.yaml` → `HEALTH_FAIL` убран из файла, иначе следующий upgrade снова сломал бы релиз.

Сервис отвечал всё время эксперимента: **`ok=3669 fail=0`**.

![Откат: rollout завершён, сломанный RS = 0](<screenshots/Снимок экрана 2026-10-16 в 23.47.00.png>)
![История: Rollback to 7](<screenshots/Снимок экрана 2026-10-16 в 23.48.02.png>)
![Сломанный под удалён после отката](<screenshots/Снимок экрана 2026-10-16 в 23.49.24.png>)
![ok=3669 fail=0 за время сломанного релиза и отката](<screenshots/Снимок экрана 2026-10-16 в 23.49.32.png>)

2 часть была для меня оч долгой и непростой, хорошо, что она закончилась... 

---

## Часть 3 — Postgres через оператора

В 3 части нужно было развернуть оператор БД (CloudNativePG или Zalando postgres-operator) один раз на кластер и добавить в чарт шаблон, который создаёт объект оператора (например, `Cluster` у CloudNativePG).

**Оператор** - это такая программа, которая добавляет в Kubernetes новый вид объекта и сама знает, как его обслуживать

**Мой выбор: CloudNativePG** - он управляет подами напрямую, сам создает Secret с готовой строкой подключения, кроме этого еще хорошо документирован! 

### Оператор восстал против политик

Возникла некая проблема, что проверка до установки (`helm template … | kubectl apply --dry-run=server`) показала, что под оператора блокируют
`require-labels`, `require-resources`, `trusted-registry`)) Решили перезалить образы в свой реестр и привести поды в соответствие: так сохранился единый реестр, политики не ослабляются

- Образы оператора и postgres зеркалированы в `registry.shop.local` (pull + tag).
- `operator/cnpg-values.yaml`: `image.repository`, `resources`, `podLabels` (`managed-by`).
- Оператор ставится **отдельно** от чарта `shop`: он один на кластер, его CRD — кластерные объекты.

![Установка CNPG: оператор Running, CRD](<screenshots/Снимок экрана 2026-10-17 в 00.14.42.png>)

### Объект `Cluster` в чарте

Был написан `templates/postgres-cluster.yaml`, где описали, что нужно: `imageName`, `resources`, `storage.size: 1Gi`, `instances: 1`, `inheritedMetadata.labels` — оператор копирует их на всё, что создаёт

Из одного объекта `Cluster` оператор создал: Job `shop-db-1-initdb` (одноразовая инициализация тома), под `shop-db-1`, 3 адреса (Service) для подключения: на запись, только на чтение и «любой» и Secret с логином, паролем и готовой строкой подключения + сертификаты

![Postgres поднят: initdb Completed, Cluster in healthy state, созданные объекты](<screenshots/Снимок экрана 2026-10-17 в 00.20.53.png>)

![Схема создана, заказ записан api и обработан worker](<screenshots/Снимок экрана 2026-10-17 в 00.31.21.png>)

### Reconciliation у оператора

Удалим под базы руками, чтобы проверить, вернёт ли его оператор.

`kubectl delete pod shop-db-1` → оператор вернул под **с тем же именем** `shop-db-1` (новый IP), данные на месте

Восстановление заняло ~3 минуты: при удалении instance manager делает **smart shutdown** (ждёт закрытия сессий,
`smartShutdownTimeout: 180`), а поды держат соединения в пуле. Всё это время `api` отвечал `db error`
**Вывод:** при `instances: 1` нет отказоустойчивости; с `instances: 2+` CNPG сделал бы failover на реплику за секунды

![Удаление пода postgres и его возврат](<screenshots/Снимок экрана 2026-10-17 в 00.40.14.png>)
![Во время smart shutdown: Waiting for the instances to become active](<screenshots/Снимок экрана 2026-10-17 в 00.41.18.png>)

### spec и status

у любого объекта в Kubernetes две половины:

`spec` — то, что описано в чарте, плюс значения по умолчанию, проставленные оператором (`bootstrap.initdb`,
`smartShutdownTimeout`, `stopDelay`, параметры postgres…). 

`status` пишет оператор: `phase`, `currentPrimary`,
`readyInstances`, `instancesStatus`, `conditions`, сертификаты и т.д.

![Cluster spec (1)](<screenshots/Снимок экрана 2026-10-17 в 00.42.49.png>)
![Cluster spec (2)](<screenshots/Снимок экрана 2026-10-17 в 00.42.56.png>)
![Cluster status (1)](<screenshots/Снимок экрана 2026-10-17 в 00.43.22.png>)
![Cluster status (2)](<screenshots/Снимок экрана 2026-10-17 в 00.43.28.png>)

### Оператор vs `kube-controller-manager`

Итак, чем же отличается оператор от `kube-controller-manager`? `controller-manager` по сути следит, чтобы у Deployment было нужное число подов, и пересоздаёт упавшие. Оператор устроен точно так же: следит, сравнивает, чинит. Разница в том, что они знают: первый знает только общие вещи, а оператор знает как эксплуатировать конкретно postgres. Короче он что то типа администратора БД)

Еще оператор это обычное приложение в кластере, а controller-manager встроен в сам Kubernetes

| | `kube-controller-manager` | Оператор (CNPG) |
|---|---|---|
| Принцип | reconcile loop: watch → сравнить `spec` с фактом → устранить расхождение → записать `status` | тот же |
| Как общается с кластером | обычный клиент apiserver | обычный клиент apiserver — те же API, RBAC и admission (Kyverno проверял поды оператора) |
| Объекты | встроенные: Deployment, ReplicaSet, Job, Node, EndpointSlice… | свои CRD: `Cluster`, `Backup`, `Pooler`… |
| Что «знает» | общее: «нужно N одинаковых подов» | эксплуатацию postgres: primary/replica, failover, smart/fast shutdown, initdb, TLS, пароли |
| Где запущен | компонент control plane (static pod) | обычный Deployment в `cnpg-system` |
| Кто пишет | проект Kubernetes | авторы проекта; ставится отдельно |

3 часть все и осталось еще немного...

---

## Часть 4 — Падение control plane

Тут нужно остановить `etcd` или `apiserver` и и попробовать что-то изменить, проверить что все продолжает работать и вернуть control plane и убедиться, что кластер снова управляем.

В minikube `etcd` и `apiserver` запускаются не как обычные приложения - kubelet поднимает их по файлам-описаниям в папке /etc/kubernetes/manifests/ на ноде: убрал файл, и kubelet остановит компонент, вернул — запустит. Поэтому заходим на ноду (minikube ssh) и перемещаем файл etcd.yaml в другую папку. Через несколько секунд контейнер etcd пропал.

```bash
minikube ssh
sudo mv /etc/kubernetes/manifests/etcd.yaml /tmp/      # остановить etcd
sudo mv /tmp/etcd.yaml /etc/kubernetes/manifests/      # вернуть
```

![Остановка etcd: контейнер исчез](<screenshots/Снимок экрана 2026-10-20 в 12.56.40.png>)

**Управление — не работает:** `kubectl get`, `kubectl apply`, `helm upgrade` завершаются таймаутами.

![kubectl и helm не работают](<screenshots/Снимок экрана 2026-10-20 в 12.58.42.png>)

**А вот приложение — работает!!!** цикл curl через Service всё время отвечал `health=200 orders=200`, то есть все хорошо) новый заказ, созданный при
мёртвом etcd, записан в postgres и обработан worker.

Для обработки запроса никому не нужен apiserver. Сетевые правила уже прописаны на ноде, поды уже запущены, база работает. Все, что было запущено до отключения, работает дальше

![health=200 orders=200 при мёртвом etcd](<screenshots/Снимок экрана 2026-10-20 в 13.02.15.png>)
![Заказ создан и обработан без control plane](<screenshots/Снимок экрана 2026-10-20 в 13.03.08.png>)

**apiserver упал вслед за etcd:** его проверка здоровья включает проверку `etcd`. kubelet увидел, что apiserver не работает, и стал его перезапускать, а запуститься без `etcd` тот не мог.

kubelet сам поднимает упавшие контейнеры. Контейнер `api` был убит прямо на ноде (`docker kill`), и kubelet тут же запустил его заново, без всякого `apiserver`.

![apiserver Exited; возврат etcd](<screenshots/Снимок экрана 2026-10-20 в 13.04.53.png>)
![Восстановление: нода ещё NotReady](<screenshots/Снимок экрана 2026-10-20 в 13.06.50.png>)

Вернул файл etcd.yaml на место. Сначала поднялся etcd, потом apiserver, нода ненадолго была NotReady, а затем снова заработал kubectl.

**Сюрприз))** первый helm upgrade после восстановления упал с ошибкой о webhook оператора CNPG. Оператор CNPG и Kyverno без apiserver работать не могут: им нужно следить за объектами в кластере. Пока apiserver лежал, они падали и перезапускались, и между перезапусками у них растёт пауза. apiserver уже вернулся, а оператор ещё нет, и его проверка не отвечала, поэтому изменение было отклонено. Через несколько минут всё поднялось, и helm upgrade прошёл (честно, все поднялось, я просто забыл заскринить это, уже не хочется возвращаться...)

![После восстановления: RESTARTS 2 у убитого пода, helm upgrade упал на webhook CNPG](<screenshots/Снимок экрана 2026-10-20 в 13.08.55.png>)

### Что выжило при падении control plane

| Компонент | При упавшем etcd/apiserver |
|---|---|
| Поды api, worker, postgres; трафик через Service; запись заказов в БД | ✅ работают |
| Перезапуск упавшего контейнера (kubelet на ноде, по `restartPolicy`) | ✅ работает |
| `kubectl`, `helm`, любые изменения в кластере | ❌ не работают |
| Пересоздание удалённого пода, масштабирование, обновления | ❌ контроллеры без API ничего не могут |
| Оператор CNPG, Kyverno (клиенты API) | ❌ падают и перезапускаются, поднимаются только после apiserver |

Кластер без control plane - это когда запущенное продолжает работать, но ничто не может измениться (ни по воле разраба, ни при сбое)

На этом эта часть - все

---

## Часть 5 — Мониторинг

Тут нужно использовать стек из Лабы 2: `kube-prometheus-stack`, релиз `monitoring` в namespace `monitoring`

### Опять стек против моих политик

Стек тянет 9 образов из 5 реестров без лимитов и меток и по итогу заблокировано 6 нагрузок (плюс поды Prometheus/Alertmanager,
которые создаёт prometheus-operator)

Короче решил ослабить только три правила (реестр, лимиты, метки), а запрет privileged и запуска от root продолжает действовать и для мониторинга

Потом еще наткнулся на граблю: исключения в Kyverno по умолчанию выключены, пришлось включить. Разрешил создавать их только в namespace kyverno

![Включение PolicyException в Kyverno](<screenshots/Снимок экрана 2026-10-20 в 13.16.26.png>)
![PolicyException](<screenshots/Снимок экрана 2026-10-20 в 13.23.31.png>)
![Исключение применено, блокировок нет](<screenshots/Снимок экрана 2026-10-20 в 13.31.41.png>)
![Установка kube-prometheus-stack](<screenshots/Снимок экрана 2026-10-20 в 13.31.51.png>)
![Поды стека мониторинга Running](<screenshots/Снимок экрана 2026-10-20 в 13.31.59.png>)

### ServiceMonitor

`ServiceMonitor` - это такая инструкция для Prometheus: «найди такие-то Service и собирай с их подов метрики по адресу /metrics каждые 15 секунд». На нем должна быть метка `release: monitoring`

![servicemonitor.yaml](<screenshots/Снимок экрана 2026-10-20 в 13.43.39.png>)
![Targets: serviceMonitor/shop/shop-shop/0 — 7/7 UP](<screenshots/Снимок экрана 2026-10-20 в 13.50.58.png>)

### 3 алерта (`templates/prometheusrule.yaml`)

Дальше было написано 3 алерта:

| Алерт | Выражение | Обоснование |
|---|---|---|
| `ShopAPIHighErrorRate` (critical, `for: 2m`) | доля 5xx среди запросов без `handler="health"` > 5% | важно, потому что пользователи просто не смогут оформить заказ или даже просто посмотреть его |
| `ShopWorkerStalled` (warning, `for: 2m`) | `max(pending) > 0` **and** `time() - max(last_success) > 300` | `api` может отвечать 200, но заказы все равно не переходят в `processed`. Опасен, потому что является невидимым снаружи сбоев |
| `ShopWorkerUnavailable` (critical, `for: 1m`) | `kube_deployment_status_replicas_available{deployment="shop-worker"} < 1` | обработка заказов полностью остановлена - нет ни одного живого пода |

### Срабатывание алерта

Сделал `kubectl scale deploy shop-worker --replicas=0`, то есть убрал все поды worker. По итогу:

-через ~30 секунд ShopWorkerUnavailable перешёл в Pending, а ещё через минуту стал Firing
- алерт дошёл до Alertmanager с `severity="critical"`
- В метках алерта видно job="kube-state-metrics": сигнал о worker пришёл не от worker (его подов нет), а от наблюдателя со стороны

![Prometheus: ShopWorkerUnavailable FIRING](<screenshots/Снимок экрана 2026-10-20 в 14.04.13.png>)
![Alertmanager: ShopWorkerUnavailable, severity=critical](<screenshots/Снимок экрана 2026-10-20 в 14.07.10.png>)

Восстановление — `helm upgrade`, в результате чего алерт погас: Prometheus → Inactive

![Prometheus: все алерты shop Inactive](<screenshots/Снимок экрана 2026-10-20 в 14.09.06.png>)
![Alertmanager: алерта shop больше нет](<screenshots/Снимок экрана 2026-10-20 в 14.09.13.png>)

### Дашборд

Дашборд лежит прямо в чарте: ConfigMap с меткой `grafana_dashboard: "1"`, и Grafana подхватывает его сама. Там есть 7 панелей, каждая из которых отвечает на вопрос дежурного:

- **ошибки api** почему сработал алерт
- **задержка (p95)** медленно ли отвечаем
- **трафик** выросла ли нагрузка
- **очередь заказов** успевает ли worker
- **реплики** хватает ли подов
- **память против лимита** близко ли к OOM
- **CPU**

![Дашборд shop — обзор](<screenshots/Снимок экрана 2026-10-20 в 14.29.54.png>)

---

## Итоги

- На кластере стоят ограждения Kyverno (5 правил), заведённые раньше чарта - 5 манифестов-нарушителей отклоняются, каждый своей политикой.
- Helm-чарт `shop` (`api`, `worker`, `postgres` через CNPG) ставится без единого отклонения; rolling update проходит без потерь (`fail=0`), сломанный релиз не роняет сервис и откатывается `helm rollback`.
- Поды `api` и `postgres` при ручном удалении возвращаются сами - ReplicaSet-контроллер и оператор соответственно.
- При падении control plane `shop` продолжает отвечать и обрабатывать заказы - после восстановления кластер снова управляем.
- Мониторинг: ServiceMonitor (7/7 целей), 3 алерта, дашборд из 7 панелей.

Сегодня на этом все, можно и отдохнуть

![tusa](mems/tusw.gif)