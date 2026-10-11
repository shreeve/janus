package main

import (
	"errors"
	"os"
)

// reopenSupervisorLog: Windows has no supervisor item, so nothing hands
// the edge a supervisor log to roll.
func reopenSupervisorLog(string, os.FileMode, bool, bool) error {
	return errors.New("no supervisor log on windows")
}
