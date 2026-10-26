#!/bin/bash

# Get parameters
filepath=$1
codedir=$2

cmd=unsquashfs

# check option is "-direct"
if [ "$3" == "-direct" ]; then
    cmd=unsquashfs-dio
fi

# Run unsquashfs
$cmd -p 8 -d $codedir $filepath
if [ $? -ne 0 ]; then
    echo "$cmd failed" >&2
    exit 1
fi

# Record the tool version for debugging
# it will be deleted during code gc.
$cmd -v > $codedir.tool-version
if [ $? -ne 1 ]; then
    echo "$cmd write version failed" >&2
    exit 1
fi

# Remove the filepath
rm -f $filepath
