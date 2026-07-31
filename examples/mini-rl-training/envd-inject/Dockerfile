ARG BASE_IMAGE
FROM ${BASE_IMAGE}

COPY envd /usr/bin/envd
RUN chmod +x /usr/bin/envd

ENTRYPOINT ["/usr/bin/envd", "-isnotfc"]
