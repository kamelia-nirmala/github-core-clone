// Entry point: listen on an ephemeral port and serve the forge.
// The launcher (main.ts) reads the "PORT=<n>" line from stdout.
package main

func main() {
	serve()
}
