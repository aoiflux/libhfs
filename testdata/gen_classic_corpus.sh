#!/bin/sh
# gen_classic_corpus.sh — build *populated* classic HFS volumes for the corpus tests.
#
# gen_corpus.sh formats volumes and says, correctly, that populating one needs a
# mount that WSL cannot give. That is true for HFS+. It is not true for classic
# HFS: hfsutils manipulates an HFS volume image entirely in user space, with no
# kernel driver, no mount and no root, so the volumes here have real directory
# trees, real file content, real deletions and a catalog B-tree that has
# actually split.
#
# That matters more than it sounds. Classic HFS is the format the hermetic
# fixtures are worst at, because every fixture is built from the same reading of
# the spec as the parser, and the two real images on hand are both HFS+. The
# first run of this corpus found that PathForCNID could not name the path of any
# file on a classic volume: classic HFS writes a thread record for every
# directory and, per Inside Macintosh: Files, only writes one for a file when
# something asks for a file ID reference. 367 of 367 files here have none. HFS+
# makes threads mandatory (TN1150), so no HFS+ image could ever have shown it.
#
# hfsutils is a third-party implementation this package shares no code or
# reasoning with, which is the whole point of using it.
#
# A caveat to record honestly: fsck.hfs reports these volumes as needing repair.
# The complaints are writer-style artifacts — "unused node is not erased",
# "reserved fields in the catalog record have incorrect data" — not structural
# damage, and Apple's fsck is stricter than the format requires. Unlike the
# mkfs.hfsplus volumes from gen_corpus.sh, these are NOT fsck-clean, and a test
# that wants a pristine volume should not use them.
#
# Requires nothing preinstalled and no root: hfsutils is fetched and unpacked
# into $HOME as an ordinary user. Under WSL, invoke from PowerShell rather than
# Git Bash, which rewrites /mnt/... arguments into Windows paths:
#
#   wsl.exe -- sh /mnt/o/research/libhfs/testdata/gen_classic_corpus.sh /mnt/e/dataset/hfs_synth
#
# Volumes total about 80 MB. Creation dates are fresh on every run, so a
# regenerated image is not byte-identical to its predecessor.

set -u

OUT=${1:-}
if [ -z "$OUT" ]; then
	echo "usage: $0 <output-directory>" >&2
	exit 2
fi

TOOLS=$HOME/.hfsutils
BIN=$TOOLS/root/usr/bin

# --- fetch hfsutils without root -------------------------------------------
# apt-get download needs no privileges and dpkg-deb -x unpacks anywhere, so the
# tools land in $HOME rather than needing a sudo password WSL will not give.
if [ ! -x "$BIN/hformat" ]; then
	echo "fetching hfsutils into $TOOLS"
	mkdir -p "$TOOLS/pkg" "$TOOLS/root" || exit 1
	( cd "$TOOLS/pkg" && apt-get download hfsutils >/dev/null 2>&1 ) || {
		echo "could not download hfsutils; is apt available?" >&2
		exit 1
	}
	for d in "$TOOLS"/pkg/*.deb; do
		dpkg-deb -x "$d" "$TOOLS/root" || exit 1
	done
fi
if [ ! -x "$BIN/hformat" ]; then
	echo "hfsutils not usable at $BIN" >&2
	exit 1
fi

mkdir -p "$OUT" || exit 1
SRC=$OUT/.src
rm -rf "$SRC"; mkdir -p "$SRC" || exit 1

printf 'hello' > "$SRC/tiny.txt"
: > "$SRC/empty.txt"
head -c 4096 /dev/urandom > "$SRC/exact.bin"
head -c 300000 /dev/urandom > "$SRC/big.bin"
head -c 1200000 /dev/urandom > "$SRC/huge.bin"
head -c 65536 /dev/urandom > "$SRC/chunk.bin"
head -c 900000 /dev/urandom > "$SRC/frag.bin"

fail=0
report() {
	if [ "$1" -eq 0 ]; then
		echo "  ok"
	else
		echo "  FAILED"
		fail=$((fail + 1))
	fi
}

# ---------------------------------------------------------------------------
# classic_full: a deep tree, enough files to split the catalog past one leaf,
# real deletions to leave stale records behind for the carver, a rename, and two
# names with high-bit MacRoman bytes so the name decoder meets a third-party
# writer rather than a fixture built from the same spec reading it uses.
echo "classic_full.dd"
IMG=$OUT/classic_full.dd
dd if=/dev/zero of="$IMG" bs=1M count=48 status=none
"$BIN/hformat" -l FULLVOL "$IMG" >/dev/null 2>&1 || { report 1; exit 1; }
"$BIN/hmount" "$IMG" >/dev/null 2>&1 || { report 1; exit 1; }

"$BIN/hmkdir" :Docs
"$BIN/hmkdir" :Docs:Reports
"$BIN/hmkdir" :Docs:Reports:2026
"$BIN/hmkdir" :Media
"$BIN/hcopy" -r "$SRC/tiny.txt"  :Docs:tiny.txt
"$BIN/hcopy" -r "$SRC/empty.txt" :Docs:empty.txt
"$BIN/hcopy" -r "$SRC/exact.bin" :Docs:Reports:exact.bin
"$BIN/hcopy" -r "$SRC/big.bin"   :Docs:Reports:2026:deep.bin
"$BIN/hcopy" -r "$SRC/huge.bin"  :Media:huge.bin

# 0xE9 is MacRoman e-acute, 0xA9 the copyright sign. Both decode to the wrong
# code point if the name bytes are widened instead of mapped.
"$BIN/hcopy" -r "$SRC/tiny.txt" ":Docs:caf\351.txt"
"$BIN/hcopy" -r "$SRC/tiny.txt" ":Docs:\251 2026.txt"

i=0
while [ $i -lt 400 ]; do
	"$BIN/hcopy" -r "$SRC/tiny.txt" ":Media:f$i.txt" || break
	i=$((i + 1))
done
echo "  $i bulk files"

# Deleting every third one leaves records behind in nodes that have already
# split, which is what the carving tests need and what a freshly formatted
# volume can never have.
j=0
while [ $j -lt 120 ]; do
	"$BIN/hdel" ":Media:f$j.txt" >/dev/null 2>&1
	j=$((j + 3))
done
echo "  $((j / 3)) deleted"

"$BIN/hrename" :Docs:tiny.txt :Docs:renamed.txt
"$BIN/hattrib" -t TEXT -c ttxt :Docs:renamed.txt
"$BIN/humount" >/dev/null 2>&1
report 0

# ---------------------------------------------------------------------------
# classic_frag: write many chunks, punch every other one out, then write a file
# large enough that it has to reuse the holes. A fork needing more than three
# extents spills into the extents overflow file, which is a separate code path
# on classic HFS from the one HFS+ uses.
echo "classic_frag.dd"
IMG=$OUT/classic_frag.dd
dd if=/dev/zero of="$IMG" bs=1M count=16 status=none
"$BIN/hformat" -l FRAGVOL "$IMG" >/dev/null 2>&1 || { report 1; exit 1; }
"$BIN/hmount" "$IMG" >/dev/null 2>&1 || { report 1; exit 1; }
i=0
while [ $i -lt 60 ]; do "$BIN/hcopy" -r "$SRC/chunk.bin" ":c$i.bin" || break; i=$((i + 1)); done
i=0
while [ $i -lt 60 ]; do "$BIN/hdel" ":c$i.bin" >/dev/null 2>&1; i=$((i + 2)); done
"$BIN/hcopy" -r "$SRC/frag.bin" :frag.bin
"$BIN/humount" >/dev/null 2>&1
report 0

# ---------------------------------------------------------------------------
# classic_small: a volume with a handful of records, so the catalog is still a
# single leaf. Tests that key off the pristine/used distinction need one of
# each, and this is the used-but-unsplit middle case.
echo "classic_small.dd"
IMG=$OUT/classic_small.dd
dd if=/dev/zero of="$IMG" bs=1M count=16 status=none
"$BIN/hformat" -l SMALLVOL "$IMG" >/dev/null 2>&1 || { report 1; exit 1; }
"$BIN/hmount" "$IMG" >/dev/null 2>&1 || { report 1; exit 1; }
"$BIN/hcopy" -r "$SRC/tiny.txt" :plain.txt
"$BIN/hmkdir" :OneDir
"$BIN/hcopy" -r "$SRC/exact.bin" :OneDir:exact.bin
"$BIN/humount" >/dev/null 2>&1
report 0

echo
for f in "$OUT"/*.dd; do
	printf '%-22s %s bytes  sig=%s\n' "$(basename "$f")" \
		"$(wc -c < "$f")" \
		"$(dd if="$f" bs=1 skip=1024 count=2 status=none | od -An -tx1 | tr -d ' \n')"
done

rm -rf "$SRC"

echo
if [ "$fail" -ne 0 ]; then
	echo "$fail volume(s) failed"
	exit 1
fi
echo "all volumes written to $OUT"
echo "run the suite against one with:"
echo "  LIBHFS_CORPUS_IMAGE=$OUT/classic_full.dd go test -count=1 ."
