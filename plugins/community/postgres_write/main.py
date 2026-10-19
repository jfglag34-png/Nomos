#!/usr/bin/env python3
"""Nomos PostgreSQL connector. Explicitly enabled transactional DML writes; no automatic retries."""
import base64
import datetime
import decimal
import json
import os
import re
import sys
import uuid

WRITE = True
MAX_OUTPUT = 8 * 1024 * 1024


class ConnectorError(Exception):
    def __init__(self, code, message, retryable=False):
        self.code, self.message, self.retryable = code, message, retryable


def integer(data, name, default, minimum, maximum):
    value = data.get(name, default)
    if type(value) is not int or not minimum <= value <= maximum:
        raise ConnectorError("bad_input", name + " is outside allowed bounds")
    return value


def json_value(value):
    if isinstance(value, (datetime.date, datetime.time, datetime.datetime)):
        return value.isoformat()
    if isinstance(value, (decimal.Decimal, uuid.UUID)):
        return str(value)
    if isinstance(value, (bytes, bytearray, memoryview)):
        return {"base64": base64.b64encode(bytes(value)).decode("ascii")}
    raise TypeError("unsupported database value")


def execute(data, driver=None):
    if not isinstance(data, dict):
        raise ConnectorError("bad_input", "input must be a JSON object")
    sql = data.get("sql")
    if not isinstance(sql, str) or not sql.strip() or len(sql) > 65536:
        raise ConnectorError("bad_input", "sql must be a nonempty string up to 65536 characters")
    if "params" in data and "named_params" in data:
        raise ConnectorError("bad_input", "use params OR named_params")
    params = data.get("named_params", data.get("params", []))
    if not isinstance(params, (list, dict)):
        raise ConnectorError("bad_input", "params must be an array or object")
    maximum = integer(data, "max_rows", 1000, 1, 10000)
    timeout = integer(data, "statement_timeout_ms", 30000, 1, 120000)
    if WRITE:
        if os.environ.get("NOMOS_POSTGRES_ALLOW_WRITE") != "1":
            raise ConnectorError("write_disabled", "operator must set NOMOS_POSTGRES_ALLOW_WRITE=1")
        if not re.match(r"^\s*(INSERT|UPDATE|DELETE)\b", sql, re.IGNORECASE):
            raise ConnectorError("write_query_required", "write connector accepts a single INSERT, UPDATE or DELETE")
    dsn = os.environ.get("NOMOS_POSTGRES_DSN", "")
    if not dsn:
        raise ConnectorError("no_dsn", "NOMOS_POSTGRES_DSN is required")
    if driver is None:
        try:
            import psycopg as driver
        except ImportError:
            raise ConnectorError("dependency_missing", "install the pinned requirements.lock") from None
    try:
        with driver.connect(dsn, connect_timeout=10, autocommit=False, client_encoding="UTF8") as conn:
            # Must precede all user SQL. The read connector does not trust a SELECT regex.
            conn.read_only = not WRITE
            with conn.cursor() as cur:
                cur.execute("SELECT set_config('statement_timeout', %s, true)", (str(timeout),))
                cur.execute("SELECT set_config('lock_timeout', %s, true)", (str(min(timeout, 10000)),))
                # Extended protocol rejects multiple SQL statements; values never interpolated.
                cur.execute(sql, params, prepare=True)
                columns, rows, truncated = [], [], False
                if cur.description is not None:
                    columns = [column.name for column in cur.description]
                    rows = cur.fetchmany(maximum + 1)
                    truncated = len(rows) > maximum
                    rows = [list(row) for row in rows[:maximum]]
                if WRITE and truncated:
                    # Roll back rather than commit a write while hiding part of its RETURNING.
                    raise ConnectorError("result_limit", "write RETURNING exceeded max_rows; transaction rolled back")
                output = {"columns": columns, "rows": rows, "row_count": cur.rowcount,
                          "truncated": truncated, "read_only": not WRITE}
                try:
                    raw = json.dumps(output, default=json_value, ensure_ascii=False, allow_nan=False)
                except (TypeError, ValueError):
                    raise ConnectorError("result_encoding", "result cannot be encoded; transaction rolled back") from None
                if len(raw.encode("utf-8")) > MAX_OUTPUT:
                    raise ConnectorError("result_limit", "result exceeds 8 MiB; transaction rolled back")
            # Context commits only after validation and output-size checks; errors roll back.
        return json.loads(raw)
    except ConnectorError:
        raise
    except Exception as error:
        state = getattr(error, "sqlstate", None)
        safe_state = state if isinstance(state, str) and re.fullmatch(r"[0-9A-Z]{5}", state) else "unknown"
        # Write commit outcome may be ambiguous. Never automatically mark it retryable.
        raise ConnectorError("database_error", "database operation failed (SQLSTATE " + safe_state + ")", False) from None


def main():
    try:
        data = json.load(sys.stdin)
    except (ValueError, UnicodeError):
        print(json.dumps({"status": "error", "error": {"code": "bad_json", "message": "invalid JSON", "retryable": False}}))
        return 2
    try:
        output = execute(data)
        print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False, allow_nan=False))
        return 0
    except ConnectorError as error:
        print(json.dumps({"status": "error", "error": {"code": error.code, "message": error.message, "retryable": error.retryable}}))
        return 1
    except Exception:
        print(json.dumps({"status": "error", "error": {"code": "internal_error", "message": "connector failed; credentials and SQL omitted", "retryable": False}}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
