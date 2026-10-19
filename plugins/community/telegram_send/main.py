#!/usr/bin/env python3
"""Send plain Telegram text or an inline base64 document; no automatic send retries."""
import base64
import binascii
import json
import os
import re
import sys
import urllib.error
import urllib.request
import uuid


class ConnectorError(Exception):
    def __init__(self, code, message):
        self.code, self.message = code, message


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def build_request(data):
    if not isinstance(data, dict):
        raise ConnectorError("bad_input", "input must be an object")
    token = os.environ.get("NOMOS_TELEGRAM_TOKEN", "")
    if not token:
        raise ConnectorError("no_token", "NOMOS_TELEGRAM_TOKEN is required")
    if not re.fullmatch(r"[0-9]+:[A-Za-z0-9_-]+", token):
        raise ConnectorError("bad_config", "invalid bot token format")
    chat = data.get("chat_id", os.environ.get("NOMOS_TELEGRAM_CHAT_ID", ""))
    if not isinstance(chat, str) or not chat or len(chat) > 128:
        raise ConnectorError("bad_input", "chat_id must be a nonempty string (or configure env fallback)")
    timeout = data.get("timeout_seconds", 30)
    if type(timeout) is not int or not 1 <= timeout <= 60:
        raise ConnectorError("bad_input", "timeout_seconds must be 1..60")
    text, document = data.get("text", ""), data.get("document_base64", "")
    if not isinstance(text, str) or not isinstance(document, str) or bool(text) == bool(document):
        raise ConnectorError("bad_input", "provide exactly one of text and document_base64")
    if text:
        if len(text.encode("utf-16-le")) // 2 > 4096:
            raise ConnectorError("text_limit", "text exceeds 4096 UTF-16 code units")
        payload = json.dumps({"chat_id": chat, "text": text}).encode()
        req = urllib.request.Request("https://api.telegram.org/bot" + token + "/sendMessage", data=payload,
                                     headers={"Content-Type": "application/json"}, method="POST")
        return req, timeout
    if len(document) > 8 * 1024 * 1024:
        raise ConnectorError("document_limit", "base64 input too large (decoded document limit 5 MiB)")
    try:
        content = base64.b64decode(document, validate=True)
    except (ValueError, binascii.Error):
        raise ConnectorError("bad_input", "invalid document_base64") from None
    if not content or len(content) > 5 * 1024 * 1024:
        raise ConnectorError("document_limit", "document must be 1 byte..5 MiB")
    filename = data.get("filename", "document.bin")
    if not isinstance(filename, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", filename):
        raise ConnectorError("bad_input", "filename must be a simple ASCII filename without paths")
    caption = data.get("caption", "")
    if not isinstance(caption, str) or len(caption.encode("utf-16-le")) // 2 > 1024:
        raise ConnectorError("bad_input", "caption exceeds 1024 UTF-16 code units")
    boundary = "nomos-" + uuid.uuid4().hex
    chunks = []
    for name, value in (("chat_id", chat), ("caption", caption)):
        chunks.append(("--" + boundary + '\r\nContent-Disposition: form-data; name="' + name + '"\r\n\r\n').encode() + value.encode() + b"\r\n")
    chunks.append(("--" + boundary + '\r\nContent-Disposition: form-data; name="document"; filename="' + filename + '"\r\nContent-Type: application/octet-stream\r\n\r\n').encode() + content + b"\r\n")
    chunks.append(("--" + boundary + "--\r\n").encode())
    req = urllib.request.Request("https://api.telegram.org/bot" + token + "/sendDocument", data=b"".join(chunks),
                                 headers={"Content-Type": "multipart/form-data; boundary=" + boundary}, method="POST")
    return req, timeout


def execute(data):
    req, timeout = build_request(data)
    # Do not use proxy settings from inherited env, and never follow a redirect with the token.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(req, timeout=timeout) as response:
            raw = response.read(65537)
            if len(raw) > 65536:
                raise ConnectorError("response_limit", "Telegram response exceeds 64 KiB")
            body = json.loads(raw)
    except urllib.error.HTTPError as error:
        raise ConnectorError("telegram_http_" + str(error.code), "Telegram request failed; do not blindly retry sends") from None
    except ConnectorError:
        raise
    except Exception:
        raise ConnectorError("telegram_network", "delivery outcome may be unknown; no automatic retry") from None
    if not isinstance(body, dict) or body.get("ok") is not True:
        raise ConnectorError("telegram_rejected", "Telegram did not confirm the send")
    try:
        message = body["result"]
        if type(message["message_id"]) is not int:
            raise ValueError()
        return {"sent": True, "message_id": message["message_id"], "chat_id": str(message["chat"]["id"])}
    except (KeyError, TypeError, ValueError):
        raise ConnectorError("telegram_response", "invalid response; send may have succeeded") from None


def main():
    try:
        data = json.load(sys.stdin)
    except (ValueError, UnicodeError):
        print(json.dumps({"status": "error", "error": {"code": "bad_json", "message": "invalid JSON", "retryable": False}}))
        return 2
    try:
        print(json.dumps({"status": "ok", "output": execute(data)}))
        return 0
    except ConnectorError as error:
        print(json.dumps({"status": "error", "error": {"code": error.code, "message": error.message, "retryable": False}}))
        return 1
    except Exception:
        print(json.dumps({"status": "error", "error": {"code": "internal_error", "message": "connector failed; token and response omitted", "retryable": False}}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
