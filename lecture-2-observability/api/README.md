# API для лабораторной по наблюдаемости

Локальный запуск:

```bash
python -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --reload
```

Проверка: `/health`, `/fail`, `/slow`, `/load`, `/metrics`.

Для отправки трейсов по OTLP задайте `OTEL_EXPORTER_OTLP_ENDPOINT`, например
`http://localhost:4318/v1/traces`. Имя сервиса задаётся через `OTEL_SERVICE_NAME`.
