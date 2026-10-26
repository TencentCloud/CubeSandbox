#!/bin/sh

# Check if the user is running this script as root
if [ "$(id -u)" != "0" ]; then
    echo "This script must be run as root"
    exit 1
fi

STATIC_LIBSECCOMP_DIR=/usr/local/lib64/libseccomp
TMP_COMPILE_DIR=/tmp/libseccomp
LIBSECCOMP_VERSION=2.5.1
LIBSECCOMP_URL=https://github.com/seccomp/libseccomp/archive/refs/tags/v${LIBSECCOMP_VERSION}.tar.gz
PACKAGE_MANAGER="yum"


detect_package_manager() {
		
	if [ "Darwin" == "$(uname -s)" ]; then
		show_error_message "Darwin" || exit 1
	else
		# Here we will use package managers to determine which operating system the user is using.

		# Debian or any derivative of it
		if hash 2>/dev/null apt-get; then
			PACKAGE_MANAGER="apt-get"   || exit 1
		# Fedora
		elif hash 2>/dev/null dnf; then
			PACKAGE_MANAGER="dnf"
		# CentOS and its derivatives
		elif hash 2>/dev/null yum; then
			PACKAGE_MANAGER="yum"
		# Unsupported platform
		else
			printf "\e[31;1mFatal error: \e[0;31mUnsupported platform, please open an issue\[0m" || exit 1
		fi
	fi
}


detect_package_manager || exit 1

if [ ! -d ${STATIC_LIBSECCOMP_DIR} ]; then
	pushd .
	rm -rf ${TMP_COMPILE_DIR}
	mkdir -p ${TMP_COMPILE_DIR}
	cd ${TMP_COMPILE_DIR}
	wget -c ${LIBSECCOMP_URL}
	tar zxvf v${LIBSECCOMP_VERSION}.tar.gz
	cd libseccomp-${LIBSECCOMP_VERSION}
	${PACKAGE_MANAGER} -y install gperf libtool || exit 1
	sh ./autogen.sh 
	./configure CFLAGS="-U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=1 -O2" --enable-shared --enable-static --prefix=${STATIC_LIBSECCOMP_DIR}

	make -j $(nproc)
	mkdir -p ${STATIC_LIBSECCOMP_DIR}
	make install
	popd
	rm -rf ${TMP_COMPILE_DIR}
fi

echo "Bootstrap complete!"
