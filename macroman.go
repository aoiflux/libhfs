package hfs

// TextEncoding selects how classic HFS filename bytes are decoded.
//
// Classic HFS stores names as raw bytes in whatever Mac script encoding the
// volume was written with; unlike HFS+ it records no per-name encoding hint, so
// the choice belongs to the caller.
type TextEncoding uint8

const (
	// TextEncodingMacRoman decodes names as Mac OS Roman, the encoding used by
	// Western-localised systems and by the overwhelming majority of surviving
	// HFS media. This is the default.
	TextEncodingMacRoman TextEncoding = iota

	// TextEncodingRaw widens each byte to the code point of the same value,
	// preserving the original bytes at the cost of rendering anything above
	// 0x7F as the wrong character. Use it when byte fidelity matters more than
	// legibility, or when the volume uses a script encoding this package does
	// not model.
	TextEncodingRaw
)

func (e TextEncoding) String() string {
	switch e {
	case TextEncodingMacRoman:
		return "MacRoman"
	case TextEncodingRaw:
		return "raw"
	default:
		return "unknown"
	}
}

// SetTextEncoding selects the encoding used to decode classic HFS names. It has
// no effect on HFS+ or HFSX volumes, which store names as UTF-16 and need no
// such choice.
//
// Safe for concurrent use, though changing it while other goroutines are
// reading means they may decode with either setting.
func (v *Volume) SetTextEncoding(e TextEncoding) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.textEncoding = e
}

// TextEncoding reports the encoding used to decode classic HFS names.
func (v *Volume) TextEncoding() TextEncoding {
	if v == nil {
		return TextEncodingMacRoman
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.textEncoding
}

// decodeHFSName converts classic HFS name bytes to a Go string.
func (v *Volume) decodeHFSName(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if v != nil && v.TextEncoding() == TextEncodingRaw {
		return decodeRawBytes(b)
	}
	return decodeMacRoman(b)
}

// decodeRawBytes widens each byte to the code point of the same value. This is
// what the package did before MacRoman support existed; it is wrong for any
// byte above 0x7F but never fails.
func decodeRawBytes(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// decodeMacRoman converts Mac OS Roman bytes to a Go string. Bytes below 0x80
// are ASCII; the rest come from the table below.
func decodeMacRoman(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		if c < 0x80 {
			r[i] = rune(c)
			continue
		}
		r[i] = macRomanHigh[c-0x80]
	}
	return string(r)
}

// macRomanHigh maps Mac OS Roman bytes 0x80–0xFF to Unicode.
//
// Two entries are worth knowing about. 0xDB was the generic currency sign
// (U+00A4) in the original encoding and became the Euro sign (U+20AC) in
// Mac OS 8.5; the Euro mapping is used here because that is what Apple's own
// text-encoding converter produces, but a name written on an older system will
// render differently than it did then. 0xF0 is the Apple logo, which has no
// Unicode assignment and lives in the private-use area at U+F8FF, so it renders
// as the Apple logo only on Apple platforms.
var macRomanHigh = [128]rune{
	/* 0x80 */ 0x00C4, 0x00C5, 0x00C7, 0x00C9, 0x00D1, 0x00D6, 0x00DC, 0x00E1,
	/* 0x88 */ 0x00E0, 0x00E2, 0x00E4, 0x00E3, 0x00E5, 0x00E7, 0x00E9, 0x00E8,
	/* 0x90 */ 0x00EA, 0x00EB, 0x00ED, 0x00EC, 0x00EE, 0x00EF, 0x00F1, 0x00F3,
	/* 0x98 */ 0x00F2, 0x00F4, 0x00F6, 0x00F5, 0x00FA, 0x00F9, 0x00FB, 0x00FC,
	/* 0xA0 */ 0x2020, 0x00B0, 0x00A2, 0x00A3, 0x00A7, 0x2022, 0x00B6, 0x00DF,
	/* 0xA8 */ 0x00AE, 0x00A9, 0x2122, 0x00B4, 0x00A8, 0x2260, 0x00C6, 0x00D8,
	/* 0xB0 */ 0x221E, 0x00B1, 0x2264, 0x2265, 0x00A5, 0x00B5, 0x2202, 0x2211,
	/* 0xB8 */ 0x220F, 0x03C0, 0x222B, 0x00AA, 0x00BA, 0x03A9, 0x00E6, 0x00F8,
	/* 0xC0 */ 0x00BF, 0x00A1, 0x00AC, 0x221A, 0x0192, 0x2248, 0x2206, 0x00AB,
	/* 0xC8 */ 0x00BB, 0x2026, 0x00A0, 0x00C0, 0x00C3, 0x00D5, 0x0152, 0x0153,
	/* 0xD0 */ 0x2013, 0x2014, 0x201C, 0x201D, 0x2018, 0x2019, 0x00F7, 0x25CA,
	/* 0xD8 */ 0x00FF, 0x0178, 0x2044, 0x20AC, 0x2039, 0x203A, 0xFB01, 0xFB02,
	/* 0xE0 */ 0x2021, 0x00B7, 0x201A, 0x201E, 0x2030, 0x00C2, 0x00CA, 0x00C1,
	/* 0xE8 */ 0x00CB, 0x00C8, 0x00CD, 0x00CE, 0x00CF, 0x00CC, 0x00D3, 0x00D4,
	/* 0xF0 */ 0xF8FF, 0x00D2, 0x00DA, 0x00DB, 0x00D9, 0x0131, 0x02C6, 0x02DC,
	/* 0xF8 */ 0x00AF, 0x02D8, 0x02D9, 0x02DA, 0x00B8, 0x02DD, 0x02DB, 0x02C7,
}
