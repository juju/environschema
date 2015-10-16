// Copyright 2015 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

// Package form provides ways to create and process forms based on
// environschema schemas.
//
// The API exposed by this package is not currently subject
// to the environschema.v1 API compatibility guarantees.
package form

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/juju/schema"
	"golang.org/x/crypto/ssh/terminal"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/environschema.v1"
)

// Form describes a form based on a schema.
type Form struct {
	// Title holds the title of the form, giving contextual
	// information for the fields.
	Title string

	// Fields holds the fields that make up the body of the form.
	Fields environschema.Fields
}

// Filler represents an object that can fill out a Form. The the form is
// described in f. The returned value should be compatible with the
// schema defined in f.Fields.
type Filler interface {
	Fill(f Form) (map[string]interface{}, error)
}

// SortedFields returns the given fields sorted first by group name.
// Those in the same group are sorted so that secret fields come after
// non-secret ones, finally the fields are sorted by description.
func SortedFields(fields environschema.Fields) []NamedAttr {
	fs := make(namedAttrSlice, 0, len(fields))
	for k, v := range fields {
		fs = append(fs, NamedAttr{
			Name: k,
			Attr: v,
		})
	}
	sort.Sort(fs)
	return fs
}

// NamedAttr associates a name with an environschema.Field.
type NamedAttr struct {
	Name string
	environschema.Attr
}

type namedAttrSlice []NamedAttr

func (s namedAttrSlice) Len() int {
	return len(s)
}

func (s namedAttrSlice) Less(i, j int) bool {
	a1 := &s[i]
	a2 := &s[j]
	if a1.Group != a2.Group {
		return a1.Group < a2.Group
	}
	if a1.Secret != a2.Secret {
		return a2.Secret
	}
	return a1.Description < a2.Description
}

func (s namedAttrSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// IOFiller is a Filler based around an io.Reader and io.Writer.
type IOFiller struct {
	// In is used to read responses from the user. If this is nil,
	// then os.Stdin will be used.
	In io.Reader

	// Out is used to write prompts and information to the user. If
	// this is nil, then os.Stdout will be used.
	Out io.Writer

	// MaxTries is the number of times to attempt to get a valid
	// response when prompting. If this is 0 then the default of 3
	// attempts will be used.
	MaxTries int
}

// Fill implements Filler.Fill by writing the field information to
// f.Out, then reading input from f.In. If f.In is a terminal and the
// attribute is secret, echo will be disabled.
//
// Fill processes fields by first sorting them and then prompting for
// the value of each one in turn.
//
// The fields are sorted by first by group name. Those in the same group
// are sorted so that secret fields come after non-secret ones, finally
// the fields are sorted by description.
//
// Each field will be prompted for, then the returned value will be
// validated against the field's type. If the returned value does not
// validate correctly it will be prompted again up to MaxTries before
// giving up.
func (f IOFiller) Fill(form Form) (map[string]interface{}, error) {
	if f.MaxTries == 0 {
		f.MaxTries = 3
	}
	if f.In == nil {
		f.In = os.Stdin
	}
	if f.Out == nil {
		f.Out = os.Stdout
	}
	fields := SortedFields(form.Fields)
	values := make(map[string]interface{}, len(fields))
	checkers := make([]schema.Checker, len(fields))
	for i, field := range fields {
		checker, err := field.Checker()
		if err != nil {
			return nil, errgo.Notef(err, "invalid field %s", field.Name)
		}
		checkers[i] = checker
	}
	if form.Title != "" {
		f.printf("%s\n", form.Title)
	}
	for i, field := range fields {
		v, err := f.promptLoop(field, checkers[i])
		if err != nil {
			return nil, errgo.Notef(err, "cannot complete form")
		}
		if v != nil {
			values[field.Name] = v
		}
	}
	return values, nil
}

func (f IOFiller) promptLoop(attr NamedAttr, checker schema.Checker) (interface{}, error) {
	def, envVar := DefaultFromEnv(attr.Attr)
	var defVal interface{}
	if def != "" {
		v, err := checker.Coerce(def, nil)
		if err != nil {
			f.printf("warning: invalid default value in $%s\n", envVar)
			def = ""
		} else {
			defVal = v
		}
	}
	for i := 0; i < f.MaxTries; i++ {
		vStr, err := f.prompt(attr, checker, def)
		if err != nil {
			return nil, errgo.Mask(err)
		}
		if vStr == "" {
			if defVal != nil {
				return defVal, nil
			}
			if !attr.Mandatory {
				// No value entered but the attribute is not mandatory.
				return nil, nil
			}
		}
		v, err := checker.Coerce(vStr, nil)
		if err == nil {
			return v, nil
		}
		f.printf("invalid input: %v\n", err)
	}
	return nil, errgo.New("too many invalid inputs")
}

func (f IOFiller) printf(format string, a ...interface{}) {
	fmt.Fprintf(f.Out, format, a...)
}

func (f IOFiller) prompt(attr NamedAttr, checker schema.Checker, def string) (string, error) {
	prompt := attr.Description
	if def != "" {
		if attr.Secret {
			def = strings.Repeat("*", len(def))
		}
		prompt = fmt.Sprintf("%s [%s]", attr.Description, def)
	}
	f.printf("%s: ", prompt)
	input, err := readLine(f.Out, f.In, attr.Secret)
	if err != nil {
		return "", errgo.Notef(err, "cannot read input")
	}
	return input, nil
}

func readLine(w io.Writer, r io.Reader, secret bool) (string, error) {
	if f, ok := r.(*os.File); ok && secret && terminal.IsTerminal(int(f.Fd())) {
		defer w.Write([]byte{'\n'})
		line, err := terminal.ReadPassword(int(f.Fd()))
		return string(line), err
	}
	var input []byte
	for {
		var buf [1]byte
		n, err := r.Read(buf[:])
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			input = append(input, buf[0])
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return "", errgo.Mask(err)
		}
	}
	return strings.TrimRight(string(input), "\r"), nil
}

// DefaultFromEnv returns any default value found in the environment for
// the given attribute and, if found, the variable that the value was
// found in.
//
// The environment variables specified in attr will be checked in order
// and the first non-empty value found is returned. If no non-empty
// values are found then ("", "") is returned.
func DefaultFromEnv(attr environschema.Attr) (val, envVar string) {
	if attr.EnvVar != "" {
		if val := os.Getenv(attr.EnvVar); val != "" {
			return val, attr.EnvVar
		}
	}
	for _, envVar := range attr.EnvVars {
		if val := os.Getenv(envVar); val != "" {
			return val, envVar
		}
	}
	return "", ""
}
