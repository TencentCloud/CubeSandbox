FROM ubuntu:24.04

RUN apt-get update
RUN DEBIAN_FRONTEND=noninteractive apt-get install -y \
	bash-completion \
	bpftrace \
	busybox \
	curl \
	dnsutils \
	ethtool \
	file \
	gawk \
	iperf \
	iperf3 \
	iproute2 \
	iputils-ping \
	netcat-openbsd \
	netperf \
	net-tools \
	sysstat \
	tcpdump \
	telnet \
	tzdata \
	vim \
	wget \
	skopeo \
	umoci

RUN cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN update-ca-certificates

# Volume plugin host tools (cosfs / coscmd / jq). Injected into the build
# context by build-cube-images.sh from deploy/scripts/docker-install-volume-deps.sh.
COPY docker-install-volume-deps.sh /tmp/docker-install-volume-deps.sh
RUN chmod +x /tmp/docker-install-volume-deps.sh \
	&& /tmp/docker-install-volume-deps.sh \
	&& rm -f /tmp/docker-install-volume-deps.sh

RUN mkdir -p /usr/local/services/cubemaster
RUN mkdir -p /usr/local/services/tools

COPY cubemaster /usr/local/services/cubemaster
RUN chmod +x /usr/local/services/cubemaster/cubemaster


COPY tools/gracestop.sh /usr/local/services/tools/gracestop.sh
RUN chmod +x /usr/local/services/tools/gracestop.sh

WORKDIR /usr/local/services/cubemaster/

ENTRYPOINT ["/usr/local/services/cubemaster/cubemaster"]