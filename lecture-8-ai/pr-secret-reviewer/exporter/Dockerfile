FROM python:3.12-slim

WORKDIR /app

RUN pip install --no-cache-dir requests prometheus_client

COPY n8n_exporter.py .

CMD ["python", "n8n_exporter.py"]