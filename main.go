package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
)

func main() {
	port := flag.Int("port", 8080, "port to listen to")
	flag.Parse()

	zoro := Zoro{}

	http.ListenAndServe(
		fmt.Sprintf(":%d", *port),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			vals := r.URL.Query()
			url := vals.Get("spec")

			vars := make(map[string]string, len(vals))
			for k := range vals {
				vars[k] = vals.Get(k)
			}

			// just for neatness in the cli
			defer w.Write([]byte("\n"))

			bts, err := zoro.SpecExec(r.Context(), url, vars)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				bytes.NewBuffer([]byte(err.Error())).WriteTo(w)
			}

			w.Header().Add("content-type", "application/json; charset=utf-8")
			bytes.NewBuffer(bts).WriteTo(w)
		}),
	)
}
