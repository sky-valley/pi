package providers

// errnoCode is Node's code for the socket error in err's chain, if it names
// one. Plan 9 reports socket failures as text, which names none.
func errnoCode(error) (string, bool) { return "", false }
