#!/usr/bin/ls
podman run  -v $(pwd):/home -w /home cube:build make
