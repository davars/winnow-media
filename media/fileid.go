package media

import "os"

// fileID is what a Source records of its input to notice that the file changed:
// the file's device and inode where the platform reports them, its size and its
// modification and status-change times.
type fileID struct {
	dev, ino         uint64
	size             int64
	mtimeNS, ctimeNS int64
}

// idOf returns the identity of the file info describes.
func idOf(info os.FileInfo) fileID {
	dev, ino, ctime := platformID(info.Sys())
	return fileID{dev: dev, ino: ino, size: info.Size(), mtimeNS: info.ModTime().UnixNano(), ctimeNS: ctime}
}
