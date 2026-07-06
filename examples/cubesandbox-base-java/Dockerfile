# syntax=docker/dockerfile:1.7
#
# Demo: a Cube-ready sandbox image that runs a tiny Java HTTP server on :8080
# while the cubesandbox-base entrypoint keeps envd running on :49983 in the
# background. A good starting point for Java workloads in CubeSandbox.
#
# Build:
#   docker build -t cubesandbox-demo-java:latest examples/cubesandbox-base-java
#
# Run locally:
#   docker run --rm -d -p 8080:8080 -p 49983:49983 \
#     --name cube-demo-java cubesandbox-demo-java:latest
#   curl http://127.0.0.1:8080/
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

# Java 17 LTS (headless) + Maven for the most common Java toolchain.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        openjdk-17-jdk-headless \
        maven \
    && rm -rf /var/lib/apt/lists/*

ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64 \
    PATH="${JAVA_HOME}/bin:${PATH}"

WORKDIR /app
COPY HelloWorldServer.java /app/HelloWorldServer.java
COPY HelloWorldServerTest.java /app/HelloWorldServerTest.java

# Compile both the server and its test, then run the test suite.
RUN javac HelloWorldServer.java HelloWorldServerTest.java
RUN java HelloWorldServerTest

EXPOSE 8080 49983

# cube-entrypoint.sh (inherited from the base image) will:
#   1. Launch envd on ${ENVD_PORT:-49983} in the background.
#   2. exec the CMD below as the foreground process, so the Java server owns
#      PID 1's child and receives SIGTERM on `docker stop`.
CMD ["java", "HelloWorldServer"]
