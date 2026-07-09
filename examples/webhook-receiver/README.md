# CubeSandbox Webhook Receiver

This example starts a small HTTP receiver for CubeAPI webhook callbacks. It uses
only the Python standard library.

## Run

```bash
cd examples/webhook-receiver
python3 receiver.py --host 127.0.0.1 --port 9000
```

Enable signature verification:

```bash
WEBHOOK_SECRET=change-me python3 receiver.py --host 127.0.0.1 --port 9000
```

Configure CubeAPI:

```bash
export CUBE_API_WEBHOOK_URLS=http://127.0.0.1:9000/webhook
export CUBE_API_WEBHOOK_SECRET=change-me
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
sudo systemctl restart cube-sandbox-cube-api.service
```

Create, pause, resume, or delete a sandbox. The receiver prints each callback as
one JSON line containing the event name, timestamp, sandbox ID, optional template
ID, and Cube webhook headers.
