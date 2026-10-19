"""Offline unit/HTTP tests plus real PostgreSQL tests when NOMOS_TEST_POSTGRES_DSN is set."""
import base64
import importlib.util
import json
import os
from pathlib import Path
import threading
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import uuid

ROOT = Path(__file__).resolve().parents[2]
DB_DSN = os.environ.get("NOMOS_TEST_POSTGRES_DSN", "")


def load(name):
    spec = importlib.util.spec_from_file_location("nomos_" + name, ROOT / "plugins/community" / name / "main.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


pg_read, pg_write, http, telegram = [load(name) for name in ("postgres_read", "postgres_write", "http_request", "telegram_send")]


class FakeColumn:
    name = "value"


class FakeCursor:
    description = [FakeColumn()]
    rowcount = 2

    def __init__(self):
        self.calls = []

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False

    def execute(self, *args, **kwargs):
        self.calls.append((args, kwargs))

    def fetchmany(self, count):
        return [(1,), (2,)][:count]


class FakeConnection:
    def __init__(self):
        self.read_only = None
        self.cur = FakeCursor()
        self.error_on_exit = None

    def __enter__(self):
        return self

    def __exit__(self, kind, error, tb):
        self.error_on_exit = error
        return False

    def cursor(self):
        return self.cur


class PostgresOfflineTests(unittest.TestCase):
    def test_database_readonly_and_prepared_parameters(self):
        conn = FakeConnection()
        driver = mock.Mock()
        driver.connect.return_value = conn
        value = "'; DROP TABLE people; --"
        with mock.patch.dict(os.environ, {"NOMOS_POSTGRES_DSN": "hidden"}, clear=True):
            result = pg_read.execute({"sql": "SELECT %s", "params": [value], "max_rows": 1}, driver)
        self.assertTrue(conn.read_only)
        self.assertTrue(result["truncated"])
        args, kwargs = conn.cur.calls[-1]
        self.assertEqual(args, ("SELECT %s", [value]))
        self.assertTrue(kwargs["prepare"])

    def test_write_optin_and_rollback_on_result_limit(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(pg_write.ConnectorError) as error:
                pg_write.execute({"sql": "DELETE FROM x"})
            self.assertEqual(error.exception.code, "write_disabled")
        conn = FakeConnection()
        driver = mock.Mock()
        driver.connect.return_value = conn
        with mock.patch.dict(os.environ, {"NOMOS_POSTGRES_DSN": "hidden", "NOMOS_POSTGRES_ALLOW_WRITE": "1"}, clear=True):
            with self.assertRaises(pg_write.ConnectorError) as error:
                pg_write.execute({"sql": "INSERT INTO x VALUES (1) RETURNING x", "max_rows": 1}, driver)
            self.assertEqual(error.exception.code, "result_limit")
        self.assertFalse(conn.read_only)
        self.assertIsNotNone(conn.error_on_exit)

    def test_error_does_not_leak_dsn_sql_or_params(self):
        driver = mock.Mock()
        driver.connect.side_effect = RuntimeError("password=SECRET SELECT confidential")
        with mock.patch.dict(os.environ, {"NOMOS_POSTGRES_DSN": "password=SECRET"}, clear=True):
            with self.assertRaises(pg_read.ConnectorError) as error:
                pg_read.execute({"sql": "SELECT %s", "params": ["SECRET"]}, driver)
        self.assertNotIn("SECRET", error.exception.message)
        self.assertFalse(error.exception.retryable)

    def test_write_rejects_ddl_and_read_inputs_are_bounded(self):
        with mock.patch.dict(os.environ, {"NOMOS_POSTGRES_ALLOW_WRITE": "1"}, clear=True):
            with self.assertRaises(pg_write.ConnectorError):
                pg_write.execute({"sql": "DROP TABLE x"})
        for data in ({"sql": "SELECT 1", "max_rows": 10001}, {"sql": "SELECT 1", "statement_timeout_ms": 0}, {"sql": "SELECT 1", "params": [], "named_params": {}}):
            with self.assertRaises(pg_read.ConnectorError):
                pg_read.execute(data)


class HTTPHandler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send_json(self, body, status=200, headers=None):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(raw)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self.server.calls.append((self.command, self.path, self.headers.get("Authorization")))
        if self.path == "/v1/retry" and sum(x[1] == self.path for x in self.server.calls) == 1:
            self.send_json({}, 429, {"Retry-After": "0"})
        elif self.path == "/v1/page1":
            self.send_json({"items": [1], "next": "page2"})
        elif self.path == "/v1/page2":
            self.send_json({"items": [2], "next": None})
        elif self.path == "/v1/escape":
            self.send_json({"next": "https://attacker.invalid/steal"})
        elif self.path == "/v1/redirect":
            self.send_json({}, 302, {"Location": "/v1/never"})
        elif self.path == "/v1/cycle":
            self.send_json({"next": "cycle"})
        else:
            self.send_json({"ok": True})

    def do_POST(self):
        self.server.calls.append((self.command, self.path, self.headers.get("Authorization")))
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        self.send_json({"password": "DO_NOT_LEAK"}, 503)


class HTTPTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), HTTPHandler)
        cls.server.calls = []
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def setUp(self):
        self.server.calls.clear()
        self.env = mock.patch.dict(os.environ, {"NOMOS_HTTP_BASE_URL": "http://127.0.0.1:%d/v1" % self.server.server_port,
                                               "NOMOS_HTTP_ALLOW_PRIVATE": "1", "NOMOS_HTTP_ALLOW_INSECURE": "1",
                                               "NOMOS_HTTP_BEARER_TOKEN": "SECRET"}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_pagination_and_authorization(self):
        result = http.execute({"path": "page1", "next_path": "next", "max_pages": 3})
        self.assertEqual(result["page_count"], 2)
        self.assertFalse(result["truncated"])
        self.assertEqual([x["body"]["items"] for x in result["responses"]], [[1], [2]])
        self.assertEqual([x[2] for x in self.server.calls], ["Bearer SECRET", "Bearer SECRET"])

    def test_bounded_pagination(self):
        result = http.execute({"path": "page1", "next_path": "next", "max_pages": 1})
        self.assertTrue(result["truncated"])
        self.assertEqual(len(self.server.calls), 1)

    def test_429_retry_only_for_safe_method(self):
        result = http.execute({"path": "retry"})
        self.assertEqual(result["page_count"], 1)
        self.assertEqual(len(self.server.calls), 2)
        os.environ["NOMOS_HTTP_ALLOW_WRITE"] = "1"
        with self.assertRaises(http.ConnectorError) as error:
            http.execute({"path": "write", "method": "POST", "body": {"x": 1}})
        self.assertEqual(error.exception.code, "http_503")
        self.assertFalse(error.exception.retryable)
        self.assertNotIn("DO_NOT_LEAK", error.exception.message)
        self.assertEqual(sum(x[0] == "POST" for x in self.server.calls), 1)

    def test_redirect_not_followed(self):
        with self.assertRaises(http.ConnectorError) as error:
            http.execute({"path": "redirect"})
        self.assertEqual(error.exception.code, "http_302")
        self.assertEqual(len(self.server.calls), 1)

    def test_cross_origin_next_and_cycles_rejected(self):
        for path, code in (("escape", "url_denied"), ("cycle", "pagination_cycle")):
            with self.assertRaises(http.ConnectorError) as error:
                http.execute({"path": path, "next_path": "next", "max_pages": 3})
            self.assertEqual(error.exception.code, code)

    def test_default_private_and_write_denial(self):
        del os.environ["NOMOS_HTTP_ALLOW_PRIVATE"]
        with self.assertRaises(http.ConnectorError) as error:
            http.execute({"path": "private"})
        self.assertEqual(error.exception.code, "private_address")
        with self.assertRaises(http.ConnectorError) as error:
            http.execute({"method": "DELETE"})
        self.assertEqual(error.exception.code, "write_disabled")

    def test_path_escape_and_arbitrary_host_denied(self):
        base = os.environ["NOMOS_HTTP_BASE_URL"]
        for path in ("../admin", "%2e%2e/admin", "https://attacker.invalid/", "//attacker.invalid/", "x\r\nBad: value"):
            with self.assertRaises(http.ConnectorError):
                http.target(base, path)

    def test_connects_to_validated_ip_with_tls_hostname_preserved(self):
        conn = http.PinnedHTTPS("api.example.com", 443, timeout=3)
        conn.resolved_ip = "93.184.216.34"
        with mock.patch.object(http.socket, "create_connection") as create:
            conn._context = mock.Mock()
            conn.connect()
            create.assert_called_once_with(("93.184.216.34", 443), 3)
            conn._context.wrap_socket.assert_called_once_with(create.return_value, server_hostname="api.example.com")


class TelegramTests(unittest.TestCase):
    def setUp(self):
        self.env = mock.patch.dict(os.environ, {"NOMOS_TELEGRAM_TOKEN": "123:SECRET", "NOMOS_TELEGRAM_CHAT_ID": "42"}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_text_request_and_document_multipart(self):
        req, _ = telegram.build_request({"text": "hello"})
        self.assertTrue(req.full_url.endswith("/sendMessage"))
        self.assertEqual(json.loads(req.data), {"chat_id": "42", "text": "hello"})
        req, _ = telegram.build_request({"document_base64": base64.b64encode(b"file contents").decode(), "filename": "report.txt"})
        self.assertTrue(req.full_url.endswith("/sendDocument"))
        self.assertIn(b'filename="report.txt"', req.data)
        self.assertIn(b"file contents", req.data)

    def test_rejects_paths_oversize_and_conflicting_modes(self):
        for data in ({"document_base64": "aGVsbG8=", "filename": "../secret"}, {"document_base64": "!!!"},
                     {"text": "a" * 4097}, {"text": "x", "document_base64": "aGVsbG8="}):
            with self.assertRaises(telegram.ConnectorError):
                telegram.build_request(data)

    def test_network_error_never_retries_or_leaks_token(self):
        opener = mock.Mock()
        opener.open.side_effect = OSError("https://api.telegram.org/bot123:SECRET/sendMessage")
        with mock.patch.object(telegram.urllib.request, "build_opener", return_value=opener):
            with self.assertRaises(telegram.ConnectorError) as error:
                telegram.execute({"text": "hi"})
        self.assertEqual(opener.open.call_count, 1)
        self.assertNotIn("SECRET", error.exception.message)

    def test_telegram_requires_ok_true(self):
        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b'{"ok":false}'
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch.object(telegram.urllib.request, "build_opener", return_value=opener):
            with self.assertRaises(telegram.ConnectorError):
                telegram.execute({"text": "hi"})


@unittest.skipUnless(DB_DSN, "set NOMOS_TEST_POSTGRES_DSN to run real database tests")
class PostgreSQLIntegrationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import psycopg
        cls.driver = psycopg
        cls.table = "nomos_connector_test_" + uuid.uuid4().hex
        with psycopg.connect(DB_DSN) as conn:
            conn.execute('CREATE TABLE "' + cls.table + '" (value text)')

    @classmethod
    def tearDownClass(cls):
        with cls.driver.connect(DB_DSN) as conn:
            conn.execute('DROP TABLE "' + cls.table + '"')

    def setUp(self):
        self.env = mock.patch.dict(os.environ, {"NOMOS_POSTGRES_DSN": DB_DSN, "NOMOS_POSTGRES_ALLOW_WRITE": "1"}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        with self.driver.connect(DB_DSN) as conn:
            conn.execute('DELETE FROM "' + self.table + '"')

    def test_parameterized_write_read_and_injection_literal(self):
        value = "'; DROP TABLE example; --"
        result = pg_write.execute({"sql": 'INSERT INTO "' + self.table + '" VALUES (%s) RETURNING value', "params": [value]})
        self.assertEqual(result["row_count"], 1)
        result = pg_read.execute({"sql": 'SELECT value FROM "' + self.table + '"'})
        self.assertEqual(result["rows"], [[value]])

    def test_readonly_blocks_writes_and_changing_transaction_mode(self):
        for sql in ('INSERT INTO "' + self.table + '" VALUES (\'bad\')', "SET TRANSACTION READ WRITE"):
            with self.assertRaises(pg_read.ConnectorError):
                pg_read.execute({"sql": sql})

    def test_multiple_statements_rejected(self):
        with self.assertRaises(pg_write.ConnectorError):
            pg_write.execute({"sql": 'INSERT INTO "' + self.table + '" VALUES (%s); DELETE FROM "' + self.table + '"', "params": ["x"]})
        result = pg_read.execute({"sql": 'SELECT value FROM "' + self.table + '"'})
        self.assertEqual(result["rows"], [])

    def test_returning_limit_rolls_back_entire_write(self):
        with self.assertRaises(pg_write.ConnectorError):
            pg_write.execute({"sql": 'INSERT INTO "' + self.table + '" VALUES (\'a\'), (\'b\') RETURNING value', "max_rows": 1})
        self.assertEqual(pg_read.execute({"sql": 'SELECT value FROM "' + self.table + '"'})["rows"], [])

    def test_read_query_timeout(self):
        with self.assertRaises(pg_read.ConnectorError):
            pg_read.execute({"sql": "SELECT pg_sleep(0.05)", "statement_timeout_ms": 1})


if __name__ == "__main__":
    unittest.main()
