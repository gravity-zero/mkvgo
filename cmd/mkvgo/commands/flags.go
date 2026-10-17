package commands

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// cliFlags is one command's flags: a flag.FlagSet holds their definitions and
// values, read with mkvgo's syntax rather than the flag package's. A value
// follows its flag as the next argument, flags and operands come in any
// order, and a flag is known only under the spellings it registers ("-o",
// "-segment" and "--segment"): no "-flag=value", no other dash count.
type cliFlags struct {
	fs       *flag.FlagSet
	spelling map[string]string // what is typed -> the flag's name in fs
	missing  map[string]string // the flag's name -> its own message for a missing value
}

func newFlags(cmd string) *cliFlags {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &cliFlags{fs: fs, spelling: map[string]string{}, missing: map[string]string{}}
}

// Missing sets the message a flag exits with when its value is missing.
func (c *cliFlags) Missing(spelling, msg string) { c.missing[c.spelling[spelling]] = msg }

// define registers v under its spellings; the first, without its dashes, names it in the FlagSet.
func (c *cliFlags) define(v flag.Value, spellings ...string) {
	name := strings.TrimLeft(spellings[0], "-")
	c.fs.Var(v, name, "")
	for _, s := range spellings {
		c.spelling[s] = name
	}
}

// String binds a flag taking a value to p.
func (c *cliFlags) String(p *string, spellings ...string) {
	c.define((*stringValue)(p), spellings...)
}

// Bool binds a flag taking no value to p, set true when present.
func (c *cliFlags) Bool(p *bool, spellings ...string) {
	c.define((*boolValue)(p), spellings...)
}

// Func binds a flag taking a value to fn, called with each value in turn;
// fn reports a bad value itself (Fatal), so its message is the command's own.
func (c *cliFlags) Func(fn func(string), spellings ...string) {
	c.define(funcValue(fn), spellings...)
}

// Do binds a flag taking no value to fn, called each time it is present.
func (c *cliFlags) Do(fn func(), spellings ...string) {
	c.define(doValue(fn), spellings...)
}

// Parse reads args and returns the operands; an unknown argument starting
// with '-' exits as an unknown flag.
func (c *cliFlags) Parse(args []string) []string {
	return c.parse(args, true)
}

// ParseKnown is Parse leaving unknown flags among the operands, for a command
// that hands them to another parser (serve and the shared HLS flags).
func (c *cliFlags) ParseKnown(args []string) []string {
	return c.parse(args, false)
}

func (c *cliFlags) parse(args []string, strict bool) []string {
	var operands []string
	for i := 0; i < len(args); i++ {
		name, ok := c.spelling[args[i]]
		if !ok {
			if strict {
				rejectFlagArg(args[i])
			}
			operands = append(operands, args[i])
			continue
		}
		tok, f := args[i], c.fs.Lookup(name)
		v := "true"
		if b, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool || !b.IsBoolFlag() {
			if msg, own := c.missing[name]; own && i+1 >= len(args) {
				Fatal(msg)
			}
			v = needValue(args, &i)
		}
		if err := c.fs.Set(name, v); err != nil {
			Fatal(tok + ": " + err.Error())
		}
	}
	return operands
}

type stringValue string

func (s *stringValue) Set(v string) error { *s = stringValue(v); return nil }
func (s *stringValue) String() string     { return string(*s) }

type boolValue bool

func (b *boolValue) Set(v string) error {
	x, err := strconv.ParseBool(v)
	*b = boolValue(x)
	return err
}
func (b *boolValue) String() string   { return strconv.FormatBool(bool(*b)) }
func (b *boolValue) IsBoolFlag() bool { return true }

type funcValue func(string)

func (f funcValue) Set(v string) error { f(v); return nil }
func (f funcValue) String() string     { return "" }

type doValue func()

func (d doValue) Set(string) error { d(); return nil }
func (d doValue) String() string   { return "" }
func (d doValue) IsBoolFlag() bool { return true }

// damageBytesFlag parses --damage-bytes into n: a positive byte count, or a negative one to refuse any damage.
func damageBytesFlag(n *int64) func(string) {
	return func(v string) {
		b, err := strconv.ParseInt(v, 10, 64)
		if err != nil || b == 0 {
			Fatal(fmt.Sprintf("invalid --damage-bytes %q (a positive byte count, or -1 to refuse any damage)", v))
		}
		*n = b
	}
}
