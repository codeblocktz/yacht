// Command yacht-cli is the engine's command line: sign in, link a directory
// to an app, and `up` it.
//
// Everything that names it is a variable the linker sets, so a product ships
// the same program under its own name without a line of its own:
//
//	go build -ldflags "-X main.name=datahuru -X main.product=Datahuru \
//	  -X main.host=https://app.datahuru.com -X main.version=v1.2.3" \
//	  -o datahuru github.com/codeblocktz/yacht/cmd/yacht-cli
package main

import "github.com/codeblocktz/yacht/internal/cli"

// Set at build time; see the package comment. Empty takes the defaults in
// cli.Brand: name "yacht", product from the name, no default host, the link
// directory ".<name>" and variables <NAME>_TOKEN, <NAME>_HOST and <NAME>_APP.
var (
	name      = "yacht"
	product   = ""
	host      = ""
	linkDir   = ""
	envPrefix = ""
	version   = "dev"
)

func main() {
	cli.Main(cli.Brand{
		Name: name, Product: product, DefaultHost: host,
		LinkDir: linkDir, EnvPrefix: envPrefix, Version: version,
	})
}
