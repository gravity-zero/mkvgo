package mp4

import (
	"bytes"
	"strconv"
	"strings"
)

// networkFUSE are the FUSE filesystems that reach their files across a
// network (their subtype in mountinfo, fuse.<subtype>); any other FUSE mount
// - mergerfs pooling local disks first among them - is taken as local.
var networkFUSE = map[string]bool{
	"rclone": true, "sshfs": true, "s3fs": true, "gcsfuse": true, "davfs": true, "davfs2": true,
	"goofys": true, "juicefs": true, "blobfuse": true, "blobfuse2": true, "ossfs": true, "curlftpfs": true,
	"smbnetfs": true, "httpfs2": true, "ceph-fuse": true, "glusterfs": true, "mfs": true, "lizardfs": true,
}

// mountFSType is the filesystem type /proc/self/mountinfo gives the mount of
// device major:minor ("fuse.mergerfs", "ext4", ...); empty when none matches.
func mountFSType(mountinfo []byte, major, minor uint64) string {
	dev := strconv.FormatUint(major, 10) + ":" + strconv.FormatUint(minor, 10)
	for _, line := range bytes.Split(mountinfo, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 3 || f[2] != dev {
			continue
		}
		for i, x := range f {
			if x == "-" && i+1 < len(f) {
				return f[i+1]
			}
		}
	}
	return ""
}

// networkFSType reports whether a mountinfo filesystem type reaches its files across a network.
func networkFSType(t string) bool {
	if sub, ok := strings.CutPrefix(t, "fuse."); ok {
		return networkFUSE[sub]
	}
	switch t {
	case "nfs", "nfs4", "cifs", "smb3", "smbfs", "9p", "ceph", "afs", "davfs", "glusterfs":
		return true
	}
	return false
}
