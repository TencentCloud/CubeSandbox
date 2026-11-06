#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
"""Set a scalar key in a whitelisted one-click yaml/toml file.

Python 3.8, stdlib only. Tables/mappings must already exist; a missing leaf
key is inserted. First-class CUBE_* fields are skipped with a warning so
CUBE_SET_* cannot bypass CIDR / database / s3lvol install checks.
"""

from __future__ import print_function

import argparse
import math
import os
import re
import stat
import sys
import tempfile


ALLOWLIST = (
    "Cubelet/config/config.toml",
    "Cubelet/dynamicconf/conf.yaml",
    "CubeMaster/conf.yaml",
    "CubeTemplateCenter/conf.yaml",
)

# (relpath, path-segments). First-class install.sh patchers own these fields.
RESERVED = (
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.internal.v1.network", "eth_name")),
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.internal.v1.network", "cidr")),
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.internal.v1.network", "cube_router_enable")),
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.internal.v1.network", "cube_router_cidr")),
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.internal.v1.network", "cube_egress_admin_url")),
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.controller.config.v1.cubelet", "cubeops_addr")),
    ("Cubelet/config/config.toml", ("plugins", "io.cubelet.internal.v1.storage", "cow", "s3", "enable")),
    ("CubeMaster/conf.yaml", ("common", "http_bind")),
    ("CubeMaster/conf.yaml", ("common", "http_port")),
    ("CubeMaster/conf.yaml", ("common", "cube_ops_addr")),
    ("CubeMaster/conf.yaml", ("cubelet_conf", "grpc", "grpc_port")),
    ("Cubelet/config/config.toml", ("grpc", "tcp_address")),
    ("CubeMaster/conf.yaml", ("instance_db_config", "driver")),
    ("CubeMaster/conf.yaml", ("instance_db_config", "addr")),
    ("CubeMaster/conf.yaml", ("instance_db_config", "user")),
    ("CubeMaster/conf.yaml", ("instance_db_config", "pwd")),
    ("CubeMaster/conf.yaml", ("instance_db_config", "db_name")),
    ("CubeMaster/conf.yaml", ("redis", "nodes")),
    ("CubeMaster/conf.yaml", ("redis", "password")),
    ("CubeMaster/conf.yaml", ("redis", "db_no")),
    ("CubeMaster/conf.yaml", ("redis", "master_name")),
    ("CubeMaster/conf.yaml", ("redis", "sentinel_nodes")),
    ("CubeMaster/conf.yaml", ("redis", "sentinel_password")),
    ("CubeTemplateCenter/conf.yaml", ("common", "http_bind")),
    ("CubeTemplateCenter/conf.yaml", ("common", "master_addr")),
    ("Cubelet/dynamicconf/conf.yaml", ("meta_server_config", "meta_server_endpoint")),
    ("Cubelet/dynamicconf/conf.yaml", ("meta_server_config", "cubemaster_http_addr")),
    ("CubeTemplateCenter/conf.yaml", ("instance_db_config", "driver")),
    ("CubeTemplateCenter/conf.yaml", ("instance_db_config", "addr")),
    ("CubeTemplateCenter/conf.yaml", ("instance_db_config", "user")),
    ("CubeTemplateCenter/conf.yaml", ("instance_db_config", "pwd")),
    ("CubeTemplateCenter/conf.yaml", ("instance_db_config", "db_name")),
    ("CubeTemplateCenter/conf.yaml", ("redis", "nodes")),
    ("CubeTemplateCenter/conf.yaml", ("redis", "password")),
    ("CubeTemplateCenter/conf.yaml", ("redis", "db_no")),
    ("CubeTemplateCenter/conf.yaml", ("redis", "master_name")),
    ("CubeTemplateCenter/conf.yaml", ("redis", "sentinel_nodes")),
    ("CubeTemplateCenter/conf.yaml", ("redis", "sentinel_password")),
)

TOML_TABLE_RE = re.compile(r"^(\s*)\[\s*(.+?)\s*\]\s*(?:#.*)?$")
TOML_KEY_RE = re.compile(r"^(\s*)([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$")
YAML_KEY_RE = re.compile(r"^(\s*)([^:#\s][^:]*)\s*:\s*(.*)$")
BLOCK_SCALAR_RE = re.compile(r"^[|>][+-]?(\s.*)?$")


class ConfigSetError(Exception):
    pass


def log(message):
    sys.stderr.write("[one-click] %s\n" % message)


def fail(message):
    raise ConfigSetError(message)


def parse_path(path):
    """Split plugins.\"io.cubelet...images\".image_base_path into segments."""
    if not path:
        fail("CUBE_SET path must not be empty")
    segs = []
    i = 0
    n = len(path)
    while i < n:
        if path[i] == '"':
            j = i + 1
            buf = []
            while j < n:
                if path[j] == "\\" and j + 1 < n:
                    buf.append(path[j + 1])
                    j += 2
                    continue
                if path[j] == '"':
                    break
                buf.append(path[j])
                j += 1
            if j >= n or path[j] != '"':
                fail("unclosed quote in path: %s" % path)
            segs.append("".join(buf))
            i = j + 1
            if i == n:
                break
            if path[i] != ".":
                fail("expected '.' after quoted segment in path: %s" % path)
            i += 1
            continue
        j = i
        while j < n and path[j] != ".":
            if path[j] == '"':
                fail("unexpected quote in path: %s" % path)
            j += 1
        if j == i:
            fail("empty path segment: %s" % path)
        segs.append(path[i:j])
        if j == n:
            break
        i = j + 1
        if i == n:
            fail("trailing '.' in path: %s" % path)
    if not segs:
        fail("CUBE_SET path must not be empty")
    return tuple(segs)


def toml_table_name(segments):
    """Rebuild a TOML table header from parent segments."""
    parts = []
    for seg in segments:
        if "." in seg or not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", seg):
            parts.append('"' + seg.replace("\\", "\\\\").replace('"', '\\"') + '"')
        else:
            parts.append(seg)
    return ".".join(parts)


def infer_kind(value, type_name):
    if "\n" in value or "\r" in value:
        fail("CUBE_SET value must not contain a newline")
    name = (type_name or "auto").lower()
    if name not in ("auto", "string", "int", "bool", "float"):
        fail("CUBE_SET type must be auto|string|int|bool|float (got %s)" % type_name)
    if name == "string":
        return "string", value
    if name == "bool":
        low = value.lower()
        if low not in ("true", "false"):
            fail("CUBE_SET bool value must be true or false (got %s)" % value)
        return "bool", low == "true"
    if name == "int":
        if not re.match(r"^-?[0-9]+$", value):
            fail("CUBE_SET int value is not an integer: %s" % value)
        return "int", int(value)
    if name == "float":
        try:
            parsed = float(value)
        except ValueError:
            fail("CUBE_SET float value is not a number: %s" % value)
        if not math.isfinite(parsed):
            fail("CUBE_SET float value must be finite: %s" % value)
        return "float", parsed
    low = value.lower()
    if low in ("true", "false"):
        return "bool", low == "true"
    if re.match(r"^-?[0-9]+$", value):
        parsed = int(value)
        # 0755 / -0 would not round-trip; keep the operator's digits.
        if str(parsed) != value:
            return "string", value
        return "int", parsed
    if re.match(r"^-?[0-9]+\.[0-9]+$", value):
        parsed = float(value)
        if str(parsed) != value and repr(parsed) != value:
            return "string", value
        return "float", parsed
    return "string", value


def format_toml_value(kind, parsed):
    if kind == "bool":
        return "true" if parsed else "false"
    if kind == "int":
        return str(parsed)
    if kind == "float":
        rendered = repr(parsed)
        if "." not in rendered and "e" not in rendered.lower():
            rendered += ".0"
        return rendered
    escaped = (
        parsed.replace("\\", "\\\\")
        .replace('"', '\\"')
        .replace("\n", "\\n")
        .replace("\t", "\\t")
    )
    return '"%s"' % escaped


def yaml_quote_string(value):
    escaped = (
        value.replace("\\", "\\\\")
        .replace('"', '\\"')
        .replace("\n", "\\n")
        .replace("\t", "\\t")
        .replace("\r", "\\r")
    )
    return '"%s"' % escaped


def format_yaml_value(kind, parsed):
    if kind == "string":
        return yaml_quote_string(parsed)
    if kind == "bool":
        return "true" if parsed else "false"
    if kind == "float":
        rendered = repr(parsed)
        if "." not in rendered and "e" not in rendered.lower():
            rendered += ".0"
        return rendered
    if kind == "int":
        return str(parsed)
    return yaml_quote_string(str(parsed))


def normalize_relpath(relpath):
    if not relpath or relpath.startswith("/") or relpath.startswith("\\"):
        fail("CUBE_SET file must be a relative allowlisted path (got %s)" % relpath)
    parts = []
    for part in relpath.replace("\\", "/").split("/"):
        if part in ("", ".", ".."):
            fail("CUBE_SET file must not contain '.' or '..': %s" % relpath)
        parts.append(part)
    return "/".join(parts)


def resolve_target(root, relpath):
    rel = normalize_relpath(relpath)
    if rel not in ALLOWLIST:
        fail("CUBE_SET file is not allowlisted: %s" % rel)
    root = os.path.abspath(root)
    target = os.path.abspath(os.path.join(root, rel))
    if not (target == root or target.startswith(root + os.sep)):
        fail("CUBE_SET file escapes install root: %s" % rel)
    if os.path.islink(target):
        fail("refusing to patch a symlink target: %s -> %s" % (target, os.readlink(target)))
    if not os.path.isfile(target):
        fail("required file not found: %s" % target)
    return rel, target


def atomic_write(path, text):
    mode = stat.S_IMODE(os.stat(path).st_mode)
    directory = os.path.dirname(path)
    fd, tmp = tempfile.mkstemp(prefix=os.path.basename(path) + ".", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(text)
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
        tmp = None
    finally:
        if tmp is not None and os.path.exists(tmp):
            os.unlink(tmp)


def toml_header_dotted(header):
    try:
        return ".".join(parse_path(header))
    except ConfigSetError:
        return header.replace('"', "")


def iter_toml_tables(lines):
    for i, line in enumerate(lines):
        header, is_arr = parse_toml_header(line)
        if header is None or is_arr:
            continue
        if not header or header[0] in "\"'[{0123456789-":
            continue
        yield i, header, toml_header_dotted(header)


def find_toml_table(lines, table):
    matches = []
    for i, raw, _dotted in iter_toml_tables(lines):
        if raw == table:
            matches.append(i)
    if not matches:
        fail("TOML table [%s] not found" % table)
    if len(matches) != 1:
        fail("expected exactly one live [%s] (got %s)" % (table, len(matches)))
    return matches[0]


def resolve_toml_table_and_key(lines, segments):
    """Match a table even after bash `source` stripped embedded quotes.

    plugins."io.cubelet...images".image_base_path and the sourced form
    plugins.io.cubelet...images.image_base_path both resolve by longest
    unquoted table-header prefix.
    """
    dotted = ".".join(segments)
    exact = toml_table_name(segments[:-1]) if len(segments) >= 2 else ""
    try:
        if exact:
            return find_toml_table(lines, exact), segments[-1]
    except ConfigSetError:
        pass
    best = None
    for idx, raw, header_dotted in iter_toml_tables(lines):
        prefix = header_dotted + "."
        if not dotted.startswith(prefix):
            continue
        rest = dotted[len(prefix) :]
        if not rest or "." in rest or not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", rest):
            continue
        if best is None or len(header_dotted) > len(best[0]):
            best = (header_dotted, idx, rest, raw)
    if best is None:
        fail("TOML table for path %s not found" % dotted)
    raw = best[3]
    matches = [i for i, header, _d in iter_toml_tables(lines) if header == raw]
    if len(matches) != 1:
        fail("expected exactly one live [%s] (got %s)" % (raw, len(matches)))
    return best[1], best[2]


def is_toml_table_header_line(line):
    header, is_arr = parse_toml_header(line)
    if header is None:
        return False
    if is_arr:
        return True
    # Value arrays look like ["x"] / [1] / []; table names start with ident.
    if not header or header[0] in "\"'[{0123456789-":
        return False
    return True


def table_body_end(lines, header_idx):
    i = header_idx + 1
    while i < len(lines):
        if is_toml_table_header_line(lines[i]):
            return i
        i += 1
    return len(lines)


def looks_quoted_scalar(rest):
    s = (rest or "").strip()
    return s.startswith('"') or s.startswith("'")


def render_existing_or_auto(kind, parsed, raw_value, type_name, rest, formatter):
    if (type_name or "auto").lower() == "auto" and looks_quoted_scalar(rest):
        return formatter("string", raw_value)
    return formatter(kind, parsed)


def apply_toml(lines, segments, kind, parsed, raw_value, type_name):
    if len(segments) < 2:
        fail("TOML path must be table.key (got %s)" % ".".join(segments))
    header_idx, key = resolve_toml_table_and_key(lines, segments)
    if not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", key):
        fail("TOML leaf key must be a bare identifier: %s" % key)
    end = table_body_end(lines, header_idx)
    key_indent = None
    found = None
    for i in range(header_idx + 1, end):
        stripped = lines[i].lstrip()
        if not stripped or stripped.startswith("#"):
            continue
        m = TOML_KEY_RE.match(lines[i])
        if not m:
            continue
        key_indent = m.group(1)
        if m.group(2) == key:
            found = i
    if found is not None:
        rest = TOML_KEY_RE.match(lines[found]).group(3).strip()
        if rest.startswith('"""') or rest.startswith("'''") or rest.endswith("\\"):
            fail("refusing to replace a multi-line TOML value: %s" % key)
        if rest.startswith("[") or rest.startswith("{"):
            fail("refusing to replace a TOML array/inline table: %s" % key)
        indent = TOML_KEY_RE.match(lines[found]).group(1)
        rendered = render_existing_or_auto(
            kind, parsed, raw_value, type_name, rest, format_toml_value
        )
        lines[found] = "%s%s = %s\n" % (indent, key, rendered)
        return lines, "updated"
    parent_header, _is_arr = parse_toml_header(lines[header_idx])
    if parent_header and toml_child_table_exists(lines, parent_header, key):
        fail("refusing to shadow TOML table/array-of-tables %s" % key)
    insert_at = end
    while insert_at > header_idx + 1 and lines[insert_at - 1].strip() == "":
        insert_at -= 1
    if key_indent is None:
        header_indent = re.match(r"^(\s*)", lines[header_idx]).group(1)
        key_indent = header_indent + "    "
    rendered = format_toml_value(kind, parsed)
    lines.insert(insert_at, "%s%s = %s\n" % (key_indent, key, rendered))
    return lines, "inserted"


def parse_toml_header(line):
    # Accept a trailing `# comment` after [table] / [[aot]].
    stripped = line.strip()
    if not stripped.startswith("["):
        return None, False
    if stripped.startswith("[["):
        close = stripped.find("]]")
        if close < 2:
            return None, False
        rest = stripped[close + 2 :]
        if rest.strip() and not rest.strip().startswith("#"):
            return None, False
        return stripped[2:close].strip(), True
    close = stripped.find("]")
    if close < 1:
        return None, False
    rest = stripped[close + 1 :]
    if rest.strip() and not rest.strip().startswith("#"):
        return None, False
    return stripped[1:close].strip(), False


def toml_child_table_exists(lines, parent_header, key):
    # Match [parent.key] and implicit ancestors: [parent.key.child] defines
    # parent.key as a table even when that header itself is absent.
    wanted = toml_header_dotted(parent_header) + "." + key
    for line in lines:
        header, _is_arr = parse_toml_header(line)
        if header is None:
            continue
        dotted = toml_header_dotted(header)
        if dotted == wanted or dotted.startswith(wanted + "."):
            return True
    return False


def yaml_is_seq_item(line):
    stripped = line.lstrip(" ")
    return stripped.startswith("- ") or stripped == "-"


def yaml_body_kind(lines, start, parent_indent):
    """Return 'mapping', 'sequence', or None when the parent body is empty."""
    i = start
    while i < len(lines):
        if is_yaml_ignorable(lines[i]):
            i += 1
            continue
        indent = yaml_indent(lines[i])
        if indent <= parent_indent:
            return None
        if yaml_is_seq_item(lines[i]):
            return "sequence"
        if yaml_entry(lines[i]) is not None:
            return "mapping"
        i += 1
    return None


def yaml_has_nested_body(lines, found_i, found_indent):
    i = found_i + 1
    while i < len(lines):
        if is_yaml_ignorable(lines[i]):
            i += 1
            continue
        return yaml_indent(lines[i]) > found_indent
    return False


def yaml_indent(line):
    return len(line) - len(line.lstrip(" "))


def is_yaml_ignorable(line):
    stripped = line.strip()
    return not stripped or stripped.startswith("#")


def yaml_entry(line):
    if is_yaml_ignorable(line):
        return None
    m = YAML_KEY_RE.match(line.rstrip("\n"))
    if not m:
        return None
    return yaml_indent(line), m.group(2).strip(), m.group(3)


def find_yaml_child(lines, start, parent_indent, name):
    i = start
    first_child_indent = None
    while i < len(lines):
        if is_yaml_ignorable(lines[i]):
            i += 1
            continue
        entry = yaml_entry(lines[i])
        if entry is None:
            indent = yaml_indent(lines[i])
            if indent <= parent_indent:
                break
            i += 1
            continue
        indent, key, rest = entry
        if indent <= parent_indent:
            break
        if first_child_indent is None:
            first_child_indent = indent
        if indent != first_child_indent:
            i += 1
            continue
        if key == name:
            return i, indent, rest, first_child_indent
        i += 1
    return None, first_child_indent, None, first_child_indent


def mapping_insert_index(lines, start, parent_indent, child_indent):
    i = start
    last_content = start
    while i < len(lines):
        if is_yaml_ignorable(lines[i]):
            i += 1
            continue
        indent = yaml_indent(lines[i])
        if indent <= parent_indent:
            break
        last_content = i + 1
        i += 1
    return last_content, child_indent if child_indent is not None else parent_indent + 2


def apply_yaml(lines, segments, kind, parsed, raw_value, type_name):
    if not segments:
        fail("YAML path must not be empty")
    for name in segments:
        if not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", name):
            fail("YAML path segment must be a bare identifier: %s" % name)
    start = 0
    parent_indent = -1
    child_indent = 0
    for depth, name in enumerate(segments[:-1]):
        found_i, found_indent, rest, child_indent = find_yaml_child(
            lines, start, parent_indent, name
        )
        if found_i is None:
            fail("YAML parent %r not found" % ".".join(segments[: depth + 1]))
        rest_s = rest.strip() if rest is not None else ""
        if rest_s.startswith("#"):
            rest_s = ""
        if rest_s:
            # Only `key:` / `key: # comment` is a mapping parent. A colon
            # inside a scalar (addr: "127.0.0.1:3306") or a flow collection
            # is not a place to insert a block child.
            if BLOCK_SCALAR_RE.match(rest_s):
                fail("YAML parent %r is a block scalar, not a mapping" % name)
            fail("YAML parent %r is a scalar, not a mapping" % name)
        start = found_i + 1
        parent_indent = found_indent
        if yaml_body_kind(lines, start, parent_indent) == "sequence":
            fail("YAML parent %r is a sequence, not a mapping" % name)
    leaf = segments[-1]
    found_i, found_indent, rest, child_indent = find_yaml_child(
        lines, start, parent_indent, leaf
    )
    if found_i is not None:
        rest_s = rest.strip() if rest is not None else ""
        if rest_s.startswith("#"):
            rest_s = ""
        if BLOCK_SCALAR_RE.match(rest_s):
            fail("refusing to replace YAML block scalar %r" % leaf)
        if rest_s.startswith("[") or rest_s.startswith("{"):
            fail("refusing to replace a YAML flow collection %r" % leaf)
        if yaml_has_nested_body(lines, found_i, found_indent):
            fail("refusing to replace YAML mapping/sequence %r with a scalar" % leaf)
        nxt = found_i + 1
        while nxt < len(lines) and is_yaml_ignorable(lines[nxt]):
            nxt += 1
        if (
            nxt < len(lines)
            and yaml_is_seq_item(lines[nxt])
            and yaml_indent(lines[nxt]) == found_indent
        ):
            fail("refusing to replace YAML mapping/sequence %r with a scalar" % leaf)
        indent = " " * found_indent
        rendered = render_existing_or_auto(
            kind, parsed, raw_value, type_name, rest_s, format_yaml_value
        )
        lines[found_i] = "%s%s: %s\n" % (indent, leaf, rendered)
        return lines, "updated"
    if yaml_body_kind(lines, start, parent_indent) == "sequence":
        parent_name = segments[-2] if len(segments) > 1 else leaf
        fail("YAML parent %r is a sequence, not a mapping" % parent_name)
    insert_at, use_indent = mapping_insert_index(lines, start, parent_indent, child_indent)
    if child_indent is None and len(segments) > 1:
        # empty mapping: insert as first child
        use_indent = parent_indent + 2
        insert_at = start
    rendered = format_yaml_value(kind, parsed)
    lines.insert(insert_at, "%s%s: %s\n" % (" " * use_indent, leaf, rendered))
    return lines, "inserted"


def is_reserved(rel, segments):
    dotted = ".".join(segments)
    for reserved_rel, reserved_segs in RESERVED:
        if reserved_rel == rel and ".".join(reserved_segs) == dotted:
            return True
    return False


def apply_set(root, relpath, path, value, type_name, dry_run=False):
    rel, target = resolve_target(root, relpath)
    segments = parse_path(path)
    if is_reserved(rel, segments):
        log(
            "WARNING: CUBE_SET skipped %s %s (owned by a first-class CUBE_* key)"
            % (rel, path)
        )
        return "skipped"
    kind, parsed = infer_kind(value, type_name)
    with open(target, "r", encoding="utf-8") as fh:
        text = fh.read()
    newline = "\n"
    if "\r\n" in text:
        newline = "\r\n"
        text = text.replace("\r\n", "\n")
    if text and not text.endswith("\n"):
        text += "\n"
    lines = [line + "\n" for line in text.splitlines()]
    if rel.endswith(".toml"):
        lines, action = apply_toml(lines, segments, kind, parsed, value, type_name)
    elif rel.endswith(".yaml"):
        lines, action = apply_yaml(lines, segments, kind, parsed, value, type_name)
    else:
        fail("unsupported config suffix: %s" % rel)
    out = "".join(lines)
    if newline != "\n":
        out = out.replace("\n", newline)
    if dry_run:
        log("CUBE_SET dry-run %s %s (%s)" % (action, rel, path))
        return action
    atomic_write(target, out)
    log("CUBE_SET %s %s (%s)" % (action, rel, path))
    return action


def main(argv):
    parser = argparse.ArgumentParser(description="Apply one CUBE_SET_* override")
    parser.add_argument("--root", required=True, help="install / package root")
    parser.add_argument("--file", required=True, help="allowlisted relative path")
    parser.add_argument("--path", required=True, help="dotted key path")
    parser.add_argument("--value", required=True, help="scalar value")
    parser.add_argument(
        "--type",
        default="auto",
        dest="type_name",
        help="auto|string|int|bool|float",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="validate and compute the write without touching the file",
    )
    args = parser.parse_args(argv)
    try:
        action = apply_set(
            args.root,
            args.file,
            args.path,
            args.value,
            args.type_name,
            dry_run=args.dry_run,
        )
    except ConfigSetError as exc:
        log("ERROR: %s" % exc)
        return 1
    sys.stdout.write("%s\n" % action)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
