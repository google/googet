/*
Copyright 2016 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The goopack binary creates a GooGet package using the provided GooSpec file.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"google3/third_party/golang/googet/goolib/goolib"
)

var (
	outputDir = flag.String("output_dir", "", "where to put the built package")
)

const (
	flgDefValue   = "flag generated for goospec variable"
	varFlagPrefix = "var:"
)

func addFlags(args []string) {
	for _, arg := range args {
		if len(arg) <= 1 || arg[0] != '-' {
			continue
		}

		name := arg[1:]
		if name[0] == '-' {
			name = name[1:]
		}

		if !strings.HasPrefix(name, varFlagPrefix) {
			continue
		}

		name = strings.SplitN(name, "=", 2)[0]

		if flag.Lookup(name) != nil {
			continue
		}

		flag.String(name, "", flgDefValue)
	}
}

func populateVars() map[string]string {
	varMap := map[string]string{}
	flag.Visit(func(flg *flag.Flag) {
		if strings.HasPrefix(flg.Name, varFlagPrefix) {
			varMap[strings.TrimPrefix(flg.Name, varFlagPrefix)] = flg.Value.String()
		}
	})

	return varMap
}

func usage() {
	fmt.Printf("Usage: %s <path/to/goospec>\n", filepath.Base(os.Args[0]))
}

func main() {
	addFlags(os.Args[1:])
	flag.Parse()

	switch len(flag.Args()) {
	case 0:
		fmt.Println("Not enough args.")
		usage()
		os.Exit(1)
	case 1:
	default:
		fmt.Println("Too many args.")
		usage()
		os.Exit(1)
	}
	if flag.Arg(1) == "help" {
		usage()
		os.Exit(0)
	}

	outDir := *outputDir
	if outDir == "" {
		var err error
		outDir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	varMap := populateVars()
	gs, err := goolib.ReadGooSpec(flag.Arg(0), varMap)
	if err != nil {
		log.Fatal(err)
	}

	baseDir := filepath.Dir(filepath.Clean(flag.Arg(0)))
	if baseDir == "." {
		baseDir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}
	if _,err := goolib.CreatePackage(gs, baseDir, outDir); err != nil {
		log.Fatal(err)
	}
}

