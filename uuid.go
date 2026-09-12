package hfs

import (
	"crypto/md5"
	"fmt"
)

// hfsUUIDNamespace is the namespace macOS mixes into the hash when it derives a
// displayable volume UUID from the stored 64-bit identifier. It is Apple's
// kFSUUIDNamespaceSHA1, B3E20F39-F292-11D6-97A4-00306543ECAC. Without it the
// output is a well-formed UUID that matches nothing.
var hfsUUIDNamespace = [16]byte{
	0xB3, 0xE2, 0x0F, 0x39, 0xF2, 0x92, 0x11, 0xD6,
	0x97, 0xA4, 0x00, 0x30, 0x65, 0x43, 0xEC, 0xAC,
}

// VolumeIdentifier is the 64-bit unique identifier a volume carries in the last
// two words of its Finder information.
//
// macOS writes it the first time it mounts a volume that does not have one. Its
// absence therefore means the volume has never been mounted by a system that
// writes them, which is itself worth reporting; its presence survives renaming
// and everything short of rewriting the volume header, so it is the most
// durable handle on "this particular volume" the format offers.
//
// Two different values go by the name UUID here, and they share no digits.
// [VolumeIdentifier.String] is the identifier as stored. [VolumeIdentifier.UUID]
// is the RFC 4122 string macOS displays, which is derived from the stored bytes
// by hashing rather than by reformatting them.
type VolumeIdentifier struct {
	// High is FinderInfo[6] and Low is FinderInfo[7], each as stored.
	High uint32
	Low  uint32
}

// IsZero reports whether the volume carries no identifier.
func (id VolumeIdentifier) IsZero() bool { return id.High == 0 && id.Low == 0 }

// String renders the identifier as the sixteen hexadecimal digits of the two
// words, high word first — the raw form, as stored.
//
// This is not what diskutil prints. See [VolumeIdentifier.UUID].
func (id VolumeIdentifier) String() string {
	return fmt.Sprintf("%08X%08X", id.High, id.Low)
}

// bytes returns the eight identifier bytes in the order macOS hashes them.
//
// Apple's hfs_util (ConvertHFSUUIDToUUID) stores the two words with
// OSSwapHostToBigInt32 before hashing, so the bytes fed to MD5 are big-endian —
// the same order they occupy in the volume header. Isolated here because it is
// the one assumption in the derivation that a real macOS-written volume would
// confirm or refute.
func (id VolumeIdentifier) bytes() [8]byte {
	return [8]byte{
		byte(id.High >> 24), byte(id.High >> 16), byte(id.High >> 8), byte(id.High),
		byte(id.Low >> 24), byte(id.Low >> 16), byte(id.Low >> 8), byte(id.Low),
	}
}

// UUID derives the RFC 4122 string form macOS displays for this volume.
//
// The displayed volume UUID is not the stored identifier rewritten with
// hyphens. macOS hashes the eight stored bytes together with a fixed namespace
// and then forces the version and variant nibbles, so the two values share no
// digits and a tool that prints the raw identifier as a UUID will not match
// diskutil, Spotlight, or any log line that records one.
//
// It returns the empty string when the volume carries no identifier, since
// there is nothing to derive from.
func (id VolumeIdentifier) UUID() string {
	if id.IsZero() {
		return ""
	}

	raw := id.bytes()
	h := md5.New()
	h.Write(hfsUUIDNamespace[:])
	h.Write(raw[:])
	sum := h.Sum(nil)

	sum[6] = 0x30 | (sum[6] & 0x0F)
	sum[8] = 0x80 | (sum[8] & 0x3F)

	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// VolumeIdentifier returns the volume's 64-bit unique identifier.
//
// It reports [ErrNotFound] when both words are zero, which means the volume has
// never been mounted by a system that writes one rather than that the value
// could not be read. Classic HFS keeps the same two words at the same place
// within drFndrInfo, so a classic volume can carry one too.
//
// Safe for concurrent use.
func (v *Volume) VolumeIdentifier() (VolumeIdentifier, error) {
	if v == nil {
		return VolumeIdentifier{}, ErrNotFound
	}
	id := VolumeIdentifier{
		High: v.header.FinderInfo[6],
		Low:  v.header.FinderInfo[7],
	}
	if id.IsZero() {
		return VolumeIdentifier{}, ErrNotFound
	}
	return id, nil
}

// UUID returns the RFC 4122 volume UUID macOS displays, derived from
// [Volume.VolumeIdentifier]. It reports [ErrNotFound] when the volume carries
// no identifier.
//
// Read [VolumeIdentifier.UUID] before relying on the value: it is a hash of the
// stored bytes rather than a reformatting of them, and the stored form is
// available from [Volume.VolumeIdentifier] when an unmediated value is wanted.
//
// Safe for concurrent use.
func (v *Volume) UUID() (string, error) {
	id, err := v.VolumeIdentifier()
	if err != nil {
		return "", err
	}
	return id.UUID(), nil
}
