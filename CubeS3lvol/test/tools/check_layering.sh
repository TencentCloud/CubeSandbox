#!/usr/bin/env bash
#
#  check_layering.sh -- enforce the three layering rules from HANDOFF section 8.
#
#  The rules, and what each one buys:
#
#    1. lib/s3bsdev/ must not include anything from module/.
#       The core library has to be unit-testable on its own, which is what
#       test/integration/ relies on: every test there links libs3bsdev.a and
#       nothing else. One include of a vbdev header would drag in the bdev
#       layer, SPDK's nvmf stack, and the whole module -- and the way that
#       failure arrives is a link error in a test that has nothing to do with
#       the change.
#
#    2. Only s3_client_aws.c may include <aws/...>.
#       aws-c-s3 types must not escape into the rest of the tree, so that
#       replacing the SDK is one file's worth of work. This is the rule most
#       likely to be broken by accident: needing one aws_byte_cursor somewhere
#       else looks harmless in isolation.
#
#    3. module/ must not name aws_* identifiers at all.
#       All S3 access goes through include/s3lvol/s3_client.h. Rule 2 stops the
#       include; this one also stops a forward declaration or an extern.
#
#  === Comments are stripped before matching ===
#
#  Not a detail: this tree's comments discuss aws-c-s3 at length, naming
#  aws_s3_meta_request and friends, and several explain what *not* to include.
#  Grepping the raw text reports those as violations, and a check that cries wolf
#  gets ignored or deleted -- so the manual greps in HANDOFF section 8 needed a
#  human to sort real from imagined.
#
#  gcc -fpreprocessed -dD -E -P removes comments while leaving #include
#  directives alone and expanding nothing, which is exactly the input wanted
#  here. It also means a violation cannot hide inside a comment-like construct.
#
#  Exit status: 0 all three hold, 1 something is broken, 2 the check itself could
#  not run (no compiler). The last is deliberately distinct: "cannot check" must
#  never look like "checked and fine".
#

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
cd "${ROOT}" || exit 2

CC_BIN="${CC:-cc}"
command -v "${CC_BIN}" >/dev/null 2>&1 || {
	echo "check_layering: no ${CC_BIN} to strip comments with; not checking" >&2
	exit 2
}

FAILED=0

pass() { printf '  [OK]   %s\n' "$1"; }
fail()
{
	printf '  [FAIL] %s\n' "$1"
	FAILED=1
}

# Comments out, #include and code in.
strip_comments()
{
	"${CC_BIN}" -fpreprocessed -dD -E -P "$1" 2>/dev/null
}

# Every source file under the given directories.
sources()
{
	find "$@" -type f \( -name '*.c' -o -name '*.h' \) 2>/dev/null | sort
}

echo "=== layering rules (HANDOFF section 8) ==="

# --------------------------------------------------------------------------
# 1. lib/s3bsdev/ must not include module/
# --------------------------------------------------------------------------
HITS=""
for f in $(sources lib); do
	out="$(strip_comments "${f}" |
		grep -n '^[[:space:]]*#[[:space:]]*include.*module/' || true)"
	[ -n "${out}" ] && HITS="${HITS}${f}: ${out}"$'\n'
done
if [ -z "${HITS}" ]; then
	pass "lib/ includes nothing from module/"
else
	fail "lib/ includes a module/ header, so it no longer builds alone:"
	printf '%s' "${HITS}" | sed 's/^/         /'
fi

# --------------------------------------------------------------------------
# 2. Only s3_client_aws.c may include <aws/...>
#
# Both bracket forms are matched: "aws/..." would work just as well as <aws/...>
# with the right -I, so checking only the angle form leaves a way round the rule.
# --------------------------------------------------------------------------
ALLOWED="lib/s3bsdev/s3_client_aws.c"
HITS=""
for f in $(sources lib module include app test); do
	[ "${f#./}" = "${ALLOWED}" ] && continue
	if strip_comments "${f}" |
	   grep -q '^[[:space:]]*#[[:space:]]*include[[:space:]]*[<"]aws/'; then
		HITS="${HITS}${f}"$'\n'
	fi
done
if [ -z "${HITS}" ]; then
	pass "only ${ALLOWED} includes <aws/...>"
else
	fail "aws-c-s3 headers reached beyond ${ALLOWED}:"
	printf '%s' "${HITS}" | sed 's/^/         /'
fi

# The rule has two halves, and only checking one of them would let the file it
# names stop being the S3 client without anything noticing.
if [ -f "${ALLOWED}" ] && strip_comments "${ALLOWED}" |
   grep -q '^[[:space:]]*#[[:space:]]*include[[:space:]]*[<"]aws/'; then
	pass "and it still does (the rule has something to be about)"
else
	fail "${ALLOWED} includes no aws header -- either it moved, or rule 2 is \
now vacuously true"
fi

# --------------------------------------------------------------------------
# 3. module/ must not name aws_* at all
# --------------------------------------------------------------------------
HITS=""
for f in $(sources module); do
	out="$(strip_comments "${f}" | grep -n '\baws_[A-Za-z0-9_]*' || true)"
	[ -n "${out}" ] && HITS="${HITS}${f}: ${out}"$'\n'
done
if [ -z "${HITS}" ]; then
	pass "module/ names no aws_* identifier"
else
	fail "module/ reaches aws-c-s3 directly instead of via s3_client.h:"
	printf '%s' "${HITS}" | sed 's/^/         /'
fi

echo ""
if [ "${FAILED}" -eq 0 ]; then
	echo "all three layering rules hold"
else
	echo "layering is broken; see above" >&2
fi
exit "${FAILED}"
