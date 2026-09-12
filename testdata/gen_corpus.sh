#!/bin/sh
# gen_corpus.sh — format HFS, HFS+ and HFSX volumes for the corpus tests.
#
# The corpus tests (LIBHFS_CORPUS_IMAGE) exist because synthetic fixtures are
# built from the same reading of the spec as the parser, so they cannot catch a
# misreading. A real image is better evidence, but the two on hand between them
# cover only two block sizes and one volume kind. This script fills the gap with
# volumes written by Apple's own formatter — hfsprogs, the Linux port of
# diskdev_cmds — which is an implementation this package shares no code or
# reasoning with.
#
# What these volumes prove is *geometry*: block sizes from 512 to 65536, HFSX
# case sensitivity, a journal, an HFS wrapper around an embedded HFS+ volume
# (the only source of a real non-zero base offset besides img11), and classic
# HFS. They are empty, so they prove nothing about file content, and the
# features still missing from the corpus — extended attributes, hard links,
# symlinks, decmpfs compression, genuine deletions — need a volume that has been
# written to, not merely formatted.
#
# Getting one needs a mount, and mounting HFS+ needs a kernel module. WSL2 has
# no hfsplus module and its sudo wants a password, so under WSL these volumes
# can be formatted but not populated. That is the binding constraint on corpus
# coverage; it is not something a cleverer script can work around.
#
# Requires hfsprogs:   apt-get install hfsprogs
# Under WSL, invoke from PowerShell rather than Git Bash, which rewrites
# /mnt/... arguments into Windows paths and breaks the call:
#
#   wsl.exe -- sh /mnt/o/research/libhfs/testdata/gen_corpus.sh /mnt/e/dataset/hfs_synth
#
# Volumes are ~48 MB each (128 MB for the 65536-byte one, which needs the room
# to hold a catalog at that block size), about 513 MB in total.
#
# Each volume carries a fresh identifier and creation date, so successive runs
# are not byte-identical. Nothing in the suite pins those values, but do not
# treat a regenerated image as a reproducible fixture.

set -u

OUT=${1:-}
if [ -z "$OUT" ]; then
	echo "usage: $0 <output-directory>" >&2
	exit 2
fi
if ! command -v mkfs.hfsplus >/dev/null 2>&1; then
	echo "mkfs.hfsplus not found; install hfsprogs" >&2
	exit 1
fi
mkdir -p "$OUT" || exit 1

fail=0

# mk <name> <size-in-MiB> <mkfs-options...>
mk() {
	name=$1
	size=$2
	shift 2
	img="$OUT/$name.dd"

	rm -f "$img"
	dd if=/dev/zero of="$img" bs=1M count="$size" status=none 2>/dev/null
	if ! mkfs.hfsplus "$@" "$img" >/dev/null 2>&1; then
		printf 'FAIL  %-16s mkfs.hfsplus %s\n' "$name" "$*"
		rm -f "$img"
		fail=$((fail + 1))
		return
	fi

	# fsck is an independent check that the volume is well formed. It is not
	# this package's opinion of the image, which is the whole point of using
	# it: a volume both fsck and libhfs accept is evidence, where a volume
	# only libhfs accepts is not.
	if command -v fsck.hfsplus >/dev/null 2>&1; then
		if fsck.hfsplus -n -f "$img" >/dev/null 2>&1; then
			printf 'ok    %-16s %-28s fsck clean\n' "$name" "$*"
		else
			printf 'DIRTY %-16s %-28s fsck reports a problem\n' "$name" "$*"
			fail=$((fail + 1))
		fi
	else
		printf 'ok    %-16s %-28s (fsck.hfsplus absent)\n' "$name" "$*"
	fi
}

# Block size sweep. 4096 is the default and the size both real corpus images
# use for HFS+; the others are what catch arithmetic that only looks right at
# 4096. At 512 the boot blocks and volume header span three allocation blocks
# rather than one, which is the case TestReservedBlocksAcrossBlockSizes pins.
mk hfsp_b512    48 -b 512   -v b512
mk hfsp_b1024   48 -b 1024  -v b1024
mk hfsp_b4096   48 -b 4096  -v b4096
mk hfsp_b65536 128 -b 65536 -v b65536

# HFSX with case-sensitive comparison: the only way to reach the CompType
# branch of Volume.Capabilities on a real volume.
mk hfsx_sens    48 -s -b 4096 -v casesens

# A journal, so the journaled attribute bit and the .journal files it creates
# are exercised against something a formatter actually wrote.
mk hfsp_journal 48 -J -b 4096 -v journaled

# An HFS wrapper around an embedded HFS+ volume. This is the important one: it
# is a real non-zero base offset, and a wrong base offset is silent — it reads
# plausible bytes from the wrong part of the image rather than failing.
mk hfsp_wrapped 48 -w -b 4096 -v wrapped

# Classic HFS, where the boot blocks and MDB sit outside the allocation block
# area and nothing is reserved within it.
mk hfs_classic  48 -h -b 4096 -v classic

# Case sensitivity and a journal at the smallest block size together, so the
# combination is covered rather than each alone.
mk hfsx_j_b512  48 -s -J -b 512 -v sensj512

echo
if [ "$fail" -ne 0 ]; then
	echo "$fail volume(s) failed"
	exit 1
fi
echo "all volumes written to $OUT"
echo "run the suite against one with:"
echo "  LIBHFS_CORPUS_IMAGE=$OUT/hfsp_b512.dd go test -count=1 ."
