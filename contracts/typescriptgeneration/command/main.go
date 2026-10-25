// Command writes the generated TypeScript contract (contract.ts) to -output.
//
//	go run github.com/mycelpf/mycel_module_sdk/contracts/typescriptgeneration/command -output path/contract.ts
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mycelpf/mycel_module_sdk/contracts/typescriptgeneration"
)

func main() {
	output := flag.String("output", "", "file to write; - writes to standard output")
	flag.Parse()
	source := typescriptgeneration.Generate()
	if *output == "" || *output == "-" {
		_, _ = os.Stdout.Write(source)
		return
	}
	if err := os.WriteFile(*output, source, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
