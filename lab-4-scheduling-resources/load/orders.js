// Нагрузка на путь заказа: POST /order (запись в postgres) + GET /orders (чтение).
// Параметры через env: BASE_URL, RATE (итераций в секунду), DURATION.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://shop-api.shop.svc:8080';

export const options = {
  scenarios: {
    orders: {
      // постоянный темп: RATE итераций в секунду, даже если сервис тормозит
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 20),
      timeUnit: '1s',
      duration: __ENV.DURATION || '40m',
      preAllocatedVUs: 20,
      maxVUs: 200,
    },
  },
  // черновик SLA — пригодится в Части 8
  thresholds: {
    http_req_duration: ['p(95)<300'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const created = http.post(`${BASE}/order`, JSON.stringify({ item: 'book', qty: 1 }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'POST /order' },
  });
  check(created, { 'order created': (r) => r.status === 201 });

  const list = http.get(`${BASE}/orders`, { tags: { name: 'GET /orders' } });
  check(list, { 'orders listed': (r) => r.status === 200 });
}
