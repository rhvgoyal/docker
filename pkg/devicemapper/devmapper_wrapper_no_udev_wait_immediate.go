// +build linux,libdm_no_udev_wait_immediate

package devicemapper

import (
	"github.com/Sirupsen/logrus"
)

// libdevmapper does not support dm_udev_wait_immediate() functionality.
const libraryUdevWaitImmediate = false

func dmUdevWaitImmediateFct(cookie uint) (int, int) {
	// Error, nobody should be calling it.
	logrus.Debugf("dm_udev_wait_immediate() is not supported by libdevmapper")
	return -1, 1
}
