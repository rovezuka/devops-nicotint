import json
import logging
import os
import random
import time
from contextlib import asynccontextmanager
from contextvars import ContextVar

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import PlainTextResponse, Response
from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from starlette.middleware.base import BaseHTTPMiddleware

logger = logging.getLogger("api")
request_id: ContextVar[str] = ContextVar("request_id", default="-")

REQUESTS = Counter(
    "http_requests_total",
    "Total HTTP requests",
    ("method", "path", "status"),
)
ERRORS = Counter(
    "http_errors_total",
    "Total HTTP 5xx responses",
    ("method", "path"),
)
LATENCY = Histogram(
    "http_response_duration_seconds",
    "HTTP response duration in seconds",
    ("method", "path"),
)


def configure_logging() -> None:
    class JsonFormatter(logging.Formatter):
        def format(self, record: logging.LogRecord) -> str:
            span = trace.get_current_span()
            context = span.get_span_context()
            trace_id = format(context.trace_id, "032x") if context.is_valid else "-"
            return json.dumps(
                {
                    "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                    "level": record.levelname.lower(),
                    "message": record.getMessage(),
                    "trace_id": trace_id,
                    "request_id": request_id.get(),
                }
            )

    handler = logging.StreamHandler()
    handler.setFormatter(JsonFormatter())
    logger.handlers.clear()
    logger.addHandler(handler)
    logger.setLevel(logging.INFO)
    logger.propagate = False


def configure_tracing() -> None:
    resource = Resource.create({"service.name": os.getenv("OTEL_SERVICE_NAME", "api")})
    provider = TracerProvider(resource=resource)
    endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
    if endpoint:
        provider.add_span_processor(
            BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint))
        )
    trace.set_tracer_provider(provider)


class RequestMetricsMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request: Request, call_next):
        started = time.perf_counter()
        response = await call_next(request)
        path = request.url.path
        method = request.method
        status = str(response.status_code)
        REQUESTS.labels(method, path, status).inc()
        LATENCY.labels(method, path).observe(time.perf_counter() - started)
        if response.status_code >= 500:
            ERRORS.labels(method, path).inc()
        request_id.set(response.headers.get("x-request-id", "-"))
        logger.info("request completed method=%s path=%s status=%s", method, path, status)
        return response


@asynccontextmanager
async def lifespan(_: FastAPI):
    configure_logging()
    configure_tracing()
    yield
    provider = trace.get_tracer_provider()
    if isinstance(provider, TracerProvider):
        provider.shutdown()


app = FastAPI(title="observability-api", lifespan=lifespan)
app.add_middleware(RequestMetricsMiddleware)
tracer = trace.get_tracer("api")


@app.get("/metrics", include_in_schema=False)
async def metrics() -> Response:
    return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)


@app.get("/health", response_class=PlainTextResponse)
async def health() -> str:
    return "ok"


@app.get("/fail")
async def fail():
    span = trace.get_current_span()
    span.set_status(trace.Status(trace.StatusCode.ERROR, "intentional failure"))
    logger.error("intentional failure requested")
    return PlainTextResponse("intentional failure", status_code=500)


@app.get("/slow")
async def slow():
    delay = random.uniform(1, 3)
    with tracer.start_as_current_span("slow-op") as span:
        span.set_attribute("delay.seconds", delay)
        time.sleep(delay)
    return {"status": "slow", "delay_seconds": round(delay, 2)}


@app.get("/load")
async def load():
    base_url = os.getenv("SELF_URL", "http://127.0.0.1:8000")
    async with httpx.AsyncClient(base_url=base_url, timeout=10) as client:
        responses = await __import__("asyncio").gather(
            *(client.get("/health") for _ in range(10))
        )
    return {"requests": len(responses), "successful": sum(r.is_success for r in responses)}


FastAPIInstrumentor.instrument_app(app)
