#!/usr/bin/env python3
"""Regression tests for the bashlex-based risk classifier.

Covers:
  - every review-bot bypass sample (prefix commands, env assignments,
    quoted / unspaced redirects) — must be RISKY
  - fail-safe semantics: unparseable constructs (arithmetic expansion,
    `[[ ]]`, fork bombs) are RISKY by design (never silently safe)
  - legacy behavior guards: /dev/null + fd redirects are SAFE, whitelist
    applies per command, destructive redirect beats whitelist
  - new structural rules: pipe-into-shell, process-substitution exec,
    command substitution recursion

Run:  python3 test_risky.py
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from cubesandbox_risky import (  # noqa: E402
    is_risky,
    is_sentinel,
    load_safe_whitelist,
    parse_sentinel,
)

EMPTY = set()
PASS = 0
FAIL = 0
FAILURES = []


def check(desc: str, command: str, expected_risky: bool, safe: set = EMPTY,
          note: str = ""):
    global PASS, FAIL
    got = is_risky(command, safe)
    ok = got == expected_risky
    if ok:
        PASS += 1
    else:
        FAIL += 1
        FAILURES.append(f"  FAIL {desc}: {command!r} → risky={got}, "
                        f"expected={expected_risky} {note}")


# ---------------------------------------------------------------------------
# Review-bot bypass samples — all must now be RISKY
# ---------------------------------------------------------------------------
print("== review-bot bypass samples (must be RISKY) ==")
check("sudo git reset --hard", "sudo git reset --hard", True)
check("sudo npm install", "sudo npm install", True)
check("env npm install", "env npm install", True)
check("sudo pip install", "sudo pip install", True)
check("sudo yarn add foo", "sudo yarn add foo", True)
check("sudo -u root rm -rf /tmp/x", "sudo -u root rm -rf /tmp/x", True)
check("nice -n 5 npm install", "nice -n 5 npm install", True)
check("timeout 10 git reset --hard", "timeout 10 git reset --hard", True)
check("timeout suffixed duration", "timeout 10s git reset --hard", True)
check("timeout min duration", "timeout 5m npm install", True)
check("timeout hour duration", "timeout 1h rm -rf /tmp/x", True)
check("env FOO=bar npm install", "env FOO=bar npm install", True)
check("DEBUG=1 npm install", "DEBUG=1 npm install", True)
check("FOO=bar rm -rf /tmp/x", "FOO=bar rm -rf /tmp/x", True)
check("quoted redirect target", 'echo hi > "out file.txt"', True)
check("quoted redirect, no space", 'echo hi>"out file.txt"', True)
check("redirect to variable", 'echo hi > "$f"', True)
check("unspaced redirect", "echo hi>file", True)
check("unspaced append", "cmd>>log", True)
check("csh-style &>", "echo hi &>all.log", True)
check("csh-style >&", "echo hi >&all.log", True)
check("fd-prefixed append", "echo hi 2>>log", True)

# ---------------------------------------------------------------------------
# Fail-safe semantics: unparseable → RISKY (never silently safe)
# ---------------------------------------------------------------------------
print("== fail-safe: unparseable → RISKY ==")
check("arithmetic expansion", "echo $((1 > 0))", True,
      note="bashlex cannot parse; fail-safe → risky (by design)")
check("[[ ]] comparison", "[[ 5 > 3 ]]", True,
      note="bashlex cannot parse; fail-safe → risky (by design)")
check("fork bomb", ":(){ :|:& };:", True,
      note="unparseable; fail-safe → risky (by design)")

# ---------------------------------------------------------------------------
# Legacy behavior guards (must stay correct)
# ---------------------------------------------------------------------------
print("== legacy behavior guards ==")
check("plain ls", "ls -la", False)
check("dev null redirect", "echo hi > /dev/null", False)
check("dev stderr redirect", "echo hi 2>/dev/null", False)
check("fd dup 2>&1", "cat file 2>&1", False)
check("fd dup >&2", "echo hi >&2", False)
check("fd dup 1>&2", "echo hi 1>&2", False)
check("fd dup in pipe", "echo hi 2>&1 | tee log", False,
      note="2>&1 safe; tee is not a download/exec combo")
check("rm -rf", "rm -rf /tmp/x", True)
check("git status safe", "git status", False)
check("git commit safe", "git commit -m 'wip'", False)
check("curl -o output", "curl -o out.bin http://x", True)
check("curl -O output", "curl -O http://x", True)
check("curl --output", "curl --output out.bin http://x", True)
check("wget -O output", "wget -O out.bin http://x", True)
check("curl no flag safe", "curl -sI http://x", False)
check("npm install", "npm install", True)
check("git clean -fd", "git clean -fd", True)
check("git -C repo reset --hard", "git -C /tmp/repo reset --hard", True,
      note="subcommand scanned across trailing tokens")
check("quoted subcmd word in commit msg", "git commit -m 'reset clean'", False,
      note="quoted content is one token 'reset clean' — not a subcommand")

# ---------------------------------------------------------------------------
# New structural rules
# ---------------------------------------------------------------------------
print("== new structural rules ==")
check("pipe curl|bash", "curl http://x | bash", True)
check("pipe wget|sh", "wget -qO- http://x | sh", True)
check("pipe curl|python", "curl http://x | python3", True)
check("safe pipe", "ls | grep foo", False)
check("process subst exec", "bash <(curl http://x)", True)
check("cmd subst recursion", "VAR=$(rm -rf /tmp)", True)
check("cmd subst nested sudo", "VAR=$(sudo npm install)", True)
check("subshell", "(rm -rf /tmp/x)", True)
check("assignment plain safe", "DEBUG=1 echo hi", False)
check("env assignment safe", "env FOO=1 echo hi", False)

# ---------------------------------------------------------------------------
# Comment handling: comment-only input is safe; a leading comment/shebang
# followed by a real command must still be classified
# ---------------------------------------------------------------------------
print("== comment handling ==")
check("comment-only", "# just a comment", False)
check("comment then risky cmd", "# install dependencies\nnpm install", True)
check("shebang then risky cmd", "#!/bin/bash\nrm -rf /tmp/important", True)

# ---------------------------------------------------------------------------
# Whitelist behavior
# ---------------------------------------------------------------------------
print("== whitelist behavior ==")
check("whitelist ls", "ls -la", False, safe={"ls", "echo"})
check("whitelist echo devnull", "echo hi > /dev/null", False, safe={"echo"})
check("whitelist echo file", "echo hi > out.txt", True, safe={"echo"},
      note="destructive redirect beats whitelist")
check("whitelist git", "git status", False, safe={"git"})
check("whitelist git reset", "git reset --hard", False, safe={"git"},
      note="whitelist wins over subcommand check (design semantics)")

# ---------------------------------------------------------------------------
# Sentinel + whitelist parsing
# ---------------------------------------------------------------------------
print("== sentinel / whitelist parsing ==")
check("sentinel never risky", "cubesandbox-rollback last", False)
check("sentinel in compound", "cubesandbox-rollback drop snap-1 && ls", False)
check("bare sentinel safe", "cubesandbox-rollback", False)
check("prefix-named tool not sentinel", "cubesandbox-rollback-helper", False,
      note="word boundary required — else a real rollback would run")
check("prefix tool + risky compound", "cubesandbox-rollback-tool.py && rm -rf x", True,
      note="prefix tool is not the sentinel, so rm -rf must be flagged")
assert is_sentinel("cubesandbox-rollback last")
assert is_sentinel("cubesandbox-rollback")
assert not is_sentinel("ls -la")
assert not is_sentinel("cubesandbox-rollback-helper")
assert not is_sentinel("cubesandbox-rollback-tool.py")
assert parse_sentinel("cubesandbox-rollback drop snap-1") == ("drop", "snap-1")
assert parse_sentinel("cubesandbox-rollback") == ("last", None)
assert parse_sentinel('cubesandbox-rollback checkpoint "my milestone"') == ("checkpoint", "my milestone")
assert parse_sentinel("cubesandbox-rollback checkpoint 'single quoted'") == ("checkpoint", "single quoted")
assert parse_sentinel("cubesandbox-rollback last") == ("last", None)
assert load_safe_whitelist.__doc__  # just ensure importable
os.environ["CUBE_ROLLBACK_SAFE"] = "ls, echo, git "
assert load_safe_whitelist() == {"ls", "echo", "git"}
del os.environ["CUBE_ROLLBACK_SAFE"]
PASS += 6

# ---------------------------------------------------------------------------
print(f"\n{'-' * 60}")
print(f"PASS: {PASS}  FAIL: {FAIL}")
if FAILURES:
    print("Failures:")
    print("\n".join(FAILURES))
    sys.exit(1)
print("All tests passed.")
