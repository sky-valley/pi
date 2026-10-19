package daemon

// prepareSignals: Windows children do not inherit signal dispositions.
func prepareSignals() {}
