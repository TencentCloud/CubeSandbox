#!/usr/bin/env python3
"""Risky command classifier.

Parses the whole command with bashlex — a Python port of bash's own parser —
and evaluates risk from the AST structure (assignments, redirects, wrapper
prefixes, subshells, pipelines).  Anything the parser cannot handle is
treated as risky (fail-safe).
"""

from __future__ import annotations

import os
import re
import sys
from typing import List, Optional, Set, Tuple

try:
    import bashlex
except ImportError:  # pragma: no cover - deployment error path
    bashlex = None  # type: ignore[assignment]

SENTINEL_PREFIX = "cubesandbox-rollback"

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# Wrapper commands: skipped to find the real command word
_WRAPPER_CMDS: frozenset = frozenset({
    "sudo", "env", "nohup", "nice", "timeout", "command", "exec",
    "setsid", "doas", "su",
})

# Wrapper options that consume a following value (else the value would be
# mistaken for the command word).  Unknown options are skipped bare — a
# heuristic; see README "Known limitations".
_WRAPPER_OPT_WITH_VALUE: dict = {
    "sudo":   {"-u", "-g", "-p", "-C", "-h", "--user", "--group", "--prompt"},
    "env":    {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"},
    "nice":   {"-n", "--adjustment"},
    "timeout": {"-s", "-k", "-f", "--signal", "--kill-after"},
}

# Commands whose *presence* makes a command risky
_RISKY_FIRST_WORDS: Set[str] = {
    "rm", "rmdir", "chmod", "chown", "mv", "dd", "shred", "mkfs", "fdisk",
    "apt", "apt-get", "dnf", "yum", "pacman", "brew", "snap",
}

# Commands that are risky only with specific subcommands.
# against every trailing token (a `git log reset` style false positive is a
# harmless extra snapshot; a missed `git -C repo reset --hard` is not).
_RISKY_WITH_SUB: dict = {
    "git":  {"reset", "clean"},
    "npm":  {"install", "uninstall", "update"},
    "pip":  {"install", "uninstall"},
    "pip3": {"install", "uninstall"},
    "yarn": {"add", "remove"},
    "pnpm": {"add", "remove"},
    "cargo": {"install", "uninstall"},
    "go":   {"install", "get"},
    "make": {"install"},
}

# Commands dangerous when used with -o / -O / --output
_RISKY_OUTPUT_CMDS: Set[str] = {"curl", "wget"}

# curl|bash style: download on the left, interpreter on the right
_DOWNLOAD_CMDS: Set[str] = {"curl", "wget"}
_EXEC_CMDS: Set[str] = {
    "bash", "sh", "zsh", "dash", "ksh", "fish",
    "python", "python3", "python2", "perl", "php", "ruby", "node", "lua",
}

# Redirect operators that write to a file (vs `<`, `<<` which only read)
_WRITE_REDIR_TYPES: frozenset = frozenset({">", ">>", ">&", "&>", "&>>", ">|", "<>"})

_ASSIGN_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
_NUM_RE = re.compile(r"^[0-9]+$")
_FD_REF_RE = re.compile(r"^&[0-9]+$")
_OUTPUT_FLAG_RE = re.compile(r"^(?:-[oO]\b|--output(?:-document)?\b)")

# ---------------------------------------------------------------------------
# AST helpers
# ---------------------------------------------------------------------------
def _norm_command_word(w: str) -> str:
    """Basename + strip backslash escapes + lowercase."""
    return os.path.basename(w.lstrip("\\")).lower()


def _unwrap_index(words: List[str]) -> int:
    """Index of the real command word.

    Skips wrapper prefixes (``sudo`` / ``env`` / ``nohup`` / ``nice`` /
    ``timeout`` …) along with their options and option values, then skips
    leading ``VAR=value`` tokens (``env FOO=bar npm install``).
    """
    i = 0
    n = len(words)
    while i < n and words[i] in _WRAPPER_CMDS:
        wrapper = words[i]
        i += 1
        while i < n:
            w = words[i]
            if w.startswith("-"):
                i += 1
                if i < n and w in _WRAPPER_OPT_WITH_VALUE.get(wrapper, ()):
                    i += 1  # option value
                continue
            if wrapper in ("timeout", "nice") and _NUM_RE.match(w):
                i += 1  # bare duration / priority
                continue
            break
    while i < n and _ASSIGN_RE.match(words[i]):
        i += 1  # env FOO=bar cmd
    return i


def _command_parts(cmd_node) -> Tuple[List[str], List]:
    """Return (words, redirects) from a bashlex command node."""
    words: List[str] = []
    redirects: List = []
    for part in cmd_node.parts:
        if part.kind == "word":
            words.append(part.word)
        elif part.kind == "redirect":
            redirects.append(part)
    return words, redirects


def _redirect_destructive(rd) -> bool:
    """True if a redirect writes to a real file.

    Safe targets: /dev/* paths and fd references (``2>&1`` / ``>&1`` /
    ``>&2``).  Anything else — including quoted targets and unparseable
    opaque words — counts as destructive (fail-safe).
    """
    if rd.type not in _WRITE_REDIR_TYPES:
        return False  # <, << heredoc, etc. only read
    out = rd.output
    if isinstance(out, int):
        return False  # 2>&1 → fd 1
    if isinstance(out, str):
        if out.isdigit():
            return False  # >&2 parsed as string fd
        target = out
    else:
        target = getattr(out, "word", None)
    if target is None:
        return True  # opaque target → destructive (fail-safe)
    if target.startswith("/dev/"):
        return False
    if _FD_REF_RE.match(target):
        return False
    return True


def _find_risky_subcommand(fw: str, words: List[str], start: int) -> Optional[str]:
    """Return the risky subcommand token if any trailing token matches."""
    risky_subs = _RISKY_WITH_SUB.get(fw, frozenset())
    for w in words[start:]:
        n = _norm_command_word(w)
        if n in risky_subs:
            return n
    return None


def _has_output_flag(words: List[str]) -> bool:
    return any(_OUTPUT_FLAG_RE.match(w) for w in words)


_SINGLE_CHILD_ATTRS = ("command", "list", "body")


def _children(node):
    """Yield child AST nodes of any bashlex node.

    bashlex uses a ``parts`` list for compound nodes (list/pipeline/command),
    singular ``command`` / ``body`` attributes for substitutions and function
    bodies, and a *list-valued* ``list`` attribute on ``compound`` nodes
    (subshells/braces).  Normalise all shapes so callers can walk the tree
    uniformly.  Children are deduplicated by identity (a function body may be
    reachable both via ``parts`` and via ``body``).
    """
    seen: set = set()
    for child in getattr(node, "parts", None) or []:
        if id(child) not in seen:
            seen.add(id(child))
            yield child
    for attr in _SINGLE_CHILD_ATTRS:
        v = getattr(node, attr, None)
        if v is None:
            continue
        if isinstance(v, list):
            for child in v:
                if hasattr(child, "kind") and id(child) not in seen:
                    seen.add(id(child))
                    yield child
        elif hasattr(v, "kind") and id(v) not in seen:
            seen.add(id(v))
            yield v


def _has_self_pipe(node) -> bool:
    """True if the subtree contains a pipeline whose ends run the same
    command — the fork-bomb signature `:(){ :|:& };:` pipes `:` into `:`.
    A normal `ls | grep` pipe has different names and is not flagged."""
    if node.kind == "pipeline":
        cmds = [p for p in node.parts if getattr(p, "kind", None) == "command"]
        if len(cmds) >= 2:
            names = []
            for c in cmds:
                words, _ = _command_parts(c)
                i = _unwrap_index(words)
                names.append(_norm_command_word(words[i]) if i < len(words) else "")
            if names and names[0] and names[0] == names[-1]:
                return True
    return any(_has_self_pipe(c) for c in _children(node))


def _command_in_subst(node) -> Optional[str]:
    """First command name inside a process/command substitution."""
    for n in _children(node):
        if n.kind == "command":
            words, _ = _command_parts(n)
            i = _unwrap_index(words)
            if i < len(words):
                return _norm_command_word(words[i])
        elif n.kind in ("list", "pipeline", "commandsubstitution",
                        "processsubstitution"):
            r = _command_in_subst(n)
            if r:
                return r
    return None


# ---------------------------------------------------------------------------
# Per-node evaluation
# ---------------------------------------------------------------------------
def _check_command_node(cmd_node, safe: Set[str], out: List[str]) -> None:
    words, redirects = _command_parts(cmd_node)

    # Recurse into nested substitutions / subshells FIRST, and always —
    # even when the outer command word is missing or whitelisted
    # (`VAR=$(rm -rf /tmp)` has no command word at all; the risky command
    # lives inside the assignment).
    for part in cmd_node.parts:
        if part.kind in ("word", "assignment"):
            _check_tree(list(_children(part)), safe, out)
        elif part.kind in ("commandsubstitution", "processsubstitution",
                           "subshell", "function", "heredoc"):
            _check_tree([part], safe, out)

    # Destructive redirect (checked before the whitelist — a redirect
    # writes/changes files even for whitelisted commands)
    for rd in redirects:
        if _redirect_destructive(rd):
            target = getattr(rd.output, "word", rd.output)
            out.append(f"redirect > {target!r}")
            return

    start = _unwrap_index(words)
    if start >= len(words):
        return
    fw = _norm_command_word(words[start])
    if not fw:
        return

    # Whitelist applies only to non-redirecting commands
    if fw in safe:
        return

    if fw in _RISKY_FIRST_WORDS:
        out.append(fw)
        return
    if fw in _RISKY_WITH_SUB:
        sub = _find_risky_subcommand(fw, words, start + 1)
        if sub:
            out.append(f"{fw} {sub}")
            return
    if fw in _RISKY_OUTPUT_CMDS and _has_output_flag(words[start + 1:]):
        out.append(f"{fw} (output flag)")
        return

    # exec <(download): `bash <(curl …)` style — interpreter consumes a
    # download process substitution
    if fw in _EXEC_CMDS:
        for part in cmd_node.parts:
            if part.kind == "word":
                for sub in _children(part):
                    if sub.kind == "processsubstitution":
                        inner = _command_in_subst(sub)
                        if inner in _DOWNLOAD_CMDS:
                            out.append(f"exec <(download): {fw} <({inner} …)")
                            return


def _pipeline_risky(pipe_node) -> Optional[str]:
    """curl|bash style: download command piped into an interpreter."""
    cmds = [p for p in pipe_node.parts if getattr(p, "kind", None) == "command"]
    if len(cmds) < 2:
        return None

    def _name_of(c) -> Optional[str]:
        words, _ = _command_parts(c)
        i = _unwrap_index(words)
        return _norm_command_word(words[i]) if i < len(words) else None

    first = _name_of(cmds[0])
    last = _name_of(cmds[-1])
    if first in _DOWNLOAD_CMDS and last in _EXEC_CMDS:
        return f"pipe: {first} | {last}"
    return None


def _check_tree(nodes, safe: Set[str], out: List[str]) -> None:
    for node in nodes:
        k = node.kind
        if k == "command":
            _check_command_node(node, safe, out)
        elif k == "pipeline":
            r = _pipeline_risky(node)
            if r:
                out.append(r)
            for p in node.parts:
                if getattr(p, "kind", None) == "command":
                    _check_tree([p], safe, out)
        elif k == "list":
            _check_tree(list(_children(node)), safe, out)
        elif k in ("subshell", "compound", "commandsubstitution",
                   "processsubstitution", "function", "heredoc",
                   "word", "assignment"):
            if k == "function" and _has_self_pipe(node):
                # Fork-bomb signature: `:(){ :|:& };:` — a function whose
                # body pipes its own name.
                out.append("fork bomb (function body pipeline)")
            _check_tree(list(_children(node)), safe, out)
        # operator / pipe / parameter / comment / redirect: nothing to check


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------
def load_safe_whitelist() -> Set[str]:
    """Parse CUBE_ROLLBACK_SAFE env var (comma-separated) into a set."""
    raw = os.environ.get("CUBE_ROLLBACK_SAFE", "")
    if not raw.strip():
        return set()
    return {w.strip().lower() for w in raw.split(",") if w.strip()}


def is_sentinel(command: str) -> bool:
    """True if the command starts with `cubesandbox-rollback`."""
    return command.strip().startswith(SENTINEL_PREFIX)


def parse_sentinel(command: str) -> Tuple[str, Optional[str]]:
    """Return (subcommand, argument) from a sentinel command.

    Quoted arguments survive parsing (``checkpoint "my milestone"`` keeps
    the spaces); unparseable input (e.g. unterminated quote) falls back to
    a plain whitespace split rather than crashing.
    """
    parts = command.strip().split(None, 2)
    if len(parts) < 2:
        return ("last", None)
    try:
        import shlex
        shlexed = shlex.split(command)
    except ValueError:
        shlexed = parts
    if len(shlexed) < 2:
        return ("last", None)
    return (shlexed[1].lower(),
            shlexed[2] if len(shlexed) > 2 else None)


def is_risky(command: str, safe: Set[str]) -> bool:
    """
    Fail-safe: commands bashlex cannot parse (arithmetic expansion, `[[ ]]`,
    fork bombs, …) are treated as risky rather than silently safe.
    """
    cmd = command.strip()
    if not cmd:
        return False
    if is_sentinel(cmd):
        return False  # sentinel itself is never risky
    if cmd.startswith("#"):
        return False  # pure comment

    if bashlex is None:
        # Deployment error: classifier unavailable → over-approximate.
        print("cubesandbox-risky: bashlex not installed; treating command as "
              "risky (fail-safe). Install requirements: "
              "`pip install -r requirements.txt`", file=sys.stderr)
        return True

    try:
        ast = bashlex.parse(cmd)
    except Exception:
        return True  # unparseable → risky (fail-safe)

    reasons: List[str] = []
    _check_tree(ast, safe, reasons)
    return bool(reasons)
