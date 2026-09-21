// k6 login load test.
//
// The point of using k6 rather than vegeta here: no body files. Each virtual
// user picks a username at runtime, so "200 unique users" is a constant to
// change, not 200 files to generate.
//
// Run:
//   k6 run -e RATE=3000 -e DURATION=30s loadtest/login.js
//
// RATE is an OPEN-LOOP arrival rate: k6 keeps starting new requests at this
// rate whether or not the server keeps up. That is the only way to answer
// "does it support 3000/s" — a closed-loop tool silently slows down with the
// server and reports a flattering latency for a rate it never actually drove.
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate } from 'k6/metrics';

const RATE = Number(__ENV.RATE || 3000);
const DURATION = __ENV.DURATION || '30s';
const BASE = __ENV.BASE || 'http://127.0.0.1:8080';

// The spec asks for at least 200 distinct users. They are spread across the
// whole 10M keyspace rather than taken from the front, so the test cannot
// accidentally benefit from rows that happen to sit on the same InnoDB pages.
const UNIQUE_USERS = Number(__ENV.USERS || 200);
const STRIDE = Math.floor(10000000 / UNIQUE_USERS);
const PASSWORD = __ENV.PASSWORD || 'password123';

const socketErrors = new Counter('socket_errors');
const loginOK = new Rate('login_success');

export const options = {
  scenarios: {
    login: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      // Must be high enough that k6 never throttles itself; if k6 runs out of
      // VUs it becomes closed-loop and the numbers stop meaning anything.
      // k6 warns loudly when this happens — do not ignore that warning.
      preAllocatedVUs: Math.min(RATE * 2, 4000),
      maxVUs: Math.min(RATE * 4, 8000),
    },
  },
  // The spec's pass/fail criteria, encoded so the run exits non-zero on a miss.
  thresholds: {
    http_req_failed: ['rate<0.01'],
    login_success: ['rate>0.99'],
    socket_errors: ['count==0'],
    http_req_duration: ['p(99)<1000'],
  },
  discardResponseBodies: false,
  // Cookies are irrelevant to a login benchmark and only add bookkeeping.
  noCookiesReset: false,
};

export default function () {
  const n = (__ITER % UNIQUE_USERS) * STRIDE + 1;
  const username = 'user' + String(n).padStart(8, '0');

  const res = http.post(
    `${BASE}/api/login`,
    JSON.stringify({ username: username, password: PASSWORD }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'login' } },
  );

  // error_code 0 means a real HTTP response came back. Anything else is a
  // connection reset, refusal or timeout — the "no socket/timeout error"
  // requirement is exactly this counter staying at zero.
  if (res.error_code !== 0) socketErrors.add(1);

  loginOK.add(res.status === 200);
  check(res, {
    'status 200': (r) => r.status === 200,
    'has user_id': (r) => r.status === 200 && r.json('user_id') !== undefined,
  });
}
