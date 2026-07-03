package main

import "testing"

func TestGreetingUsesDefaultName(t *testing.T) {
	got := Greeting("")
	want := "hello cube from Go inside CubeSandbox"
	if got != want {
		t.Fatalf("Greeting(\"\") = %q, want %q", got, want)
	}
}

func TestGreetingUsesProvidedName(t *testing.T) {
	got := Greeting("Alice")
	want := "hello Alice from Go inside CubeSandbox"
	if got != want {
		t.Fatalf("Greeting(\"Alice\") = %q, want %q", got, want)
	}
}
