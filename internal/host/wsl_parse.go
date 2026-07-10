package host

import (
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

func decodeWindowsText(value []byte) string {
	if len(value) >= 2 && value[0] == 0xff && value[1] == 0xfe {
		value = value[2:]
	}
	if looksUTF16LE(value) {
		units := make([]uint16, 0, len(value)/2)
		for i := 0; i+1 < len(value); i += 2 {
			units = append(units, binary.LittleEndian.Uint16(value[i:i+2]))
		}
		return string(utf16.Decode(units))
	}
	return string(value)
}

func looksUTF16LE(value []byte) bool {
	if len(value) < 2 {
		return false
	}
	zeroes := 0
	pairs := len(value) / 2
	for i := 1; i < len(value); i += 2 {
		if value[i] == 0 {
			zeroes++
		}
	}
	return pairs > 0 && zeroes*2 >= pairs
}

func parseWSLDistributions(output []byte) []string {
	text := decodeWindowsText(output)
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' || r == '\x00' })
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}
