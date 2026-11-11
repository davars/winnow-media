//go:build !unix

package media

// platformID reports nothing on a platform without Stat_t; a file's identity
// is then its size and modification time.
func platformID(any) (dev, ino uint64, ctimeNS int64) { return 0, 0, 0 }
