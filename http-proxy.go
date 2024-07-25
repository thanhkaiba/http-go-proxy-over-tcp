package httproxytcp

import "net/http"

type HttpProxy interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}
