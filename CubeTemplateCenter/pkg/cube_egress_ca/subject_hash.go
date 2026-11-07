// SPDX-License-Identifier: Apache-2.0
//

package cube_egress_ca

import (
	"crypto/sha1"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// opensslSubjectHash reproduces `openssl x509 -subject_hash`, the file-name
// convention Android's conscrypt trust store requires: system CAs live under
// system/etc/security/cacerts as "<hash>.<n>", where <hash> is the OpenSSL
// X509_NAME_hash of the certificate subject. Writing the CubeEgress root
// there under any other name is silently ignored by Android's TLS stack, so
// the Android anchor copy must use this name.
//
// The algorithm (openssl crypto/x509/x509_cmp.c X509_NAME_hash_ex over the
// canonical encoding built by crypto/x509/x_name.c x509_name_canon):
//
//  1. Re-encode the subject name canonically:
//     - each RDN is DER-encoded as SET OF AttributeTypeAndValue and the SET
//     encodings are concatenated *without* the outer SEQUENCE header;
//     - values of the string types (UTF8String, BMPString, UniversalString,
//     PrintableString, T61String, IA5String, VisibleString) are converted
//     to UTF-8 and canonicalized: leading/trailing spaces stripped, runs
//     of whitespace collapsed to a single space, ASCII letters lowercased,
//     and re-encoded as UTF8String;
//     - values of other types are copied with their original tag;
//     - RDN order is preserved.
//  2. SHA-1 over that encoding; the hash is the first four bytes,
//     little-endian.
func opensslSubjectHash(cert *x509.Certificate) (uint32, error) {
	canonical, err := canonicalNameDER(cert.RawSubject)
	if err != nil {
		return 0, fmt.Errorf("cube_egress_ca: canonicalize subject: %w", err)
	}
	sum := sha1.Sum(canonical)
	return binary.LittleEndian.Uint32(sum[:4]), nil
}

// canonicalNameDER builds the OpenSSL canonical encoding of a DER-encoded
// X509 Name (see opensslSubjectHash for the exact rules).
func canonicalNameDER(subjectDER []byte) ([]byte, error) {
	var name asn1.RawValue
	rest, err := asn1.Unmarshal(subjectDER, &name)
	if err != nil {
		return nil, fmt.Errorf("subject is not valid DER: %w", err)
	}
	if len(rest) != 0 || name.Class != asn1.ClassUniversal || name.Tag != asn1.TagSequence {
		return nil, errors.New("subject is not a single DER SEQUENCE")
	}

	var out []byte
	rdnBytes := name.Bytes
	for len(rdnBytes) > 0 {
		var rdn asn1.RawValue
		rdnBytes, err = asn1.Unmarshal(rdnBytes, &rdn)
		if err != nil {
			return nil, fmt.Errorf("subject RDN is not valid DER: %w", err)
		}
		if rdn.Class != asn1.ClassUniversal || rdn.Tag != asn1.TagSet {
			return nil, errors.New("subject RDN is not a SET")
		}
		atvBytes := rdn.Bytes
		var setMembers []byte
		for len(atvBytes) > 0 {
			var atv asn1.RawValue
			atvBytes, err = asn1.Unmarshal(atvBytes, &atv)
			if err != nil {
				return nil, fmt.Errorf("subject AttributeTypeAndValue is not valid DER: %w", err)
			}
			if atv.Class != asn1.ClassUniversal || atv.Tag != asn1.TagSequence {
				return nil, errors.New("subject AttributeTypeAndValue is not a SEQUENCE")
			}
			encoded, err := canonicalATV(atv.FullBytes)
			if err != nil {
				return nil, err
			}
			setMembers = append(setMembers, encoded...)
		}
		out = append(out, derTLV(asn1.ClassUniversal, asn1.TagSet, setMembers)...)
	}
	return out, nil
}

// canonicalATV re-encodes one AttributeTypeAndValue {OID, value} from its
// raw TLV bytes, canonicalizing the value when it is one of the string types
// OpenSSL canonicalizes (see opensslSubjectHash).
func canonicalATV(raw []byte) ([]byte, error) {
	var atv asn1.RawValue
	if _, err := asn1.Unmarshal(raw, &atv); err != nil {
		return nil, err
	}
	members := atv.Bytes
	var oid asn1.ObjectIdentifier
	afterOID, err := asn1.Unmarshal(members, &oid)
	if err != nil {
		return nil, fmt.Errorf("subject AttributeTypeAndValue type is not an OID: %w", err)
	}
	oidTLV := members[:len(members)-len(afterOID)]
	var value asn1.RawValue
	afterValue, err := asn1.Unmarshal(afterOID, &value)
	if err != nil {
		return nil, fmt.Errorf("subject AttributeTypeAndValue value is not valid DER: %w", err)
	}
	if len(afterValue) != 0 {
		return nil, errors.New("subject AttributeTypeAndValue has trailing data")
	}

	valueTLV := value.FullBytes
	if isCanonicalizableString(value.Class, value.Tag) {
		utf8Bytes, err := stringToUTF8(value.Tag, value.Bytes)
		if err != nil {
			return nil, err
		}
		valueTLV = derTLV(asn1.ClassUniversal, asn1.TagUTF8String, canonicalStringBytes(utf8Bytes))
	}
	body := append(append([]byte{}, oidTLV...), valueTLV...)
	return derTLV(asn1.ClassUniversal, asn1.TagSequence, body), nil
}

// isCanonicalizableString reports whether OpenSSL's asn1_string_canon
// canonicalizes the given ASN.1 tag.
func isCanonicalizableString(class int, tag int) bool {
	if class != asn1.ClassUniversal {
		return false
	}
	switch tag {
	case asn1.TagUTF8String, 30 /* BMPString */, 28, /* UniversalString */
		asn1.TagPrintableString, 20 /* T61String */, asn1.TagIA5String, 26 /* VisibleString */ :
		return true
	}
	return false
}

// stringToUTF8 converts the raw content bytes of an ASN.1 string value to
// UTF-8, mirroring OpenSSL's ASN1_STRING_to_UTF8 for the canonicalizable
// types. ASCII-compatible encodings (UTF8, Printable, IA5, Visible, T61 as
// Latin-1) pass through or expand per byte; BMP is UTF-16BE and Universal is
// UTF-32.
func stringToUTF8(tag int, raw []byte) ([]byte, error) {
	switch tag {
	case 30: // BMPString: UTF-16 big endian
		if len(raw)%2 != 0 {
			return nil, errors.New("cube_egress_ca: BMPString has odd length")
		}
		u16 := make([]uint16, len(raw)/2)
		for i := range u16 {
			u16[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
		}
		return []byte(string(utf16.Decode(u16))), nil
	case 28: // UniversalString: UTF-32 big endian
		if len(raw)%4 != 0 {
			return nil, errors.New("cube_egress_ca: UniversalString length not a multiple of 4")
		}
		out := make([]byte, 0, len(raw))
		for i := 0; i < len(raw); i += 4 {
			r := rune(uint32(raw[i])<<24 | uint32(raw[i+1])<<16 | uint32(raw[i+2])<<8 | uint32(raw[i+3]))
			out = utf8.AppendRune(out, r)
		}
		return out, nil
	default:
		// UTF8String passes through; Printable/IA5/Visible are ASCII
		// subsets and T61 is treated as Latin-1, so a byte-wise widening is
		// byte-identical to OpenSSL's conversion for valid inputs.
		return raw, nil
	}
}

// canonicalStringBytes applies OpenSSL's in-place canonicalization rules:
// strip leading and trailing whitespace, collapse runs of whitespace to a
// single space, and lowercase ASCII letters. Non-ASCII bytes are copied
// unchanged.
func canonicalStringBytes(in []byte) []byte {
	isSpace := func(b byte) bool {
		switch b {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			return true
		}
		return false
	}
	start, end := 0, len(in)
	for start < end && isSpace(in[start]) {
		start++
	}
	for end > start && isSpace(in[end-1]) {
		end--
	}
	out := make([]byte, 0, end-start)
	for i := start; i < end; i++ {
		b := in[i]
		if b < 0x80 && isSpace(b) {
			out = append(out, ' ')
			for i+1 < end && in[i+1] < 0x80 && isSpace(in[i+1]) {
				i++
			}
			continue
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		out = append(out, b)
	}
	return out
}

// derTLV encodes one DER TLV with a universal-class tag and the given
// content. Lengths use the short form when they fit, the long form
// otherwise.
func derTLV(class int, tag int, content []byte) []byte {
	var identifier byte
	if class == asn1.ClassUniversal {
		identifier = byte(tag)
	} else {
		// Not needed for the canonical name encoding (all wrappers are
		// universal), but kept for completeness.
		identifier = byte(0x80 | (int(class) << 6) | tag)
	}
	// SET and SEQUENCE are constructed types.
	if tag == asn1.TagSet || tag == asn1.TagSequence {
		identifier |= 0x20
	}
	out := []byte{identifier}
	n := len(content)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	case n < 0x10000:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x83, byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, content...)
}
