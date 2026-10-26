#!/usr/bin/env python3
"""Resolve Cargo.lock conflicts by keeping incoming (theirs) version for each block."""

with open('Cargo.lock', 'r') as f:
    lines = f.read().split('\n')

result = []
i = 0
while i < len(lines):
    if lines[i].startswith('<<<<<<< '):
        i += 1
        # Skip HEAD lines
        while i < len(lines) and not lines[i].startswith('======='):
            i += 1
        i += 1  # skip =======
        # Keep incoming lines
        while i < len(lines) and not lines[i].startswith('>>>>>>>'):
            result.append(lines[i])
            i += 1
        i += 1  # skip >>>>>>> marker
    else:
        result.append(lines[i])
        i += 1

with open('Cargo.lock', 'w') as f:
    f.write('\n'.join(result))

print('Cargo.lock conflicts resolved')
