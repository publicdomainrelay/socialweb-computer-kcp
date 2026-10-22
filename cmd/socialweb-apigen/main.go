package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apigen"
)

func main() {
	root := flag.String("root", ".", "the repository root: the api groups are read from it, and the revision lock beside deploy/")
	out := flag.String("out", "", "the directory the generated files are written under, at their repository-relative paths; empty writes them into -root")
	list := flag.Bool("list", false, "print the repository-relative path of every generated file and write nothing")
	hash := flag.String("hash", "", "print the name and spec hash of this APIResourceSchema manifest, as the revision lock records it, and exit")
	flag.Parse()
	if err := run(*root, *out, *list, *hash); err != nil {
		fmt.Fprintf(os.Stderr, "socialweb-apigen: %v\n", err)
		os.Exit(1)
	}
}

func run(root, out string, list bool, hash string) error {
	if hash != "" {
		data, err := os.ReadFile(hash)
		if err != nil {
			return err
		}
		name, sum, err := apigen.SpecHash(data)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", name, sum)
		return nil
	}
	outputs, err := apigen.Generate(root)
	if err != nil {
		return err
	}
	if list {
		for _, o := range outputs {
			fmt.Println(o.Path)
		}
		return nil
	}
	if out == "" {
		out = root
	}
	return apigen.Write(out, outputs)
}
