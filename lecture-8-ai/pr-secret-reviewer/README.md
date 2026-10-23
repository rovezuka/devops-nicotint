# PR Secret Reviewer

Автоматическое ревью пулл-реквестов на случайно закоммиченные секреты. ИИ-агент в n8n по вебхуку GitHub забирает дифф PR, прогоняет его через детерминированный сканер, классифицирует находки по политике и белому списку, пишет отчёт на диск и оставляет комментарий с вердиктом прямо в тред PR.

Полностью без кнопок: разработчик открывает PR или пушит коммиты, дальше система всё делает сама.

## Состав

```text
pr-secret-reviewer/
├── docker-compose.yml       # n8n, github-mcp, экспортер метрик
├── exporter/                # экспортер метрик n8n в формате Prometheus
├── policy/
│   ├── security-policy.md   # политика классификации находок
│   ├── allowlist.json       # белый список безопасных значений
│   └── agent-skill.md       # скилл агента (системный промпт)
├── mock/
│   └── pull-request.json    # моковое событие PR для локальных тестов
├── reports/                 # сюда агент пишет отчёты
└── deploy/
    ├── prometheus-scrape.yaml  # additional scrape config для Prometheus
    └── prometheus-rules.yaml   # правила алертов
```

## Как это работает

```text
GitHub PR webhook
        ↓
ngrok (туннель)
        ↓
n8n: Extract PR → IF (ping?) → AI Agent
                                ├── pull_request_read (GitHub MCP)
                                ├── scan_diff (Code Tool, детерминированный сканер)
                                ├── read_file (filesystem MCP: политика, белый список)
                                ├── write_file (filesystem MCP: отчёт)
                                └── pull_request_review_write (GitHub MCP: комментарий)
        ↓
отчёт в reports/ + комментарий с вердиктом в PR
```

Верстка: `block` (вероятный реальный секрет), `warn` (подозрительно, нужен человек), `false_positive` (безопасно по контексту или белому списку).

Два MCP-сервера подключаются по-разному: GitHub MCP живет отдельным контейнером (HTTP-транспорт), filesystem MCP запускается как stdio-процесс внутри контейнера n8n через community-пакет `n8n-nodes-mcp`.

## Запуск

Нужны: Docker, Minikube (для мониторинга), PAT от GitHub с правами на чтение PR и запись комментариев, API-ключ модели, аккаунт ngrok.

1. `.env` рядом с docker-compose.yml:

```
GITHUB_PERSONAL_ACCESS_TOKEN=...
N8N_API_TOKEN=...          # создается в n8n UI: Settings → n8n API
N8N_WORKFLOW_ID=...         # ID воркфлоу, чтобы экспортер считал только его
```

2. Поднять стек:

```bash
docker compose up -d
```

3. В n8n UI (`http://localhost:5678`): создать владельца, установить community-пакет `n8n-nodes-mcp`, импортировать воркфлоу из `workflow.json`, настроить credentials (GitHub MCP: `http://github-mcp:8082/mcp` + Bearer PAT; filesystem MCP: `npx -y @modelcontextprotocol/server-filesystem /files/policy /files/reports`; модель), вставить содержимое `policy/agent-skill.md` в системный промпт агента, активировать воркфлоу.

4. Туннель и вебхук:

```bash
ngrok http 5678
```

В настройках репозитория: Settings → Webhooks → Add webhook, Payload URL `https://<поддомен>.ngrok.app/webhook/github-secret-review`, content type `application/json`, события Pull requests.

5. Мониторинг (переиспользуем стек из лабы 2):

```bash
helm upgrade monitoring prometheus-community/kube-prometheus-stack \
  -n monitoring \
  -f deploy/prometheus-scrape.yaml \
  --reuse-values

kubectl apply -f deploy/prometheus-rules.yaml -n monitoring
```

6. Проверка: открыть PR или запушить коммит в ветку с открытым PR. В Recent Deliveries вебхука - 200, в треде PR - комментарий с вердиктом, в `reports/` - отчёт.

## Метрики и алерты

Экспортер (порт 9200) отдает: `n8n_webhook_up`, `n8n_workflow_runs_total` по статусам, `n8n_review_duration_seconds`, `n8n_exporter_api_errors_total`.

Три алерта и почему именно они:

1. `SecretReviewerWebhookDown` (`n8n_webhook_up == 0`, 2m, critical). Вебхук - триггер всей системы. Если он мертв, ревью не запускается вообще, и это самая дорогая тишина: никто не заметит, пока не всплывет утечка.

2. `SecretReviewerWorkflowErrors` (ошибки воркфлоу за 15m, warning). GitHub получает свой 200 мгновенно (асинхронная схема), поэтому сбой агента не виден отправителю. PR остается без ревью, и владелец об этом не знает.

3. `SecretReviewerSlowReview` (`n8n_review_duration_seconds > 120`, 10m, warning). Ранняя деградация: если модель или MCP-серверы тормозят, это видно по длительности до того, как что-то упадет окончательно.

Общая логика: сломанный ревьюер выглядит как работающий, поэтому каждый алерт закрывает свой режим отказа (мертв, падает, тормозит).

## Известные ограничения

- Проверка подписи вебхука (`X-Hub-Signature-256`) не настроена: для локальной лабы допустимо, в проде обязательна.
- Агент видит сырые значения из диффа в своем контексте (модель через API). В отчёт и комментарий уходят только маски, но контекст модели - зона риска. Вариант устранения - локальная модель вместо API.
- Сканер на регулярках: ловит известные форматы токенов и присваивания, но не понимает код. Поэтому `block` не понижается никогда: критичные решения остаются за детерминированным слоем.