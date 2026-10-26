#!/bin/bash

curHour=$(date +%Y:%m:%d-%H:%M:%S)
logPath="/data/log/cube-proxy"
maxSize=512000
i=0

for f in $logPath/access.*log $logPath/error.*log; do
    fileSize=$(du -k $f | cut -f1)
    if [ $fileSize -gt $maxSize ]; then
        mv -f $f $f.$curHour
        i=$((i + 1))
    fi
done

[ $i -lt 1 ] && exit 0

kill -USR1 $(cat /usr/local/openresty/nginx/logs/nginx.pid)

for f in $logPath/access*.log.$curHour $logPath/error*.log.$curHour; do
    [ ! -f "$f" ] && continue
    gzip $f &
done

sleep 2

for f in $logPath/access.log $logPath/error.log; do
    files=$(ls $f.* | sort -r -t "." -k 2)
    j=0
    for file in $files; do
        j=$((j + 1))
        if [ $j -gt 3 ]; then
            rm $file
        fi
    done
done
