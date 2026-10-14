package mp4

import "testing"

// The filesystem type of a mount is read off its mountinfo line; the network
// ones are told apart from local ones, FUSE by its subtype.
func TestMountFSType(t *testing.T) {
	mountinfo := []byte(`22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
45 22 0:51 / /mnt/pool rw,relatime shared:30 - fuse.mergerfs pool rw,user_id=0
46 22 0:52 / /mnt/cloud rw,nosuid shared:31 - fuse.rclone gdrive: rw,user_id=1000
47 22 0:53 / /mnt/ssh rw shared:32 - fuse.sshfs user@host:/data rw
48 22 0:54 / /mnt/nas rw shared:33 - nfs4 nas:/export rw,vers=4.2
49 22 0:55 / /mnt/smb rw shared:34 optional:1 - cifs //nas/share rw
50 22 0:56 / /mnt/s3 rw shared:35 - fuse.s3fs s3fs rw
`)
	for _, c := range []struct {
		major, minor uint64
		fstype       string
		network      bool
	}{
		{8, 1, "ext4", false},
		{0, 51, "fuse.mergerfs", false},
		{0, 52, "fuse.rclone", true},
		{0, 53, "fuse.sshfs", true},
		{0, 54, "nfs4", true},
		{0, 55, "cifs", true},
		{0, 56, "fuse.s3fs", true},
		{9, 9, "", false},
	} {
		got := mountFSType(mountinfo, c.major, c.minor)
		if got != c.fstype || networkFSType(got) != c.network {
			t.Errorf("%d:%d: %q network %v, want %q network %v", c.major, c.minor, got, networkFSType(got), c.fstype, c.network)
		}
	}
}
