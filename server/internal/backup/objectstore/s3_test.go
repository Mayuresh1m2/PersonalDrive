package objectstore

import "testing"

func TestRestoreState(t *testing.T) {
	cases := []struct {
		class, header string
		want          RestoreState
	}{
		{"STANDARD", "", RestoreNotNeeded},
		{"", "", RestoreNotNeeded},
		{"DEEP_ARCHIVE", "", RestoreNone},
		{"DEEP_ARCHIVE", `ongoing-request="true"`, RestoreInProgress},
		{"GLACIER", `ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`, RestoreReady},
	}
	for _, c := range cases {
		if got := restoreState(c.class, c.header); got != c.want {
			t.Errorf("restoreState(%q, %q) = %s, want %s", c.class, c.header, got, c.want)
		}
	}
	if (ObjectInfo{Restore: RestoreInProgress}).Readable() || !(ObjectInfo{Restore: RestoreReady}).Readable() {
		t.Error("Readable wrong")
	}
}
