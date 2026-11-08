//go:build linux

package scan

import (
	"reflect"
	"testing"
)

func TestADriveIsNamedWhereItIsMountedWholeNotWhereOneOfItsFoldersIs(t *testing.T) {
	// zharp-prod-1: a volume at /mnt/HC_Volume_107340381, whose postgresql folder is also mounted
	// at /var/lib/postgresql (a shorter path), plus Docker's single-file /etc/hosts and a tmpfs.
	mountinfo := `26 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
27 26 8:16 /postgresql /var/lib/postgresql rw,relatime shared:2 - ext4 /dev/sdb rw
28 26 8:16 / /mnt/HC_Volume_107340381 rw,relatime shared:3 - ext4 /dev/sdb rw
29 26 0:25 / /run rw,nosuid shared:4 - tmpfs tmpfs rw
30 26 8:1 /etc/hosts-file /etc/hosts rw - ext4 /dev/sda1 rw
31 26 8:32 /data /srv/data rw - xfs /dev/sdc rw
`
	isDir := func(mount string) bool { return mount != "/etc/hosts" }
	got := drivesFrom(mountinfo, isDir)
	// /dev/sdc is only mounted by a folder: that folder is the drive here.
	want := []string{"/", "/mnt/HC_Volume_107340381", "/srv/data"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drives = %v, want %v", got, want)
	}
}
