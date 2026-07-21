package priomptdb

import (
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
)

// postgres claims postgres:// and postgresql:// DSNs; pgx wants $1,$2,… placeholders.
var postgres = dialect{
	driver: "pgx",
	match: func(dsn string) bool {
		return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
	},
	rebind: func(q string) string {
		var b strings.Builder
		n := 0
		for _, c := range q {
			if c == '?' {
				n++
				fmt.Fprintf(&b, "$%d", n)
				continue
			}
			b.WriteRune(c)
		}
		return b.String()
	},
}
