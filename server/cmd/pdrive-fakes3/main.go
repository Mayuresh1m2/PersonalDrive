// Command pdrive-fakes3 runs an in-memory S3-compatible server for the
// disaster drill and local testing. It is not part of the product.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9900", "listen address")
	flag.Parse()
	faker := gofakes3.New(s3mem.New())
	log.Printf("fake S3 listening on http://%s (in-memory, data is lost on exit)", *addr)
	log.Fatal(http.ListenAndServe(*addr, faker.Server()))
}
