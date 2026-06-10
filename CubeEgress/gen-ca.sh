#!/usr/bin/env bash
set -euo pipefail

CURVE="${CURVE:-prime256v1}"
DAYS="${DAYS:-3650}"
SUBJ="${SUBJ:-/C=CN/ST=State/L=City/O=MyOrg/OU=MyUnit/CN=My Root CA}"
KEY_FILE="ca.key"
CRT_FILE="ca.crt"

openssl ecparam -name "$CURVE" -genkey -noout -out "$KEY_FILE"
chmod 600 "$KEY_FILE"

openssl req -x509 -new -key "$KEY_FILE" \
    -sha256 -days "$DAYS" \
    -subj "$SUBJ" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash" \
    -out "$CRT_FILE"

openssl x509 -in "$CRT_FILE" -noout -subject -issuer -dates
