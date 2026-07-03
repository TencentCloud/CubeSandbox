package main

import "fmt"

func Greeting(name string) string {
	if name == "" {
		name = "cube"
	}
	return fmt.Sprintf("hello %s from Go inside CubeSandbox", name)
}

func main() {
	fmt.Println(Greeting("cube"))
}
