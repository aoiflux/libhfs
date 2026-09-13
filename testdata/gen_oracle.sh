#!/bin/sh
# gen_oracle.sh — record what a third-party HFS implementation sees in an image.
#
# Every corpus test in this repo so far checks libhfs against itself: that its
# block accounting adds up, that two code paths agree, that a walk is
# self-consistent. None of that can catch a misread field, because both sides of
# the comparison come from the same parser. A volume whose file sizes are all
# decoded from the wrong offset is perfectly self-consistent.
#
# This writes an independent answer. hfsutils is a separate implementation with
# no code in common with libhfs, so where the two agree on a file's size the
# agreement means something. Its listing goes next to the image as
# <image>.oracle.tsv and TestCorpusMatchesOracle reads it if it is there.
#
# Usage:
#   wsl.exe -- sh /mnt/o/research/libhfs/testdata/gen_oracle.sh /mnt/e/dataset/hfs_synth/*.dd
#
# Classic HFS only, for two reasons: hfsutils cannot read HFS+ at all, and on a
# wrapped volume it reads the HFS *wrapper* while libhfs reads the embedded HFS+
# volume inside it. Those are two different filesystems with different contents,
# so wrapped images are skipped rather than compared. Both cases are detected
# below, not assumed.
#
# Format, tab-separated, sorted by path:
#   V <volume name> <free bytes>
#   d <path> <item count>
#   f <path> <data size> <rsrc size> <type/creator>
#
# Names are recorded as the raw bytes hfsutils prints, with no escaping: a
# high-bit MacRoman byte appears as that byte. The reader decodes them with the
# package's own MacRoman table.

B="${HFSUTILS_BIN:-$HOME/hfstools/root/usr/bin}"

if [ ! -x "$B/hls" ]; then
	echo "hfsutils not found at $B — set HFSUTILS_BIN" >&2
	exit 1
fi

# list_dir prints one tagged line per entry of a single directory. Recursion is
# deliberately not used: POSIX sh has no local variables, so a recursive walker
# clobbers its own path variables the moment it descends, and the second
# subdirectory of any directory gets a corrupted path. The driver below expands
# the tree iteratively instead.
list_dir() {
	"$B/hls" -l -a "$1" 2>/dev/null | awk -v up="$2" '
		# f  TYPE/CREA  <rsrc>  <data>  Mon DD HH:MM  name
		#
		# hfsutils marks a file "f" or "F" — the capital carries a flag
		# distinction that does not change what the entry is. Matching only the
		# lowercase form silently drops those files from the oracle, which on
		# the real corpus image hid exactly one file and made libhfs look wrong
		# for finding it.
		$1 == "f" || $1 == "F" {
			name = ""
			for (i = 8; i <= NF; i++) name = name (i > 8 ? " " : "") $i
			printf "f\t%s%s\t%s\t%s\t%s\n", up, name, $4, $3, $2
			next
		}
		# d  <N> items  Mon DD HH:MM  name
		$1 == "d" {
			name = ""
			for (i = 7; i <= NF; i++) name = name (i > 7 ? " " : "") $i
			printf "d\t%s%s\t%s\n", up, name, $2
			next
		}
	'
}

# is_wrapped reports whether the MDB carries an embedded HFS+ volume, which is
# drEmbedSigWord "H+" at offset 0x7C of the 512-byte MDB at 1024.
is_wrapped() {
	sig=$(dd if="$1" bs=1 skip=$((1024 + 124)) count=2 status=none 2>/dev/null | od -An -c | tr -d ' \n')
	[ "$sig" = "H+" ]
}

for img in "$@"; do
	case "$img" in
	*.oracle.tsv) continue ;;
	esac
	[ -f "$img" ] || continue

	if is_wrapped "$img"; then
		echo "skip (HFS wrapper around an embedded HFS+ volume): $img" >&2
		continue
	fi
	if ! "$B/hmount" "$img" >/dev/null 2>&1; then
		echo "skip (not classic HFS, or unreadable): $img" >&2
		continue
	fi

	result=$(mktemp)
	expanded=$(mktemp)
	: >"$expanded"

	list_dir ":" "/" >"$result"

	# Expand directories breadth-first. A directory's HFS path is a pure
	# transformation of its Unix path — HFS forbids ":" in names — so no queue
	# bookkeeping is needed beyond remembering which are already done.
	while :; do
		todo=$(awk -F'\t' '$1 == "d" { print $2 }' "$result" |
			LC_ALL=C sort -u | grep -vxF -f "$expanded")
		[ -n "$todo" ] || break
		printf '%s\n' "$todo" >>"$expanded"
		printf '%s\n' "$todo" | while IFS= read -r up; do
			[ -n "$up" ] || continue
			hp=":$(printf '%s' "${up#/}" | tr '/' ':'):"
			list_dir "$hp" "$up/" >>"$result"
		done
	done

	out="$img.oracle.tsv"
	{
		"$B/hvol" 2>/dev/null | awk '
			/^Volume name is/ { gsub(/"/, "", $4); vn = $4 }
			/^Volume has/     { free = $3 }
			END { printf "V\t%s\t%s\n", vn, free }
		'
		cat "$result"
	} | LC_ALL=C sort >"$out"

	rm -f "$result" "$expanded"
	"$B/humount" >/dev/null 2>&1
	echo "wrote $out ($(wc -l <"$out") lines)"
done
