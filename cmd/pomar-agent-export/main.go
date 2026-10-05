// pomar-agent-export verifies binary candidates returned as bounded base64 text.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/zeroemployeeorg/pomar/internal/agentartifact"
)

func main() {
	result := flag.String("result", "", "saved GET agent/result JSON")
	binding := flag.String("binding", "", "independently retained controller binding JSON")
	output := flag.String("output", "", "new private output directory")
	flag.Parse()
	if *result == "" || *binding == "" || *output == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*result, *binding, *output); err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	// Do not print arbitrary guest contents or authentication evidence.
	fmt.Println("export: verified wheel and sdist bytes; private receipt saved")
}

func run(resultPath, bindingPath, output string) error {
	bf, err := os.Open(bindingPath)
	if err != nil {
		return err
	}
	defer bf.Close()
	b, err := io.ReadAll(io.LimitReader(bf, (16<<10)+1))
	if err != nil {
		return err
	}
	if len(b) > 16<<10 {
		return fmt.Errorf("binding exceeds limit")
	}
	var binding agentartifact.Binding
	if err := json.Unmarshal(b, &binding); err != nil {
		return err
	}
	f, err := os.Open(resultPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = agentartifact.Receive(f, binding, output)
	return err
}
