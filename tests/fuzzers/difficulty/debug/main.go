package main

import (
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"io/ioutil"
	"os"

	"github.com/ethereum/go-ethereum/tests/fuzzers/difficulty"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: debug <file>")
		os.Exit(1)
	}
	crasher := os.Args[1]

	canonicalPath, err := common.VerifyPath(crasher)
	if err != nil {
		fmt.Println("path not verified: " + err.Error())
		return
	}

	data, err := ioutil.ReadFile(canonicalPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading crasher %v: %v", canonicalPath, err)
		os.Exit(1)
	}
	difficulty.Fuzz(data)
}
