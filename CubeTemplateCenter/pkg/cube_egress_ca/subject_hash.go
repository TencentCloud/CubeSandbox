// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube_egress_ca

import (
	"crypto/md5"
	"crypto/x509"
	"encoding/binary"
)

// opensslSubjectHashOld reproduces `openssl x509 -subject_hash_old`, the
// file-name convention Android's trust stores require: system CAs live
// under the cacerts directories as "<hash>.<n>", where <hash> is the
// OpenSSL X509_NAME_hash_old of the certificate subject — MD5 over the
// raw DER encoding of the subject, first four bytes little-endian.
//
// Android reads this hash, NOT the SHA1-based X509_NAME_hash
// (`openssl x509 -subject_hash`, the convention Debian-style
// /etc/ssl/certs symlinks use): conscrypt's TrustedCertificateStore.hash()
// calls NativeCrypto.X509_NAME_hash_old, and
// android.security.net.config.DirectoryCertificateSource keys its
// directory scan the same way. Verified against a stock Android 14
// (redroid 14.0.0_r75) image: all 134 preinstalled system CA files are
// named by subject_hash_old, none by subject_hash. Writing the CubeEgress
// root there under any other name is silently ignored by Android's TLS
// stack, so the Android anchor copies must use this name.
//
// There is deliberately no canonicalization here: X509_NAME_hash_old
// hashes the name exactly as encoded in the certificate (OpenSSL's
// cached DER), unlike X509_NAME_hash which first lowercases and
// re-encodes the string attributes. The MD5 here only feeds the legacy
// file-name convention inherited from OpenSSL; it is not used for any
// security purpose.
func opensslSubjectHashOld(cert *x509.Certificate) uint32 {
	sum := md5.Sum(cert.RawSubject) // #nosec G401 -- legacy file-name convention, not a security primitive
	return binary.LittleEndian.Uint32(sum[:4])
}
