package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	hfs "github.com/aoiflux/libhfs"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Printf("Usage: %s <hfs_image_or_device> <source_path> <output_file>\n", os.Args[0])
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  ./extract disk.img /etc/hosts ./hosts")
		fmt.Println("  ./extract disk.img /Library/Fonts/Helvetica.ttc ./Helvetica.ttc")
		fmt.Println()
		fmt.Println("Flags:")
		fmt.Println("  source_path   Absolute path inside the HFS volume (case-insensitive for HFS+)")
		fmt.Println("  output_file   Local path to write the extracted data fork")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	srcPath := os.Args[2]
	outPath := os.Args[3]

	f, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("failed to open volume: %v", err)
	}
	defer f.Close()

	vol, err := hfs.Open(f)
	if err != nil {
		log.Fatalf("failed to parse HFS volume: %v", err)
	}
	defer vol.Close()

	fmt.Printf("Volume : %s\n", vol.Kind())
	fmt.Printf("Source : %s\n", srcPath)
	fmt.Printf("Output : %s\n", outPath)
	fmt.Println()

	hfsFile, err := vol.OpenFileByPath(srcPath)
	if err != nil {
		if errors.Is(err, hfs.ErrNotFound) {
			log.Fatalf("file not found: %s", srcPath)
		}
		if errors.Is(err, hfs.ErrNotFile) {
			log.Fatalf("%s is a directory, not a file", srcPath)
		}
		log.Fatalf("failed to open file: %v", err)
	}

	out, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("failed to create output file: %v", err)
	}
	defer out.Close()

	written, err := io.Copy(out, &hfsFileReader{f: hfsFile})
	if err != nil {
		log.Fatalf("extraction failed after %d bytes: %v", written, err)
	}

	fmt.Printf("Extracted %s (%d bytes)\n", outPath, written)
}

// hfsFileReader wraps hfs.File to satisfy io.Reader via sequential reads.
type hfsFileReader struct {
	f   *hfs.File
	off int64
}

func (r *hfsFileReader) Read(p []byte) (int, error) {
	if r.off >= r.f.Size() {
		return 0, io.EOF
	}
	n, err := r.f.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}
