# syntax=docker/dockerfile:1.7

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG BUNDLER_VERSION=2.5.23

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl ruby ruby-dev \
    && gem install bundler -v "${BUNDLER_VERSION}" --no-document \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/sinatra
COPY Gemfile app.rb ./
RUN bundle config set without development \
    && bundle install --jobs 4 --retry 3 \
    && mkdir -p /workspace/data \
    && chown -R 1000:1000 /workspace

ENV APP_ENV=production \
    BUNDLE_WITHOUT=development \
    PORT=4567 \
    WORKSPACE=/workspace

EXPOSE 4567 49983

# The cubesandbox-base entrypoint keeps envd on :49983 and executes this CMD.
CMD ["bundle", "exec", "ruby", "app.rb", "-o", "0.0.0.0"]
