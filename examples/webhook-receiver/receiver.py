#!/usr/bin/env python3
"""CubeSandbox Webhook Receiver — minimal Flask server for receiving and verifying webhook events."""

import argparse
import hashlib
import hmac
import json
import os
import sys

from flask import Flask, jsonify, request

app = Flask(__name__)

SHARED_SECRET = None  # type: str | None
LOG_FILE = "webhook_events.jsonl"


def verify_signature(body, signature):
    """Verify HMAC-SHA256 signature of the request body."""
    if SHARED_SECRET is None:
        return True
    expected = hmac.new(SHARED_SECRET.encode("utf-8"), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)


def save_event(payload):
    """Append event as one JSON line to the log file."""
    try:
        with open(LOG_FILE, "a", encoding="utf-8") as f:
            f.write(json.dumps(payload, ensure_ascii=False) + "\n")
    except OSError as e:
        print(f"[ERROR] Failed to write event: {e}", file=sys.stderr)


@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok"}), 200


@app.route("/webhook", methods=["POST"])
def webhook():
    raw_body = request.get_data()

    if SHARED_SECRET is not None:
        signature = request.headers.get("X-Cube-Webhook-Signature", "")
        if not signature:
            print("[WARN] Missing X-Cube-Webhook-Signature header", file=sys.stderr)
            return jsonify({"error": "missing signature"}), 401
        if not verify_signature(raw_body, signature):
            print("[WARN] Signature verification failed", file=sys.stderr)
            return jsonify({"error": "invalid signature"}), 403

    try:
        payload = json.loads(raw_body)
    except json.JSONDecodeError as e:
        print(f"[ERROR] Invalid JSON: {e}", file=sys.stderr)
        return jsonify({"error": "invalid json"}), 400

    event_type = payload.get("event", "unknown")
    sandbox_id = payload.get("sandbox_id", "?")
    template_id = payload.get("template_id")
    timestamp_iso = payload.get("timestamp", "")

    print(
        "[EVENT] %s | sandbox=%s%s | time=%s"
        % (
            event_type,
            sandbox_id,
            (" | template=" + template_id) if template_id else "",
            timestamp_iso,
        )
    )
    save_event(payload)
    return jsonify({"received": True}), 200


def main():
    global SHARED_SECRET

    parser = argparse.ArgumentParser(description="CubeSandbox Webhook Receiver")
    parser.add_argument(
        "--port", type=int, default=int(os.environ.get("WEBHOOK_RECEIVER_PORT", "5000"))
    )
    parser.add_argument("--secret", default=os.environ.get("WEBHOOK_SECRET"))

    args = parser.parse_args()
    SHARED_SECRET = args.secret

    print("=" * 56)
    print("  CubeSandbox Webhook Receiver")
    print("  Listening on 0.0.0.0:%d" % args.port)
    if SHARED_SECRET:
        print("  Signature verification: ENABLED (HMAC-SHA256)")
    else:
        print("  Signature verification: DISABLED")
    print("  Events saved to: %s" % os.path.abspath(LOG_FILE))
    print("=" * 56)
    print()

    app.run(host="0.0.0.0", port=args.port, debug=False)


if __name__ == "__main__":
    main()
