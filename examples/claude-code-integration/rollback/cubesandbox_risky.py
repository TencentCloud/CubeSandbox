#!/usr/bin/env python3
"""Risky command classifier.  从严 (strict): over-classify, parse compound commands."""

from __future__ import annotations

import os
import re
import sys
from typing import Set, List, Tuple, Optional

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
SENTINEL_PREFIX = "cubesandbox-rollback"

# Prefixes to skip before extracting the real first word
_SKIPPABLE_PREFIXES: tuple = ("sudo", "env", "nohup", "nice")

# Commands whose *presence* makes a compound segment risky
_RISKY_FIRST_WORDS: Set[str] = {
    "rm", "rmdir", "chmod", "chown", "mv", "dd", "shred", "mkfs", "fdisk",
    "apt", "apt-get", "dnf", "yum", "pacman", "brew", "snap",
}

# Commands that are risky only with specific subcommands
_RISKY_WITH_SUB: dict = {
    "git":  {"reset", "clean"},
    "npm":  {"install", "uninstall", "update"},
    "pip":  {"install", "uninstall"},
    "pip3": {"install", "uninstall"},
    "yarn": {"add", "remove"},
    "pnpm": {"add", "remove"},
    "cargo": {"install", "uninstall"},
    "go":   {"install", "get"},
    "make":  {"install"},
}

# Commands dangerous when used with -o / -O / --output
_RISKY_OUTPUT_CMDS: Set[str] = {"curl", "wget"}

# regex for -o / -O / --output flag (short or long, outside quotes)
_OUTPUT_FLAG_RE = re.compile(r"(?:\s|^)(?:-[oO]\b|--output\b)")

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def _strip_quoted(text: str) -> str:
    """Remove single- and double-quoted substrings."""
    return re.sub(r"""('[^']*'|"[^"]*")""", "", text)


def _first_word(segment: str) -> str:
    """Return the first non-empty whitespace-delimited token, lowercased,
    after skipping `sudo`, `env`, `nohup`, `nice` prefixes.  basename
    normalises absolute paths (e.g. `/usr/bin/rm` → `rm`)."""
    seg = segment.strip()
    if not seg:
        return ""
    parts = seg.split(None)
    idx = 0
    # Walk past skippable prefixes
    while idx < len(parts) and parts[idx].lower() in _SKIPPABLE_PREFIXES:
        idx += 1
    if idx >= len(parts):
        return ""
    return os.path.basename(parts[idx].lower())


_REDIR_RE = re.compile(
    r'(?:^|\s)([0-9]{1,2}>>?|&>>?|>&>?|>>?)\s*(\S+)'
)


def _has_destructive_redirect(segment: str) -> bool:
    """True if the segment has a redirect that writes to a real file.

    Safe targets (/dev/null, fd refs like &1/&2, any /dev/* path) are NOT
    destructive — so the ubiquitous `2>/dev/null` / `2>&1` / `>/dev/null`
    never triggers a snapshot.  The operator forms `&>` and the csh-style
    `>&` are parsed as a single operator (not `>` + a `&` target), and
    fd-duplication targets like `2>&1` / `>&1` are treated as safe.
    Only writes to an actual file or directory count.
    """
    for op, target in _REDIR_RE.findall(_strip_quoted(segment)):
        if _is_safe_redirect_target(op, target):
            continue
        return True
    return False


def _is_safe_redirect_target(op: str, target: str) -> bool:
    """True if a redirect targets /dev/null or a file descriptor.

    ``2>&1`` / ``1>&2`` parse as fd-prefixed operator + ``&N`` target;
    the csh-style ``>&1`` parses as ``>&`` operator + ``N`` target.  Both
    duplicate to an existing descriptor and never touch the filesystem.
    """
    if target == '/dev/null' or target.startswith('/dev/'):
        return True
    if re.fullmatch(r'&[0-9]+', target):
        return True  # 2>&1 / 1>&2 / 2>&3 style fd duplication
    if op.startswith('>&') and re.fullmatch(r'[0-9]+', target):
        return True  # csh-style >&1 (dup stdout to fd 1)
    return False


def _has_output_flag(segment: str) -> bool:
    """True if the segment has -o / -O / --output flag (for curl/wget)."""
    cleaned = _strip_quoted(segment)
    return bool(_OUTPUT_FLAG_RE.search(cleaned))


# ---------------------------------------------------------------------------
# Quote-aware compound split
# ---------------------------------------------------------------------------
def _is_inside_quotes(command: str, pos: int) -> bool:
    """Return True if position `pos` in `command` is inside single or double quotes."""
    in_single = in_double = False
    for i, ch in enumerate(command[:pos]):
        if ch == "'" and not in_double:
            in_single = not in_single
        elif ch == '"' and not in_single:
            in_double = not in_double
    # At the boundary, we consider the separator to be outside quotes
    # only if both states are "closed" at the character just before pos.
    return in_single or in_double


def split_compound(command: str) -> List[str]:
    """Split by &&, ||, ;, | that are NOT inside quotes. Return list of segments."""
    sep_re = re.compile(r"(&&|\|\||;|\|)")
    segments: List[str] = []
    last = 0
    for m in sep_re.finditer(command):
        if _is_inside_quotes(command, m.start()):
            continue
        segments.append(command[last:m.start()].strip())
        last = m.end()
    if last < len(command):
        segments.append(command[last:].strip())
    return [s for s in segments if s]


# ---------------------------------------------------------------------------
# Per-segment risk evaluation  (whitelist checked here — 从严: per-segment)
# ---------------------------------------------------------------------------
def _segment_risky(segment: str, safe: Set[str]) -> Optional[str]:
    """Return the matched risky word/subcommand, or None if safe."""
    fw = _first_word(segment)
    if not fw:
        return None

    # Redirect check (从严: any redirect to a real file makes it risky — even
    # for whitelisted commands, since a redirect writes/changes files;
    # /dev/null and fd refs like &1/&2 are harmless and excluded)
    if _has_destructive_redirect(segment):
        return f">{fw} (redirect)"

    # Whitelist applies only to non-redirecting commands
    if fw in safe:
        return None

    # Unconditional risky first-words
    if fw in _RISKY_FIRST_WORDS:
        return fw

    # Risky first-word + subcommand check
    if fw in _RISKY_WITH_SUB:
        # Skip sudo/env prefixes before checking subcommand
        parts = segment.strip().split(None)
        sub_idx = 1
        while sub_idx < len(parts) and parts[sub_idx].lower() in _SKIPPABLE_PREFIXES:
            sub_idx += 1
        if sub_idx < len(parts):
            sub = os.path.basename(parts[sub_idx].lower())
            if sub in _RISKY_WITH_SUB[fw]:
                return f"{fw} {sub}"

    # curl/wget with -o / -O / --output flag
    if fw in _RISKY_OUTPUT_CMDS and _has_output_flag(segment):
        return f"{fw} (output flag)"

    return None


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
    """Return (subcommand, argument) from a sentinel command."""
    parts = command.strip().split(None, 2)
    if len(parts) < 2:
        return ("last", None)
    return (parts[1].lower(), parts[2] if len(parts) > 2 else None)


def is_risky(command: str, safe: Set[str]) -> bool:
    """从严: compound commands checked per-segment. Any risky segment → risky.
    Whitelist is checked *per segment*, not whole-command."""
    cmd = command.strip()
    if not cmd:
        return False
    if is_sentinel(cmd):
        return False  # sentinel itself is never risky

    segments = split_compound(cmd)
    # Always check per-segment — even for single-segment commands
    for seg in segments:
        if not seg:
            continue
        if _segment_risky(seg, safe):
            return True
    # If no compound (i.e. split_compound returned empty because there are no
    # separators), check the whole command as a single segment.
    if not segments:
        return _segment_risky(cmd, safe) is not None
    return False
