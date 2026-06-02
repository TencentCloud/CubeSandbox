"""CubeSandbox examples runner.

A declarative CLI to discover, run, and assert the example scripts shipped in
the ``examples/`` directory. Each example declares its execution steps and
expected outcomes in a ``cube-example.yaml`` manifest; this runner executes
those steps against a running CubeSandbox deployment and produces JSON +
Markdown reports.
"""

__version__ = "0.1.0"
