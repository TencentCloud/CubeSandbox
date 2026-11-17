#!/usr/bin/env python3
"""Measure JuiceFS snapshot startup and pause/resume across Cube nodes.

Run this script on CUBE_CONTROL_NODE. It uses the local control-node database
and cubeopscli; it does not connect to compute nodes over SSH.
"""

from cubebench_jfs_local import main


if __name__ == "__main__":
    raise SystemExit(main("cross"))
