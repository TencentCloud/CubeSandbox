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

RUN mkdir -p /usr/local/services/cubemaster
RUN mkdir -p /usr/local/services/tools

COPY cubemaster /usr/local/services/cubemaster
RUN chmod +x /usr/local/services/cubemaster/cubemaster


COPY tools/gracestop.sh /usr/local/services/tools/gracestop.sh
RUN chmod +x /usr/local/services/tools/gracestop.sh

WORKDIR /usr/local/services/cubemaster/

ENTRYPOINT ["/usr/local/services/cubemaster/cubemaster"]