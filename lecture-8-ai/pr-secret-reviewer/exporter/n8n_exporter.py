"""Промetheus-экспортер метрик n8n для workflow PR Secret Reviewer.

Опрашивает n8n REST API (executions) и проверяет доступность webhook.
Отдаёт метрики на :9200/metrics для скрейпа Prometheus из Minikube.
"""

import os
import time

import requests
from prometheus_client import CONTENT_TYPE_LATEST, Gauge, start_http_server
from prometheus_client.core import CollectorRegistry, Counter

N8N_BASE_URL = os.environ.get("N8N_BASE_URL", "http://n8n:5678")
N8N_API_TOKEN = os.environ.get("N8N_API_TOKEN", "")
WEBHOOK_PATH = os.environ.get("WEBHOOK_PATH", "/webhook/github-secret-review")
SCRAPE_INTERVAL = int(os.environ.get("SCRAPE_INTERVAL", "30"))
# Фильтр по workflow: если задан, считаем только его executions
# (иначе на панелях будут линии всех workflows n8n, включая тестовые)
WORKFLOW_ID = os.environ.get("WORKFLOW_ID", "")

# Метрики
registry = CollectorRegistry()

# Дедупликация: считаем каждый execution ровно один раз, по его ID.
# Иначе polling каждые SCRAPE_INTERVAL секунд инкрементирует одни и те же
# executions, и increase() в PromQL раздувает счётчик в десятки раз.
seen_execution_ids = set()

# Базлайн на старте: существующие executions помечаем_seen_ без инкремента,
# иначе каждый рестарт экспортера засчитывает всю недавнюю историю
# как новые события (разовый spike на графике).
first_cycle = True

# Активные прогоны не базлайним: пусть посчитаются, когда завершатся.
ACTIVE_STATUSES = {"new", "running", "waiting"}

workflow_runs_total = Counter(
    "n8n_workflow_runs_total",
    "Total workflow executions by status",
    labelnames=["workflow_id", "status"],
    registry=registry,
)

webhook_up = Gauge(
    "n8n_webhook_up",
    "Webhook endpoint availability (1 = up, 0 = down)",
    registry=registry,
)

review_duration_seconds = Gauge(
    "n8n_review_duration_seconds",
    "Duration of the last completed secret-review execution, in seconds",
    labelnames=["workflow_id"],
    registry=registry,
)

api_errors_total = Counter(
    "n8n_exporter_api_errors_total",
    "Errors while querying the n8n API",
    registry=registry,
)


def fetch_executions():
    """Берёт последние executions из n8n API."""
    headers = {"X-N8N-API-KEY": N8N_API_TOKEN}
    try:
        response = requests.get(
            f"{N8N_BASE_URL}/api/v1/executions?limit=50",
            headers=headers,
            timeout=10,
        )
        response.raise_for_status()
        return response.json().get("data", [])
    except Exception as error:  # noqa: BLE001 - экспортер не должен падать
        api_errors_total.inc()
        print(f"n8n API error: {error}")
        return []


def check_webhook():
    """HEAD/GET к webhook-эндпоинту: 200/404/405 считаем 'endpoint жив'."""
    try:
        response = requests.get(
            f"{N8N_BASE_URL}{WEBHOOK_PATH}", timeout=10, allow_redirects=False
        )
        # 405 = метод не тот, но эндпоинт существует и отвечает
        webhook_up.set(1 if response.status_code < 500 else 0)
    except Exception as error:  # noqa: BLE001
        api_errors_total.inc()
        print(f"webhook check error: {error}")
        webhook_up.set(0)


def collect():
    """Один цикл сбора: executions + webhook."""
    global first_cycle  # иначе присваивание ниже делает имя локальным
    check_webhook()

    executions = fetch_executions()
    for execution in executions:
        # Только наш workflow, если фильтр задан
        workflow_id = str(execution.get("workflowId", "unknown"))
        if WORKFLOW_ID and workflow_id != WORKFLOW_ID:
            continue

        execution_id = str(execution.get("id", ""))
        if not execution_id or execution_id in seen_execution_ids:
            continue  # уже посчитан — не инкрементируем повторно

        status = execution.get("status", "unknown")

        # Первый цикл после старта: завершённые прогоны — это история,
        # не события. Помечаем seen и не считаем. Активные — посчитаются
        # на следующих циклах, когда завершатся.
        if first_cycle and status not in ACTIVE_STATUSES:
            seen_execution_ids.add(execution_id)
            continue

        seen_execution_ids.add(execution_id)

        status = execution.get("status", "unknown")
        workflow_runs_total.labels(
            workflow_id=workflow_id, status=status
        ).inc()

        # Длительность последнего успешного прогона
        if status == "success":
            started = execution.get("startedAt")
            stopped = execution.get("stoppedAt")
            if started and stopped:
                from datetime import datetime

                fmt = "%Y-%m-%dT%H:%M:%S.%fZ"
                try:
                    duration = (
                        datetime.strptime(stopped, fmt)
                        - datetime.strptime(started, fmt)
                    ).total_seconds()
                    review_duration_seconds.labels(
                        workflow_id=workflow_id
                    ).set(duration)
                except (ValueError, TypeError):
                    pass

    first_cycle = False


if __name__ == "__main__":
    start_http_server(9200, registry=registry)
    print(f"n8n exporter listening on :9200, scraping {N8N_BASE_URL}")
    while True:
        collect()
        time.sleep(SCRAPE_INTERVAL)