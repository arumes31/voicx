package fixtures

import "testing"

func Fuzz(*testing.F)                     {}
func Fuzz1(*testing.F)                    {}
func Fuzz_(*testing.F)                    {}
func FuzzÜnicode(*testing.F)              {}
func FuzzExplicitEmptyResults(*testing.F) {}
