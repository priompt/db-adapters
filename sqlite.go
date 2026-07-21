package priomptdb

import _ "modernc.org/sqlite" // pure-Go driver — single binary, zero external deps

// sqlite is the self-hosted default and the catch-all: the DSN is a file path,
// and placeholders are already ?-style, so rebind is the identity.
var sqlite = dialect{
	driver: "sqlite",
	match:  func(string) bool { return true },
	rebind: func(q string) string { return q },
}
