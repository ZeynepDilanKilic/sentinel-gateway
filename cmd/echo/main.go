// Command echo is a throwaway upstream for local testing: it reflects the
// request it received so you can see exactly what the gateway forwarded.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	name := flag.String("name", "echo", "service name")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "service=%s\nmethod=%s\npath=%s\nhost=%s\n", *name, r.Method, r.URL.RequestURI(), r.Host)
		for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			fmt.Fprintf(w, "%s=%s\n", k, r.Header.Get(k))
		}
	})
	log.Printf("%s listening on %s", *name, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
