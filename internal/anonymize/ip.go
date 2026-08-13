package anonymize

import "net"

// ipv4PrivatePrefix is the leading octet of the RFC 1918 /8 block anonymized
// IPv4 addresses are placed in. 10.0.0.0/8 rather than 192.168.0.0/16 or
// 172.16.0.0/12 purely because it's the largest of the three private blocks,
// giving the most room to spread digest-derived addresses across.
const ipv4PrivatePrefix = 10

// ipv6ULAPrefix is the leading byte of the RFC 4193 Unique Local Address
// range (fd00::/8) anonymized IPv6 addresses are placed in.
const ipv6ULAPrefix = 0xfd

// aliasIP returns a deterministic, structurally valid replacement IP for
// original, derived from digest. The replacement is always in a private
// range (RFC 1918 for IPv4, RFC 4193 ULA for IPv6) and matches original's
// address family whenever original parses as an IP.
//
// original is used only to pick the address family; the replacement's actual
// bytes come entirely from digest, so this never leaks any bit of the real
// address. If original does not parse as an IP at all — which should not
// happen in practice, since the caller's matcher is what decided this value
// belongs to CategoryIP in the first place — this falls back to treating it
// as IPv4 rather than panicking or erroring, since Aliaser.Alias has no
// error return. That fallback is intentional and covered by a test, not an
// oversight: see alias_test.go's malformed-input case.
func aliasIP(digest []byte, original string) string {
	parsed := net.ParseIP(original)
	if parsed != nil && parsed.To4() == nil {
		return aliasIPv6(digest)
	}
	return aliasIPv4(digest)
}

// aliasIPv4 builds a 10.x.x.x address from digest. The last octet is
// clamped to [1,254] so the result never looks like a network or broadcast
// address for a /24 subnet, even though nothing here actually depends on a
// /24 mask being in play.
func aliasIPv4(digest []byte) string {
	b1, b2, b3 := digest[0], digest[1], digest[2]
	last := 1 + (b3 % 254)
	ip := net.IPv4(ipv4PrivatePrefix, b1, b2, last)
	return ip.String()
}

// aliasIPv6 builds an fd00::/8 ULA address from digest. SHA-256 digests are
// 32 bytes, so there is no risk of running out of digest material for the
// remaining 15 bytes after the fixed leading byte.
func aliasIPv6(digest []byte) string {
	var b [16]byte
	b[0] = ipv6ULAPrefix
	copy(b[1:], digest[:15])
	return net.IP(b[:]).String()
}
