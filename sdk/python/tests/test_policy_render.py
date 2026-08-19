from cubesandbox._policy import Inject

CASES = [
    ("", "tok", "tok"),
    ("Bearer ${SECRET}", "tok", "Bearer tok"),
    ("Basic ${SECRET}:${SECRET}", "tok", "Basic tok:${SECRET}"),
    ("${SECRET}-${SECRET}-${SECRET}", "tok", "tok-${SECRET}-${SECRET}"),
    ("Bearer ${SECRET}", "a%2Fb", "Bearer a%2Fb"),
    ("Bearer static", "tok", "Bearer static"),
]


def test_render_matches_server_substitution():
    for fmt, secret, want in CASES:
        got = Inject(header="H", secret=secret, format=(fmt or None)).render()
        assert got == want, f"format={fmt!r} secret={secret!r}: got {got!r}, want {want!r}"
