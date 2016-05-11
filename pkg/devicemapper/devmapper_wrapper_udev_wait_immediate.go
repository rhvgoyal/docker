// +build linux,!libdm_no_udev_wait_immediate

package devicemapper

/*
#cgo LDFLAGS: -L. -ldevmapper
#include <libdevmapper.h>
*/
import "C"

// pkg devicemapper supports dm_udev_wait_immediate() as libdevmapper
// supports it.
const libraryUdevWaitImmediate = true

func dmUdevWaitImmediateFct(cookie uint) (int, int) {
	var ready C.int
	return int(C.dm_udev_wait_immediate(C.uint32_t(cookie), &ready)), int(ready)
}
