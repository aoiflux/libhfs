#!/bin/sh
# gen_blkid_oracle.sh — write "<image>.blkid.tsv" beside each image given.
#
# Why this exists
# ---------------
# libhfs derives an HFS+ volume UUID by the algorithm in Apple's hfs_util.c:
# MD5 over a fixed namespace UUID followed by the volume header's two Finder
# identifier words in big-endian order, then the RFC 4122 version and variant
# bits. Nothing in the repo could check that derivation. The corpus tests
# compare libhfs to libhfs, and `diskutil info` needs a Mac.
#
# util-linux's libblkid implements the same derivation from the same published
# source, in C, with no code shared with this package. It also reads the volume
# label — from drVN for classic HFS, and from the root catalog record for HFS+,
# which are the two paths Volume.VolumeName takes. So one blkid run gives an
# independent expectation for both.
#
# Run it from WSL or any Linux host with util-linux installed:
#
#   sh testdata/gen_blkid_oracle.sh /mnt/e/dataset/hfs_synth/*.dd \
#                                   /mnt/e/dataset/img8_hfs.dd \
#                                   /mnt/e/dataset/img11_hfs.dd
#
# Output is one line per image:
#
#   U <type> <uuid> <label>
#
# with "-" where blkid reported nothing. A "-" is not an assertion that the
# field is absent — blkid declines to read a label it cannot reach, and on at
# least one image here (64 KiB allocation blocks) it does exactly that — so the
# reader treats "-" as "unchecked", never as "expected empty".

if ! command -v blkid >/dev/null 2>&1; then
	echo "gen_blkid_oracle.sh: blkid not found (install util-linux)" >&2
	exit 1
fi

for img in "$@"; do
	if [ ! -f "$img" ]; then
		echo "skip (not a file): $img" >&2
		continue
	fi

	type=$(blkid -o value -s TYPE "$img" 2>/dev/null)
	case "$type" in
	hfs | hfsplus) ;;
	*)
		echo "skip (blkid says '${type:-unknown}', not HFS): $img" >&2
		continue
		;;
	esac

	uuid=$(blkid -o value -s UUID "$img" 2>/dev/null)
	label=$(blkid -o value -s LABEL "$img" 2>/dev/null)

	out="$img.blkid.tsv"
	printf 'U\t%s\t%s\t%s\n' "$type" "${uuid:--}" "${label:--}" >"$out"
	echo "wrote $out: $type ${uuid:--} ${label:--}"
done
