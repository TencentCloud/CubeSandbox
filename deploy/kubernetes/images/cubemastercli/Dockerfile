FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY cubemastercli /usr/local/bin/cubemastercli

RUN chmod +x /usr/local/bin/cubemastercli \
    && { \
        printf '%s\n' \
          '# Cube cubemastercli interactive defaults.' \
          '# This is intentionally a bash interactive helper, not a wrapper binary:' \
          '# - kubectl exec -it ... -- bash can run cubemastercli list directly.' \
          '# - non-interactive commands still execute the original binary unchanged.' \
          '__cube_cubemastercli_has_flag() {' \
          '    local flag="$1"' \
          '    shift' \
          '' \
          '    while [ "$#" -gt 0 ]; do' \
          '        case "$1" in' \
          '            "${flag}"|"${flag}"=*)' \
          '                return 0' \
          '                ;;' \
          '        esac' \
          '        shift' \
          '    done' \
          '' \
          '    return 1' \
          '}' \
          '' \
          'cubemastercli() {' \
          '    local args=()' \
          '' \
          '    if [ -n "${CUBEMASTERCLI_ADDRESS:-}" ] && ! __cube_cubemastercli_has_flag "--address" "$@"; then' \
          '        args+=(--address "${CUBEMASTERCLI_ADDRESS}")' \
          '    fi' \
          '' \
          '    if [ -n "${CUBEMASTERCLI_PORT:-}" ] && ! __cube_cubemastercli_has_flag "--port" "$@"; then' \
          '        args+=(--port "${CUBEMASTERCLI_PORT}")' \
          '    fi' \
          '' \
          '    command /usr/local/bin/cubemastercli "${args[@]}" "$@"' \
          '}'; \
      } >/etc/cubemastercli-interactive.bashrc \
    && printf '\n%s\n' '[ -f /etc/cubemastercli-interactive.bashrc ] && . /etc/cubemastercli-interactive.bashrc' >>/root/.bashrc

CMD ["sleep", "infinity"]
