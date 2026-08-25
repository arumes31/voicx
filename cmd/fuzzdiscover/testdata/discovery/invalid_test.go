package fixtures

import "testing"

type definedF testing.F
type lookalikeF struct{}
type receiver struct{}
type localAlias = testing.F

func Fuzzlower(*testing.F)             {}
func FuzzWithResult(*testing.F) error  { return nil }
func (receiver) FuzzMethod(*testing.F) {}
func FuzzGeneric[T any](*testing.F)    {}
func FuzzTwo(*testing.F, string)       {}
func FuzzDefined(*definedF)            {}
func FuzzLocalAlias(*localAlias)       {}
func FuzzLookalike(*lookalikeF)        {}
func FuzzMissingParameter()            {}
