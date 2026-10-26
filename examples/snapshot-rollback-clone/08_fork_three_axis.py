# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 08: Fork semantics — inheritance and isolation.
#
# After a.clone(n=2) produces b and c:
#   - Inheritance: b and c start with the same filesystem state as a at fork time.
#   - Isolation:   writes in b are not visible in c, and vice versa.
#   - Continuity:  a keeps running after the fork; its state is unaffected.
#   - No leak:     clone() cleans up its internal snapshot automatically.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

a = Sandbox.create(template=TEMPLATE_ID)
print(f"[a] created: {a.sandbox_id}")
a.run_code("open('/tmp/origin.txt','w').write('from a')")

b, c = a.clone(n=2)
print(f"[b] cloned:  {b.sandbox_id}")
print(f"[c] cloned:  {c.sandbox_id}")

# Inheritance: b and c both see the file a wrote before the fork
for sb, name in [(b, "b"), (c, "c")]:
    r = sb.run_code("print(open('/tmp/origin.txt').read())")
    marker = r.logs.stdout[0].strip() if r.logs.stdout else "<empty>"
    print(f"[{name}] origin.txt = {marker!r}")
    assert marker == "from a", f"[{name}] expected 'from a', got {marker!r}"

# Isolation: writes in b are not visible in c
b.run_code("open('/tmp/b_only.txt','w').write('b')")
r = c.run_code("import os; print(os.path.exists('/tmp/b_only.txt'))")
leaked = r.logs.stdout[0].strip() if r.logs.stdout else "<empty>"
print(f"[c] sees b_only.txt: {leaked}  (expect False)")
assert leaked == "False", "isolation violated"

# Continuity: a is still running
r = a.run_code("print(open('/tmp/origin.txt').read())")
still = r.logs.stdout[0].strip() if r.logs.stdout else "<empty>"
print(f"[a] still running, origin.txt = {still!r}")
assert still == "from a"

print("OK: inheritance, isolation and continuity all verified")

# Cleanup
for sb in [a, b, c]:
    sb.kill()
print("all sandboxes killed")
