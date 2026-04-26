package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	hfs "github.com/aoiflux/libhfs"
)

type stats struct {
	files  int
	dirs   int
	bytes  uint64
	errors int
}

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <hfs_image_or_device> <directory_path>\n", os.Args[0])
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  ./traverse disk.img /")
		fmt.Println("  ./traverse disk.img /Users/admin/Documents")
		fmt.Println()
		fmt.Println("Recursively lists all files and directories under the given path,")
		fmt.Println("printing sizes and a summary at the end.")
		os.Exit(1)
	}

	f, err := os.Open(os.Args[1])
	if err != nil {
		log.Fatalf("failed to open volume: %v", err)
	}
	defer f.Close()

	vol, err := hfs.Open(f)
	if err != nil {
		log.Fatalf("failed to parse HFS volume: %v", err)
	}
	defer vol.Close()

	root := os.Args[2]
	if root == "" {
		root = "/"
	}

	fmt.Printf("=== Traversing %s (%s) ===\n\n", root, vol.Kind())

	var s stats
	if err := traverse(vol, root, 0, &s); err != nil {
		log.Fatalf("traversal error: %v", err)
	}

	fmt.Println()
	fmt.Println("=== Summary ===")
	fmt.Printf("Directories : %d\n", s.dirs)
	fmt.Printf("Files       : %d\n", s.files)
	fmt.Printf("Total Size  : %s\n", formatBytes(s.bytes))
	if s.errors > 0 {
		fmt.Printf("Errors      : %d\n", s.errors)
	}
}

const maxDepth = 32

func traverse(vol *hfs.Volume, path string, depth int, s *stats) error {
	if depth > maxDepth {
		return nil
	}

	entries, err := vol.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read dir %q: %w", path, err)
	}

	indent := strings.Repeat("  ", depth)

	for _, e := range entries {
		var fullPath string
		if path == "/" {
			fullPath = "/" + e.Name
		} else {
			fullPath = path + "/" + e.Name
		}

		if e.IsDirectory {
			s.dirs++
			fmt.Printf("%s[DIR] %s/\n", indent, e.Name)
			if err := traverse(vol, fullPath, depth+1, s); err != nil {
				fmt.Printf("%s  (error: %v)\n", indent, err)
				s.errors++
			}
		} else {
			s.files++
			rec, recErr := vol.OpenCNID(e.CNID)
			var size uint64
			if recErr == nil {
				size = rec.DataFork.LogicalSize
				s.bytes += size
			}
			fmt.Printf("%s[FILE] %-40s  %s\n", indent, e.Name, formatBytes(size))
		}
	}
	return nil
}

func formatBytes(n uint64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
