package commands

import "os"

// raiseInterrupt exits 130: Windows has no signal to die of.
func raiseInterrupt() { os.Exit(130) }
