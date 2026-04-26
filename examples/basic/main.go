package main

import (
	"fmt"
	"log"
	"os"
	"time"

	hfs "github.com/aoiflux/libhfs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <hfs_image_or_device>\n", os.Args[0])
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  ./basic disk.img")
		fmt.Println("  ./basic /dev/disk2s1")
		fmt.Println("  ./basic \\\\.\\PhysicalDrive1   # Windows (Administrator)")
		fmt.Println()
		fmt.Println("Opens an HFS/HFS+/HFSX volume, prints metadata, and lists the root directory.")
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

	printVolumeInfo(vol)
	fmt.Println()
	listRootDir(vol)
}

func printVolumeInfo(vol *hfs.Volume) {
	h := vol.Header()

	fmt.Println("=== HFS Volume Information ===")
	fmt.Printf("Kind            : %s\n", vol.Kind())
	fmt.Printf("Block Size      : %d bytes\n", h.BlockSize)
	fmt.Printf("Total Blocks    : %d\n", h.TotalBlocks)
	fmt.Printf("Free Blocks     : %d\n", h.FreeBlocks)

	total := uint64(h.TotalBlocks) * uint64(h.BlockSize)
	free := uint64(h.FreeBlocks) * uint64(h.BlockSize)
	fmt.Printf("Volume Size     : %s\n", formatBytes(total))
	fmt.Printf("Free Space      : %s\n", formatBytes(free))

	fmt.Printf("File Count      : %d\n", h.FileCount)
	fmt.Printf("Folder Count    : %d\n", h.FolderCount)
	fmt.Printf("Next Catalog ID : %d\n", h.NextCatalogID)

	if !h.CreateTime.IsZero() {
		fmt.Printf("Created         : %s\n", h.CreateTime.Format(time.RFC3339))
	}
	if !h.ModifyTime.IsZero() {
		fmt.Printf("Modified        : %s\n", h.ModifyTime.Format(time.RFC3339))
	}
	if !h.BackupTime.IsZero() && h.BackupTime.Year() > 1904 {
		fmt.Printf("Last Backup     : %s\n", h.BackupTime.Format(time.RFC3339))
	}
}

func listRootDir(vol *hfs.Volume) {
	entries, err := vol.ReadDir("/")
	if err != nil {
		log.Fatalf("failed to list root directory: %v", err)
	}

	fmt.Printf("=== Root Directory (%d entries) ===\n", len(entries))
	for _, e := range entries {
		kind := "FILE"
		if e.IsDirectory {
			kind = "DIR "
		}
		fmt.Printf("[%s] %s  (CNID %d)\n", kind, e.Name, e.CNID)
	}
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
