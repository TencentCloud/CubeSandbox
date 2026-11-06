# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

ARG GRAFANA_IMAGE=grafana/grafana-oss:10.4.18
FROM ${GRAFANA_IMAGE}

USER root
COPY --chown=472:0 grafana/dashboards/ /usr/share/cubesandbox/dashboards/
RUN test -s /usr/share/cubesandbox/dashboards/cubesandbox-host-sandbox.json \
    && test -s /usr/share/cubesandbox/dashboards/cubesandbox-logs.json \
    && test -s /usr/share/cubesandbox/dashboards/cubesandbox-logs-elasticsearch.json

USER 472
