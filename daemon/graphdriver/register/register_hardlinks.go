// +build !exclude_graphdriver_hardlinks,linux

package register

import (
	// register the hardlinks graphdriver
	_ "github.com/docker/docker/daemon/graphdriver/hardlinks"
)
