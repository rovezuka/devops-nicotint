# Лабораторная работа 2

Цель работы - развернуть в Kubernetes полный стек наблюдаемости для HTTP-сервиса: метрики, логи, трейсы и алерты. В качестве локального кластера мы использовали Minikube, а компоненты разворачивали через Helm.

## Часть 0. Тестовый сервис

### Подготовка окружения

Для работы мы установили и проверили следующие инструменты:

```bash
kubectl
helm
minikube
docker
```

Проверка установки:

![Проверка установленных инструментов](<screenshots/Screenshot From 2026-10-02 18-30-33.png>)

Запуск Minikube:

![Запуск Minikube](<screenshots/Screenshot From 2026-10-02 18-38-42.png>)
  
На этом месте мы столкнулись с первой проблемой. Для загрузки образов нужен был VPN, но VPN-клиент поднимал HTTP-прокси на `127.0.0.1:10809` и экспортировал `HTTP_PROXY`/`HTTPS_PROXY` в окружение. Для контейнера адрес `127.0.0.1` означает сам контейнер, а не хост, поэтому Minikube не мог через этот прокси выйти в интернет. При запуске кластера появилась такая ошибка:

```
❗️ Local proxy ignored: not passing HTTP_PROXY=http://127.0.0.1:10809 to docker env.
❗️ Failing to connect to https://registry.k8s.io/ from inside the minikube container
```

Мы включили в VPN-клиенте приём подключений из LAN и пересоздали кластер, указав LAN-адрес шлюза:

```
HTTP_PROXY=http://192.168.49.1:10809
HTTPS_PROXY=http://192.168.49.1:10809
NO_PROXY=localhost,127.0.0.1,192.168.49.0/24,10.96.0.0/12,10.244.0.0/16,...
```

Затем появилась другая проблема: `kubectl` пытался ходить к API-серверу кластера через VPN-прокси:

```
kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80
Unable to connect to the server: net/http: TLS handshake timeout
```

`kubectl` написан на Go и учитывает `HTTPS_PROXY`, поэтому адрес API-сервера `192.168.49.2:8443` тоже отправлялся в прокси. Мы добавили в `NO_PROXY` подсети ноды, сервисов и подов, а также домены `.svc` и `.cluster.local`, и прописали их в `~/.bashrc`. После этого внутренний трафик пошёл напрямую, а через VPN остались только внешние запросы.

После этого проверил, что всё работает:

![Проверка состояния кластера](<screenshots/Screenshot From 2026-10-02 18-42-42.png>)

### Сервис api

Для лабораторной мы подготовили небольшой сервис на FastAPI.

Сервис предоставляет следующие эндпоинты:

| Эндпоинт | Назначение |
| --- | --- |
| `/health` | возвращает `ok` и используется в проверках Kubernetes |
| `/fail` | возвращает HTTP 500 и помечает текущий спан ошибочным |
| `/slow` | выполняет операцию с задержкой от 1 до 3 секунд |
| `/load` | выполняет 10 внутренних запросов к `/health` |
| `/metrics` | отдаёт метрики в формате Prometheus |

В приложении мы добавили:

- счётчик общего числа HTTP-запросов;
- счётчик ответов `5xx`;
- гистограмма времени ответа;
- JSON-логирование в стандартный вывод;
- автоматическая OpenTelemetry-инструментация FastAPI;
- вложенный спан `slow-op` в эндпоинт `/slow`;
- статус ошибки для спана эндпоинта `/fail`.

### Сборка образа

Сначала мы собрали образ локально:

```bash
docker build -t observability-api:local ./lecture-2-observability/api
```

Далее мы его отдельно загрузили во внутреннее хранилище Minikube:

```bash
minikube image load observability-api:local
```

### Helm-чарт приложения

Для сервиса мы создали Helm-чарт:

```text
lecture-2-observability/deploy/api/
```

Чарт создаёт:

- `Deployment` с подом приложения;
- `Service` типа `ClusterIP`;
- пробы готовности и живости на `/health`;
- `ServiceMonitor` для Prometheus;
- `PrometheusRule` с правилами алертов.

Перед установкой мы проверили результат рендеринга:

```bash
helm template api ./lecture-2-observability/deploy/api
```

Сервис мы установили в пространство имён `observability`:

```bash
helm upgrade --install api \
  ./lecture-2-observability/deploy/api \
  --namespace observability \
  --create-namespace \
  --wait
```

Проверка пода и Service:

```bash
kubectl get pods -n observability -o wide
kubectl get svc -n observability
helm list -n observability
```

![Проверка пода и Service приложения](<screenshots/Screenshot From 2026-10-02 20-06-40.png>)

Для локальной проверки мы пробросили порт к Service:

```bash
kubectl -n observability port-forward svc/api 8000:8000
```

```bash
curl http://localhost:8000/health
curl -i http://localhost:8000/fail
curl http://localhost:8000/slow
curl http://localhost:8000/load
```

![Проверка эндпоинтов приложения](<screenshots/Screenshot From 2026-10-02 20-11-43.png>)

## Часть 1. Метрики

### Установка Prometheus и Grafana

Сначала мы добавили репозиторий Prometheus Community:

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
```

Prometheus, Grafana, Alertmanager и Prometheus Operator мы установили готовым чартом `kube-prometheus-stack`:

```bash
helm upgrade --install monitoring \
  prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --create-namespace \
  --wait
```

Проверка компонентов:

```bash
kubectl get pods -n monitoring
kubectl get svc -n monitoring
helm list -n monitoring
```

![Компоненты стека мониторинга](<screenshots/Screenshot From 2026-10-02 19-19-57.png>)

Grafana мы открыли через проброс порта:

```bash
kubectl -n monitoring port-forward svc/monitoring-grafana 3000:80
```

Grafana работает, источник данных Prometheus подключён:

![Источник данных Prometheus в Grafana](<screenshots/Screenshot From 2026-10-02 19-36-22.png>)

### ServiceMonitor

Prometheus использует модель, при которой сам периодически обращается к эндпоинту `/metrics` и забирает текущие значения метрик.

Чтобы подключить приложение, мы добавили ресурс `ServiceMonitor`. Он находит Service `api` по меткам и скрейпит именованный порт `http` по пути `/metrics` каждые 15 секунд.

Prometheus Operator выбирает этот ServiceMonitor по label:

```yaml
release: monitoring
```

Проверка ресурса:

```bash
kubectl get servicemonitor -n observability
kubectl get servicemonitor api -n observability -o yaml
```

![Созданный ServiceMonitor](<screenshots/Screenshot From 2026-10-02 20-22-38.png>)

Для проверки цели мы открыли Prometheus:

```bash
kubectl -n monitoring port-forward \
  svc/monitoring-kube-prometheus-prometheus 9090:9090
```

На странице `http://localhost:9090/targets` цель приложения находилась в состоянии `UP`.

Дополнительно проверялся запрос:

```promql
up{namespace="observability",service="api"}
```

Результат равен `1`, то есть последний скрейп прошёл успешно.

![Цель api в состоянии UP](<screenshots/Screenshot From 2026-10-02 20-25-35.png>)

### RED-метрики

В сервисе определены следующие метрики:

- `http_requests_total` - счётчик HTTP-запросов;
- `http_errors_total` - счётчик ответов с кодом `5xx`;
- `http_response_duration_seconds` - гистограмма длительности ответов.

Для дашборда мы использовали следующие запросы PromQL.

Интенсивность запросов:

```promql
sum(rate(http_requests_total{path!="/metrics"}[5m]))
```

Доля ошибок:

```promql
100 *
sum(rate(http_errors_total{path!="/metrics"}[5m]))
/
sum(rate(http_requests_total{path!="/metrics"}[5m]))
```

p95 времени ответа:

```promql
histogram_quantile(
  0.95,
  sum by (le) (
    rate(http_response_duration_seconds_bucket{path!="/metrics"}[5m])
  )
)
```

В запросах исключался `/metrics`, чтобы Prometheus не учитывался как пользовательская нагрузка. `/health` оставался в расчётах, поскольку эндпоинт `/load` создаёт через него 10 внутренних запросов.

На дашборде мы добавили три панели типа «временной ряд»:

- `Request rate`;
- `Error rate`;
- `Response latency p95`.

Для проверки использовались вызовы:

```bash
curl http://localhost:8000/load
curl http://localhost:8000/fail
curl http://localhost:8000/slow
```

После `/load` росла интенсивность запросов, после `/fail` увеличивалась доля ошибок, после `/slow` повышался p95.

![Дашборд с графиком интенсивности запросов](<screenshots/Screenshot From 2026-10-02 21-06-39.png>)

![Дашборд с ошибками и p95](<screenshots/Screenshot From 2026-10-02 21-09-24.png>)

## Часть 2. Логи

### Loki

Loki используется как хранилище логов. Он не забирает логи подов самостоятельно, поэтому отдельно устанавливался агент сбора.

Для Minikube мы запустили Loki в режиме `SingleBinary` с файловым хранилищем и PVC:

```bash
helm repo add grafana https://grafana.github.io/helm-charts
helm repo update
```

```bash
helm upgrade --install loki \
  grafana/loki \
  --namespace monitoring \
  --values ./lecture-2-observability/deploy/loki-values.yaml \
  --wait \
  --timeout 5m
```

В values-файле мы отключили компоненты чтения, записи и бэкенда режима `SimpleScalable` и включили один под `singleBinary`. Для записи данных Kubernetes создал PVC размером 1 GiB.

Проверка:

```bash
kubectl get pods,pvc -n monitoring -l app.kubernetes.io/instance=loki
helm list -n monitoring
```

![Под Loki и PVC](<screenshots/Screenshot From 2026-10-03 12-45-23.png>)

### Grafana Alloy

В качестве агента сбора логов мы выбрали Grafana Alloy. Он устанавливается как `DaemonSet`, то есть создаёт по одному поду на каждой ноде Kubernetes.

Alloy находит поды через Kubernetes API, читает их стандартный вывод и отправляет логи в Loki:

```text
discovery.kubernetes
    -> loki.source.kubernetes
    -> loki.write
    -> Loki
```

Установка:

```bash
helm upgrade --install alloy \
  grafana/alloy \
  --namespace monitoring \
  --values ./lecture-2-observability/deploy/alloy-values.yaml \
  --wait \
  --timeout 5m
```

Проверка:

```bash
kubectl get pods -n monitoring -l app.kubernetes.io/name=alloy -o wide
```

На одной ноде Minikube появился один под Alloy с двумя контейнерами: основным `alloy` и `config-reloader`.

![Под Alloy с двумя контейнерами](<screenshots/Screenshot From 2026-10-03 12-53-47.png>)

### Grafana и LogQL

В Grafana мы добавили источник данных Loki:

```text
http://loki.monitoring.svc.cluster.local:3100
```

Для отображения только JSON-логов приложения мы использовали запрос:

```logql
{namespace="observability", app="api"}
| json
| __error__=""
```

`| json` разбирает JSON-поля, а `__error__=""` исключает обычные логи доступа Uvicorn, которые JSON не являются.

Для фильтрации ошибок:

```logql
{namespace="observability", app="api"}
| json
| __error__=""
| level="error"
```

После вызова `/fail` мы нашли в Grafana запись с сообщением `intentional failure requested` и `trace_id`.

![JSON-лог ошибки в Grafana](<screenshots/Screenshot From 2026-10-03 13-08-34.png>)

В RED-дашборд мы также добавили панель логов, чтобы метрики и логи были доступны в одном окне Grafana.

![Дашборд с метриками и логами](<screenshots/Screenshot From 2026-10-03 13-20-53.png>)

## Часть 3. Трейсы

### Jaeger

Для трейсов мы использовали Jaeger v2 в режиме All-in-One. Для учебной среды выбрали хранение в памяти, поэтому данные Jaeger пропадают при пересоздании пода.

Мы добавили Helm-репозиторий:

```bash
helm repo add jaegertracing https://jaegertracing.github.io/helm-charts
helm repo update
```

Установка:

```bash
helm upgrade --install jaeger \
  jaegertracing/jaeger \
  --namespace monitoring \
  --wait
```

Service Jaeger предоставляет:

- `4317` - OTLP gRPC;
- `4318` - OTLP HTTP;
- `16686` - интерфейс Jaeger.

Проверка:

```bash
kubectl get pods -n monitoring -l app.kubernetes.io/instance=jaeger
kubectl get svc jaeger -n monitoring
```

### Настройка экспортёра OpenTelemetry

В Helm-чарт приложения мы добавили переменные окружения:

```yaml
env:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://jaeger.monitoring.svc.cluster.local:4318/v1/traces
  OTEL_SERVICE_NAME: api
```

После обновления релиза приложения:

```bash
helm upgrade api \
  ./lecture-2-observability/deploy/api \
  --namespace observability \
  --wait
```

трейсы начали отправляться в Jaeger по OTLP HTTP.

Интерфейс Jaeger мы открыли через проброс порта:

```bash
kubectl -n monitoring port-forward svc/jaeger 16686:16686
```

Адрес интерфейса:

```text
http://localhost:16686
```

### Проверка трейсов

Для генерации трейсов использовались:

```bash
curl http://localhost:8000/slow
curl -i http://localhost:8000/fail
```

В Jaeger мы выбрали сервис `api`.

Для `/slow` в waterfall видны:

- корневой HTTP-спан;
- вложенный спан `slow-op`;
- задержка вложенной операции.

![Трейс slow с вложенным спаном slow-op](<screenshots/Screenshot From 2026-10-03 13-55-02.png>)

Для `/fail` спан запроса отмечен как ошибочный.

![Ошибочный трейс fail](<screenshots/Screenshot From 2026-10-03 13-55-35.png>)

Связь между логом и трейсом мы проверили по одинаковому `trace_id`: идентификатор взяли из JSON-лога в Grafana и нашли по нему тот же трейс в Jaeger.

![Trace ID в логе Grafana](<screenshots/Screenshot From 2026-10-03 13-57-53.png>)

![Поиск трейса по Trace ID в Jaeger](<screenshots/Screenshot From 2026-10-03 13-58-29.png>)

## Часть 4. Алерты

### PrometheusRule

В Helm-чарте `api` мы создали ресурс `PrometheusRule`. Prometheus Operator выбирает его по метке:

```yaml
release: monitoring
```

Были настроены три критичных алерта.

### ApiHighErrorRate

Условие: доля ответов `5xx` выше 5% в течение одной минуты.

Этот алерт показывает прямую проблему пользователей: значительная часть запросов завершается ошибкой. В такой ситуации нужно проверить последние изменения, логи приложения и зависимые компоненты.

### ApiHighLatencyP95

Условие: p95 времени ответа выше одной секунды в течение одной минуты.

Этот алерт показывает деградацию производительности. Сначала нужно определить медленный эндпоинт по метрикам, а затем открыть соответствующий трейс в Jaeger.

### ApiDown

Условие: Prometheus не видит цель `api` или значение `up` равно нулю.

Этот алерт означает, что сервис недоступен для пользователей либо нарушена цепочка под -> Service -> ServiceMonitor -> Prometheus. Нужно проверить под, Service, пробы и события Kubernetes.

Для `ApiDown` мы использовали комбинацию `up == 0` и `absent(up{...})`, чтобы ловить и неудачный скрейп существующей цели, и полное исчезновение цели.

### Проверка алертов

Алерт высокой доли ошибок провоцировался серией запросов:

```bash
for i in $(seq 1 30); do
  curl -s -o /dev/null http://localhost:8000/fail
done
```

Алерт высокой задержки провоцировался запросами `/slow`:

```bash
for i in $(seq 1 20); do
  curl -s -o /dev/null http://localhost:8000/slow &
done
wait
```

Недоступность API проверялась временным масштабированием Deployment до нуля:

```bash
kubectl scale deployment api -n observability --replicas=0
```

После проверки приложение восстанавливалось:

```bash
kubectl scale deployment api -n observability --replicas=1
```

Во всех случаях учитывался параметр `for: 1m`: условие должно оставаться истинным одну минуту, прежде чем алерт перейдёт в состояние «сработал» (`Firing`).

![Алерт высокой доли ошибок](<screenshots/Screenshot From 2026-10-03 14-20-41.png>)

![Алерт высокой задержки](<screenshots/Screenshot From 2026-10-03 14-15-36.png>)

![Алерт недоступности сервиса](<screenshots/Screenshot From 2026-10-03 14-25-44.png>)

![Алерты в Prometheus](<screenshots/Screenshot From 2026-10-03 14-21-07.png>)

### Alertmanager и вебхук

Alertmanager уже входил в `kube-prometheus-stack`. Для доставки уведомлений мы настроили получателя-вебхука. Сначала конфигурацию проверили отдельным `AlertmanagerConfig`, затем перенесли в собственный Helm-чарт `deploy/monitoring`.

URL вебхука передаётся при установке релиза:

```bash
export ALERTMANAGER_WEBHOOK_URL='https://webhook.site/UUID'

helm upgrade --install monitoring-config \
  ./lecture-2-observability/deploy/monitoring \
  --namespace observability \
  --set-string alertmanager.webhookUrl="$ALERTMANAGER_WEBHOOK_URL" \
  --wait
```

После провокации алерта сообщение пришло на вебхук:

![Полученное событие на webhook.site](<screenshots/Screenshot From 2026-10-03 16-05-25.png>)

### Karma

Karma мы установили через Helm и подключили к Service Alertmanager:

```bash
helm repo add wiremind https://wiremind.github.io/wiremind-helm-charts
helm repo update
```

```bash
helm upgrade --install karma \
  wiremind/karma \
  --namespace monitoring \
  --values ./lecture-2-observability/deploy/karma-values.yaml \
  --wait
```

Для доступа к интерфейсу мы использовали проброс порта:

```bash
kubectl -n monitoring port-forward svc/karma 8080:80
```

Адрес интерфейса:

```text
http://localhost:8080
```

В Karma виден спровоцированный алерт:

![Алерт в Karma](<screenshots/Screenshot From 2026-10-03 16-21-49.png>)

После разрешения всех алертов на вебхук поступило ещё одно уведомление:

![Уведомление о разрешении алертов](<screenshots/Screenshot From 2026-10-03 16-22-36.png>)
