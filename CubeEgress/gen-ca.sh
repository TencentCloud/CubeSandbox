#!/usr/bin/env bash
set -euo pipefail

CURVE="${CURVE:-prime256v1}"
DAYS="${DAYS:-3650}"
SUBJ="${SUBJ:-/C=CN/ST=State/L=City/O=MyOrg/OU=MyUnit/CN=My Root CA}"
KEY_FILE="ca.key"
CRT_FILE="ca.crt"

openssl ecparam -name "$CURVE" -genkey -noout -out "$KEY_FILE"
chmod 600 "$KEY_FILE"

# Minimal config to suppress the system openssl.cnf's default x509v3
# extensions (specifically [v3_ca]'s basicConstraints + subjectKeyIdentifier).
# Without this, OpenSSL 1.1.1 doubles them up with our -addext values and
# produces a certificate that Go's crypto/x509 parser rejects (RFC 5280
# prohibits duplicate extensions).
cat > "${KEY_FILE}.cnf" <<'EOF'
[req]
distinguished_name = req_distinguished_name
prompt = no
[req_distinguished_name]
EOF

openssl req -x509 -new -key "$KEY_FILE" \
    -sha256 -days "$DAYS" \
    -config "${KEY_FILE}.cnf" \
    -subj "$SUBJ" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash" \
    -out "$CRT_FILE"
rm -f "${KEY_FILE}.cnf"

openssl x509 -in "$CRT_FILE" -noout -subject -issuer -dates
