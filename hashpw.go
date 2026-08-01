package main

import (
	"fmt"
	"os"
	"voicx/internal/auth"
)

func main() {
	h, err := auth.HashPassword(os.Args[1])
	if err != nil {
		panic(err)
	}
	fmt.Print(h)
}
