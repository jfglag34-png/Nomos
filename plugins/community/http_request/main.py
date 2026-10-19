#!/usr/bin/env python3
"""Bounded JSON HTTP connector. Operator-selected base URL, pinned DNS, no redirects."""
import http.client
import ipaddress
import json
import os
import socket
import ssl
import sys
import time
import urllib.parse

MAX_BYTES = 4 * 1024 * 1024


class ConnectorError(Exception):
    def __init__(self, code, message, retryable=False):
        self.code, self.message, self.retryable = code, message, retryable


def bounded_int(data, key, default, low, high):
    value = data.get(key, default)
    if type(value) is not int or not low <= value <= high:
        raise ConnectorError("bad_input", key + " is outside allowed bounds")
    return value


def path_value(obj, path):
    for part in path.split("."):
        if not isinstance(obj, dict) or part not in obj:
            return None
        obj = obj[part]
    return obj


def target(base, path):
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme not in ("https", "http") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ConnectorError("bad_config", "base URL must be an HTTP(S) origin/path without credentials, query or fragment")
    if parsed.scheme != "https" and os.environ.get("NOMOS_HTTP_ALLOW_INSECURE") != "1":
        raise ConnectorError("insecure_url", "HTTPS required unless operator explicitly allows HTTP")
    if not isinstance(path, str) or len(path) > 8192 or any(ord(c) < 32 or ord(c) == 127 for c in path):
        raise ConnectorError("bad_input", "invalid path")
    prefix = parsed.path.rstrip("/") + "/"
    if urllib.parse.urlsplit(path).scheme or path.startswith("//"):
        candidate = urllib.parse.urlsplit(path)
        if (candidate.scheme, candidate.hostname, candidate.port or (443 if candidate.scheme == "https" else 80)) != (parsed.scheme, parsed.hostname, parsed.port or (443 if parsed.scheme == "https" else 80)) or candidate.username or candidate.password:
            raise ConnectorError("url_denied", "pagination cannot leave configured origin")
        url = path
    else:
        url = urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, prefix + path.lstrip("/"), "", ""))
        # Preserve a caller-supplied query without interpreting it as part of the path.
        url = urllib.parse.urlunsplit(urllib.parse.urlsplit(url))
    final = urllib.parse.urlsplit(url)
    decoded = urllib.parse.unquote(final.path)
    if final.fragment or "\\" in decoded or any(part in (".", "..") for part in decoded.split("/")) or not final.path.startswith(prefix):
        raise ConnectorError("url_denied", "path must stay under configured base path")
    return final


def addresses(host, port):
    try:
        resolved = sorted({info[4][0] for info in socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)})
    except OSError:
        raise ConnectorError("dns_error", "DNS lookup failed", True) from None
    if not resolved:
        raise ConnectorError("dns_error", "DNS lookup returned no addresses", True)
    if os.environ.get("NOMOS_HTTP_ALLOW_PRIVATE") != "1":
        if any(not ipaddress.ip_address(value.split("%")[0]).is_global for value in resolved):
            raise ConnectorError("private_address", "private, loopback, link-local and reserved addresses require operator permission")
    return resolved


class PinnedHTTPS(http.client.HTTPSConnection):
    def connect(self):
        raw = socket.create_connection((self.resolved_ip, self.port), self.timeout)
        try:
            self.sock = self._context.wrap_socket(raw, server_hostname=self.host)
        except BaseException:
            raw.close()
            raise


class PinnedHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.create_connection((self.resolved_ip, self.port), self.timeout)


def request_once(url, method, headers, payload, timeout):
    port = url.port or (443 if url.scheme == "https" else 80)
    ips = addresses(url.hostname, port)
    cls = PinnedHTTPS if url.scheme == "https" else PinnedHTTP
    conn = cls(url.hostname, port, timeout=timeout)
    conn.resolved_ip = ips[0]  # Connect exactly to a validated address; no second DNS lookup.
    try:
        path = urllib.parse.urlunsplit(("", "", url.path, url.query, ""))
        conn.request(method, path, body=payload, headers=headers)
        response = conn.getresponse()
        if response.getheader("Content-Encoding", "identity").lower() != "identity":
            raise ConnectorError("response_encoding", "compressed responses are not accepted")
        raw = response.read(MAX_BYTES + 1)
        if len(raw) > MAX_BYTES:
            raise ConnectorError("response_limit", "response exceeds 4 MiB")
        return response.status, response.getheader("Retry-After", ""), raw
    finally:
        conn.close()


def execute(data):
    if not isinstance(data, dict):
        raise ConnectorError("bad_input", "input must be an object")
    base = os.environ.get("NOMOS_HTTP_BASE_URL", "")
    if not base:
        raise ConnectorError("no_base_url", "operator must configure NOMOS_HTTP_BASE_URL")
    method = data.get("method", "GET")
    if not isinstance(method, str):
        raise ConnectorError("bad_input", "method must be a string")
    method = method.upper()
    if method not in ("GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"):
        raise ConnectorError("bad_input", "unsupported HTTP method")
    safe = method in ("GET", "HEAD")
    if not safe and os.environ.get("NOMOS_HTTP_ALLOW_WRITE") != "1":
        raise ConnectorError("write_disabled", "operator must set NOMOS_HTTP_ALLOW_WRITE=1 for mutating methods")
    timeout = bounded_int(data, "timeout_seconds", 30, 1, 60)
    retries = bounded_int(data, "retries", 2, 0, 3) if safe else 0
    pages = bounded_int(data, "max_pages", 1, 1, 20)
    if not safe and pages != 1:
        raise ConnectorError("bad_input", "pagination is read-only")
    next_path = data.get("next_path", "")
    if not isinstance(next_path, str) or len(next_path) > 128:
        raise ConnectorError("bad_input", "next_path must be a short dot-separated JSON path")
    headers = {"Accept": "application/json", "Accept-Encoding": "identity", "User-Agent": "Nomos-connectors/0.1"}
    try:
        custom = json.loads(os.environ.get("NOMOS_HTTP_HEADERS_JSON", "{}"))
    except ValueError:
        raise ConnectorError("bad_config", "invalid operator headers JSON") from None
    if not isinstance(custom, dict):
        raise ConnectorError("bad_config", "operator headers must be an object")
    for key, value in custom.items():
        if not isinstance(key, str) or not isinstance(value, str) or not key or any(c in key + value for c in "\r\n\x00") or key.lower() in ("host", "content-length", "transfer-encoding", "connection", "accept-encoding"):
            raise ConnectorError("bad_config", "invalid or unsafe operator header")
        headers[key] = value
    token = os.environ.get("NOMOS_HTTP_BEARER_TOKEN", "")
    if token:
        if any(c in token for c in "\r\n\x00"):
            raise ConnectorError("bad_config", "invalid bearer token")
        headers["Authorization"] = "Bearer " + token
    idempotency = data.get("idempotency_key", "")
    if idempotency:
        if not isinstance(idempotency, str) or len(idempotency) > 128 or any(ord(c) < 33 or ord(c) > 126 for c in idempotency):
            raise ConnectorError("bad_input", "invalid idempotency_key")
        headers["Idempotency-Key"] = idempotency
    payload = None
    if "body" in data:
        if safe:
            raise ConnectorError("bad_input", "GET/HEAD must not contain a JSON body")
        try:
            payload = json.dumps(data["body"], allow_nan=False).encode()
        except (ValueError, TypeError):
            raise ConnectorError("bad_input", "body is not JSON encodable") from None
        if len(payload) > MAX_BYTES:
            raise ConnectorError("request_limit", "request body exceeds 4 MiB")
        headers["Content-Type"] = "application/json"
    path = data.get("path", "")
    responses, total, truncated, seen = [], 0, False, set()
    for _ in range(pages):
        url = target(base, path)
        address = urllib.parse.urlunsplit(url)
        if address in seen:
            raise ConnectorError("pagination_cycle", "pagination repeated a URL")
        seen.add(address)
        for attempt in range(retries + 1):
            try:
                status, retry_after, raw = request_once(url, method, headers, payload, timeout)
            except (OSError, http.client.HTTPException):
                if attempt < retries:
                    time.sleep(min(2 ** attempt, 5))
                    continue
                raise ConnectorError("network_error", "request failed; credentials and URL omitted", safe) from None
            if safe and status in (429, 502, 503, 504) and attempt < retries:
                delay = min(int(retry_after), 5) if retry_after.isdigit() else min(2 ** attempt, 5)
                time.sleep(delay)
                continue
            break
        if status < 200 or status >= 300:
            raise ConnectorError("http_" + str(status), "remote HTTP " + str(status) + "; response body omitted", safe and (status == 429 or status >= 500))
        total += len(raw)
        if total > MAX_BYTES:
            raise ConnectorError("response_limit", "combined page bodies exceed 4 MiB")
        try:
            body = json.loads(raw) if raw else None
        except (ValueError, UnicodeError):
            raise ConnectorError("not_json", "remote response is not JSON") from None
        responses.append({"status_code": status, "body": body})
        path = path_value(body, next_path) if next_path else None
        if not path:
            break
        if not isinstance(path, str):
            raise ConnectorError("pagination_value", "next page must be a URL or relative path string")
        path = urllib.parse.urljoin(address, path)
        truncated = len(responses) == pages
    return {"responses": responses, "page_count": len(responses), "truncated": truncated}


def main():
    try:
        data = json.load(sys.stdin)
    except (ValueError, UnicodeError):
        print(json.dumps({"status": "error", "error": {"code": "bad_json", "message": "invalid JSON", "retryable": False}}))
        return 2
    try:
        print(json.dumps({"status": "ok", "output": execute(data)}, ensure_ascii=False, allow_nan=False))
        return 0
    except ConnectorError as error:
        print(json.dumps({"status": "error", "error": {"code": error.code, "message": error.message, "retryable": error.retryable}}))
        return 1
    except Exception:
        print(json.dumps({"status": "error", "error": {"code": "internal_error", "message": "connector failed; credentials and URL omitted", "retryable": False}}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
