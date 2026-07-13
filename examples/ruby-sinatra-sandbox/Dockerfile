# syntax=docker/dockerfile:1.7

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE} AS builder

ARG DEBIAN_FRONTEND=noninteractive
ARG BUNDLER_VERSION=2.5.23

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential ca-certificates ruby ruby-dev \
    && gem install bundler -v "${BUNDLER_VERSION}" --no-document \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/sinatra
COPY Gemfile ./
RUN bundle config set without development \
    && bundle config set path /opt/bundle \
    && bundle install --jobs 4 --retry 3 \
    && rm -rf /opt/bundle/ruby/*/cache

FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG BUNDLER_VERSION=2.5.23

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ruby \
    && gem install bundler -v "${BUNDLER_VERSION}" --no-document \
    && apt-get purge -y curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/sinatra
COPY --from=builder --chown=1000:1000 /opt/bundle /opt/bundle
COPY --from=builder --chown=1000:1000 /opt/sinatra/Gemfile.lock ./Gemfile.lock
COPY --chown=1000:1000 Gemfile app.rb ./

RUN mkdir -p /workspace/data \
    && chown -R 1000:1000 /workspace /opt/sinatra /opt/bundle

ENV APP_ENV=production \
    BUNDLE_PATH=/opt/bundle \
    BUNDLE_WITHOUT=development \
    PORT=4567 \
    WORKSPACE=/workspace

EXPOSE 4567 49983

# The base entrypoint keeps envd privileged for sandbox management. Only the
# application process drops to the base image's unprivileged UID 1000 user.
CMD ["runuser", "--user", "user", "--", "bundle", "exec", "ruby", "app.rb", "-o", "0.0.0.0"]
