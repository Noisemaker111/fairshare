package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// du reports how much disk a set of folders really uses: a file hardlinked from many places (as
// package managers do from their store) is counted once, and files shared with anything outside
// the folders are counted apart.
func cmdDu(args []string) int {
	fl := flag.NewFlagSet("du", flag.ExitOnError)
	fl.Parse(args)
	type fileID struct{ vol, hi, lo uint32 }
	seen := map[fileID]bool{}
	var apparent, unique, linkedOut, files, dirs int64
	for _, root := range fl.Args() {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 || isReparse(d) {
				if d.IsDir() {
					return filepath.SkipDir // junctions and links: their target is counted where it lives
				}
				return nil
			}
			if d.IsDir() {
				dirs++
				return nil
			}
			files++
			p, _ := windows.UTF16PtrFromString(path)
			h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				return nil
			}
			var info windows.ByHandleFileInformation
			err = windows.GetFileInformationByHandle(h, &info)
			windows.CloseHandle(h)
			if err != nil {
				return nil
			}
			size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
			apparent += size
			id := fileID{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}
			if !seen[id] {
				seen[id] = true
				unique += size
				if info.NumberOfLinks > 1 {
					linkedOut += size
				}
			}
			return nil
		})
	}
	gb := func(b int64) float64 { return float64(b) / (1 << 30) }
	fmt.Printf("%d files in %d folders: %.2f GB as listed, %.2f GB of distinct files, of which %.2f GB are hardlinks shared with elsewhere (a package store)\n",
		files, dirs, gb(apparent), gb(unique), gb(linkedOut))
	return 0
}

func isReparse(d fs.DirEntry) bool {
	info, err := d.Info()
	if err != nil {
		return false
	}
	if sys, ok := info.Sys().(*windows.Win32FileAttributeData); ok {
		return sys.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return info.Mode()&os.ModeIrregular != 0
}
