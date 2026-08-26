#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Stdlib gaps that SPDK's unmodified rpc.py assumes.
#
#  Ubuntu 20.04 ships Python 3.8. SPDK v26.09-pre rpc.py uses
#  argparse.BooleanOptionalAction, which landed in 3.9. Without this backfill
#  rcow_wait_rpc treats every probe as "target not answering" and the unit
#  crash-loops. 3.9+ is a no-op: only missing attributes are filled in.
#
#  Semantics match CPython 3.9: --foo sets True, --no-foo sets False.

import argparse


def _boolean_optional_action():
    class BooleanOptionalAction(argparse.Action):
        def __init__(self,
                     option_strings,
                     dest,
                     default=None,
                     type=None,
                     choices=None,
                     required=False,
                     help=None,
                     metavar=None):
            strings = []
            for option_string in option_strings:
                strings.append(option_string)
                if option_string.startswith('--'):
                    strings.append('--no-' + option_string[2:])
            super().__init__(
                option_strings=strings,
                dest=dest,
                nargs=0,
                default=default,
                type=type,
                choices=choices,
                required=required,
                help=help,
                metavar=metavar)

        def __call__(self, parser, namespace, values, option_string=None):
            setattr(namespace, self.dest,
                    not str(option_string).startswith('--no-'))

        def format_usage(self):
            return ' | '.join(self.option_strings)

    return BooleanOptionalAction


def apply():
    if not hasattr(argparse, 'BooleanOptionalAction'):
        argparse.BooleanOptionalAction = _boolean_optional_action()


apply()
