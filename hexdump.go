package main

import (
	"fmt"
	"strings"
)

// hexDump renders data in the style of `xxd -g 1`: one line per 16 bytes with a
// byte offset, the bytes in hex, and an ASCII gutter. base is the offset of the
// first byte, so a dump taken after `xxd -s N` (or `-s`) keeps real addresses.
func hexDump(data []byte, base int64) string {
	const perLine = 16
	var sb strings.Builder
	for len(data) > 0 {
		n := perLine
		if len(data) < n {
			n = len(data)
		}
		line := data[:n]

		fmt.Fprintf(&sb, "%08x:", base)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, " %02x", line[i])
		}
		for i := n; i < perLine; i++ {
			sb.WriteString("   ")
		}
		sb.WriteString("  ")
		for i := 0; i < n; i++ {
			sb.WriteByte(printable(line[i]))
		}

		data = data[n:]
		base += int64(n)
		if len(data) > 0 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func printable(b byte) byte {
	if b >= 0x20 && b <= 0x7e {
		return b
	}
	return '.'
}
