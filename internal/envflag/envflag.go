// Package envflag defines flags that can also be set from the environment: a
// flag given on the command line wins over the variable, and the variable over
// the default.
package envflag

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// fromEnv sets the flag from its environment variable, if that is set. The
// value is set on the flag and is not its default: -help prints defaults, and
// a token or a client secret from the environment must not end up in it.
func fromEnv(name, env string) {
	v, ok := os.LookupEnv(env)
	if !ok {
		return
	}
	if err := flag.Set(name, v); err != nil {
		fmt.Fprintf(os.Stderr, "invalid $%s=%q: %v\n", env, v, err)
		os.Exit(2)
	}
}

func String(name, env, def, usage string) *string {
	p := flag.String(name, def, usage+" [$"+env+"]")
	fromEnv(name, env)
	return p
}

func Duration(name, env string, def time.Duration, usage string) *time.Duration {
	p := flag.Duration(name, def, usage+" [$"+env+"]")
	fromEnv(name, env)
	return p
}

func Bool(name, env string, def bool, usage string) *bool {
	p := flag.Bool(name, def, usage+" [$"+env+"]")
	fromEnv(name, env)
	return p
}

func Int(name, env string, def int, usage string) *int {
	p := flag.Int(name, def, usage+" [$"+env+"]")
	fromEnv(name, env)
	return p
}
