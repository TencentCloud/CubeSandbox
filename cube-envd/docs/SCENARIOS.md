# cube-envd acceptance scenario results

Generated: 2026-09-14T06:00:24Z

| scenario | result | detail |
|---|---|---|
| health | PASS | GET /health -> 204 |
| identity | PASS | GET /status -> service=cube-envd ready=True |
| command.output | PASS | stdout='hello from cube-envd' stderr='warn' exitCode=0 status='exit status 0' |
| command.exit_code | PASS | exit 7 -> exitCode=7 error='exit status 7' |
| files.write | PASS | POST /files -> 200 |
| files.read | PASS | GET /files -> 200 body='cube-envd file scenario\n' |
| files.range | PASS | GET /files Range 0-3 -> 206 body='cube' |

cube-envd log: `/tmp/cube-envd-scenarios.log`
