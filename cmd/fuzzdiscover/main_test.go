package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverFindsASTValidTargets(t *testing.T) {
	targets, err := discover(filepath.Join("testdata", "discovery"))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	want := []fuzzTarget{
		{Module: ".", Package: ".", Target: "Fuzz"},
		{Module: ".", Package: ".", Target: "Fuzz1"},
		{Module: ".", Package: ".", Target: "FuzzAnonymous"},
		{Module: ".", Package: ".", Target: "FuzzCanonical"},
		{Module: ".", Package: ".", Target: "FuzzDotImport"},
		{Module: ".", Package: ".", Target: "FuzzExplicitEmptyResults"},
		{Module: ".", Package: ".", Target: "FuzzMultiline"},
		{Module: ".", Package: ".", Target: "FuzzNonFParameter"},
		{Module: ".", Package: ".", Target: "FuzzSelectorAlias"},
		{Module: ".", Package: ".", Target: "FuzzTrailingComment"},
		{Module: ".", Package: ".", Target: "Fuzz_"},
		{Module: ".", Package: ".", Target: "FuzzÜnicode"},
		{Module: "client", Package: ".", Target: "FuzzClient"},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("targets = %#v, want %#v", targets, want)
	}
}

func TestDiscoverReturnsNoTargetsForEmptyRepository(t *testing.T) {
	dir := t.TempDir()
	targets, err := discover(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %#v, want none", targets)
	}
}
