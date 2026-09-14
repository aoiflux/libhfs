package libhfs

import (
	"fmt"
	"time"
)

// FileIdentity is a value that identifies a file across two readings of the
// same volume.
//
// A CNID alone will not do it. HFS allocates catalog node IDs from
// VolumeHeader.NextCatalogID and reuses them once it wraps, so the same number
// can name a file in one reading and a different file, created later, in the
// next. Pairing the CNID with the creation date, which nothing in normal use
// rewrites, makes an accidental collision require both a reused number and a
// matching birth second.
//
// This is evidence of sameness, not proof of it. Both halves come from the
// volume, and anything that can write the volume can write both; a match says
// the two records agree, which is as much as any on-disk identifier can say.
//
// Three things commonly mislead:
//
//   - It compares two readings of one volume, not two volumes. When Source is
//     [TimeSourceHFSLocal] the date is an un-anchored wall-clock reading, so
//     comparing it against a date from another machine's volume is meaningless
//     even though Equal will agree when both sides say so.
//   - Hard links collapse. [Volume.IdentityByCNID] resolves a link to its
//     target inode, as the rest of this package does, so every path pointing at
//     one file yields one identity. Use [Volume.OpenCNIDRaw] with
//     [CatalogRecord.Identity] when the link itself is the thing being tracked.
//   - Creation dates survive copying. A file copied with cp -p, or restored
//     from a backup, carries its birth date forward but gets a new CNID, so a
//     non-match is not proof of difference either.
type FileIdentity struct {
	// CNID is the catalog node ID.
	CNID uint32

	// Created is the record's creation date, zero when the volume recorded
	// none. See [CatalogTimes] for why zero means absent.
	Created time.Time

	// Source says how Created must be read, carried along so that a comparison
	// can refuse to treat a classic HFS local wall-clock reading as an absolute
	// instant. See [TimeSource].
	Source TimeSource
}

// Identity returns the record's composite identity. See [FileIdentity].
func (r CatalogRecord) Identity() FileIdentity {
	return FileIdentity{
		CNID:    r.CNID,
		Created: r.Times.Created,
		Source:  r.Times.Source,
	}
}

// Equal reports whether two identities name the same file.
//
// Both halves must agree, and the timestamps must have come from the same kind
// of clock: a classic HFS local reading and an HFS+ GMT one are not comparable
// even when their numeric values match, so they are never equal.
//
// Use this rather than ==. The struct is comparable, but time.Time equality
// under == depends on the monotonic reading and the location pointer as well as
// the instant, which is not what a caller means.
func (id FileIdentity) Equal(other FileIdentity) bool {
	return id.CNID == other.CNID &&
		id.Source == other.Source &&
		id.Created.Equal(other.Created)
}

// Comparable reports whether this identity is safe to match on.
//
// It is false when the CNID is zero or no creation date was recorded, which
// leaves nothing but a reusable number to match on. A false here is a fact
// about the volume rather than a failure: classic HFS volumes and partially
// recovered records routinely produce them, and such a record is better matched
// by path and content.
func (id FileIdentity) Comparable() bool {
	return id.CNID != 0 && !id.Created.IsZero()
}

// String renders the identity as "<cnid>@<RFC3339 creation date>", or
// "<cnid>@unknown" when no date was recorded. The form is stable and suitable
// as a map key for correlating two readings of a volume.
func (id FileIdentity) String() string {
	if id.Created.IsZero() {
		return fmt.Sprintf("%d@unknown", id.CNID)
	}
	return fmt.Sprintf("%d@%s", id.CNID, id.Created.UTC().Format(time.RFC3339))
}

// IdentityByCNID returns the identity of the record with this CNID.
//
// A hard link resolves to its target inode, as everywhere else in this package
// ([Volume.OpenCNID]), so every path pointing at one file yields one identity —
// which is the correct answer, and is also why two links cannot be told apart
// this way.
func (v *Volume) IdentityByCNID(cnid uint32) (FileIdentity, error) {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return FileIdentity{}, err
	}
	return rec.Identity(), nil
}

// IdentityByPath is [Volume.IdentityByCNID] addressed by path.
func (v *Volume) IdentityByPath(path string) (FileIdentity, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return FileIdentity{}, err
	}
	return rec.Identity(), nil
}
